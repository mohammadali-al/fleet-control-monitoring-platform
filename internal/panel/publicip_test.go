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

	"github.com/coder/websocket"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

type panelCaptureAdapter struct{ c *capture.Controller }

func (a panelCaptureAdapter) Capture(sessionID, cgroupPath string) (agent.SessionCapture, error) {
	return a.c.Capture(sessionID, cgroupPath)
}
func (a panelCaptureAdapter) Close()                                { a.c.Close() }
func (a panelCaptureAdapter) PublicIPCache() *capture.PublicIPCache { return a.c.PublicIPCache() }

func TestRefreshPublicIPEndpointAndLiveFlow(t *testing.T) {
	var mu sync.Mutex
	currentIP := "203.0.113.9"
	mkReflector := func() *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			ip := currentIP
			mu.Unlock()
			_, _ = w.Write([]byte(ip))
		}))
		t.Cleanup(s.Close)
		return s
	}
	r1, r2 := mkReflector(), mkReflector()

	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ra, err := mt.StartAgent("w1", agent.Config{
		DataDir: t.TempDir(),
		Capture: config.CaptureConfig{Reflectors: []string{r1.URL, r2.URL}},
		CaptureFactory: func(ctx context.Context, cfg capture.Config) (agent.Capturer, error) {
			ctrl, err := capture.NewController(cfg)
			if err != nil {
				return nil, err
			}
			ctrl.Start(ctx)
			return panelCaptureAdapter{ctrl}, nil
		},
		Logger: quiet,
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	t.Cleanup(func() { _ = ra.Stop() })

	reg := registry.New()
	for _, mc := range []registry.Machine{
		{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true},
		{ID: "w2", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.2"}, Provisioned: true},
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
		GateSecret:   []byte("test-secret"),
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

	h, err := New(Options{Assets: web.FS, Templates: web.Templates, Backend: mgr, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// Wait for w1 online and initial reflector IP to populate registry + fragments.
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if info, ok := mgr.Machine("w1"); ok && info.State == fleet.StateOnline && info.PublicIP == "203.0.113.9" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if info, _ := mgr.Machine("w1"); info.PublicIP != "203.0.113.9" {
		t.Fatalf("initial w1 PublicIP = %q, want 203.0.113.9", info.PublicIP)
	}

	_, _, rows := body(t, srv.URL+"/fragments/machines-rows")
	if !strings.Contains(rows, "203.0.113.9") {
		t.Errorf("machines-rows missing initial public IP: %s", rows)
	}

	// Subscribe to /ws/events to verify live push on manual refresh.
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

	// Change reflector IP and trigger manual refresh on online w1.
	mu.Lock()
	currentIP = "198.51.100.42"
	mu.Unlock()
	if code, b := post("/api/machines/w1/refresh-public-ip", "application/json", `{}`); code != http.StatusOK || !strings.Contains(b, `"requested"`) {
		t.Fatalf("POST w1 refresh = (%d, %s), want 200 requested", code, b)
	}

	// Expect /ws/events to carry the updated machine PublicIP.
	sawLive := false
	for !sawLive {
		var msg eventMsg
		if err := readJSON(wsCtx, wsConn, &msg); err != nil {
			t.Fatalf("read ws event: %v", err)
		}
		if msg.Type == "machine" && msg.Machine != nil && msg.Machine.ID == "w1" && msg.Machine.PublicIP == "198.51.100.42" {
			sawLive = true
		}
	}

	// Offline machine gives clear "agent offline" 409 response.
	if code, b := post("/api/machines/w2/refresh-public-ip", "application/json", `{}`); code != http.StatusConflict || !strings.Contains(strings.ToLower(b), "agent offline") {
		t.Errorf("POST offline w2 refresh = (%d, %s), want 409 containing 'agent offline'", code, b)
	}

	// Unknown machine -> 404; non-JSON -> 415; unknown body field -> 400.
	if code, _ := post("/api/machines/nope/refresh-public-ip", "application/json", `{}`); code != http.StatusNotFound {
		t.Errorf("POST unknown refresh = %d, want 404", code)
	}
	if code, _ := post("/api/machines/w1/refresh-public-ip", "text/plain", `{}`); code != http.StatusUnsupportedMediaType {
		t.Errorf("POST non-JSON refresh = %d, want 415", code)
	}
	if code, _ := post("/api/machines/w1/refresh-public-ip", "application/json", `{"bad":1}`); code != http.StatusBadRequest {
		t.Errorf("POST bad JSON refresh = %d, want 400", code)
	}

	// Without backend -> 503.
	nb := noBackend(t)
	req, _ := http.NewRequest("POST", nb.URL+"/api/machines/w1/refresh-public-ip", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no-backend refresh = %d, want 503", resp.StatusCode)
	}
	_ = json.Marshal // keep encoding/json check clean if unused
}
