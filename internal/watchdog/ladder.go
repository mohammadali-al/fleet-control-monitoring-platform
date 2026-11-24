package watchdog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Metric names, as used in policy YAML, events and detector keys.
const (
	MetricCPU    = "cpu_percent"
	MetricMemory = "memory_percent"
	MetricLoad   = "load_per_cpu"
	MetricDisk   = "disk_percent"
	MetricEgress = "egress_mbps"
)

// ScopeMachine is the breach scope for machine-level metrics; a service breach
// uses its unit name as scope.
const ScopeMachine = "machine"

// Target kinds.
const (
	TargetMachine = "machine"
	TargetService = "service"
	TargetSession = "session"
)

// Target is what an action is aimed at: the whole machine, one systemd unit,
// or one command session's cgroup.
type Target struct {
	Kind       string
	Name       string // unit name (service) or session id (session)
	CgroupPath string // session cgroup directory (session)
}

func (t Target) String() string {
	if t.Kind == TargetMachine {
		return TargetMachine
	}
	return t.Kind + ":" + t.Name
}

// ThrottleSpec is what step 1 applies.
type ThrottleSpec struct {
	// CPUPercent is the CPU quota as percent of the whole machine.
	CPUPercent float64
	// EgressKbit, when > 0, also shapes egress (tc tbf) for a machine target.
	EgressKbit int
}

// ErrNotApplicable is returned by an Actuator method that cannot act on the
// given target (for example the egress shaper without a usable interface). The
// ladder then moves on to the next rung without counting the step.
var ErrNotApplicable = errors.New("watchdog: action not applicable to target")

// Actuator performs every system action the ladder can take. Keeping them
// behind one interface lets the decision logic run, and be tested, without root
// and without touching the machine. SystemActuator is the real implementation.
type Actuator interface {
	// Throttle applies a transient CPU quota (and optional egress shaping).
	Throttle(ctx context.Context, t Target, spec ThrottleSpec) error
	// Unthrottle lifts a throttle applied earlier.
	Unthrottle(ctx context.Context, t Target) error
	// KillSession kills a session's whole cgroup in one action.
	KillSession(ctx context.Context, t Target) error
	// RestartService restarts a systemd unit.
	RestartService(ctx context.Context, unit string) error
	// Reboot reboots the machine.
	Reboot(ctx context.Context) error
}

// Event kinds.
const (
	EventBreach     = "breach"    // a sustained breach began
	EventRecovered  = "recovered" // a breach cleared (fell below recovery)
	EventAction     = "action"    // a ladder step was taken
	EventFailed     = "action-failed"
	EventSuppressed = "suppressed"  // a due action was held back (cooldown, boot window, ...)
	EventAlertOnly  = "alert-only"  // caps exhausted (or forced): measuring and notifying only
	EventLifted     = "unthrottled" // a throttle ended
	EventDay        = "day-rollover"
	EventPolicy     = "policy"
	EventCounters   = "counters-reset"
)

// Event is one thing the watchdog reports (to the log and, once the panel is
// wired, to the panel).
type Event struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	Scope  string    `json:"scope,omitempty"`  // breach scope
	Metric string    `json:"metric,omitempty"` // breached metric
	Value  float64   `json:"value,omitempty"`
	Target string    `json:"target,omitempty"`
	Step   string    `json:"step,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// breach is one detector currently in the breached state.
type breach struct {
	Scope    string
	Metric   string
	Value    float64
	Trigger  float64
	Severity float64 // value / trigger
}

// detector implements the anti-flap rule for one (scope, metric): a breach must
// be seen on need consecutive samples before it counts, and once it counts it
// clears only when the value falls below the recovery level (hysteresis).
type detector struct {
	over   int
	active bool
}

// update feeds one reading and reports "breach" on the sample that confirms a
// sustained breach, "recovered" on the sample that clears it, "" otherwise.
func (d *detector) update(v float64, th Threshold, need int) string {
	if d.active {
		if v < th.Recover {
			d.active, d.over = false, 0
			return EventRecovered
		}
		return ""
	}
	if v >= th.Trigger {
		d.over++
		if d.over >= need {
			d.active = true
			return EventBreach
		}
		return ""
	}
	d.over = 0
	return ""
}

type activeThrottle struct {
	target Target
	until  time.Time
	cause  string // breach key that caused it
}

// Engine is the decision core: it consumes samples, runs the detectors and, when
// a breach is sustained and the guards allow, takes the next ladder step through
// the Actuator. It does no I/O of its own.
type Engine struct {
	mu       sync.Mutex
	log      *slog.Logger
	act      Actuator
	guards   *Guards
	policy   Policy
	bootTime func() time.Time
	notify   func(Event)

	det        map[string]*detector
	throttled  []activeThrottle
	lastActive time.Time
	lastNote   string
	pending    []Event
	recent     []Event
}

const recentEvents = 100

// NewEngine builds an Engine. bootTime may be nil (no boot window); notify may
// be nil. notify is called after the engine's lock is released but must still
// not block for long.
func NewEngine(p Policy, act Actuator, g *Guards, bootTime func() time.Time, notify func(Event), log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{log: log, act: act, guards: g, policy: p, bootTime: bootTime, notify: notify, det: map[string]*detector{}}
}

// SetPolicy swaps the policy live. Detector progress is kept, so a policy push
// does not reset a breach in progress.
func (e *Engine) SetPolicy(p Policy) {
	e.mu.Lock()
	e.policy = p
	e.mu.Unlock()
}

// Policy returns the policy in force.
func (e *Engine) Policy() Policy {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.policy
}

// Events returns the most recent events, oldest first.
func (e *Engine) Events() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Event(nil), e.recent...)
}

// Throttled lists the targets currently throttled.
func (e *Engine) Throttled() []Target {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Target
	for _, t := range e.throttled {
		out = append(out, t.target)
	}
	return out
}

// Emit records an event raised outside Observe (policy changes, counter resets).
func (e *Engine) Emit(ev Event) {
	e.mu.Lock()
	e.emitLocked(ev)
	evs := e.drainLocked()
	e.mu.Unlock()
	e.dispatch(evs)
}

func (e *Engine) emitLocked(ev Event) {
	e.pending = append(e.pending, ev)
	e.recent = append(e.recent, ev)
	if len(e.recent) > recentEvents {
		e.recent = e.recent[len(e.recent)-recentEvents:]
	}
	e.log.Info("watchdog event", "component", "fleet-watchdog", "kind", ev.Kind, "scope", ev.Scope,
		"metric", ev.Metric, "value", ev.Value, "target", ev.Target, "step", ev.Step, "detail", ev.Detail)
}

func (e *Engine) drainLocked() []Event {
	evs := e.pending
	e.pending = nil
	return evs
}

func (e *Engine) dispatch(evs []Event) {
	if e.notify == nil {
		return
	}
	for _, ev := range evs {
		e.notify(ev)
	}
}

// Observe processes one sample: detect, lift expired throttles, and take at most
// one ladder step.
func (e *Engine) Observe(ctx context.Context, s Sample) {
	e.mu.Lock()
	e.observeLocked(ctx, s)
	evs := e.drainLocked()
	e.mu.Unlock()
	e.dispatch(evs)
}

func (e *Engine) observeLocked(ctx context.Context, s Sample) {
	p := e.policy
	now := s.Time
	if e.guards.Roll(now, p.AutoResetMidnight) {
		e.emitLocked(Event{Time: now, Kind: EventDay, Detail: "daily counters reset"})
	}
	active := e.detect(s, p)
	e.liftThrottles(ctx, now, active)

	var ladderBreaches []breach
	for _, b := range active {
		if b.Metric != MetricDisk { // disk is alert-only: no ladder step frees disk
			ladderBreaches = append(ladderBreaches, b)
		}
	}
	if len(ladderBreaches) == 0 {
		e.lastNote = ""
		if st := e.guards.Snapshot(); st.Stage != StepNone {
			if e.lastActive.IsZero() {
				e.lastActive = now // restarted mid-episode: count the clear time from now
			}
			if now.Sub(e.lastActive) >= p.StageReset.D() {
				if err := e.guards.ResetStage(); err != nil {
					e.log.Warn("reset ladder stage", "component", "fleet-watchdog", "err", err)
				}
			}
		}
		return
	}
	e.lastActive = now
	sort.Slice(ladderBreaches, func(i, j int) bool { return ladderBreaches[i].Severity > ladderBreaches[j].Severity })
	e.escalate(ctx, s, p, ladderBreaches[0])
}

// detect updates every detector from the sample and returns the active breaches.
func (e *Engine) detect(s Sample, p Policy) []breach {
	need := p.SustainSamples()
	seen := map[string]bool{}
	var active []breach
	feed := func(scope, metric string, th Threshold, v float64, present bool) {
		if !th.Enabled() {
			return
		}
		key := scope + "|" + metric
		seen[key] = true
		d := e.det[key]
		if d == nil {
			d = &detector{}
			e.det[key] = d
		}
		if !present {
			if d.active {
				e.emitLocked(Event{Time: s.Time, Kind: EventRecovered, Scope: scope, Metric: metric, Detail: "unit not running"})
			}
			d.active, d.over = false, 0
			return
		}
		switch d.update(v, th, need) {
		case EventBreach:
			e.emitLocked(Event{Time: s.Time, Kind: EventBreach, Scope: scope, Metric: metric, Value: v,
				Detail: fmt.Sprintf("sustained %d samples at or above %v", need, th.Trigger)})
		case EventRecovered:
			e.emitLocked(Event{Time: s.Time, Kind: EventRecovered, Scope: scope, Metric: metric, Value: v,
				Detail: fmt.Sprintf("below recovery level %v", th.Recover)})
		}
		if d.active {
			active = append(active, breach{Scope: scope, Metric: metric, Value: v, Trigger: th.Trigger, Severity: v / th.Trigger})
		}
	}
	m, t := s.Machine, p.Machine
	feed(ScopeMachine, MetricCPU, t.CPUPercent, m.CPUPercent, true)
	feed(ScopeMachine, MetricMemory, t.MemoryPercent, m.MemoryPercent, true)
	feed(ScopeMachine, MetricLoad, t.LoadPerCPU, m.LoadPerCPU, true)
	feed(ScopeMachine, MetricDisk, t.DiskPercent, m.DiskPercent, true)
	feed(ScopeMachine, MetricEgress, t.EgressMbps, m.EgressMbps, true)
	for unit, st := range p.Services {
		u := s.Services[unit]
		feed(unit, MetricCPU, st.CPUPercent, u.CPUPercent, u.Present)
		feed(unit, MetricMemory, st.MemoryPercent, u.MemoryPercent, u.Present)
	}
	for key := range e.det {
		if !seen[key] {
			delete(e.det, key)
		}
	}
	return active
}

func (e *Engine) liftThrottles(ctx context.Context, now time.Time, active []breach) {
	activeKeys := map[string]bool{}
	for _, b := range active {
		activeKeys[b.Scope+"|"+b.Metric] = true
	}
	kept := e.throttled[:0]
	for _, th := range e.throttled {
		reason := ""
		switch {
		case !now.Before(th.until):
			reason = "hold time elapsed"
		case !activeKeys[th.cause]:
			reason = "recovered"
		}
		if reason == "" {
			kept = append(kept, th)
			continue
		}
		if err := e.act.Unthrottle(ctx, th.target); err != nil {
			e.emitLocked(Event{Time: now, Kind: EventFailed, Target: th.target.String(), Step: "unthrottle", Detail: err.Error()})
			kept = append(kept, th) // retry next sample
			continue
		}
		e.emitLocked(Event{Time: now, Kind: EventLifted, Target: th.target.String(), Detail: reason})
	}
	e.throttled = kept
}

// chooseTarget scopes the action to the offender where it can be attributed: a
// service breach targets that service; a machine CPU/load/memory breach targets
// the session (else watched service) holding at least AttributionShare of it;
// anything else targets the machine.
func chooseTarget(s Sample, p Policy, b breach) Target {
	if b.Scope != ScopeMachine {
		return Target{Kind: TargetService, Name: b.Scope}
	}
	var machineVal float64
	pick := func(u UsageMetrics) float64 { return u.CPUPercent }
	switch b.Metric {
	case MetricCPU, MetricLoad:
		machineVal = s.Machine.CPUPercent
	case MetricMemory:
		machineVal = s.Machine.MemoryPercent
		pick = func(u UsageMetrics) float64 { return u.MemoryPercent }
	default:
		return Target{Kind: TargetMachine}
	}
	if machineVal <= 0 {
		return Target{Kind: TargetMachine}
	}
	need := p.AttributionShare * machineVal
	var best *SessionUsage
	for i := range s.Sessions {
		if v := pick(s.Sessions[i].UsageMetrics); v > 0 && v >= need && (best == nil || v > pick(best.UsageMetrics)) {
			best = &s.Sessions[i]
		}
	}
	if best != nil {
		return Target{Kind: TargetSession, Name: best.ID, CgroupPath: best.CgroupPath}
	}
	bestUnit, bestVal := "", 0.0
	for unit := range p.Services {
		if v := pick(s.Services[unit]); v > 0 && v >= need && v > bestVal {
			bestUnit, bestVal = unit, v
		}
	}
	if bestUnit != "" {
		return Target{Kind: TargetService, Name: bestUnit}
	}
	return Target{Kind: TargetMachine}
}

func stepEnabled(p Policy, st Step) bool {
	switch st {
	case StepThrottle:
		return p.Steps.Throttle
	case StepKillSession:
		return p.Steps.KillSession
	case StepRestart:
		return p.Steps.Restart
	case StepReboot:
		return p.Steps.Reboot
	}
	return false
}

// stepApplicable says whether a rung can do anything for this target and metric.
// Throttling is a CPU quota (and egress shaping for the machine), so it does not
// answer a memory breach; killing needs a session; restart and reboot always apply.
func stepApplicable(p Policy, st Step, t Target, metric string) bool {
	switch st {
	case StepThrottle:
		if t.Kind == TargetMachine {
			return metric == MetricEgress && p.Throttle.EgressKbit > 0
		}
		return metric != MetricMemory
	case StepKillSession:
		return t.Kind == TargetSession
	default:
		return true
	}
}

// ladderOrder returns the rungs to try, in order: every enabled, applicable rung
// above the current stage; when there is none, the last applicable rung again
// (it stays subject to cooldowns and caps).
func ladderOrder(p Policy, stage Step, t Target, metric string) []Step {
	var all, above []Step
	for st := StepThrottle; st <= StepReboot; st++ {
		if !stepEnabled(p, st) || !stepApplicable(p, st, t, metric) {
			continue
		}
		all = append(all, st)
		if st > stage {
			above = append(above, st)
		}
	}
	if len(above) == 0 && len(all) > 0 {
		above = all[len(all)-1:]
	}
	return above
}

func (e *Engine) suppress(now time.Time, reason string, b breach, detail string) {
	if e.lastNote == reason {
		return
	}
	e.lastNote = reason
	e.emitLocked(Event{Time: now, Kind: EventSuppressed, Scope: b.Scope, Metric: b.Metric, Value: b.Value, Detail: detail})
}

// escalate takes the next ladder step for breach b, if the guards allow.
func (e *Engine) escalate(ctx context.Context, s Sample, p Policy, b breach) {
	now := s.Time
	st := e.guards.Snapshot()
	if st.AlertOnly || p.AlertOnly {
		e.suppress(now, "alert-only", b, "alert-only mode: measuring and notifying, no action taken")
		return
	}
	if e.bootTime != nil {
		if boot := e.bootTime(); !boot.IsZero() && now.Sub(boot) < p.BootWindow.D() {
			e.suppress(now, "boot-window", b, fmt.Sprintf("boot stability window (%s) not over", p.BootWindow.D()))
			return
		}
	}
	if rem := e.guards.CooldownRemaining(now, p.Cooldowns); rem > 0 {
		e.suppress(now, "cooldown", b, fmt.Sprintf("cooldown: %s left", rem.Round(time.Second)))
		return
	}
	target := chooseTarget(s, p, b)
	order := ladderOrder(p, st.Stage, target, b.Metric)
	if len(order) == 0 {
		e.suppress(now, "no-step", b, "no enabled ladder step applies")
		return
	}
	e.lastNote = "" // a due action is about to run: a later hold-back is news again
	for _, step := range order {
		done, stop := e.take(ctx, s, p, b, target, step)
		if done || stop {
			return
		}
	}
}

// take attempts one rung. done: it acted (or was refused for good); stop: do not
// try later rungs this sample. Neither: the rung did not apply, try the next.
func (e *Engine) take(ctx context.Context, s Sample, p Policy, b breach, target Target, step Step) (done, stop bool) {
	now := s.Time
	ev := Event{Time: now, Kind: EventAction, Scope: b.Scope, Metric: b.Metric, Value: b.Value, Target: target.String(), Step: step.String()}
	fail := func(err error) (bool, bool) {
		ev.Kind, ev.Detail = EventFailed, err.Error()
		e.emitLocked(ev)
		return true, true
	}
	switch step {
	case StepThrottle:
		spec := ThrottleSpec{CPUPercent: p.Throttle.CPUPercent}
		if target.Kind == TargetMachine {
			spec.EgressKbit = p.Throttle.EgressKbit
		}
		err := e.act.Throttle(ctx, target, spec)
		if errors.Is(err, ErrNotApplicable) {
			return false, false
		}
		if err != nil {
			return fail(err)
		}
		if rerr := e.guards.Record(step, now); rerr != nil {
			e.log.Warn("record throttle", "component", "fleet-watchdog", "err", rerr)
		}
		e.throttled = append(e.throttled, activeThrottle{target: target, until: now.Add(p.Throttle.Duration.D()), cause: b.Scope + "|" + b.Metric})
		ev.Detail = fmt.Sprintf("cpu quota %v%% of machine held %s", spec.CPUPercent, p.Throttle.Duration.D())
		e.emitLocked(ev)
		return true, true
	case StepKillSession:
		err := e.act.KillSession(ctx, target)
		if errors.Is(err, ErrNotApplicable) {
			return false, false
		}
		if err != nil {
			return fail(err)
		}
		if rerr := e.guards.Record(step, now); rerr != nil {
			e.log.Warn("record kill", "component", "fleet-watchdog", "err", rerr)
		}
		e.dropThrottle(target)
		ev.Detail = "session cgroup killed"
		e.emitLocked(ev)
		return true, true
	case StepRestart, StepReboot:
		if e.guards.CapExhausted(step, p.Caps) {
			if err := e.guards.SetAlertOnly(true); err != nil {
				e.log.Warn("latch alert-only", "component", "fleet-watchdog", "err", err)
			}
			limit := p.Caps.RestartsPerDay
			if step == StepReboot {
				limit = p.Caps.RebootsPerDay
			}
			e.emitLocked(Event{Time: now, Kind: EventAlertOnly, Scope: b.Scope, Metric: b.Metric, Value: b.Value, Step: step.String(),
				Detail: fmt.Sprintf("daily %s cap (%d) exhausted: alert-only until reset", step, limit)})
			return true, true
		}
		// Persist first: if the counters cannot be saved, do not act.
		if rerr := e.guards.Record(step, now); rerr != nil {
			return fail(fmt.Errorf("cannot persist counters, refusing to %s: %w", step, rerr))
		}
		var err error
		if step == StepRestart {
			unit := p.AgentUnit
			if target.Kind == TargetService {
				unit = target.Name
			}
			ev.Target = TargetService + ":" + unit
			err = e.act.RestartService(ctx, unit)
		} else {
			ev.Target = TargetMachine
			err = e.act.Reboot(ctx)
		}
		if err != nil {
			return fail(err) // the attempt stays counted: caps are conservative
		}
		e.throttled = nil // a restart or reboot discards cgroup throttles
		ev.Detail = "issued"
		e.emitLocked(ev)
		return true, true
	}
	return false, false
}

func (e *Engine) dropThrottle(t Target) {
	kept := e.throttled[:0]
	for _, th := range e.throttled {
		if th.target != t {
			kept = append(kept, th)
		}
	}
	e.throttled = kept
}
