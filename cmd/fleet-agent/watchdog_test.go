package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `fleet-agent watchdog --config ...` starts the loop on the config's data dir,
// runs under the policy stored there, and stops cleanly on cancellation.
func TestWatchdogSubcommandStartsAndShutsDownCleanly(t *testing.T) {
	cfg := writeAgentConfig(t, "worker")
	data := filepath.Join(filepath.Dir(cfg), "data", "watchdog")
	if err := os.MkdirAll(data, 0o750); err != nil {
		t.Fatal(err)
	}
	// A policy the main agent stored earlier: the watchdog runs under it at start.
	if err := os.WriteFile(filepath.Join(data, "policy.yaml"), []byte("version: 7\nboot_window: 0s\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"watchdog", "--config", cfg}, &stdout, &stderr) }()

	deadline := time.After(10 * time.Second)
	for !strings.Contains(stderr.String(), "watchdog ready") {
		select {
		case code := <-done:
			t.Fatalf("exited early with %d: %s", code, stderr.String())
		case <-deadline:
			t.Fatalf("watchdog did not start: %s", stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !strings.Contains(stderr.String(), "policy_version=7") {
		t.Errorf("watchdog did not load the stored policy: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "listen") {
		t.Errorf("the watchdog process opened a channel listener: %s", stderr.String())
	}
	cancel() // what SIGTERM does in production
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code %d, want 0; log: %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watchdog did not shut down")
	}
	if !strings.Contains(stderr.String(), "shut down") {
		t.Errorf("no clean-shutdown log line: %s", stderr.String())
	}
}

func TestWatchdogSubcommandRejectsBadFlagAndBadConfig(t *testing.T) {
	var stdout, stderr lockedBuf
	if code := run(context.Background(), []string{"watchdog", "--nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("bad flag: exit %d, want 2", code)
	}
	bad := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr = lockedBuf{}
	if code := run(context.Background(), []string{"watchdog", "--config", bad}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "watchdog") {
		t.Errorf("bad config: exit %d, stderr %q", code, stderr.String())
	}
}
