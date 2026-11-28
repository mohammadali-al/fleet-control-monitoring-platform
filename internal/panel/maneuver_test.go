package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/baseline"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/maneuver"
)

func TestManeuverAPIWithoutBackend(t *testing.T) {
	srv := noBackend(t)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/maneuvers"},
		{http.MethodPost, "/api/maneuvers"},
		{http.MethodPost, "/api/maneuvers/preview"},
		{http.MethodGet, "/api/maneuvers/x"},
		{http.MethodPut, "/api/maneuvers/x"},
		{http.MethodPost, "/api/maneuvers/x/duplicate"},
		{http.MethodDelete, "/api/maneuvers/x"},
	} {
		status, raw := doJSON(t, tc.method, srv.URL+tc.path, map[string]any{}, nil)
		if status != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d (%s), want 503", tc.method, tc.path, status, raw)
		}
	}
}

func TestManeuverAPICRUDPreviewAndValidation(t *testing.T) {
	dataDir := t.TempDir()
	srv := openBaselineTestServer(t, dataDir)

	// Seed a baseline item for timeline materialization.
	status, raw := doJSON(t, http.MethodPost, srv.URL+"/api/baseline", map[string]any{
		"id":               "http-get",
		"name":             "HTTP GET flood",
		"command_template": "hey -z {duration}s -q {rate} {url}",
		"defaults":         map[string]string{"rate": "200"},
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("seed baseline = %d (%s)", status, raw)
	}

	// Reject deferred patterns (linear, spikes).
	for _, pat := range []string{maneuver.PatternLinear, maneuver.PatternSpikes} {
		status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/maneuvers", map[string]any{
			"name":      "Deferred",
			"target":    map[string]string{"ip": "198.51.100.10"},
			"total_sec": 60,
			"pattern":   map[string]any{"type": pat},
		}, nil)
		if status != http.StatusBadRequest || !strings.Contains(string(raw), "Load Controller") {
			t.Fatalf("pattern %q status=%d (%s), want 400 with Load Controller note", pat, status, raw)
		}
	}

	// Reject invalid target IP / multiple origins.
	status, _ = doJSON(t, http.MethodPost, srv.URL+"/api/maneuvers", map[string]any{
		"name":      "Bad IP",
		"target":    map[string]string{"ip": "not-an-ip"},
		"total_sec": 60,
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("bad ip status=%d, want 400", status)
	}

	// Preview materializes baseline items with target (ip, origin).
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/maneuvers/preview", map[string]any{
		"name":      "Web probe",
		"target":    map[string]string{"ip": "198.51.100.10", "origin": "https://shop.example.test"},
		"total_sec": 120,
		"pattern":   map[string]any{"type": "all"},
		"timeline": []map[string]any{
			{"baseline_id": "http-get", "start_sec": 10, "duration_sec": 30, "replicas": 2, "inputs": map[string]string{"path": "/api"}},
		},
		"agents": []string{"w1"},
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("preview = %d (%s)", status, raw)
	}
	var plan maneuver.MaterializedPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Command != "hey -z 30s -q 200 https://shop.example.test/api" {
		t.Fatalf("preview plan = %+v", plan)
	}

	// Create valid Maneuver.
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/maneuvers", map[string]any{
		"name":      "Web probe",
		"target":    map[string]string{"ip": "198.51.100.10", "origin": "https://shop.example.test"},
		"total_sec": 120,
		"pattern":   map[string]any{"type": "all", "max_cpu_cores": 4, "max_ram_gb": 6},
		"timeline": []map[string]any{
			{"baseline_id": "http-get", "start_sec": 0, "duration_sec": 60, "replicas": 2},
		},
		"agents": []string{"w1", "w2"},
	}, map[string]string{"Remote-User": "alice"})
	if status != http.StatusCreated {
		t.Fatalf("create = %d (%s)", status, raw)
	}
	var mResp maneuverResponse
	if err := json.Unmarshal(raw, &mResp); err != nil {
		t.Fatal(err)
	}
	created := mResp.Maneuver
	if created.ID != "web-probe" || created.CreatedBy != "alice" {
		t.Fatalf("created = %+v", created)
	}

	// Reload on a fresh server instance sees persisted Maneuver.
	srv2 := openBaselineTestServer(t, dataDir)
	status, raw = doJSON(t, http.MethodGet, srv2.URL+"/api/maneuvers", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("reload list = %d (%s)", status, raw)
	}
	var listResp maneuverListResponse
	if err := json.Unmarshal(raw, &listResp); err != nil || len(listResp.Maneuvers) != 1 || listResp.Maneuvers[0].ID != created.ID {
		t.Fatalf("reloaded list = %+v", listResp)
	}

	// Duplicate, Update, Delete.
	status, raw = doJSON(t, http.MethodPost, srv2.URL+"/api/maneuvers/"+created.ID+"/duplicate", map[string]any{}, nil)
	if status != http.StatusCreated {
		t.Fatalf("duplicate = %d (%s)", status, raw)
	}
	var dupResp maneuverResponse
	_ = json.Unmarshal(raw, &dupResp)
	if dupResp.Maneuver.ID == created.ID {
		t.Fatalf("duplicate id matched original")
	}

	// Unresolved placeholder on update returns 422.
	status, raw = doJSON(t, http.MethodPost, srv2.URL+"/api/baseline", map[string]any{
		"id":               "needs-arg",
		"name":             "Needs arg",
		"command_template": "echo {custom_token}",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("seed baseline 2 = %d (%s)", status, raw)
	}
	status, raw = doJSON(t, http.MethodPut, srv2.URL+"/api/maneuvers/"+created.ID, map[string]any{
		"name":      "Web probe",
		"target":    map[string]string{"ip": "198.51.100.10"},
		"total_sec": 60,
		"timeline": []map[string]any{
			{"baseline_id": "needs-arg", "start_sec": 0, "duration_sec": 10},
		},
	}, nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("unresolved update = %d (%s), want 422", status, raw)
	}
	_ = baseline.ErrNotFound

	status, _ = doJSON(t, http.MethodDelete, srv2.URL+"/api/maneuvers/"+dupResp.Maneuver.ID, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("delete = %d", status)
	}
}
