package channel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

// Reconnect and keepalive defaults.
const (
	defaultMinBackoff   = 500 * time.Millisecond
	defaultMaxBackoff   = 30 * time.Second
	defaultPingInterval = 20 * time.Second
	pingTimeout         = 10 * time.Second
)

// DialerConfig configures a main-server-side Dialer.
type DialerConfig struct {
	// Addr is the agent's host:port.
	Addr string
	// TLS comes from pki.DialTLSConfig with the agent's expected IP.
	TLS *tls.Config
	// MachineID is this (main) side's id, claimed in hello. It must equal the
	// CN of the main server's certificate; the agent checks that.
	MachineID string
	// Handler receives non-internal envelopes. May be nil.
	Handler Handler
	// OnConnect, if set, is called each time a handshake completes, with the
	// agent's verified id.
	OnConnect func(agentID string)
	// MinBackoff and MaxBackoff bound the reconnect delay (defaults 500ms and
	// 30s). The delay doubles per failed attempt, is capped, and has jitter.
	MinBackoff, MaxBackoff time.Duration
	// PingInterval is the keepalive period (default 20s). A missed pong drops
	// the connection so the reconnect loop can replace it.
	PingInterval time.Duration
}

// Dialer is the main-server side under server_dial: it dials the agent over
// mTLS, performs the hello handshake and keeps the connection up.
type Dialer struct {
	cfg DialerConfig

	mu      sync.Mutex
	conn    *websocket.Conn // active, handshaken connection
	agentID string          // verified id of the agent on conn
}

// NewDialer validates cfg and fills defaults. Call Run to start.
func NewDialer(cfg DialerConfig) (*Dialer, error) {
	if cfg.Addr == "" || cfg.TLS == nil {
		return nil, errors.New("channel: dialer needs an address and a TLS config")
	}
	if cfg.MachineID == "" {
		return nil, errors.New("channel: dialer needs a machine id")
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = defaultMinBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = cfg.MinBackoff
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = defaultPingInterval
	}
	return &Dialer{cfg: cfg}, nil
}

// Run connects and reconnects until ctx is cancelled, then returns nil. A
// connection that completed its handshake resets the backoff.
func (d *Dialer) Run(ctx context.Context) error {
	delay := d.cfg.MinBackoff
	for {
		connected, _ := d.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			delay = d.cfg.MinBackoff
		}
		select {
		case <-time.After(jitter(delay)):
		case <-ctx.Done():
			return nil
		}
		delay = min(delay*2, d.cfg.MaxBackoff)
	}
}

// jitter spreads d over [d/2, d] so a fleet of reconnecting dialers does not
// hit the agents in lockstep.
func jitter(d time.Duration) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + rand.N(half+1)
}

// AgentID returns the verified id of the connected agent, or "" if none.
func (d *Dialer) AgentID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.agentID
}

// Send writes an envelope to the connected agent.
func (d *Dialer) Send(ctx context.Context, env Envelope) error {
	d.mu.Lock()
	c := d.conn
	d.mu.Unlock()
	if c == nil {
		return ErrNotConnected
	}
	return WriteEnvelope(ctx, c, env)
}

// runOnce makes one connection attempt and serves it until it ends. connected
// reports whether the handshake completed.
func (d *Dialer) runOnce(ctx context.Context) (connected bool, err error) {
	tr := &http.Transport{TLSClientConfig: d.cfg.TLS.Clone(), DisableKeepAlives: true}
	defer tr.CloseIdleConnections()

	dctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	c, resp, err := websocket.Dial(dctx, "wss://"+d.cfg.Addr+Path, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: tr},
	})
	cancel()
	if err != nil {
		return false, fmt.Errorf("channel: dial: %w", err)
	}
	defer c.CloseNow()
	c.SetReadLimit(maxMessageBytes)

	// The agent's identity is the CN of its verified server cert, not anything
	// it says in a message.
	agentID, err := pki.PeerID(resp.TLS)
	if err != nil {
		return false, err
	}
	if err := d.handshake(ctx, c, agentID); err != nil {
		_ = c.Close(websocket.StatusPolicyViolation, "handshake failed")
		return false, err
	}

	d.mu.Lock()
	d.conn, d.agentID = c, agentID
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.conn, d.agentID = nil, ""
		d.mu.Unlock()
	}()
	if d.cfg.OnConnect != nil {
		d.cfg.OnConnect(agentID)
	}

	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	go d.keepAlive(sctx, c)
	return true, readLoop(sctx, c, d.cfg.Handler)
}

// handshake sends hello and expects hello-ack naming the agent's verified id.
func (d *Dialer) handshake(ctx context.Context, c *websocket.Conn, agentID string) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	hello, err := NewEnvelope(TypeHello, "hello", HelloPayload{MachineID: d.cfg.MachineID})
	if err != nil {
		return err
	}
	if err := WriteEnvelope(ctx, c, hello); err != nil {
		return err
	}
	ack, err := ReadEnvelope(ctx, c)
	if err != nil {
		return err
	}
	if ack.Type != TypeHelloAck {
		return fmt.Errorf("%w: reply is %q, want hello-ack", ErrBadHandshake, ack.Type)
	}
	claimed, err := helloID(ack)
	if err != nil {
		return err
	}
	if claimed != agentID {
		return fmt.Errorf("%w: hello-ack id %q does not match certificate id %q", ErrBadHandshake, claimed, agentID)
	}
	return nil
}

// keepAlive pings until ctx ends; a failed ping closes the connection, which
// ends readLoop and hands control back to the reconnect loop.
func (d *Dialer) keepAlive(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(d.cfg.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				_ = c.CloseNow()
				return
			}
		}
	}
}
