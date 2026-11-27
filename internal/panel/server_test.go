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
		{"/", 200, "text/html", "<html"},
		{"/index.html", 200, "text/html", "<html"},
		{"/machines.html", 200, "text/html", "<html"},
		{"/styleguide.html", 200, "text/html", "<html"},
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
		{"/nope", 404, "", ""},
		{"/nope.html", 404, "", ""},
		{"/fragments/nope", 404, "", ""},
		{"/assets/", 404, "", ""},         // no directory listing
		{"/assets", 404, "", ""},          // ditto
		{"/assets/css/", 404, "", ""},     // ditto
		{"/_src/shell.html", 404, "", ""}, // source templates are not served
		{"/pages/machines.html", 404, "", ""},
		{"/embed.go", 404, "", ""}, // Go sources are not served
		{"/templates.go", 404, "", ""},
		{"/../go.mod", 404, "", ""},
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
}

func TestRootIsMachines(t *testing.T) {
	srv := noBackend(t)
	_, _, root := body(t, srv.URL+"/")
	_, _, machines := body(t, srv.URL+"/machines.html")
	if root != machines {
		t.Error("GET / does not render the Machines page")
	}
}

func TestMethods(t *testing.T) {
	srv := noBackend(t)
	resp, err := http.Head(srv.URL + "/machines.html")
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("HEAD = %v %v, want 200", resp, err)
	}
	resp, err = http.Post(srv.URL+"/machines.html", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST page = %d, want 405", resp.StatusCode)
	}
}

// Every page renders from the Go templates: it carries the shell (sprite, nav,
// theme toggle), marks exactly its own nav item current, leaves no template
// action behind, and shows the Mock chip only while it is still mock data.
func TestAllPagesRender(t *testing.T) {
	srv := noBackend(t)
	navRe := regexp.MustCompile(`<a class="nav__link" href="([a-z]+)\.html" aria-current="page"`)
	for _, p := range pages {
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
		for _, must := range []string{`data-theme="dark"`, `data-bg="matrix"`, `localStorage.getItem('fleet-theme')`, `id="bg-canvas"`, `assets/css/fonts.css`, `assets/css/bg.css`, `assets/js/bg.js`, `<symbol id="i-fleet"`, `class="sidebar"`, `class="topbar"`, `theme-toggle`, `id="main"`, `assets/js/app.js`, `data-page="` + p.name + `"`, `<h1>`} {
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
	for _, name := range []string{"machines", "commands", "terminal"} {
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
	// Pages still render, saying so.
	_, _, html := body(t, srv.URL+"/machines.html")
	if !strings.Contains(html, "without a backend") {
		t.Error("machines page does not say there is no backend")
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
