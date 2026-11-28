package panel

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/version"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

// The Machines SPA route serves the React shell and the compiled bundle
// carrying the Machines registry, live /ws/events wiring, token-enrollment UI,
// SSH onboarding, and public-IP refresh actions.
func TestMachinesPageHasTokenEnrollmentUI(t *testing.T) {
	srv := noBackend(t)
	status, _, page := body(t, srv.URL+"/machines")
	if status != 200 {
		t.Fatalf("GET /machines = %d", status)
	}
	for _, want := range []string{`<div id="root"></div>`, `/assets/dist/app.css`, `/assets/dist/app.js`} {
		if !strings.Contains(page, want) {
			t.Errorf("machines SPA shell lacks %q", want)
		}
	}

	status, _, js := body(t, srv.URL+"/assets/dist/app.js")
	if status != 200 || len(js) == 0 {
		t.Fatalf("GET /assets/dist/app.js = %d (len=%d)", status, len(js))
	}
	for _, want := range []string{
		"Add agent (token)",
		"Onboard machine",
		"Refresh public IPs",
		"/refresh-public-ip",
		"/api/machines",
		"/api/enroll/tokens",
		"/api/enroll/machines",
		"/ws/events",
		"macOS machines can only be monitor agents",
		"All reported disks OK",
		"Disk figures update live from agent telemetry.",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("SPA bundle lacks %q", want)
		}
	}
}

func TestMachinesPageHasDetailsDrawerInBundle(t *testing.T) {
	srv := noBackend(t)
	status, _, js := body(t, srv.URL+"/assets/dist/app.js")
	if status != 200 || len(js) == 0 {
		t.Fatalf("GET /assets/dist/app.js = %d (len=%d)", status, len(js))
	}
	if strings.Contains(js, "The machine details view comes later.") {
		t.Error("SPA bundle still contains the stale Details stub toast")
	}
	for _, want := range []string{
		"Machine details",
		"Identity & connection",
		"Watchdog stage",
		"Connect mode",
		"Last seen",
		"Private IPs",
		"Public IP",
		"Public IP checked",
		"Enrollment & certificates",
		"Cert fingerprint",
		"/api/machines/",
		"data-machine-details",
		"Close machine details",
		"Live updates via /ws/events",
		"Agent version",
		"Server version",
		"Outdated",
		"Update outdated",
		"Update agent",
		"/update-outdated",
		"/update",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("SPA bundle lacks details drawer string %q", want)
		}
	}
}

func TestGetMachineDetailsEndpoint(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := e.reg.Update("w1", func(m *registry.Machine) {
		m.ConnectMode = registry.ConnectModeAgentDial
		m.PrimaryPrivateIP = "10.0.0.5"
		m.PublicIP = "203.0.113.10"
		m.PublicIPAt = &now
		m.CertFingerprint = "abcdef0123456789"
		m.SSHHostKey = "SHA256:testhostkey"
		m.EnrolledVia = registry.EnrolledToken
		m.AgentVersion = "0.9.0"
		m.Disk = registry.DiskState{
			Status:     "warn",
			TotalBytes: 100 * 1000 * 1000 * 1000,
			FreeBytes:  15 * 1000 * 1000 * 1000,
			UpdatedAt:  &now,
		}
		m.Specs = &registry.Specs{
			Hostname:  "worker-01",
			OS:        "linux",
			OSVersion: "Ubuntu 24.04",
			Arch:      "amd64",
			CPUs:      8,
			MemBytes:  16 * 1000 * 1000 * 1000,
		}
	}); err != nil {
		t.Fatalf("reg.Update: %v", err)
	}

	st, _, rawJSON := body(t, e.srv.URL+"/api/machines/w1")
	if st != http.StatusOK {
		t.Fatalf("GET /api/machines/w1 = %d, want 200 (%s)", st, rawJSON)
	}
	var got machineResponse
	if err := json.Unmarshal([]byte(rawJSON), &got); err != nil {
		t.Fatalf("decode machineResponse: %v (%s)", err, rawJSON)
	}

	if got.Machine.ID != "w1" || got.Machine.Role != registry.RoleWorker || got.Machine.State != "online" {
		t.Errorf("machine identity/state = %+v", got.Machine)
	}
	if got.Machine.ConnectMode != registry.ConnectModeAgentDial {
		t.Errorf("connect_mode = %q, want %q", got.Machine.ConnectMode, registry.ConnectModeAgentDial)
	}
	if got.Machine.PrimaryPrivateIP != "10.0.0.5" {
		t.Errorf("primary private IP = %q", got.Machine.PrimaryPrivateIP)
	}
	if got.Machine.PublicIP != "203.0.113.10" || got.Machine.PublicIPAt == nil {
		t.Errorf("public IP = %q at %v", got.Machine.PublicIP, got.Machine.PublicIPAt)
	}
	if got.Machine.Disk.Status != "warn" || got.Machine.Disk.TotalBytes == 0 || got.Machine.Disk.UpdatedAt == nil {
		t.Errorf("disk = %+v", got.Machine.Disk)
	}
	if got.Machine.CertFingerprint != "abcdef0123456789" || got.Machine.SSHHostKey != "SHA256:testhostkey" {
		t.Errorf("fingerprints = cert:%q ssh:%q", got.Machine.CertFingerprint, got.Machine.SSHHostKey)
	}
	if got.Machine.EnrolledVia != registry.EnrolledToken || !got.Machine.Provisioned {
		t.Errorf("enrollment = via:%q provisioned:%v", got.Machine.EnrolledVia, got.Machine.Provisioned)
	}
	if got.Machine.AgentVersion != "0.9.0" || got.ServerVersion != version.Version {
		t.Errorf("versions = agent:%q server:%q, want 0.9.0 / %q", got.Machine.AgentVersion, got.ServerVersion, version.Version)
	}
	if got.Machine.Specs == nil || got.Machine.Specs.Hostname != "worker-01" {
		t.Errorf("specs = %+v", got.Machine.Specs)
	}

	// No secrets / private keys in payload.
	lower := strings.ToLower(rawJSON)
	for _, secret := range []string{"private_key", "private key", "-----begin", "password"} {
		if strings.Contains(lower, secret) {
			t.Errorf("GET /api/machines/w1 leaked %q in JSON: %s", secret, rawJSON)
		}
	}

	// Unknown machine -> 404.
	if st := e.call(t, "GET", "/api/machines/does-not-exist", nil, nil); st != http.StatusNotFound {
		t.Errorf("GET /api/machines/does-not-exist = %d, want 404", st)
	}

	// Without backend -> 503.
	nb := noBackend(t)
	if st, _, _ := body(t, nb.URL+"/api/machines/w1"); st != http.StatusServiceUnavailable {
		t.Errorf("no-backend GET /api/machines/w1 = %d, want 503", st)
	}
}

func TestGetMachineDetailsWatchdog(t *testing.T) {
	e := newObsEnv(t, nil)
	var worker machineResponse
	if st, _, raw := body(t, e.srv.URL+"/api/machines/w1"); st != http.StatusOK {
		t.Fatalf("GET /api/machines/w1 = %d (%s)", st, raw)
	} else if err := json.Unmarshal([]byte(raw), &worker); err != nil {
		t.Fatalf("decode worker: %v", err)
	}
	if worker.Watchdog == nil {
		t.Fatal("expected watchdog state for worker w1, got nil")
	}
	if worker.Watchdog.Stage != "none" || worker.Watchdog.Condition != "normal" {
		t.Errorf("worker watchdog = %+v, want stage=none condition=normal", worker.Watchdog)
	}

	var mon machineResponse
	if st, _, raw := body(t, e.srv.URL+"/api/machines/mon1"); st != http.StatusOK {
		t.Fatalf("GET /api/machines/mon1 = %d (%s)", st, raw)
	} else if err := json.Unmarshal([]byte(raw), &mon); err != nil {
		t.Fatalf("decode monitor: %v", err)
	}
	if mon.Watchdog != nil {
		t.Errorf("expected nil watchdog state for monitor mon1, got %+v", mon.Watchdog)
	}
}

func TestMachineUpdateEndpointsAndLiveFlow(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	al, err := mt.ListenAgents("")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	newBytes := []byte("panel-test-new-agent-binary-v2.0.0")
	if err := os.WriteFile(filepath.Join(binDir, config.AgentBinaryName(runtime.GOOS, runtime.GOARCH)), newBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := registry.New()
	for _, mc := range []registry.Machine{
		{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeAgentDial, Provisioned: true},
		{ID: "w2", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeAgentDial, Provisioned: true, AgentVersion: "1.0.0"},
		{ID: "w3", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeAgentDial, Provisioned: true},
	} {
		if err := reg.Add(mc); err != nil {
			t.Fatal(err)
		}
	}
	gate, _ := exec.NewGate([]byte("sec"), nil)
	mgr, err := fleet.New(fleet.Config{
		Registry:       reg,
		Gate:           gate,
		AgentDial:      al,
		PollInterval:   10 * time.Millisecond,
		AgentBinaryDir: binDir,
		ServerVersion:  "2.0.0",
		Logger:         quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Close() })

	dataDir1 := t.TempDir()
	binPath1 := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(binPath1, []byte("old-v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	restarted := make(chan struct{}, 1)
	w1, err := mt.StartDialingAgent("w1", al.Addr().String(), agent.Config{
		DataDir: dataDir1,
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		AgentVersion: "1.0.0",
		Update: agent.UpdateOptions{
			BinaryPath: binPath1,
			Restart: func(context.Context) error {
				select {
				case restarted <- struct{}{}:
				default:
				}
				return nil
			},
			ReconnectTimeout: 2 * time.Second,
		},
		Logger: quiet,
	}, agent.DialSettings{})
	if err != nil {
		t.Fatal(err)
	}

	w3, err := mt.StartDialingAgent("w3", al.Addr().String(), agent.Config{
		DataDir: t.TempDir(),
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		AgentVersion: "2.0.0",
		Logger:       quiet,
	}, agent.DialSettings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w3.Stop() })

	h, err := New(Options{Assets: web.FS, Templates: web.Templates, Backend: mgr, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		m1, ok1 := mgr.Machine("w1")
		m3, ok3 := mgr.Machine("w3")
		if ok1 && m1.State == fleet.StateOnline && m1.AgentVersion == "1.0.0" &&
			ok3 && m3.State == fleet.StateOnline && m3.AgentVersion == "2.0.0" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// GET /api/machines surfaces server_version and agent_version.
	st, _, raw := body(t, srv.URL+"/api/machines")
	if st != http.StatusOK {
		t.Fatalf("GET /api/machines = %d (%s)", st, raw)
	}
	var list machinesResponse
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	if list.ServerVersion != "2.0.0" {
		t.Fatalf("ServerVersion = %q, want 2.0.0", list.ServerVersion)
	}

	// Subscribe to /ws/events to observe live update progress and reconnect.
	wsCtx, wsCancel := context.WithTimeout(context.Background(), wait)
	defer wsCancel()
	wsConn, _, err := websocket.Dial(wsCtx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/events", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer wsConn.CloseNow()
	var hello map[string]string
	_ = readJSON(wsCtx, wsConn, &hello)

	post := func(path, ct, payload string) (int, string) {
		t.Helper()
		req, err := http.NewRequest("POST", srv.URL+path, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Trigger update on outdated online w1.
	if code, b := post("/api/machines/w1/update", "application/json", `{}`); code != http.StatusOK || !strings.Contains(b, `"dispatched"`) {
		t.Fatalf("POST /api/machines/w1/update = (%d, %s), want 200 dispatched", code, b)
	}
	select {
	case <-restarted:
	case <-time.After(wait):
		t.Fatal("timed out waiting for w1 restart")
	}
	_ = w1.Stop()

	w1New, err := mt.StartDialingAgent("w1", al.Addr().String(), agent.Config{
		DataDir: dataDir1,
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		AgentVersion: "2.0.0",
		Update:       agent.UpdateOptions{BinaryPath: binPath1, ReconnectTimeout: 2 * time.Second},
		Logger:       quiet,
	}, agent.DialSettings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w1New.Stop() })

	sawUpdated := false
	for !sawUpdated {
		var msg eventMsg
		if err := readJSON(wsCtx, wsConn, &msg); err != nil {
			t.Fatalf("read ws event: %v", err)
		}
		if msg.Type == "machine" && msg.Machine != nil && msg.Machine.ID == "w1" &&
			msg.Machine.AgentVersion == "2.0.0" && msg.Machine.Update != nil && msg.Machine.Update.Status == channel.AgentUpdateOK {
			sawUpdated = true
		}
	}

	// Already on current version (w3) -> noop.
	if code, b := post("/api/machines/w3/update", "application/json", `{}`); code != http.StatusOK || !strings.Contains(b, `"noop"`) {
		t.Errorf("POST current w3 update = (%d, %s), want 200 noop", code, b)
	}
	// Bulk update-outdated when all online are current -> dispatched: 0.
	if code, b := post("/api/machines/update-outdated", "application/json", `{}`); code != http.StatusOK || !strings.Contains(b, `"dispatched":0`) {
		t.Errorf("POST update-outdated = (%d, %s), want 200 dispatched:0", code, b)
	}
	// Offline, unknown, bad content-type, bad body.
	if code, b := post("/api/machines/w2/update", "application/json", `{}`); code != http.StatusConflict || !strings.Contains(strings.ToLower(b), "agent offline") {
		t.Errorf("POST offline w2 update = (%d, %s), want 409", code, b)
	}
	if code, _ := post("/api/machines/nope/update", "application/json", `{}`); code != http.StatusNotFound {
		t.Errorf("POST unknown update = %d, want 404", code)
	}
	if code, _ := post("/api/machines/w1/update", "text/plain", `{}`); code != http.StatusUnsupportedMediaType {
		t.Errorf("POST non-JSON update = %d, want 415", code)
	}
	if code, _ := post("/api/machines/w1/update", "application/json", `{"bad":1}`); code != http.StatusBadRequest {
		t.Errorf("POST bad JSON update = %d, want 400", code)
	}
	// Missing binary -> 503 when an outdated machine needs an update.
	_ = reg.Update("w3", func(m *registry.Machine) { m.AgentVersion = "1.0.0" })
	_ = os.Remove(filepath.Join(binDir, config.AgentBinaryName(runtime.GOOS, runtime.GOARCH)))
	if code, _ := post("/api/machines/w3/update", "application/json", `{}`); code != http.StatusServiceUnavailable {
		t.Errorf("POST update with missing binary = %d, want 503", code)
	}

	// Without backend -> 503.
	nb := noBackend(t)
	req, _ := http.NewRequest("POST", nb.URL+"/api/machines/w1/update", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no-backend update = %d, want 503", resp.StatusCode)
	}
}
