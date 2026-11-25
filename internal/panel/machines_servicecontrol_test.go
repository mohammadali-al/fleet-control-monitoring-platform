package panel

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

type panelFakeServiceController struct {
	mu      sync.Mutex
	actions []string
	state   string
	err     error
}

func (f *panelFakeServiceController) Stop(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, "stop")
	if f.err != nil {
		return f.err
	}
	f.state = "stopped"
	return nil
}

func (f *panelFakeServiceController) Start(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, "start")
	if f.err != nil {
		return f.err
	}
	f.state = "running"
	return nil
}

func (f *panelFakeServiceController) Status(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, "status")
	if f.err != nil {
		return "", f.err
	}
	if f.state == "" {
		return "running", nil
	}
	return f.state, nil
}

func (f *panelFakeServiceController) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

func TestPanelServiceControlMonitorAndWorkerFleettest(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}

	monCtrl := &panelFakeServiceController{state: "running"}
	mon, err := mt.StartMonitor("mon1", agent.MonitorConfig{
		ServiceController: monCtrl,
		Logger:            quiet,
	})
	if err != nil {
		t.Fatalf("StartMonitor: %v", err)
	}
	t.Cleanup(func() { _ = mon.Stop() })

	wrkCtrl := &panelFakeServiceController{state: "running"}
	wrk, err := mt.StartAgent("w1", agent.Config{
		DataDir: t.TempDir(),
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		Logger: quiet,
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	wrk.SetServiceController(wrkCtrl)
	t.Cleanup(func() { _ = wrk.Stop() })

	reg := registry.New()
	for _, mc := range []registry.Machine{
		{ID: "mon1", Role: registry.RoleMonitor, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true},
		{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true},
		{ID: "off1", Role: registry.RoleMonitor, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.2"}, Provisioned: true},
	} {
		if err := reg.Add(mc); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := exec.NewGate([]byte("test-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := fleet.New(fleet.Config{
		Registry:     reg,
		Gate:         gate,
		NewLink:      mt.Factory(),
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Close() })

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		m1, _ := mgr.Machine("mon1")
		w1, _ := mgr.Machine("w1")
		if m1.State == fleet.StateOnline && w1.State == fleet.StateOnline {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if m1, _ := mgr.Machine("mon1"); m1.State != fleet.StateOnline {
		t.Fatalf("mon1 state = %q, want online", m1.State)
	}

	s := &server{be: mgr, log: quiet}
	mux := http.NewServeMux()
	s.routesMachines(mux)
	srv := httptest.NewServer(secure(mux))
	t.Cleanup(srv.Close)

	call := func(method, path, ct, body string) (int, serviceControlResponse) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
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
		raw, _ := io.ReadAll(resp.Body)
		var out serviceControlResponse
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	// Monitor Stop: triggers disable path and returns 200 (no 501).
	if code, res := call("POST", "/api/machines/mon1/stop", "application/json", `{}`); code != http.StatusOK || !res.OK || res.State != "stopped" || res.Machine != "mon1" {
		t.Fatalf("POST monitor stop = (%d, %+v), want 200 ok stopped", code, res)
	}
	if code, res := call("GET", "/api/machines/mon1/service-status", "", ""); code != http.StatusOK || !res.OK || res.State != "stopped" {
		t.Fatalf("GET monitor service-status = (%d, %+v), want 200 stopped", code, res)
	}
	if code, res := call("POST", "/api/machines/mon1/start", "application/json", `{}`); code != http.StatusOK || !res.OK || res.State != "running" {
		t.Fatalf("POST monitor start = (%d, %+v), want 200 ok running", code, res)
	}
	if got := strings.Join(monCtrl.snapshot(), ","); got != "stop,status,start" {
		t.Errorf("monitor actions = %q, want stop,status,start", got)
	}

	// Worker Stop and Start: trigger the exact same service-control path.
	if code, res := call("POST", "/api/machines/w1/stop", "application/json", `{}`); code != http.StatusOK || !res.OK || res.State != "stopped" {
		t.Fatalf("POST worker stop = (%d, %+v), want 200 ok stopped", code, res)
	}
	if code, res := call("POST", "/api/machines/w1/start", "application/json", `{}`); code != http.StatusOK || !res.OK || res.State != "running" {
		t.Fatalf("POST worker start = (%d, %+v), want 200 ok running", code, res)
	}
	if got := strings.Join(wrkCtrl.snapshot(), ","); got != "stop,start" {
		t.Errorf("worker actions = %q, want stop,start", got)
	}

	// Error mapping checks (unknown -> 404, offline -> 409, non-JSON -> 415).
	if code, _ := call("POST", "/api/machines/nope/stop", "application/json", `{}`); code != http.StatusNotFound {
		t.Errorf("unknown machine stop = %d, want 404", code)
	}
	if code, _ := call("POST", "/api/machines/off1/stop", "application/json", `{}`); code != http.StatusConflict {
		t.Errorf("offline machine stop = %d, want 409", code)
	}
	if code, _ := call("POST", "/api/machines/mon1/stop", "text/plain", `{}`); code != http.StatusUnsupportedMediaType {
		t.Errorf("non-JSON stop = %d, want 415", code)
	}
}
