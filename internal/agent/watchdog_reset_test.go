package agent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

// The operator's counter reset (T-0028): the server pushes watchdog-reset, the
// main agent leaves a request in the shared data dir, and the watchdog (a
// separate process in production, the loop object here) clears its daily
// counters and alert-only latch at its next sample, keeping the cooldowns, and
// reports a counters-reset event that reaches the server.
func TestCounterResetPushedFromServerClearsWatchdogState(t *testing.T) {
	e := newWdEnv(t)

	// A watchdog that exhausted its caps: counters up, alert-only latched.
	now := time.Now()
	g := e.wd.Guards()
	for _, step := range []watchdog.Step{watchdog.StepRestart, watchdog.StepRestart, watchdog.StepReboot} {
		if err := g.Record(step, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.SetAlertOnly(true); err != nil {
		t.Fatal(err)
	}
	before := g.Snapshot()
	if before.Restarts != 2 || before.Reboots != 1 || !before.AlertOnly || before.Stage == watchdog.StepNone {
		t.Fatalf("setup state %+v", before)
	}

	e.startServer(t)
	eventually(t, "agent online", func() bool {
		for _, mi := range e.m.Machines() {
			if mi.ID == "w1" {
				return mi.State == fleet.StateOnline
			}
		}
		return false
	})
	if err := e.m.ResetCounters(context.Background(), "w1"); err != nil {
		t.Fatalf("ResetCounters: %v", err)
	}
	eventually(t, "the main agent to leave the reset request", func() bool { return watchdog.ResetRequested(e.dir) })

	// Nothing changes until the watchdog takes its next sample.
	if st := g.Snapshot(); st.Restarts != 2 || !st.AlertOnly {
		t.Fatalf("state changed before the watchdog sampled: %+v", st)
	}
	e.wd.Step(context.Background())
	after := g.Snapshot()
	if after.Restarts != 0 || after.Reboots != 0 || after.AlertOnly || after.Stage != watchdog.StepNone {
		t.Errorf("after the reset: %+v, want counters 0, no alert-only latch, stage none", after)
	}
	if !after.LastRestart.Equal(before.LastRestart) || !after.LastReboot.Equal(before.LastReboot) {
		t.Errorf("cooldown clocks changed by a reset: %+v -> %+v", before, after)
	}
	if watchdog.ResetRequested(e.dir) {
		t.Error("the reset request was not consumed")
	}

	// The state on disk (what a restarted watchdog would read) is reset too.
	g2, err := watchdog.LoadGuards(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st := g2.Snapshot(); st.Restarts != 0 || st.Reboots != 0 || st.AlertOnly {
		t.Errorf("persisted state %+v, want reset", st)
	}

	// The watchdog reported it; it arrives at the server through the forwarder.
	eventually(t, "counters-reset event at the server", func() bool {
		for _, ev := range e.m.WatchdogEvents("w1") {
			if ev.Kind == watchdog.EventCounters {
				return true
			}
		}
		return false
	})

	// A second step does not reset again (the request was taken once).
	if err := g.Record(watchdog.StepRestart, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.wd.Step(context.Background())
	if st := g.Snapshot(); st.Restarts != 1 {
		t.Errorf("restarts %d after a later step, want 1 (no second reset)", st.Restarts)
	}
}

// A reset for a machine the manager does not know is refused, not queued: the
// operator learns it did not happen.
func TestCounterResetRefusedForUnknown(t *testing.T) {
	e := newWdEnv(t)
	e.startServer(t)
	if err := e.m.ResetCounters(context.Background(), "nope"); !errors.Is(err, fleet.ErrUnknown) {
		t.Errorf("ResetCounters for an unknown machine = %v, want ErrUnknown", err)
	}
}

// The agent takes the wire message from the channel itself: a watchdog-reset
// envelope (no payload) is enough, and nothing is sent back.
func TestAgentHandlesWatchdogResetEnvelope(t *testing.T) {
	e := newWdEnv(t)
	if err := e.wd.Guards().Record(watchdog.StepRestart, time.Now()); err != nil {
		t.Fatal(err)
	}
	rs := dialAgent(t, e.mt, "w1")
	rs.send(channel.TypeWatchdogReset, "", nil)
	eventually(t, "reset request", func() bool { return watchdog.ResetRequested(e.dir) })
	e.wd.Step(context.Background())
	if st := e.wd.Guards().Snapshot(); st.Restarts != 0 {
		t.Errorf("restarts %d, want 0", st.Restarts)
	}
}
