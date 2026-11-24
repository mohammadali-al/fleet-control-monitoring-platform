package panel

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

func get(t *testing.T, h http.Handler, method, target string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec.Result()
}

func TestServesPanel(t *testing.T) {
	h := Handler(web.FS)
	srv := httptest.NewServer(h)
	defer srv.Close()

	cases := []struct {
		path     string
		status   int
		ctPrefix string
		body     string
	}{
		{"/", 200, "text/html", "<html"},
		{"/index.html", 200, "text/html", "<html"},
		{"/machines.html", 200, "text/html", "<html"},
		{"/assets/css/tokens.css", 200, "text/css", ""},
		{"/assets/js/app.js", 200, "text/javascript", ""},
		{"/nope", 404, "", ""},
		{"/nope.html", 404, "", ""},
		{"/assets/", 404, "", ""},         // no directory listing
		{"/assets", 404, "", ""},          // ditto
		{"/assets/css/", 404, "", ""},     // ditto
		{"/_src/shell.html", 404, "", ""}, // source templates are not embedded
		{"/embed.go", 404, "", ""},        // Go sources are not served
		{"/../go.mod", 404, "", ""},
	}
	for _, c := range cases {
		resp, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("GET %s = %d, want %d", c.path, resp.StatusCode, c.status)
			continue
		}
		if c.ctPrefix != "" && !strings.HasPrefix(resp.Header.Get("Content-Type"), c.ctPrefix) {
			t.Errorf("GET %s Content-Type = %q, want prefix %q", c.path, resp.Header.Get("Content-Type"), c.ctPrefix)
		}
		if c.body != "" && !strings.Contains(strings.ToLower(string(body)), c.body) {
			t.Errorf("GET %s body missing %q", c.path, c.body)
		}
	}
}

func TestRootIsIndex(t *testing.T) {
	h := Handler(web.FS)
	want, err := web.FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	resp := get(t, h, http.MethodGet, "/")
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, want) {
		t.Error("GET / does not return web/index.html")
	}
}

func TestMethods(t *testing.T) {
	h := Handler(web.FS)
	if r := get(t, h, http.MethodHead, "/machines.html"); r.StatusCode != 200 {
		t.Errorf("HEAD = %d, want 200", r.StatusCode)
	}
	if r := get(t, h, http.MethodPost, "/"); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", r.StatusCode)
	}
}

func TestURL(t *testing.T) {
	for in, want := range map[string]string{
		":6789":          "http://localhost:6789",
		"0.0.0.0:80":     "http://localhost:80",
		"127.0.0.1:7000": "http://127.0.0.1:7000",
	} {
		if got := URL(in); got != want {
			t.Errorf("URL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServeShutsDownOnCancel(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, Handler(web.FS), log) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	if _, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
		t.Error("server still accepting after shutdown")
	}
}
