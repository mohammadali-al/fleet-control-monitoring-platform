package watchdog

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct {
	cmds []string
	err  error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
	return nil, f.err
}

func TestSystemActuatorCommandLines(t *testing.T) {
	fr := &fakeRunner{}
	a := &SystemActuator{Runner: fr, Iface: "eth9", NCPU: 4}
	ctx := context.Background()

	if err := a.Throttle(ctx, Target{Kind: TargetService, Name: "web.service"}, ThrottleSpec{CPUPercent: 50}); err != nil {
		t.Fatal(err)
	}
	if err := a.Unthrottle(ctx, Target{Kind: TargetService, Name: "web.service"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Throttle(ctx, Target{Kind: TargetMachine}, ThrottleSpec{CPUPercent: 50, EgressKbit: 2000}); err != nil {
		t.Fatal(err)
	}
	if err := a.Unthrottle(ctx, Target{Kind: TargetMachine}); err != nil {
		t.Fatal(err)
	}
	if err := a.RestartService(ctx, "web.service"); err != nil {
		t.Fatal(err)
	}
	if err := a.Reboot(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl set-property --runtime web.service CPUQuota=200%", // 50% of 4 CPUs
		"systemctl set-property --runtime web.service CPUQuota=",
		"tc qdisc replace dev eth9 root tbf rate 2000kbit burst 32kbit latency 400ms",
		"tc qdisc del dev eth9 root",
		"systemctl restart web.service",
		"systemctl reboot",
	}
	if strings.Join(fr.cmds, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(fr.cmds, "\n"), strings.Join(want, "\n"))
	}
}

func TestSystemActuatorRefusesBadInput(t *testing.T) {
	fr := &fakeRunner{}
	a := &SystemActuator{Runner: fr, Iface: "eth9"}
	ctx := context.Background()
	if err := a.RestartService(ctx, "--now"); err == nil {
		t.Error("flag-like unit name must be refused")
	}
	if err := a.Throttle(ctx, Target{Kind: TargetService, Name: "a b"}, ThrottleSpec{CPUPercent: 50}); err == nil {
		t.Error("unit name with a space must be refused")
	}
	if err := a.Throttle(ctx, Target{Kind: TargetMachine}, ThrottleSpec{CPUPercent: 50}); !errors.Is(err, ErrNotApplicable) {
		t.Errorf("machine throttle without egress shaping: got %v", err)
	}
	if err := a.KillSession(ctx, Target{Kind: TargetService, Name: "x"}); !errors.Is(err, ErrNotApplicable) {
		t.Errorf("kill of a non-session: got %v", err)
	}
	if len(fr.cmds) != 0 {
		t.Errorf("refused calls must not run commands: %q", fr.cmds)
	}
	fr.err = errors.New("exit 1")
	if err := a.Reboot(ctx); err == nil {
		t.Error("runner error must propagate")
	}
}

// Session actions are plain cgroup v2 file writes, so a temp dir stands in for the
// cgroup tree and no root is needed.
func TestSystemActuatorSessionCgroupFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sess1")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &SystemActuator{SessionRoot: root, NCPU: 2}
	tgt := Target{Kind: TargetSession, Name: "sess1", CgroupPath: dir}
	ctx := context.Background()

	if err := a.Throttle(ctx, tgt, ThrottleSpec{CPUPercent: 25}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "cpu.max")); got != "50000 100000" { // 25% of 2 CPUs = half a CPU
		t.Errorf("cpu.max = %q", got)
	}
	if err := a.Unthrottle(ctx, tgt); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "cpu.max")); got != "max 100000" {
		t.Errorf("cpu.max after unthrottle = %q", got)
	}
	if err := a.KillSession(ctx, tgt); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "cgroup.kill")); got != "1" {
		t.Errorf("cgroup.kill = %q (the whole cgroup is killed in one write)", got)
	}

	// A path outside the session root is refused, and a missing root disables it.
	for _, bad := range []string{root, filepath.Join(root, ".."), t.TempDir(), filepath.Join(root, "..", "x")} {
		if err := a.KillSession(ctx, Target{Kind: TargetSession, CgroupPath: bad}); err == nil {
			t.Errorf("kill of %q outside the session root must be refused", bad)
		}
	}
	if err := (&SystemActuator{}).KillSession(ctx, tgt); !errors.Is(err, ErrNotApplicable) {
		t.Errorf("no session root: got %v", err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExecRunnerRunsAndReportsFailure(t *testing.T) {
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("no true(1)")
	}
	if _, err := (ExecRunner{}).Run(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := (ExecRunner{}).Run(context.Background(), "false"); err == nil {
		t.Fatal("a failing command must return an error")
	}
}

// The real systemctl path needs root (and systemd); it is skipped clearly when
// either is missing. As root it only runs a read-only systemctl query: no unit is
// throttled, restarted or rebooted by a test.
func TestSystemctlAvailableAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("SKIPPED (root-gated): real systemctl/tc actions need root; the command lines are covered by TestSystemActuatorCommandLines")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("SKIPPED (root-gated): systemctl not installed")
	}
	if _, err := (ExecRunner{}).Run(context.Background(), "systemctl", "--version"); err != nil {
		t.Fatal(err)
	}
}
