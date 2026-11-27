package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	exe "github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

// Monitor agents run a headless watchdog (FLEET_WATCHDOG_MODE=headless): their
// state and events must appear in the Watchdog panel alongside workers and be
// labeled as monitor agents.
func TestWatchdogMonitorAgentsVisible(t *testing.T) {
	e := newObsEnv(t, nil)

	c, ctx := e.wsDial("/ws/watchdog")
	var hello map[string]string
	if err := readJSON(ctx, c, &hello); err != nil || hello["type"] != "hello" {
		t.Fatalf("first frame = %v %v", hello, err)
	}

	now := time.Now()
	e.link("mon1").h(context.Background(), wdEnvelope(t, channel.WatchdogEventPayload{
		Time:   now.Add(-time.Second),
		Kind:   watchdog.EventBreach,
		Scope:  "machine",
		Metric: "memory_percent",
		Value:  97.2,
	}))
	e.link("mon1").h(context.Background(), wdEnvelope(t, channel.WatchdogEventPayload{
		Time:   now,
		Kind:   watchdog.EventAction,
		Scope:  "machine",
		Metric: "memory_percent",
		Step:   "restart",
		Target: "service:fleet-agent.service",
		Detail: "issued",
	}))

	for i := 0; i < 2; i++ {
		var frame struct {
			Type  string              `json:"type"`
			Event fleet.WatchdogEvent `json:"event"`
		}
		if err := readJSON(ctx, c, &frame); err != nil || frame.Type != "event" || frame.Event.Machine != "mon1" {
			t.Fatalf("ws frame %d = %+v %v", i, frame, err)
		}
	}

	var hosts struct {
		Summary wdSummary      `json:"summary"`
		Hosts   []hostState    `json:"hosts"`
		Policy  map[string]any `json:"policy"`
	}
	if code, _ := e.req("GET", "/api/watchdog", nil, &hosts); code != 200 {
		t.Fatalf("GET /api/watchdog = %d", code)
	}
	if got := hosts.Policy["workers"].(float64); got != 2 {
		t.Errorf("policy.workers = %v, want 2 (workers only)", got)
	}
	var mon *hostState
	for i := range hosts.Hosts {
		if hosts.Hosts[i].Machine == "mon1" {
			mon = &hosts.Hosts[i]
		}
	}
	if mon == nil {
		t.Fatalf("mon1 missing from /api/watchdog hosts: %+v", hosts.Hosts)
	}
	if mon.Role != registry.RoleMonitor || !mon.Headless || mon.Condition != "breaching" ||
		mon.Stage != "restart" || mon.Level != 3 || mon.Restarts != 1 || mon.LastEvent == nil ||
		mon.LastEvent.Kind != watchdog.EventAction {
		t.Errorf("mon1 host state = %+v", mon)
	}

	var evs struct {
		Events []fleet.WatchdogEvent `json:"events"`
	}
	if code, _ := e.req("GET", "/api/watchdog/events?machine=mon1", nil, &evs); code != 200 || len(evs.Events) != 2 {
		t.Fatalf("GET /api/watchdog/events?machine=mon1 = %d %+v", code, evs)
	}
	if evs.Events[0].Kind != watchdog.EventAction || evs.Events[1].Kind != watchdog.EventBreach {
		t.Errorf("monitor events order/content = %+v", evs.Events)
	}
}

// Host watchdog stage + daily counters are persisted in registry.Machine and
// survive a fleet-server restart even when the in-memory event ring is empty.
func TestWatchdogHostStatePersistedAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	store := registry.NewStore(dataDir)
	reg := registry.New()
	for _, mc := range []registry.Machine{
		{ID: "w1", Role: registry.RoleWorker, PrivateIPs: []string{"10.0.0.1"}, Provisioned: true},
		{ID: "mon1", Role: registry.RoleMonitor, PrivateIPs: []string{"10.0.0.2"}, Provisioned: true},
	} {
		if err := reg.Add(mc); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}

	gate, err := exe.NewGate([]byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	startServer := func(r *registry.Registry) (*httptest.Server, *fleet.Manager, map[string]*obsLink, func()) {
		var mu sync.Mutex
		links := map[string]*obsLink{}
		m, err := fleet.New(fleet.Config{
			Registry: r, Gate: gate, DataDir: dataDir, PollInterval: 10 * time.Millisecond, Logger: log,
			NewLink: func(mc registry.Machine, h channel.Handler) (fleet.Link, error) {
				l := &obsLink{h: h, up: true}
				mu.Lock()
				links[mc.ID] = l
				mu.Unlock()
				return l, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		o, err := fleet.OpenObserve(dataDir, log)
		if err != nil {
			t.Fatal(err)
		}
		m.SetObserve(o)
		ctx, cancel := context.WithCancel(context.Background())
		m.Start(ctx)
		eventually(t, "online", func() bool {
			w, ok1 := m.Machine("w1")
			mn, ok2 := m.Machine("mon1")
			return ok1 && ok2 && w.State == fleet.StateOnline && mn.State == fleet.StateOnline
		})
		h, err := New(Options{Assets: web.FS, Templates: web.Templates, Backend: m, Logger: log})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h)
		return srv, m, links, func() { srv.Close(); cancel(); m.Close() }
	}

	fetchHosts := func(srv *httptest.Server) map[string]hostState {
		resp, err := srv.Client().Get(srv.URL + "/api/watchdog")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET /api/watchdog = %d", resp.StatusCode)
		}
		var body struct {
			Summary wdSummary   `json:"summary"`
			Hosts   []hostState `json:"hosts"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		out := map[string]hostState{}
		for _, h := range body.Hosts {
			out[h.Machine] = h
		}
		return out
	}

	// Run 1: send watchdog events for both w1 and mon1, then shut down.
	srv1, _, links1, stop1 := startServer(reg)
	now := time.Now()
	for _, ev := range []channel.WatchdogEventPayload{
		{Time: now.Add(-4 * time.Second), Kind: watchdog.EventBreach, Scope: "machine", Metric: "cpu_percent", Value: 98},
		{Time: now.Add(-3 * time.Second), Kind: watchdog.EventAction, Scope: "machine", Metric: "cpu_percent", Step: "restart", Target: "service:fleet-agent.service"},
		{Time: now.Add(-2 * time.Second), Kind: watchdog.EventAction, Scope: "machine", Metric: "cpu_percent", Step: "restart", Target: "service:fleet-agent.service"},
		{Time: now.Add(-time.Second), Kind: watchdog.EventAction, Scope: "machine", Metric: "cpu_percent", Step: "reboot", Target: "machine"},
		{Time: now, Kind: watchdog.EventAlertOnly, Scope: "machine", Metric: "cpu_percent", Step: "reboot", Detail: "daily reboot cap (1) exhausted"},
	} {
		links1["w1"].h(context.Background(), wdEnvelope(t, ev))
	}
	links1["mon1"].h(context.Background(), wdEnvelope(t, channel.WatchdogEventPayload{
		Time: now, Kind: watchdog.EventAction, Scope: "machine", Metric: "memory_percent", Step: "restart", Target: "service:fleet-agent.service",
	}))
	before := fetchHosts(srv1)
	if before["w1"].Restarts != 2 || before["w1"].Reboots != 1 || before["mon1"].Restarts != 1 {
		t.Fatalf("pre-restart hosts = %+v", before)
	}
	stop1()

	// Run 2: reload registry from disk into a fresh server (empty in-memory events).
	reg2, err := registry.NewStore(dataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	srv2, m2, links2, stop2 := startServer(reg2)
	defer stop2()

	if len(m2.WatchdogEvents("w1")) != 0 || len(m2.WatchdogEvents("mon1")) != 0 {
		t.Fatalf("expected empty in-memory event rings after restart")
	}

	byID := fetchHosts(srv2)
	w1 := byID["w1"]
	if w1.Stage != "reboot" || w1.Level != 4 || w1.Restarts != 2 || w1.Reboots != 1 ||
		w1.Condition != "alert-only" || len(w1.Breaches) != 1 || w1.LastEvent == nil ||
		w1.LastEvent.Kind != watchdog.EventAlertOnly {
		t.Fatalf("w1 after restart = %+v", w1)
	}
	mon1 := byID["mon1"]
	if mon1.Role != registry.RoleMonitor || !mon1.Headless || mon1.Stage != "restart" ||
		mon1.Level != 3 || mon1.Restarts != 1 || mon1.Reboots != 0 {
		t.Fatalf("mon1 after restart = %+v", mon1)
	}

	// Live events after restart continue updating from the persisted state.
	links2["mon1"].h(context.Background(), wdEnvelope(t, channel.WatchdogEventPayload{
		Time: now.Add(2 * time.Second), Kind: watchdog.EventAction, Scope: "machine", Metric: "memory_percent", Step: "restart", Target: "service:fleet-agent.service",
	}))
	byID = fetchHosts(srv2)
	if byID["mon1"].Restarts != 2 {
		t.Errorf("mon1 restarts after post-restart event = %d, want 2", byID["mon1"].Restarts)
	}

	// Counter reset clears stage, daily counters, and alert-only latch; an active
	// breach keeps condition "breaching" until recovered.
	links2["w1"].h(context.Background(), wdEnvelope(t, channel.WatchdogEventPayload{
		Time: now.Add(3 * time.Second), Kind: watchdog.EventCounters, Detail: "counters reset",
	}))
	byID = fetchHosts(srv2)
	if w1 = byID["w1"]; w1.Stage != "none" || w1.Restarts != 0 || w1.Reboots != 0 || w1.Condition != "breaching" {
		t.Errorf("w1 after reset = %+v", w1)
	}
	links2["w1"].h(context.Background(), wdEnvelope(t, channel.WatchdogEventPayload{
		Time: now.Add(4 * time.Second), Kind: watchdog.EventRecovered, Scope: "machine", Metric: "cpu_percent", Value: 80,
	}))
	byID = fetchHosts(srv2)
	if w1 = byID["w1"]; w1.Condition != "normal" || len(w1.Breaches) != 0 {
		t.Errorf("w1 after recovery = %+v", w1)
	}

	// Counters remain scoped to "today": yesterday's counts read as 0 today.
	yesterday := now.AddDate(0, 0, -1).In(time.Local).Format("2006-01-02")
	stale := hostStateFromWatchdogState(&registry.WatchdogState{
		Day:           yesterday,
		Stage:         "restart",
		RestartsToday: 3,
		RebootsToday:  1,
		AlertOnly:     true,
	}, now)
	if stale.Restarts != 0 || stale.Reboots != 0 || stale.Stage != "restart" || stale.Condition != "alert-only" {
		t.Errorf("yesterday's persisted state = %+v, want today's counters 0", stale)
	}
}

func TestWatchdogGroupsAssignmentPersistenceAndResolution(t *testing.T) {
	dataDir := t.TempDir()
	store := registry.NewStore(dataDir)
	reg := registry.New()
	for _, mc := range []registry.Machine{
		{ID: "w1", Role: registry.RoleWorker, PrivateIPs: []string{"10.0.0.1"}, Provisioned: true},
		{ID: "w2", Role: registry.RoleWorker, PrivateIPs: []string{"10.0.0.2"}, Provisioned: true},
		{ID: "gone", Role: registry.RoleWorker, PrivateIPs: []string{"10.0.0.9"}, Removed: true},
	} {
		if err := reg.Add(mc); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}

	gate, err := exe.NewGate([]byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	startServer := func(r *registry.Registry) (*httptest.Server, *fleet.Manager, func()) {
		m, err := fleet.New(fleet.Config{
			Registry: r, Gate: gate, DataDir: dataDir, PollInterval: 10 * time.Millisecond, Logger: log,
			NewLink: func(mc registry.Machine, h channel.Handler) (fleet.Link, error) {
				return &obsLink{h: h, up: true}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		o, err := fleet.OpenObserve(dataDir, log)
		if err != nil {
			t.Fatal(err)
		}
		m.SetObserve(o)
		ctx, cancel := context.WithCancel(context.Background())
		m.Start(ctx)
		eventually(t, "online", func() bool {
			w, ok1 := m.Machine("w1")
			w2, ok2 := m.Machine("w2")
			return ok1 && ok2 && w.State == fleet.StateOnline && w2.State == fleet.StateOnline
		})
		h, err := New(Options{Assets: web.FS, Templates: web.Templates, Backend: m, Logger: log})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h)
		return srv, m, func() { srv.Close(); cancel(); m.Close() }
	}

	postJSON := func(srv *httptest.Server, path string, in any, out any) int {
		t.Helper()
		b, _ := json.Marshal(in)
		req, err := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode
	}

	fetchHosts := func(srv *httptest.Server) map[string]hostState {
		t.Helper()
		resp, err := srv.Client().Get(srv.URL + "/api/watchdog")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/watchdog = %d", resp.StatusCode)
		}
		var body struct {
			Hosts []hostState `json:"hosts"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		out := map[string]hostState{}
		for _, h := range body.Hosts {
			out[h.Machine] = h
		}
		return out
	}

	srv1, _, stop1 := startServer(reg)
	policyYAML := `version: 1
caps: {restarts_per_day: 3, reboots_per_day: 1}
throttle: {cpu_percent: 50}
overrides:
  groups:
    edge:
      throttle: {cpu_percent: 30}
      caps: {restarts_per_day: 5}
    gpu:
      caps: {restarts_per_day: 7, reboots_per_day: 2}
  hosts:
    w2:
      caps: {restarts_per_day: 2}
`
	if code := postJSON(srv1, "/api/watchdog/policy", map[string]string{"yaml": policyYAML}, nil); code != http.StatusOK {
		t.Fatalf("POST /api/watchdog/policy = %d", code)
	}

	// Validation & error cases.
	for name, tc := range map[string]struct {
		id   string
		body any
		want int
	}{
		"invalid group": {id: "w1", body: map[string]any{"groups": []string{"bad/name"}}, want: http.StatusBadRequest},
		"empty group":   {id: "w1", body: map[string]any{"groups": []string{"  "}}, want: http.StatusBadRequest},
		"dup group":     {id: "w1", body: map[string]any{"groups": []string{"edge", "edge"}}, want: http.StatusBadRequest},
		"unknown field": {id: "w1", body: map[string]any{"groups": []string{"edge"}, "extra": 1}, want: http.StatusBadRequest},
		"unknown host":  {id: "nope", body: map[string]any{"groups": []string{"edge"}}, want: http.StatusNotFound},
		"removed host":  {id: "gone", body: map[string]any{"groups": []string{"edge"}}, want: http.StatusConflict},
	} {
		if got := postJSON(srv1, "/api/machines/"+tc.id+"/groups", tc.body, nil); got != tc.want {
			t.Errorf("%s: got %d, want %d", name, got, tc.want)
		}
	}

	// Assign groups: w1 -> [edge, gpu] (later group gpu wins on restarts_per_day=7, edge sets throttle=30).
	// w2 -> [edge] (host w2 override restarts_per_day=2 beats edge=5, edge still sets throttle=30).
	var resp1 struct {
		Machine fleet.MachineInfo `json:"machine"`
	}
	if code := postJSON(srv1, "/api/machines/w1/groups", map[string]any{"groups": []string{" edge ", "gpu"}}, &resp1); code != http.StatusOK {
		t.Fatalf("POST /api/machines/w1/groups = %d", code)
	}
	if len(resp1.Machine.Groups) != 2 || resp1.Machine.Groups[0] != "edge" || resp1.Machine.Groups[1] != "gpu" {
		t.Fatalf("w1 machine groups = %v, want [edge gpu]", resp1.Machine.Groups)
	}
	if code := postJSON(srv1, "/api/machines/w2/groups", map[string]any{"groups": []string{"edge"}}, nil); code != http.StatusOK {
		t.Fatalf("POST /api/machines/w2/groups = %d", code)
	}

	hosts1 := fetchHosts(srv1)
	if w1 := hosts1["w1"]; len(w1.Groups) != 2 || w1.RestartsX != 7 || w1.RebootsX != 2 ||
		w1.EffectivePolicy == nil || w1.EffectivePolicy.Throttle.CPUPercent != 30 {
		t.Fatalf("w1 watchdog host = %+v (eff=%+v)", w1, w1.EffectivePolicy)
	}
	if w2 := hosts1["w2"]; len(w2.Groups) != 1 || w2.Groups[0] != "edge" || w2.RestartsX != 2 ||
		w2.EffectivePolicy == nil || w2.EffectivePolicy.Throttle.CPUPercent != 30 {
		t.Fatalf("w2 watchdog host = %+v (eff=%+v)", w2, w2.EffectivePolicy)
	}
	stop1()

	// Restart server from disk: group assignments and effective-policy resolution survive.
	reg2, err := registry.NewStore(dataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	srv2, _, stop2 := startServer(reg2)
	defer stop2()

	hosts2 := fetchHosts(srv2)
	if w1 := hosts2["w1"]; len(w1.Groups) != 2 || w1.RestartsX != 7 || w1.RebootsX != 2 ||
		w1.EffectivePolicy == nil || w1.EffectivePolicy.Throttle.CPUPercent != 30 {
		t.Fatalf("w1 after restart = %+v", w1)
	}
	if w2 := hosts2["w2"]; len(w2.Groups) != 1 || w2.Groups[0] != "edge" || w2.RestartsX != 2 ||
		w2.EffectivePolicy == nil || w2.EffectivePolicy.Throttle.CPUPercent != 30 {
		t.Fatalf("w2 after restart = %+v", w2)
	}

	// Clearing w1 groups reverts to default policy.
	if code := postJSON(srv2, "/api/machines/w1/groups", map[string]any{"groups": []string{}}, nil); code != http.StatusOK {
		t.Fatalf("clear w1 groups = %d", code)
	}
	hostsAfterClear := fetchHosts(srv2)
	if w1 := hostsAfterClear["w1"]; len(w1.Groups) != 0 || w1.RestartsX != 3 || w1.RebootsX != 1 ||
		w1.EffectivePolicy == nil || w1.EffectivePolicy.Throttle.CPUPercent != 50 {
		t.Fatalf("w1 after clear = %+v", w1)
	}

	// Bundle includes Watchdog groups UI on Machines and Watchdog pages.
	_, _, js := body(t, srv2.URL+"/assets/dist/app.js")
	for _, want := range []string{"Watchdog groups", "/groups", "wd-eff-host", "Save groups"} {
		if !strings.Contains(js, want) {
			t.Errorf("SPA bundle lacks %q", want)
		}
	}

	// Without backend -> 503.
	nb := noBackend(t)
	req, _ := http.NewRequest("POST", nb.URL+"/api/machines/w1/groups", bytes.NewReader([]byte(`{"groups":["edge"]}`)))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("no-backend POST groups = %d, want 503", resp.StatusCode)
		}
	}
}
