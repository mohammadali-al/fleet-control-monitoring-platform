package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

// Watchdog page API (T-0012 library, T-0022 wiring): the policy profile that
// is stored on the main server and pushed to the worker agents, the per-host
// state the agents report, and the event history.
//
//	GET  /api/watchdog                 summary tiles and per-host state
//	GET  /api/watchdog/events?machine= event history, newest first
//	GET  /api/watchdog/policy          the stored profile (or a starting template) and its effective values
//	POST /api/watchdog/policy {yaml}   validate, store and push to every connected worker
//
// Counter reset is not here: nothing in the control channel tells an agent's
// watchdog to reset its counters yet, so the page keeps those buttons as
// stubs (see the task log).
func (s *server) routesWatchdog(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/watchdog", s.observed(s.wdHosts))
	mux.HandleFunc("GET /api/watchdog/events", s.observed(s.wdEvents))
	mux.HandleFunc("GET /api/watchdog/policy", s.observed(s.wdPolicy))
	mux.HandleFunc("POST /api/watchdog/policy", s.observed(s.wdSavePolicy))
}

// hostState is what the page shows for one worker, derived from the policy
// acknowledgement and the events the agent forwarded. The agent's counters and
// ladder stage live on the agent (guards.go) and are not reported as such, so
// they are reconstructed from the event stream: an approximation, labelled so.
type hostState struct {
	Machine    string `json:"machine"`
	Connection string `json:"connection"` // the machine's channel state
	// Condition: normal | breaching | throttled | alert-only | not-reporting.
	Condition string `json:"condition"`
	Stage     string `json:"stage"` // last ladder step taken: none | throttle | kill_session | restart | reboot
	Level     int    `json:"level"` // 0..4, for the ladder bars
	Restarts  int    `json:"restarts_today"`
	Reboots   int    `json:"reboots_today"`
	RestartsX int    `json:"restart_cap"`
	RebootsX  int    `json:"reboot_cap"`
	// Policy: what the manager pushed and what the agent acknowledged.
	PolicyPushed int                  `json:"policy_pushed"`
	PolicyInForc int                  `json:"policy_in_force"`
	PolicyAcked  bool                 `json:"policy_acked"`
	PolicyOK     bool                 `json:"policy_ok"`
	PolicyErr    string               `json:"policy_error,omitempty"`
	Events       int                  `json:"events"`
	LastEvent    *fleet.WatchdogEvent `json:"last_event,omitempty"`
	// Breaches are the (scope, metric) pairs currently breached per the events.
	Breaches []string `json:"breaches,omitempty"`
}

var stageLevel = map[string]int{"none": 0, "throttle": 1, "kill_session": 2, "restart": 3, "reboot": 4}

// deriveHost folds a machine's events (oldest first) into its state at now.
func deriveHost(events []fleet.WatchdogEvent, autoReset bool, now time.Time) hostState {
	h := hostState{Stage: "none", Condition: "normal", Events: len(events)}
	today := now.In(time.Local).Format("2006-01-02")
	breaches := map[string]bool{}
	throttled := map[string]bool{}
	alertOnly := false
	for i := range events {
		ev := events[i]
		switch ev.Kind {
		case watchdog.EventBreach:
			breaches[ev.Scope+"/"+ev.Metric] = true
		case watchdog.EventRecovered:
			delete(breaches, ev.Scope+"/"+ev.Metric)
		case watchdog.EventAction:
			h.Stage = ev.Step
			day := ev.Time.In(time.Local).Format("2006-01-02")
			switch ev.Step {
			case "throttle":
				throttled[ev.Target] = true
			case "restart":
				if day == today {
					h.Restarts++
				}
			case "reboot":
				if day == today {
					h.Reboots++
				}
			}
		case watchdog.EventLifted:
			delete(throttled, ev.Target)
		case watchdog.EventAlertOnly:
			alertOnly = true
		case watchdog.EventDay:
			h.Restarts, h.Reboots = 0, 0
			h.Stage = "none"
			if autoReset {
				alertOnly = false
			}
		case watchdog.EventCounters:
			h.Restarts, h.Reboots = 0, 0
			h.Stage = "none"
			alertOnly = false
		}
	}
	h.Level = stageLevel[h.Stage]
	for k := range breaches {
		h.Breaches = append(h.Breaches, k)
	}
	sort.Strings(h.Breaches)
	switch {
	case alertOnly:
		h.Condition = "alert-only"
	case len(throttled) > 0:
		h.Condition = "throttled"
	case len(breaches) > 0:
		h.Condition = "breaching"
	}
	if n := len(events); n > 0 {
		last := events[n-1]
		h.LastEvent = &last
	}
	return h
}

type wdSummary struct {
	Normal       int `json:"normal"`
	Acting       int `json:"acting"`
	AlertOnly    int `json:"alert_only"`
	NotReporting int `json:"not_reporting"`
}

func (s *server) wdHosts(w http.ResponseWriter, _ *http.Request, ob observer, o *fleet.Observe) {
	var pf *watchdog.Profile
	if o.Profile != nil {
		var err error
		if pf, err = o.Profile.Current(); err != nil {
			s.log.Warn("watchdog: stored policy", "component", "panel", "err", err)
		}
	}
	now := time.Now()
	hosts := []hostState{}
	var sum wdSummary
	pushed := 0
	workers := 0
	for _, m := range s.be.Machines() {
		if m.Role != registry.RoleWorker || m.Removed {
			continue
		}
		workers++
		pol := watchdog.DefaultPolicy()
		if pf != nil {
			if p, err := pf.Effective(m.ID, nil); err == nil {
				pol = p
			}
		}
		h := deriveHost(ob.WatchdogEvents(m.ID), pol.AutoResetMidnight, now)
		h.Machine, h.Connection = m.ID, m.State
		h.RestartsX, h.RebootsX = pol.Caps.RestartsPerDay, pol.Caps.RebootsPerDay
		if st, ok := ob.WatchdogStatus(m.ID); ok {
			h.PolicyPushed, h.PolicyInForc, h.PolicyAcked, h.PolicyOK, h.PolicyErr = st.Pushed, st.InForce, st.Acked, st.AckOK, st.AckErr
		}
		if pf != nil && h.PolicyAcked && h.PolicyOK && h.PolicyInForc == pf.Version {
			pushed++
		}
		if m.State != fleet.StateOnline {
			h.Condition = "not-reporting"
		}
		switch h.Condition {
		case "normal":
			sum.Normal++
		case "alert-only":
			sum.AlertOnly++
		case "not-reporting":
			sum.NotReporting++
		default:
			sum.Acting++
		}
		hosts = append(hosts, h)
	}
	policy := map[string]any{"stored": pf != nil, "workers": workers, "in_force": pushed}
	if pf != nil {
		policy["version"] = pf.Version
	}
	writeJSON(w, http.StatusOK, map[string]any{"summary": sum, "hosts": hosts, "policy": policy})
}

const (
	defaultEventLimit = 100
	maxEventLimit     = 200
)

func (s *server) wdEvents(w http.ResponseWriter, r *http.Request, ob observer, _ *fleet.Observe) {
	limit := defaultEventLimit
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, maxEventLimit)
	}
	only := r.URL.Query().Get("machine")
	var evs []fleet.WatchdogEvent
	for _, m := range s.be.Machines() {
		if m.Role != registry.RoleWorker || (only != "" && m.ID != only) {
			continue
		}
		evs = append(evs, ob.WatchdogEvents(m.ID)...)
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Time.After(evs[j].Time) })
	if len(evs) > limit {
		evs = evs[:limit]
	}
	if evs == nil {
		evs = []fleet.WatchdogEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}

func (s *server) wdPolicy(w http.ResponseWriter, _ *http.Request, _ observer, o *fleet.Observe) {
	if o.Profile == nil {
		notConfigured(w, "the watchdog policy store")
		return
	}
	pf, err := o.Profile.Current()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	out := map[string]any{"has_profile": pf != nil}
	if pf == nil {
		def := watchdog.DefaultPolicy()
		out["version"] = 0
		out["yaml"] = policyTemplate(1, def)
		out["effective"] = def
		out["hosts"], out["groups"] = []string{}, []string{}
	} else {
		eff, err := pf.Effective("", nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
			return
		}
		hosts, groups := pf.Names()
		if hosts == nil {
			hosts = []string{}
		}
		if groups == nil {
			groups = []string{}
		}
		out["version"], out["yaml"], out["effective"], out["hosts"], out["groups"] = pf.Version, string(pf.Raw), eff, hosts, groups
	}
	writeJSON(w, http.StatusOK, out)
}

type policyBody struct {
	YAML string `json:"yaml"`
}

func (s *server) wdSavePolicy(w http.ResponseWriter, r *http.Request, ob observer, o *fleet.Observe) {
	if o.Profile == nil {
		notConfigured(w, "the watchdog policy store")
		return
	}
	var b policyBody
	if !decode(w, r, &b) {
		return
	}
	if strings.TrimSpace(b.YAML) == "" {
		writeJSON(w, http.StatusBadRequest, errBody("the policy is empty"))
		return
	}
	pf, err := o.Profile.Save([]byte(b.YAML))
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, fleet.ErrPolicyVersion) {
			code = http.StatusConflict
		}
		writeJSON(w, code, errBody(err.Error()))
		return
	}
	s.log.Info("watchdog policy saved", "component", "panel", "operator", operatorFrom(r), "version", pf.Version)
	online := 0
	for _, m := range s.be.Machines() {
		if m.Role == registry.RoleWorker && !m.Removed && m.State == fleet.StateOnline {
			online++
		}
	}
	ob.PushPolicies(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"version": pf.Version, "pushed_to": online})
}

// policyTemplate renders a starting profile from the baked-in default policy,
// so the editor never starts blank. It only uses keys the profile schema has.
func policyTemplate(version int, p watchdog.Policy) string {
	th := func(name string, t watchdog.Threshold) string {
		return fmt.Sprintf("  %s:\n    trigger: %v\n    recover: %v\n", name, t.Trigger, t.Recover)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Watchdog policy profile. Every key is optional; an absent key keeps the baked-in default.\n")
	fmt.Fprintf(&b, "# Raise \"version\" on every save: agents refuse an older profile.\n")
	fmt.Fprintf(&b, "version: %d\n", version)
	fmt.Fprintf(&b, "sample_interval: %s\nsustain: %s\n", p.SampleInterval.D(), p.Sustain.D())
	fmt.Fprintf(&b, "machine:\n")
	b.WriteString(th("cpu_percent", p.Machine.CPUPercent))
	b.WriteString(th("memory_percent", p.Machine.MemoryPercent))
	b.WriteString(th("load_per_cpu", p.Machine.LoadPerCPU))
	b.WriteString(th("disk_percent", p.Machine.DiskPercent))
	fmt.Fprintf(&b, "steps:\n  throttle: %t\n  kill_session: %t\n  restart: %t\n  reboot: %t\n", p.Steps.Throttle, p.Steps.KillSession, p.Steps.Restart, p.Steps.Reboot)
	fmt.Fprintf(&b, "throttle:\n  duration: %s\n  cpu_percent: %v\n", p.Throttle.Duration.D(), p.Throttle.CPUPercent)
	fmt.Fprintf(&b, "cooldowns:\n  throttle: %s\n  kill_session: %s\n  restart: %s\n  reboot: %s\n",
		p.Cooldowns.Throttle.D(), p.Cooldowns.KillSession.D(), p.Cooldowns.Restart.D(), p.Cooldowns.Reboot.D())
	fmt.Fprintf(&b, "caps:\n  restarts_per_day: %d\n  reboots_per_day: %d\n", p.Caps.RestartsPerDay, p.Caps.RebootsPerDay)
	fmt.Fprintf(&b, "boot_window: %s\nstage_reset: %s\nauto_reset_midnight: %t\n", p.BootWindow.D(), p.StageReset.D(), p.AutoResetMidnight)
	fmt.Fprintf(&b, "# Per-host and per-group overrides, for example:\n# overrides:\n#   hosts:\n#     wrk-07:\n#       throttle:\n#         cpu_percent: 40\n")
	return b.String()
}
