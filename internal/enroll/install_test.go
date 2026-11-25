package enroll

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

func TestWatchdogRenderedUnits(t *testing.T) {
	worker := RenderWatchdogSystemdUnit(config.RoleWorker)
	for _, want := range []string{
		"Description=Fleet watchdog (load-safety net)\n",
		"User=root\n",
		"Group=root\n",
		"Environment=FLEET_DATA_DIR=" + StateDir + "\n",
		"ExecStart=" + BinaryPath + " watchdog --config " + ConfigDir + "/config.json\n",
		"WorkingDirectory=" + StateDir + "\n",
		"Restart=always\n",
		"OOMScoreAdjust=-900\n",
		"Nice=-5\n",
		"WantedBy=multi-user.target\n",
	} {
		if !strings.Contains(worker, want) {
			t.Errorf("worker watchdog unit missing %q:\n%s", want, worker)
		}
	}
	if strings.Contains(worker, "FLEET_WATCHDOG_MODE=headless") {
		t.Errorf("worker watchdog unit must not be headless:\n%s", worker)
	}

	monitor := RenderWatchdogSystemdUnit(config.RoleMonitor)
	for _, want := range []string{
		"Description=Fleet watchdog (headless)\n",
		"Environment=FLEET_DATA_DIR=" + StateDir + "\n",
		"Environment=FLEET_WATCHDOG_MODE=headless\n",
		"ExecStart=" + BinaryPath + " watchdog --config " + ConfigDir + "/config.json\n",
		"Restart=always\n",
		"WantedBy=multi-user.target\n",
	} {
		if !strings.Contains(monitor, want) {
			t.Errorf("monitor watchdog unit missing %q:\n%s", want, monitor)
		}
	}

	pf, err := watchdog.ParseProfile([]byte(MonitorWatchdogPolicyYAML))
	if err != nil {
		t.Fatalf("MonitorWatchdogPolicyYAML invalid: %v", err)
	}
	pol, err := pf.Effective("mon-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pol.Steps.Throttle || pol.Steps.KillSession || !pol.Steps.Restart || !pol.Steps.Reboot ||
		pol.LoadController.CPUPercent != 0 || pol.LoadController.MemoryPercent != 0 {
		t.Errorf("monitor stripped policy = %+v", pol)
	}

	for deployPath, got := range map[string]string{
		"../../deploy/systemd/fleet-agent-liveness.service":    RenderAgentLivenessService(),
		"../../deploy/systemd/fleet-agent-liveness.timer":      RenderAgentLivenessTimer(),
		"../../deploy/systemd/fleet-watchdog-liveness.service": RenderWatchdogLivenessService(),
		"../../deploy/systemd/fleet-watchdog-liveness.timer":   RenderWatchdogLivenessTimer(),
	} {
		raw, err := os.ReadFile(deployPath)
		if err != nil {
			t.Fatal(err)
		}
		want := string(raw)
		if i := strings.Index(want, "[Unit]"); i >= 0 {
			want = want[i:]
		}
		if strings.TrimSpace(got) != strings.TrimSpace(want) {
			t.Errorf("%s differs from rendered:\n got: %s\nwant: %s", deployPath, got, want)
		}
	}
}

func TestRenderScriptWatchdogAndLiveness(t *testing.T) {
	for _, role := range []string{config.RoleWorker, config.RoleMonitor} {
		s := RenderScript(ScriptParams{
			BaseURL:    "https://fleet.example:7445",
			Pin:        "pin",
			CASHA256:   "00",
			Token:      "tok",
			Role:       role,
			ServerAddr: "fleet.example:7444",
			ServerID:   "fleet-server",
		})
		if bash, err := exec.LookPath("bash"); err == nil {
			cmd := exec.Command(bash, "-n")
			cmd.Stdin = strings.NewReader(s)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s script syntax error: %v\n%s", role, err, out)
			}
		}
		for _, want := range []string{
			WatchdogSystemdUnitPath,
			AgentLivenessServicePath,
			AgentLivenessTimerPath,
			WatchdogLivenessServicePath,
			WatchdogLivenessTimerPath,
			"systemctl enable " + WatchdogSystemdUnit,
			"systemctl restart " + WatchdogSystemdUnit,
			"systemctl enable --now " + AgentLivenessTimer,
			"systemctl enable --now " + WatchdogLivenessTimer,
			RenderWatchdogSystemdUnit(role),
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s script missing %q", role, want)
			}
		}
		if strings.Contains(s, "com.fleet.watchdog") {
			t.Errorf("%s script unexpectedly references a macOS watchdog plist", role)
		}
	}
}

func TestScriptInstallsWatchdogOnLinuxWorkerAndMonitor(t *testing.T) {
	need(t, "curl", "openssl", "bash", "install", "awk")
	if _, err := os.Stat("/proc/meminfo"); err != nil {
		t.Skip("Linux only")
	}

	t.Run("worker_full", func(t *testing.T) {
		e := newEnv(t)
		base, _ := startEndpoint(t, e)
		m, _ := e.svc.Mint(config.RoleWorker, "alice")
		root := t.TempDir()

		out, err := runScript(t, e, base, m.Token.Code, root, OSLinux)
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		wdUnit, err := os.ReadFile(filepath.Join(root, WatchdogSystemdUnitPath))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(wdUnit), "Description=Fleet watchdog (load-safety net)") ||
			strings.Contains(string(wdUnit), "FLEET_WATCHDOG_MODE=headless") {
			t.Errorf("worker watchdog unit = %s", wdUnit)
		}
		if _, err := os.Stat(filepath.Join(root, WatchdogPolicyPath)); err == nil {
			t.Error("worker must not receive monitor stripped policy.yaml")
		}
		for _, p := range []string{AgentLivenessServicePath, AgentLivenessTimerPath, WatchdogLivenessServicePath, WatchdogLivenessTimerPath} {
			if _, err := os.Stat(filepath.Join(root, p)); err != nil {
				t.Errorf("missing %s: %v", p, err)
			}
		}
		logBytes, _ := os.ReadFile(filepath.Join(root, "calls.log"))
		log := string(logBytes)
		for _, want := range []string{
			"systemctl enable " + WatchdogSystemdUnit,
			"systemctl restart " + WatchdogSystemdUnit,
			"systemctl enable --now " + AgentLivenessTimer,
			"systemctl enable --now " + WatchdogLivenessTimer,
		} {
			if !strings.Contains(log, want) {
				t.Errorf("missing %q in calls.log:\n%s", want, log)
			}
		}
	})

	t.Run("monitor_headless_stripped", func(t *testing.T) {
		e := newEnv(t)
		base, _ := startEndpoint(t, e)
		m, _ := e.svc.Mint(config.RoleMonitor, "alice")
		root := t.TempDir()

		out, err := runScript(t, e, base, m.Token.Code, root, OSLinux)
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		wdUnit, err := os.ReadFile(filepath.Join(root, WatchdogSystemdUnitPath))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(wdUnit), "Description=Fleet watchdog (headless)") ||
			!strings.Contains(string(wdUnit), "Environment=FLEET_WATCHDOG_MODE=headless") {
			t.Errorf("monitor watchdog unit = %s", wdUnit)
		}
		polPath := filepath.Join(root, WatchdogPolicyPath)
		st, err := os.Stat(polPath)
		if err != nil {
			t.Fatalf("monitor policy.yaml missing: %v", err)
		}
		if st.Mode().Perm() != 0o640 {
			t.Errorf("monitor policy.yaml mode = %o, want 0640", st.Mode().Perm())
		}
		raw, _ := os.ReadFile(polPath)
		pf, err := watchdog.ParseProfile(raw)
		if err != nil {
			t.Fatalf("monitor policy.yaml invalid: %v", err)
		}
		if pol, err := pf.Effective("mon", nil); err != nil || pol.Steps.Throttle || pol.Steps.KillSession {
			t.Errorf("monitor policy = %+v (%v)", pol, err)
		}
		logBytes, _ := os.ReadFile(filepath.Join(root, "calls.log"))
		log := string(logBytes)
		for _, want := range []string{
			"systemctl enable " + WatchdogSystemdUnit,
			"systemctl restart " + WatchdogSystemdUnit,
			"systemctl enable --now " + AgentLivenessTimer,
			"systemctl enable --now " + WatchdogLivenessTimer,
		} {
			if !strings.Contains(log, want) {
				t.Errorf("missing %q in calls.log:\n%s", want, log)
			}
		}
	})
}
