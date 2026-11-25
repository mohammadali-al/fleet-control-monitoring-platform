package fleet

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// The watchdog half of the manager (architecture 11.6): the manager pushes the
// version-stamped YAML policy profile to a worker agent when it connects and
// whenever the profile is edited, records the agent's policy-ack, and keeps the
// watchdog events the agent forwards (watchdog-ev) for the panel.

// policyEnvelopeID is the id of every policy envelope; the ack echoes it.
const policyEnvelopeID = "policy"

// maxWatchdogEvents is how many events are kept per machine (oldest dropped).
const maxWatchdogEvents = 200

// WatchdogPolicy is one policy profile for one machine.
type WatchdogPolicy struct {
	// Version is the profile's version stamp (the "version" key of the YAML).
	Version int
	// YAML is the profile text. It carries the per-host and per-group
	// overrides; the agent resolves them for itself, so every machine normally
	// gets the same text.
	YAML string
}

// WatchdogSource connects the manager to the stored policy and to whatever
// surfaces events (the panel's Watchdog page).
type WatchdogSource struct {
	// PolicyFor returns the profile to push to a machine; ok is false when
	// there is none (nothing is pushed and the agent keeps its last-known policy).
	PolicyFor func(machine string) (p WatchdogPolicy, ok bool)
	// OnEvent, if set, is called for every new event from a machine, on the
	// link's read loop (it must not block).
	OnEvent func(machine string, ev WatchdogEvent)
}

// WatchdogEvent is one event a machine's watchdog reported.
type WatchdogEvent struct {
	Machine string `json:"machine"`
	channel.WatchdogEventPayload
	// Received is when the manager got it.
	Received time.Time `json:"received"`
}

// WatchdogStatus is what the manager knows of one machine's watchdog policy.
type WatchdogStatus struct {
	// Pushed is the version last pushed; Pushes counts the pushes.
	Pushed int `json:"pushed_version"`
	Pushes int `json:"pushes"`
	// Acked is true once any policy-ack arrived; AckOK and AckErr are the
	// last one's verdict, InForce the version the agent reported as its
	// last-known policy.
	Acked   bool      `json:"acked"`
	AckOK   bool      `json:"ack_ok"`
	AckErr  string    `json:"ack_error,omitempty"`
	InForce int       `json:"in_force_version"`
	AckedAt time.Time `json:"acked_at,omitempty"`
}

type wdState struct {
	st     WatchdogStatus
	events []WatchdogEvent
}

// wdFor returns the machine's watchdog state. m.wmu is held.
func (m *Manager) wdFor(id string) *wdState {
	s := m.wd[id]
	if s == nil {
		s = &wdState{}
		m.wd[id] = s
	}
	return s
}

// SetWatchdog binds (or replaces) the policy source and event callback. Call
// PushPolicies afterwards to bring already-connected agents up to date.
func (m *Manager) SetWatchdog(src WatchdogSource) {
	m.mu.Lock()
	m.wsrc = src
	m.mu.Unlock()
}

func (m *Manager) watchdogSource() WatchdogSource {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wsrc
}

// pushPolicy sends one worker agent its policy profile, if there is one. The
// agent validates and stores it (the watchdog process applies it live) and
// answers with a policy-ack; a profile it refuses changes nothing there.
func (m *Manager) pushPolicy(ctx context.Context, n *node) {
	src := m.watchdogSource()
	if src.PolicyFor == nil {
		m.log.Debug("no watchdog policy to push", "component", "fleet", "machine", n.id)
		return
	}
	p, ok := src.PolicyFor(n.id)
	if !ok {
		return
	}
	env, err := channel.NewEnvelope(channel.TypePolicy, policyEnvelopeID, channel.PolicyPayload{Version: p.Version, YAML: p.YAML})
	if err == nil {
		err = n.link.Send(ctx, env)
	}
	if err != nil {
		m.log.Warn("push watchdog policy", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	m.wmu.Lock()
	s := m.wdFor(n.id)
	s.st.Pushed, s.st.Pushes = p.Version, s.st.Pushes+1
	m.wmu.Unlock()
	m.log.Info("pushed watchdog policy", "component", "fleet", "machine", n.id, "version", p.Version)
}

// PushPolicy re-sends the policy to one connected worker (after an edit that
// concerns that machine). It returns ErrUnknown for a machine that is not a
// worker with a link and ErrOffline when it is not connected; an offline
// agent gets its policy when it connects.
func (m *Manager) PushPolicy(ctx context.Context, machine string) error {
	m.mu.Lock()
	n := m.nodes[machine]
	m.mu.Unlock()
	mc, known := m.cfg.Registry.Get(machine)
	if !known || n == nil || mc.Role != registry.RoleWorker {
		return ErrUnknown
	}
	if n.link == nil || !n.link.Online() {
		return ErrOffline
	}
	m.pushPolicy(ctx, n)
	return nil
}

// PushPolicies re-sends the policy to every connected worker, for after the
// profile was edited. Agents that are offline get theirs when they connect.
func (m *Manager) PushPolicies(ctx context.Context) {
	m.mu.Lock()
	var nodes []*node
	for id, n := range m.nodes {
		if mc, ok := m.cfg.Registry.Get(id); ok && mc.Role == registry.RoleWorker && n.link != nil && n.link.Online() {
			nodes = append(nodes, n)
		}
	}
	m.mu.Unlock()
	for _, n := range nodes {
		m.pushPolicy(ctx, n)
	}
}

// recordPolicyAck notes the agent's answer to a pushed policy. A refusal is
// logged: the agent keeps running its previous policy.
func (m *Manager) recordPolicyAck(n *node, env channel.Envelope) {
	var ack channel.PolicyAckPayload
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		m.log.Warn("bad policy ack", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	m.wmu.Lock()
	s := m.wdFor(n.id)
	s.st.Acked, s.st.AckOK, s.st.AckErr, s.st.InForce, s.st.AckedAt = true, ack.OK, ack.Error, ack.Version, time.Now()
	m.wmu.Unlock()
	if ack.OK {
		m.log.Info("agent stored watchdog policy", "component", "fleet", "machine", n.id, "version", ack.Version)
	} else {
		m.log.Warn("agent rejected watchdog policy", "component", "fleet", "machine", n.id, "reason", ack.Error, "in_force", ack.Version)
	}
}

// recordWatchdogEvent stores one forwarded event under the machine this link
// is verified against (never a field of the payload). Forwarding is
// at-least-once, so an event seen before is dropped.
func (m *Manager) recordWatchdogEvent(n *node, env channel.Envelope) {
	var p channel.WatchdogEventPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		m.log.Warn("bad watchdog event", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	ev := WatchdogEvent{Machine: n.id, WatchdogEventPayload: p, Received: time.Now()}
	m.wmu.Lock()
	s := m.wdFor(n.id)
	for _, old := range s.events {
		if old.WatchdogEventPayload == p || (old.Time.Equal(p.Time) && old.Kind == p.Kind && old.Scope == p.Scope &&
			old.Metric == p.Metric && old.Target == p.Target && old.Step == p.Step && old.Detail == p.Detail && old.Value == p.Value) {
			m.wmu.Unlock()
			return
		}
	}
	s.events = append(s.events, ev)
	if len(s.events) > maxWatchdogEvents {
		s.events = append([]WatchdogEvent(nil), s.events[len(s.events)-maxWatchdogEvents:]...)
	}
	m.wmu.Unlock()
	m.log.Info("watchdog event", "component", "fleet", "machine", n.id, "kind", p.Kind, "scope", p.Scope,
		"metric", p.Metric, "step", p.Step, "detail", p.Detail)
	if cb := m.watchdogSource().OnEvent; cb != nil {
		cb(n.id, ev)
	}
}

// WatchdogEvents returns the stored events of a machine, oldest first (the
// last maxWatchdogEvents of them).
func (m *Manager) WatchdogEvents(machine string) []WatchdogEvent {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	if s := m.wd[machine]; s != nil {
		return append([]WatchdogEvent(nil), s.events...)
	}
	return nil
}

// WatchdogStatus reports the policy push state of a machine; ok is false when
// nothing was pushed to or heard from it yet.
func (m *Manager) WatchdogStatus(machine string) (WatchdogStatus, bool) {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	if s := m.wd[machine]; s != nil {
		return s.st, true
	}
	return WatchdogStatus{}, false
}

// ResetCounters tells one connected worker agent's watchdog to do the
// operator's counter reset: the daily restart/reboot counters, the alert-only
// latch and the ladder stage are cleared (cooldowns are kept). The watchdog
// applies it at its next sample and reports it as a counters-reset event. It
// returns ErrUnknown for a machine that is not a worker with a link and
// ErrOffline when it is not connected: a reset is an operator's immediate
// intent and is not queued for a later connection.
func (m *Manager) ResetCounters(ctx context.Context, machine string) error {
	m.mu.Lock()
	n := m.nodes[machine]
	m.mu.Unlock()
	mc, known := m.cfg.Registry.Get(machine)
	if !known || n == nil || mc.Role != registry.RoleWorker {
		return ErrUnknown
	}
	if n.link == nil || !n.link.Online() {
		return ErrOffline
	}
	env, err := channel.NewEnvelope(channel.TypeWatchdogReset, "", nil)
	if err == nil {
		err = n.link.Send(ctx, env)
	}
	if err != nil {
		m.log.Warn("push watchdog counter reset", "component", "fleet", "machine", machine, "err", err)
		return err
	}
	m.log.Info("pushed watchdog counter reset", "component", "fleet", "machine", machine)
	return nil
}
