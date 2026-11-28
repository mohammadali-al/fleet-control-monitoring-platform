package agent_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestSelfUpdateNoOpAlreadyOnVersion(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(binPath, []byte("v2-bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	var restarted atomic.Int32
	wrk, err := mt.StartAgent("w1", agent.Config{
		DataDir: t.TempDir(),
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		AgentVersion: "2.0.0",
		Update: agent.UpdateOptions{
			BinaryPath: binPath,
			Restart: func(context.Context) error {
				restarted.Add(1)
				return nil
			},
		},
		Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wrk.Stop() })

	reg := registry.New()
	_ = reg.Add(registry.Machine{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true})
	gate, _ := exec.NewGate([]byte("sec"), nil)
	mgr, err := fleet.New(fleet.Config{
		Registry:      reg,
		Gate:          gate,
		NewLink:       mt.Factory(),
		PollInterval:  10 * time.Millisecond,
		ServerVersion: "2.0.0",
		Logger:        quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Close() })

	eventually(t, "w1 online on 2.0.0", func() bool {
		info, ok := mgr.Machine("w1")
		return ok && info.State == fleet.StateOnline && info.AgentVersion == "2.0.0"
	})

	out, err := mgr.UpdateAgent(ctx, "w1")
	if err != nil || out.Status != "noop" {
		t.Fatalf("UpdateAgent = (%+v, %v), want noop", out, err)
	}
	if restarted.Load() != 0 {
		t.Errorf("restarted = %d, want 0", restarted.Load())
	}
}

func TestSelfUpdateChecksumMismatchDoesNotSwap(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	oldBytes := []byte("original-v1-binary")
	binPath := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(binPath, oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	// Serve a binary whose contents do NOT match the expected checksum in the envelope.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("corrupted-binary-payload"))
	}))
	t.Cleanup(srv.Close)

	var restarted atomic.Int32
	wrk, err := mt.StartAgent("w1", agent.Config{
		DataDir: t.TempDir(),
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		AgentVersion: "1.0.0",
		Update: agent.UpdateOptions{
			BinaryPath: binPath,
			HTTPClient: srv.Client(),
			Restart: func(context.Context) error {
				restarted.Add(1)
				return nil
			},
		},
		Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wrk.Stop() })

	// Drive update envelope directly via a real mTLS manager using a resolver whose file has different bytes than HTTP.
	binDir := t.TempDir()
	wantBytes := []byte("valid-v2-binary")
	if err := os.WriteFile(filepath.Join(binDir, config.AgentBinaryName(runtime.GOOS, runtime.GOARCH)), wantBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	_ = reg.Add(registry.Machine{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true})
	gate, _ := exec.NewGate([]byte("sec"), nil)
	mgr, err := fleet.New(fleet.Config{
		Registry:       reg,
		Gate:           gate,
		NewLink:        mt.Factory(),
		PollInterval:   10 * time.Millisecond,
		AgentBinaryDir: binDir,
		UpdateBaseURL:  srv.URL,
		ServerVersion:  "2.0.0",
		Logger:         quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Close() })

	eventually(t, "w1 online", func() bool {
		info, ok := mgr.Machine("w1")
		return ok && info.State == fleet.StateOnline && info.AgentVersion == "1.0.0"
	})

	if _, err := mgr.UpdateAgent(ctx, "w1"); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	eventually(t, "update failed on checksum mismatch", func() bool {
		info, _ := mgr.Machine("w1")
		return info.Update != nil && info.Update.Status == channel.AgentUpdateFailed
	})
	if restarted.Load() != 0 {
		t.Errorf("restarted = %d, want 0", restarted.Load())
	}
	got, _ := os.ReadFile(binPath)
	if string(got) != string(oldBytes) {
		t.Errorf("binary on disk modified despite checksum mismatch: %q", got)
	}
}

func TestSelfUpdateRestartFailureRollsBack(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	oldBytes := []byte("original-v1-binary")
	newBytes := []byte("new-v2-binary-valid")
	binPath := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(binPath, oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, config.AgentBinaryName(runtime.GOOS, runtime.GOARCH)), newBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	wrk, err := mt.StartAgent("w1", agent.Config{
		DataDir: t.TempDir(),
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		AgentVersion: "1.0.0",
		Update: agent.UpdateOptions{
			BinaryPath: binPath,
			Restart: func(context.Context) error {
				return errors.New("systemctl unavailable")
			},
		},
		Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wrk.Stop() })

	reg := registry.New()
	_ = reg.Add(registry.Machine{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true})
	gate, _ := exec.NewGate([]byte("sec"), nil)
	mgr, err := fleet.New(fleet.Config{
		Registry:       reg,
		Gate:           gate,
		NewLink:        mt.Factory(),
		PollInterval:   10 * time.Millisecond,
		AgentBinaryDir: binDir,
		ServerVersion:  "2.0.0",
		Logger:         quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Close() })

	eventually(t, "w1 online", func() bool {
		info, ok := mgr.Machine("w1")
		return ok && info.State == fleet.StateOnline && info.AgentVersion == "1.0.0"
	})

	if _, err := mgr.UpdateAgent(ctx, "w1"); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	eventually(t, "update failed and rolled back", func() bool {
		info, _ := mgr.Machine("w1")
		return info.Update != nil && info.Update.Status == channel.AgentUpdateFailed
	})
	got, _ := os.ReadFile(binPath)
	if string(got) != string(oldBytes) {
		t.Errorf("binary after rollback = %q, want %q", got, oldBytes)
	}
}

func TestSelfUpdateApplyVerifyRestartAndReconnectSuccess(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	oldBytes := []byte("original-v1-binary")
	newBytes := []byte("new-v2-binary-verified")
	binPath := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(binPath, oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, config.AgentBinaryName(runtime.GOOS, runtime.GOARCH)), newBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	al, err := mt.ListenAgents("")
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	_ = reg.Add(registry.Machine{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeAgentDial, Provisioned: true})
	gate, _ := exec.NewGate([]byte("sec"), nil)
	mgr, err := fleet.New(fleet.Config{
		Registry:       reg,
		Gate:           gate,
		AgentDial:      al,
		PollInterval:   10 * time.Millisecond,
		AgentBinaryDir: binDir,
		ServerVersion:  "2.0.0",
		Logger:         quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Close() })

	restartedCh := make(chan struct{}, 1)
	wrk1, err := mt.StartDialingAgent("w1", al.Addr().String(), agent.Config{
		DataDir: dataDir,
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		AgentVersion: "1.0.0",
		Update: agent.UpdateOptions{
			BinaryPath: binPath,
			Restart: func(context.Context) error {
				select {
				case restartedCh <- struct{}{}:
				default:
				}
				return nil
			},
			ReconnectTimeout: 2 * time.Second,
			GraceWindow:      5 * time.Second,
		},
		Logger: quiet(),
	}, agent.DialSettings{})
	if err != nil {
		t.Fatal(err)
	}

	eventually(t, "w1 online on 1.0.0", func() bool {
		info, ok := mgr.Machine("w1")
		return ok && info.State == fleet.StateOnline && info.AgentVersion == "1.0.0"
	})

	if _, err := mgr.UpdateAgent(ctx, "w1"); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	select {
	case <-restartedCh:
	case <-time.After(wait):
		t.Fatal("timed out waiting for service restart")
	}

	// Verify atomic swap + backup happened before restart.
	if got, _ := os.ReadFile(binPath); string(got) != string(newBytes) {
		t.Fatalf("replaced binary = %q, want %q", got, newBytes)
	}
	if bak, _ := os.ReadFile(binPath + ".bak"); string(bak) != string(oldBytes) {
		t.Fatalf("backup binary = %q, want %q", bak, oldBytes)
	}

	// Simulate systemd stopping v1 and starting v2 on the same DataDir.
	_ = wrk1.Stop()
	wrk2, err := mt.StartDialingAgent("w1", al.Addr().String(), agent.Config{
		DataDir: dataDir,
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		AgentVersion: "2.0.0",
		Update: agent.UpdateOptions{
			BinaryPath:       binPath,
			ReconnectTimeout: 2 * time.Second,
		},
		Logger: quiet(),
	}, agent.DialSettings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wrk2.Stop() })

	eventually(t, "w1 reconnected on 2.0.0 and update ok", func() bool {
		info, ok := mgr.Machine("w1")
		return ok && info.State == fleet.StateOnline && info.AgentVersion == "2.0.0" &&
			info.Update != nil && info.Update.Status == channel.AgentUpdateOK
	})
}

func TestSelfUpdateReconnectTimeoutRollsBack(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	oldBytes := []byte("original-v1-binary")
	newBytes := []byte("broken-v2-binary")
	binPath := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(binPath, oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, config.AgentBinaryName(runtime.GOOS, runtime.GOARCH)), newBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	al, err := mt.ListenAgents("")
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	_ = reg.Add(registry.Machine{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeAgentDial, Provisioned: true})
	gate, _ := exec.NewGate([]byte("sec"), nil)
	mgr, err := fleet.New(fleet.Config{
		Registry:       reg,
		Gate:           gate,
		AgentDial:      al,
		PollInterval:   10 * time.Millisecond,
		AgentBinaryDir: binDir,
		ServerVersion:  "2.0.0",
		Logger:         quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Close() })

	restartedCh := make(chan struct{}, 1)
	wrk1, err := mt.StartDialingAgent("w1", al.Addr().String(), agent.Config{
		DataDir: dataDir,
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		AgentVersion: "1.0.0",
		Update: agent.UpdateOptions{
			BinaryPath: binPath,
			Restart: func(context.Context) error {
				select {
				case restartedCh <- struct{}{}:
				default:
				}
				return nil
			},
			ReconnectTimeout: 150 * time.Millisecond,
			GraceWindow:      300 * time.Millisecond,
		},
		Logger: quiet(),
	}, agent.DialSettings{})
	if err != nil {
		t.Fatal(err)
	}

	eventually(t, "w1 online on 1.0.0", func() bool {
		info, ok := mgr.Machine("w1")
		return ok && info.State == fleet.StateOnline && info.AgentVersion == "1.0.0"
	})

	if _, err := mgr.UpdateAgent(ctx, "w1"); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	select {
	case <-restartedCh:
	case <-time.After(wait):
		t.Fatal("timed out waiting for service restart")
	}
	_ = wrk1.Stop()

	// Start v2 with a different CA so mTLS dial fails and v2 hits ReconnectTimeout.
	otherCA, err := pki.NewCA(pki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	badTLS, err := mt.DialTLS(otherCA, "w1", mt.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	rolledBackRestart := make(chan struct{}, 1)
	wrk2, err := mt.StartDialingAgent("w1", al.Addr().String(), agent.Config{
		DataDir: dataDir,
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		AgentVersion: "2.0.0",
		Update: agent.UpdateOptions{
			BinaryPath: binPath,
			Restart: func(context.Context) error {
				select {
				case rolledBackRestart <- struct{}{}:
				default:
				}
				return nil
			},
			ReconnectTimeout: 150 * time.Millisecond,
		},
		Logger: quiet(),
	}, agent.DialSettings{TLS: badTLS})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-rolledBackRestart:
	case <-time.After(wait):
		t.Fatal("timed out waiting for v2 reconnect-timeout rollback restart")
	}
	_ = wrk2.Stop()

	// Binary on disk must be rolled back to original v1 bytes.
	if got, _ := os.ReadFile(binPath); string(got) != string(oldBytes) {
		t.Fatalf("binary after rollback = %q, want %q", got, oldBytes)
	}

	// Bring v1 back up: it reports the deferred rollback failure to the server.
	wrk1Back, err := mt.StartDialingAgent("w1", al.Addr().String(), agent.Config{
		DataDir: dataDir,
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		AgentVersion: "1.0.0",
		Update: agent.UpdateOptions{
			BinaryPath: binPath,
		},
		Logger: quiet(),
	}, agent.DialSettings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wrk1Back.Stop() })

	eventually(t, "w1 reconnected on 1.0.0 with failed update report", func() bool {
		info, ok := mgr.Machine("w1")
		return ok && info.State == fleet.StateOnline && info.AgentVersion == "1.0.0" &&
			info.Update != nil && info.Update.Status == channel.AgentUpdateFailed
	})
}

type fakeWdSampler struct {
	cpu float64
}

func (s *fakeWdSampler) Sample(_ context.Context, units []string) (watchdog.Sample, error) {
	svcs := make(map[string]watchdog.UsageMetrics, len(units))
	for _, u := range units {
		svcs[u] = watchdog.UsageMetrics{Present: true, CPUPercent: s.cpu}
	}
	return watchdog.Sample{
		Time:     time.Now(),
		Machine:  watchdog.MachineMetrics{CPUPercent: s.cpu},
		Services: svcs,
	}, nil
}

type fakeWdActuator struct {
	mu       sync.Mutex
	restarts []string
}

func (a *fakeWdActuator) Throttle(context.Context, watchdog.Target, watchdog.ThrottleSpec) error {
	return nil
}
func (a *fakeWdActuator) Unthrottle(context.Context, watchdog.Target) error { return nil }
func (a *fakeWdActuator) KillSession(context.Context, watchdog.Target) error {
	return nil
}
func (a *fakeWdActuator) RestartService(_ context.Context, unit string) error {
	a.mu.Lock()
	a.restarts = append(a.restarts, unit)
	a.mu.Unlock()
	return nil
}
func (a *fakeWdActuator) Reboot(context.Context) error { return nil }
func (a *fakeWdActuator) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.restarts...)
}

func TestSelfUpdateWatchdogCoordinationAndExpiredRollback(t *testing.T) {
	dataDir := t.TempDir()
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "fleet-agent")
	bakPath := binPath + ".bak"
	oldBytes := []byte("v1-good")
	newBytes := []byte("v2-dead-on-boot")
	if err := os.WriteFile(binPath, newBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bakPath, oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed a policy where any 90% CPU breach goes straight to restart, with 0 boot window.
	polYAML := []byte("version: 1\nboot_window: 0s\nsample_interval: 10s\nsustain: 10s\nsteps:\n  throttle: false\n  kill_session: false\n  restart: true\n  reboot: false\nmachine:\n  cpu_percent:\n    trigger: 95\n    recover: 91\n")
	if _, err := watchdog.StorePolicy(dataDir, polYAML, -1); err != nil {
		t.Fatal(err)
	}

	pendingPath := filepath.Join(dataDir, watchdog.WatchdogSubdir, "update-pending.json")
	now := time.Now().UTC()
	pending := map[string]any{
		"request_id":         "req-wd-1",
		"target_version":     "2.0.0",
		"previous_version":   "1.0.0",
		"bin_path":           binPath,
		"backup_path":        bakPath,
		"started_at":         now,
		"grace_until":        now.Add(10 * time.Second),
		"reconnect_deadline": now.Add(5 * time.Second),
	}
	pb, _ := json.Marshal(pending)
	if err := os.WriteFile(pendingPath, pb, 0o640); err != nil {
		t.Fatal(err)
	}

	sampler := &fakeWdSampler{cpu: 99}
	act := &fakeWdActuator{}
	wd, err := agent.NewWatchdog(config.AgentConfig{MachineID: "w1"}, dataDir, quiet(), agent.WatchdogRunOptions{
		Sampler:  sampler,
		Actuator: act,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. During active grace window, high CPU does NOT escalate or increment restarts.
	ctx := context.Background()
	wd.Step(ctx)
	wd.Step(ctx)
	if st := wd.Guards().Snapshot(); st.Restarts != 0 || st.Stage != watchdog.StepNone || st.AlertOnly {
		t.Fatalf("watchdog escalated during update grace window: %+v", st)
	}
	if rs := act.snapshot(); len(rs) != 0 {
		t.Fatalf("watchdog called RestartService during grace window: %v", rs)
	}

	// 2. When reconnect_deadline expires without commit, watchdog rolls back and restarts fleet-agent.service.
	pending["reconnect_deadline"] = now.Add(-time.Second)
	pb, _ = json.Marshal(pending)
	if err := os.WriteFile(pendingPath, pb, 0o640); err != nil {
		t.Fatal(err)
	}
	wd.Step(ctx)

	if got, _ := os.ReadFile(binPath); string(got) != string(oldBytes) {
		t.Fatalf("binary after watchdog rollback = %q, want %q", got, oldBytes)
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Errorf("pending marker not removed after watchdog rollback: %v", err)
	}
	if rs := act.snapshot(); len(rs) != 1 || rs[0] != "fleet-agent.service" {
		t.Errorf("watchdog rollback restarts = %v, want [fleet-agent.service]", rs)
	}
	if st := wd.Guards().Snapshot(); st.Restarts != 0 {
		t.Errorf("watchdog rollback consumed daily restart cap: restarts=%d", st.Restarts)
	}
	_ = sha256Hex
}
