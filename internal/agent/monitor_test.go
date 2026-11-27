package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// okICMP stands in for the ICMP prober: the live one needs an ICMP socket
// (root, CAP_NET_RAW or ping_group_range), which T-0010's own test gates and
// skips off-root. Everything else here is probed for real.
type okICMP struct{}

func (okICMP) Type() probe.Type { return probe.TypeICMP }
func (okICMP) Probe(_ context.Context, _ probe.Target, _ time.Duration) probe.Result {
	return probe.Result{OK: true, Latency: time.Millisecond, Sent: 1, Received: 1}
}

type monEnv struct {
	mgr     *fleet.Manager
	mt      *fleettest.MTLS
	mon     *fleettest.RunningMonitor
	jobs    *monitor.Jobs
	sink    *monitor.MemorySink
	reg     *registry.Registry
	stopMgr func()
	job     monitor.Job
	url     string // the origin the HTTP target probes
}

func newMonEnv(t *testing.T) *monEnv {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(srv.Close)

	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	mon, err := mt.StartMonitor("m1", agent.MonitorConfig{Probers: []probe.Prober{okICMP{}}, Logger: quiet()})
	if err != nil {
		t.Fatalf("StartMonitor: %v", err)
	}
	t.Cleanup(func() {
		if err := mon.Stop(); err != nil {
			t.Errorf("monitor Run: %v", err)
		}
	})

	jobs, err := monitor.OpenJobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec, err := monitor.SpecFromURL("127.0.0.1", srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	job, err := jobs.Create(spec, []string{"m1"})
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := reg.Add(registry.Machine{ID: "m1", Role: registry.RoleMonitor, ConnectMode: registry.ConnectModeServerDial,
		PrivateIPs: []string{"127.0.0.1"}, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	sink := &monitor.MemorySink{}
	e := &monEnv{mt: mt, mon: mon, jobs: jobs, sink: sink, reg: reg, job: job, url: job.Origin}
	e.mgr = e.newManager(t)
	return e
}

// newManager starts a manager for the registry; a second one models a main
// server restart.
func (e *monEnv) newManager(t *testing.T) *fleet.Manager {
	t.Helper()
	gate, err := exec.NewGate([]byte("s"), nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := fleet.New(fleet.Config{
		Registry: e.reg, Gate: gate, NewLink: e.mt.Factory(), PollInterval: 10 * time.Millisecond, Logger: quiet(),
		Monitor: fleet.MonitorSource{ConfigFor: e.jobs.ConfigFor, Ingest: monitor.NewIngestor(e.sink, e.jobs.HistoryKeyFor)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); m.Close() }) }
	t.Cleanup(stop)
	e.stopMgr = stop
	return m
}

func (e *monEnv) state() string {
	info, ok := e.mgr.Machine("m1")
	if !ok {
		return ""
	}
	return info.State
}

func (e *monEnv) samplesFor(targetID string) (ok, bad int) {
	for _, s := range e.sink.Samples() {
		if s.Result.TargetID != targetID {
			continue
		}
		if s.Result.OK {
			ok++
		} else {
			bad++
		}
	}
	return
}

func TestMonitorEndToEnd(t *testing.T) {
	e := newMonEnv(t)

	// Connects and is reported online.
	eventually(t, "m1 online", func() bool { return e.state() == fleet.StateOnline })

	// Receives its pushed config (the job's three targets).
	want := e.jobs.ConfigFor("m1")
	if len(want.Targets) != 3 {
		t.Fatalf("job has %d targets, want 3 (icmp, tcp, http)", len(want.Targets))
	}
	eventually(t, "mon-config applied", func() bool { return len(e.mon.Config().Targets) == len(want.Targets) })

	// Its probe results arrive at the sink, under the verified agent id and
	// the (ip, origin) history key.
	httpID, tcpID, icmpID := e.job.ID+"/http/0", e.job.ID+"/tcp", e.job.ID+"/icmp"
	eventually(t, "results in the sink", func() bool {
		a, _ := e.samplesFor(httpID)
		b, _ := e.samplesFor(tcpID)
		c, _ := e.samplesFor(icmpID)
		return a > 0 && b > 0 && c > 0
	})
	for _, s := range e.sink.Samples() {
		if s.Agent != "m1" {
			t.Fatalf("sample agent = %q, want m1", s.Agent)
		}
		if s.HistoryKey != monitor.HistoryKey("127.0.0.1", e.url) {
			t.Fatalf("sample history key = %q, want %q", s.HistoryKey, monitor.HistoryKey("127.0.0.1", e.url))
		}
		if s.Result.TargetID == httpID && (!s.Result.OK || s.Result.Status != 200) {
			t.Fatalf("http probe of a live server failed: %+v", s.Result)
		}
	}
	if _, bad := e.samplesFor(tcpID); bad != 0 {
		t.Errorf("tcp probe of a live server failed %d times", bad)
	}

	// A config change (the global probe frequency) reaches the connected agent
	// when the server pushes again, and the targets keep running.
	st := e.jobs.Settings()
	st.IntervalMS = 500
	if err := e.jobs.SetSettings(st); err != nil {
		t.Fatal(err)
	}
	e.mgr.PushMonitorConfigs(context.Background())
	eventually(t, "updated mon-config applied", func() bool { return e.mon.Config().IntervalMS == 500 })
	if n := len(e.mon.Config().Targets); n != 3 {
		t.Errorf("targets after the update = %d, want 3", n)
	}
}

// A monitor agent never stops: while the main server is away it keeps probing
// and buffers, and the whole backlog is delivered once a server is back.
func TestMonitorBacklogFlushesOnReconnect(t *testing.T) {
	e := newMonEnv(t)
	tcpID := e.job.ID + "/tcp"
	eventually(t, "m1 online", func() bool { return e.state() == fleet.StateOnline })
	eventually(t, "first results", func() bool { a, _ := e.samplesFor(tcpID); return a > 0 })

	// The main server goes away.
	e.stopMgr()
	eventually(t, "agent sees the disconnect", func() bool { return !e.mon.Connected() })
	outageStart := time.Now()
	base := len(e.sink.Samples())

	// Probing continues and results pile up locally; nothing reaches the sink.
	// (Probes run at the 1 s default, three targets: 6+ results in 3 s.)
	eventually(t, "a backlog builds up", func() bool { return e.mon.Pending() >= 6 })
	if n := len(e.sink.Samples()); n != base {
		t.Errorf("sink grew by %d during the outage", n-base)
	}
	backlog := e.mon.Pending()

	// A (restarted) main server connects: the monitor is online again, gets its
	// config again, and the backlog is flushed.
	e.mgr = e.newManager(t)
	eventually(t, "m1 online again", func() bool { return e.state() == fleet.StateOnline })
	eventually(t, "backlog flushed", func() bool { return e.mon.Pending() < backlog })
	eventually(t, "outage results in the sink", func() bool {
		n := 0
		for _, s := range e.sink.Samples() {
			if s.Result.TargetID == tcpID && s.Result.Time.After(outageStart) {
				n++
			}
		}
		return n >= 2
	})
	if d := e.mon.Dropped(); d != 0 {
		t.Errorf("%d results dropped during a short outage", d)
	}
	// Order is kept: per target, result times never go backwards.
	last := map[string]time.Time{}
	for _, s := range e.sink.Samples() {
		if s.Result.Time.Before(last[s.Result.TargetID]) {
			t.Fatalf("results of %s arrived out of order", s.Result.TargetID)
		}
		last[s.Result.TargetID] = s.Result.Time
	}
}

// The config exchange at the protocol level: the monitor asks for its config on
// connect, applies a pushed mon-config and acknowledges it, rejects a broken
// one without changing anything, and ignores worker messages.
func TestMonitorConfigProtocol(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	mon, err := mt.StartMonitor("m1", agent.MonitorConfig{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mon.Stop() })

	dc, err := mt.DialerConfig("m1")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []channel.Envelope
	dc.Handler = func(_ context.Context, env channel.Envelope) {
		mu.Lock()
		got = append(got, env)
		mu.Unlock()
	}
	d, err := channel.NewDialer(dc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	count := func(typ string) (n int) {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range got {
			if e.Type == typ {
				n++
			}
		}
		return
	}
	lastAck := func() (channel.ConfigAckPayload, bool) {
		mu.Lock()
		defer mu.Unlock()
		for i := len(got) - 1; i >= 0; i-- {
			if got[i].Type == channel.TypeConfigAck {
				var a channel.ConfigAckPayload
				_ = json.Unmarshal(got[i].Payload, &a)
				return a, true
			}
		}
		return channel.ConfigAckPayload{}, false
	}

	eventually(t, "config-req on connect", func() bool { return count(channel.TypeConfigReq) == 1 })

	good, err := monitor.ConfigEnvelope(probe.Config{IntervalMS: 700, Targets: []probe.Target{
		{ID: "t1", Type: probe.TypeTCP, Address: "127.0.0.1:1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Send(ctx, good); err != nil {
		t.Fatal(err)
	}
	eventually(t, "ack of the good config", func() bool { a, ok := lastAck(); return ok && a.OK })
	if c := mon.Config(); c.IntervalMS != 700 || len(c.Targets) != 1 {
		t.Fatalf("applied config = %+v", c)
	}

	bad := channel.Envelope{Type: channel.TypeMonConfig, ID: "bad", Payload: []byte(`{"targets": "nope"}`)}
	if err := d.Send(ctx, bad); err != nil {
		t.Fatal(err)
	}
	eventually(t, "rejection of the broken config", func() bool { a, ok := lastAck(); return ok && !a.OK && a.Error != "" })
	if c := mon.Config(); c.IntervalMS != 700 || len(c.Targets) != 1 {
		t.Errorf("a rejected config changed the applied one: %+v", c)
	}

	// A worker message is not understood by a monitor agent and must not break it.
	run, _ := channel.NewEnvelope(channel.TypeRun, "r1", map[string]string{"command": "true"})
	if err := d.Send(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := d.Send(ctx, good); err != nil {
		t.Fatal(err)
	}
	eventually(t, "still answering after a stray run", func() bool { return count(channel.TypeConfigAck) == 3 })
	if count(channel.TypeExit) != 0 {
		t.Error("a monitor agent answered a run")
	}
}

func TestMonitorReportsPublicIPOnConnectAndRefresh(t *testing.T) {
	var mu sync.Mutex
	currentIP := "203.0.113.19"
	mkReflector := func() *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			ip := currentIP
			mu.Unlock()
			_, _ = w.Write([]byte(ip))
		}))
		t.Cleanup(s.Close)
		return s
	}
	r1, r2 := mkReflector(), mkReflector()

	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	mon, err := mt.StartMonitor("m1", agent.MonitorConfig{
		Reflectors: []string{r1.URL, r2.URL},
		Logger:     quiet(),
	})
	if err != nil {
		t.Fatalf("StartMonitor: %v", err)
	}
	t.Cleanup(func() { _ = mon.Stop() })

	reg := registry.New()
	if err := reg.Add(registry.Machine{
		ID:          "m1",
		Role:        registry.RoleMonitor,
		ConnectMode: registry.ConnectModeServerDial,
		PrivateIPs:  []string{"127.0.0.1"},
		Provisioned: true,
	}); err != nil {
		t.Fatal(err)
	}
	gate, err := exec.NewGate([]byte("s"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := fleet.New(fleet.Config{
		Registry:     reg,
		Gate:         gate,
		NewLink:      mt.Factory(),
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	defer func() { cancel(); mgr.Close() }()

	// Boot reflector resolution updates the monitor's PublicIP in the registry.
	eventually(t, "monitor boot public IP", func() bool {
		info, ok := mgr.Machine("m1")
		return ok && info.State == fleet.StateOnline && info.PublicIP == "203.0.113.19" && info.PublicIPAt != nil
	})
	firstInfo, _ := mgr.Machine("m1")
	firstAt := *firstInfo.PublicIPAt

	// Manual refresh updates the monitor's IP when changed.
	mu.Lock()
	currentIP = "198.51.100.88"
	mu.Unlock()
	if err := mgr.RefreshPublicIP(ctx, "m1"); err != nil {
		t.Fatalf("RefreshPublicIP(m1): %v", err)
	}
	eventually(t, "monitor refreshed public IP", func() bool {
		info, ok := mgr.Machine("m1")
		return ok && info.PublicIP == "198.51.100.88" && info.PublicIPAt != nil && info.PublicIPAt.After(firstAt)
	})
}
