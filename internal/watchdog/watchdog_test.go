package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
)

// fakeAct records every action instead of performing it.
type fakeAct struct {
	mu         sync.Mutex
	calls      []string
	notApplic  map[string]bool // "throttle"/"kill" -> return ErrNotApplicable
	rebootFail error
}

func (f *fakeAct) rec(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeAct) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAct) Throttle(_ context.Context, t Target, spec ThrottleSpec) error {
	if f.notApplic["throttle"] {
		return ErrNotApplicable
	}
	f.rec(fmt.Sprintf("throttle %s %.0f", t, spec.CPUPercent))
	return nil
}
func (f *fakeAct) Unthrottle(_ context.Context, t Target) error {
	f.rec("unthrottle " + t.String())
	return nil
}
func (f *fakeAct) KillSession(_ context.Context, t Target) error {
	if f.notApplic["kill"] {
		return ErrNotApplicable
	}
	f.rec("kill " + t.String())
	return nil
}
func (f *fakeAct) RestartService(_ context.Context, unit string) error {
	f.rec("restart " + unit)
	return nil
}
func (f *fakeAct) Reboot(context.Context) error {
	f.rec("reboot")
	return f.rebootFail
}

// fakeSampler replays whatever sample the test sets.
type fakeSampler struct {
	mu sync.Mutex
	s  Sample
}

func (f *fakeSampler) set(s Sample) {
	f.mu.Lock()
	f.s = s
	f.mu.Unlock()
}
func (f *fakeSampler) Sample(context.Context, []string) (Sample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.s, nil
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var t0 = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

// rig is one simulated host: a data dir, a fake actuator and a Watchdog. A
// "reboot" is simulated by building a new rig over the same data dir.
type rig struct {
	t   *testing.T
	dir string
	act *fakeAct
	w   *Watchdog
	now time.Time
	ev  []Event
	mu  sync.Mutex
}

func newRig(t *testing.T, dir string, boot time.Time) *rig {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	r := &rig{t: t, dir: dir, act: &fakeAct{}, now: t0}
	w, err := New(Config{
		DataDir: dir, MachineID: "w1", Groups: []string{"edge"},
		Sampler: &fakeSampler{}, Actuator: r.act, Log: quietLog(),
		BootTime: func() time.Time { return boot },
		Notify: func(e Event) {
			r.mu.Lock()
			r.ev = append(r.ev, e)
			r.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.w = w
	return r
}

func (r *rig) events(kind string) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.ev {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// tick observes s at the current simulated time, then advances one 5 s sample.
func (r *rig) tick(s Sample) {
	r.t.Helper()
	s.Time = r.now
	r.w.Engine().Observe(context.Background(), s)
	r.now = r.now.Add(5 * time.Second)
}

func (r *rig) apply(yaml string) {
	r.t.Helper()
	if err := r.w.ApplyPolicy([]byte(yaml)); err != nil {
		r.t.Fatalf("apply policy: %v", err)
	}
}

// hog is a machine CPU breach attributable to session s1.
func hog() Sample {
	return Sample{
		Machine:  MachineMetrics{CPUPercent: 99},
		Sessions: []SessionUsage{{ID: "s1", CgroupPath: "/sys/fs/cgroup/fleet/s1", UsageMetrics: UsageMetrics{Present: true, CPUPercent: 80}}},
	}
}

func idle() Sample { return Sample{Machine: MachineMetrics{CPUPercent: 10}} }

const fastPolicy = `version: 1
boot_window: 0s
stage_reset: 1h
throttle: {duration: 5s, cpu_percent: 40}
cooldowns: {throttle: 10s, kill_session: 10s, restart: 10s, reboot: 10s}
`

func wantCalls(t *testing.T, r *rig, want ...string) {
	t.Helper()
	got := r.act.all()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("actions:\n got  %q\n want %q", got, want)
	}
}

// A simulated sustained breach: nothing happens for the first 3 samples, the 4th
// (20 s) confirms the breach and the ladder then climbs throttle, kill session,
// restart, reboot, one rung per cooldown; the exhausted reboot cap ends in
// alert-only and no further action.
func TestSustainedBreachClimbsLadderInOrder(t *testing.T) {
	r := newRig(t, "", t0.Add(-24*time.Hour))
	r.apply(fastPolicy)

	for i := 0; i < 3; i++ { // 15 s of breach: not yet sustained
		r.tick(hog())
	}
	wantCalls(t, r)
	if len(r.events(EventBreach)) != 0 {
		t.Fatal("breach reported before the 20 s sustain window")
	}
	r.tick(hog()) // 4th consecutive breaching sample (t=15s)
	wantCalls(t, r, "throttle session:s1 40")
	if len(r.events(EventBreach)) != 1 {
		t.Fatalf("want one breach event, got %d", len(r.events(EventBreach)))
	}

	r.tick(hog()) // t=20s: throttle hold (5 s) over; cooldown (10 s) still running
	wantCalls(t, r, "throttle session:s1 40", "unthrottle session:s1")
	r.tick(hog()) // t=25s: cooldown over -> step 2
	r.tick(hog()) // t=30s: cooldown
	r.tick(hog()) // t=35s -> step 3 (the offender is a session, so the agent unit restarts)
	r.tick(hog()) // t=40s: cooldown
	r.tick(hog()) // t=45s -> step 4
	wantCalls(t, r,
		"throttle session:s1 40", "unthrottle session:s1",
		"kill session:s1",
		"restart fleet-agent.service",
		"reboot")

	// The reboot cap (1/day) is used up: the next due step latches alert-only.
	r.tick(hog()) // t=50s
	r.tick(hog()) // t=55s: cooldown over, would reboot again
	if got := len(r.events(EventAlertOnly)); got != 1 {
		t.Fatalf("want one alert-only event, got %d", got)
	}
	if !r.w.Guards().Snapshot().AlertOnly {
		t.Fatal("alert-only must be latched")
	}
	n := len(r.act.all())
	for i := 0; i < 20; i++ {
		r.tick(hog())
	}
	if len(r.act.all()) != n {
		t.Fatalf("alert-only must not act further: %q", r.act.all()[n:])
	}
}

// Hysteresis and anti-flap on a watched service.
func TestSustainFlapAndHysteresis(t *testing.T) {
	r := newRig(t, "", t0.Add(-24*time.Hour))
	r.apply(fastPolicy + "services:\n  web.service:\n    cpu_percent: {trigger: 80, recover: 60}\n")
	svc := func(cpu float64) Sample {
		return Sample{Services: map[string]UsageMetrics{"web.service": {Present: true, CPUPercent: cpu}}}
	}

	// Three high samples, one low, three high: never 4 in a row, never a breach.
	for _, v := range []float64{85, 85, 85, 40, 85, 85, 85} {
		r.tick(svc(v))
	}
	wantCalls(t, r)
	if len(r.events(EventBreach)) != 0 {
		t.Fatal("a flapping signal must not count as a sustained breach")
	}

	r.tick(svc(85)) // the 4th consecutive high sample
	if got := len(r.events(EventBreach)); got != 1 {
		t.Fatalf("want a breach after 4 consecutive samples, got %d", got)
	}
	wantCalls(t, r, "throttle service:web.service 40")

	// In the hysteresis band (recover <= v < trigger) the breach holds.
	r.tick(svc(70))
	r.tick(svc(61))
	if len(r.events(EventRecovered)) != 0 {
		t.Fatal("must not clear above the recovery level")
	}
	// Below recovery: clears.
	r.tick(svc(50))
	if len(r.events(EventRecovered)) != 1 {
		t.Fatal("want recovered below the recovery level")
	}
}

func TestThrottleLiftedEarlyOnRecovery(t *testing.T) {
	r := newRig(t, "", t0.Add(-24*time.Hour))
	r.apply("version: 1\nboot_window: 0s\nthrottle: {duration: 120s, cpu_percent: 40}\n")
	for i := 0; i < 4; i++ {
		r.tick(hog())
	}
	wantCalls(t, r, "throttle session:s1 40")
	r.tick(hog()) // still held: 120 s hold, breach continues
	wantCalls(t, r, "throttle session:s1 40")
	r.tick(idle())
	wantCalls(t, r, "throttle session:s1 40", "unthrottle session:s1")
}

// Cooldown after a throttle is 10 minutes by default.
func TestCooldownAfterThrottle(t *testing.T) {
	r := newRig(t, "", t0.Add(-24*time.Hour))
	r.apply("version: 1\nboot_window: 0s\n")
	for i := 0; i < 4; i++ {
		r.tick(hog())
	}
	wantCalls(t, r, "throttle session:s1 50")
	for r.now.Before(t0.Add(15*time.Second + 10*time.Minute)) { // up to just before throttle + 10 min
		r.tick(hog())
	}
	for _, c := range r.act.all() {
		if strings.HasPrefix(c, "kill") {
			t.Fatalf("escalated inside the 10 min cooldown: %q", r.act.all())
		}
	}
	r.tick(hog()) // at throttle + 10 min
	got := r.act.all()
	if got[len(got)-1] != "kill session:s1" {
		t.Fatalf("want kill after the cooldown, got %q", got)
	}
}

// The counters and cooldowns survive a (simulated) reboot, and the exhausted cap
// turns into alert-only that also survives.
func TestCapsAndCooldownsPersistAcrossReboot(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir, t0.Add(-24*time.Hour))
	r.apply("version: 1\nboot_window: 0s\nstage_reset: 1h\ncaps: {restarts_per_day: 1, reboots_per_day: 1}\n" +
		"steps: {throttle: false, kill_session: false, restart: true, reboot: false}\n" +
		"cooldowns: {throttle: 1m, kill_session: 1m, restart: 30m, reboot: 60m}\n")
	for i := 0; i < 4; i++ {
		r.tick(hog())
	}
	wantCalls(t, r, "restart fleet-agent.service")
	restartedAt := r.now.Add(-5 * time.Second)

	// --- reboot: a new process over the same data dir ---
	r2 := newRig(t, dir, r.now)
	r2.now = r.now
	if st := r2.w.Guards().Snapshot(); st.Restarts != 1 || !st.LastRestart.Equal(restartedAt) {
		t.Fatalf("counters did not persist: %+v", st)
	}
	if r2.w.PolicyVersion() != 1 {
		t.Fatalf("last-known policy not reloaded, version %d", r2.w.PolicyVersion())
	}
	// The 30 min restart cooldown still holds after the reboot.
	for i := 0; i < 4; i++ {
		r2.tick(hog())
	}
	wantCalls(t, r2)
	// After the cooldown the daily cap (1) is exhausted -> alert-only, no action.
	r2.now = restartedAt.Add(31 * time.Minute)
	for i := 0; i < 4; i++ {
		r2.tick(hog())
	}
	wantCalls(t, r2)
	if len(r2.events(EventAlertOnly)) != 1 || !r2.w.Guards().Snapshot().AlertOnly {
		t.Fatalf("want alert-only latched; events %+v", r2.ev)
	}

	// A second reboot: alert-only itself persisted.
	r3 := newRig(t, dir, r2.now)
	r3.now = r2.now
	if !r3.w.Guards().Snapshot().AlertOnly {
		t.Fatal("alert-only did not persist across reboot")
	}
	for i := 0; i < 6; i++ {
		r3.tick(hog())
	}
	wantCalls(t, r3)

	// Midnight resets the daily counters and clears alert-only (auto_reset_midnight).
	r3.now = time.Date(2026, 11, 25, 0, 5, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		r3.tick(hog())
	}
	wantCalls(t, r3, "restart fleet-agent.service")
	if r3.w.Guards().Snapshot().Restarts != 1 {
		t.Fatalf("counter after midnight: %+v", r3.w.Guards().Snapshot())
	}
}

func TestRebootCounterPersistsAndOperatorReset(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir, t0.Add(-24*time.Hour))
	r.apply("version: 1\nboot_window: 0s\nsteps: {throttle: false, kill_session: false, restart: false, reboot: true}\n" +
		"cooldowns: {throttle: 1m, kill_session: 1m, restart: 1m, reboot: 60m}\n")
	for i := 0; i < 4; i++ {
		r.tick(hog())
	}
	wantCalls(t, r, "reboot")

	r2 := newRig(t, dir, r.now.Add(-time.Minute))
	r2.now = r.now
	if st := r2.w.Guards().Snapshot(); st.Reboots != 1 {
		t.Fatalf("reboot counter lost: %+v", st)
	}
	r2.now = r2.now.Add(2 * time.Hour) // past the 60 min cooldown, same day
	for i := 0; i < 4; i++ {
		r2.tick(hog())
	}
	wantCalls(t, r2) // the 1/day cap is spent -> alert-only
	if !r2.w.Guards().Snapshot().AlertOnly {
		t.Fatal("want alert-only")
	}

	// Operator reset (panel button) clears counters and alert-only.
	if err := r2.w.ResetCounters(r2.now); err != nil {
		t.Fatal(err)
	}
	st := r2.w.Guards().Snapshot()
	if st.AlertOnly || st.Reboots != 0 {
		t.Fatalf("reset left state %+v", st)
	}
	for i := 0; i < 4; i++ {
		r2.tick(hog())
	}
	wantCalls(t, r2, "reboot")
}

func TestBootStabilityWindow(t *testing.T) {
	boot := t0.Add(-time.Minute)
	r := newRig(t, "", boot)
	// Default boot window is 5 min.
	r.apply("version: 1\nsteps: {throttle: true, kill_session: false, restart: false, reboot: false}\n")
	for i := 0; i < 20; i++ { // up to ~100 s after t0, still inside 5 min of boot
		r.tick(hog())
	}
	wantCalls(t, r)
	if len(r.events(EventSuppressed)) != 1 {
		t.Fatalf("want one suppressed notice (not one per sample), got %d", len(r.events(EventSuppressed)))
	}
	r.now = boot.Add(5*time.Minute + time.Second)
	r.tick(hog())
	wantCalls(t, r, "throttle session:s1 50")
}

func TestForcedAlertOnlyPolicy(t *testing.T) {
	r := newRig(t, "", t0.Add(-24*time.Hour))
	r.apply("version: 1\nboot_window: 0s\nalert_only: true\n")
	for i := 0; i < 10; i++ {
		r.tick(hog())
	}
	wantCalls(t, r)
	if len(r.events(EventBreach)) != 1 {
		t.Fatal("alert-only must still detect and report the breach")
	}
}

func TestInapplicableStepsAreSkipped(t *testing.T) {
	r := newRig(t, "", t0.Add(-24*time.Hour))
	r.act.notApplic = map[string]bool{"throttle": true}
	r.apply("version: 1\nboot_window: 0s\n")
	for i := 0; i < 4; i++ {
		r.tick(hog())
	}
	// Throttle is not applicable: the same sample goes on to kill the session.
	wantCalls(t, r, "kill session:s1")

	// A memory breach is not answered by a CPU quota: it goes straight to kill.
	r2 := newRig(t, "", t0.Add(-24*time.Hour))
	r2.apply("version: 1\nboot_window: 0s\n")
	mem := Sample{
		Machine:  MachineMetrics{MemoryPercent: 99},
		Sessions: []SessionUsage{{ID: "m1", CgroupPath: "/x/m1", UsageMetrics: UsageMetrics{Present: true, MemoryPercent: 70}}},
	}
	for i := 0; i < 4; i++ {
		r2.tick(mem)
	}
	wantCalls(t, r2, "kill session:m1")
}

func TestUnattributedMachineBreachRestartsAgent(t *testing.T) {
	r := newRig(t, "", t0.Add(-24*time.Hour))
	r.apply("version: 1\nboot_window: 0s\n")
	for i := 0; i < 4; i++ { // CPU breach, no session or service holds half of it
		r.tick(Sample{Machine: MachineMetrics{CPUPercent: 99}})
	}
	// No throttle (machine target, not egress) and no kill: first applicable rung is the restart.
	wantCalls(t, r, "restart fleet-agent.service")
}

func TestDiskBreachIsAlertOnly(t *testing.T) {
	r := newRig(t, "", t0.Add(-24*time.Hour))
	r.apply("version: 1\nboot_window: 0s\n")
	for i := 0; i < 8; i++ {
		r.tick(Sample{Machine: MachineMetrics{DiskPercent: 99}})
	}
	wantCalls(t, r)
	if len(r.events(EventBreach)) != 1 {
		t.Fatal("disk breach must be reported")
	}
}

func TestRebootFailureStillCounts(t *testing.T) {
	r := newRig(t, "", t0.Add(-24*time.Hour))
	r.act.rebootFail = errors.New("boom")
	r.apply("version: 1\nboot_window: 0s\nsteps: {throttle: false, kill_session: false, restart: false, reboot: true}\n")
	for i := 0; i < 4; i++ {
		r.tick(hog())
	}
	if len(r.events(EventFailed)) != 1 || r.w.Guards().Snapshot().Reboots != 1 {
		t.Fatalf("failed reboot must be reported and counted: %+v", r.w.Guards().Snapshot())
	}
}

func TestCorruptCountersStartAlertOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, WatchdogSubdir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(dir), []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, dir, t0.Add(-24*time.Hour))
	if !r.w.Guards().Snapshot().AlertOnly {
		t.Fatal("lost counters must fail safe to alert-only")
	}
	if _, err := os.Stat(statePath(dir) + ".corrupt"); err != nil {
		t.Fatalf("corrupt file not kept aside: %v", err)
	}
}

// A pushed YAML policy applies live and persists; invalid ones are refused and the
// last-known keeps running.
func TestPolicyPushAppliedLiveAndInvalidRejected(t *testing.T) {
	r := newRig(t, "", t0.Add(-24*time.Hour))
	if r.w.PolicyVersion() != 0 {
		t.Fatalf("first boot must run the baked-in default, got v%d", r.w.PolicyVersion())
	}
	push := func(version int, yaml string) channel.PolicyAckPayload {
		t.Helper()
		env, err := channel.NewEnvelope(channel.TypePolicy, "p1", channel.PolicyPayload{Version: version, YAML: yaml})
		if err != nil {
			t.Fatal(err)
		}
		reply, ok := r.w.HandleEnvelope(env)
		if !ok || reply.Type != channel.TypePolicyAck || reply.ID != "p1" {
			t.Fatalf("bad reply %+v ok=%v", reply, ok)
		}
		var ack channel.PolicyAckPayload
		if err := json.Unmarshal(reply.Payload, &ack); err != nil {
			t.Fatal(err)
		}
		return ack
	}

	good := "version: 2\nboot_window: 0s\nservices:\n  web.service:\n    cpu_percent: {trigger: 70, recover: 50}\n"
	if ack := push(2, good); !ack.OK || ack.Version != 2 {
		t.Fatalf("valid policy refused: %+v", ack)
	}
	// Live: no restart of the watchdog, and the new service threshold is in force.
	if _, ok := r.w.Engine().Policy().Services["web.service"]; !ok || r.w.PolicyVersion() != 2 {
		t.Fatal("pushed policy not applied live")
	}
	web := func(v float64) Sample {
		return Sample{Services: map[string]UsageMetrics{"web.service": {Present: true, CPUPercent: v}}}
	}
	for i := 0; i < 4; i++ {
		r.tick(web(75))
	}
	wantCalls(t, r, "throttle service:web.service 50")

	before := r.w.Engine().Policy()
	for name, bad := range map[string]string{
		"malformed":         "version: 3\n  : :",
		"unknown key":       "version: 3\nbogus: 1\n",
		"no version":        "boot_window: 0s\n",
		"bad duration":      "version: 3\nsustain: soon\n",
		"hysteresis":        "version: 3\nmachine:\n  cpu_percent: {trigger: 99, recover: 99.5}\n",
		"below LC ceiling":  "version: 3\nmachine:\n  cpu_percent: {trigger: 85, recover: 80}\n",
		"watch self":        "version: 3\nservices:\n  fleet-watchdog.service:\n    cpu_percent: {trigger: 50, recover: 30}\n",
		"invalid override":  "version: 3\noverrides:\n  hosts:\n    w1:\n      throttle: {cpu_percent: 0}\n",
		"override version":  "version: 3\noverrides:\n  groups:\n    edge:\n      version: 9\n",
		"sustain < sample":  "version: 3\nsample_interval: 10s\nsustain: 5s\n",
		"stale version":     "version: 1\n",
		"flow type mistake": "version: 3\nsteps: [throttle]\n",
	} {
		if ack := push(3, bad); ack.OK || ack.Error == "" || ack.Version != 2 {
			t.Errorf("%s: want refusal with version 2 still in force, got %+v", name, ack)
		}
	}
	if r.w.PolicyVersion() != 2 {
		t.Fatalf("an invalid push replaced the last-known policy: v%d", r.w.PolicyVersion())
	}
	after := r.w.Engine().Policy()
	if after.Version != before.Version || len(after.Services) != len(before.Services) {
		t.Fatal("running policy changed by a rejected push")
	}
	if got := len(r.events(EventPolicy)); got < 2 {
		t.Fatalf("policy events not reported: %d", got)
	}

	// The persisted last-known policy is still the good one after a "reboot".
	r2 := newRig(t, r.dir, t0)
	if r2.w.PolicyVersion() != 2 {
		t.Fatalf("last-known policy not reloaded after reboot: v%d", r2.w.PolicyVersion())
	}
}

func TestPolicyFromDiskAppliedLive(t *testing.T) {
	// Process split: the channel holder stores the policy; the watchdog loop picks it up.
	dir := t.TempDir()
	fs := &fakeSampler{}
	w, err := New(Config{DataDir: dir, MachineID: "w1", Sampler: fs, Actuator: &fakeAct{}, Log: quietLog(), BootTime: func() time.Time { return t0.Add(-time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StorePolicy(dir, []byte("version: 4\nboot_window: 0s\nsample_interval: 2s\nsustain: 8s\n"), -1); err != nil {
		t.Fatal(err)
	}
	fs.set(idle())
	w.Step(context.Background())
	if w.PolicyVersion() != 4 || w.Engine().Policy().SustainSamples() != 4 || w.Engine().Policy().SampleInterval.D() != 2*time.Second {
		t.Fatalf("policy on disk not applied: v%d %+v", w.PolicyVersion(), w.Engine().Policy().SampleInterval)
	}
	// An invalid file on disk is ignored (reported once) and the last-known keeps running.
	if err := os.WriteFile(policyPath(dir), []byte("version: 5\nnope: 1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	w.Step(context.Background())
	w.Step(context.Background())
	if w.PolicyVersion() != 4 {
		t.Fatalf("invalid on-disk policy applied: v%d", w.PolicyVersion())
	}
	n := 0
	for _, e := range w.Engine().Events() {
		if e.Kind == EventPolicy && strings.HasPrefix(e.Detail, "rejected") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want the rejection reported once, got %d", n)
	}
	// StorePolicy refuses invalid and stale input.
	if _, err := StorePolicy(dir, []byte("version: 6\nnope: 1\n"), -1); err == nil {
		t.Fatal("invalid policy must not be stored")
	}
	if _, err := StorePolicy(dir, []byte("version: 1\n"), 7); !errors.Is(err, ErrStalePolicy) {
		t.Fatalf("want ErrStalePolicy, got %v", err)
	}
}

func TestPerHostAndGroupOverridesApply(t *testing.T) {
	r := newRig(t, "", t0) // machine w1, group edge
	r.apply(`version: 7
sustain: 20s
overrides:
  groups:
    edge:
      throttle: {cpu_percent: 30}
      caps: {restarts_per_day: 5}
  hosts:
    w1:
      caps: {restarts_per_day: 2}
    other:
      caps: {restarts_per_day: 9}
`)
	p := r.w.Engine().Policy()
	if p.Throttle.CPUPercent != 30 {
		t.Errorf("group override not applied: %v", p.Throttle.CPUPercent)
	}
	if p.Caps.RestartsPerDay != 2 { // the host override wins over the group's
		t.Errorf("host override must beat group: %d", p.Caps.RestartsPerDay)
	}
	if p.Throttle.Duration.D() != 120*time.Second || p.Caps.RebootsPerDay != 1 {
		t.Errorf("untouched keys must keep defaults: %+v", p)
	}
}

func TestRunLoopSamplesAndStops(t *testing.T) {
	dir := t.TempDir()
	fs := &fakeSampler{}
	act := &fakeAct{}
	w, err := New(Config{DataDir: dir, MachineID: "w1", Sampler: fs, Actuator: act, Log: quietLog(), BootTime: func() time.Time { return time.Now().Add(-time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.ApplyPolicy([]byte("version: 1\nsample_interval: 100ms\nsustain: 200ms\nboot_window: 0s\n")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := hog()
		s.Time = time.Now()
		fs.set(s)
		if len(act.all()) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	if got := act.all(); len(got) == 0 || got[0] != "throttle session:s1 50" {
		t.Fatalf("Run loop did not act on a sustained breach: %q", got)
	}
}

// The default (first-boot) thresholds sit above the Load Controller ceiling, and a
// Maneuver sitting at its ceiling never starts a breach.
func TestDefaultsSitAboveLoadControllerCeiling(t *testing.T) {
	p := DefaultPolicy()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Machine.CPUPercent.Trigger <= p.LoadController.CPUPercent || p.Machine.MemoryPercent.Trigger <= p.LoadController.MemoryPercent {
		t.Fatalf("default triggers not above the ceiling: %+v %+v", p.Machine, p.LoadController)
	}
	r := newRig(t, "", t0.Add(-24*time.Hour))
	for i := 0; i < 60; i++ {
		r.tick(Sample{Machine: MachineMetrics{CPUPercent: p.LoadController.CPUPercent, MemoryPercent: p.LoadController.MemoryPercent}})
	}
	wantCalls(t, r)
	if len(r.events(EventBreach)) != 0 {
		t.Fatal("intended Maneuver load at the ceiling must never count as a breach")
	}
}

func TestSetGroupsReResolvesPolicyLive(t *testing.T) {
	r := newRig(t, "", t0)
	r.apply(`version: 8
caps: {restarts_per_day: 3}
throttle: {cpu_percent: 50}
overrides:
  groups:
    edge:
      throttle: {cpu_percent: 30}
      caps: {restarts_per_day: 5}
    gpu:
      caps: {restarts_per_day: 7}
  hosts:
    w1:
      caps: {restarts_per_day: 2}
`)
	// Initial rig has Groups: ["edge"], host w1 wins on restarts_per_day (2), edge applies on throttle (30).
	if p := r.w.Engine().Policy(); p.Throttle.CPUPercent != 30 || p.Caps.RestartsPerDay != 2 {
		t.Fatalf("initial resolved policy = throttle:%v restarts:%d", p.Throttle.CPUPercent, p.Caps.RestartsPerDay)
	}
	// Mutating returned Groups slice does not affect Watchdog.
	gs := r.w.Groups()
	gs[0] = "tampered"
	if got := r.w.Groups(); len(got) != 1 || got[0] != "edge" {
		t.Fatalf("Groups() leaked internal slice: %v", got)
	}
	// Dropping "edge" reverts throttle to default (50) while keeping host override (2).
	if err := r.w.SetGroups([]string{"gpu"}); err != nil {
		t.Fatal(err)
	}
	if p := r.w.Engine().Policy(); p.Throttle.CPUPercent != 50 || p.Caps.RestartsPerDay != 2 {
		t.Fatalf("after SetGroups([gpu]) = throttle:%v restarts:%d", p.Throttle.CPUPercent, p.Caps.RestartsPerDay)
	}
	// A non-host-overridden machine resolves group > default and later-group-wins.
	w2, err := New(Config{
		DataDir: t.TempDir(), MachineID: "w2", Groups: []string{"edge", "gpu"},
		Sampler: &fakeSampler{}, Actuator: &fakeAct{}, Log: quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.ApplyPolicy([]byte(`version: 8
caps: {restarts_per_day: 3}
throttle: {cpu_percent: 50}
overrides:
  groups:
    edge:
      throttle: {cpu_percent: 30}
      caps: {restarts_per_day: 5}
    gpu:
      caps: {restarts_per_day: 7}
`)); err != nil {
		t.Fatal(err)
	}
	if p := w2.Engine().Policy(); p.Throttle.CPUPercent != 30 || p.Caps.RestartsPerDay != 7 {
		t.Fatalf("w2 group resolution = throttle:%v restarts:%d, want 30 and 7", p.Throttle.CPUPercent, p.Caps.RestartsPerDay)
	}
}
