package agent

// The agent_dial direction of the agent runtime: instead of listening for the
// main server (server_dial, agent.go / monitor.go), the agent dials OUT to the
// main server's agent_dial listener with capped backoff + jitter and serves the
// same protocol on that connection. The worker and monitor logic is the same
// code as under server_dial (worker.handle, the probe engine and flusher); only
// the channel endpoint differs (channel.AgentDialer instead of channel.Listener).

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
)

// DialSettings says where and how an agent_dial agent dials the main server.
type DialSettings struct {
	// ServerAddr is the main server's agent_dial listener, host:port.
	ServerAddr string
	// TLS is the agent's client TLS config (channel.AgentDialTLSConfig).
	TLS *tls.Config
	// MinBackoff and MaxBackoff bound the reconnect delay (defaults 500 ms and
	// 30 s; doubled per failed attempt, capped, with jitter).
	MinBackoff, MaxBackoff time.Duration
	// PingInterval is the keepalive period (default 20 s).
	PingInterval time.Duration
}

func (d DialSettings) validate() error {
	if d.ServerAddr == "" {
		return errors.New("agent: agent_dial needs the main server's address (server_addr)")
	}
	if d.TLS == nil {
		return errors.New("agent: agent_dial needs a TLS config")
	}
	return nil
}

func (d DialSettings) dialer(machineID string, h channel.Handler, onConnect func(string)) (*channel.AgentDialer, error) {
	return channel.NewAgentDialer(channel.AgentDialerConfig{
		Addr: d.ServerAddr, TLS: d.TLS, MachineID: machineID, Handler: h, OnConnect: onConnect,
		MinBackoff: d.MinBackoff, MaxBackoff: d.MaxBackoff, PingInterval: d.PingInterval,
	})
}

// serverAddr is the address of the main server an agent_dial agent dials.
type serverAddr string

func (serverAddr) Network() string  { return "tcp" }
func (a serverAddr) String() string { return string(a) }

// Runner is a built agent of either role and direction: it serves until its
// context is cancelled. Addr is the address it listens on (server_dial) or
// dials (agent_dial).
type Runner interface {
	Addr() net.Addr
	Run(ctx context.Context) error
}

// DialingWorker is the worker agent under agent_dial, with the watchdog wiring
// (policy store + event forward): the same worker as WatchdogAgent, on an
// outbound connection.
type DialingWorker struct {
	log *slog.Logger
	d   *channel.AgentDialer
	w   *worker
	ww  *watchdogWire
}

// NewDialingWorker validates cfg (MachineID and DataDir are required; TLS and
// ListenAddr are not used, ds carries the TLS config) and builds the worker.
func NewDialingWorker(cfg Config, ds DialSettings, opts WatchdogOptions) (*DialingWorker, error) {
	if cfg.MachineID == "" {
		return nil, errors.New("agent: machine id is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("agent: a data dir is required")
	}
	if err := ds.validate(); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	w := newWorker(cfg)
	ww := newWatchdogWire(cfg, opts)
	d, err := ds.dialer(cfg.MachineID, func(ctx context.Context, env channel.Envelope) {
		if env.Type == channel.TypePolicy {
			ww.handlePolicy(ctx, env, w.sender)
			return
		}
		w.handle(ctx, env)
	}, func(peer string) {
		w.onConnect(peer)
		ww.kickNow()
	})
	if err != nil {
		return nil, err
	}
	w.sender.set(d)
	return &DialingWorker{log: cfg.Logger, d: d, w: w, ww: ww}, nil
}

// Addr is the main server address being dialed.
func (a *DialingWorker) Addr() net.Addr { return serverAddr(a.d.Addr()) }

// Connected reports whether the main server is connected and verified.
func (a *DialingWorker) Connected() bool { return a.d.Peer() != "" }

// Run dials and serves the channel and forwards watchdog events until ctx is
// cancelled, then terminates every running session and stops captures.
func (a *DialingWorker) Run(ctx context.Context) error {
	fctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.ww.forwardLoop(fctx, a.w.sender)
	}()
	a.w.start(ctx)
	err := a.d.Run(ctx)
	a.w.shutdown()
	cancel()
	wg.Wait()
	return err
}

// DialingMonitor is the monitor agent under agent_dial: it runs the probe
// engine for the targets the server pushes (mon-config) and flushes the
// results over the outbound connection, buffering them while it is down.
type DialingMonitor struct {
	log      *slog.Logger
	d        *channel.AgentDialer
	eng      *probe.Engine
	fl       *monitor.Flusher
	pipCache *capture.PublicIPCache
	pip      *publicIPReporter

	mu   sync.Mutex
	base context.Context // lifetime context, set by Run
	svc  ServiceController
}

// NewDialingMonitor validates cfg (MachineID is required; TLS and ListenAddr
// are not used, ds carries the TLS config) and builds the monitor.
func NewDialingMonitor(cfg MonitorConfig, ds DialSettings) (*DialingMonitor, error) {
	if cfg.MachineID == "" {
		return nil, errors.New("agent: machine id is required")
	}
	if err := ds.validate(); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	svc := cfg.ServiceController
	if svc == nil {
		svc = defaultServiceController()
	}
	m := &DialingMonitor{log: cfg.Logger, base: context.Background(), svc: svc}
	m.fl = monitor.NewFlusher(m.send, monitor.FlusherOptions{Interval: cfg.FlushInterval, MaxBuffered: cfg.MaxBuffered})
	m.eng = probe.NewEngine(m.fl.Add)
	for _, p := range cfg.Probers {
		m.eng.Register(p)
	}
	m.pipCache = capture.NewPublicIPCache(capture.NewReflector(cfg.Reflectors), 0)
	m.pip = newPublicIPReporter(serviceSenderFunc(m.send), cfg.Logger, m.baseCtx)
	m.pip.bind(m.pipCache)
	d, err := ds.dialer(cfg.MachineID, m.handle, m.onConnect)
	if err != nil {
		return nil, err
	}
	m.d = d
	return m, nil
}

// Addr is the main server address being dialed.
func (m *DialingMonitor) Addr() net.Addr { return serverAddr(m.d.Addr()) }

// Connected reports whether the main server is connected and verified.
func (m *DialingMonitor) Connected() bool { return m.d.Peer() != "" }

// Pending is the number of probe results buffered and not yet sent.
func (m *DialingMonitor) Pending() int { return m.fl.Pending() }

// Config is the probe configuration currently applied.
func (m *DialingMonitor) Config() probe.Config { return m.eng.Config() }

// Run probes, flushes and keeps the connection up until ctx is cancelled.
func (m *DialingMonitor) Run(ctx context.Context) error {
	m.mu.Lock()
	m.base = ctx
	m.mu.Unlock()

	m.pipCache.Start(ctx)
	defer func() {
		m.pipCache.Stop()
		m.pip.unbind()
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.eng.Run(ctx) }()
	go func() { defer wg.Done(); m.fl.Run(ctx) }()
	err := m.d.Run(ctx)
	wg.Wait()
	return err
}

func (m *DialingMonitor) baseCtx() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.base
}

func (m *DialingMonitor) send(ctx context.Context, env channel.Envelope) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	return m.d.Send(ctx, env)
}

func (m *DialingMonitor) onConnect(peer string) {
	m.log.Info("main server connected", "component", "fleet-agent", "peer", peer)
	env, err := channel.NewEnvelope(channel.TypeConfigReq, "config", nil)
	if err == nil {
		err = m.send(m.baseCtx(), env)
	}
	if err != nil {
		m.log.Warn("request config", "component", "fleet-agent", "err", err)
	}
	m.pip.reportOnConnect()
}

func (m *DialingMonitor) serviceController() ServiceController {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.svc
}

// handle mirrors Monitor.handle: mon-config, service-control, and
// public-ip-refresh are understood.
func (m *DialingMonitor) handle(ctx context.Context, env channel.Envelope) {
	switch env.Type {
	case channel.TypeServiceControl:
		handleServiceControl(ctx, m.log, serviceSenderFunc(m.send), m.serviceController(), env)
		return
	case channel.TypePublicIPRefresh:
		m.pip.handleRefresh()
		return
	case channel.TypeMonConfig:
	default:
		m.log.Debug("ignoring envelope a monitor agent does not handle", "component", "fleet-agent", "type", env.Type)
		return
	}
	cfg, err := monitor.DecodeConfig(env)
	if err == nil {
		m.eng.Apply(cfg)
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

// LoadDialTLS reads the pinned CA, certificate and key files and builds the
// agent's client TLS config for agent_dial; serverID is the identity the main
// server's certificate must carry.
func LoadDialTLS(caFile, certFile, keyFile, serverID string) (*tls.Config, error) {
	for name, path := range map[string]string{"ca_file": caFile, "cert_file": certFile, "key_file": keyFile} {
		if path == "" {
			return nil, fmt.Errorf("agent: %s is not set in the agent config (was the machine onboarded?)", name)
		}
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("agent: read CA: %w", err)
	}
	cert, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("agent: read certificate: %w", err)
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("agent: read key: %w", err)
	}
	return channel.AgentDialTLSConfig(cert, key, ca, serverID)
}

// DialFromConfig builds an agent_dial agent (worker or monitor, by ac.Role)
// from the agent section of the config file. It is the agent_dial counterpart
// of WorkerFromConfig / MonitorFromConfig, which serve server_dial.
func DialFromConfig(ac config.AgentConfig, dataDir string, log *slog.Logger) (Runner, error) {
	if ac.MachineID == "" {
		return nil, errors.New("agent: machine_id is not set in the agent config")
	}
	tlsCfg, err := LoadDialTLS(ac.CAFile, ac.CertFile, ac.KeyFile, ac.ServerIDOrDefault())
	if err != nil {
		return nil, err
	}
	ds := DialSettings{ServerAddr: ac.ServerAddr, TLS: tlsCfg}
	if ac.Role == config.RoleMonitor {
		return NewDialingMonitor(MonitorConfig{MachineID: ac.MachineID, Reflectors: ac.Capture.Reflectors, Logger: log}, ds)
	}
	cg := ac.CgroupRoot
	if cg == "" {
		cg = DefaultCgroupRoot()
	}
	return NewDialingWorker(Config{
		MachineID:  ac.MachineID,
		DataDir:    dataDir,
		CgroupRoot: cg,
		Capture:    ac.Capture,
		Logger:     log,
	}, ds, WatchdogOptions{Groups: ac.Watchdog.Groups})
}
