package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// The Machines SPA route serves the React shell and the compiled bundle
// carrying the Machines registry, live /ws/events wiring, token-enrollment UI,
// SSH onboarding, and public-IP refresh actions.
func TestMachinesPageHasTokenEnrollmentUI(t *testing.T) {
	srv := noBackend(t)
	status, _, page := body(t, srv.URL+"/machines")
	if status != 200 {
		t.Fatalf("GET /machines = %d", status)
	}
	for _, want := range []string{`<div id="root"></div>`, `/assets/dist/app.css`, `/assets/dist/app.js`} {
		if !strings.Contains(page, want) {
			t.Errorf("machines SPA shell lacks %q", want)
		}
	}

	status, _, js := body(t, srv.URL+"/assets/dist/app.js")
	if status != 200 || len(js) == 0 {
		t.Fatalf("GET /assets/dist/app.js = %d (len=%d)", status, len(js))
	}
	for _, want := range []string{
		"Add agent (token)",
		"Onboard machine",
		"Refresh public IPs",
		"/refresh-public-ip",
		"/api/machines",
		"/api/enroll/tokens",
		"/api/enroll/machines",
		"/ws/events",
		"macOS machines can only be monitor agents",
		"All reported disks OK",
		"Disk figures update live from agent telemetry.",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("SPA bundle lacks %q", want)
		}
	}
}

func TestMachinesPageHasDetailsDrawerInBundle(t *testing.T) {
	srv := noBackend(t)
	status, _, js := body(t, srv.URL+"/assets/dist/app.js")
	if status != 200 || len(js) == 0 {
		t.Fatalf("GET /assets/dist/app.js = %d (len=%d)", status, len(js))
	}
	if strings.Contains(js, "The machine details view comes later.") {
		t.Error("SPA bundle still contains the stale Details stub toast")
	}
	for _, want := range []string{
		"Machine details",
		"Identity & connection",
		"Watchdog stage",
		"Connect mode",
		"Last seen",
		"Private IPs",
		"Public IP",
		"Public IP checked",
		"Enrollment & certificates",
		"Cert fingerprint",
		"/api/machines/",
		"data-machine-details",
		"Close machine details",
		"Live updates via /ws/events",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("SPA bundle lacks details drawer string %q", want)
		}
	}
}

func TestGetMachineDetailsEndpoint(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := e.reg.Update("w1", func(m *registry.Machine) {
		m.ConnectMode = registry.ConnectModeAgentDial
		m.PublicIP = "203.0.113.10"
		m.PublicIPAt = &now
		m.CertFingerprint = "abcdef0123456789"
		m.SSHHostKey = "SHA256:testhostkey"
		m.EnrolledVia = registry.EnrolledToken
		m.Disk = registry.DiskState{
			Status:     "warn",
			TotalBytes: 100 * 1000 * 1000 * 1000,
			FreeBytes:  15 * 1000 * 1000 * 1000,
			UpdatedAt:  &now,
		}
		m.Specs = &registry.Specs{
			Hostname:  "worker-01",
			OS:        "linux",
			OSVersion: "Ubuntu 24.04",
			Arch:      "amd64",
			CPUs:      8,
			MemBytes:  16 * 1000 * 1000 * 1000,
		}
	}); err != nil {
		t.Fatalf("reg.Update: %v", err)
	}

	st, _, rawJSON := body(t, e.srv.URL+"/api/machines/w1")
	if st != http.StatusOK {
		t.Fatalf("GET /api/machines/w1 = %d, want 200 (%s)", st, rawJSON)
	}
	var got machineResponse
	if err := json.Unmarshal([]byte(rawJSON), &got); err != nil {
		t.Fatalf("decode machineResponse: %v (%s)", err, rawJSON)
	}

	if got.Machine.ID != "w1" || got.Machine.Role != registry.RoleWorker || got.Machine.State != "online" {
		t.Errorf("machine identity/state = %+v", got.Machine)
	}
	if got.Machine.ConnectMode != registry.ConnectModeAgentDial {
		t.Errorf("connect_mode = %q, want %q", got.Machine.ConnectMode, registry.ConnectModeAgentDial)
	}
	if got.Machine.PublicIP != "203.0.113.10" || got.Machine.PublicIPAt == nil {
		t.Errorf("public IP = %q at %v", got.Machine.PublicIP, got.Machine.PublicIPAt)
	}
	if got.Machine.Disk.Status != "warn" || got.Machine.Disk.TotalBytes == 0 || got.Machine.Disk.UpdatedAt == nil {
		t.Errorf("disk = %+v", got.Machine.Disk)
	}
	if got.Machine.CertFingerprint != "abcdef0123456789" || got.Machine.SSHHostKey != "SHA256:testhostkey" {
		t.Errorf("fingerprints = cert:%q ssh:%q", got.Machine.CertFingerprint, got.Machine.SSHHostKey)
	}
	if got.Machine.EnrolledVia != registry.EnrolledToken || !got.Machine.Provisioned {
		t.Errorf("enrollment = via:%q provisioned:%v", got.Machine.EnrolledVia, got.Machine.Provisioned)
	}
	if got.Machine.Specs == nil || got.Machine.Specs.Hostname != "worker-01" {
		t.Errorf("specs = %+v", got.Machine.Specs)
	}

	// No secrets / private keys in payload.
	lower := strings.ToLower(rawJSON)
	for _, secret := range []string{"private_key", "private key", "-----begin", "password"} {
		if strings.Contains(lower, secret) {
			t.Errorf("GET /api/machines/w1 leaked %q in JSON: %s", secret, rawJSON)
		}
	}

	// Unknown machine -> 404.
	if st := e.call(t, "GET", "/api/machines/does-not-exist", nil, nil); st != http.StatusNotFound {
		t.Errorf("GET /api/machines/does-not-exist = %d, want 404", st)
	}

	// Without backend -> 503.
	nb := noBackend(t)
	if st, _, _ := body(t, nb.URL+"/api/machines/w1"); st != http.StatusServiceUnavailable {
		t.Errorf("no-backend GET /api/machines/w1 = %d, want 503", st)
	}
}

func TestGetMachineDetailsWatchdog(t *testing.T) {
	e := newObsEnv(t, nil)
	var worker machineResponse
	if st, _, raw := body(t, e.srv.URL+"/api/machines/w1"); st != http.StatusOK {
		t.Fatalf("GET /api/machines/w1 = %d (%s)", st, raw)
	} else if err := json.Unmarshal([]byte(raw), &worker); err != nil {
		t.Fatalf("decode worker: %v", err)
	}
	if worker.Watchdog == nil {
		t.Fatal("expected watchdog state for worker w1, got nil")
	}
	if worker.Watchdog.Stage != "none" || worker.Watchdog.Condition != "normal" {
		t.Errorf("worker watchdog = %+v, want stage=none condition=normal", worker.Watchdog)
	}

	var mon machineResponse
	if st, _, raw := body(t, e.srv.URL+"/api/machines/mon1"); st != http.StatusOK {
		t.Fatalf("GET /api/machines/mon1 = %d (%s)", st, raw)
	} else if err := json.Unmarshal([]byte(raw), &mon); err != nil {
		t.Fatalf("decode monitor: %v", err)
	}
	if mon.Watchdog != nil {
		t.Errorf("expected nil watchdog state for monitor mon1, got %+v", mon.Watchdog)
	}
}
