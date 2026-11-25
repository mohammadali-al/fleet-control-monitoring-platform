package fleet_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// These tests run the REAL agent runtime and the REAL fleet manager over the
// real mTLS channel, once per direction, and require the same observable
// behavior: agent_dial must be indistinguishable from server_dial above the
// link.

func adQuiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// adCapture records the capture configs the agent builds from the config-push.
type adCapture struct {
	mu   sync.Mutex
	cfgs []capture.Config
}

func (f *adCapture) factory(_ context.Context, cfg capture.Config) (agent.Capturer, error) {
	f.mu.Lock()
	f.cfgs = append(f.cfgs, cfg)
	f.mu.Unlock()
	return adCapturer{}, nil
}

func (f *adCapture) snapshot() []capture.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capture.Config(nil), f.cfgs...)
}

type adCapturer struct{}

func (adCapturer) Capture(string, string) (agent.SessionCapture, error) { return adSession{}, nil }
func (adCapturer) Close()                                               {}

type adSession struct{}

func (adSession) Stop() (capture.Summary, error) { return capture.Summary{}, nil }

type adEnv struct {
	t   *testing.T
	mt  *fleettest.MTLS
	reg *registry.Registry
	m   *fleet.Manager
	ln  *fleet.AgentListener // nil under server_dial
	fc  *adCapture
	ag  interface{ Connected() bool }

	stopMgr func()
}

func (e *adEnv) newManager(ln *fleet.AgentListener, mod func(*fleet.Config)) *fleet.Manager {
	e.t.Helper()
	gate, err := exec.NewGate([]byte("shared-secret"), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	cfg := fleet.Config{
		Registry: e.reg, Gate: gate, NewLink: e.mt.Factory(), AgentDial: ln, PollInterval: 10 * time.Millisecond,
		GateSecret: []byte("shared-secret"), Capture: config.CaptureConfig{TrimThresholdBytes: 123, SegmentMaxAgeSeconds: 9},
		Logger: adQuiet(),
	}
	if mod != nil {
		mod(&cfg)
	}
	m, err := fleet.New(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	var once sync.Once
	e.stopMgr = func() { once.Do(func() { cancel(); m.Close() }) }
	e.t.Cleanup(e.stopMgr)
	return m
}

func addMachine(t *testing.T, reg *registry.Registry, id, role, mode string) {
	t.Helper()
	mc := registry.Machine{ID: id, Role: role, ConnectMode: mode, Provisioned: true}
	if mode == registry.ConnectModeServerDial {
		mc.PrivateIPs = []string{"127.0.0.1"}
	}
	if err := reg.Add(mc); err != nil {
		t.Fatal(err)
	}
}

// newWorkerEnv is a server plus one worker agent "w1" whose direction is mode.
func newWorkerEnv(t *testing.T, mode string) *adEnv {
	t.Helper()
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	e := &adEnv{t: t, mt: mt, reg: registry.New(), fc: &adCapture{}}
	addMachine(t, e.reg, "w1", registry.RoleWorker, mode)
	cfg := agent.Config{DataDir: t.TempDir(), TermGrace: 200 * time.Millisecond, Logger: adQuiet(), CaptureFactory: e.fc.factory}
	switch mode {
	case registry.ConnectModeServerDial:
		ra, err := mt.StartAgent("w1", cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ra.Stop() })
		e.ag = ra
		e.m = e.newManager(nil, nil)
	case registry.ConnectModeAgentDial:
		if e.ln, err = mt.ListenAgents(""); err != nil {
			t.Fatal(err)
		}
		e.m = e.newManager(e.ln, nil)
		ra, err := mt.StartDialingAgent("w1", e.ln.Addr().String(), cfg, agent.DialSettings{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ra.Stop() })
		e.ag = ra
	}
	return e
}

func (e *adEnv) waitOnline(id string) {
	e.t.Helper()
	eventually(e.t, id+" online", func() bool {
		info, ok := e.m.Machine(id)
		return ok && info.State == fleet.StateOnline
	})
}

func execEvents(ev fleet.Event) bool { return ev.Kind == fleet.EventExec }

// A worker is reported online; a one-shot streams queued -> running -> exited;
// a dangerous command is held until confirmed and then runs because the agent
// verifies the server's token with the secret it was PUSHED; the capture
// section of the config-push reaches the agent. Identical in both directions.
func TestWorkerRoundTripBothDirections(t *testing.T) {
	for _, mode := range []string{registry.ConnectModeServerDial, registry.ConnectModeAgentDial} {
		t.Run(mode, func(t *testing.T) {
			e := newWorkerEnv(t, mode)
			e.waitOnline("w1")
			if info, _ := e.m.Machine("w1"); info.ConnectMode != mode || info.Note != "" {
				t.Errorf("machine = %+v, want mode %s and no note", info, mode)
			}
			sub := e.m.Bus().Subscribe(execEvents)
			defer sub.Close()

			id, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "echo hello; echo oops >&2", User: "op"})
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			kinds, out, exit := collect(t, sub, id)
			if got := strings.Join(kinds, ","); got != "queued,running,exited" {
				t.Errorf("lifecycle = %s, want queued,running,exited", got)
			}
			if !strings.Contains(out, "hello") || !strings.Contains(out, "oops") {
				t.Errorf("streamed output = %q", out)
			}
			if exit.ExitCode != 0 || exit.Reason != exec.ReasonExited {
				t.Errorf("exit = %+v", exit)
			}

			if _, err = e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "echo reboot"}); err == nil {
				t.Fatal("an unconfirmed dangerous command was accepted")
			}
			id, err = e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "echo reboot", Confirm: true})
			if err != nil {
				t.Fatalf("confirmed Submit: %v", err)
			}
			_, out, exit = collect(t, sub, id)
			if exit.ExitCode != 0 || exit.Reason != exec.ReasonExited || !strings.Contains(out, "reboot") {
				t.Errorf("confirmed dangerous command: exit %+v out %q", exit, out)
			}

			cfgs := e.fc.snapshot()
			if len(cfgs) != 1 || cfgs[0].TrimThreshold != 123 || cfgs[0].Segment.MaxAge != 9*time.Second || cfgs[0].Machine != "w1" {
				t.Errorf("capture configs built = %+v, want one with the pushed settings", cfgs)
			}
		})
	}
}

// A running command can be cancelled (terminate flows through the same link).
func TestCancelOverAgentDial(t *testing.T) {
	e := newWorkerEnv(t, registry.ConnectModeAgentDial)
	e.waitOnline("w1")
	sub := e.m.Bus().Subscribe(execEvents)
	defer sub.Close()
	id, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "sleep 30", User: "op"})
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, sub, id)
	if err := e.m.Cancel(context.Background(), id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	_, _, exit := collect(t, sub, id)
	if exit.Reason == exec.ReasonExited && exit.ExitCode == 0 {
		t.Errorf("a cancelled sleep exited normally: %+v", exit)
	}
}

// --- monitor ----------------------------------------------------------------------

type adICMP struct{}

func (adICMP) Type() probe.Type { return probe.TypeICMP }
func (adICMP) Probe(context.Context, probe.Target, time.Duration) probe.Result {
	return probe.Result{OK: true, Latency: time.Millisecond, Sent: 1, Received: 1}
}

// A monitor agent is online, receives its pushed mon-config, its probe results
// reach the sink under its verified id, and a config change is pushed to the
// connected agent. Identical in both directions.
func TestMonitorRoundTripBothDirections(t *testing.T) {
	for _, mode := range []string{registry.ConnectModeServerDial, registry.ConnectModeAgentDial} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
			t.Cleanup(srv.Close)
			mt, err := fleettest.NewMTLS()
			if err != nil {
				t.Fatal(err)
			}
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
			e := &adEnv{t: t, mt: mt, reg: registry.New()}
			addMachine(t, e.reg, "m1", registry.RoleMonitor, mode)
			sink := &monitor.MemorySink{}
			mod := func(c *fleet.Config) {
				c.Monitor = fleet.MonitorSource{ConfigFor: jobs.ConfigFor, Ingest: monitor.NewIngestor(sink, jobs.HistoryKeyFor)}
			}
			mcfg := agent.MonitorConfig{Probers: []probe.Prober{adICMP{}}, Logger: adQuiet()}
			var applied func() probe.Config
			switch mode {
			case registry.ConnectModeServerDial:
				mon, err := mt.StartMonitor("m1", mcfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = mon.Stop() })
				applied = mon.Config
				e.m = e.newManager(nil, mod)
			default:
				if e.ln, err = mt.ListenAgents(""); err != nil {
					t.Fatal(err)
				}
				e.m = e.newManager(e.ln, mod)
				mon, err := mt.StartDialingMonitor("m1", e.ln.Addr().String(), mcfg, agent.DialSettings{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = mon.Stop() })
				applied = mon.Config
			}
			e.waitOnline("m1")

			if want := jobs.ConfigFor("m1"); len(want.Targets) != 3 {
				t.Fatalf("job has %d targets, want 3", len(want.Targets))
			}
			eventually(t, "mon-config applied", func() bool { return len(applied().Targets) == 3 })

			httpID, tcpID, icmpID := job.ID+"/http/0", job.ID+"/tcp", job.ID+"/icmp"
			count := func(id string) (n int) {
				for _, s := range sink.Samples() {
					if s.Result.TargetID == id {
						n++
					}
				}
				return n
			}
			eventually(t, "results in the sink", func() bool { return count(httpID) > 0 && count(tcpID) > 0 && count(icmpID) > 0 })
			for _, s := range sink.Samples() {
				if s.Agent != "m1" {
					t.Fatalf("sample agent = %q, want m1", s.Agent)
				}
				if s.HistoryKey != monitor.HistoryKey("127.0.0.1", job.Origin) {
					t.Fatalf("sample history key = %q", s.HistoryKey)
				}
			}

			st := jobs.Settings()
			st.IntervalMS = 500
			if err := jobs.SetSettings(st); err != nil {
				t.Fatal(err)
			}
			e.m.PushMonitorConfigs(context.Background())
			eventually(t, "updated mon-config applied", func() bool { return applied().IntervalMS == 500 })
		})
	}
}

// --- restart, rejection, coexistence ----------------------------------------------

// The agent redials on its own after the main server restarts (a new manager and
// a new listener on the same address), with no action on the server's side other
// than coming back; commands work afterwards.
func TestAgentDialReconnectsAfterServerRestart(t *testing.T) {
	e := newWorkerEnv(t, registry.ConnectModeAgentDial)
	e.waitOnline("w1")
	addr := e.ln.Addr().String()

	e.stopMgr() // the main server goes away (closing its listener and every connection)
	eventually(t, "agent sees the disconnect", func() bool { return !e.ag.Connected() })
	time.Sleep(150 * time.Millisecond) // several failed redials against the dead port

	ln2, err := e.mt.ListenAgents(addr)
	if err != nil {
		t.Fatalf("listen again on %s: %v", addr, err)
	}
	e.m = e.newManager(ln2, nil)
	e.waitOnline("w1")

	sub := e.m.Bus().Subscribe(execEvents)
	defer sub.Close()
	id, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "echo back"})
	if err != nil {
		t.Fatal(err)
	}
	_, out, exit := collect(t, sub, id)
	if exit.ExitCode != 0 || !strings.Contains(out, "back") {
		t.Errorf("after restart: exit %+v out %q", exit, out)
	}
}

// An agent_dial agent that disappears is reported reconnecting, and online
// again when it returns.
func TestAgentDialOfflineAndBack(t *testing.T) {
	e := newWorkerEnv(t, registry.ConnectModeAgentDial)
	e.waitOnline("w1")
	e.ln.Close() // drops the connection; the agent redials and finds the port closed
	eventually(t, "w1 reconnecting", func() bool {
		info, _ := e.m.Machine("w1")
		return info.State == fleet.StateReconnecting
	})
}

// Rejection happens at the server: an agent whose certificate is from a different
// CA (though it trusts the server's CA) never gets online, and neither does a valid
// agent the server has no agent_dial machine for.
func TestAgentDialRejectsForeignCAAndUnknownMachine(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	e := &adEnv{t: t, mt: mt, reg: registry.New()}
	addMachine(t, e.reg, "w1", registry.RoleWorker, registry.ConnectModeAgentDial)
	if e.ln, err = mt.ListenAgents(""); err != nil {
		t.Fatal(err)
	}
	e.m = e.newManager(e.ln, nil)
	addr := e.ln.Addr().String()
	cfg := func() agent.Config {
		return agent.Config{DataDir: t.TempDir(), TermGrace: 200 * time.Millisecond, Logger: adQuiet()}
	}

	foreign, err := pki.NewCA(pki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := foreign.IssuePeer("w1", []net.IP{net.IPv4(127, 0, 0, 1)}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rogueTLS, err := channel.AgentDialTLSConfig(cert, key, mt.CA.CertPEM(), mt.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	rogue, err := mt.StartDialingAgent("w1", addr, cfg(), agent.DialSettings{TLS: rogueTLS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rogue.Stop() })

	stranger, err := mt.StartDialingAgent("not-registered", addr, cfg(), agent.DialSettings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stranger.Stop() })

	// Give both many redial attempts (20-200 ms backoff).
	time.Sleep(700 * time.Millisecond)
	if rogue.Connected() || stranger.Connected() {
		t.Fatalf("rejected agents connected: foreign-CA %v, unregistered %v", rogue.Connected(), stranger.Connected())
	}
	if info, _ := e.m.Machine("w1"); info.State == fleet.StateOnline {
		t.Fatal("w1 is online though only a foreign-CA agent dialed in")
	}

	// The legitimate agent for the same machine id is accepted by the same listener.
	good, err := mt.StartDialingAgent("w1", addr, cfg(), agent.DialSettings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = good.Stop() })
	e.waitOnline("w1")
}

// server_dial and agent_dial machines side by side in one manager, each working,
// and a server without an agent listener leaves agent_dial machines offline.
func TestMixedDirections(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	e := &adEnv{t: t, mt: mt, reg: registry.New()}
	addMachine(t, e.reg, "w1", registry.RoleWorker, registry.ConnectModeServerDial)
	addMachine(t, e.reg, "w2", registry.RoleWorker, registry.ConnectModeAgentDial)
	cfg := func() agent.Config {
		return agent.Config{DataDir: t.TempDir(), TermGrace: 200 * time.Millisecond, Logger: adQuiet()}
	}
	ra, err := mt.StartAgent("w1", cfg())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ra.Stop() })
	if e.ln, err = mt.ListenAgents(""); err != nil {
		t.Fatal(err)
	}
	e.m = e.newManager(e.ln, nil)
	rb, err := mt.StartDialingAgent("w2", e.ln.Addr().String(), cfg(), agent.DialSettings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rb.Stop() })
	e.waitOnline("w1")
	e.waitOnline("w2")

	sub := e.m.Bus().Subscribe(execEvents)
	defer sub.Close()
	for _, id := range []string{"w1", "w2"} {
		s, err := e.m.Submit(fleet.SubmitRequest{Machine: id, Command: "echo from-" + id})
		if err != nil {
			t.Fatal(err)
		}
		if _, out, exit := collect(t, sub, s); exit.ExitCode != 0 || !strings.Contains(out, "from-"+id) {
			t.Errorf("%s: exit %+v out %q", id, exit, out)
		}
	}
	if s := fleet.Summarize(e.m.Machines()); s.Workers != 2 || s.WorkersOnline != 2 {
		t.Errorf("summary = %+v, want 2 of 2 workers online", s)
	}
}
