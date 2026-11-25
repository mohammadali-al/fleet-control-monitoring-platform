package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// writeAgentConfig writes what onboarding writes (a config file plus the pinned
// CA, certificate and key) into a temp dir and returns the config path.
func writeAgentConfig(t *testing.T, role string) string {
	t.Helper()
	dir := t.TempDir()
	ca, err := pki.NewCA(pki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.IssuePeer("w1", []net.IP{net.IPv4(127, 0, 0, 1)}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"ca.crt": ca.CertPEM(), "agent.crt": certPEM, "agent.key": keyPEM}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	doc := map[string]any{"agent": config.AgentConfig{
		Role: role, MachineID: "w1", ConnectMode: config.ConnectModeServerDial,
		CAFile: filepath.Join(dir, "ca.crt"), CertFile: filepath.Join(dir, "agent.crt"), KeyFile: filepath.Join(dir, "agent.key"),
		ListenAddr: "127.0.0.1:0", DataDir: filepath.Join(dir, "data"),
	}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startsAndShutsDownCleanly(t *testing.T, role string) {
	t.Helper()
	cfg := writeAgentConfig(t, role)
	var stdout, stderr lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"--config", cfg}, &stdout, &stderr) }()

	deadline := time.After(10 * time.Second)
	for !strings.Contains(stderr.String(), "starting") {
		select {
		case code := <-done:
			t.Fatalf("exited early with %d: %s", code, stderr.String())
		case <-deadline:
			t.Fatalf("%s did not start: %s", role, stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel() // what SIGTERM does in production
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code %d, want 0; log: %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not shut down", role)
	}
	if !strings.Contains(stderr.String(), "shut down") {
		t.Errorf("no clean-shutdown log line: %s", stderr.String())
	}
}

func TestWorkerStartsAndShutsDownCleanly(t *testing.T) { startsAndShutsDownCleanly(t, "worker") }

// The monitor role runs (no root, no data dir needed) and stops on cancel.
func TestMonitorStartsAndShutsDownCleanly(t *testing.T) { startsAndShutsDownCleanly(t, "monitor") }

func TestMonitorNeedsOnboarding(t *testing.T) {
	var stdout, stderr lockedBuf
	code := run(context.Background(), []string{"--role", "monitor"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "machine_id") {
		t.Errorf("unonboarded monitor: exit %d, stderr %q, want a machine_id error", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "not implemented") {
		t.Errorf("stderr still says the monitor role is not implemented: %q", stderr.String())
	}
}

func TestRoleIsRequiredAndWorkerNeedsOnboarding(t *testing.T) {
	var stdout, stderr lockedBuf
	if code := run(context.Background(), nil, &stdout, &stderr); code != 2 {
		t.Errorf("no role: exit %d, want 2", code)
	}
	stderr = lockedBuf{}
	if code := run(context.Background(), []string{"--role", "worker"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "machine_id") {
		t.Errorf("unonboarded worker: exit %d, stderr %q, want a machine_id error", code, stderr.String())
	}
}

func TestVersionFlag(t *testing.T) {
	var stdout, stderr lockedBuf
	if code := run(context.Background(), []string{"--version"}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) == "" {
		t.Errorf("--version: exit %d, stdout %q", code, stdout.String())
	}
}
