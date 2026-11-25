package agent

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// The agent writes its heartbeat from its loop, at the pushed write interval,
// carrying the pushed threshold.
func TestAgentHeartbeatFollowsPolicy(t *testing.T) {
	dir := t.TempDir()
	if _, err := watchdog.StorePolicy(dir, []byte("version: 1\nheartbeat_write: 2s\nheartbeat_threshold: 60s\n"), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunHeartbeat(ctx, HeartbeatOptions{DataDir: dir, MachineID: "wrk-1", Log: quiet()})
		close(done)
	}()
	path := watchdog.HeartbeatPath(dir, watchdog.HeartbeatAgent)
	var first time.Time
	waitFor(t, "the first heartbeat", 2*time.Second, func() bool {
		at, th, err := watchdog.ReadHeartbeat(path)
		if err != nil {
			return false
		}
		if th != 60*time.Second {
			t.Fatalf("threshold in the file = %s, want the pushed 60s", th)
		}
		first = at
		return true
	})
	// A second beat after the 2 s write interval (the file has second resolution).
	waitFor(t, "the next heartbeat", 5*time.Second, func() bool {
		at, _, err := watchdog.ReadHeartbeat(path)
		return err == nil && at.After(first)
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunHeartbeat did not stop on cancel")
	}
}

// A hung agent stops beating: a probe that never returns withholds the
// heartbeat (so the file goes stale), without piling up goroutines, and the
// beat resumes when the agent recovers.
func TestAgentHeartbeatWithheldWhenProbeHangs(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	var calls atomic.Int32
	probe := func() bool {
		calls.Add(1)
		<-release // deadlocked
		return true
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunHeartbeat(ctx, HeartbeatOptions{DataDir: dir, Probe: probe, ProbeTimeout: 50 * time.Millisecond, Log: quiet()})

	path := watchdog.HeartbeatPath(dir, watchdog.HeartbeatAgent)
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a heartbeat was written while the agent was hung")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d probes in flight/started while the first was stuck, want 1", n)
	}
	close(release) // the agent recovers
	waitFor(t, "the heartbeat to resume", 3*time.Second, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

// A healthy probe beats at once (the first beat does not wait a full interval).
func TestAgentHeartbeatImmediate(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunHeartbeat(ctx, HeartbeatOptions{DataDir: dir, Probe: func() bool { return true }, Log: quiet()})
	waitFor(t, "an immediate heartbeat", time.Second, func() bool {
		_, _, err := watchdog.ReadHeartbeat(watchdog.HeartbeatPath(dir, watchdog.HeartbeatAgent))
		return err == nil
	})
}
