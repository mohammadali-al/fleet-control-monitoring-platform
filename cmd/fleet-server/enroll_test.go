package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/enroll"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/panel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

type fakeHost struct {
	reg       *registry.Registry
	ca        *pki.CA
	reconcile int
	submitted []fleet.SubmitRequest
	submitErr error
}

func (f *fakeHost) Registry() *registry.Registry { return f.reg }
func (f *fakeHost) CA() *pki.CA                  { return f.ca }
func (f *fakeHost) Reconcile()                   { f.reconcile++ }
func (f *fakeHost) Submit(r fleet.SubmitRequest) (string, error) {
	f.submitted = append(f.submitted, r)
	return "s1", f.submitErr
}

func newHost(t *testing.T) *fakeHost {
	t.Helper()
	ca, err := pki.NewCA(pki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &fakeHost{reg: registry.New(), ca: ca}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func serverCfg() config.ServerConfig {
	return config.ServerConfig{EnrollAddr: ":7445", EnrollHost: "fleet.example.com", AgentDialAddr: ":7444"}
}

func TestNewEnrollNeedsItsSettings(t *testing.T) {
	h := newHost(t)
	cfg := serverCfg()
	if _, err := newEnroll(cfg, t.TempDir(), h, quiet()); err != nil {
		t.Fatalf("complete config: %v", err)
	}
	for name, mod := range map[string]func(*config.ServerConfig){
		"no host":          func(c *config.ServerConfig) { c.EnrollHost = "" },
		"no agent_dial":    func(c *config.ServerConfig) { c.AgentDialAddr = "" },
		"bad enroll addr":  func(c *config.ServerConfig) { c.EnrollAddr = "nope" },
		"bad agent_dial":   func(c *config.ServerConfig) { c.AgentDialAddr = ":0" },
		"enroll port text": func(c *config.ServerConfig) { c.EnrollAddr = ":http" },
	} {
		c := serverCfg()
		mod(&c)
		if _, err := newEnroll(c, t.TempDir(), h, quiet()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := newEnroll(cfg, t.TempDir(), &fakeHost{reg: registry.New()}, quiet()); err == nil {
		t.Error("accepted a manager with no CA")
	}
}

func TestEnableEnrollOffByDefaultAndWhenUnsupported(t *testing.T) {
	panelH := http.NotFoundHandler()
	got, err := enableEnroll(context.Background(), config.ServerConfig{}, t.TempDir(), newHost(t), panelH, quiet())
	if err != nil || got == nil {
		t.Fatalf("off: %v", err)
	}
	// A manager that cannot host enrollment is an error when it is asked for.
	if _, err := enableEnroll(context.Background(), serverCfg(), t.TempDir(), struct{}{}, panelH, quiet()); err == nil {
		t.Error("enroll_addr with a manager that cannot host it was accepted")
	}
}

// The operator API is mounted beside the panel, which keeps serving every page.
func TestPanelStillServesAllPagesWithEnrollmentMounted(t *testing.T) {
	h := newHost(t)
	svc, err := newEnroll(serverCfg(), t.TempDir(), h, quiet())
	if err != nil {
		t.Fatal(err)
	}
	p, err := panel.New(panel.Options{Assets: web.FS, Templates: web.Templates})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(withEnroll(p, svc))
	t.Cleanup(srv.Close)

	for _, path := range []string{"/", "/machines.html", "/projects.html", "/commands.html", "/terminal.html", "/baseline.html",
		"/maneuver.html", "/monitor.html", "/watchdog.html", "/evidence.html", "/backup.html", "/assets/js/live.js"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
	resp, err := http.Get(srv.URL + "/api/enroll/machines")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"machines":[]`) {
		t.Errorf("GET /api/enroll/machines = %d %s", resp.StatusCode, b)
	}
	// The panel's own API is untouched (no backend: 503).
	if resp, err := http.Get(srv.URL + "/api/machines"); err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /api/machines = %v %v", resp, err)
	}
}

func TestFleetRunnerSendsStopThroughTheQueue(t *testing.T) {
	h := newHost(t)
	r := fleetRunner{h}
	if err := r.Run(context.Background(), "w1", enroll.StopCommand("linux")); err != nil {
		t.Fatal(err)
	}
	if len(h.submitted) != 1 || h.submitted[0].Machine != "w1" || !h.submitted[0].Confirm ||
		h.submitted[0].Command != "systemctl disable --now --no-block fleet-agent.service" {
		t.Errorf("submitted = %+v", h.submitted)
	}
	// A monitor agent has no command role: Stop reports it cannot be stopped remotely.
	h.submitErr = fleet.ErrNoShell
	if err := r.Run(context.Background(), "m1", "x"); !errors.Is(err, enroll.ErrStopUnsupported) {
		t.Errorf("monitor stop err = %v, want ErrStopUnsupported", err)
	}
	h.submitErr = fleet.ErrOffline
	if err := r.Run(context.Background(), "w1", "x"); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("offline stop err = %v", err)
	}
}
