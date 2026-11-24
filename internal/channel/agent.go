package channel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

// ListenerConfig configures an agent-side Listener.
type ListenerConfig struct {
	// Addr is the listen address, e.g. "0.0.0.0:7443" or "127.0.0.1:0".
	Addr string
	// TLS comes from pki.ServerTLSConfig (client certs required).
	TLS *tls.Config
	// MachineID is this agent's id, sent in hello-ack. It must equal the CN of
	// the agent's certificate; the main server checks that.
	MachineID string
	// Handler receives non-internal envelopes. May be nil.
	Handler Handler
	// OnConnect, if set, is called once the handshake with the main server
	// completes, with the main server's verified id.
	OnConnect func(peerID string)
}

// Listener is the agent side under server_dial: an mTLS WebSocket server that
// accepts one main-server connection at a time.
type Listener struct {
	cfg ListenerConfig
	ln  net.Listener

	mu     sync.Mutex
	conn   *websocket.Conn // active, handshaken connection
	peer   string          // verified id of the active peer
	active bool            // a connection is in flight (even before handshake)
}

// Listen binds the address. Call Serve to start accepting.
func Listen(cfg ListenerConfig) (*Listener, error) {
	if cfg.TLS == nil {
		return nil, errors.New("channel: listener needs a TLS config")
	}
	if cfg.MachineID == "" {
		return nil, errors.New("channel: listener needs a machine id")
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("channel: listen: %w", err)
	}
	return &Listener{cfg: cfg, ln: ln}, nil
}

// Addr is the bound address (useful with port 0).
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Serve accepts connections until ctx is cancelled or Close is called. It
// closes the listener and any active connection before returning.
func (l *Listener) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc(Path, l.handle)
	srv := &http.Server{
		Handler:     mux,
		TLSConfig:   l.cfg.TLS,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
			l.closeConn()
		case <-stop:
		}
	}()
	err := srv.ServeTLS(l.ln, "", "")
	l.closeConn()
	if ctx.Err() != nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Close stops accepting and drops the active connection.
func (l *Listener) Close() error {
	err := l.ln.Close()
	l.closeConn()
	return err
}

// Peer returns the verified id of the connected main server, or "" if none.
func (l *Listener) Peer() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.peer
}

// Send writes an envelope to the connected main server.
func (l *Listener) Send(ctx context.Context, env Envelope) error {
	l.mu.Lock()
	c := l.conn
	l.mu.Unlock()
	if c == nil {
		return ErrNotConnected
	}
	return WriteEnvelope(ctx, c, env)
}

func (l *Listener) closeConn() {
	l.mu.Lock()
	c := l.conn
	l.mu.Unlock()
	if c != nil {
		_ = c.CloseNow()
	}
}

func (l *Listener) handle(w http.ResponseWriter, r *http.Request) {
	// The TLS config already required and verified a client cert; PeerID reads
	// only the verified chain.
	peer, err := pki.PeerID(r.TLS)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	l.mu.Lock()
	if l.active {
		l.mu.Unlock()
		http.Error(w, "already connected", http.StatusConflict)
		return
	}
	l.active = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.active, l.conn, l.peer = false, nil, ""
		l.mu.Unlock()
	}()

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already wrote the HTTP error
	}
	defer c.CloseNow()
	c.SetReadLimit(maxMessageBytes)
	ctx := r.Context()

	if err := l.handshake(ctx, c, peer); err != nil {
		_ = c.Close(websocket.StatusPolicyViolation, "handshake failed")
		return
	}
	l.mu.Lock()
	l.conn, l.peer = c, peer
	l.mu.Unlock()
	if l.cfg.OnConnect != nil {
		l.cfg.OnConnect(peer)
	}

	_ = readLoop(ctx, c, l.cfg.Handler)
}

// handshake expects hello from the main server and answers hello-ack.
func (l *Listener) handshake(ctx context.Context, c *websocket.Conn, peer string) error {
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
	if claimed != peer {
		return fmt.Errorf("%w: hello id %q does not match certificate id %q", ErrBadHandshake, claimed, peer)
	}
	ack, err := NewEnvelope(TypeHelloAck, hello.ID, HelloPayload{MachineID: l.cfg.MachineID})
	if err != nil {
		return err
	}
	return WriteEnvelope(ctx, c, ack)
}
