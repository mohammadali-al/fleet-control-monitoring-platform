package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pastebin"
)

type pasteFrame struct {
	Text    string `json:"text"`
	Version uint64 `json:"version"`
	Client  string `json:"client"`
}

func readPasteFrame(t *testing.T, ctx context.Context, c *websocket.Conn) pasteFrame {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, data, err := c.Read(rctx)
	if err != nil {
		t.Fatalf("read pastebin frame: %v", err)
	}
	var f pasteFrame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decode frame %s: %v", data, err)
	}
	return f
}

func TestPastebinAPIWithoutBackend(t *testing.T) {
	srv := noBackend(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		status, raw := doJSON(t, method, srv.URL+"/api/pastebin", map[string]any{"text": "x"}, nil)
		if status != http.StatusServiceUnavailable {
			t.Errorf("%s /api/pastebin = %d (%s), want 503", method, status, raw)
		}
	}
	resp, err := http.Get(srv.URL + "/ws/pastebin")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /ws/pastebin = %d, want 503", resp.StatusCode)
	}
}

func TestPastebinPostGetPersistAndSizeCap(t *testing.T) {
	dataDir := t.TempDir()
	srv := openBaselineTestServer(t, dataDir)

	status, raw := doJSON(t, http.MethodGet, srv.URL+"/api/pastebin", nil, nil)
	var got pasteFrame
	if status != http.StatusOK || json.Unmarshal(raw, &got) != nil || got.Text != "" || got.Version != 0 {
		t.Fatalf("initial GET = %d %s", status, raw)
	}

	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/pastebin", map[string]any{"text": "hello pad", "client": "c1"}, nil)
	if status != http.StatusOK {
		t.Fatalf("POST = %d (%s)", status, raw)
	}
	var posted struct {
		Version uint64 `json:"version"`
	}
	if err := json.Unmarshal(raw, &posted); err != nil || posted.Version != 1 {
		t.Fatalf("POST body = %s, want version 1", raw)
	}

	status, raw = doJSON(t, http.MethodGet, srv.URL+"/api/pastebin", nil, nil)
	if status != http.StatusOK || json.Unmarshal(raw, &got) != nil || got.Text != "hello pad" || got.Version != 1 {
		t.Fatalf("GET after POST = %d %s", status, raw)
	}
	disk, err := os.ReadFile(filepath.Join(dataDir, "pastebin", "paste.txt"))
	if err != nil || string(disk) != "hello pad" {
		t.Fatalf("paste.txt = %q err=%v", disk, err)
	}

	// Missing text and unknown fields are rejected.
	for _, body := range []map[string]any{{"client": "c1"}, {"text": "x", "bogus": 1}} {
		if status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/pastebin", body, nil); status != http.StatusBadRequest {
			t.Errorf("POST %v = %d (%s), want 400", body, status, raw)
		}
	}

	// Over the 5 MB cap: 400, and the pad is unchanged.
	big := strings.Repeat("x", pastebin.MaxBytes+1)
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/pastebin", map[string]any{"text": big}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("oversize POST = %d, want 400 (%.80s)", status, raw)
	}
	status, raw = doJSON(t, http.MethodGet, srv.URL+"/api/pastebin", nil, nil)
	if status != http.StatusOK || json.Unmarshal(raw, &got) != nil || got.Text != "hello pad" {
		t.Fatalf("pad changed after rejected POST: %s", raw)
	}

	// Non-JSON mutations are refused like every other API.
	resp, err := http.Post(srv.URL+"/api/pastebin", "text/plain", strings.NewReader(`{"text":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain POST = %d, want 415", resp.StatusCode)
	}
}

func TestPastebinSurvivesRestart(t *testing.T) {
	dataDir := t.TempDir()
	srv := openBaselineTestServer(t, dataDir)
	if status, raw := doJSON(t, http.MethodPost, srv.URL+"/api/pastebin", map[string]any{"text": "persisted"}, nil); status != http.StatusOK {
		t.Fatalf("POST = %d (%s)", status, raw)
	}
	srv.Close()

	srv2 := openBaselineTestServer(t, dataDir)
	status, raw := doJSON(t, http.MethodGet, srv2.URL+"/api/pastebin", nil, nil)
	var got pasteFrame
	if status != http.StatusOK || json.Unmarshal(raw, &got) != nil || got.Text != "persisted" {
		t.Fatalf("GET after restart = %d %s", status, raw)
	}
}

func TestPastebinWebsocketBroadcast(t *testing.T) {
	srv := openBaselineTestServer(t, t.TempDir())
	if status, raw := doJSON(t, http.MethodPost, srv.URL+"/api/pastebin", map[string]any{"text": "before"}, nil); status != http.StatusOK {
		t.Fatalf("seed POST = %d (%s)", status, raw)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/pastebin"
	c1, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	defer c1.CloseNow()
	c2, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer c2.CloseNow()

	for i, c := range []*websocket.Conn{c1, c2} {
		if f := readPasteFrame(t, ctx, c); f.Text != "before" || f.Version != 1 || f.Client != "" {
			t.Fatalf("conn %d snapshot = %+v", i+1, f)
		}
	}

	if status, raw := doJSON(t, http.MethodPost, srv.URL+"/api/pastebin", map[string]any{"text": "after", "client": "tab-x"}, nil); status != http.StatusOK {
		t.Fatalf("POST = %d (%s)", status, raw)
	}
	for i, c := range []*websocket.Conn{c1, c2} {
		if f := readPasteFrame(t, ctx, c); f.Text != "after" || f.Version != 2 || f.Client != "tab-x" {
			t.Fatalf("conn %d broadcast = %+v", i+1, f)
		}
	}
}
