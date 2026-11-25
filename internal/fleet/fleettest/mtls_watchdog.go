package fleettest

import (
	"context"
	"net"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

// RunningWatchdogAgent is a worker agent with the watchdog wiring (policy
// store + event forward), started by StartWatchdogAgent.
type RunningWatchdogAgent struct {
	*agent.WatchdogAgent
	Port int

	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Stop shuts the agent down and returns its Run error.
func (r *RunningWatchdogAgent) Stop() error {
	r.cancel()
	<-r.done
	return r.err
}

// StartWatchdogAgent is StartAgent for the production worker (agent.WorkerFromConfig
// builds the same thing): a real worker agent on 127.0.0.1 whose main agent also
// stores pushed watchdog policies and forwards the events the watchdog process
// records in cfg.DataDir. Factory dials it like any agent.
func (m *MTLS) StartWatchdogAgent(machineID string, cfg agent.Config, opts agent.WatchdogOptions) (*RunningWatchdogAgent, error) {
	certPEM, keyPEM, err := m.CA.IssuePeer(machineID, []net.IP{net.IPv4(127, 0, 0, 1)}, time.Hour)
	if err != nil {
		return nil, err
	}
	cfg.MachineID = machineID
	cfg.ListenAddr = "127.0.0.1:0"
	if cfg.TLS, err = pki.ServerTLSConfig(certPEM, keyPEM, m.CA.CertPEM()); err != nil {
		return nil, err
	}
	a, err := agent.NewWithWatchdog(cfg, opts)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	port := a.Addr().(*net.TCPAddr).Port
	r := &RunningWatchdogAgent{WatchdogAgent: a, Port: port, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(r.done); r.err = a.Run(ctx) }()
	// Factory looks machines up by port only.
	m.mu.Lock()
	m.agents[machineID] = &RunningAgent{Port: port, cancel: cancel, done: r.done}
	m.mu.Unlock()
	return r, nil
}
