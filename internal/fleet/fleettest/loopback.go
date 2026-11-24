// Package fleettest provides an in-process stand-in for the control channel so
// tests of the fleet manager and the panel can drive a real exec.Executor (real
// shell, real PTY, the real dangerous-command gate) without sockets or TLS.
// It is test support only; production code never imports it.
package fleettest

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// Loopback builds links whose far end is an Executor in this process.
type Loopback struct {
	gate *exec.Gate

	mu      sync.Mutex
	links   map[string]*link
	offline map[string]bool // machines to start offline
}

// NewLoopback returns a Loopback whose executors enforce gate (the same gate,
// hence the same secret, the manager mints tokens with).
func NewLoopback(gate *exec.Gate) *Loopback {
	return &Loopback{gate: gate, links: map[string]*link{}, offline: map[string]bool{}}
}

// StartOffline makes the next link for id start disconnected.
func (l *Loopback) StartOffline(id string) {
	l.mu.Lock()
	l.offline[id] = true
	l.mu.Unlock()
}

// SetOnline connects or drops the link to a machine, as if its agent came up or
// the network failed.
func (l *Loopback) SetOnline(id string, up bool) {
	l.mu.Lock()
	lk := l.links[id]
	l.mu.Unlock()
	if lk != nil {
		lk.mu.Lock()
		lk.up = up
		lk.mu.Unlock()
	}
}

// Factory is the fleet.LinkFactory.
func (l *Loopback) Factory() fleet.LinkFactory {
	return func(mc registry.Machine, h channel.Handler) (fleet.Link, error) {
		lk := &link{h: h, up: true}
		l.mu.Lock()
		if l.offline[mc.ID] {
			lk.up = false
			delete(l.offline, mc.ID)
		}
		l.links[mc.ID] = lk
		l.mu.Unlock()
		ex, err := exec.New(exec.Config{
			MachineID: mc.ID, Host: "loopback-" + mc.ID, Sender: agentSide{lk}, Gate: l.gate,
			TermGrace: 300 * time.Millisecond,
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			return nil, err
		}
		lk.ex = ex
		return lk, nil
	}
}

type link struct {
	ex *exec.Executor
	h  channel.Handler

	mu sync.Mutex // guards up, and serialises delivery to h like a single read loop
	up bool
}

func (k *link) Online() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.up
}

// Send is server -> agent.
func (k *link) Send(ctx context.Context, env channel.Envelope) error {
	if !k.Online() {
		return channel.ErrNotConnected
	}
	k.ex.Handle(ctx, env)
	return nil
}

func (k *link) Run(ctx context.Context) error {
	<-ctx.Done()
	k.ex.Shutdown()
	return nil
}

// agentSide is agent -> server.
type agentSide struct{ k *link }

func (a agentSide) Send(ctx context.Context, env channel.Envelope) error {
	a.k.mu.Lock()
	defer a.k.mu.Unlock()
	if !a.k.up {
		return channel.ErrNotConnected
	}
	a.k.h(ctx, env)
	return nil
}
