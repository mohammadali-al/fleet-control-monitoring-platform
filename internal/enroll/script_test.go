package enroll

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
)

// These tests run the real bootstrap script with the real curl and openssl
// against a real endpoint, with the system commands that need root (id,
// systemctl, launchctl) stubbed and the install paths moved into a temp dir.

func need(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s is not installed", n)
		}
	}
}

func TestCurlPinsTheEndpoint(t *testing.T) {
	need(t, "curl")
	e := newEnv(t)
	base, _ := startEndpoint(t, e)
	m, _ := e.svc.Mint("worker", "alice")
	u := base + "/enroll/e/" + m.Token.Code

	out, err := exec.Command("curl", "-fsSk", "--pinnedpubkey", "sha256//"+e.svc.Pin(), u).CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "#!/bin/bash") {
		t.Fatalf("curl with the right pin: %v\n%s", err, out)
	}
	out, err = exec.Command("curl", "-fsSk", "--pinnedpubkey", "sha256//AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", u).CombinedOutput()
	if err == nil {
		t.Fatalf("curl connected with the wrong pin:\n%s", out)
	}
}

// runScript fetches the script exactly as the one-liner does (pinned curl, by
// code), moves its install paths under root, and runs it as the given OS.
func runScript(t *testing.T, e *env, base, code, root, osName string) (string, error) {
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
		stub("uname", `case "$1" in -s) echo Darwin;; -m) echo arm64;; -r) echo 23.5.0;; esac`)
		stub("sw_vers", `echo 14.5`)
		stub("sysctl", `case "$2" in hw.ncpu) echo 10;; hw.memsize) echo 17179869184;; esac`)
	}
	cmd := exec.Command("bash")
	cmd.Stdin = strings.NewReader(s) // piped, like curl | bash
	cmd.Env = append(os.Environ(), "PATH="+stubs+":"+os.Getenv("PATH"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	return out.String(), err
}

func TestScriptEnrollsALinuxWorker(t *testing.T) {
	need(t, "curl", "openssl", "bash", "install", "awk")
	if _, err := os.Stat("/proc/meminfo"); err != nil {
		t.Skip("Linux only")
	}
	e := newEnv(t)
	base, sawBody := startEndpoint(t, e)
	m, _ := e.svc.Mint("worker", "alice")
	root := t.TempDir()

	out, err := runScript(t, e, base, m.Token.Code, root, OSLinux)
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	ms := e.reg.List()
	if len(ms) != 1 {
		t.Fatalf("registry has %d machines\n%s", len(ms), out)
	}
	id := ms[0].ID
	if ms[0].Role != "worker" || ms[0].Specs == nil || ms[0].Specs.OS != "linux" || ms[0].Specs.CPUs < 1 || ms[0].Specs.MemBytes == 0 || ms[0].Specs.Kernel == "" {
		t.Errorf("registry entry = %+v specs %+v", ms[0], ms[0].Specs)
	}

	cfgDir := filepath.Join(root, "etc/fleet-agent")
	for f, perm := range map[string]os.FileMode{"agent.key": 0o600, "agent.crt": 0o644, "ca.crt": 0o644, "config.json": 0o640} {
		st, err := os.Stat(filepath.Join(cfgDir, f))
		if err != nil {
			t.Errorf("%s: %v", f, err)
		} else if st.Mode().Perm() != perm {
			t.Errorf("%s mode = %o, want %o", f, st.Mode().Perm(), perm)
		}
	}
	// The key the machine generated is the one the certificate was issued for,
	// and the server never saw it.
	keyPEM, _ := os.ReadFile(filepath.Join(cfgDir, "agent.key"))
	certPEM, _ := os.ReadFile(filepath.Join(cfgDir, "agent.crt"))
	if !strings.Contains(string(keyPEM), "PRIVATE KEY") {
		t.Fatal("no private key written")
	}
	if strings.Contains(sawBody(), "PRIVATE KEY") {
		t.Error("the private key was sent to the server")
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Errorf("installed certificate and key do not match: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(e.ca.CertPEM())
	c := parse(t, certPEM)
	if _, err := c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("installed certificate does not verify: %v", err)
	}
	if c.Subject.CommonName != id {
		t.Errorf("certificate CN %q != machine %q", c.Subject.CommonName, id)
	}
	cfg, err := config.Load(filepath.Join(cfgDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ac := cfg.Agent; ac.Validate() != nil || ac.Role != "worker" || !ac.Dials() || ac.MachineID != id || ac.ServerAddr != "127.0.0.1:7444" {
		t.Errorf("installed agent config = %+v (%v)", ac, ac.Validate())
	}
	if b, _ := os.ReadFile(filepath.Join(root, BinaryPath)); string(b) != "#!fake-agent-binary" {
		t.Errorf("agent binary = %q", b)
	}
	unit, _ := os.ReadFile(filepath.Join(root, SystemdUnitPath))
	if !strings.Contains(string(unit), "Restart=always") || !strings.Contains(string(unit), "WantedBy=multi-user.target") {
		t.Errorf("unit = %s", unit)
	}
	wrap, err := os.ReadFile(filepath.Join(root, WrapperPath))
	if err != nil || !strings.Contains(string(wrap), StopCommand(OSLinux)) {
		t.Errorf("wrapper missing or without the stop command: %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(root, "calls.log"))
	for _, want := range []string{"systemctl daemon-reload", "systemctl enable fleet-agent.service", "systemctl restart fleet-agent.service"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("service not set up: missing %q in\n%s", want, log)
		}
	}
	if strings.Contains(string(log), "launchctl") {
		t.Error("launchctl used on Linux")
	}

	// The code is spent: running the same one-liner again is refused.
	if out, err := runScript(t, e, base, m.Token.Code, t.TempDir(), OSLinux); err == nil {
		t.Errorf("the one-liner worked a second time:\n%s", out)
	}
}

func TestScriptEnrollsAMacMonitorAndRefusesAMacWorker(t *testing.T) {
	need(t, "curl", "openssl", "bash", "install", "awk")
	e := newEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet-agent-darwin-arm64"), []byte("MACH-O"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.svc.cfg.Binary = BinaryDir(dir, "", "darwin", "arm64")
	base, _ := startEndpoint(t, e)

	// A worker: the script refuses on the macOS machine, before anything is
	// downloaded or installed, and burns nothing.
	w, _ := e.svc.Mint("worker", "alice")
	root := t.TempDir()
	out, err := runScript(t, e, base, w.Token.Code, root, OSDarwin)
	if err == nil || !strings.Contains(out, "monitor agents") {
		t.Fatalf("macOS worker was not refused: %v\n%s", err, out)
	}
	if len(e.reg.List()) != 0 {
		t.Error("a refused macOS worker was registered")
	}
	if _, err := os.Stat(filepath.Join(root, "etc")); err == nil {
		t.Error("the refused macOS worker installed files")
	}

	m, _ := e.svc.Mint("monitor", "alice")
	root = t.TempDir()
	out, err = runScript(t, e, base, m.Token.Code, root, OSDarwin)
	if err != nil {
		t.Fatalf("macOS monitor: %v\n%s", err, out)
	}
	ms := e.reg.List()
	if len(ms) != 1 || ms[0].Role != "monitor" || ms[0].Specs.OS != "darwin" || ms[0].Specs.Arch != "arm64" || ms[0].Specs.CPUs != 10 {
		t.Fatalf("registry = %+v", ms)
	}
	plist, _ := os.ReadFile(filepath.Join(root, LaunchdPlistPath))
	if !strings.Contains(string(plist), "<key>KeepAlive</key><true/>") {
		t.Errorf("plist = %s", plist)
	}
	log, _ := os.ReadFile(filepath.Join(root, "calls.log"))
	if !strings.Contains(string(log), "launchctl enable system/com.fleet.agent") || !strings.Contains(string(log), "launchctl bootstrap system") {
		t.Errorf("launchd not set up:\n%s", log)
	}
	if strings.Contains(string(log), "systemctl") {
		t.Error("systemctl used on macOS")
	}
	if _, err := os.Stat(filepath.Join(root, SystemdUnitPath)); err == nil {
		t.Error("a systemd unit was written on macOS")
	}
}
