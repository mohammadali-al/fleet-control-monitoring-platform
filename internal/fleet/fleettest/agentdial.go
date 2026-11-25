package fleettest

import (
	"context"
	"crypto/tls"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

// The agent_dial direction: the main server runs the listener, the real agent
// runtime (internal/agent) dials out to it over mTLS. Pass the listener to
// fleet.Config.AgentDial; agent_dial machines then behave exactly like the
// server_dial ones StartAgent / StartMonitor / Factory give.

// ListenAgents binds an agent_dial listener on addr ("127.0.0.1:0" if empty),
// with the throwaway CA's server certificate. The manager serves it from Start.
// To model a server restart, close the manager and listen again on the same
// listener address.
func (m *MTLS) ListenAgents(addr string) (*fleet.AgentListener, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	return fleet.ListenAgents(fleet.AgentListenerConfig{Addr: addr, CA: m.CA, ServerID: m.ServerID})
}

// DialTLS is the client TLS config of an agent_dial agent for machineID: a
// certificate issued by ca, the pinned CA being ca too, and the server's id
// expected to be serverID. Use the throwaway CA for a legitimate agent and
// another pki.CA to model an agent from a different CA.
func (m *MTLS) DialTLS(ca *pki.CA, machineID, serverID string) (*tls.Config, error) {
	certPEM, keyPEM, err := ca.IssuePeer(machineID, nil, time.Hour)
	if err != nil {
		return nil, err
	}
	return channel.AgentDialTLSConfig(certPEM, keyPEM, ca.CertPEM(), serverID)
}

// dialSettings fills ds for a test: the server address, a certificate from the
// throwaway CA unless ds.TLS is set, and short reconnect delays.
func (m *MTLS) dialSettings(machineID, serverAddr string, ds agent.DialSettings) (agent.DialSettings, error) {
	ds.ServerAddr = serverAddr
	if ds.TLS == nil {
		tlsCfg, err := m.DialTLS(m.CA, machineID, m.ServerID)
		if err != nil {
			return ds, err
		}
		ds.TLS = tlsCfg
	}
	if ds.MinBackoff == 0 {
		ds.MinBackoff = 20 * time.Millisecond
	}
	if ds.MaxBackoff == 0 {
		ds.MaxBackoff = 200 * time.Millisecond
	}
	return ds, nil
}

// RunningDialingAgent is a worker agent started by StartDialingAgent.
type RunningDialingAgent struct {
	*agent.DialingWorker

	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Stop shuts the agent down (running sessions are terminated) and returns its
// Run error.
func (r *RunningDialingAgent) Stop() error {
	r.cancel()
	<-r.done
	return r.err
}

// StartDialingAgent starts a real worker agent (with the watchdog wiring, as in
// production) that dials the server at serverAddr as machineID. cfg supplies
// the rest (DataDir is required); ds the dial settings (see dialSettings).
func (m *MTLS) StartDialingAgent(machineID, serverAddr string, cfg agent.Config, ds agent.DialSettings) (*RunningDialingAgent, error) {
	ds, err := m.dialSettings(machineID, serverAddr, ds)
	if err != nil {
		return nil, err
	}
	cfg.MachineID = machineID
	a, err := agent.NewDialingWorker(cfg, ds, agent.WatchdogOptions{})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &RunningDialingAgent{DialingWorker: a, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(r.done); r.err = a.Run(ctx) }()
	return r, nil
}

// RunningDialingMonitor is a monitor agent started by StartDialingMonitor.
type RunningDialingMonitor struct {
	*agent.DialingMonitor

	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Stop shuts the monitor agent down and returns its Run error.
func (r *RunningDialingMonitor) Stop() error {
	r.cancel()
	<-r.done
	return r.err
}

// StartDialingMonitor is StartDialingAgent for the monitor role.
func (m *MTLS) StartDialingMonitor(machineID, serverAddr string, cfg agent.MonitorConfig, ds agent.DialSettings) (*RunningDialingMonitor, error) {
	ds, err := m.dialSettings(machineID, serverAddr, ds)
	if err != nil {
		return nil, err
	}
	cfg.MachineID = machineID
	mon, err := agent.NewDialingMonitor(cfg, ds)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &RunningDialingMonitor{DialingMonitor: mon, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(r.done); r.err = mon.Run(ctx) }()
	return r, nil
}
