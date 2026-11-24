package channel

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

const (
	agentID = "agent-01"
	mainID  = "main-server"
	wait    = 10 * time.Second
)

var loopback = net.ParseIP("127.0.0.1")

type identity struct{ cert, key []byte }

func newCA(t *testing.T) *pki.CA {
	t.Helper()
	ca, err := pki.NewCA(pki.Options{})
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	return ca
}

func issue(t *testing.T, ca *pki.CA, id string) identity {
	t.Helper()
	c, k, err := ca.IssuePeer(id, []net.IP{loopback}, time.Hour)
	if err != nil {
		t.Fatalf("IssuePeer(%s): %v", id, err)
	}
	return identity{c, k}
}

// harness is a Listener plus its Dialer config, on a throwaway CA.
type harness struct {
	t         *testing.T
	ca        *pki.CA
	agent     identity
	main      identity
	got       chan Envelope // listener-side handler
	dialerGot chan Envelope // dialer-side handler
}

func newHarness(t *testing.T) *harness {
	ca := newCA(t)
	return &harness{
		t: t, ca: ca,
		agent:     issue(t, ca, agentID),
		main:      issue(t, ca, mainID),
		got:       make(chan Envelope, 16),
		dialerGot: make(chan Envelope, 16),
	}
}

// startListener binds addr (use "127.0.0.1:0" for ephemeral), serves in the
// background, and returns the listener plus a channel that fires on each
// handshake, carrying the verified peer id. stop cancels and waits for Serve.
func (h *harness) startListener(addr string) (l *Listener, connected <-chan string, stop func()) {
	h.t.Helper()
	tlsCfg, err := pki.ServerTLSConfig(h.agent.cert, h.agent.key, h.ca.CertPEM())
	if err != nil {
		h.t.Fatalf("ServerTLSConfig: %v", err)
	}
	conn := make(chan string, 8)
	l, err = Listen(ListenerConfig{
		Addr: addr, TLS: tlsCfg, MachineID: agentID,
		Handler:   func(_ context.Context, env Envelope) { h.got <- env },
		OnConnect: func(id string) { conn <- id },
	})
	if err != nil {
		h.t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = l.Serve(ctx) }()
	return l, conn, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait):
			h.t.Error("Serve did not return after cancel")
		}
	}
}

func (h *harness) dialerConfig(addr string, id identity, caPEM []byte) DialerConfig {
	h.t.Helper()
	tlsCfg, err := pki.DialTLSConfig(id.cert, id.key, caPEM, loopback)
	if err != nil {
		h.t.Fatalf("DialTLSConfig: %v", err)
	}
	return DialerConfig{
		Addr: addr, TLS: tlsCfg, MachineID: mainID,
		Handler:    func(_ context.Context, env Envelope) { h.dialerGot <- env },
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
	}
}

// runDialer starts a Dialer in the background; connected fires on handshake.
func (h *harness) runDialer(cfg DialerConfig) (d *Dialer, connected <-chan string, stop func()) {
	h.t.Helper()
	conn := make(chan string, 8)
	cfg.OnConnect = func(id string) { conn <- id }
	d, err := NewDialer(cfg)
	if err != nil {
		h.t.Fatalf("NewDialer: %v", err)
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

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(wait):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func TestHandshakeVerifiedIDs(t *testing.T) {
	h := newHarness(t)
	l, lConn, stopL := h.startListener("127.0.0.1:0")
	defer stopL()
	d, dConn, stopD := h.runDialer(h.dialerConfig(l.Addr().String(), h.main, h.ca.CertPEM()))
	defer stopD()

	if got := recv(t, lConn, "listener handshake"); got != mainID {
		t.Errorf("listener saw peer %q, want %q", got, mainID)
	}
	if got := recv(t, dConn, "dialer handshake"); got != agentID {
		t.Errorf("dialer saw agent %q, want %q", got, agentID)
	}
	if got := d.AgentID(); got != agentID {
		t.Errorf("Dialer.AgentID() = %q, want %q", got, agentID)
	}
	if got := l.Peer(); got != mainID {
		t.Errorf("Listener.Peer() = %q, want %q", got, mainID)
	}
}

func TestPingPong(t *testing.T) {
	h := newHarness(t)
	l, _, stopL := h.startListener("127.0.0.1:0")
	defer stopL()
	d, dConn, stopD := h.runDialer(h.dialerConfig(l.Addr().String(), h.main, h.ca.CertPEM()))
	defer stopD()
	recv(t, dConn, "handshake")

	ctx := context.Background()
	// main -> agent
	if err := d.Send(ctx, Envelope{Type: TypePing, ID: "p1"}); err != nil {
		t.Fatalf("Send ping: %v", err)
	}
	if pong := recv(t, h.dialerGot, "pong"); pong.Type != TypePong || pong.ID != "p1" {
		t.Errorf("got %+v, want pong p1", pong)
	}
	// agent -> main
	if err := l.Send(ctx, Envelope{Type: TypePing, ID: "p2"}); err != nil {
		t.Fatalf("Listener.Send ping: %v", err)
	}
	if pong := recv(t, h.got, "pong"); pong.Type != TypePong || pong.ID != "p2" {
		t.Errorf("got %+v, want pong p2", pong)
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	h := newHarness(t)
	l, lConn, stopL := h.startListener("127.0.0.1:0")
	defer stopL()
	d, dConn, stopD := h.runDialer(h.dialerConfig(l.Addr().String(), h.main, h.ca.CertPEM()))
	defer stopD()
	recv(t, lConn, "listener handshake")
	recv(t, dConn, "dialer handshake")

	ctx := context.Background()
	payload := json.RawMessage(`{"cmd":"noop","args":[1,2,3],"nested":{"ok":true}}`)

	// main -> agent
	want := Envelope{Type: "test.echo", ID: "e-1", Payload: payload}
	if err := d.Send(ctx, want); err != nil {
		t.Fatalf("Send: %v", err)
	}
	checkEnvelope(t, recv(t, h.got, "envelope at agent"), want)

	// agent -> main
	back := Envelope{Type: "test.reply", ID: "e-2", Payload: json.RawMessage(`"hello"`)}
	if err := l.Send(ctx, back); err != nil {
		t.Fatalf("Listener.Send: %v", err)
	}
	checkEnvelope(t, recv(t, h.dialerGot, "envelope at main"), back)

	// No payload stays no payload.
	bare := Envelope{Type: "test.bare", ID: "e-3"}
	if err := d.Send(ctx, bare); err != nil {
		t.Fatalf("Send bare: %v", err)
	}
	checkEnvelope(t, recv(t, h.got, "bare envelope"), bare)
}

func checkEnvelope(t *testing.T, got, want Envelope) {
	t.Helper()
	if got.Type != want.Type || got.ID != want.ID {
		t.Errorf("got type/id %q/%q, want %q/%q", got.Type, got.ID, want.Type, want.ID)
	}
	if string(got.Payload) != string(want.Payload) {
		t.Errorf("got payload %s, want %s", got.Payload, want.Payload)
	}
}

func TestSendBeforeConnect(t *testing.T) {
	h := newHarness(t)
	d, err := NewDialer(h.dialerConfig("127.0.0.1:1", h.main, h.ca.CertPEM()))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Send(context.Background(), Envelope{Type: "x"}); err != ErrNotConnected {
		t.Errorf("Send = %v, want ErrNotConnected", err)
	}
}

func TestReconnectAfterListenerRestart(t *testing.T) {
	h := newHarness(t)
	l1, lConn1, stopL1 := h.startListener("127.0.0.1:0")
	addr := l1.Addr().String()
	d, dConn, stopD := h.runDialer(h.dialerConfig(addr, h.main, h.ca.CertPEM()))
	defer stopD()
	recv(t, lConn1, "first listener handshake")
	recv(t, dConn, "first dialer handshake")

	// Kill the listener (and its connection) outright.
	stopL1()
	_ = l1.Close()
	deadline := time.Now().Add(wait)
	for d.AgentID() != "" {
		if time.Now().After(deadline) {
			t.Fatal("dialer still shows a connection after the listener died")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Bring a new listener up on the same address; the dialer must find it.
	l2, lConn2, stopL2 := h.startListener(addr)
	defer stopL2()
	if got := recv(t, lConn2, "second listener handshake"); got != mainID {
		t.Errorf("second listener saw peer %q, want %q", got, mainID)
	}
	if got := recv(t, dConn, "second dialer handshake"); got != agentID {
		t.Errorf("dialer saw agent %q after reconnect, want %q", got, agentID)
	}

	// And the new connection works.
	if err := d.Send(context.Background(), Envelope{Type: "test.after", ID: "r-1"}); err != nil {
		t.Fatalf("Send after reconnect: %v", err)
	}
	if got := recv(t, h.got, "envelope after reconnect"); got.ID != "r-1" {
		t.Errorf("got %+v, want ID r-1", got)
	}
	_ = l2
}

func TestForeignCADialerRejected(t *testing.T) {
	h := newHarness(t)
	l, lConn, stopL := h.startListener("127.0.0.1:0")
	defer stopL()

	// The dialer trusts our CA (so the agent's cert verifies) but presents a
	// client cert issued by a different CA: only the listener can reject it.
	rogue := issue(t, newCA(t), mainID)
	d, err := NewDialer(h.dialerConfig(l.Addr().String(), rogue, h.ca.CertPEM()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	connected, err := d.runOnce(ctx)
	if connected || err == nil {
		t.Fatalf("runOnce = (%v, %v), want a rejected connection", connected, err)
	}
	if d.AgentID() != "" {
		t.Errorf("dialer recorded agent %q despite rejection", d.AgentID())
	}
	select {
	case id := <-lConn:
		t.Fatalf("listener completed a handshake with %q", id)
	case <-time.After(200 * time.Millisecond):
	}
	if l.Peer() != "" {
		t.Errorf("listener recorded peer %q", l.Peer())
	}
}

func TestDialerRejectsForeignAgent(t *testing.T) {
	// The reverse direction: the agent's cert is from another CA.
	h := newHarness(t)
	rogueCA := newCA(t)
	h.ca, h.agent = rogueCA, issue(t, rogueCA, agentID)
	l, _, stopL := h.startListener("127.0.0.1:0")
	defer stopL()

	good := newHarness(t)
	d, err := NewDialer(good.dialerConfig(l.Addr().String(), good.main, good.ca.CertPEM()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if connected, err := d.runOnce(ctx); connected || err == nil {
		t.Fatalf("runOnce = (%v, %v), want rejection", connected, err)
	}
}

func TestSecondConnectionRefusedWhileBusy(t *testing.T) {
	h := newHarness(t)
	l, lConn, stopL := h.startListener("127.0.0.1:0")
	defer stopL()
	_, dConn, stopD := h.runDialer(h.dialerConfig(l.Addr().String(), h.main, h.ca.CertPEM()))
	defer stopD()
	recv(t, lConn, "handshake")
	recv(t, dConn, "dialer handshake")

	d2, err := NewDialer(h.dialerConfig(l.Addr().String(), h.main, h.ca.CertPEM()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if connected, err := d2.runOnce(ctx); connected || err == nil {
		t.Fatalf("second connection = (%v, %v), want refusal", connected, err)
	}
	if got := l.Peer(); got != mainID {
		t.Errorf("first connection disturbed: Peer() = %q", got)
	}
}

func TestJitterBounds(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := 800 * time.Millisecond
		if got := jitter(d); got < d/2 || got > d {
			t.Fatalf("jitter(%v) = %v, outside [%v, %v]", d, got, d/2, d)
		}
	}
}

func TestListenerCloseOnCancel(t *testing.T) {
	h := newHarness(t)
	l, lConn, stopL := h.startListener("127.0.0.1:0")
	d, dConn, stopD := h.runDialer(h.dialerConfig(l.Addr().String(), h.main, h.ca.CertPEM()))
	defer stopD()
	recv(t, lConn, "handshake")
	recv(t, dConn, "dialer handshake")
	stopL() // asserts Serve returns with a live connection
	if _, err := net.DialTimeout("tcp", l.Addr().String(), time.Second); err == nil {
		t.Error("listener still accepting after cancel")
	}
	_ = d
}
