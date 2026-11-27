package panel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/evidence"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// Monitor data page API (internal/monitor, T-0021): the live charts and the
// probe-config editor. Probe results arrive from the monitor agents into a
// fleet.Series (the monitor sink); the browser gets a snapshot here and new
// results on /ws/monitor.
//
//	GET  /api/monitor/live?window=60        series in the window, command overlay, per-agent summary
//	GET  /api/monitor/history               persisted samples by metric + time range (History UI data source)
//	GET  /api/monitor/config                jobs, probe settings, monitor agents and their load
//	POST /api/monitor/settings              global frequency and timeouts (pushed to the agents)
//	POST /api/monitor/jobs                  create a job {url, ip, paths, interval_ms}
//	POST /api/monitor/jobs/{id}/delete      delete a job (its history is kept)
//	POST /api/monitor/jobs/{id}/interval    per-target frequency override {target, interval_ms}
func (s *server) routesMonitor(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/monitor/live", s.observed(s.monLive))
	mux.HandleFunc("GET /api/monitor/history", s.observed(s.monHistory))
	mux.HandleFunc("GET /api/monitor/config", s.observed(s.monConfig))
	mux.HandleFunc("POST /api/monitor/settings", s.observed(s.monSettings))
	mux.HandleFunc("POST /api/monitor/jobs", s.observed(s.monCreateJob))
	mux.HandleFunc("POST /api/monitor/jobs/{id}/delete", s.observed(s.monDeleteJob))
	mux.HandleFunc("POST /api/monitor/jobs/{id}/interval", s.observed(s.monInterval))
}

// monitorStatus maps a monitor package error to an HTTP status.
func monitorStatus(err error) int {
	switch {
	case errors.Is(err, monitor.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, monitor.ErrDuplicate), errors.Is(err, monitor.ErrNoCapacity), errors.Is(err, monitor.ErrNoMonitorAgent):
		return http.StatusConflict
	case errors.Is(err, monitor.ErrIPRequired), errors.Is(err, monitor.ErrBadIP), errors.Is(err, monitor.ErrBadOrigin),
		errors.Is(err, monitor.ErrBadPath), errors.Is(err, monitor.ErrPathNeedsOrg),
		errors.Is(err, monitor.ErrBadProbeType), errors.Is(err, monitor.ErrOriginRequired),
		errors.Is(err, monitor.ErrDNSNameRequired), errors.Is(err, monitor.ErrBadDNSServer):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

const (
	defaultWindowSec = 60 // the live window (architecture 8.4)
	maxWindowSec     = 600
)

func windowFrom(r *http.Request) int {
	if v, err := strconv.Atoi(r.URL.Query().Get("window")); err == nil && v >= 10 {
		return min(v, maxWindowSec)
	}
	return defaultWindowSec
}

type monAgent struct {
	ID       string     `json:"id"`
	State    string     `json:"state"`
	Targets  int        `json:"targets"`
	Samples  int        `json:"samples"`
	OKPct    float64    `json:"ok_pct"`
	LastAt   *time.Time `json:"last_at,omitempty"`
	LastMs   float64    `json:"last_ms"`
	LastType string     `json:"last_type,omitempty"`
}

const overlayIntervals = 12

type monInterval struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Packets int       `json:"packets"`
}

type monCommand struct {
	Session string     `json:"session"`
	Machine string     `json:"machine"`
	Command string     `json:"command"`
	User    string     `json:"user,omitempty"`
	Mode    string     `json:"mode"`
	Start   time.Time  `json:"start"`
	End     *time.Time `json:"end,omitempty"` // nil: still running
	// Packets is the archived packet count of the session; nil until its
	// evidence has been pulled to the main server.
	Packets *int `json:"packets"`
	// IntervalPackets holds the archived packet count for this session in
	// each of the live window's intervals; nil until evidence is archived.
	IntervalPackets []int `json:"interval_packets,omitempty"`
}

func (s *server) monLive(w http.ResponseWriter, r *http.Request, _ observer, o *fleet.Observe) {
	if o.Series == nil {
		notConfigured(w, "the monitor sink")
		return
	}
	win := windowFrom(r)
	now := time.Now()
	since := now.Add(-time.Duration(win) * time.Second)
	series := o.Series.Snapshot(since)

	byAgent := map[string]*monAgent{}
	for _, m := range s.be.Machines() {
		if m.Role == registry.RoleMonitor && !m.Removed {
			byAgent[m.ID] = &monAgent{ID: m.ID, State: m.State}
		}
	}
	if o.Jobs != nil {
		for agent, n := range o.Jobs.AgentLoad() {
			if a := byAgent[agent]; a != nil {
				a.Targets = n
			}
		}
	}
	okCount := map[string]int{}
	for _, sd := range series {
		a := byAgent[sd.Agent]
		if a == nil {
			a = &monAgent{ID: sd.Agent, State: "unknown"} // data from an agent no longer registered
			byAgent[sd.Agent] = a
		}
		for _, p := range sd.Points {
			a.Samples++
			if p.OK {
				okCount[sd.Agent]++
			}
			if a.LastAt == nil || p.T.After(*a.LastAt) {
				t := p.T
				a.LastAt, a.LastMs, a.LastType = &t, p.Ms, string(sd.Type)
			}
		}
	}
	agents := make([]monAgent, 0, len(byAgent))
	for id, a := range byAgent {
		if a.Samples > 0 {
			a.OKPct = float64(okCount[id]) * 100 / float64(a.Samples)
		}
		agents = append(agents, *a)
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })

	// Targets by history key, so the page can label and filter the lines.
	type jobRef struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Origin string `json:"origin,omitempty"`
		IP     string `json:"ip"`
		Key    string `json:"key"`
	}
	jobs := []jobRef{}
	targets := map[string]string{} // target id -> "type address"
	if o.Jobs != nil {
		for _, j := range o.Jobs.List() {
			label := j.Origin
			if label == "" && j.DNSName != "" {
				label = "dns://" + j.DNSName
			}
			if label == "" {
				label = j.IP
			}
			jobs = append(jobs, jobRef{ID: j.ID, Label: label + " · " + j.IP, Origin: j.Origin, IP: j.IP, Key: j.HistoryKey})
			for _, t := range j.Targets {
				targets[t.Target.ID] = string(t.Target.Type) + " " + t.Target.Address
			}
		}
	}

	cmds := []monCommand{}
	for _, c := range s.be.Commands() {
		start := c.Queued
		if c.Started != nil {
			start = *c.Started
		} else {
			continue // queued, not yet running: nothing was sent
		}
		if c.Finished != nil && c.Finished.Before(since) {
			continue
		}
		cmds = append(cmds, monCommand{Session: c.Session, Machine: c.Machine, Command: c.Command, User: c.User, Mode: c.Mode, Start: start, End: c.Finished})
	}
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Start.Before(cmds[j].Start) })
	intervals := buildIntervals(since, now)
	if o.Archive != nil && len(cmds) > 0 {
		populateCommandPacketStats(o.Archive, cmds, intervals, since, now)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"now": now, "window_sec": win, "series": series, "agents": agents, "jobs": jobs, "targets": targets, "commands": cmds, "intervals": intervals,
	})
}

// monHistory queries persisted monitor time-series samples by metric and time
// range from o.Store (the data source for the History UI).
func (s *server) monHistory(w http.ResponseWriter, r *http.Request, _ observer, o *fleet.Observe) {
	if o.Store == nil {
		notConfigured(w, "the monitor history store")
		return
	}
	q := r.URL.Query()
	metric, err := monitor.NormalizeMetric(q.Get("metric"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	end := time.Now().UTC()
	if raw := firstQuery(q, "to", "end"); raw != "" {
		t, err := parseHistoryTime(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errBody("to must be RFC 3339 or unix seconds"))
			return
		}
		end = t
	}
	start := end.Add(-time.Hour)
	if raw := firstQuery(q, "from", "start"); raw != "" {
		t, err := parseHistoryTime(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errBody("from must be RFC 3339 or unix seconds"))
			return
		}
		start = t
	}
	if end.Before(start) {
		writeJSON(w, http.StatusBadRequest, errBody("to must not be before from"))
		return
	}
	var step time.Duration
	if raw := strings.TrimSpace(q.Get("step")); raw != "" {
		d, err := parseStep(raw)
		if err != nil || d < 0 {
			writeJSON(w, http.StatusBadRequest, errBody("step must be a non-negative duration or seconds"))
			return
		}
		step = d
	}
	hq := monitor.HistoryQuery{
		Metric:     metric,
		Agent:      strings.TrimSpace(q.Get("agent")),
		Target:     strings.TrimSpace(q.Get("target")),
		HistoryKey: strings.TrimSpace(q.Get("history_key")),
		Type:       probe.Type(strings.TrimSpace(q.Get("type"))),
		Start:      start,
		End:        end,
		Step:       step,
	}
	series, err := o.Store.QueryRange(r.Context(), hq)
	if err != nil {
		if errors.Is(err, monitor.ErrBadQuery) {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		s.log.Warn("monitor: history query failed", "component", "panel", "metric", metric, "err", err)
		writeJSON(w, http.StatusBadGateway, errBody("monitor history query failed: "+err.Error()))
		return
	}
	if series == nil {
		series = []monitor.HistorySeries{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"metric":   metric,
		"from":     start,
		"to":       end,
		"step_sec": step.Seconds(),
		"series":   series,
	})
}

func firstQuery(q map[string][]string, keys ...string) string {
	for _, k := range keys {
		if vs := q[k]; len(vs) > 0 && strings.TrimSpace(vs[0]) != "" {
			return strings.TrimSpace(vs[0])
		}
	}
	return ""
}

func parseHistoryTime(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC(), nil
	}
	sec, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(int64(sec * 1000)).UTC(), nil
}

func parseStep(raw string) (time.Duration, error) {
	if d, err := time.ParseDuration(raw); err == nil {
		return d, nil
	}
	sec, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(sec * float64(time.Second)), nil
}

func segmentSessions(sg evidence.ArchivedSegment) []string {
	if len(sg.Sessions) > 0 {
		return sg.Sessions
	}
	if sg.Session != "" {
		return []string{sg.Session}
	}
	return nil
}

func buildIntervals(since, now time.Time) []monInterval {
	step := now.Sub(since) / overlayIntervals
	intervals := make([]monInterval, overlayIntervals)
	for i := range intervals {
		a := since.Add(time.Duration(i) * step)
		b := since.Add(time.Duration(i+1) * step)
		if i == overlayIntervals-1 {
			b = now
		}
		intervals[i] = monInterval{Start: a, End: b}
	}
	return intervals
}

func intervalIndex(ts, since, now time.Time, step time.Duration) (int, bool) {
	if step <= 0 || ts.Before(since) || ts.After(now) {
		return 0, false
	}
	idx := int(ts.Sub(since) / step)
	if idx >= overlayIntervals {
		idx = overlayIntervals - 1
	}
	return idx, true
}

func populateCommandPacketStats(arch *evidence.Archive, cmds []monCommand, intervals []monInterval, since, now time.Time) {
	type sessKey struct{ machine, session string }
	byMachine := map[string]map[string]bool{}
	for _, c := range cmds {
		m := byMachine[c.Machine]
		if m == nil {
			m = map[string]bool{}
			byMachine[c.Machine] = m
		}
		m[c.Session] = true
	}
	totals := map[sessKey]int{}
	found := map[sessKey]bool{}
	bins := map[sessKey][]int{}
	step := now.Sub(since) / overlayIntervals

	for machine, wanted := range byMachine {
		segs, err := arch.Segments(machine)
		if err != nil {
			continue
		}
		for _, sg := range segs {
			ids := segmentSessions(sg)
			var matched []string
			for _, id := range ids {
				if wanted[id] {
					matched = append(matched, id)
				}
			}
			if len(matched) == 0 {
				continue
			}
			for _, id := range matched {
				k := sessKey{machine, id}
				found[k] = true
				if bins[k] == nil {
					bins[k] = make([]int, overlayIntervals)
				}
			}
			shared := len(ids) > 1
			inWindow := !sg.EndTime.Before(since) && !sg.StartTime.After(now)
			if !shared && !inWindow {
				totals[sessKey{machine, matched[0]}] += sg.Records
				continue
			}
			f, err := arch.OpenSegment(machine, sg)
			if err != nil {
				if !shared {
					totals[sessKey{machine, matched[0]}] += sg.Records
				}
				continue
			}
			br := bufio.NewReaderSize(f, 64<<10)
			for {
				line, rerr := br.ReadBytes('\n')
				if len(line) > 0 {
					var row struct {
						SessionID string    `json:"session_id"`
						Timestamp time.Time `json:"timestamp"`
					}
					if jerr := json.Unmarshal(line, &row); jerr == nil && wanted[row.SessionID] {
						k := sessKey{machine, row.SessionID}
						totals[k]++
						if idx, ok := intervalIndex(row.Timestamp, since, now, step); ok {
							bins[k][idx]++
							intervals[idx].Packets++
						}
					}
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					break
				}
			}
			_ = f.Close()
		}
	}
	for i := range cmds {
		k := sessKey{cmds[i].Machine, cmds[i].Session}
		if found[k] {
			n := totals[k]
			cmds[i].Packets = &n
			cmds[i].IntervalPackets = bins[k]
		}
	}
}

// archivedPackets counts the archived records belonging to session on machine;
// ok is false when none of its evidence is on the main server yet. When a
// segment is shared across multiple sessions, only records of session are
// counted.
func archivedPackets(o *fleet.Observe, machine, session string) (int, bool) {
	segs, err := o.Archive.Segments(machine)
	if err != nil {
		return 0, false
	}
	n, found := 0, false
	for _, sg := range segs {
		ids := segmentSessions(sg)
		if !slices.Contains(ids, session) {
			continue
		}
		found = true
		if len(ids) <= 1 {
			n += sg.Records
			continue
		}
		counts, err := sessionPacketCounts(o.Archive, machine, sg)
		if err != nil {
			continue
		}
		n += counts[session]
	}
	return n, found
}

func (s *server) monConfig(w http.ResponseWriter, _ *http.Request, _ observer, o *fleet.Observe) {
	if o.Jobs == nil {
		notConfigured(w, "the monitor jobs store")
		return
	}
	jobs := o.Jobs.List()
	if jobs == nil {
		jobs = []monitor.Job{}
	}
	load := o.Jobs.AgentLoad()
	type agent struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		Targets int    `json:"targets"`
		Max     int    `json:"max"`
	}
	agents := []agent{}
	for _, m := range s.be.Machines() {
		if m.Role == registry.RoleMonitor && !m.Removed {
			agents = append(agents, agent{ID: m.ID, State: m.State, Targets: load[m.ID], Max: monitor.MaxTargetsPerAgent})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs": jobs, "settings": o.Jobs.Settings(), "agents": agents,
		"bounds": map[string]int{"min_interval_ms": int(probe.MinInterval / time.Millisecond), "max_interval_ms": int(probe.MaxInterval / time.Millisecond)},
	})
}

type settingsBody struct {
	IntervalMS int            `json:"interval_ms"`
	TimeoutMS  map[string]int `json:"timeout_ms"`
}

func validInterval(ms int) bool {
	return ms >= int(probe.MinInterval/time.Millisecond) && ms <= int(probe.MaxInterval/time.Millisecond)
}

func (s *server) monSettings(w http.ResponseWriter, r *http.Request, ob observer, o *fleet.Observe) {
	if o.Jobs == nil {
		notConfigured(w, "the monitor jobs store")
		return
	}
	var b settingsBody
	if !decode(w, r, &b) {
		return
	}
	if !validInterval(b.IntervalMS) {
		writeJSON(w, http.StatusBadRequest, errBody("the probe frequency must be between "+
			strconv.Itoa(int(probe.MinInterval/time.Millisecond))+" ms and "+strconv.Itoa(int(probe.MaxInterval/time.Millisecond))+" ms"))
		return
	}
	cur := o.Jobs.Settings()
	next := monitor.Settings{IntervalMS: b.IntervalMS, TimeoutMS: map[probe.Type]int{}}
	for t, ms := range cur.TimeoutMS {
		next.TimeoutMS[t] = ms
	}
	for name, ms := range b.TimeoutMS {
		t := probe.Type(name)
		switch t {
		case probe.TypeHTTP, probe.TypeTCP, probe.TypeICMP, probe.TypeDNS:
		default:
			writeJSON(w, http.StatusBadRequest, errBody("unknown probe type "+strconv.Quote(name)))
			return
		}
		if ms <= 0 || ms > 60000 {
			writeJSON(w, http.StatusBadRequest, errBody("the "+name+" timeout must be between 1 ms and 60000 ms"))
			return
		}
		next.TimeoutMS[t] = ms
	}
	if err := o.Jobs.SetSettings(next); err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	s.log.Info("monitor settings changed", "component", "panel", "operator", operatorFrom(r), "interval_ms", next.IntervalMS)
	ob.PushMonitorConfigs(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"settings": o.Jobs.Settings()})
}

type jobBody struct {
	URL        string   `json:"url"`
	IP         string   `json:"ip"`
	Paths      []string `json:"paths"`
	ProbeType  string   `json:"probe_type"`
	ProbeTypes []string `json:"probe_types"`
	DNSName    string   `json:"dns_name"`
	DNSServer  string   `json:"dns_server"`
	IntervalMS int      `json:"interval_ms"`
}

func (s *server) monCreateJob(w http.ResponseWriter, r *http.Request, ob observer, o *fleet.Observe) {
	if o.Jobs == nil {
		notConfigured(w, "the monitor jobs store")
		return
	}
	var b jobBody
	if !decode(w, r, &b) {
		return
	}
	if b.IntervalMS != 0 && !validInterval(b.IntervalMS) {
		writeJSON(w, http.StatusBadRequest, errBody("the frequency override is out of range"))
		return
	}
	var probeTypes []probe.Type
	for _, pt := range b.ProbeTypes {
		if pt = strings.TrimSpace(pt); pt != "" {
			probeTypes = append(probeTypes, probe.Type(pt))
		}
	}
	if len(probeTypes) == 0 && strings.TrimSpace(b.ProbeType) != "" {
		probeTypes = []probe.Type{probe.Type(strings.TrimSpace(b.ProbeType))}
	}
	var spec monitor.Spec
	if strings.TrimSpace(b.URL) != "" {
		var err error
		if spec, err = monitor.SpecFromURL(b.IP, b.URL); err != nil {
			writeJSON(w, monitorStatus(err), errBody(err.Error()))
			return
		}
		if len(probeTypes) > 0 && !slices.Contains(probeTypes, probe.TypeHTTP) {
			spec.Paths = nil
		}
	} else {
		spec = monitor.Spec{IP: b.IP}
	}
	spec.ProbeTypes = probeTypes
	spec.DNSName = b.DNSName
	spec.DNSServer = b.DNSServer
	for _, p := range b.Paths {
		if p = strings.TrimSpace(p); p != "" {
			spec.Paths = append(spec.Paths, p)
		}
	}
	var agents []string
	for _, m := range s.be.Machines() {
		if m.Role == registry.RoleMonitor && !m.Removed {
			agents = append(agents, m.ID)
		}
	}
	job, err := o.Jobs.Create(spec, agents)
	if err != nil {
		writeJSON(w, monitorStatus(err), errBody(err.Error()))
		return
	}
	if b.IntervalMS != 0 {
		for _, t := range job.Targets {
			if err := o.Jobs.SetTargetInterval(job.ID, t.Target.ID, time.Duration(b.IntervalMS)*time.Millisecond); err != nil {
				s.log.Warn("monitor: frequency override", "component", "panel", "job", job.ID, "err", err)
			}
		}
		job, _ = o.Jobs.Get(job.ID)
	}
	s.log.Info("monitor job created", "component", "panel", "operator", operatorFrom(r), "job", job.ID, "ip", job.IP, "origin", job.Origin)
	ob.PushMonitorConfigs(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusCreated, map[string]any{"job": job})
}

func (s *server) monDeleteJob(w http.ResponseWriter, r *http.Request, ob observer, o *fleet.Observe) {
	if o.Jobs == nil {
		notConfigured(w, "the monitor jobs store")
		return
	}
	id := r.PathValue("id")
	if err := o.Jobs.Delete(id); err != nil {
		writeJSON(w, monitorStatus(err), errBody(err.Error()))
		return
	}
	s.log.Info("monitor job deleted", "component", "panel", "operator", operatorFrom(r), "job", id)
	ob.PushMonitorConfigs(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

type intervalBody struct {
	Target     string `json:"target"`
	IntervalMS int    `json:"interval_ms"` // 0 clears the override
}

func (s *server) monInterval(w http.ResponseWriter, r *http.Request, ob observer, o *fleet.Observe) {
	if o.Jobs == nil {
		notConfigured(w, "the monitor jobs store")
		return
	}
	var b intervalBody
	if !decode(w, r, &b) {
		return
	}
	if b.IntervalMS != 0 && !validInterval(b.IntervalMS) {
		writeJSON(w, http.StatusBadRequest, errBody("the frequency override is out of range"))
		return
	}
	id := r.PathValue("id")
	if err := o.Jobs.SetTargetInterval(id, b.Target, time.Duration(b.IntervalMS)*time.Millisecond); err != nil {
		writeJSON(w, monitorStatus(err), errBody(err.Error()))
		return
	}
	ob.PushMonitorConfigs(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"job": id, "target": b.Target, "interval_ms": b.IntervalMS})
}
