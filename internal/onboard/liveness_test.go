package onboard

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// Onboarding installs the main agent's liveness timer and one-shot (T-0027),
// enables the timer, and the installed text is the deploy/ copy.
func TestProvisionInstallsAgentLiveness(t *testing.T) {
	f := newFixture(t, "root")
	if _, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker)); err != nil {
		t.Fatal(err)
	}
	l := DefaultLayout()

	svc, ok := f.fm.file("/etc/systemd/system/fleet-agent-liveness.service")
	if !ok || svc.mode != "0644" || svc.owner != "root" {
		t.Fatalf("liveness service not installed: %+v", svc)
	}
	tim, ok := f.fm.file("/etc/systemd/system/fleet-agent-liveness.timer")
	if !ok || !strings.Contains(string(tim.data), "OnUnitActiveSec=45s\n") || !strings.Contains(string(tim.data), "Unit=fleet-agent-liveness.service\n") {
		t.Fatalf("liveness timer: %q", tim.data)
	}
	if !strings.Contains(string(svc.data), "/var/lib/fleet-agent/watchdog/heartbeat-agent") || !strings.Contains(string(svc.data), "fleet-agent.service") {
		t.Fatalf("liveness service does not check the agent heartbeat: %q", svc.data)
	}

	var sawEnable bool
	for _, c := range f.fm.cmds {
		if strings.HasPrefix(c, "systemctl enable --now fleet-agent-liveness.timer") {
			sawEnable = true
		}
	}
	if !sawEnable {
		t.Fatalf("liveness timer not enabled: %v", f.fm.cmds)
	}

	// The shipped deploy/ files are the same text as what onboarding installs.
	for path, want := range map[string]string{
		"../../deploy/systemd/fleet-agent-liveness.service": renderLivenessService(l),
		"../../deploy/systemd/fleet-agent-liveness.timer":   renderLivenessTimer(l),
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(got)
		// The deploy copies carry an explanatory comment header the installed ones do not.
		if i := strings.Index(body, "[Unit]"); i >= 0 {
			body = body[i:]
		}
		if strings.TrimSpace(body) != strings.TrimSpace(want) {
			t.Errorf("%s differs from what onboarding installs:\n got: %s\nwant: %s", path, body, want)
		}
	}
}
