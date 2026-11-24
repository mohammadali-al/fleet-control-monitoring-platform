package watchdog

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestParsers(t *testing.T) {
	tot, idle, err := parseProcStat("cpu  100 20 30 400 50 6 7 8 9 10\ncpu0 1 1 1 1 1 1 1 1 1 1\nintr 5\n")
	if err != nil || tot != 100+20+30+400+50+6+7+8 || idle != 450 {
		t.Errorf("proc stat: %d %d %v", tot, idle, err)
	}
	if _, _, err := parseProcStat("intr 5\n"); err == nil {
		t.Error("missing cpu line must fail")
	}
	total, avail, err := parseMeminfo("MemTotal:       16000000 kB\nMemFree: 1 kB\nMemAvailable:    4000000 kB\n")
	if err != nil || total != 16000000 || avail != 4000000 {
		t.Errorf("meminfo: %d %d %v", total, avail, err)
	}
	if _, _, err := parseMeminfo("MemTotal: 1 kB\n"); err == nil {
		t.Error("missing MemAvailable must fail")
	}
	if l, err := parseLoadavg("3.25 2.00 1.00 1/200 999\n"); err != nil || l != 3.25 {
		t.Errorf("loadavg: %v %v", l, err)
	}
	if u, err := parseCgroupCPUUsec("usage_usec 123456\nuser_usec 100\nsystem_usec 23456\n"); err != nil || u != 123456 {
		t.Errorf("cpu.stat: %v %v", u, err)
	}
	route := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\n" +
		"eth0\t00000000\t0102A8C0\t0003\t0\t0\t100\t00000000\n" +
		"eth0\t0002A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\n"
	if got := parseDefaultRouteIface(route); got != "eth0" {
		t.Errorf("default route iface = %q", got)
	}
	if got := parseDefaultRouteIface("Iface\tDestination\n"); got != "" {
		t.Errorf("no route: %q", got)
	}
}

// Sampling the live host needs no root: /proc, cgroup files and netlink are
// readable by anyone. Units without a cgroup are reported absent, not as errors.
func TestSystemSamplerLive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	if _, err := os.Stat("/proc/stat"); err != nil {
		t.Skip("no /proc")
	}
	s := NewSystemSampler("", t.TempDir())
	ctx := context.Background()
	first, err := s.Sample(ctx, []string{"definitely-not-a-unit.service"})
	if err != nil {
		t.Fatal(err)
	}
	if u := first.Services["definitely-not-a-unit.service"]; u.Present {
		t.Error("a missing unit must be reported absent")
	}
	time.Sleep(150 * time.Millisecond)
	second, err := s.Sample(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := second.Machine
	for name, v := range map[string]float64{"cpu": m.CPUPercent, "mem": m.MemoryPercent, "disk": m.DiskPercent} {
		if v < 0 || v > 100 {
			t.Errorf("%s percent out of range: %v", name, v)
		}
	}
	if m.MemoryPercent == 0 {
		t.Error("memory percent should be above zero on a live host")
	}
	if m.LoadPerCPU < 0 || m.EgressMbps < 0 {
		t.Errorf("negative reading: %+v", m)
	}
	if _, err := s.Sample(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSystemSamplerReadsCgroupFiles(t *testing.T) {
	root := t.TempDir()
	unit := filepath.Join(root, "system.slice", "web.service")
	sessions := filepath.Join(root, "fleet")
	sess := filepath.Join(sessions, "s1")
	for _, d := range []string{unit, sess} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	proc := t.TempDir()
	write(proc, "stat", "cpu 100 0 100 800 0 0 0 0\n")
	write(proc, "meminfo", "MemTotal: 1000 kB\nMemAvailable: 500 kB\n")
	write(proc, "loadavg", "1.0 1.0 1.0 1/1 1\n")

	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	s := &SystemSampler{CgroupRoot: root, SessionRoot: sessions, ProcRoot: proc, DiskPath: root, Now: func() time.Time { return now }}
	write(unit, "cpu.stat", "usage_usec 0\n")
	write(unit, "memory.current", "256000\n") // 25 % of 1000 kB
	write(sess, "cpu.stat", "usage_usec 0\n")
	write(sess, "memory.current", "512000\n")
	if _, err := s.Sample(context.Background(), []string{"web.service"}); err != nil {
		t.Fatal(err)
	}

	// One second later: the unit used 0.5 CPU-seconds, the session 1 CPU-second.
	now = now.Add(time.Second)
	write(proc, "stat", "cpu 200 0 200 1600 0 0 0 0\n")
	write(unit, "cpu.stat", "usage_usec 500000\n")
	write(sess, "cpu.stat", "usage_usec 1000000\n")
	out, err := s.Sample(context.Background(), []string{"web.service"})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Machine.CPUPercent; got < 19.9 || got > 20.1 { // 200 busy of 1000 new jiffies
		t.Errorf("machine cpu = %v, want 20", got)
	}
	if got := out.Machine.MemoryPercent; got != 50 {
		t.Errorf("machine mem = %v, want 50", got)
	}
	ncpu := float64(runtime.NumCPU())
	web := out.Services["web.service"]
	if !web.Present || web.MemoryPercent < 24.99 || web.MemoryPercent > 25.01 {
		t.Fatalf("unit not read (want present, 25%% memory): %+v", web)
	}
	if want := 100 * 0.5 / ncpu; web.CPUPercent < want-0.01 || web.CPUPercent > want+0.01 {
		t.Errorf("unit cpu = %v, want %v", web.CPUPercent, want)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].ID != "s1" || out.Sessions[0].CgroupPath != sess {
		t.Fatalf("sessions: %+v", out.Sessions)
	}
	if want := 100 * 1.0 / ncpu; out.Sessions[0].CPUPercent < want-0.01 || out.Sessions[0].CPUPercent > want+0.01 {
		t.Errorf("session cpu = %v, want %v", out.Sessions[0].CPUPercent, want)
	}
}
