package watchdog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// hangSampler samples normally until block is called, then blocks in Sample until
// release is closed (a hung watchdog loop).
type hangSampler struct {
	mu      sync.Mutex
	blocked bool
	release chan struct{}
}

func (h *hangSampler) block() {
	h.mu.Lock()
	h.blocked = true
	h.mu.Unlock()
}

func (h *hangSampler) Sample(ctx context.Context, _ []string) (Sample, error) {
	h.mu.Lock()
	b := h.blocked
	h.mu.Unlock()
	if b {
		<-h.release
	}
	return Sample{Time: time.Now()}, nil
}

// The watchdog beats from its loop at the pushed write interval, and a loop that
// hangs (here: inside the sampler) stops beating, so the file goes stale.
func TestWatchdogLoopBeatsAndHangStopsIt(t *testing.T) {
	dir := t.TempDir()
	hs := &hangSampler{release: make(chan struct{})}
	t.Cleanup(func() { close(hs.release) })
	w, err := New(Config{DataDir: dir, MachineID: "w1", Sampler: hs, Actuator: &fakeAct{}, Log: quietLog(),
		BootTime: func() time.Time { return time.Now().Add(-time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.ApplyPolicy([]byte("version: 1\nsample_interval: 100ms\nsustain: 200ms\nheartbeat_write: 2s\nheartbeat_threshold: 60s\n")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // the stuck sampler ignores ctx; the release cleanup above frees it
	go func() { _ = w.RunWithHeartbeat(ctx) }()

	path := HeartbeatPath(dir, HeartbeatWatchdog)
	var first time.Time
	waitUntil(t, "the first heartbeat", 2*time.Second, func() bool {
		at, th, err := ReadHeartbeat(path)
		if err != nil {
			return false
		}
		if th != 60*time.Second {
			t.Fatalf("threshold in the file = %s, want the pushed 60s", th)
		}
		first = at
		return true
	})
	// Healthy: a second beat follows (file resolution is one second).
	waitUntil(t, "the next heartbeat", 5*time.Second, func() bool {
		at, _, err := ReadHeartbeat(path)
		return err == nil && at.After(first)
	})

	// Hang the loop, then watch the file stop moving for longer than a write interval.
	hs.block()
	time.Sleep(300 * time.Millisecond) // let the loop enter the stuck sample
	frozen, _, _ := ReadHeartbeat(path)
	time.Sleep(3 * time.Second)
	if at, _, _ := ReadHeartbeat(path); !at.Equal(frozen) {
		t.Fatalf("a hung loop kept beating: %v -> %v", frozen, at)
	}
}

func waitUntil(t *testing.T, what string, d time.Duration, cond func() bool) {
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

// RunWithHeartbeat still samples and stops on cancel, like Run.
func TestRunWithHeartbeatStopsOnCancel(t *testing.T) {
	dir := t.TempDir()
	w, err := New(Config{DataDir: dir, MachineID: "w1", Sampler: &fakeSampler{}, Actuator: &fakeAct{}, Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.RunWithHeartbeat(ctx) }()
	waitUntil(t, "the first heartbeat", 2*time.Second, func() bool {
		_, _, err := ReadHeartbeat(HeartbeatPath(dir, HeartbeatWatchdog))
		return err == nil
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop on cancel")
	}
}
