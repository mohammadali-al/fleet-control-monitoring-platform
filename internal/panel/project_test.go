package panel

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/project"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

func TestProjectAPIWithoutBackend(t *testing.T) {
	srv := noBackend(t)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/projects"},
		{http.MethodGet, "/api/projects/active"},
		{http.MethodPost, "/api/projects"},
		{http.MethodGet, "/api/projects/x"},
		{http.MethodPut, "/api/projects/x"},
		{http.MethodDelete, "/api/projects/x"},
		{http.MethodGet, "/api/projects/x/tests"},
		{http.MethodPost, "/api/projects/x/tests"},
		{http.MethodPost, "/api/projects/x/tests/t1/start"},
		{http.MethodPost, "/api/projects/x/tests/t1/stop"},
	} {
		status, raw := doJSON(t, tc.method, srv.URL+tc.path, map[string]any{}, nil)
		if status != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d (%s), want 503", tc.method, tc.path, status, raw)
		}
	}
}

func TestProjectAPICRUDPersistenceLockAndManeuverWiring(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ra, err := mt.StartAgent("w1", agent.Config{DataDir: t.TempDir(), TermGrace: 200 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ra.Stop() })

	dataDir := t.TempDir()
	reg := registry.New()
	for _, mc := range []registry.Machine{
		{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true},
		{ID: "mon1", Role: registry.RoleMonitor, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.2"}, Provisioned: true},
	} {
		if err := reg.Add(mc); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := exec.NewGate([]byte("panel-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := fleet.New(fleet.Config{
		Registry:     reg,
		Gate:         gate,
		NewLink:      mt.Factory(),
		PollInterval: 10 * time.Millisecond,
		GateSecret:   []byte("panel-secret"),
		DataDir:      dataDir,
		Logger:       quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.EnableObserve(dataDir); err != nil {
		t.Fatalf("EnableObserve: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Close() })

	h, err := New(Options{Assets: web.FS, Templates: web.Templates, Backend: mgr, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// Wait for w1 online so Maneuver sync-start handshake succeeds.
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if info, ok := mgr.Machine("w1"); ok && info.State == fleet.StateOnline {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Seed baseline item and Maneuver.
	status, raw := doJSON(t, http.MethodPost, srv.URL+"/api/baseline", map[string]any{
		"id":               "sleep-probe",
		"name":             "Sleep probe",
		"command_template": "echo target={ip}; sleep {duration}",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create baseline = %d (%s)", status, raw)
	}
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/maneuvers", map[string]any{
		"id":        "mv-proj",
		"name":      "Project probe",
		"target":    map[string]string{"ip": "198.51.100.10", "origin": "https://shop.example.test"},
		"total_sec": 15,
		"pattern":   map[string]any{"type": "all"},
		"timeline": []map[string]any{
			{"baseline_id": "sleep-probe", "start_sec": 0, "duration_sec": 10, "replicas": 1},
		},
		"agents": []string{"w1"},
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create maneuver = %d (%s)", status, raw)
	}

	// Create two projects.
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects", map[string]any{
		"name":        "Acme Engagement",
		"description": "External validation",
		"metadata":    map[string]string{"customer": "Acme Corp"},
	}, map[string]string{"Remote-User": "alice"})
	if status != http.StatusCreated {
		t.Fatalf("create project 1 = %d (%s)", status, raw)
	}
	var p1Resp struct {
		Project project.Project `json:"project"`
	}
	_ = json.Unmarshal(raw, &p1Resp)
	p1 := p1Resp.Project
	if p1.ID != "acme-engagement" || p1.Metadata["customer"] != "Acme Corp" {
		t.Fatalf("unexpected project 1: %+v", p1)
	}

	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects", map[string]any{
		"name": "Beta Engagement",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create project 2 = %d (%s)", status, raw)
	}
	var p2Resp struct {
		Project project.Project `json:"project"`
	}
	_ = json.Unmarshal(raw, &p2Resp)
	p2 := p2Resp.Project

	// Create planned tests in both projects.
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects/"+p1.ID+"/tests", map[string]any{
		"name":        "Spike #1",
		"maneuver_id": "mv-proj",
	}, map[string]string{"Remote-User": "alice"})
	if status != http.StatusCreated {
		t.Fatalf("create test 1 = %d (%s)", status, raw)
	}
	var t1Resp struct {
		Test project.Test `json:"test"`
	}
	_ = json.Unmarshal(raw, &t1Resp)
	t1 := t1Resp.Test

	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects/"+p2.ID+"/tests", map[string]any{
		"name":        "Beta #1",
		"maneuver_id": "mv-proj",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create test 2 = %d (%s)", status, raw)
	}
	var t2Resp struct {
		Test project.Test `json:"test"`
	}
	_ = json.Unmarshal(raw, &t2Resp)
	t2 := t2Resp.Test

	// Starting without a monitor is rejected with 400.
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects/"+p1.ID+"/tests/"+t1.ID+"/start", map[string]any{
		"lead_ms": 250,
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("start without monitor = %d (%s), want 400", status, raw)
	}

	// Subscribe to /ws/events to observe live maneuver run events triggered by project test start.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/events"
	wsConn, wsCtx := dialWithHeaders(t, wsURL, nil)
	var hello eventMsg
	if err := readJSON(wsCtx, wsConn, &hello); err != nil || hello.Type != "hello" {
		t.Fatalf("ws hello = %+v, err=%v", hello, err)
	}

	// Start t1 with create_monitor: true -> creates monitor job, runs Maneuver, acquires global lock.
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects/"+p1.ID+"/tests/"+t1.ID+"/start", map[string]any{
		"create_monitor": true,
		"lead_ms":        250,
	}, map[string]string{"Remote-User": "alice"})
	if status != http.StatusAccepted {
		t.Fatalf("start t1 = %d (%s)", status, raw)
	}
	var startRes fleet.StartProjectTestResult
	if err := json.Unmarshal(raw, &startRes); err != nil {
		t.Fatal(err)
	}
	if startRes.Test.Status != project.StatusRunning || startRes.Test.Monitor.JobID == "" || startRes.Run.RunID == "" {
		t.Fatalf("unexpected start result: %+v", startRes)
	}

	// Concurrent start on p2/t2 MUST be refused with 409 Conflict by the server-side lock.
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects/"+p2.ID+"/tests/"+t2.ID+"/start", map[string]any{
		"create_monitor": true,
		"lead_ms":        250,
	}, nil)
	if status != http.StatusConflict || !strings.Contains(string(raw), "active_test") {
		t.Fatalf("concurrent start = %d (%s), want 409 with active_test", status, raw)
	}

	// GET /api/projects/active returns the running test lock.
	status, raw = doJSON(t, http.MethodGet, srv.URL+"/api/projects/active", nil, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), t1.ID) {
		t.Fatalf("GET /api/projects/active = %d (%s)", status, raw)
	}

	// Confirm /ws/events sees the Maneuver reaching running state.
	sawRunning := false
	wsDeadline := time.Now().Add(wait)
	for time.Now().Before(wsDeadline) && !sawRunning {
		typ, data, err := wsConn.Read(wsCtx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg eventMsg
		if json.Unmarshal(data, &msg) == nil && msg.Type == "maneuver" && msg.Maneuver != nil && msg.Maneuver.RunID == startRes.Run.RunID {
			if msg.Maneuver.State == fleet.ManeuverStateRunning {
				sawRunning = true
			}
		}
	}
	if !sawRunning {
		t.Fatalf("did not observe maneuver running event for project test")
	}

	// Stop t1 -> stops Maneuver, records completion, releases lock.
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects/"+p1.ID+"/tests/"+t1.ID+"/stop", map[string]any{}, map[string]string{"Remote-User": "bob"})
	if status != http.StatusOK {
		t.Fatalf("stop t1 = %d (%s)", status, raw)
	}

	// Completed tests by date are returned on GET /api/projects/{id}.
	status, raw = doJSON(t, http.MethodGet, srv.URL+"/api/projects/"+p1.ID, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("get p1 = %d (%s)", status, raw)
	}
	var detail struct {
		Project         project.Project     `json:"project"`
		CompletedByDate []project.DateGroup `json:"completed_by_date"`
		Active          any                 `json:"active"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Active != nil {
		t.Fatalf("expected active lock nil after stop, got %+v", detail.Active)
	}
	if len(detail.CompletedByDate) != 1 || len(detail.CompletedByDate[0].Tests) != 1 {
		t.Fatalf("completed_by_date = %+v, want 1 date group with 1 test", detail.CompletedByDate)
	}
	if detail.CompletedByDate[0].Tests[0].Status != project.StatusCompleted {
		t.Fatalf("completed test status = %q", detail.CompletedByDate[0].Tests[0].Status)
	}

	// Persistence across server restart: reopen store from same dataDir.
	srv2 := openBaselineTestServer(t, dataDir)
	status, raw = doJSON(t, http.MethodGet, srv2.URL+"/api/projects/"+p1.ID, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), "Spike #1") {
		t.Fatalf("reload p1 = %d (%s)", status, raw)
	}
}

func TestProjectsSPARouteInBundle(t *testing.T) {
	srv := noBackend(t)
	status, _, page := body(t, srv.URL+"/projects")
	if status != http.StatusOK {
		t.Fatalf("GET /projects = %d", status)
	}
	for _, want := range []string{`<div id="root"></div>`, `/assets/dist/app.css`, `/assets/dist/app.js`} {
		if !strings.Contains(page, want) {
			t.Errorf("/projects SPA shell lacks %q", want)
		}
	}

	status, _, js := body(t, srv.URL+"/assets/dist/app.js")
	if status != http.StatusOK || len(js) == 0 {
		t.Fatalf("GET /assets/dist/app.js = %d (len=%d)", status, len(js))
	}
	for _, want := range []string{
		"/api/projects",
		"Projects & tests",
		"Active test lock held:",
		"completed tests",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("SPA bundle lacks %q", want)
		}
	}
}
