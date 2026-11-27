package watchdog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
)

// WatchdogSubdir is the directory under the base data dir (D-0006) that holds
// the watchdog's persistent state: the last-known policy and the guard counters.
const WatchdogSubdir = config.WatchdogDirName

// File names inside <DataDir>/watchdog.
const (
	policyFileName = "policy.yaml"
	stateFileName  = "state.json"
)

// Duration is a time.Duration written in policy YAML as a string such as "20s".
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalJSON accepts only strings ("5s", "10m").
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("duration must be a string such as \"20s\" or \"10m\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Threshold is one metric's hysteresis pair: a breach starts when the value is
// at or above Trigger and clears only when it falls below Recover. A zero
// Trigger disables the metric.
type Threshold struct {
	Trigger float64 `json:"trigger"`
	Recover float64 `json:"recover"`
}

// Enabled reports whether the metric is watched.
func (t Threshold) Enabled() bool { return t.Trigger > 0 }

// MachineThresholds are the machine-level limits. CPU and memory are percent of
// the whole machine; load is the 1-minute load average per CPU; disk is percent
// used of the watched filesystem; egress is megabits per second.
type MachineThresholds struct {
	CPUPercent    Threshold `json:"cpu_percent"`
	MemoryPercent Threshold `json:"memory_percent"`
	LoadPerCPU    Threshold `json:"load_per_cpu"`
	DiskPercent   Threshold `json:"disk_percent"`
	EgressMbps    Threshold `json:"egress_mbps"`
}

// ServiceThresholds are the per-service limits (a systemd unit's cgroup). CPU
// and memory are percent of the whole machine.
type ServiceThresholds struct {
	CPUPercent    Threshold `json:"cpu_percent"`
	MemoryPercent Threshold `json:"memory_percent"`
}

// LoadControllerCeiling is the highest CPU/RAM ceiling the Load Controller (D-0005,
// architecture 11.2) may drive the machine to. Machine thresholds must sit above
// it: the watchdog is a backstop for load that escapes the controller and must
// never throttle intended Maneuver load.
type LoadControllerCeiling struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
}

// Steps toggles each rung of the escalation ladder.
type Steps struct {
	Throttle    bool `json:"throttle"`
	KillSession bool `json:"kill_session"`
	Restart     bool `json:"restart"`
	Reboot      bool `json:"reboot"`
}

// ThrottlePolicy configures step 1.
type ThrottlePolicy struct {
	Duration Duration `json:"duration"`
	// CPUPercent is the CPU quota applied, as percent of the whole machine.
	CPUPercent float64 `json:"cpu_percent"`
	// EgressKbit, when > 0, is the tc tbf rate applied to egress.
	EgressKbit int `json:"egress_kbit"`
}

// Cooldowns block further actions for a while after each kind of action.
type Cooldowns struct {
	Throttle    Duration `json:"throttle"`
	KillSession Duration `json:"kill_session"`
	Restart     Duration `json:"restart"`
	Reboot      Duration `json:"reboot"`
}

// Caps are the daily limits on the disruptive steps.
type Caps struct {
	RestartsPerDay int `json:"restarts_per_day"`
	RebootsPerDay  int `json:"reboots_per_day"`
}

// Policy is one effective (override-resolved) watchdog policy. Every key is
// optional in YAML: absent keys keep the baked-in default.
type Policy struct {
	// Version stamps the profile. The baked-in default is version 0; a pushed
	// profile must carry version >= 1.
	Version        int                          `json:"version"`
	SampleInterval Duration                     `json:"sample_interval"`
	Sustain        Duration                     `json:"sustain"`
	Machine        MachineThresholds            `json:"machine"`
	Services       map[string]ServiceThresholds `json:"services"`
	LoadController LoadControllerCeiling        `json:"load_controller_ceiling"`
	Steps          Steps                        `json:"steps"`
	Throttle       ThrottlePolicy               `json:"throttle"`
	Cooldowns      Cooldowns                    `json:"cooldowns"`
	Caps           Caps                         `json:"caps"`
	BootWindow     Duration                     `json:"boot_window"`
	// StageReset is how long the host must stay clear before the ladder
	// starts again from step 1.
	StageReset Duration `json:"stage_reset"`
	// AlertOnly forces alert-only mode: measure and notify, never act.
	AlertOnly bool `json:"alert_only"`
	// AutoResetMidnight clears the daily counters and a latched alert-only
	// mode when the local date changes.
	AutoResetMidnight bool `json:"auto_reset_midnight"`
	// AgentUnit is the unit restarted (step 3) when the offender is a session
	// or cannot be attributed to a watched service.
	AgentUnit string `json:"agent_unit"`
	// AttributionShare is the minimum share of the breached metric one
	// session or service must hold to be named the offender.
	AttributionShare float64 `json:"attribution_share"`
	// DiskPath is the filesystem the disk threshold watches.
	DiskPath string `json:"disk_path"`
	// HeartbeatWrite is how often the watchdog and the main agent write their
	// heartbeat file from their main loop; HeartbeatThreshold is how old the
	// file may get before the liveness timer calls the service hung and
	// restarts it (T-0027). Bounds: see ValidateHeartbeat. The liveness
	// timer's own 1 min cadence is fixed and not a policy field.
	HeartbeatWrite     Duration `json:"heartbeat_write"`
	HeartbeatThreshold Duration `json:"heartbeat_threshold"`
}

// DefaultPolicy is the minimal baked-in profile used on first boot, before the
// server has pushed anything, and whenever no valid last-known profile exists.
// Its machine thresholds sit above the default Load Controller ceiling.
func DefaultPolicy() Policy {
	return Policy{
		Version:        0,
		SampleInterval: Duration(5 * time.Second),
		Sustain:        Duration(20 * time.Second),
		Machine: MachineThresholds{
			CPUPercent:    Threshold{Trigger: 97, Recover: 92},
			MemoryPercent: Threshold{Trigger: 96, Recover: 92},
			LoadPerCPU:    Threshold{Trigger: 8, Recover: 5},
			DiskPercent:   Threshold{Trigger: 95, Recover: 90},
			// Egress is off until a profile sets a bandwidth limit.
		},
		Services:       map[string]ServiceThresholds{},
		LoadController: LoadControllerCeiling{CPUPercent: 90, MemoryPercent: 90},
		Steps:          Steps{Throttle: true, KillSession: true, Restart: true, Reboot: true},
		Throttle:       ThrottlePolicy{Duration: Duration(120 * time.Second), CPUPercent: 50},
		Cooldowns: Cooldowns{
			Throttle:    Duration(10 * time.Minute),
			KillSession: Duration(time.Minute),
			Restart:     Duration(30 * time.Minute),
			Reboot:      Duration(60 * time.Minute),
		},
		Caps:              Caps{RestartsPerDay: 3, RebootsPerDay: 1},
		BootWindow:        Duration(5 * time.Minute),
		StageReset:        Duration(15 * time.Minute),
		AutoResetMidnight: true,
		AgentUnit:         "fleet-agent.service",
		AttributionShare:  0.5,
		DiskPath:          "/",

		HeartbeatWrite:     Duration(DefaultHeartbeatWrite),
		HeartbeatThreshold: Duration(DefaultHeartbeatThreshold),
	}
}

// SustainSamples is the number of consecutive breaching samples needed before
// acting: Sustain divided by SampleInterval, rounded up (20 s / 5 s = 4).
func (p Policy) SustainSamples() int {
	if p.SampleInterval <= 0 {
		return 1
	}
	n := int(math.Ceil(float64(p.Sustain) / float64(p.SampleInterval)))
	if n < 1 {
		n = 1
	}
	return n
}

var unitNameRE = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]+$`)

func validUnit(s string) bool { return unitNameRE.MatchString(s) && !strings.HasPrefix(s, "-") }

// selfUnits are never watched or acted on: restarting the watchdog from itself
// would defeat the liveness design.
var selfUnits = map[string]bool{"fleet-watchdog.service": true, "fleet-watchdog-liveness.service": true}

func (t Threshold) check(name string, max float64) error {
	if t.Trigger < 0 || t.Recover < 0 {
		return fmt.Errorf("%s: trigger and recover must not be negative", name)
	}
	if !t.Enabled() {
		return nil
	}
	if max > 0 && t.Trigger > max {
		return fmt.Errorf("%s: trigger %v is above %v", name, t.Trigger, max)
	}
	if t.Recover <= 0 || t.Recover > t.Trigger {
		return fmt.Errorf("%s: recover %v must be > 0 and <= trigger %v (hysteresis)", name, t.Recover, t.Trigger)
	}
	return nil
}

// Validate checks a policy for internal consistency, including the rule that
// machine CPU/memory thresholds sit above the Load Controller ceiling.
func (p Policy) Validate() error {
	if p.SampleInterval.D() < 100*time.Millisecond {
		return fmt.Errorf("sample_interval %s is below the 100ms minimum", p.SampleInterval.D())
	}
	if p.Sustain.D() < p.SampleInterval.D() {
		return fmt.Errorf("sustain %s must be at least one sample_interval (%s)", p.Sustain.D(), p.SampleInterval.D())
	}
	m := p.Machine
	for _, c := range []struct {
		name string
		t    Threshold
		max  float64
	}{
		{"machine.cpu_percent", m.CPUPercent, 100},
		{"machine.memory_percent", m.MemoryPercent, 100},
		{"machine.disk_percent", m.DiskPercent, 100},
		{"machine.load_per_cpu", m.LoadPerCPU, 0},
		{"machine.egress_mbps", m.EgressMbps, 0},
	} {
		if err := c.t.check(c.name, c.max); err != nil {
			return err
		}
	}
	lc := p.LoadController
	if lc.CPUPercent < 0 || lc.CPUPercent > 100 || lc.MemoryPercent < 0 || lc.MemoryPercent > 100 {
		return errors.New("load_controller_ceiling values must be within 0..100")
	}
	// Must not fight the Load Controller (architecture 11.2): a Maneuver that
	// sits at its ceiling must neither trigger nor hold a breach.
	for _, c := range []struct {
		name    string
		t       Threshold
		ceiling float64
	}{
		{"machine.cpu_percent", m.CPUPercent, lc.CPUPercent},
		{"machine.memory_percent", m.MemoryPercent, lc.MemoryPercent},
	} {
		if !c.t.Enabled() {
			continue
		}
		if c.t.Trigger <= c.ceiling {
			return fmt.Errorf("%s trigger %v must be above the Load Controller ceiling %v", c.name, c.t.Trigger, c.ceiling)
		}
		if c.t.Recover < c.ceiling {
			return fmt.Errorf("%s recover %v must not be below the Load Controller ceiling %v", c.name, c.t.Recover, c.ceiling)
		}
	}
	for name, s := range p.Services {
		if !validUnit(name) {
			return fmt.Errorf("services: invalid unit name %q", name)
		}
		if selfUnits[name] {
			return fmt.Errorf("services: %s must not be watched by the watchdog itself", name)
		}
		if err := s.CPUPercent.check("services."+name+".cpu_percent", 100); err != nil {
			return err
		}
		if err := s.MemoryPercent.check("services."+name+".memory_percent", 100); err != nil {
			return err
		}
	}
	if p.Throttle.Duration.D() <= 0 {
		return errors.New("throttle.duration must be positive")
	}
	if p.Throttle.CPUPercent <= 0 || p.Throttle.CPUPercent > 100 {
		return errors.New("throttle.cpu_percent must be within (0, 100]")
	}
	if p.Throttle.EgressKbit < 0 {
		return errors.New("throttle.egress_kbit must not be negative")
	}
	for name, d := range map[string]Duration{
		"cooldowns.throttle": p.Cooldowns.Throttle, "cooldowns.kill_session": p.Cooldowns.KillSession,
		"cooldowns.restart": p.Cooldowns.Restart, "cooldowns.reboot": p.Cooldowns.Reboot,
		"boot_window": p.BootWindow, "stage_reset": p.StageReset,
	} {
		if d < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	if p.Caps.RestartsPerDay < 0 || p.Caps.RebootsPerDay < 0 {
		return errors.New("caps must not be negative")
	}
	if p.AttributionShare <= 0 || p.AttributionShare > 1 {
		return errors.New("attribution_share must be within (0, 1]")
	}
	if p.Steps.Restart && !validUnit(p.AgentUnit) {
		return fmt.Errorf("agent_unit %q is not a valid unit name", p.AgentUnit)
	}
	if selfUnits[p.AgentUnit] {
		return fmt.Errorf("agent_unit must not be the watchdog's own unit")
	}
	if !filepath.IsAbs(p.DiskPath) {
		return fmt.Errorf("disk_path %q must be absolute", p.DiskPath)
	}
	return ValidateHeartbeat(p.HeartbeatWrite.D(), p.HeartbeatThreshold.D())
}

// Profile is a parsed, validated, version-stamped policy profile: the base
// policy plus the per-host and per-group overrides it carries.
type Profile struct {
	Version int
	// Raw is the YAML text as pushed; it is what is persisted as last-known.
	Raw []byte

	base   map[string]any
	hosts  map[string]map[string]any
	groups map[string]map[string]any
}

// DefaultProfile wraps the baked-in default policy.
func DefaultProfile() *Profile {
	b, _ := json.Marshal(DefaultPolicy())
	var tree map[string]any
	_ = json.Unmarshal(b, &tree)
	return &Profile{Version: 0, base: tree}
}

// ParseProfile parses and validates policy YAML. It validates the base policy
// and the policy each host/group override resolves to, so a profile that parses
// here can always be applied. The error describes the first problem found.
func ParseProfile(raw []byte) (*Profile, error) {
	tree, err := parseYAML(raw)
	if err != nil {
		return nil, err
	}
	ver, ok := tree["version"].(int64)
	if !ok || ver < 1 {
		return nil, errors.New("policy: \"version\" is required and must be an integer >= 1")
	}
	pf := &Profile{Version: int(ver), Raw: append([]byte(nil), raw...), hosts: map[string]map[string]any{}, groups: map[string]map[string]any{}}
	if ov, present := tree["overrides"]; present {
		om, ok := ov.(map[string]any)
		if !ok {
			return nil, errors.New("policy: \"overrides\" must be a mapping with \"hosts\" and/or \"groups\"")
		}
		for kind, v := range om {
			if kind != "hosts" && kind != "groups" {
				return nil, fmt.Errorf("policy: overrides.%s: expected \"hosts\" or \"groups\"", kind)
			}
			entries, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("policy: overrides.%s must be a mapping keyed by name", kind)
			}
			for name, e := range entries {
				em, ok := e.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("policy: overrides.%s.%s must be a mapping", kind, name)
				}
				for _, banned := range []string{"version", "overrides"} {
					if _, present := em[banned]; present {
						return nil, fmt.Errorf("policy: overrides.%s.%s must not set %q", kind, name, banned)
					}
				}
				if kind == "hosts" {
					pf.hosts[name] = em
				} else {
					pf.groups[name] = em
				}
			}
		}
		delete(tree, "overrides")
	}
	pf.base = tree
	if _, err := pf.decode(nil); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	for name, em := range pf.hosts {
		if _, err := pf.decode([]map[string]any{em}); err != nil {
			return nil, fmt.Errorf("policy: overrides.hosts.%s: %w", name, err)
		}
	}
	for name, em := range pf.groups {
		if _, err := pf.decode([]map[string]any{em}); err != nil {
			return nil, fmt.Errorf("policy: overrides.groups.%s: %w", name, err)
		}
	}
	return pf, nil
}

// Effective resolves the policy for one machine: the base, then the overrides
// of each of its groups (in the order given), then the machine's own override.
func (pf *Profile) Effective(machineID string, groups []string) (Policy, error) {
	var layers []map[string]any
	for _, g := range groups {
		if em, ok := pf.groups[g]; ok {
			layers = append(layers, em)
		}
	}
	if em, ok := pf.hosts[machineID]; ok {
		layers = append(layers, em)
	}
	return pf.decode(layers)
}

// Names lists the host and group override names (for status output).
func (pf *Profile) Names() (hosts, groups []string) {
	for h := range pf.hosts {
		hosts = append(hosts, h)
	}
	for g := range pf.groups {
		groups = append(groups, g)
	}
	sort.Strings(hosts)
	sort.Strings(groups)
	return hosts, groups
}

func (pf *Profile) decode(layers []map[string]any) (Policy, error) {
	tree := deepCopy(pf.base)
	for _, l := range layers {
		mergeInto(tree, l)
	}
	b, err := json.Marshal(tree)
	if err != nil {
		return Policy{}, err
	}
	p := DefaultPolicy()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, err
	}
	p.Version = pf.Version
	if p.Services == nil {
		p.Services = map[string]ServiceThresholds{}
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func deepCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			out[k] = deepCopy(sub)
		} else {
			out[k] = v
		}
	}
	return out
}

// mergeInto overlays src onto dst: mappings merge key by key, anything else
// replaces.
func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				mergeInto(dv, sv)
				continue
			}
			dst[k] = deepCopy(sv)
			continue
		}
		dst[k] = v
	}
}

// policyPath and statePath locate the persisted files under the base data dir.
func policyPath(dataDir string) string {
	return filepath.Join(dataDir, WatchdogSubdir, policyFileName)
}
func statePath(dataDir string) string { return filepath.Join(dataDir, WatchdogSubdir, stateFileName) }

// LoadLastKnown reads and validates the persisted last-known profile. A missing
// file returns (nil, nil); a corrupt or invalid file returns the error so the
// caller can fall back to the baked-in default.
func LoadLastKnown(dataDir string) (*Profile, error) {
	raw, err := os.ReadFile(policyPath(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read last-known policy: %w", err)
	}
	pf, err := ParseProfile(raw)
	if err != nil {
		return nil, fmt.Errorf("last-known policy is invalid: %w", err)
	}
	return pf, nil
}

// SaveLastKnown atomically persists the profile's YAML as the last-known policy.
func SaveLastKnown(dataDir string, pf *Profile) error {
	if len(pf.Raw) == 0 {
		return errors.New("save policy: profile has no source text")
	}
	return writeFileAtomic(policyPath(dataDir), pf.Raw, 0o640)
}

// writeFileAtomic writes via a temporary file in the same directory and a rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Chmod(name, perm); err != nil {
		cleanup()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// ErrStalePolicy means a pushed profile is older than the stored one.
var ErrStalePolicy = errors.New("policy version is older than the current one")

// StorePolicy validates a pushed profile and, if it is acceptable, persists it
// as the last-known policy. It is the entry point for a process that holds the
// channel but not the watchdog loop: the running watchdog notices the file
// change and applies it live. An invalid or older profile is rejected and the
// stored one is left untouched. current is the version already in force (-1 to
// take it from disk).
func StorePolicy(dataDir string, raw []byte, current int) (*Profile, error) {
	pf, err := ParseProfile(raw)
	if err != nil {
		return nil, err
	}
	if current < 0 {
		if old, err := LoadLastKnown(dataDir); err == nil && old != nil {
			current = old.Version
		} else {
			current = 0
		}
	}
	if pf.Version < current {
		return nil, fmt.Errorf("%w (pushed %d, current %d)", ErrStalePolicy, pf.Version, current)
	}
	if err := SaveLastKnown(dataDir, pf); err != nil {
		return nil, err
	}
	return pf, nil
}
