package panel

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

// noBackend serves the panel without a backend: pages render, the API says 503.
func noBackend(t *testing.T) *httptest.Server {
	t.Helper()
	h, err := New(Options{Assets: web.FS, Templates: web.Templates})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func body(t *testing.T, url string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func TestServesPanel(t *testing.T) {
	srv := noBackend(t)
	cases := []struct {
		path     string
		status   int
		ctPrefix string
		body     string
	}{
		{"/", 200, "text/html", `<div id="root">`},
		{"/index.html", 200, "text/html", `<div id="root">`},
		{"/machines", 200, "text/html", `<div id="root">`},
		{"/commands", 200, "text/html", `<div id="root">`},
		{"/terminal", 200, "text/html", `<div id="root">`},
		{"/watchdog", 200, "text/html", `<div id="root">`},
		{"/evidence", 200, "text/html", `<div id="root">`},
		{"/backup", 200, "text/html", `<div id="root">`},
		{"/projects", 200, "text/html", `<div id="root">`}, // SPA catch-all
		{"/deep/app/route", 200, "text/html", `<div id="root">`},
		{"/projects.html", 200, "text/html", "<html"}, // legacy fallback
		{"/styleguide.html", 200, "text/html", "<html"},
		{"/assets/dist/app.js", 200, "text/javascript", ""},
		{"/assets/dist/app.css", 200, "text/css", ".bg-canvas"},
		{"/assets/css/fonts.css", 200, "text/css", "space mono"},
		{"/assets/css/tokens.css", 200, "text/css", ""},
		{"/assets/css/bg.css", 200, "text/css", ".bg-canvas"},
		{"/assets/js/app.js", 200, "text/javascript", ""},
		{"/assets/js/bg.js", 200, "text/javascript", ""},
		{"/assets/js/live.js", 200, "text/javascript", ""},
		{"/assets/fonts/space-mono-700-latin.woff2", 200, "", ""},
		{"/assets/fonts/red-hat-mono-latin.woff2", 200, "", ""},
		{"/assets/vendor/xterm/xterm.js", 200, "text/javascript", ""},
		{"/assets/vendor/xterm/xterm.css", 200, "text/css", ""},
		{"/assets/favicon.svg", 200, "image/svg+xml", "#ff2a3c"},
		{"/assets/favicon-32x32.png", 200, "image/png", ""},
		{"/assets/apple-touch-icon.png", 200, "image/png", ""},
		{"/favicon.ico", 200, "image/svg+xml", "#ff2a3c"},
		{"/api/nope", 404, "application/json", "not found"},
		{"/fragments/nope", 404, "", ""},
		{"/ws/nope", 404, "", ""},
		{"/assets/nope.js", 404, "", ""},
		{"/assets/", 404, "", ""},     // no directory listing
		{"/assets", 404, "", ""},      // ditto
		{"/assets/css/", 404, "", ""}, // ditto
		{"/_src/shell.html", 404, "", ""},
		{"/pages/machines.html", 404, "", ""},
		{"/dist/index.html", 404, "", ""},
		{"/src/main.jsx", 404, "", ""},
		{"/embed.go", 404, "", ""},
		{"/templates.go", 404, "", ""},
	}
	for _, c := range cases {
		resp, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("GET %s = %d, want %d", c.path, resp.StatusCode, c.status)
			continue
		}
		if c.ctPrefix != "" && !strings.HasPrefix(resp.Header.Get("Content-Type"), c.ctPrefix) {
			t.Errorf("GET %s Content-Type = %q, want prefix %q", c.path, resp.Header.Get("Content-Type"), c.ctPrefix)
		}
		if c.body != "" && !strings.Contains(strings.ToLower(string(b)), c.body) {
			t.Errorf("GET %s body missing %q", c.path, c.body)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q", c.path, got)
		}
	}
	// /favicon.ico redirects cleanly to the self-hosted SVG instead of 404ing or serving the SPA shell.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, err := noRedirect.Get(srv.URL + "/favicon.ico"); err != nil {
		t.Fatalf("GET /favicon.ico: %v", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/assets/favicon.svg" {
			t.Errorf("GET /favicon.ico = %d Location=%q, want 302 -> /assets/favicon.svg", resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// Path traversal never leaks repo files.
	_, _, traversal := body(t, srv.URL+"/../go.mod")
	if strings.Contains(traversal, "module github.com") {
		t.Error("path traversal leaked go.mod")
	}
}

func TestSPAShellAndCatchAll(t *testing.T) {
	srv := noBackend(t)
	_, _, root := body(t, srv.URL+"/")
	_, _, idx := body(t, srv.URL+"/index.html")
	_, _, machines := body(t, srv.URL+"/machines")
	_, _, commands := body(t, srv.URL+"/commands")
	_, _, terminal := body(t, srv.URL+"/terminal")
	_, _, watchdog := body(t, srv.URL+"/watchdog")
	_, _, evidence := body(t, srv.URL+"/evidence")
	_, _, backup := body(t, srv.URL+"/backup")
	_, _, deep := body(t, srv.URL+"/machines/w1")
	if root != idx || root != machines || root != commands || root != terminal || root != watchdog || root != evidence || root != backup || root != deep {
		t.Error("SPA routes did not all serve the identical shell HTML")
	}
	for _, must := range []string{
		`data-theme="dark"`,
		`data-bg="matrix"`,
		`localStorage.getItem('fleet-theme')`,
		`href="/assets/favicon.svg"`,
		`href="/assets/favicon-32x32.png"`,
		`href="/assets/apple-touch-icon.png"`,
		`/assets/dist/app.css`,
		`/assets/dist/app.js`,
		`<div id="root"></div>`,
	} {
		if !strings.Contains(root, must) {
			t.Errorf("SPA shell missing %q", must)
		}
	}
	if strings.Contains(root, "fonts.googleapis.com") || strings.Contains(root, "fonts.gstatic.com") {
		t.Error("SPA shell has external Google Fonts reference")
	}
}

func TestOperateSPARoutesInBundle(t *testing.T) {
	srv := noBackend(t)
	for _, route := range []string{"/commands", "/terminal"} {
		status, _, page := body(t, srv.URL+route)
		if status != 200 {
			t.Fatalf("GET %s = %d", route, status)
		}
		for _, want := range []string{`<div id="root"></div>`, `/assets/dist/app.css`, `/assets/dist/app.js`} {
			if !strings.Contains(page, want) {
				t.Errorf("%s SPA shell lacks %q", route, want)
			}
		}
	}

	status, _, js := body(t, srv.URL+"/assets/dist/app.js")
	if status != 200 || len(js) == 0 {
		t.Fatalf("GET /assets/dist/app.js = %d (len=%d)", status, len(js))
	}
	for _, want := range []string{
		"Command runner",
		"/api/commands",
		"/ws/events",
		"/ws/shell",
		"/assets/vendor/xterm/xterm.js",
		"/assets/vendor/xterm/addon-fit.js",
		"/assets/vendor/xterm/xterm.css",
		"Confirm a dangerous command",
		"concurrent long-lived session",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("SPA bundle lacks %q", want)
		}
	}
	for _, stale := range []string{
		"Runs as the agent's service user",
		"shares the machine's serial queue",
		"starts when the machine is free",
	} {
		if strings.Contains(js, stale) {
			t.Errorf("SPA bundle still contains stale GAP-10 copy %q", stale)
		}
	}
}

func TestObserveSPARoutesInBundle(t *testing.T) {
	srv := noBackend(t)
	for _, route := range []string{"/watchdog", "/evidence"} {
		status, _, page := body(t, srv.URL+route)
		if status != 200 {
			t.Fatalf("GET %s = %d", route, status)
		}
		for _, want := range []string{`<div id="root"></div>`, `/assets/dist/app.css`, `/assets/dist/app.js`} {
			if !strings.Contains(page, want) {
				t.Errorf("%s SPA shell lacks %q", route, want)
			}
		}
	}

	status, _, js := body(t, srv.URL+"/assets/dist/app.js")
	if status != 200 || len(js) == 0 {
		t.Fatalf("GET /assets/dist/app.js = %d (len=%d)", status, len(js))
	}
	for _, want := range []string{
		"Watchdog",
		"/api/watchdog",
		"/api/watchdog/reset",
		"/api/watchdog/policy",
		"/api/watchdog/events",
		"/ws/watchdog",
		"Reset all counters",
		"Evidence",
		"/api/evidence/sessions",
		"/api/evidence/sync",
		"/api/evidence/pull",
		"/api/evidence/manifest/",
		"/api/evidence/export",
		"payload_head",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("SPA bundle lacks %q", want)
		}
	}
	if strings.Contains(js, "when a segment also holds other sessions it is an upper bound") {
		t.Error("SPA bundle contains stale shared-segment upper-bound copy")
	}
}

func TestAdministerSPARouteInBundle(t *testing.T) {
	srv := noBackend(t)
	status, _, page := body(t, srv.URL+"/backup")
	if status != 200 {
		t.Fatalf("GET /backup = %d", status)
	}
	for _, want := range []string{`<div id="root"></div>`, `/assets/dist/app.css`, `/assets/dist/app.js`} {
		if !strings.Contains(page, want) {
			t.Errorf("/backup SPA shell lacks %q", want)
		}
	}

	status, _, js := body(t, srv.URL+"/assets/dist/app.js")
	if status != 200 || len(js) == 0 {
		t.Fatalf("GET /assets/dist/app.js = %d (len=%d)", status, len(js))
	}
	for _, want := range []string{
		"Backup server",
		"/api/backup",
		"/api/backup/diff",
		"/api/backup/server",
		"/api/backup/run",
		"/api/backup/import",
		"/api/backup/schedule",
		"/api/backup/reminder/dismiss",
		"Back up now",
		"Import from backup",
		"Add or replace backup server",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("SPA bundle lacks %q", want)
		}
	}
}

func TestMethods(t *testing.T) {
	srv := noBackend(t)
	resp, err := http.Head(srv.URL + "/machines")
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("HEAD = %v %v, want 200", resp, err)
	}
	resp, err = http.Post(srv.URL+"/machines", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST page = %d, want 405", resp.StatusCode)
	}
}

// Un-ported legacy pages still render from Go templates during migration.
func TestLegacyPagesRender(t *testing.T) {
	srv := noBackend(t)
	navRe := regexp.MustCompile(`<a class="nav__link" href="([a-z]+)\.html" aria-current="page"`)
	for _, p := range pages {
		if p.spa {
			continue
		}
		status, hdr, html := body(t, srv.URL+"/"+p.name+".html")
		if status != 200 {
			t.Errorf("%s: status %d", p.name, status)
			continue
		}
		if !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") {
			t.Errorf("%s: Content-Type %q", p.name, hdr.Get("Content-Type"))
		}
		if !strings.HasPrefix(strings.ToLower(html), "<!doctype html>") {
			t.Errorf("%s: no doctype", p.name)
		}
		for _, must := range []string{`data-theme="dark"`, `data-bg="matrix"`, `localStorage.getItem('fleet-theme')`, `href="assets/favicon.svg"`, `id="bg-canvas"`, `assets/css/fonts.css`, `assets/css/bg.css`, `assets/js/bg.js`, `<symbol id="i-fleet"`, `class="sidebar"`, `class="topbar"`, `theme-toggle`, `id="main"`, `assets/js/app.js`, `data-page="` + p.name + `"`, `<h1>`} {
			if !strings.Contains(html, must) {
				t.Errorf("%s: missing %q", p.name, must)
			}
		}
		if strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
			t.Errorf("%s: external Google Fonts reference found (must be self-hosted)", p.name)
		}
		if strings.Contains(html, "{{") || strings.Contains(html, "}}") {
			t.Errorf("%s: unresolved template action in output", p.name)
		}
		if got := navRe.FindAllStringSubmatch(html, -1); len(got) != 1 || got[0][1] != p.name {
			t.Errorf("%s: aria-current marks %v, want only %s", p.name, got, p.name)
		}
		if mock := strings.Contains(html, "Mock data</span>"); mock != p.mock {
			t.Errorf("%s: Mock chip present=%v, want %v", p.name, mock, p.mock)
		}
	}
}

func TestLivePagesLoadLiveScript(t *testing.T) {
	srv := noBackend(t)
	for _, name := range []string{"commands", "terminal"} {
		_, _, html := body(t, srv.URL+"/"+name+".html")
		if !strings.Contains(html, "assets/js/live.js") {
			t.Errorf("%s: live.js not loaded", name)
		}
	}
	_, _, term := body(t, srv.URL+"/terminal.html")
	for _, want := range []string{"assets/vendor/xterm/xterm.js", "assets/vendor/xterm/addon-fit.js", "assets/vendor/xterm/xterm.css"} {
		if !strings.Contains(term, want) {
			t.Errorf("terminal: missing %s", want)
		}
	}
	_, _, mock := body(t, srv.URL+"/projects.html")
	if strings.Contains(mock, "live.js") {
		t.Error("a mock page loads live.js")
	}
}

// D-0005: on the Maneuver stub, Spike is disabled with a reason and
// all-in-one is the default.
func TestManeuverStub(t *testing.T) {
	srv := noBackend(t)
	_, _, html := body(t, srv.URL+"/maneuver.html")
	spike := regexp.MustCompile(`<input[^>]*value="spikes"[^>]*>`).FindString(html)
	if spike == "" || !strings.Contains(spike, "disabled") || strings.Contains(spike, "checked") {
		t.Errorf("spike input = %q, want disabled and not checked", spike)
	}
	all := regexp.MustCompile(`<input[^>]*value="all"[^>]*>`).FindString(html)
	if !strings.Contains(all, "checked") {
		t.Errorf("all-in-one input = %q, want checked", all)
	}
	checked := 0
	for _, in := range regexp.MustCompile(`<input[^>]*name="pattern"[^>]*>`).FindAllString(html, -1) {
		if strings.Contains(in, "checked") {
			checked++
		}
	}
	if checked != 1 {
		t.Errorf("%d pattern radios checked, want exactly the default", checked)
	}
	if !strings.Contains(html, "needs the Load Controller") || !strings.Contains(html, "deferred") {
		t.Error("no visible reason for the disabled Spike pattern")
	}
	if !strings.Contains(html, `aria-describedby="spike-reason"`) {
		t.Error("the disabled Spike input is not linked to its reason")
	}
}

func TestAPIWithoutBackend(t *testing.T) {
	srv := noBackend(t)
	for _, path := range []string{"/api/machines", "/api/commands", "/api/commands/x"} {
		status, _, b := body(t, srv.URL+path)
		if status != http.StatusServiceUnavailable || !strings.Contains(b, "error") {
			t.Errorf("GET %s = %d %s, want 503 JSON error", path, status, b)
		}
	}
	// Un-ported legacy pages still render, saying so.
	_, _, html := body(t, srv.URL+"/commands.html")
	if !strings.Contains(html, "without a backend") {
		t.Error("commands page does not say there is no backend")
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

func TestHumanBytesAndDisk(t *testing.T) {
	if got := humanBytes(500 * 1000 * 1000 * 1000); got != "500 GB" {
		t.Errorf("humanBytes = %q", got)
	}
	if v := diskView(registryDisk(1000, 100)); v == nil || v.Pct != 90 || v.Class != "danger" || v.Word == "" {
		t.Errorf("90%% disk = %+v, want danger with a word", v)
	}
	if v := diskView(registryDisk(1000, 200)); v == nil || v.Pct != 80 || v.Class != "warn" {
		t.Errorf("80%% disk = %+v, want warn", v)
	}
	if v := diskView(registryDisk(1000, 600)); v == nil || v.Class != "" {
		t.Errorf("40%% disk = %+v, want plain", v)
	}
	if diskView(registryDisk(0, 0)) != nil {
		t.Error("no figures must give no meter")
	}
}
