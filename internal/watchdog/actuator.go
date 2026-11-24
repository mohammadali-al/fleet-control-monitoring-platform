package watchdog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Runner runs an external command. SystemActuator goes through it for every
// systemctl and tc call, so the command lines can be tested without root.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs real commands.
type ExecRunner struct{}

// Run executes the command and returns its combined output.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// cpuPeriodUsec is the cgroup v2 cpu.max period used when writing a quota.
const cpuPeriodUsec = 100000

// SystemActuator is the real Actuator: systemctl for services and reboot,
// cgroup v2 files for sessions, tc tbf for egress. All of it needs root; the
// decision logic never touches it directly.
type SystemActuator struct {
	// Runner runs systemctl and tc (default ExecRunner).
	Runner Runner
	// SessionRoot is the directory under which session cgroups live; a session
	// target outside it is refused.
	SessionRoot string
	// Iface is the interface shaped for egress throttling. Empty means the
	// default route's interface at the time of the call.
	Iface string
	// NCPU converts "percent of the machine" to systemd's per-CPU CPUQuota
	// (default runtime.NumCPU).
	NCPU int
}

func (a *SystemActuator) runner() Runner {
	if a.Runner != nil {
		return a.Runner
	}
	return ExecRunner{}
}

func (a *SystemActuator) ncpu() int {
	if a.NCPU > 0 {
		return a.NCPU
	}
	return runtime.NumCPU()
}

func (a *SystemActuator) iface() string {
	if a.Iface != "" {
		return a.Iface
	}
	return DefaultRouteIface()
}

// sessionDir validates a session target's cgroup path.
func (a *SystemActuator) sessionDir(t Target) (string, error) {
	if t.Kind != TargetSession || t.CgroupPath == "" {
		return "", ErrNotApplicable
	}
	if a.SessionRoot == "" {
		return "", fmt.Errorf("session cgroup root is not configured: %w", ErrNotApplicable)
	}
	rel, err := filepath.Rel(a.SessionRoot, t.CgroupPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("session cgroup %q is outside %q", t.CgroupPath, a.SessionRoot)
	}
	return t.CgroupPath, nil
}

func (a *SystemActuator) quotaPercentPerCPU(spec ThrottleSpec) int {
	q := int(spec.CPUPercent*float64(a.ncpu()) + 0.5)
	if q < 1 {
		q = 1
	}
	return q
}

// Throttle applies a CPU quota to a service (systemctl set-property --runtime,
// a transient drop-in) or a session (cgroup cpu.max), or shapes egress with tc
// tbf for a machine target.
func (a *SystemActuator) Throttle(ctx context.Context, t Target, spec ThrottleSpec) error {
	switch t.Kind {
	case TargetService:
		if !validUnit(t.Name) {
			return fmt.Errorf("invalid unit name %q", t.Name)
		}
		_, err := a.runner().Run(ctx, "systemctl", "set-property", "--runtime", t.Name,
			"CPUQuota="+strconv.Itoa(a.quotaPercentPerCPU(spec))+"%")
		return err
	case TargetSession:
		dir, err := a.sessionDir(t)
		if err != nil {
			return err
		}
		quota := int64(a.quotaPercentPerCPU(spec)) * cpuPeriodUsec / 100
		return os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(fmt.Sprintf("%d %d", quota, cpuPeriodUsec)), 0o644)
	case TargetMachine:
		if spec.EgressKbit <= 0 {
			return ErrNotApplicable
		}
		dev := a.iface()
		if dev == "" {
			return fmt.Errorf("no egress interface: %w", ErrNotApplicable)
		}
		_, err := a.runner().Run(ctx, "tc", "qdisc", "replace", "dev", dev, "root", "tbf",
			"rate", strconv.Itoa(spec.EgressKbit)+"kbit", "burst", "32kbit", "latency", "400ms")
		return err
	}
	return ErrNotApplicable
}

// Unthrottle lifts what Throttle applied.
func (a *SystemActuator) Unthrottle(ctx context.Context, t Target) error {
	switch t.Kind {
	case TargetService:
		if !validUnit(t.Name) {
			return fmt.Errorf("invalid unit name %q", t.Name)
		}
		_, err := a.runner().Run(ctx, "systemctl", "set-property", "--runtime", t.Name, "CPUQuota=")
		return err
	case TargetSession:
		dir, err := a.sessionDir(t)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(fmt.Sprintf("max %d", cpuPeriodUsec)), 0o644); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err // a vanished session needs no unthrottle
		}
		return nil
	case TargetMachine:
		dev := a.iface()
		if dev == "" {
			return nil
		}
		_, err := a.runner().Run(ctx, "tc", "qdisc", "del", "dev", dev, "root")
		return err
	}
	return nil
}

// KillSession kills a session's whole cgroup in one action: cgroup.kill (kernel
// 5.14+), else SIGKILL to every pid in cgroup.procs. Never per-socket.
func (a *SystemActuator) KillSession(_ context.Context, t Target) error {
	dir, err := a.sessionDir(t)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644); err == nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("kill session %s: %w", t.Name, err)
	}
	var firstErr error
	for _, f := range strings.Fields(string(b)) {
		pid, perr := strconv.Atoi(f)
		if perr != nil || pid <= 1 {
			continue
		}
		if kerr := syscall.Kill(pid, syscall.SIGKILL); kerr != nil && !errors.Is(kerr, syscall.ESRCH) && firstErr == nil {
			firstErr = kerr
		}
	}
	return firstErr
}

// RestartService runs systemctl restart <unit>.
func (a *SystemActuator) RestartService(ctx context.Context, unit string) error {
	if !validUnit(unit) {
		return fmt.Errorf("invalid unit name %q", unit)
	}
	_, err := a.runner().Run(ctx, "systemctl", "restart", unit)
	return err
}

// Reboot runs systemctl reboot.
func (a *SystemActuator) Reboot(ctx context.Context) error {
	_, err := a.runner().Run(ctx, "systemctl", "reboot")
	return err
}
