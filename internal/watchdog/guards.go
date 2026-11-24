package watchdog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// Step is one rung of the escalation ladder.
type Step int

// The ladder, in order. StepNone means no action has been taken yet.
const (
	StepNone Step = iota
	StepThrottle
	StepKillSession
	StepRestart
	StepReboot
)

func (s Step) String() string {
	switch s {
	case StepThrottle:
		return "throttle"
	case StepKillSession:
		return "kill_session"
	case StepRestart:
		return "restart"
	case StepReboot:
		return "reboot"
	}
	return "none"
}

// State is everything the guards persist so that it survives a reboot (and so a
// reboot cannot reset the daily caps or the cooldowns). It lives in
// <DataDir>/watchdog/state.json (D-0006).
type State struct {
	// Day is the local date (YYYY-MM-DD) the daily counters belong to.
	Day      string `json:"day"`
	Restarts int    `json:"restarts"`
	Reboots  int    `json:"reboots"`
	// Last* are when each kind of action was last taken (zero = never).
	LastThrottle    time.Time `json:"last_throttle"`
	LastKillSession time.Time `json:"last_kill_session"`
	LastRestart     time.Time `json:"last_restart"`
	LastReboot      time.Time `json:"last_reboot"`
	// AlertOnly is latched when a daily cap is exhausted: keep measuring and
	// notifying, act no further until reset (operator, or midnight if the
	// policy allows).
	AlertOnly bool `json:"alert_only"`
	// Stage is the last ladder step taken in the current episode.
	Stage Step `json:"stage"`
}

// Guards enforces cooldowns, daily caps and the alert-only latch over a
// persisted State. It is safe for concurrent use.
type Guards struct {
	mu    sync.Mutex
	path  string
	state State
	// Corrupt is set when the state file existed but could not be read; the
	// guards then start in alert-only mode (fail safe: never risk a reboot
	// loop on lost counters) until an operator resets them.
	Corrupt error
}

// LoadGuards reads the persisted state at <dataDir>/watchdog/state.json. A
// missing file yields a fresh state. A corrupt file is moved aside and the guards
// start latched in alert-only mode.
func LoadGuards(dataDir string) (*Guards, error) {
	g := &Guards{path: statePath(dataDir)}
	b, err := os.ReadFile(g.path)
	switch {
	case err == nil:
		if jerr := json.Unmarshal(b, &g.state); jerr != nil {
			g.Corrupt = fmt.Errorf("watchdog state %s is corrupt: %w", g.path, jerr)
			_ = os.Rename(g.path, g.path+".corrupt")
			g.state = State{AlertOnly: true}
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, fmt.Errorf("read watchdog state: %w", err)
	}
	return g, nil
}

// Snapshot returns a copy of the current state.
func (g *Guards) Snapshot() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

// Roll starts a new day when the local date changed since the counters were
// last written: the daily counters reset, and so does a latched alert-only mode
// if autoReset is set. It reports whether the date changed.
func (g *Guards) Roll(now time.Time, autoReset bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	today := dayKey(now)
	if g.state.Day == today {
		return false
	}
	changed := g.state.Day != ""
	g.state.Day = today
	g.state.Restarts, g.state.Reboots = 0, 0
	if autoReset {
		g.state.AlertOnly = false
	}
	_ = g.saveLocked()
	return changed
}

// CooldownRemaining returns how long until every cooldown has elapsed (0 when
// none is running).
func (g *Guards) CooldownRemaining(now time.Time, c Cooldowns) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	var rem time.Duration
	for _, x := range []struct {
		last time.Time
		d    Duration
	}{
		{g.state.LastThrottle, c.Throttle},
		{g.state.LastKillSession, c.KillSession},
		{g.state.LastRestart, c.Restart},
		{g.state.LastReboot, c.Reboot},
	} {
		if x.last.IsZero() {
			continue
		}
		if r := x.last.Add(x.d.D()).Sub(now); r > rem {
			rem = r
		}
	}
	return rem
}

// CapExhausted reports whether the daily cap for step is used up. Only restart
// and reboot are capped.
func (g *Guards) CapExhausted(step Step, caps Caps) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch step {
	case StepRestart:
		return g.state.Restarts >= caps.RestartsPerDay
	case StepReboot:
		return g.state.Reboots >= caps.RebootsPerDay
	}
	return false
}

// Record notes that step is being taken at now, advances the stage and persists
// the state. For restart and reboot it must be called, and must succeed, before
// the action runs: if the counters cannot be saved the action is refused rather
// than risk a reboot loop with the caps lost.
func (g *Guards) Record(step Step, now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch step {
	case StepThrottle:
		g.state.LastThrottle = now
	case StepKillSession:
		g.state.LastKillSession = now
	case StepRestart:
		g.state.LastRestart = now
		g.state.Restarts++
	case StepReboot:
		g.state.LastReboot = now
		g.state.Reboots++
	}
	g.state.Stage = step
	return g.saveLocked()
}

// SetAlertOnly latches or clears alert-only mode and persists it.
func (g *Guards) SetAlertOnly(v bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.AlertOnly == v {
		return nil
	}
	g.state.AlertOnly = v
	return g.saveLocked()
}

// ResetStage returns the ladder to step 1 (the host has been clear long enough).
func (g *Guards) ResetStage() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.Stage == StepNone {
		return nil
	}
	g.state.Stage = StepNone
	return g.saveLocked()
}

// Reset is the operator's counter reset (panel button): zero the daily counters,
// clear alert-only mode and restart the ladder. Cooldowns are kept: a reset must
// not allow an immediate second restart or reboot. now sets the day.
func (g *Guards) Reset(now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state.Day = dayKey(now)
	g.state.Restarts, g.state.Reboots = 0, 0
	g.state.AlertOnly = false
	g.state.Stage = StepNone
	g.Corrupt = nil
	return g.saveLocked()
}

func (g *Guards) saveLocked() error {
	b, err := json.MarshalIndent(g.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode watchdog state: %w", err)
	}
	return writeFileAtomic(g.path, append(b, '\n'), 0o640)
}
