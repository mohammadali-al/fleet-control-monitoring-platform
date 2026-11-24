package watchdog

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MachineMetrics are the machine-level readings of one sample.
type MachineMetrics struct {
	CPUPercent    float64 // percent of all CPUs busy since the previous sample
	MemoryPercent float64 // percent of memory in use (total - available)
	LoadPerCPU    float64 // 1-minute load average divided by the CPU count
	DiskPercent   float64 // percent used of the watched filesystem
	EgressMbps    float64 // megabits per second sent since the previous sample
}

// UsageMetrics are one cgroup's readings, as percent of the whole machine.
type UsageMetrics struct {
	Present       bool // false when the unit has no cgroup (not running)
	CPUPercent    float64
	MemoryPercent float64
}

// SessionUsage is one command session's cgroup and its readings.
type SessionUsage struct {
	ID         string
	CgroupPath string
	UsageMetrics
}

// Sample is one reading of the whole host.
type Sample struct {
	Time     time.Time
	Machine  MachineMetrics
	Services map[string]UsageMetrics // keyed by unit name
	Sessions []SessionUsage
}

// Sampler produces a Sample. units are the systemd units the policy watches.
// The real implementation is SystemSampler; tests feed canned samples.
type Sampler interface {
	Sample(ctx context.Context, units []string) (Sample, error)
}

// --- pure parsers (shared, unit-tested on fixture text) ---

// parseProcStat returns total and idle jiffies from the aggregate "cpu" line of
// /proc/stat. idle includes iowait.
func parseProcStat(text string) (total, idle uint64, err error) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var vals []uint64
		for _, s := range f[1:] {
			v, perr := strconv.ParseUint(s, 10, 64)
			if perr != nil {
				return 0, 0, fmt.Errorf("parse /proc/stat: %w", perr)
			}
			vals = append(vals, v)
		}
		// user nice system idle iowait irq softirq steal (guest is inside user).
		n := len(vals)
		if n > 8 {
			n = 8
		}
		for _, v := range vals[:n] {
			total += v
		}
		idle = vals[3]
		if len(vals) > 4 {
			idle += vals[4]
		}
		return total, idle, nil
	}
	return 0, 0, errors.New("parse /proc/stat: no aggregate cpu line")
}

// parseMeminfo returns MemTotal and MemAvailable in kB.
func parseMeminfo(text string) (totalKB, availKB uint64, err error) {
	var haveT, haveA bool
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MemTotal:", "MemAvailable:":
			v, perr := strconv.ParseUint(f[1], 10, 64)
			if perr != nil {
				return 0, 0, fmt.Errorf("parse /proc/meminfo: %w", perr)
			}
			if f[0] == "MemTotal:" {
				totalKB, haveT = v, true
			} else {
				availKB, haveA = v, true
			}
		}
	}
	if !haveT || !haveA {
		return 0, 0, errors.New("parse /proc/meminfo: MemTotal/MemAvailable missing")
	}
	return totalKB, availKB, nil
}

// parseLoadavg returns the 1-minute load average from /proc/loadavg.
func parseLoadavg(text string) (float64, error) {
	f := strings.Fields(text)
	if len(f) < 1 {
		return 0, errors.New("parse /proc/loadavg: empty")
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/loadavg: %w", err)
	}
	return v, nil
}

// parseCgroupCPUUsec returns usage_usec from a cgroup v2 cpu.stat file.
func parseCgroupCPUUsec(text string) (uint64, error) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[0] == "usage_usec" {
			v, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse cpu.stat: %w", err)
			}
			return v, nil
		}
	}
	return 0, errors.New("parse cpu.stat: no usage_usec")
}

// parseDefaultRouteIface returns the interface of the default route from
// /proc/net/route, or "" if there is none.
func parseDefaultRouteIface(text string) string {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 8 && f[1] == "00000000" && f[7] == "00000000" {
			return f[0]
		}
	}
	return ""
}

// --- real sampler ---

// SystemSampler samples the live host: /proc for CPU, memory and load, cgroup v2
// for per-unit and per-session usage, netlink for egress bytes and statfs for
// disk. It needs no root: every source is world-readable, and units or sessions
// whose cgroups cannot be read are reported as absent.
type SystemSampler struct {
	// CgroupRoot is the cgroup v2 mount (default /sys/fs/cgroup). Units are
	// read from <root>/system.slice/<unit>.
	CgroupRoot string
	// SessionRoot is the directory holding one cgroup per command session
	// (the agent's cgroup_root). Empty disables session attribution.
	SessionRoot string
	// ProcRoot is the /proc mount (default /proc).
	ProcRoot string
	// DiskPath is the filesystem the disk metric watches (default /).
	DiskPath string
	// Now is the clock (default time.Now).
	Now func() time.Time

	mu       sync.Mutex
	prevTime time.Time
	prevTot  uint64
	prevIdle uint64
	prevTx   uint64
	haveTx   bool
	prevCg   map[string]cgPrev
}

type cgPrev struct {
	usec uint64
	at   time.Time
}

// NewSystemSampler returns a SystemSampler with defaults applied.
func NewSystemSampler(sessionRoot, diskPath string) *SystemSampler {
	return &SystemSampler{CgroupRoot: "/sys/fs/cgroup", SessionRoot: sessionRoot, ProcRoot: "/proc", DiskPath: diskPath}
}

func (s *SystemSampler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *SystemSampler) proc(name string) (string, error) {
	root := s.ProcRoot
	if root == "" {
		root = "/proc"
	}
	b, err := os.ReadFile(filepath.Join(root, name))
	return string(b), err
}

// Sample reads one Sample. CPU, egress and per-cgroup CPU are deltas against the
// previous call, so the first call reports zero for them.
func (s *SystemSampler) Sample(ctx context.Context, units []string) (Sample, error) {
	if err := ctx.Err(); err != nil {
		return Sample{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := Sample{Time: now, Services: map[string]UsageMetrics{}}
	ncpu := float64(runtime.NumCPU())

	stat, err := s.proc("stat")
	if err != nil {
		return Sample{}, fmt.Errorf("sample cpu: %w", err)
	}
	tot, idle, err := parseProcStat(stat)
	if err != nil {
		return Sample{}, err
	}
	if s.prevTot != 0 && tot > s.prevTot {
		busy := float64((tot - s.prevTot) - (idle - s.prevIdle))
		out.Machine.CPUPercent = clampPercent(100 * busy / float64(tot-s.prevTot))
	}
	s.prevTot, s.prevIdle = tot, idle

	mi, err := s.proc("meminfo")
	if err != nil {
		return Sample{}, fmt.Errorf("sample memory: %w", err)
	}
	totalKB, availKB, err := parseMeminfo(mi)
	if err != nil {
		return Sample{}, err
	}
	if totalKB > 0 {
		out.Machine.MemoryPercent = clampPercent(100 * float64(totalKB-availKB) / float64(totalKB))
	}

	la, err := s.proc("loadavg")
	if err != nil {
		return Sample{}, fmt.Errorf("sample load: %w", err)
	}
	load, err := parseLoadavg(la)
	if err != nil {
		return Sample{}, err
	}
	out.Machine.LoadPerCPU = load / ncpu

	// Disk and egress are best-effort: a failure leaves the metric at zero
	// (never a spurious breach) and is surfaced by the error return only when
	// every optional source fails.
	if pct, derr := diskPercent(s.diskPath()); derr == nil {
		out.Machine.DiskPercent = pct
	}
	if tx, terr := linkTxBytes(); terr == nil {
		if s.haveTx && tx >= s.prevTx && !s.prevTime.IsZero() {
			if dt := now.Sub(s.prevTime).Seconds(); dt > 0 {
				out.Machine.EgressMbps = float64(tx-s.prevTx) * 8 / 1e6 / dt
			}
		}
		s.prevTx, s.haveTx = tx, true
	}
	s.prevTime = now

	memBytes := float64(totalKB) * 1024
	cgRoot := s.CgroupRoot
	if cgRoot == "" {
		cgRoot = "/sys/fs/cgroup"
	}
	if s.prevCg == nil {
		s.prevCg = map[string]cgPrev{}
	}
	seen := map[string]bool{}
	read := func(dir string) UsageMetrics {
		seen[dir] = true
		u := s.readCgroup(dir, now, ncpu, memBytes)
		return u
	}
	for _, unit := range units {
		out.Services[unit] = read(filepath.Join(cgRoot, "system.slice", unit))
	}
	if s.SessionRoot != "" {
		if ents, derr := os.ReadDir(s.SessionRoot); derr == nil {
			for _, e := range ents {
				if !e.IsDir() {
					continue
				}
				dir := filepath.Join(s.SessionRoot, e.Name())
				u := read(dir)
				if u.Present {
					out.Sessions = append(out.Sessions, SessionUsage{ID: e.Name(), CgroupPath: dir, UsageMetrics: u})
				}
			}
		}
	}
	for dir := range s.prevCg {
		if !seen[dir] {
			delete(s.prevCg, dir)
		}
	}
	s.prevTime = now
	return out, nil
}

func (s *SystemSampler) diskPath() string {
	if s.DiskPath != "" {
		return s.DiskPath
	}
	return "/"
}

func (s *SystemSampler) readCgroup(dir string, now time.Time, ncpu, memBytes float64) UsageMetrics {
	cpuText, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return UsageMetrics{}
	}
	u := UsageMetrics{Present: true}
	if usec, perr := parseCgroupCPUUsec(string(cpuText)); perr == nil {
		if prev, ok := s.prevCg[dir]; ok && usec >= prev.usec {
			if dt := now.Sub(prev.at).Seconds(); dt > 0 {
				u.CPUPercent = clampPercent(100 * float64(usec-prev.usec) / 1e6 / dt / ncpu)
			}
		}
		s.prevCg[dir] = cgPrev{usec: usec, at: now}
	}
	if b, merr := os.ReadFile(filepath.Join(dir, "memory.current")); merr == nil && memBytes > 0 {
		if v, perr := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); perr == nil {
			u.MemoryPercent = clampPercent(100 * float64(v) / memBytes)
		}
	}
	return u
}

func clampPercent(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	}
	return v
}

// DefaultRouteIface returns the egress interface name (the default route's), or
// "" when it cannot be determined.
func DefaultRouteIface() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	return parseDefaultRouteIface(string(b))
}
