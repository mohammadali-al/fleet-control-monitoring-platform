package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
)

// sendTimeout bounds one write to the channel so a half-dead connection cannot
// stall the flush loop; the batch stays buffered and is retried.
const sendTimeout = 10 * time.Second

// MonitorConfig configures a Monitor (the monitor-agent role).
type MonitorConfig struct {
	// MachineID is this agent's id (the CN of its certificate).
	MachineID string
	// ListenAddr is the channel listen address (default ":7443").
	ListenAddr string
	// TLS is the listener's TLS config (pki.ServerTLSConfig). Required.
	TLS *tls.Config
	// FlushInterval is how often probe results are sent to the main server;
	// it is clamped to 2-3 s (monitor.Flusher). Zero means the default.
	FlushInterval time.Duration
	// MaxBuffered bounds the local backlog kept while the channel is down
	// (oldest dropped first). Zero means monitor.DefaultMaxBuffered.
	MaxBuffered int
	// Probers replaces or adds probers for their types (the four built-in
	// probers are always registered first). Mostly for tests.
	Probers []probe.Prober
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Monitor is a running monitor agent: it listens for the main server on the
// mTLS channel (server_dial, like the worker), runs the probe engine for the
// targets the server pushes (mon-config), and flushes the results to the server
// every 2-3 s (mon-data). While the channel is down the results stay buffered
// and go out, oldest first, when it returns. A monitor agent has no shell, no
// executor and no evidence capture, and it never stops probing on its own.
type Monitor struct {
	cfg MonitorConfig
	log *slog.Logger
	ln  *channel.Listener
	eng *probe.Engine
	fl  *monitor.Flusher

	mu   sync.Mutex
	base context.Context // lifetime context, set by Run
}

// NewMonitor validates cfg and binds the channel listener.
func NewMonitor(cfg MonitorConfig) (*Monitor, error) {
	if cfg.MachineID == "" {
		return nil, errors.New("agent: machine id is required")
	}
	if cfg.TLS == nil {
		return nil, errors.New("agent: a TLS config is required")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = fmt.Sprintf(":%d", config.DefaultChannelPort)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	m := &Monitor{cfg: cfg, log: cfg.Logger, base: context.Background()}
	m.fl = monitor.NewFlusher(m.send, monitor.FlusherOptions{Interval: cfg.FlushInterval, MaxBuffered: cfg.MaxBuffered})
	m.eng = probe.NewEngine(m.fl.Add)
	for _, p := range cfg.Probers {
		m.eng.Register(p)
	}
	ln, err := channel.Listen(channel.ListenerConfig{
		Addr:      cfg.ListenAddr,
		TLS:       cfg.TLS,
		MachineID: cfg.MachineID,
		Handler:   m.handle,
		OnConnect: m.onConnect,
	})
	if err != nil {
		return nil, err
	}
	m.ln = ln
	return m, nil
}

// MonitorFromConfig builds a Monitor from the agent section of the config
// file: the pinned CA and this machine's certificate and key are read from the
// files the onboarding flow wrote. A monitor agent needs no data directory and
// no root.
func MonitorFromConfig(ac config.AgentConfig, log *slog.Logger) (*Monitor, error) {
	if ac.MachineID == "" {
		return nil, errors.New("agent: machine_id is not set in the agent config")
	}
	if ac.ConnectMode == config.ConnectModeAgentDial {
		return nil, errors.New("agent: connect_mode agent_dial is not supported yet (only server_dial)")
	}
	tlsCfg, err := LoadTLS(ac.CAFile, ac.CertFile, ac.KeyFile)
	if err != nil {
		return nil, err
	}
	return NewMonitor(MonitorConfig{
		MachineID:  ac.MachineID,
		ListenAddr: ac.ListenAddrOrDefault(),
		TLS:        tlsCfg,
		Logger:     log,
	})
}

// Addr is the bound channel address (useful with port 0).
func (m *Monitor) Addr() net.Addr { return m.ln.Addr() }

// Connected reports whether the main server is connected and verified.
func (m *Monitor) Connected() bool { return m.ln.Peer() != "" }

// Pending is the number of probe results buffered and not yet sent.
func (m *Monitor) Pending() int { return m.fl.Pending() }

// Dropped is the number of results discarded because the buffer was full.
func (m *Monitor) Dropped() uint64 { return m.fl.Dropped() }

// Config is the probe configuration currently applied.
func (m *Monitor) Config() probe.Config { return m.eng.Config() }

// Run probes and flushes until ctx is cancelled, serving the channel the whole
// time, then returns once everything has stopped.
func (m *Monitor) Run(ctx context.Context) error {
	m.mu.Lock()
	m.base = ctx
	m.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.eng.Run(ctx) }()
	go func() { defer wg.Done(); m.fl.Run(ctx) }()
	err := m.ln.Serve(ctx)
	// Serve only returns on ctx cancellation or a listener failure; stop the
	// probe and flush loops either way.
	if ctx.Err() == nil {
		m.log.Error("monitor channel listener failed; probing continues until shutdown", "component", "fleet-agent", "err", err)
		<-ctx.Done()
	}
	wg.Wait()
	return err
}

func (m *Monitor) baseCtx() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.base
}

// send is the flusher's SendFunc: one envelope to the main server, bounded in
// time. While no server is connected it fails with channel.ErrNotConnected and
// the flusher keeps the batch.
func (m *Monitor) send(ctx context.Context, env channel.Envelope) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	return m.ln.Send(ctx, env)
}

// onConnect runs when the main server's connection is established: it asks for
// the configuration (the server answers with a mon-config message).
func (m *Monitor) onConnect(peer string) {
	m.log.Info("main server connected", "component", "fleet-agent", "peer", peer)
	env, err := channel.NewEnvelope(channel.TypeConfigReq, "config", nil)
	if err == nil {
		err = m.send(m.baseCtx(), env)
	}
	if err != nil {
		m.log.Warn("request config", "component", "fleet-agent", "err", err)
	}
}

// handle is the channel.Handler. A monitor agent understands only mon-config;
// it has no executor, so run and friends are ignored.
func (m *Monitor) handle(ctx context.Context, env channel.Envelope) {
	if env.Type != channel.TypeMonConfig {
		m.log.Debug("ignoring envelope a monitor agent does not handle", "component", "fleet-agent", "type", env.Type)
		return
	}
	cfg, err := monitor.DecodeConfig(env)
	if err == nil {
		m.eng.Apply(cfg) // idempotent: unchanged targets keep their loops
		m.log.Info("monitor config applied", "component", "fleet-agent", "targets", len(cfg.Targets))
	} else {
		m.log.Warn("monitor config rejected", "component", "fleet-agent", "err", err)
	}
	ack := channel.ConfigAckPayload{OK: err == nil}
	if err != nil {
		ack.Error = err.Error()
	}
	out, nerr := channel.NewEnvelope(channel.TypeConfigAck, env.ID, ack)
	if nerr == nil {
		nerr = m.send(ctx, out)
	}
	if nerr != nil {
		m.log.Warn("send config ack", "component", "fleet-agent", "err", nerr)
	}
}
