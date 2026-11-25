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
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// recLink is a link whose far end is the test: it records what the manager
// sends and lets the test deliver envelopes as the agent would.
type recLink struct {
	h    channel.Handler
	mu   sync.Mutex
	sent []channel.Envelope
}

func (l *recLink) Send(_ context.Context, env channel.Envelope) error {
	l.mu.Lock()
	l.sent = append(l.sent, env)
	l.mu.Unlock()
	return nil
}
func (l *recLink) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (l *recLink) Online() bool                  { return true }

func (l *recLink) sentOfType(typ string) []channel.Envelope {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []channel.Envelope
	for _, e := range l.sent {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

type ingestRec struct {
	mu     sync.Mutex
	agents []string
}

func (r *ingestRec) Handle(_ context.Context, agentID string, env channel.Envelope) error {
	r.mu.Lock()
	r.agents = append(r.agents, agentID+":"+env.Type)
	r.mu.Unlock()
	return nil
}

func (r *ingestRec) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.agents...)
}

func newMonitorMgr(t *testing.T, src fleet.MonitorSource, late bool) (*fleet.Manager, map[string]*recLink) {
	t.Helper()
	reg := registry.New()
	for id, role := range map[string]string{"mon1": registry.RoleMonitor, "w1": registry.RoleWorker} {
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
	factory := func(mc registry.Machine, h channel.Handler) (fleet.Link, error) {
		lk := &recLink{h: h}
		lmu.Lock()
		links[mc.ID] = lk
		lmu.Unlock()
		return lk, nil
	}
	cfg := fleet.Config{Registry: reg, Gate: gate, NewLink: factory, PollInterval: 10 * time.Millisecond}
	if !late {
		cfg.Monitor = src
	}
	m, err := fleet.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })
	if late {
		m.SetMonitor(src)
	}
	eventually(t, "links", func() bool {
		lmu.Lock()
		defer lmu.Unlock()
		return len(links) == 2
	})
	return m, links
}

func envOf(t *testing.T, typ string, payload any) channel.Envelope {
	t.Helper()
	e, err := channel.NewEnvelope(typ, "x", payload)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestMonitorConfigPushedToMonitorOnly(t *testing.T) {
	asked := map[string]int{}
	var mu sync.Mutex
	src := fleet.MonitorSource{ConfigFor: func(agent string) probe.Config {
		mu.Lock()
		asked[agent]++
		mu.Unlock()
		return probe.Config{IntervalMS: 700, Targets: []probe.Target{{ID: agent + "/tcp", Type: probe.TypeTCP, Address: "h:1"}}}
	}}
	for _, late := range []bool{false, true} { // bound at New, and bound later by SetMonitor
		_, links := newMonitorMgr(t, src, late)
		ctx := context.Background()
		links["mon1"].h(ctx, envOf(t, channel.TypeConfigReq, nil))
		links["w1"].h(ctx, envOf(t, channel.TypeConfigReq, nil))

		eventually(t, "mon-config sent", func() bool { return len(links["mon1"].sentOfType(channel.TypeMonConfig)) == 1 })
		cfg, err := monitor.DecodeConfig(links["mon1"].sentOfType(channel.TypeMonConfig)[0])
		if err != nil {
			t.Fatal(err)
		}
		if cfg.IntervalMS != 700 || len(cfg.Targets) != 1 || cfg.Targets[0].ID != "mon1/tcp" {
			t.Errorf("late=%v: pushed config = %+v, want mon1's targets", late, cfg)
		}
		if n := len(links["w1"].sentOfType(channel.TypeMonConfig)); n != 0 {
			t.Errorf("late=%v: a worker was sent %d mon-config messages", late, n)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if asked["w1"] != 0 {
		t.Errorf("ConfigFor was asked for a worker: %v", asked)
	}
}

func TestMonitorDataIngestedWithVerifiedAgentID(t *testing.T) {
	rec := &ingestRec{}
	_, links := newMonitorMgr(t, fleet.MonitorSource{Ingest: rec}, false)
	ctx := context.Background()
	links["mon1"].h(ctx, envOf(t, channel.TypeMonData, monitor.Batch{Seq: 1}))
	links["w1"].h(ctx, envOf(t, channel.TypeMonData, monitor.Batch{Seq: 1})) // a worker has no business sending this
	got := rec.got()
	if len(got) != 1 || got[0] != "mon1:"+channel.TypeMonData {
		t.Errorf("ingested %v, want only mon1's batch under mon1's id", got)
	}
}

func TestMonitorWithoutSourceIsHarmless(t *testing.T) {
	_, links := newMonitorMgr(t, fleet.MonitorSource{}, false)
	ctx := context.Background()
	links["mon1"].h(ctx, envOf(t, channel.TypeConfigReq, nil))
	links["mon1"].h(ctx, envOf(t, channel.TypeMonData, monitor.Batch{Seq: 1}))
	if n := len(links["mon1"].sentOfType(channel.TypeMonConfig)); n != 0 {
		t.Errorf("pushed %d mon-config without a source", n)
	}
}

func TestPushMonitorConfigsReachesConnectedMonitors(t *testing.T) {
	var mu sync.Mutex
	n := 0
	src := fleet.MonitorSource{ConfigFor: func(string) probe.Config {
		mu.Lock()
		n++
		mu.Unlock()
		return probe.Config{Targets: []probe.Target{}}
	}}
	m, links := newMonitorMgr(t, src, false)
	eventually(t, "mon1 online", func() bool { return m.Machines()[0].State == fleet.StateOnline })
	m.PushMonitorConfigs(context.Background())
	if got := len(links["mon1"].sentOfType(channel.TypeMonConfig)); got != 1 {
		t.Errorf("mon1 got %d mon-config, want 1", got)
	}
	if got := len(links["w1"].sentOfType(channel.TypeMonConfig)); got != 0 {
		t.Errorf("worker got %d mon-config, want 0", got)
	}
	var cfg probe.Config
	if err := json.Unmarshal(links["mon1"].sentOfType(channel.TypeMonConfig)[0].Payload, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Targets == nil {
		t.Error("an empty target list must be pushed as [], so the agent clears its targets")
	}
}
