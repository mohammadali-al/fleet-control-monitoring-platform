package channel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

// adHarness is an Acceptor (main-server side) plus an AgentDialer config on a
// throwaway CA: the agent_dial counterpart of harness.
type adHarness struct {
	*harness
	attached  chan *Conn    // each completed handshake, as the acceptor saw it
	serverGot chan Envelope // what the main-server side received (agent -> server)
	detached  chan string
}

func newADHarness(t *testing.T) *adHarness {
	return &adHarness{
		harness:   newHarness(t),
		attached:  make(chan *Conn, 8),
		serverGot: make(chan Envelope, 16),
		detached:  make(chan string, 8),
	}
}

// startAcceptor binds addr and serves in the background.
func (h *adHarness) startAcceptor(addr string, admit func(string) error) (a *Acceptor, stop func()) {
	h.t.Helper()
	tlsCfg, err := pki.ServerTLSConfig(h.main.cert, h.main.key, h.ca.CertPEM())
	if err != nil {
		h.t.Fatalf("ServerTLSConfig: %v", err)
	}
	a, err = Accept(AcceptorConfig{
		Addr: addr, TLS: tlsCfg, ServerID: mainID, Admit: admit,
		Attach: func(id string, c *Conn) (Handler, func(), error) {
			h.attached <- c
			return func(_ context.Context, env Envelope) { h.serverGot <- env }, func() { h.detached <- id }, nil
		},
	})
	if err != nil {
		h.t.Fatalf("Accept: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = a.Serve(ctx) }()
	return a, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait):
			h.t.Error("Serve did not return after cancel")
		}
	}
}

// agentDialerConfig is what the agent (certificate id) runs with, pinning caPEM.
func (h *adHarness) agentDialerConfig(addr string, id identity, caPEM []byte, serverID string) AgentDialerConfig {
	h.t.Helper()
	tlsCfg, err := AgentDialTLSConfig(id.cert, id.key, caPEM, serverID)
	if err != nil {
		h.t.Fatalf("AgentDialTLSConfig: %v", err)
	}
	return AgentDialerConfig{
		Addr: addr, TLS: tlsCfg, MachineID: agentID,
		Handler:    func(_ context.Context, env Envelope) { h.got <- env },
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
	}
}

func (h *adHarness) runAgentDialer(cfg AgentDialerConfig) (d *AgentDialer, connected <-chan string, stop func()) {
	h.t.Helper()
	conn := make(chan string, 8)
	cfg.OnConnect = func(id string) { conn <- id }
	d, err := NewAgentDialer(cfg)
	if err != nil {
		h.t.Fatalf("NewAgentDialer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(ctx) }()
	return d, conn, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait):
			h.t.Error("Run did not return after cancel")
		}
	}
}

func TestAgentDialHandshakeAndRoundTrip(t *testing.T) {
	h := newADHarness(t)
	a, stopA := h.startAcceptor("127.0.0.1:0", nil)
	defer stopA()
	d, dConn, stopD := h.runAgentDialer(h.agentDialerConfig(a.Addr().String(), h.agent, h.ca.CertPEM(), mainID))
	defer stopD()

	// Each side learns the other's id from its certificate.
	if got := recv(t, dConn, "agent handshake"); got != mainID {
		t.Errorf("agent saw server %q, want %q", got, mainID)
	}
	conn := recv(t, h.attached, "server attach")
	if conn.AgentID() != agentID {
		t.Errorf("server saw agent %q, want %q", conn.AgentID(), agentID)
	}
	if d.Peer() != mainID {
		t.Errorf("Peer() = %q, want %q", d.Peer(), mainID)
	}

	// Agent -> server, server -> agent, payload intact, over the same Envelope.
	want, err := NewEnvelope("test.up", "u-1", map[string]int{"n": 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Send(context.Background(), want); err != nil {
		t.Fatalf("agent Send: %v", err)
	}
	checkEnvelope(t, recv(t, h.serverGot, "agent -> server envelope"), want)
	down, _ := NewEnvelope("test.down", "d-1", map[string]string{"k": "v"})
	if err := conn.Send(context.Background(), down); err != nil {
		t.Fatalf("server Send: %v", err)
	}
	checkEnvelope(t, recv(t, h.got, "server -> agent envelope"), down)

	// The server dropping the connection is seen by the agent.
	_ = conn.Close()
	if id := recv(t, h.detached, "server detach"); id != agentID {
		t.Errorf("detached %q, want %q", id, agentID)
	}
}

func TestAgentDialSendBeforeConnect(t *testing.T) {
	h := newADHarness(t)
	d, err := NewAgentDialer(h.agentDialerConfig("127.0.0.1:1", h.agent, h.ca.CertPEM(), mainID))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Send(context.Background(), Envelope{Type: "x"}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Send before connect = %v, want ErrNotConnected", err)
	}
}

// The agent redials by itself, with backoff, once the server comes back.
func TestAgentDialerReconnectsAfterServerRestart(t *testing.T) {
	h := newADHarness(t)
	a1, stopA1 := h.startAcceptor("127.0.0.1:0", nil)
	addr := a1.Addr().String()
	d, dConn, stopD := h.runAgentDialer(h.agentDialerConfig(addr, h.agent, h.ca.CertPEM(), mainID))
	defer stopD()
	recv(t, dConn, "first handshake")
	recv(t, h.attached, "first attach")

	// Kill the server outright; the agent must notice and keep trying.
	stopA1()
	_ = a1.Close()
	deadline := time.Now().Add(wait)
	for d.Peer() != "" {
		if time.Now().After(deadline) {
			t.Fatal("agent still shows a connection after the server died")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // several failed attempts against the dead port

	// A new server on the same address: no help from anyone, the agent finds it.
	_, stopA2 := h.startAcceptor(addr, nil)
	defer stopA2()
	if got := recv(t, dConn, "second handshake"); got != mainID {
		t.Errorf("agent saw server %q after reconnect, want %q", got, mainID)
	}
	conn := recv(t, h.attached, "second attach")
	if err := d.Send(context.Background(), Envelope{Type: "test.after", ID: "r-1"}); err != nil {
		t.Fatalf("Send after reconnect: %v", err)
	}
	if got := recv(t, h.serverGot, "envelope after reconnect"); got.ID != "r-1" {
		t.Errorf("got %+v, want ID r-1", got)
	}
	if err := conn.Send(context.Background(), Envelope{Type: "test.back", ID: "r-2"}); err != nil {
		t.Fatalf("server Send after reconnect: %v", err)
	}
	if got := recv(t, h.got, "server envelope after reconnect"); got.ID != "r-2" {
		t.Errorf("got %+v, want ID r-2", got)
	}
}

// An agent whose certificate is from a different CA is rejected by the server
// even when the agent itself trusts the server's CA.
func TestAgentDialForeignCAAgentRejected(t *testing.T) {
	h := newADHarness(t)
	a, stopA := h.startAcceptor("127.0.0.1:0", nil)
	defer stopA()

	rogue := issue(t, newCA(t), agentID)
	d, err := NewAgentDialer(h.agentDialerConfig(a.Addr().String(), rogue, h.ca.CertPEM(), mainID))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if connected, err := d.runOnce(ctx); connected || err == nil {
		t.Fatalf("runOnce = (%v, %v), want a rejected connection", connected, err)
	}
	if d.Peer() != "" {
		t.Errorf("agent recorded server %q despite rejection", d.Peer())
	}
	select {
	case c := <-h.attached:
		t.Fatalf("server attached a connection from %q", c.AgentID())
	case <-time.After(200 * time.Millisecond):
	}
}

// And the reverse: the agent refuses a server whose certificate is from a
// different CA, or is not the server it expects.
func TestAgentDialRejectsForeignServer(t *testing.T) {
	t.Run("different CA", func(t *testing.T) {
		h := newADHarness(t)
		rogueCA := newCA(t)
		h.ca, h.main = rogueCA, issue(t, rogueCA, mainID)
		a, stopA := h.startAcceptor("127.0.0.1:0", nil)
		defer stopA()

		good := newADHarness(t)
		d, err := NewAgentDialer(good.agentDialerConfig(a.Addr().String(), good.agent, good.ca.CertPEM(), mainID))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		if connected, err := d.runOnce(ctx); connected || err == nil {
			t.Fatalf("runOnce = (%v, %v), want rejection", connected, err)
		}
	})
	t.Run("right CA, wrong server id", func(t *testing.T) {
		h := newADHarness(t)
		h.main = issue(t, h.ca, "someone-else")
		a, stopA := h.startAcceptor("127.0.0.1:0", nil)
		defer stopA()
		d, err := NewAgentDialer(h.agentDialerConfig(a.Addr().String(), h.agent, h.ca.CertPEM(), mainID))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		if connected, err := d.runOnce(ctx); connected || err == nil {
			t.Fatalf("runOnce = (%v, %v), want rejection", connected, err)
		}
	})
}

// hello must name the id the certificate carries; a valid agent cannot speak
// for another machine.
func TestAgentDialHelloMustMatchCertificate(t *testing.T) {
	h := newADHarness(t)
	a, stopA := h.startAcceptor("127.0.0.1:0", nil)
	defer stopA()
	cfg := h.agentDialerConfig(a.Addr().String(), h.agent, h.ca.CertPEM(), mainID)
	cfg.MachineID = "agent-02"
	d, err := NewAgentDialer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if connected, err := d.runOnce(ctx); connected || err == nil {
		t.Fatalf("runOnce = (%v, %v), want a failed handshake", connected, err)
	}
	select {
	case c := <-h.attached:
		t.Fatalf("server attached a connection from %q", c.AgentID())
	case <-time.After(200 * time.Millisecond):
	}
}

// Admit refuses an agent before any WebSocket is opened.
func TestAcceptorAdmitRefuses(t *testing.T) {
	h := newADHarness(t)
	var mu sync.Mutex
	var asked []string
	a, stopA := h.startAcceptor("127.0.0.1:0", func(id string) error {
		mu.Lock()
		asked = append(asked, id)
		mu.Unlock()
		return errors.New("not expected")
	})
	defer stopA()
	d, err := NewAgentDialer(h.agentDialerConfig(a.Addr().String(), h.agent, h.ca.CertPEM(), mainID))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if connected, err := d.runOnce(ctx); connected || err == nil {
		t.Fatalf("runOnce = (%v, %v), want a refused connection", connected, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != agentID {
		t.Errorf("Admit asked about %v, want [%s]", asked, agentID)
	}
	if len(h.attached) != 0 {
		t.Error("a refused agent was attached")
	}
}

func TestAcceptorCloseOnCancel(t *testing.T) {
	h := newADHarness(t)
	a, stopA := h.startAcceptor("127.0.0.1:0", nil)
	d, dConn, stopD := h.runAgentDialer(h.agentDialerConfig(a.Addr().String(), h.agent, h.ca.CertPEM(), mainID))
	defer stopD()
	recv(t, dConn, "handshake")
	stopA() // cancels Serve: the open connection must go with it
	deadline := time.Now().Add(wait)
	for d.Peer() != "" {
		if time.Now().After(deadline) {
			t.Fatal("agent still connected after the server stopped serving")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
