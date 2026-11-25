package enroll

import (
	"bytes"
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

func runScriptForPlatform(t *testing.T, e *env, base, code, root, osName, unameArch string) (string, error) {
	t.Helper()
	script, err := exec.Command("curl", "-fsSk", "--pinnedpubkey", "sha256//"+e.svc.Pin(), base+"/enroll/e/"+code).Output()
	if err != nil {
		return "fetch failed: " + err.Error(), err
	}
	s := string(script)
	for _, p := range []string{"/usr/local", "/etc/fleet-agent", "/etc/systemd", "/var/lib/fleet-agent", "/Library/LaunchDaemons"} {
		s = strings.ReplaceAll(s, p, root+p)
	}
	stubs := filepath.Join(root, "stubs")
	if err := os.MkdirAll(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(root, "calls.log")
	stub("id", `echo 0`)
	stub("systemctl", `echo "systemctl $*" >> `+calls)
	stub("launchctl", `echo "launchctl $*" >> `+calls)
	if osName == OSDarwin {
		stub("uname", `case "$1" in -s) echo Darwin;; -m) echo `+unameArch+`;; -r) echo 23.5.0;; esac`)
		stub("sw_vers", `echo 14.5`)
		stub("sysctl", `case "$2" in hw.ncpu) echo 10;; hw.memsize) echo 17179869184;; esac`)
	} else {
		stub("uname", `case "$1" in -s) echo Linux;; -m) echo `+unameArch+`;; -r) echo 6.8.0;; esac`)
	}
	cmd := exec.Command("bash")
	cmd.Stdin = strings.NewReader(s)
	cmd.Env = append(os.Environ(), "PATH="+stubs+":"+os.Getenv("PATH"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	return out.String(), err
}

func TestInstallerSelectsBinaryByPlatformAndErrorsClearlyWhenMissing(t *testing.T) {
	need(t, "curl", "openssl", "bash", "install", "awk")

	dir := t.TempDir()
	for _, p := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		if err := os.WriteFile(filepath.Join(dir, "fleet-agent-"+p), []byte("BIN-"+p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fallback := filepath.Join(t.TempDir(), "fleet-agent-host")
	if err := os.WriteFile(fallback, []byte("BIN-host-fallback"), 0o755); err != nil {
		t.Fatal(err)
	}

	e := newEnv(t)
	// Prefer agent_binary_dir over agent_binary_path for all four targets.
	e.svc.cfg.Binary = BinaryDir(dir, fallback, OSLinux, "amd64")
	base, _ := startEndpoint(t, e)

	for _, tc := range []struct {
		osName, unameArch, wantArch, role, wantBin string
	}{
		{OSLinux, "x86_64", "amd64", config.RoleWorker, "BIN-linux-amd64"},
		{OSLinux, "aarch64", "arm64", config.RoleWorker, "BIN-linux-arm64"},
		{OSDarwin, "x86_64", "amd64", config.RoleMonitor, "BIN-darwin-amd64"},
		{OSDarwin, "arm64", "arm64", config.RoleMonitor, "BIN-darwin-arm64"},
	} {
		t.Run(tc.osName+"_"+tc.wantArch, func(t *testing.T) {
			if tc.osName == OSLinux {
				if _, err := os.Stat("/proc/meminfo"); err != nil {
					t.Skip("Linux only")
				}
			}
			m, err := e.svc.Mint(tc.role, "alice")
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			out, err := runScriptForPlatform(t, e, base, m.Token.Code, root, tc.osName, tc.unameArch)
			if err != nil {
				t.Fatalf("script failed: %v\n%s", err, out)
			}
			got, err := os.ReadFile(filepath.Join(root, BinaryPath))
			if err != nil {
				t.Fatalf("installed binary missing: %v", err)
			}
			if string(got) != tc.wantBin {
				t.Errorf("installed binary = %q, want %q", got, tc.wantBin)
			}
		})
	}

	// Missing binary: host only has linux/amd64 fallback and dir lacks linux/arm64 and darwin/amd64.
	partialDir := t.TempDir()
	e2 := newEnv(t)
	e2.svc.cfg.Binary = BinaryDir(partialDir, fallback, OSLinux, "amd64")
	base2, _ := startEndpoint(t, e2)

	for _, tc := range []struct {
		osName, unameArch, wantArch, role string
	}{
		{OSLinux, "aarch64", "arm64", config.RoleWorker},
		{OSDarwin, "x86_64", "amd64", config.RoleMonitor},
	} {
		t.Run("missing_"+tc.osName+"_"+tc.wantArch, func(t *testing.T) {
			m, _ := e2.svc.Mint(tc.role, "alice")
			root := t.TempDir()
			out, err := runScriptForPlatform(t, e2, base2, m.Token.Code, root, tc.osName, tc.unameArch)
			if err == nil {
				t.Fatalf("expected failure for missing %s/%s binary, got success:\n%s", tc.osName, tc.wantArch, out)
			}
			if !strings.Contains(out, "404") || !strings.Contains(out, tc.osName+"/"+tc.wantArch) {
				t.Errorf("error message not clear enough:\n%s", out)
			}
			if _, err := os.Stat(filepath.Join(root, "etc")); err == nil {
				t.Error("files were installed despite missing agent binary")
			}
			if _, err := os.Stat(filepath.Join(root, BinaryPath)); err == nil {
				t.Error("binary path created despite missing agent binary")
			}
			if len(e2.reg.List()) != 0 {
				t.Errorf("machine registered despite missing agent binary: %+v", e2.reg.List())
			}
		})
	}
}
