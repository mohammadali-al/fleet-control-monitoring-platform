package fleettest

import (
	"context"
	"net"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

// RunningMonitor is a monitor agent started by StartMonitor.
type RunningMonitor struct {
	*agent.Monitor
	Port int

	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Stop shuts the monitor agent down and returns its Run error.
func (r *RunningMonitor) Stop() error {
	r.cancel()
	<-r.done
	return r.err
}

// StartMonitor starts a monitor agent (the real internal/agent runtime) for
// machineID on 127.0.0.1 with a free port and a certificate issued by the CA,
// exactly as StartAgent does for a worker. Factory then dials it like any
// agent. cfg supplies the rest; MachineID, ListenAddr and TLS are filled in.
func (m *MTLS) StartMonitor(machineID string, cfg agent.MonitorConfig) (*RunningMonitor, error) {
	certPEM, keyPEM, err := m.CA.IssuePeer(machineID, []net.IP{net.IPv4(127, 0, 0, 1)}, time.Hour)
	if err != nil {
		return nil, err
	}
	cfg.MachineID = machineID
	cfg.ListenAddr = "127.0.0.1:0"
	if cfg.TLS, err = pki.ServerTLSConfig(certPEM, keyPEM, m.CA.CertPEM()); err != nil {
		return nil, err
	}
	mon, err := agent.NewMonitor(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	port := mon.Addr().(*net.TCPAddr).Port
	r := &RunningMonitor{Monitor: mon, Port: port, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(r.done); r.err = mon.Run(ctx) }()
	// Factory looks machines up by port only; a RunningAgent carrying just the
	// port is all it needs to dial a monitor agent.
	m.mu.Lock()
	m.agents[machineID] = &RunningAgent{Port: port, cancel: cancel, done: r.done}
	m.mu.Unlock()
	return r, nil
}
