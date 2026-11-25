package channel

// The agent_dial direction (architecture 4.2): the agent dials OUT to the main
// server, which runs a WebSocket listener. Same Envelope, same hello/hello-ack
// handshake, same mTLS (internal/pki) and the same keepalive as server_dial;
// only who opens the TCP connection (and so who speaks first) is reversed:
//
//	agent -> server  hello     {machine_id: <agent id>}   (must equal the agent cert CN)
//	server -> agent  hello-ack {machine_id: <server id>}  (must equal the server cert CN)
//
// AgentDialer is the agent side (capped backoff + jitter reconnect, like
// Dialer). Acceptor is the main-server side. Once a connection is handshaken it
// is a *Conn, which has the same Send/handler semantics as a dialed one, so the
// fleet manager does not care which side opened it (see internal/fleet).

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

// AgentDialTLSConfig is the agent's TLS config under agent_dial: it presents
// the agent's certificate and trusts only the pinned CA. The server is
// identified by its certificate id, not by an address: serverID (its CN, which
// IssuePeer also puts in as a DNS SAN) is matched as the server name, so the
// server can be reached by any host name, IP or NAT'd address.
func AgentDialTLSConfig(certPEM, keyPEM, caPEM []byte, serverID string) (*tls.Config, error) {
	if serverID == "" {
		return nil, errors.New("channel: agent dial needs the server id")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("channel: load agent key pair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("channel: no valid CA certificate in caPEM")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverID,
	}, nil
}

// --- agent side -------------------------------------------------------------------

// AgentDialerConfig configures an AgentDialer.
type AgentDialerConfig struct {
	// Addr is the main server's agent_dial listener, host:port.
	Addr string
	// TLS comes from AgentDialTLSConfig.
	TLS *tls.Config
	// MachineID is this agent's id, claimed in hello. It must equal the CN of
	// the agent's certificate; the server checks that.
	MachineID string
	// Handler receives non-internal envelopes. May be nil.
	Handler Handler
	// OnConnect, if set, is called each time a handshake completes, with the
	// main server's verified id.
	OnConnect func(serverID string)
	// MinBackoff and MaxBackoff bound the reconnect delay (defaults 500ms and
	// 30s): it doubles per failed attempt, is capped, and has jitter.
	MinBackoff, MaxBackoff time.Duration
	// PingInterval is the keepalive period (default 20s). A missed pong drops
	// the connection so the reconnect loop can replace it.
	PingInterval time.Duration
}

// AgentDialer is the agent side under agent_dial: it dials the main server
// over mTLS, performs the hello handshake and keeps the connection up.
type AgentDialer struct {
	cfg AgentDialerConfig

	mu       sync.Mutex
	conn     *websocket.Conn // active, handshaken connection
	serverID string          // verified id of the server on conn
}

// NewAgentDialer validates cfg and fills defaults. Call Run to start.
func NewAgentDialer(cfg AgentDialerConfig) (*AgentDialer, error) {
	if cfg.Addr == "" || cfg.TLS == nil {
		return nil, errors.New("channel: agent dialer needs a server address and a TLS config")
	}
	if cfg.MachineID == "" {
		return nil, errors.New("channel: agent dialer needs a machine id")
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
	return &AgentDialer{cfg: cfg}, nil
}

// Addr is the server address being dialed.
func (d *AgentDialer) Addr() string { return d.cfg.Addr }

// Run connects and reconnects until ctx is cancelled, then returns nil. A
// connection that completed its handshake resets the backoff. The agent
// manages its own reconnection; the server keeps nothing for it.
func (d *AgentDialer) Run(ctx context.Context) error {
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

// Peer returns the verified id of the connected main server, or "" if none.
func (d *AgentDialer) Peer() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.serverID
}

// Send writes an envelope to the connected main server.
func (d *AgentDialer) Send(ctx context.Context, env Envelope) error {
	d.mu.Lock()
	c := d.conn
	d.mu.Unlock()
	if c == nil {
		return ErrNotConnected
	}
	return WriteEnvelope(ctx, c, env)
}

func (d *AgentDialer) runOnce(ctx context.Context) (connected bool, err error) {
	tr := &http.Transport{TLSClientConfig: d.cfg.TLS.Clone(), DisableKeepAlives: true}
	defer tr.CloseIdleConnections()

	dctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	c, resp, err := websocket.Dial(dctx, "wss://"+d.cfg.Addr+Path, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: tr},
	})
	cancel()
	if err != nil {
		return false, fmt.Errorf("channel: agent dial: %w", err)
	}
	defer c.CloseNow()
	c.SetReadLimit(maxMessageBytes)

	// The server's identity is the CN of its verified certificate, not anything
	// it says in a message.
	serverID, err := pki.PeerID(resp.TLS)
	if err != nil {
		return false, err
	}
	if err := d.handshake(ctx, c, serverID); err != nil {
		_ = c.Close(websocket.StatusPolicyViolation, "handshake failed")
		return false, err
	}

	d.mu.Lock()
	d.conn, d.serverID = c, serverID
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.conn, d.serverID = nil, ""
		d.mu.Unlock()
	}()
	if d.cfg.OnConnect != nil {
		d.cfg.OnConnect(serverID)
	}

	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	go pingLoop(sctx, c, d.cfg.PingInterval)
	return true, readLoop(sctx, c, d.cfg.Handler)
}

// handshake sends hello and expects hello-ack naming the server's verified id.
func (d *AgentDialer) handshake(ctx context.Context, c *websocket.Conn, serverID string) error {
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
	if claimed != serverID {
		return fmt.Errorf("%w: hello-ack id %q does not match certificate id %q", ErrBadHandshake, claimed, serverID)
	}
	return nil
}

// pingLoop pings until ctx ends; a failed ping closes the connection, which
// ends readLoop and hands control back to the reconnect loop (or the
// acceptor's handler).
func pingLoop(ctx context.Context, c *websocket.Conn, every time.Duration) {
	t := time.NewTicker(every)
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

// --- main-server side -------------------------------------------------------------

// Conn is one accepted, handshaken agent-initiated connection. It is what an
// Acceptor hands to its owner; Send has the same semantics as a dialed
// connection's.
type Conn struct {
	c  *websocket.Conn
	id string
}

// AgentID is the agent's verified id (its certificate CN).
func (c *Conn) AgentID() string { return c.id }

// Send writes an envelope to the agent.
func (c *Conn) Send(ctx context.Context, env Envelope) error { return WriteEnvelope(ctx, c.c, env) }

// Close drops the connection.
func (c *Conn) Close() error { return c.c.CloseNow() }

// AcceptorConfig configures an Acceptor.
type AcceptorConfig struct {
	// Addr is the listen address, e.g. ":7444" or "127.0.0.1:0".
	Addr string
	// TLS comes from pki.ServerTLSConfig (client certs required, the internal
	// CA the only trust anchor).
	TLS *tls.Config
	// ServerID is the main server's id, sent in hello-ack. It must equal the CN
	// of the server certificate in TLS; the agent checks that.
	ServerID string
	// Admit, if set, is asked before the WebSocket upgrade whether the agent
	// with this verified id may connect at all. A non-nil error refuses it
	// with HTTP 403. The id comes from the verified client certificate.
	Admit func(agentID string) error
	// Attach is called once the handshake is complete. It returns the handler
	// for the agent's non-internal envelopes and a function called when the
	// connection ends. A non-nil error drops the connection.
	Attach func(agentID string, c *Conn) (Handler, func(), error)
	// PingInterval is the keepalive period (default 20s).
	PingInterval time.Duration
}

// Acceptor is the main-server side under agent_dial: an mTLS WebSocket server
// that accepts agent-initiated connections, identifies each agent by its
// verified client certificate and hands it to Attach.
type Acceptor struct {
	cfg AcceptorConfig
	ln  net.Listener

	mu    sync.Mutex
	conns map[*Conn]struct{}
}

// Accept binds the address. Call Serve to start accepting.
func Accept(cfg AcceptorConfig) (*Acceptor, error) {
	if cfg.TLS == nil {
		return nil, errors.New("channel: acceptor needs a TLS config")
	}
	if cfg.ServerID == "" {
		return nil, errors.New("channel: acceptor needs the server id")
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = defaultPingInterval
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("channel: listen for agents: %w", err)
	}
	return &Acceptor{cfg: cfg, ln: ln, conns: map[*Conn]struct{}{}}, nil
}

// Addr is the bound address (useful with port 0).
func (a *Acceptor) Addr() net.Addr { return a.ln.Addr() }

// Serve accepts connections until ctx is cancelled or Close is called. It
// closes the listener and every open connection before returning.
func (a *Acceptor) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc(Path, a.handle)
	srv := &http.Server{
		Handler:           mux,
		TLSConfig:         a.cfg.TLS,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: handshakeTimeout,
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
			a.closeConns()
		case <-stop:
		}
	}()
	err := srv.ServeTLS(a.ln, "", "")
	a.closeConns()
	if ctx.Err() != nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Close stops accepting and drops every open connection.
func (a *Acceptor) Close() error {
	err := a.ln.Close()
	a.closeConns()
	return err
}

func (a *Acceptor) closeConns() {
	a.mu.Lock()
	cs := make([]*Conn, 0, len(a.conns))
	for c := range a.conns {
		cs = append(cs, c)
	}
	a.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
}

func (a *Acceptor) handle(w http.ResponseWriter, r *http.Request) {
	// The TLS config already required and verified a client certificate;
	// PeerID reads only the verified chain.
	id, err := pki.PeerID(r.TLS)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if a.cfg.Admit != nil {
		if err := a.cfg.Admit(id); err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already wrote the HTTP error
	}
	defer c.CloseNow()
	c.SetReadLimit(maxMessageBytes)
	conn := &Conn{c: c, id: id}
	a.mu.Lock()
	a.conns[conn] = struct{}{}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.conns, conn)
		a.mu.Unlock()
	}()
	ctx := r.Context()

	if err := a.handshake(ctx, c, id); err != nil {
		_ = c.Close(websocket.StatusPolicyViolation, "handshake failed")
		return
	}
	var h Handler
	if a.cfg.Attach != nil {
		var detach func()
		h, detach, err = a.cfg.Attach(id, conn)
		if err != nil {
			_ = c.Close(websocket.StatusPolicyViolation, "not accepted")
			return
		}
		if detach != nil {
			defer detach()
		}
	}
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	go pingLoop(pctx, c, a.cfg.PingInterval)
	_ = readLoop(pctx, c, h)
}

// handshake expects hello from the agent (its id must match its certificate)
// and answers hello-ack with the server's id.
func (a *Acceptor) handshake(ctx context.Context, c *websocket.Conn, id string) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	hello, err := ReadEnvelope(ctx, c)
	if err != nil {
		return err
	}
	if hello.Type != TypeHello {
		return fmt.Errorf("%w: first message is %q, want hello", ErrBadHandshake, hello.Type)
	}
	claimed, err := helloID(hello)
	if err != nil {
		return err
	}
	if claimed != id {
		return fmt.Errorf("%w: hello id %q does not match certificate id %q", ErrBadHandshake, claimed, id)
	}
	ack, err := NewEnvelope(TypeHelloAck, hello.ID, HelloPayload{MachineID: a.cfg.ServerID})
	if err != nil {
		return err
	}
	return WriteEnvelope(ctx, c, ack)
}
