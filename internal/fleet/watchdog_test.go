package fleet_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

func newWatchdogMgr(t *testing.T, src fleet.WatchdogSource, late bool) (*fleet.Manager, map[string]*recLink) {
	t.Helper()
	reg := registry.New()
	for id, role := range map[string]string{"mon1": registry.RoleMonitor, "w1": registry.RoleWorker, "w2": registry.RoleWorker} {
		if err := reg.Add(registry.Machine{ID: id, Role: role, PrivateIPs: []string{"10.0.0.1"}, Provisioned: true}); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := exec.NewGate([]byte("s"), nil)
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]*recLink{}
	var lmu sync.Mutex
	cfg := fleet.Config{Registry: reg, Gate: gate, PollInterval: 10 * time.Millisecond,
		NewLink: func(mc registry.Machine, h channel.Handler) (fleet.Link, error) {
			lk := &recLink{h: h}
			lmu.Lock()
			links[mc.ID] = lk
			lmu.Unlock()
			return lk, nil
		}}
	if !late {
		cfg.Watchdog = src
	}
	m, err := fleet.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })
	if late {
		m.SetWatchdog(src)
	}
	eventually(t, "links", func() bool {
		lmu.Lock()
		defer lmu.Unlock()
		return len(links) == 3
	})
	return m, links
}

func TestPolicyPushedToWorkersOnConnectAndEdit(t *testing.T) {
	var mu sync.Mutex
	ver := 1
	src := fleet.WatchdogSource{PolicyFor: func(machine string) (fleet.WatchdogPolicy, bool) {
		mu.Lock()
		defer mu.Unlock()
		if machine == "w2" {
			return fleet.WatchdogPolicy{}, false // no policy for this one
		}
		return fleet.WatchdogPolicy{Version: ver, YAML: "version: " + string(rune('0'+ver)) + "\n"}, true
	}}
	for _, late := range []bool{false, true} { // bound at New, and bound later by SetWatchdog
		mu.Lock()
		ver = 1
		mu.Unlock()
		m, links := newWatchdogMgr(t, src, late)
		ctx := context.Background()
		for _, id := range []string{"mon1", "w1", "w2"} {
			links[id].h(ctx, envOf(t, channel.TypeConfigReq, nil))
		}
		eventually(t, "policy pushed to w1", func() bool { return len(links["w1"].sentOfType(channel.TypePolicy)) == 1 })
		var p channel.PolicyPayload
		env := links["w1"].sentOfType(channel.TypePolicy)[0]
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Version != 1 || p.YAML != "version: 1\n" || env.ID == "" {
			t.Errorf("late=%v: pushed %+v (id %q)", late, p, env.ID)
		}
		if n := len(links["mon1"].sentOfType(channel.TypePolicy)); n != 0 {
			t.Errorf("late=%v: a monitor agent was sent %d policies", late, n)
		}
		if n := len(links["w2"].sentOfType(channel.TypePolicy)); n != 0 {
			t.Errorf("late=%v: w2 has no policy but was sent %d", late, n)
		}

		// An edit is pushed to every connected worker that has a policy.
		mu.Lock()
		ver = 2
		mu.Unlock()
		eventually(t, "online", func() bool { info, _ := m.Machine("w1"); return info.State == fleet.StateOnline })
		m.PushPolicies(ctx)
		if n := len(links["w1"].sentOfType(channel.TypePolicy)); n != 2 {
			t.Errorf("late=%v: w1 got %d policies after the edit, want 2", late, n)
		}
		if err := m.PushPolicy(ctx, "w1"); err != nil {
			t.Errorf("PushPolicy(w1): %v", err)
		}
		if err := m.PushPolicy(ctx, "mon1"); err == nil {
			t.Error("PushPolicy to a monitor agent succeeded")
		}
		if st, ok := m.WatchdogStatus("w1"); !ok || st.Pushed != 2 || st.Pushes != 3 {
			t.Errorf("late=%v: status %+v", late, st)
		}
	}
}

func TestPolicyAckRecordedAndRejectionSurfaced(t *testing.T) {
	src := fleet.WatchdogSource{PolicyFor: func(string) (fleet.WatchdogPolicy, bool) {
		return fleet.WatchdogPolicy{Version: 3, YAML: "version: 3\n"}, true
	}}
	m, links := newWatchdogMgr(t, src, false)
	ctx := context.Background()
	links["w1"].h(ctx, envOf(t, channel.TypeConfigReq, nil))
	eventually(t, "pushed", func() bool { return len(links["w1"].sentOfType(channel.TypePolicy)) == 1 })

	links["w1"].h(ctx, envOf(t, channel.TypePolicyAck, channel.PolicyAckPayload{OK: true, Version: 3}))
	if st, _ := m.WatchdogStatus("w1"); !st.Acked || !st.AckOK || st.InForce != 3 || st.AckErr != "" {
		t.Errorf("after an ok ack: %+v", st)
	}
	links["w1"].h(ctx, envOf(t, channel.TypePolicyAck, channel.PolicyAckPayload{OK: false, Error: "policy: bad", Version: 3}))
	if st, _ := m.WatchdogStatus("w1"); st.AckOK || st.AckErr != "policy: bad" || st.InForce != 3 {
		t.Errorf("after a refusal: %+v", st)
	}
	links["mon1"].h(ctx, envOf(t, channel.TypePolicyAck, channel.PolicyAckPayload{OK: true, Version: 9}))
	if _, ok := m.WatchdogStatus("mon1"); ok {
		t.Error("a monitor agent's policy-ack was recorded")
	}
}

func TestWatchdogEventsStoredUnderVerifiedIDAndDeduplicated(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	src := fleet.WatchdogSource{OnEvent: func(machine string, ev fleet.WatchdogEvent) {
		mu.Lock()
		seen = append(seen, machine+":"+ev.Kind)
		mu.Unlock()
	}}
	m, links := newWatchdogMgr(t, src, false)
	ctx := context.Background()
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	ev := channel.WatchdogEventPayload{Time: at, Kind: "action", Scope: "machine", Metric: "cpu", Step: "throttle", Target: "unit a.service", Value: 97}
	links["w1"].h(ctx, envOf(t, channel.TypeWatchdogEv, ev))
	links["w1"].h(ctx, envOf(t, channel.TypeWatchdogEv, ev)) // at-least-once delivery: a repeat
	ev2 := ev
	ev2.Time, ev2.Kind = at.Add(time.Second), "alert-only"
	links["w1"].h(ctx, envOf(t, channel.TypeWatchdogEv, ev2))
	links["mon1"].h(ctx, envOf(t, channel.TypeWatchdogEv, ev)) // monitor agents run a headless watchdog too

	got := m.WatchdogEvents("w1")
	if len(got) != 2 || got[0].Kind != "action" || got[1].Kind != "alert-only" || got[0].Machine != "w1" ||
		got[0].Step != "throttle" || got[0].Value != 97 || got[0].Received.IsZero() {
		t.Errorf("stored %+v, want the two distinct events in order", got)
	}
	if monGot := m.WatchdogEvents("mon1"); len(monGot) != 1 || monGot[0].Machine != "mon1" || monGot[0].Kind != "action" {
		t.Errorf("monitor agent events = %+v, want 1 stored event under mon1", monGot)
	}
	if n := len(m.WatchdogEvents("w2")); n != 0 {
		t.Errorf("w2 has %d events", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0] != "w1:action" || seen[1] != "w1:alert-only" || seen[2] != "mon1:action" {
		t.Errorf("OnEvent saw %v", seen)
	}
}

func TestWatchdogEventsAreBounded(t *testing.T) {
	m, links := newWatchdogMgr(t, fleet.WatchdogSource{}, false)
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 250; i++ {
		links["w1"].h(context.Background(), envOf(t, channel.TypeWatchdogEv,
			channel.WatchdogEventPayload{Time: at.Add(time.Duration(i) * time.Second), Kind: "breach"}))
	}
	got := m.WatchdogEvents("w1")
	if len(got) != 200 || !got[len(got)-1].Time.Equal(at.Add(249*time.Second)) || !got[0].Time.Equal(at.Add(50*time.Second)) {
		t.Errorf("kept %d events, first %v, want the newest 200", len(got), got[0].Time)
	}
}

func TestWatchdogWithoutSourceIsHarmless(t *testing.T) {
	m, links := newWatchdogMgr(t, fleet.WatchdogSource{}, false)
	links["w1"].h(context.Background(), envOf(t, channel.TypeConfigReq, nil))
	time.Sleep(100 * time.Millisecond)
	if n := len(links["w1"].sentOfType(channel.TypePolicy)); n != 0 {
		t.Errorf("pushed %d policies without a source", n)
	}
	if _, ok := m.WatchdogStatus("w1"); ok {
		t.Error("status recorded without a push")
	}
}

func TestWatchdogStatePersistedAndReloaded(t *testing.T) {
	dataDir := t.TempDir()
	store := registry.NewStore(dataDir)
	reg := registry.New()
	for id, role := range map[string]string{"mon1": registry.RoleMonitor, "w1": registry.RoleWorker} {
		if err := reg.Add(registry.Machine{ID: id, Role: role, PrivateIPs: []string{"10.0.0.1"}, Provisioned: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}
	gate, err := exec.NewGate([]byte("s"), nil)
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]*recLink{}
	var lmu sync.Mutex
	mkMgr := func(r *registry.Registry) *fleet.Manager {
		lmu.Lock()
		links = map[string]*recLink{}
		lmu.Unlock()
		m, err := fleet.New(fleet.Config{
			Registry: r, Gate: gate, DataDir: dataDir, PollInterval: 10 * time.Millisecond,
			NewLink: func(mc registry.Machine, h channel.Handler) (fleet.Link, error) {
				lk := &recLink{h: h}
				lmu.Lock()
				links[mc.ID] = lk
				lmu.Unlock()
				return lk, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.Start(ctx)
		t.Cleanup(func() { cancel(); m.Close() })
		eventually(t, "links", func() bool {
			lmu.Lock()
			defer lmu.Unlock()
			return len(links) == 2
		})
		return m
	}

	m1 := mkMgr(reg)
	now := time.Now()
	w1evs := []channel.WatchdogEventPayload{
		{Time: now.Add(-3 * time.Second), Kind: watchdog.EventBreach, Scope: "machine", Metric: "cpu_percent", Value: 99},
		{Time: now.Add(-2 * time.Second), Kind: watchdog.EventAction, Scope: "machine", Metric: "cpu_percent", Step: "restart", Target: "service:fleet-agent.service"},
		{Time: now.Add(-time.Second), Kind: watchdog.EventAction, Scope: "machine", Metric: "cpu_percent", Step: "reboot", Target: "machine"},
		{Time: now, Kind: watchdog.EventAlertOnly, Scope: "machine", Metric: "cpu_percent", Step: "reboot"},
	}
	for _, ev := range w1evs {
		links["w1"].h(context.Background(), envOf(t, channel.TypeWatchdogEv, ev))
	}
	links["mon1"].h(context.Background(), envOf(t, channel.TypeWatchdogEv, channel.WatchdogEventPayload{
		Time: now, Kind: watchdog.EventAction, Scope: "machine", Metric: "memory_percent", Step: "restart", Target: "service:fleet-agent.service",
	}))
	m1.Close()

	// Simulate fleet-server restart: reload registry from disk into a fresh Manager.
	reg2, err := registry.NewStore(dataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	m2 := mkMgr(reg2)
	if len(m2.WatchdogEvents("w1")) != 0 {
		t.Fatalf("fresh manager should start with empty in-memory event stream")
	}
	mcW1, ok := reg2.Get("w1")
	if !ok || mcW1.WatchdogState == nil {
		t.Fatalf("w1 watchdog state lost on reload: %+v", mcW1)
	}
	if mcW1.WatchdogState.Stage != "reboot" || mcW1.WatchdogState.RestartsToday != 1 || mcW1.WatchdogState.RebootsToday != 1 ||
		!mcW1.WatchdogState.AlertOnly || len(mcW1.WatchdogState.Breaches) != 1 || mcW1.WatchdogState.LastEvent == nil {
		t.Fatalf("reloaded w1 WatchdogState = %+v", mcW1.WatchdogState)
	}
	mcMon1, ok := reg2.Get("mon1")
	if !ok || mcMon1.WatchdogState == nil || mcMon1.WatchdogState.Stage != "restart" || mcMon1.WatchdogState.RestartsToday != 1 {
		t.Fatalf("reloaded mon1 WatchdogState = %+v", mcMon1.WatchdogState)
	}

	// Duplicate delivery of the last event right after restart does not double-count.
	links["w1"].h(context.Background(), envOf(t, channel.TypeWatchdogEv, w1evs[len(w1evs)-1]))
	if got, _ := reg2.Get("w1"); got.WatchdogState.RestartsToday != 1 || got.WatchdogState.RebootsToday != 1 {
		t.Errorf("retransmit changed counters: %+v", got.WatchdogState)
	}

	// New live event after restart updates on top of persisted state.
	links["w1"].h(context.Background(), envOf(t, channel.TypeWatchdogEv, channel.WatchdogEventPayload{
		Time: now.Add(time.Second), Kind: watchdog.EventCounters,
	}))
	if got, _ := reg2.Get("w1"); got.WatchdogState.Stage != "none" || got.WatchdogState.RestartsToday != 0 || got.WatchdogState.RebootsToday != 0 || got.WatchdogState.AlertOnly {
		t.Errorf("counter reset after restart = %+v", got.WatchdogState)
	}
}
