// Package watchdog is the agent-side load-safety net (architecture chapter 11):
// it samples host and per-service resource use, detects sustained breaches, and
// climbs an escalation ladder (throttle, kill the offending session, restart the
// service, reboot) behind cooldowns, daily caps, a boot-stability window and an
// alert-only mode. It is a safety net, not a tamper-proof supervisor.
//
// The pieces: metrics.go (sampling), ladder.go (detection, ladder, Actuator),
// guards.go (cooldowns, caps, persisted counters), policy.go (versioned YAML
// profiles) and actuator.go (the real, root-only system actions). This file ties
// them into a running Watchdog.
package watchdog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
)

// Config wires a Watchdog.
type Config struct {
	// DataDir is the base data directory (D-0006); the policy and counters
	// live under <DataDir>/watchdog.
	DataDir string
	// MachineID and Groups select the per-host and per-group policy overrides.
	MachineID string
	Groups    []string
	// Sampler reads host metrics; Actuator performs system actions.
	Sampler  Sampler
	Actuator Actuator
	// BootTime returns when the machine booted (default: from /proc/uptime).
	// Only used for the boot-stability window.
	BootTime func() time.Time
	// Notify receives every event (breach, action, alert-only, ...). Optional.
	Notify func(Event)
	Log    *slog.Logger
}

// Watchdog is the running safety net.
type Watchdog struct {
	cfg    Config
	log    *slog.Logger
	guards *Guards
	engine *Engine

	mu       sync.Mutex
	groups   []string
	profile  *Profile
	applied  []byte // YAML text of the profile in force (nil for the baked-in default)
	rejected []byte // last on-disk profile that failed validation (log once)
}

// New builds a Watchdog: it loads the persisted guard counters and the
// last-known policy (falling back to the baked-in default when there is none or
// it no longer validates, so a bad file can never leave the host unprotected).
func New(cfg Config) (*Watchdog, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("watchdog: DataDir is required")
	}
	if cfg.Sampler == nil || cfg.Actuator == nil {
		return nil, errors.New("watchdog: Sampler and Actuator are required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	guards, err := LoadGuards(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.Groups = append([]string(nil), cfg.Groups...)
	w := &Watchdog{cfg: cfg, log: log, guards: guards, groups: append([]string(nil), cfg.Groups...)}
	if guards.Corrupt != nil {
		log.Error("watchdog counters unreadable; starting in alert-only mode until reset", "component", "fleet-watchdog", "err", guards.Corrupt)
	}

	pf, perr := LoadLastKnown(cfg.DataDir)
	if perr != nil {
		log.Error("last-known policy unusable; using the baked-in default", "component", "fleet-watchdog", "err", perr)
	}
	var pol Policy
	if pf != nil {
		pol, perr = pf.Effective(cfg.MachineID, w.groups)
		if perr != nil {
			log.Error("last-known policy does not resolve for this host; using the baked-in default", "component", "fleet-watchdog", "err", perr)
			pf = nil
		}
	}
	if pf == nil {
		pf = DefaultProfile()
		pol, _ = pf.Effective(cfg.MachineID, w.groups)
	}
	w.profile, w.applied = pf, pf.Raw

	boot := cfg.BootTime
	if boot == nil {
		boot = procBootTime
	}
	w.engine = NewEngine(pol, cfg.Actuator, guards, boot, cfg.Notify, log)
	log.Info("watchdog ready", "component", "fleet-watchdog", "policy_version", pf.Version, "machine", cfg.MachineID, "groups", w.groups)
	return w, nil
}

// procBootTime derives the boot time from /proc/uptime.
func procBootTime() time.Time {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}
	}
	f := strings.Fields(string(b))
	if len(f) < 1 {
		return time.Time{}
	}
	up, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return time.Time{}
	}
	return time.Now().Add(-time.Duration(up * float64(time.Second)))
}

// Engine exposes the decision engine (for tests and status).
func (w *Watchdog) Engine() *Engine { return w.engine }

// Guards exposes the persisted guards.
func (w *Watchdog) Guards() *Guards { return w.guards }

// PolicyVersion is the version of the profile in force (0 = baked-in default).
func (w *Watchdog) PolicyVersion() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.profile.Version
}

// Groups returns a copy of the watchdog policy-override groups in force.
func (w *Watchdog) Groups() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.groups...)
}

// SetGroups updates this host's policy-override groups and re-resolves the
// active profile live (precedence: host > group > default).
func (w *Watchdog) SetGroups(groups []string) error {
	next := append([]string(nil), groups...)
	w.mu.Lock()
	pf := w.profile
	pol, err := pf.Effective(w.cfg.MachineID, next)
	if err != nil {
		w.mu.Unlock()
		return fmt.Errorf("policy does not resolve for this host: %w", err)
	}
	w.groups = next
	w.cfg.Groups = append([]string(nil), next...)
	w.mu.Unlock()
	w.engine.SetPolicy(pol)
	w.engine.Emit(Event{Time: time.Now(), Kind: EventPolicy, Detail: fmt.Sprintf("groups updated (%v)", next)})
	return nil
}

// Step takes one sample and processes it. A policy change on disk (written by
// StorePolicy from the process that holds the channel) is picked up first, so a
// pushed profile applies live. A failed sample is logged and skipped: the
// watchdog never acts on data it could not read.
func (w *Watchdog) Step(ctx context.Context) {
	w.reloadPolicy()
	pol := w.engine.Policy()
	units := make([]string, 0, len(pol.Services))
	for u := range pol.Services {
		units = append(units, u)
	}
	sort.Strings(units)
	s, err := w.cfg.Sampler.Sample(ctx, units)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("sample failed", "component", "fleet-watchdog", "err", err)
		}
		return
	}
	w.engine.Observe(ctx, s)
}

// Run samples every policy sample_interval until ctx is cancelled. The interval
// follows live policy changes.
func (w *Watchdog) Run(ctx context.Context) error {
	for {
		w.Step(ctx)
		timer := time.NewTimer(w.engine.Policy().SampleInterval.D())
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// ApplyPolicy validates a policy profile and, if acceptable, persists it as the
// last-known policy and applies it live. An invalid profile, or one older than
// the profile in force, is rejected with an error and changes nothing: the
// last-known policy keeps running.
func (w *Watchdog) ApplyPolicy(raw []byte) error {
	w.mu.Lock()
	cur := w.profile.Version
	groups := append([]string(nil), w.groups...)
	w.mu.Unlock()
	reject := func(err error) error {
		w.engine.Emit(Event{Time: time.Now(), Kind: EventPolicy, Detail: "rejected: " + err.Error()})
		return err
	}
	pf, err := ParseProfile(raw)
	if err != nil {
		return reject(err)
	}
	if pf.Version < cur {
		return reject(fmt.Errorf("%w (pushed %d, current %d)", ErrStalePolicy, pf.Version, cur))
	}
	if _, err := pf.Effective(w.cfg.MachineID, groups); err != nil {
		return reject(fmt.Errorf("policy does not resolve for this host: %w", err))
	}
	if err := SaveLastKnown(w.cfg.DataDir, pf); err != nil {
		return reject(err)
	}
	return w.install(pf, "pushed")
}

// install resolves pf for this host and puts it in force.
func (w *Watchdog) install(pf *Profile, how string) error {
	w.mu.Lock()
	groups := append([]string(nil), w.groups...)
	pol, err := pf.Effective(w.cfg.MachineID, groups)
	if err != nil {
		w.mu.Unlock()
		return fmt.Errorf("policy does not resolve for this host: %w", err)
	}
	w.profile, w.applied, w.rejected = pf, pf.Raw, nil
	w.mu.Unlock()
	w.engine.SetPolicy(pol)
	w.engine.Emit(Event{Time: time.Now(), Kind: EventPolicy, Detail: fmt.Sprintf("version %d applied (%s)", pf.Version, how)})
	return nil
}

// reloadPolicy applies the on-disk last-known policy if it changed. A bad file
// is reported once and ignored.
func (w *Watchdog) reloadPolicy() {
	raw, err := os.ReadFile(policyPath(w.cfg.DataDir))
	if err != nil {
		return
	}
	w.mu.Lock()
	same := bytes.Equal(raw, w.applied) || bytes.Equal(raw, w.rejected)
	cur := w.profile.Version
	w.mu.Unlock()
	if same {
		return
	}
	reject := func(err error) {
		w.mu.Lock()
		w.rejected = raw
		w.mu.Unlock()
		w.log.Warn("policy on disk rejected; last-known keeps running", "component", "fleet-watchdog", "err", err)
		w.engine.Emit(Event{Time: time.Now(), Kind: EventPolicy, Detail: "rejected: " + err.Error()})
	}
	pf, err := ParseProfile(raw)
	if err != nil {
		reject(err)
		return
	}
	if pf.Version < cur {
		reject(fmt.Errorf("%w (found %d, current %d)", ErrStalePolicy, pf.Version, cur))
		return
	}
	if err := w.install(pf, "from disk"); err != nil {
		reject(err)
	}
}

// HandleEnvelope handles a policy envelope from the server and returns the ack to
// send back. ok is false for any other envelope type.
func (w *Watchdog) HandleEnvelope(env channel.Envelope) (reply channel.Envelope, ok bool) {
	if env.Type != channel.TypePolicy {
		return channel.Envelope{}, false
	}
	return PolicyAck(env, func(raw []byte, _ int) error { return w.ApplyPolicy(raw) }, w.PolicyVersion), true
}

// PolicyAck decodes a policy envelope, applies it with apply, and builds the
// policy-ack envelope. It lets a process that holds the channel but not the
// watchdog loop reuse the protocol: pass an apply that calls StorePolicy.
func PolicyAck(env channel.Envelope, apply func(raw []byte, version int) error, current func() int) channel.Envelope {
	var p channel.PolicyPayload
	err := json.Unmarshal(env.Payload, &p)
	switch {
	case err != nil:
		err = fmt.Errorf("bad policy payload: %w", err)
	default:
		err = apply([]byte(p.YAML), p.Version)
	}
	ack := channel.PolicyAckPayload{OK: err == nil, Version: current()}
	if err != nil {
		ack.Error = err.Error()
	}
	out, _ := channel.NewEnvelope(channel.TypePolicyAck, env.ID, ack)
	return out
}

// EventEnvelope wraps an event for the server (watchdog-ev).
func EventEnvelope(ev Event) (channel.Envelope, error) {
	return channel.NewEnvelope(channel.TypeWatchdogEv, "", channel.WatchdogEventPayload(ev))
}

// ResetCounters is the operator's counter reset (panel button): the daily
// counters, alert-only mode and the ladder stage are cleared.
func (w *Watchdog) ResetCounters(now time.Time) error {
	if err := w.guards.Reset(now); err != nil {
		return err
	}
	w.engine.Emit(Event{Time: now, Kind: EventCounters, Detail: "counters and alert-only mode reset by operator"})
	return nil
}

// Status is a snapshot for the panel and for logs.
type Status struct {
	PolicyVersion int
	State         State
	Throttled     []Target
	Events        []Event
}

// Status returns the current status.
func (w *Watchdog) Status() Status {
	return Status{
		PolicyVersion: w.PolicyVersion(),
		State:         w.guards.Snapshot(),
		Throttled:     w.engine.Throttled(),
		Events:        w.engine.Events(),
	}
}
