package fleet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// AgentListener is the main server's side of agent_dial: an mTLS WebSocket
// listener the agents dial OUT to. It identifies a connecting agent by its
// verified client certificate and attaches the connection to that machine's
// Link, which is the same Link interface a dialed (server_dial) connection
// implements: the Manager does not branch on direction.
//
// Only machines the Manager has a link for (registered, provisioned,
// agent_dial) are admitted; any other id gets HTTP 403.
type AgentListener struct {
	acc *channel.Acceptor

	mu    sync.Mutex
	links map[string]*inboundLink
}

// AgentListenerConfig configures an AgentListener.
type AgentListenerConfig struct {
	// Addr is the listen address, e.g. ":7444" or "127.0.0.1:0".
	Addr string
	// CA issues the listener's certificate and is the only trust anchor for
	// agent certificates.
	CA *pki.CA
	// ServerID is the main server's identity (its certificate CN); agents
	// verify it. Required (config.ServerConfig.ServerIDOrDefault).
	ServerID string
}

// ListenAgents binds the agent_dial listener. Pass the result to
// Config.AgentDial; the Manager serves it from Start.
func ListenAgents(cfg AgentListenerConfig) (*AgentListener, error) {
	if cfg.CA == nil || cfg.ServerID == "" {
		return nil, errors.New("fleet: agent listener needs a CA and a server id")
	}
	certPEM, keyPEM, err := cfg.CA.IssuePeer(cfg.ServerID, nil, serverCertValidity)
	if err != nil {
		return nil, fmt.Errorf("fleet: issue the server's agent_dial certificate: %w", err)
	}
	tlsCfg, err := pki.ServerTLSConfig(certPEM, keyPEM, cfg.CA.CertPEM())
	if err != nil {
		return nil, err
	}
	l := &AgentListener{links: map[string]*inboundLink{}}
	l.acc, err = channel.Accept(channel.AcceptorConfig{
		Addr:     cfg.Addr,
		TLS:      tlsCfg,
		ServerID: cfg.ServerID,
		Admit:    l.admit,
		Attach:   l.attach,
	})
	if err != nil {
		return nil, err
	}
	return l, nil
}

// Addr is the bound address (useful with port 0).
func (l *AgentListener) Addr() net.Addr { return l.acc.Addr() }

// Serve accepts agents until ctx is cancelled.
func (l *AgentListener) Serve(ctx context.Context) error { return l.acc.Serve(ctx) }

// Close stops the listener and drops every agent connection.
func (l *AgentListener) Close() error { return l.acc.Close() }

// Factory is the LinkFactory for agent_dial machines: the link it returns is
// fed by whatever connection the machine's agent opens.
func (l *AgentListener) Factory() LinkFactory {
	return func(mc registry.Machine, h channel.Handler) (Link, error) {
		k := &inboundLink{l: l, id: mc.ID, h: h}
		l.mu.Lock()
		l.links[mc.ID] = k
		l.mu.Unlock()
		return k, nil
	}
}

func (l *AgentListener) lookup(id string) *inboundLink {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.links[id]
}

func (l *AgentListener) admit(id string) error {
	if l.lookup(id) == nil {
		return fmt.Errorf("fleet: %q is not a machine expected to dial in", id)
	}
	return nil
}

func (l *AgentListener) attach(id string, c *channel.Conn) (channel.Handler, func(), error) {
	k := l.lookup(id)
	if k == nil {
		return nil, nil, fmt.Errorf("fleet: %q is not a machine expected to dial in", id)
	}
	return k.attach(c)
}

// EnableAgentDial binds the agent_dial listener on addr, using the internal CA
// the onboarding provisioner holds, and makes the manager serve it from Start:
// agent_dial machines then get links fed by the connections their agents open.
// Call it before Start; server_dial machines are unaffected. Without it
// agent_dial machines stay offline with a note saying so.
func (m *Manager) EnableAgentDial(addr, serverID string) error {
	if m.cfg.Provisioner == nil || m.cfg.Provisioner.CA == nil {
		return ErrUnavailable
	}
	l, err := ListenAgents(AgentListenerConfig{Addr: addr, CA: m.cfg.Provisioner.CA, ServerID: serverID})
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx != nil || m.closed {
		_ = l.Close()
		return errors.New("fleet: EnableAgentDial must be called before Start")
	}
	m.cfg.AgentDial = l
	return nil
}

// AgentDialAddr is the bound address of the agent_dial listener, or nil when
// it is not enabled.
func (m *Manager) AgentDialAddr() net.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg.AgentDial == nil {
		return nil
	}
	return m.cfg.AgentDial.Addr()
}

// inboundLink is a Link whose connection is opened by the agent. Run just
// waits (the agent does its own reconnecting); Online is true while an
// accepted connection is attached.
type inboundLink struct {
	l  *AgentListener
	id string
	h  channel.Handler

	mu     sync.Mutex
	conn   *channel.Conn
	closed bool
}

// attach makes c the link's connection. A newer connection replaces an older
// one (an agent that reconnects before the server noticed the old one died).
func (k *inboundLink) attach(c *channel.Conn) (channel.Handler, func(), error) {
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return nil, nil, errors.New("fleet: link is closed")
	}
	old := k.conn
	k.conn = c
	k.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return k.h, func() {
		k.mu.Lock()
		if k.conn == c {
			k.conn = nil
		}
		k.mu.Unlock()
	}, nil
}

func (k *inboundLink) Send(ctx context.Context, env channel.Envelope) error {
	k.mu.Lock()
	c := k.conn
	k.mu.Unlock()
	if c == nil {
		return channel.ErrNotConnected
	}
	return c.Send(ctx, env)
}

func (k *inboundLink) Online() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.conn != nil
}

// Run holds the link open until ctx ends, then detaches it from the listener
// and drops its connection.
func (k *inboundLink) Run(ctx context.Context) error {
	<-ctx.Done()
	k.l.mu.Lock()
	if k.l.links[k.id] == k {
		delete(k.l.links, k.id)
	}
	k.l.mu.Unlock()
	k.mu.Lock()
	k.closed = true
	c := k.conn
	k.conn = nil
	k.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	return nil
}
