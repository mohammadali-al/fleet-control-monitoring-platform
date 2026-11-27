package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/baseline"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

func openBaselineTestServer(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := fleet.Open(fleet.Options{
		Config:  config.ServerConfig{},
		DataDir: dataDir,
		Logger:  log,
	})
	if err != nil {
		t.Fatalf("fleet.Open: %v", err)
	}
	if err := mgr.EnableObserve(dataDir); err != nil {
		t.Fatalf("EnableObserve: %v", err)
	}
	h, err := New(Options{
		Assets:    web.FS,
		Templates: web.Templates,
		Backend:   mgr,
		Logger:    log,
	})
	if err != nil {
		t.Fatalf("panel.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		mgr.Close()
	})
	return srv
}

func doJSON(t *testing.T, method, url string, payload any, headers map[string]string) (int, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, url, err)
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func TestBaselineAPIWithoutBackend(t *testing.T) {
	srv := noBackend(t)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/baseline"},
		{http.MethodPost, "/api/baseline"},
		{http.MethodPost, "/api/baseline/preview"},
		{http.MethodGet, "/api/baseline/x"},
		{http.MethodPut, "/api/baseline/x"},
		{http.MethodPost, "/api/baseline/x/duplicate"},
		{http.MethodDelete, "/api/baseline/x"},
	} {
		status, raw := doJSON(t, tc.method, srv.URL+tc.path, map[string]any{}, nil)
		if status != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d (%s), want 503", tc.method, tc.path, status, raw)
		}
	}
}

func TestBaselineAPICRUDPreviewAndReload(t *testing.T) {
	dataDir := t.TempDir()
	srv := openBaselineTestServer(t, dataDir)

	// Rejects non-JSON Content-Type on mutations.
	resp, err := http.Post(srv.URL+"/api/baseline", "text/plain", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON POST status = %d, want 415", resp.StatusCode)
	}

	// Initially empty.
	status, raw := doJSON(t, http.MethodGet, srv.URL+"/api/baseline", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/baseline = %d (%s)", status, raw)
	}
	var listResp struct {
		Items []baseline.Item `json:"items"`
	}
	if err := json.Unmarshal(raw, &listResp); err != nil || len(listResp.Items) != 0 {
		t.Fatalf("initial list = %s, err=%v", raw, err)
	}

	// Reject invalid template on create.
	status, _ = doJSON(t, http.MethodPost, srv.URL+"/api/baseline", map[string]any{
		"name":             "Broken",
		"command_template": "hey -q {rate",
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid template create status = %d, want 400", status)
	}

	// Create valid item.
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/baseline", map[string]any{
		"name":             "HTTP GET flood",
		"description":      "Sustained GET requests",
		"command_template": "hey -z {duration}s -q {rate} {url}",
		"defaults":         map[string]string{"rate": "200", "duration": "60"},
		"warmup_sec":       3.5,
		"tags":             []string{"http", "l7"},
	}, map[string]string{"Remote-User": "alice"})
	if status != http.StatusCreated {
		t.Fatalf("POST /api/baseline = %d (%s), want 201", status, raw)
	}
	var itemResp struct {
		Item baseline.Item `json:"item"`
	}
	if err := json.Unmarshal(raw, &itemResp); err != nil {
		t.Fatal(err)
	}
	created := itemResp.Item
	if created.ID != "http-get-flood" || created.CreatedBy != "alice" {
		t.Fatalf("created = %+v", created)
	}
	if !reflect.DeepEqual(created.Placeholders, []string{"duration", "rate", "url"}) {
		t.Fatalf("placeholders = %v", created.Placeholders)
	}

	// Get by ID.
	status, raw = doJSON(t, http.MethodGet, srv.URL+"/api/baseline/"+created.ID, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), "HTTP GET flood") {
		t.Fatalf("GET item = %d (%s)", status, raw)
	}
	status, _ = doJSON(t, http.MethodGet, srv.URL+"/api/baseline/does-not-exist", nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("GET missing = %d, want 404", status)
	}

	// Preview rejects unresolved {url}.
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/baseline/preview", map[string]any{
		"id": created.ID,
	}, nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("preview unresolved = %d (%s), want 422", status, raw)
	}
	var prevErr struct {
		Error      string   `json:"error"`
		Unresolved []string `json:"unresolved"`
	}
	if err := json.Unmarshal(raw, &prevErr); err != nil || !reflect.DeepEqual(prevErr.Unresolved, []string{"url"}) {
		t.Fatalf("preview error body = %s", raw)
	}

	// Preview succeeds when missing input is supplied.
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/baseline/preview", map[string]any{
		"command_template": created.CommandTemplate,
		"defaults":         created.Defaults,
		"inputs":           map[string]string{"url": "https://shop.example.test", "rate": "350"},
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("preview valid = %d (%s)", status, raw)
	}
	var prevOK baseline.RenderResult
	if err := json.Unmarshal(raw, &prevOK); err != nil {
		t.Fatal(err)
	}
	if prevOK.Command != "hey -z 60s -q 350 https://shop.example.test" {
		t.Fatalf("rendered = %q", prevOK.Command)
	}

	// Update item.
	status, raw = doJSON(t, http.MethodPut, srv.URL+"/api/baseline/"+created.ID, map[string]any{
		"name":             "HTTP GET flood",
		"description":      "Updated description",
		"command_template": "hey -z {duration}s -q {rate} {url}",
		"defaults":         map[string]string{"rate": "500", "duration": "90"},
	}, map[string]string{"Remote-User": "bob"})
	if status != http.StatusOK {
		t.Fatalf("PUT = %d (%s)", status, raw)
	}

	// Duplicate item.
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/baseline/"+created.ID+"/duplicate", map[string]any{}, nil)
	if status != http.StatusCreated {
		t.Fatalf("duplicate = %d (%s)", status, raw)
	}
	var dupResp struct {
		Item baseline.Item `json:"item"`
	}
	if err := json.Unmarshal(raw, &dupResp); err != nil {
		t.Fatal(err)
	}
	if dupResp.Item.ID == created.ID || dupResp.Item.Name != "HTTP GET flood (copy)" {
		t.Fatalf("dup = %+v", dupResp.Item)
	}

	// Verify file persisted under <dataDir>/baseline/items.json.
	if _, err := os.Stat(filepath.Join(dataDir, config.BaselineDirName, baseline.FileName)); err != nil {
		t.Fatalf("persisted file missing: %v", err)
	}

	// Simulate server restart: new manager + new panel server on same dataDir.
	srv.Close()
	srv2 := openBaselineTestServer(t, dataDir)
	status, raw = doJSON(t, http.MethodGet, srv2.URL+"/api/baseline", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET after restart = %d (%s)", status, raw)
	}
	if err := json.Unmarshal(raw, &listResp); err != nil || len(listResp.Items) != 2 {
		t.Fatalf("after restart items = %s", raw)
	}
	var foundUpdated bool
	for _, it := range listResp.Items {
		if it.ID == created.ID && it.Defaults["rate"] == "500" && it.Description == "Updated description" {
			foundUpdated = true
		}
	}
	if !foundUpdated {
		t.Fatalf("reloaded items did not preserve update: %+v", listResp.Items)
	}

	// Delete duplicate and verify removal persists.
	status, _ = doJSON(t, http.MethodDelete, srv2.URL+"/api/baseline/"+dupResp.Item.ID, map[string]any{}, nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200", status)
	}
	status, raw = doJSON(t, http.MethodGet, srv2.URL+"/api/baseline", nil, nil)
	if status != http.StatusOK {
		t.Fatal(status)
	}
	if err := json.Unmarshal(raw, &listResp); err != nil || len(listResp.Items) != 1 {
		t.Fatalf("after delete items = %s", raw)
	}
}

func TestBaselineSPARouteInBundle(t *testing.T) {
	srv := noBackend(t)
	status, _, page := body(t, srv.URL+"/baseline")
	if status != http.StatusOK {
		t.Fatalf("GET /baseline = %d", status)
	}
	for _, want := range []string{`<div id="root"></div>`, `/assets/dist/app.css`, `/assets/dist/app.js`} {
		if !strings.Contains(page, want) {
			t.Errorf("/baseline SPA shell lacks %q", want)
		}
	}

	status, _, js := body(t, srv.URL+"/assets/dist/app.js")
	if status != http.StatusOK || len(js) == 0 {
		t.Fatalf("GET /assets/dist/app.js = %d (len=%d)", status, len(js))
	}
	for _, want := range []string{
		"Baseline items",
		"/api/baseline",
		"/api/baseline/preview",
		"/duplicate",
		"Substitution preview",
		"Create item",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("SPA bundle lacks %q", want)
		}
	}
}
