package watchdog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Heartbeat-based hang detection (T-0027). The unit's `systemctl is-active`
// check only catches a crashed or stopped service; a process that is deadlocked
// yet still "active" needs more. So both the watchdog and the main agent write a
// tiny timestamp file every `heartbeat_write` seconds from inside their main
// loop, and an independent systemd timer (fixed 1 min cadence) runs a one-shot
// that restarts the unit when the file is older than `heartbeat_threshold`.
//
// The file is "<unix seconds> <threshold seconds>\n", fixed width, so one
// 17-byte write replaces it in place (cheap, and a reader never sees a torn
// value). It carries the live threshold, so the shell one-shot (LivenessScript)
// needs no policy parser and follows a pushed policy change at the next beat.

// Heartbeat bounds and defaults. The check cadence is the systemd timer's
// OnUnitActiveSec and is deliberately not a policy field.
const (
	// HeartbeatCheckEvery is the fixed cadence of the liveness timers.
	HeartbeatCheckEvery = time.Minute

	MinHeartbeatWrite = 2 * time.Second
	MaxHeartbeatWrite = 15 * time.Second
	// MinHeartbeatThreshold equals the check cadence: a threshold below it
	// could never be observed by the timer.
	MinHeartbeatThreshold = HeartbeatCheckEvery
	MaxHeartbeatThreshold = 300 * time.Second
	// HeartbeatThresholdFactor: the threshold must cover this many missed writes.
	HeartbeatThresholdFactor = 3

	DefaultHeartbeatWrite     = 5 * time.Second
	DefaultHeartbeatThreshold = 90 * time.Second
)

// Which service a heartbeat file belongs to.
const (
	HeartbeatWatchdog = "watchdog"
	HeartbeatAgent    = "agent"
)

// ValidateHeartbeat is the single check of the heartbeat settings, used by the
// policy parser (and so by the panel's save, which parses the YAML) on both the
// server and the agent: write within [2 s, 15 s], threshold within [1 min,
// 300 s], and threshold at least 3 x write.
func ValidateHeartbeat(write, threshold time.Duration) error {
	if write < MinHeartbeatWrite || write > MaxHeartbeatWrite {
		return fmt.Errorf("heartbeat_write %s must be between %s and %s", write, MinHeartbeatWrite, MaxHeartbeatWrite)
	}
	if threshold < MinHeartbeatThreshold || threshold > MaxHeartbeatThreshold {
		return fmt.Errorf("heartbeat_threshold %s must be between %s and %s (the liveness check runs every %s)",
			threshold, MinHeartbeatThreshold, MaxHeartbeatThreshold, HeartbeatCheckEvery)
	}
	if threshold < HeartbeatThresholdFactor*write {
		return fmt.Errorf("heartbeat_threshold %s must be at least %d x heartbeat_write (%s)",
			threshold, HeartbeatThresholdFactor, HeartbeatThresholdFactor*write)
	}
	return nil
}

// HeartbeatPath is the heartbeat file of one service ("watchdog" or "agent")
// under the base data dir, next to the watchdog's other state.
func HeartbeatPath(dataDir, who string) string {
	return filepath.Join(dataDir, WatchdogSubdir, "heartbeat-"+who)
}

// Heartbeat writes the heartbeat file. It is driven by the service's main loop
// (Tick), never by a goroutine of its own: a heartbeat that outlived a hung loop
// would defeat the check. Safe for concurrent use.
type Heartbeat struct {
	path string
	// Now is the clock (tests).
	Now func() time.Time

	mu   sync.Mutex
	last time.Time
}

// NewHeartbeat returns a writer for path. Nothing is created until the first Tick.
func NewHeartbeat(path string) *Heartbeat { return &Heartbeat{path: path, Now: time.Now} }

// Path is the heartbeat file.
func (h *Heartbeat) Path() string { return h.path }

// Tick is called from the main loop on every wake-up. It writes the heartbeat
// when `write` has elapsed since the last one and returns how long until the
// next one is due, so the loop can bound its sleep. An invalid write/threshold
// (it cannot come from a validated policy) falls back to the defaults. A failed
// write is returned but not fatal: the next Tick retries.
func (h *Heartbeat) Tick(write, threshold time.Duration) (next time.Duration, err error) {
	if ValidateHeartbeat(write, threshold) != nil {
		write, threshold = DefaultHeartbeatWrite, DefaultHeartbeatThreshold
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.Now()
	if elapsed := now.Sub(h.last); !h.last.IsZero() && elapsed >= 0 && elapsed < write {
		return write - elapsed, nil
	}
	if err := h.write(now, threshold); err != nil {
		return write, err
	}
	h.last = now
	return write, nil
}

func (h *Heartbeat) write(now time.Time, threshold time.Duration) error {
	// Opened per write (a few syscalls every 2-15 s) rather than held open, so a
	// deleted or replaced file is recreated at the next beat.
	f, err := os.OpenFile(h.path, os.O_WRONLY|os.O_CREATE, 0o644)
	if errors.Is(err, fs.ErrNotExist) {
		if err = os.MkdirAll(filepath.Dir(h.path), 0o750); err == nil {
			f, err = os.OpenFile(h.path, os.O_WRONLY|os.O_CREATE, 0o644)
		}
	}
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	line := fmt.Sprintf("%010d %05d\n", now.Unix(), int64(threshold/time.Second))
	_, err = f.WriteAt([]byte(line), 0)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}

// ReadHeartbeat parses a heartbeat file: when it was last written and the
// threshold it carried.
func ReadHeartbeat(path string) (at time.Time, threshold time.Duration, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, 0, err
	}
	var sec, th int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d %d", &sec, &th); err != nil {
		return time.Time{}, 0, errors.New("heartbeat: unreadable heartbeat file")
	}
	return time.Unix(sec, 0), time.Duration(th) * time.Second, nil
}

// HeartbeatSettings reads the effective heartbeat write interval and threshold
// for a machine from the last-known policy under dataDir, falling back to the
// defaults when there is no usable policy. The main agent uses it (it does not
// run the watchdog loop). The parsed result is cached until policy.yaml changes.
type HeartbeatSettings struct {
	DataDir   string
	MachineID string
	Groups    []string

	mu     sync.Mutex
	stamp  string // mtime+size of the policy file the cache was read from
	write  time.Duration
	thresh time.Duration
	loaded bool
}

// Get returns the settings in force.
func (s *HeartbeatSettings) Get() (write, threshold time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp := ""
	if fi, err := os.Stat(policyPath(s.DataDir)); err == nil {
		stamp = fmt.Sprintf("%d/%d", fi.ModTime().UnixNano(), fi.Size())
	}
	if s.loaded && stamp == s.stamp {
		return s.write, s.thresh
	}
	s.stamp, s.loaded = stamp, true
	s.write, s.thresh = DefaultHeartbeatWrite, DefaultHeartbeatThreshold
	if stamp == "" {
		return s.write, s.thresh
	}
	if pf, err := LoadLastKnown(s.DataDir); err == nil && pf != nil {
		if p, err := pf.Effective(s.MachineID, s.Groups); err == nil {
			s.write, s.thresh = p.HeartbeatWrite.D(), p.HeartbeatThreshold.D()
		}
	}
	return s.write, s.thresh
}

// LivenessScript is the body of the liveness one-shot the timer runs for one
// systemd unit (fleet-watchdog.service or fleet-agent.service). It is plain
// POSIX sh and is the only implementation of the freshness check:
//
//   - unit not active: start it (the original is-active check, kept);
//   - no heartbeat file: do nothing (a binary or role that does not write one yet
//     must not be restarted in a loop);
//   - otherwise the reference time is the later of the last heartbeat and the
//     moment the unit became active (a stale file from before a restart does not
//     count); older than the threshold carried in the file (90 s if unreadable)
//     means hung: log it and restart the unit.
func LivenessScript(unit, heartbeatFile string) string {
	return fmt.Sprintf(`u=%[1]s; f=%[2]s; d=%[3]d; now=$(date +%%s); `+
		`if ! systemctl is-active --quiet "$u"; then logger -t fleet-liveness "$u was not active; starting it"; systemctl start "$u"; exit 0; fi; `+
		`[ -r "$f" ] || exit 0; `+
		`read -r ts th <"$f"; `+
		`case "$ts" in ""|*[!0-9]*) ts=0;; esac; `+
		`case "$th" in ""|*[!0-9]*) th=$d;; esac; `+
		`s=$(systemctl show -p ActiveEnterTimestamp --value "$u" 2>/dev/null); `+
		`if [ -n "$s" ]; then s=$(date -d "$s" +%%s 2>/dev/null) || s=0; else s=0; fi; `+
		`ref=$ts; [ "$s" -gt "$ref" ] && ref=$s; `+
		`[ "$ref" -gt 0 ] || exit 0; `+
		`if [ $((now-ref)) -gt "$th" ]; then logger -t fleet-liveness "$u heartbeat stale (hung); restarting it"; systemctl restart "$u"; fi`,
		unit, heartbeatFile, int(DefaultHeartbeatThreshold/time.Second))
}

// LivenessExecStart is the unit file ExecStart line that runs LivenessScript:
// `$` and `%` are special to systemd in ExecStart and are doubled.
func LivenessExecStart(unit, heartbeatFile string) string {
	s := LivenessScript(unit, heartbeatFile)
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, "$", "$$")
	return "/bin/sh -c '" + s + "'"
}
