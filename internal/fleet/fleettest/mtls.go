package fleettest

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// MTLS runs real worker agents (internal/agent) on 127.0.0.1 and gives the
// fleet manager the production link to them: the same mTLS channel Dialer the
// server uses (fleet.NewDialerFactory), with certificates from a throwaway CA.
// Unlike Loopback nothing is simulated: hello handshake, config-push, executor
// and capture all run over the real channel. Register a machine in the registry
// with PrivateIPs ["127.0.0.1"] and use Factory as the manager's NewLink.
type MTLS struct {
	CA       *pki.CA
	ServerID string

	mu     sync.Mutex
	agents map[string]*RunningAgent
}

// NewMTLS creates the throwaway CA.
func NewMTLS() (*MTLS, error) {
	ca, err := pki.NewCA(pki.Options{})
	if err != nil {
		return nil, err
	}
	return &MTLS{CA: ca, ServerID: config.DefaultServerID, agents: map[string]*RunningAgent{}}, nil
}

// RunningAgent is an agent started by StartAgent.
type RunningAgent struct {
	*agent.Agent
	Port int

	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Stop shuts the agent down (running sessions are terminated) and returns its
// Run error.
func (r *RunningAgent) Stop() error {
	r.cancel()
	<-r.done
	return r.err
}

// StartAgent starts a worker agent for machineID on 127.0.0.1 with a free port
// and a certificate issued by the CA. cfg supplies the rest (DataDir is
// required); MachineID, ListenAddr and TLS are filled in.
func (m *MTLS) StartAgent(machineID string, cfg agent.Config) (*RunningAgent, error) {
	certPEM, keyPEM, err := m.CA.IssuePeer(machineID, []net.IP{net.IPv4(127, 0, 0, 1)}, time.Hour)
	if err != nil {
		return nil, err
	}
	cfg.MachineID = machineID
	cfg.ListenAddr = "127.0.0.1:0"
	if cfg.TLS, err = pki.ServerTLSConfig(certPEM, keyPEM, m.CA.CertPEM()); err != nil {
		return nil, err
	}
	a, err := agent.New(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &RunningAgent{Agent: a, Port: a.Addr().(*net.TCPAddr).Port, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(r.done); r.err = a.Run(ctx) }()
	m.mu.Lock()
	m.agents[machineID] = r
	m.mu.Unlock()
	return r, nil
}

// Factory is the fleet.LinkFactory: an mTLS dialer to the machine's agent.
func (m *MTLS) Factory() fleet.LinkFactory {
	return func(mc registry.Machine, h channel.Handler) (fleet.Link, error) {
		m.mu.Lock()
		r := m.agents[mc.ID]
		m.mu.Unlock()
		if r == nil {
			return nil, fmt.Errorf("fleettest: no agent started for %q", mc.ID)
		}
		f, err := fleet.NewDialerFactory(m.CA, m.ServerID, r.Port)
		if err != nil {
			return nil, err
		}
		return f(mc, h)
	}
}

// DialerConfig is a channel.DialerConfig a test can use to talk to a machine's
// agent directly, as the main server would, without a Manager (for protocol
// level tests, e.g. sending a run with no confirmation token). Handler and
// OnConnect are the caller's to set.
func (m *MTLS) DialerConfig(machineID string) (channel.DialerConfig, error) {
	m.mu.Lock()
	r := m.agents[machineID]
	m.mu.Unlock()
	if r == nil {
		return channel.DialerConfig{}, fmt.Errorf("fleettest: no agent started for %q", machineID)
	}
	certPEM, keyPEM, err := m.CA.IssuePeer(m.ServerID, nil, time.Hour)
	if err != nil {
		return channel.DialerConfig{}, err
	}
	tlsCfg, err := pki.DialTLSConfig(certPEM, keyPEM, m.CA.CertPEM(), net.IPv4(127, 0, 0, 1))
	if err != nil {
		return channel.DialerConfig{}, err
	}
	return channel.DialerConfig{
		Addr:      fmt.Sprintf("127.0.0.1:%d", r.Port),
		TLS:       tlsCfg,
		MachineID: m.ServerID,
	}, nil
}
