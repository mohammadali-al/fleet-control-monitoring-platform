package agent

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

// The main agent's heartbeat (T-0027). The agent writes <DataDir>/watchdog/
// heartbeat-agent every heartbeat_write seconds; fleet-agent-liveness.timer
// (every 45 s) restarts fleet-agent.service when the file is older than
// heartbeat_threshold, which catches an agent that is deadlocked yet still
// "active". The write interval and threshold are the pushed watchdog policy
// (the agent already stores it under DataDir), applied live.
//
// The write is tied to the agent's real state, not just to a running
// goroutine: before each beat it asks the agent whether its core locks can be
// taken (Alive). A deadlock there stops the beat, the file goes stale and the
// timer restarts the unit.

// aliver is implemented by every agent type below.
type aliver interface{ Alive() bool }

// Alive reports that the listener and worker locks can be taken. It blocks
// when they are deadlocked; RunHeartbeat bounds that wait.
func (a *Agent) Alive() bool {
	_ = a.ln.Peer()
	_ = a.w.current()
	return true
}

// Alive: see Agent.Alive.
func (d *DialingWorker) Alive() bool {
	_ = d.w.current()
	return true
}

// Alive: see Agent.Alive.
func (m *Monitor) Alive() bool {
	_ = m.ln.Peer()
	m.mu.Lock()
	m.mu.Unlock() // taking and releasing the lock is the probe
	return true
}

// Alive: see Agent.Alive.
func (m *DialingMonitor) Alive() bool {
	m.mu.Lock()
	m.mu.Unlock() // taking and releasing the lock is the probe
	return true
}

// HeartbeatOptions configures RunHeartbeat.
type HeartbeatOptions struct {
	// DataDir is the base data dir; the heartbeat goes to <DataDir>/watchdog.
	DataDir string
	// MachineID and Groups select the per-host and per-group policy overrides.
	MachineID string
	Groups    []string
	// Probe reports whether the agent is healthy enough to beat; nil means
	// always. It may block (a deadlock): it is run off the loop and given
	// ProbeTimeout (default 2 s). Only one probe is ever in flight.
	Probe        func() bool
	ProbeTimeout time.Duration
	Log          *slog.Logger
	// Now is the clock (tests).
	Now func() time.Time
}

// StartHeartbeat runs the heartbeat for a started Runner until ctx is
// cancelled, in its own goroutine. The agent's own Alive method, when it has
// one, is the probe. Without a usable data dir it logs and does nothing.
func StartHeartbeat(ctx context.Context, r Runner, ac config.AgentConfig, dataDir string, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if dataDir == "" {
		d, err := ac.ResolveDataDir()
		if err != nil {
			log.Warn("heartbeat disabled: no data dir", "component", "fleet-agent", "err", err)
			return
		}
		dataDir = d
	}
	opts := HeartbeatOptions{DataDir: dataDir, MachineID: ac.MachineID, Groups: ac.Watchdog.Groups, Log: log}
	if a, ok := r.(aliver); ok {
		opts.Probe = a.Alive
	}
	go RunHeartbeat(ctx, opts)
}

// RunHeartbeat writes the agent heartbeat until ctx is cancelled.
func RunHeartbeat(ctx context.Context, o HeartbeatOptions) {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	hb := watchdog.NewHeartbeat(watchdog.HeartbeatPath(o.DataDir, watchdog.HeartbeatAgent))
	if o.Now != nil {
		hb.Now = o.Now
	}
	set := &watchdog.HeartbeatSettings{DataDir: o.DataDir, MachineID: o.MachineID, Groups: o.Groups}
	pr := &prober{fn: o.Probe, timeout: o.ProbeTimeout}
	if pr.timeout <= 0 {
		pr.timeout = 2 * time.Second
	}
	var failing, wrote, stuck bool // log a state change once, not every beat
	for {
		write, threshold := set.Get()
		wait := write
		healthy := pr.ok(ctx)
		switch {
		case healthy:
			if stuck {
				log.Info("agent responsive again: heartbeat resumed", "component", "fleet-agent")
				stuck = false
			}
			next, err := hb.Tick(write, threshold)
			wait = next
			switch {
			case err != nil && !failing:
				log.Warn("heartbeat write failed", "component", "fleet-agent", "err", err)
				failing = true
			case err == nil && failing:
				log.Info("heartbeat writes recovered", "component", "fleet-agent")
				failing = false
			}
			if err == nil && !wrote {
				log.Info("heartbeat started", "component", "fleet-agent", "file", hb.Path(), "write", write, "threshold", threshold)
				wrote = true
			}
		case ctx.Err() == nil:
			if !stuck {
				log.Warn("agent unresponsive: heartbeat withheld", "component", "fleet-agent")
				stuck = true
			}
			wait = min(wait, time.Second) // look again soon: the agent may recover
		}
		if wait < 100*time.Millisecond {
			wait = 100 * time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// prober runs a health probe that might block forever, without ever piling up
// goroutines: while one is stuck, further asks fail immediately.
type prober struct {
	fn       func() bool
	timeout  time.Duration
	inflight atomic.Bool
}

func (p *prober) ok(ctx context.Context) bool {
	if p.fn == nil {
		return true
	}
	if !p.inflight.CompareAndSwap(false, true) {
		return false // the previous probe has not returned: still stuck
	}
	res := make(chan bool, 1)
	go func() {
		v := p.fn()
		p.inflight.Store(false)
		res <- v
	}()
	t := time.NewTimer(p.timeout)
	defer t.Stop()
	select {
	case v := <-res:
		return v
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}
