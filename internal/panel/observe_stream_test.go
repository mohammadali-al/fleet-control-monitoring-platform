package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

// Tests of the Monitor data and Watchdog pages (the live streams and the
// editors). The Evidence and Backup tests are in observe_test.go.

func (e *obsEnv) wsDial(path string) (*websocket.Conn, context.Context) {
	e.t.Helper()
	return dial(e.t, "ws"+strings.TrimPrefix(e.srv.URL, "http")+path)
}

func monDataEnvelope(t *testing.T, seq uint64, results ...probe.Result) channel.Envelope {
	t.Helper()
	env, err := channel.NewEnvelope(channel.TypeMonData, fmt.Sprintf("mon-%d", seq), monitor.Batch{Seq: seq, Results: results})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestMonitorLive(t *testing.T) {
	e := newObsEnv(t, nil)
	e.waitState("mon1", fleet.StateOnline)

	// Jobs: validation, creation (the agent is pushed its config), duplicate.
	if code, _ := e.req("POST", "/api/monitor/jobs", map[string]any{"url": "https://shop.example.test/checkout", "ip": "not-an-ip"}, nil); code != 400 {
		t.Errorf("bad ip = %d, want 400", code)
	}
	var created struct{ Job monitor.Job }
	if code, _ := e.req("POST", "/api/monitor/jobs", map[string]any{"url": "https://shop.example.test/checkout", "ip": "198.51.100.10", "interval_ms": 500}, &created); code != 201 {
		t.Fatalf("create job = %d", code)
	}
	job := created.Job
	if job.Origin != "https://shop.example.test:443" || len(job.Targets) != 3 {
		t.Fatalf("job = %+v", job)
	}
	for _, at := range job.Targets {
		if at.Agent != "mon1" || at.Target.IntervalMS != 500 {
			t.Errorf("target = %+v", at)
		}
	}
	if code, _ := e.req("POST", "/api/monitor/jobs", map[string]any{"url": "https://shop.example.test/checkout", "ip": "198.51.100.10"}, nil); code != 409 {
		t.Errorf("duplicate job = %d, want 409", code)
	}
	eventually(t, "mon-config pushed", func() bool { return len(e.link("mon1").sentOfType(channel.TypeMonConfig)) > 0 })
	var pushed probe.Config
	cfgs := e.link("mon1").sentOfType(channel.TypeMonConfig)
	if err := json.Unmarshal(cfgs[len(cfgs)-1].Payload, &pushed); err != nil || len(pushed.Targets) != 3 {
		t.Errorf("pushed config = %+v %v", pushed, err)
	}

	// Config view and the global defaults (also pushed).
	var cfg struct {
		Jobs     []monitor.Job    `json:"jobs"`
		Settings monitor.Settings `json:"settings"`
		Agents   []map[string]any `json:"agents"`
	}
	if code, _ := e.req("GET", "/api/monitor/config", nil, &cfg); code != 200 || len(cfg.Jobs) != 1 || len(cfg.Agents) != 1 || cfg.Agents[0]["targets"].(float64) != 3 {
		t.Fatalf("config = %d %+v", code, cfg)
	}
	if code, _ := e.req("POST", "/api/monitor/settings", map[string]any{"interval_ms": 100}, nil); code != 400 {
		t.Errorf("interval below the minimum = %d, want 400", code)
	}
	if code, _ := e.req("POST", "/api/monitor/settings", map[string]any{"interval_ms": 2000, "timeout_ms": map[string]int{"http": 3000}}, nil); code != 200 {
		t.Errorf("settings = %d", code)
	}
	cfg.Jobs, cfg.Agents = nil, nil
	e.req("GET", "/api/monitor/config", nil, &cfg)
	if cfg.Settings.IntervalMS != 2000 || cfg.Settings.TimeoutMS[probe.TypeHTTP] != 3000 || cfg.Settings.TimeoutMS[probe.TypeTCP] == 0 {
		t.Errorf("settings = %+v", cfg.Settings)
	}
	if code, _ := e.req("POST", "/api/monitor/jobs/"+job.ID+"/interval", map[string]any{"target": job.Targets[0].Target.ID, "interval_ms": 0}, nil); code != 200 {
		t.Errorf("clear override = %d", code)
	}
	if code, _ := e.req("POST", "/api/monitor/jobs/mj-9999/interval", map[string]any{"target": "x", "interval_ms": 500}, nil); code != 404 {
		t.Errorf("interval on unknown job = %d, want 404", code)
	}

	// The live stream: probe results ingested from the monitor agent arrive
	// on /ws/monitor and in the snapshot.
	c, ctx := e.wsDial("/ws/monitor")
	var hello map[string]string
	if err := readJSON(ctx, c, &hello); err != nil || hello["type"] != "hello" {
		t.Fatalf("first frame = %v %v", hello, err)
	}
	now := time.Now()
	httpTarget := job.ID + "/http/0"
	e.link("mon1").h(context.Background(), monDataEnvelope(t, 1,
		probe.Result{TargetID: httpTarget, Type: probe.TypeHTTP, Time: now.Add(-2 * time.Second), OK: true, Latency: 120 * time.Millisecond, Status: 200},
		probe.Result{TargetID: httpTarget, Type: probe.TypeHTTP, Time: now.Add(-time.Second), OK: false, Error: "timeout"},
		probe.Result{TargetID: job.ID + "/icmp", Type: probe.TypeICMP, Time: now, OK: true, Latency: 8 * time.Millisecond},
	))
	var frame struct {
		Type   string `json:"type"`
		Points []struct {
			Agent      string  `json:"agent"`
			Target     string  `json:"target"`
			HistoryKey string  `json:"history_key"`
			Type       string  `json:"type"`
			Ms         float64 `json:"ms"`
			OK         bool    `json:"ok"`
		} `json:"points"`
	}
	if err := readJSON(ctx, c, &frame); err != nil || frame.Type != "samples" || len(frame.Points) != 3 {
		t.Fatalf("samples frame = %+v %v", frame, err)
	}
	if p := frame.Points[0]; p.Agent != "mon1" || p.Target != httpTarget || p.HistoryKey != job.HistoryKey || p.Ms != 120 || !p.OK || p.Type != "http" {
		t.Errorf("first point = %+v (identity must come from the verified link and the job)", p)
	}

	var live struct {
		WindowSec int                `json:"window_sec"`
		Series    []fleet.SeriesData `json:"series"`
		Agents    []monAgent         `json:"agents"`
		Jobs      []map[string]any   `json:"jobs"`
	}
	if code, _ := e.req("GET", "/api/monitor/live", nil, &live); code != 200 || live.WindowSec != 60 || len(live.Series) != 2 {
		t.Fatalf("live = %d %+v", code, live)
	}
	if len(live.Jobs) != 1 || len(live.Agents) != 1 || live.Agents[0].ID != "mon1" || live.Agents[0].Samples != 3 || live.Agents[0].OKPct < 66 || live.Agents[0].OKPct > 67 {
		t.Errorf("jobs=%v agents=%+v", live.Jobs, live.Agents)
	}
	// A result from outside the window is not in the snapshot.
	e.link("mon1").h(context.Background(), monDataEnvelope(t, 2, probe.Result{TargetID: httpTarget, Type: probe.TypeHTTP, Time: now.Add(-5 * time.Minute), OK: true, Latency: time.Millisecond}))
	live.Series = nil
	e.req("GET", "/api/monitor/live?window=60", nil, &live)
	for _, sd := range live.Series {
		for _, p := range sd.Points {
			if p.T.Before(now.Add(-61 * time.Second)) {
				t.Errorf("point from outside the 60 s window: %+v", p)
			}
		}
	}

	// Delete.
	if code, _ := e.req("POST", "/api/monitor/jobs/"+job.ID+"/delete", map[string]string{}, nil); code != 200 {
		t.Errorf("delete = %d", code)
	}
	if code, _ := e.req("POST", "/api/monitor/jobs/"+job.ID+"/delete", map[string]string{}, nil); code != 404 {
		t.Errorf("delete twice = %d, want 404", code)
	}

	// The page is live.
	_, _, html := e.raw("/monitor.html")
	for _, want := range []string{`id="mon-svg"`, `id="mon-cols"`, `id="mon-ovl"`, `id="mon-job-form"`, `1 of 1 monitor agents online`, "assets/js/live.js"} {
		if !strings.Contains(string(html), want) {
			t.Errorf("monitor page missing %q", want)
		}
	}
	if strings.Contains(string(html), "Mock data</span>") || strings.Contains(string(html), "data-stub") {
		t.Error("monitor page still shows mock data or stubs")
	}
}

// The command overlay: a command run through the panel shows up on the
// monitor timeline, with its packets once its evidence is in the archive.
func TestMonitorCommandOverlay(t *testing.T) {
	e := newEnv(t)
	o, err := fleet.OpenObserve(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.m.SetObserve(o)

	var sub submitResponse
	if st := e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"w1"}, "command": "echo overlay-test"}, &sub); st != http.StatusAccepted {
		t.Fatalf("submit = %d", st)
	}
	session := sub.Results[0].Session
	var live struct {
		Commands []monCommand `json:"commands"`
	}
	eventually(t, "the command on the overlay", func() bool {
		live.Commands = nil
		e.call(t, "GET", "/api/monitor/live", nil, &live)
		return len(live.Commands) == 1 && live.Commands[0].End != nil
	})
	c := live.Commands[0]
	if c.Session != session || c.Machine != "w1" || c.Command != "echo overlay-test" || c.Packets != nil {
		t.Errorf("overlay command = %+v (packets must be null before the evidence is pulled)", c)
	}

	// Its evidence arrives in the archive: the overlay now reports the packets.
	part := filepath.Join(t.TempDir(), "seg.part")
	var data []byte
	t0 := time.Now().UTC()
	for i := 0; i < 4; i++ {
		b, _ := json.Marshal(synthRecord(t, session, t0.Add(time.Duration(i)*time.Millisecond), i))
		data = append(append(data, b...), '\n')
	}
	if err := os.WriteFile(part, data, 0o640); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if _, err := o.Archive.Commit("w1", capture.SegmentEntry{File: "seg-1.ndjson", SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)),
		Records: 4, StartTime: t0, EndTime: t0.Add(3 * time.Millisecond), SchemaVersion: capture.SchemaVersion, Compression: capture.CompressionNone}, part); err != nil {
		t.Fatal(err)
	}
	live.Commands = nil
	e.call(t, "GET", "/api/monitor/live", nil, &live)
	if len(live.Commands) != 1 || live.Commands[0].Packets == nil || *live.Commands[0].Packets != 4 {
		t.Errorf("overlay after the pull = %+v", live.Commands)
	}
	// And the Evidence page lists the session with the command text from the history.
	var sess struct{ Sessions []evSession }
	e.call(t, "GET", "/api/evidence/sessions", nil, &sess)
	if len(sess.Sessions) != 1 || sess.Sessions[0].Command != "echo overlay-test" || sess.Sessions[0].Kind != "oneshot" || sess.Sessions[0].Packets != 4 {
		t.Errorf("sessions = %+v", sess.Sessions)
	}
}

// ---- Watchdog ----

func wdEnvelope(t *testing.T, ev channel.WatchdogEventPayload) channel.Envelope {
	t.Helper()
	env, err := channel.NewEnvelope(channel.TypeWatchdogEv, "", ev)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestWatchdogLive(t *testing.T) {
	e := newObsEnv(t, nil)

	// No profile yet: a starting template, which is itself valid.
	var pol struct {
		HasProfile bool            `json:"has_profile"`
		Version    int             `json:"version"`
		YAML       string          `json:"yaml"`
		Effective  watchdog.Policy `json:"effective"`
	}
	if code, _ := e.req("GET", "/api/watchdog/policy", nil, &pol); code != 200 || pol.HasProfile || pol.Version != 0 || !strings.Contains(pol.YAML, "version: 1") {
		t.Fatalf("policy = %d %+v", code, pol)
	}
	if _, err := watchdog.ParseProfile([]byte(pol.YAML)); err != nil {
		t.Fatalf("the starting template is not a valid profile: %v", err)
	}
	if pol.Effective.Caps.RestartsPerDay != 3 {
		t.Errorf("effective = %+v", pol.Effective)
	}

	// Save: invalid is refused (and nothing pushed), valid is stored and pushed to the online worker only.
	if code, _ := e.req("POST", "/api/watchdog/policy", map[string]string{"yaml": "version: 1\nsample_interval: nope\n"}, nil); code != 400 {
		t.Errorf("invalid policy = %d, want 400", code)
	}
	if code, _ := e.req("POST", "/api/watchdog/policy", map[string]string{"yaml": ""}, nil); code != 400 {
		t.Errorf("empty policy = %d, want 400", code)
	}
	if n := len(e.link("w1").sentOfType(channel.TypePolicy)); n != 0 {
		t.Fatalf("%d policies pushed before any save", n)
	}
	yamlV1 := strings.Replace(pol.YAML, "cpu_percent:\n    trigger: 97", "cpu_percent:\n    trigger: 98", 1)
	var saved struct {
		Version  int `json:"version"`
		PushedTo int `json:"pushed_to"`
	}
	if code, _ := e.req("POST", "/api/watchdog/policy", map[string]string{"yaml": yamlV1}, &saved); code != 200 || saved.Version != 1 || saved.PushedTo != 1 {
		t.Fatalf("save = %d %+v", code, saved)
	}
	eventually(t, "policy pushed to w1", func() bool { return len(e.link("w1").sentOfType(channel.TypePolicy)) == 1 })
	var pp channel.PolicyPayload
	if err := json.Unmarshal(e.link("w1").sentOfType(channel.TypePolicy)[0].Payload, &pp); err != nil || pp.Version != 1 || !strings.Contains(pp.YAML, "trigger: 98") {
		t.Errorf("pushed policy = %+v %v", pp, err)
	}
	if n := len(e.link("w2").sentOfType(channel.TypePolicy)); n != 0 {
		t.Errorf("an offline agent was sent a policy")
	}
	// Same version again: refused, a save must bump it.
	if code, _ := e.req("POST", "/api/watchdog/policy", map[string]string{"yaml": yamlV1}, nil); code != 409 {
		t.Errorf("same version = %d, want 409", code)
	}
	pol.HasProfile, pol.YAML = false, ""
	e.req("GET", "/api/watchdog/policy", nil, &pol)
	if !pol.HasProfile || pol.Version != 1 || !strings.Contains(pol.YAML, "trigger: 98") {
		t.Errorf("stored policy = %+v", pol)
	}
	// An agent that connects later gets the stored profile with its config.
	e.link("w1").h(context.Background(), channel.Envelope{Type: channel.TypeConfigReq})
	eventually(t, "policy on config-req", func() bool { return len(e.link("w1").sentOfType(channel.TypePolicy)) == 2 })

	// The agent acknowledges; the hosts view shows the policy in force.
	ack, _ := channel.NewEnvelope(channel.TypePolicyAck, "policy", channel.PolicyAckPayload{OK: true, Version: 1})
	e.link("w1").h(context.Background(), ack)
	var hosts struct {
		Summary wdSummary      `json:"summary"`
		Hosts   []hostState    `json:"hosts"`
		Policy  map[string]any `json:"policy"`
	}
	eventually(t, "ack in the hosts view", func() bool {
		hosts.Hosts = nil
		e.req("GET", "/api/watchdog", nil, &hosts)
		return len(hosts.Hosts) == 2 && hosts.Hosts[0].PolicyAcked
	})
	if h := hosts.Hosts[0]; h.Machine != "w1" || !h.PolicyOK || h.PolicyInForc != 1 || h.Condition != "normal" || h.RestartsX != 3 || h.RebootsX != 1 {
		t.Errorf("w1 = %+v", h)
	}
	if h := hosts.Hosts[1]; h.Machine != "w2" || h.Condition != "not-reporting" {
		t.Errorf("w2 = %+v", h)
	}
	if hosts.Summary.Normal != 1 || hosts.Summary.NotReporting != 1 || hosts.Policy["in_force"].(float64) != 1 || hosts.Policy["version"].(float64) != 1 {
		t.Errorf("summary = %+v policy = %v", hosts.Summary, hosts.Policy)
	}

	// The live stream: an event forwarded by the agent arrives on /ws/watchdog,
	// and moves the host's state.
	c, ctx := e.wsDial("/ws/watchdog")
	var hello map[string]string
	if err := readJSON(ctx, c, &hello); err != nil || hello["type"] != "hello" {
		t.Fatalf("first frame = %v %v", hello, err)
	}
	t0 := time.Now()
	e.link("w1").h(context.Background(), wdEnvelope(t, channel.WatchdogEventPayload{Time: t0, Kind: watchdog.EventBreach, Scope: "machine", Metric: "cpu_percent", Value: 99}))
	e.link("w1").h(context.Background(), wdEnvelope(t, channel.WatchdogEventPayload{Time: t0.Add(time.Second), Kind: watchdog.EventAction, Scope: "machine", Metric: "cpu_percent", Target: "session s-7", Step: "throttle", Detail: "CPUQuota 50% for 2m"}))
	for i := 0; i < 2; i++ {
		var f struct {
			Type  string              `json:"type"`
			Event fleet.WatchdogEvent `json:"event"`
		}
		if err := readJSON(ctx, c, &f); err != nil || f.Type != "event" || f.Event.Machine != "w1" {
			t.Fatalf("event frame %d = %+v %v", i, f, err)
		}
		if i == 1 && (f.Event.Step != "throttle" || f.Event.Detail == "") {
			t.Errorf("action event = %+v", f.Event)
		}
	}
	hosts.Hosts = nil
	e.req("GET", "/api/watchdog", nil, &hosts)
	if h := hosts.Hosts[0]; h.Condition != "throttled" || h.Stage != "throttle" || h.Level != 1 || h.LastEvent == nil || h.LastEvent.Kind != watchdog.EventAction {
		t.Errorf("w1 after the throttle = %+v", h)
	}
	if hosts.Summary.Acting != 1 || hosts.Summary.Normal != 0 {
		t.Errorf("summary = %+v", hosts.Summary)
	}
	var evs struct{ Events []fleet.WatchdogEvent }
	if code, _ := e.req("GET", "/api/watchdog/events", nil, &evs); code != 200 || len(evs.Events) != 2 || evs.Events[0].Kind != watchdog.EventAction {
		t.Errorf("events = %d %+v (newest first)", code, evs.Events)
	}
	evs.Events = nil
	e.req("GET", "/api/watchdog/events?machine=w2", nil, &evs)
	if len(evs.Events) != 0 {
		t.Errorf("w2 events = %+v", evs.Events)
	}

	// The page is live, and the unbacked counter reset stays a visible stub with the reason.
	_, _, html := e.raw("/watchdog.html")
	for _, want := range []string{`id="wd-hosts"`, `id="wd-yaml"`, `id="wd-policy-form"`, `id="wd-events"`, "assets/js/live.js", "control-channel message"} {
		if !strings.Contains(string(html), want) {
			t.Errorf("watchdog page missing %q", want)
		}
	}
	if strings.Contains(string(html), "Mock data</span>") || strings.Contains(string(html), "wrk-0") {
		t.Error("watchdog page still shows mock data")
	}
}

func TestDeriveHost(t *testing.T) {
	now := time.Date(2026, 11, 25, 15, 0, 0, 0, time.Local)
	at := func(h, m int) time.Time { return time.Date(2026, 11, 25, h, m, 0, 0, time.Local) }
	ev := func(t time.Time, kind, step, target string) fleet.WatchdogEvent {
		return fleet.WatchdogEvent{Machine: "w", WatchdogEventPayload: channel.WatchdogEventPayload{Time: t, Kind: kind, Step: step, Target: target, Scope: "machine", Metric: "cpu_percent"}}
	}
	// Restarts and reboots count only for today; alert-only latches; a counter reset clears it.
	events := []fleet.WatchdogEvent{
		ev(now.Add(-48*time.Hour), watchdog.EventAction, "restart", "u"),
		ev(at(9, 0), watchdog.EventAction, "restart", "u"),
		ev(at(10, 0), watchdog.EventAction, "restart", "u"),
		ev(at(11, 0), watchdog.EventAction, "reboot", ""),
		ev(at(11, 5), watchdog.EventAlertOnly, "reboot", ""),
	}
	h := deriveHost(events, true, now)
	if h.Restarts != 2 || h.Reboots != 1 || h.Condition != "alert-only" || h.Stage != "reboot" || h.Level != 4 {
		t.Errorf("state = %+v", h)
	}
	h = deriveHost(append(events, ev(at(12, 0), watchdog.EventCounters, "", "")), true, now)
	if h.Restarts != 0 || h.Reboots != 0 || h.Condition != "normal" || h.Stage != "none" {
		t.Errorf("after reset = %+v", h)
	}
	// A throttle that was lifted, and a breach that recovered, leave the host normal.
	h = deriveHost([]fleet.WatchdogEvent{
		ev(at(9, 0), watchdog.EventBreach, "", ""), ev(at(9, 1), watchdog.EventAction, "throttle", "s-1"),
		ev(at(9, 5), watchdog.EventLifted, "", "s-1"), ev(at(9, 6), watchdog.EventRecovered, "", ""),
	}, true, now)
	if h.Condition != "normal" || h.Stage != "throttle" {
		t.Errorf("lifted = %+v", h)
	}
	// A breach alone is "breaching".
	h = deriveHost([]fleet.WatchdogEvent{ev(at(9, 0), watchdog.EventBreach, "", "")}, true, now)
	if h.Condition != "breaching" || len(h.Breaches) != 1 {
		t.Errorf("breach = %+v", h)
	}
	// Midnight rollover keeps alert-only only when auto-reset is off.
	roll := []fleet.WatchdogEvent{ev(at(1, 0), watchdog.EventAlertOnly, "", ""), ev(at(2, 0), watchdog.EventDay, "", "")}
	if h := deriveHost(roll, true, now); h.Condition != "normal" {
		t.Errorf("auto-reset rollover = %+v", h)
	}
	if h := deriveHost(roll, false, now); h.Condition != "alert-only" {
		t.Errorf("no auto-reset rollover = %+v", h)
	}
}

// The panel still serves every page, and none of the live Observe pages
// carries the mock marker; the pages that depend on the deferred Load
// Controller remain mock.
func TestAllObservePagesLive(t *testing.T) {
	e := newObsEnv(t, nil)
	for _, name := range []string{"evidence", "backup", "monitor", "watchdog"} {
		code, _, html := e.raw("/" + name + ".html")
		s := string(html)
		if code != 200 || strings.Contains(s, "Mock data</span>") || !strings.Contains(s, "assets/js/live.js") ||
			!strings.Contains(s, "theme-toggle") || !strings.Contains(s, `data-page="`+name+`"`) {
			t.Errorf("%s: code=%d mock=%v live.js=%v", name, code, strings.Contains(s, "Mock data</span>"), strings.Contains(s, "assets/js/live.js"))
		}
	}
	for _, name := range []string{"projects", "baseline", "maneuver"} {
		_, _, html := e.raw("/" + name + ".html")
		if !strings.Contains(string(html), "Mock data</span>") {
			t.Errorf("%s should remain a mock page", name)
		}
	}
}
