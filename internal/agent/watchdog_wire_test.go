package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

// --- fakes -------------------------------------------------------------------------

type idleSampler struct{}

func (idleSampler) Sample(context.Context, []string) (watchdog.Sample, error) {
	return watchdog.Sample{Time: time.Now()}, nil
}

type noopActuator struct{}

func (noopActuator) Throttle(context.Context, watchdog.Target, watchdog.ThrottleSpec) error {
	return nil
}
func (noopActuator) Unthrottle(context.Context, watchdog.Target) error  { return nil }
func (noopActuator) KillSession(context.Context, watchdog.Target) error { return nil }
func (noopActuator) RestartService(context.Context, string) error       { return nil }
func (noopActuator) Reboot(context.Context) error                       { return nil }

// policyHolder is the server's stored profile; tests edit it.
type policyHolder struct {
	mu sync.Mutex
	p  fleet.WatchdogPolicy
}

func (h *policyHolder) set(version int, yaml string) {
	h.mu.Lock()
	h.p = fleet.WatchdogPolicy{Version: version, YAML: yaml}
	h.mu.Unlock()
}

func (h *policyHolder) policyFor(string) (fleet.WatchdogPolicy, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.p, h.p.YAML != ""
}

func policyYAML(version int) string {
	return "version: " + strconv.Itoa(version) + "\nsample_interval: 1s\nboot_window: 0s\n"
}

// --- harness -----------------------------------------------------------------------

// wdEnv is one worker agent (the production wiring) over real mTLS and the
// watchdog loop object on the same data dir (a separate process in production).
type wdEnv struct {
	mt     *fleettest.MTLS
	ra     *fleettest.RunningWatchdogAgent
	wd     *watchdog.Watchdog
	dir    string
	policy *policyHolder
	m      *fleet.Manager
}

func newWdEnv(t *testing.T, groups ...string) *wdEnv {
	t.Helper()
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ra, err := mt.StartWatchdogAgent("w1", agent.Config{DataDir: dir, TermGrace: 200 * time.Millisecond, Logger: quiet()},
		agent.WatchdogOptions{Groups: groups, EventPoll: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ra.Stop(); err != nil {
			t.Errorf("agent Run: %v", err)
		}
	})
	ac := config.AgentConfig{MachineID: "w1", Watchdog: config.WatchdogConfig{Groups: groups}}
	wd, err := agent.NewWatchdog(ac, dir, quiet(), agent.WatchdogRunOptions{Sampler: idleSampler{}, Actuator: noopActuator{}})
	if err != nil {
		t.Fatal(err)
	}
	return &wdEnv{mt: mt, ra: ra, wd: wd, dir: dir, policy: &policyHolder{}}
}

// startServer starts the fleet manager; its connection to the agent triggers
// the policy push. A test can raise events before calling it.
func (e *wdEnv) startServer(t *testing.T) {
	t.Helper()
	reg := registry.New()
	if err := reg.Add(registry.Machine{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial,
		PrivateIPs: []string{"127.0.0.1"}, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	gate, err := exec.NewGate([]byte("s"), nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := fleet.New(fleet.Config{
		Registry: reg, Gate: gate, NewLink: e.mt.Factory(), PollInterval: 10 * time.Millisecond, Logger: quiet(),
		Watchdog: fleet.WatchdogSource{PolicyFor: e.policy.policyFor},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })
	e.m = m
}

func (e *wdEnv) status() fleet.WatchdogStatus {
	st, _ := e.m.WatchdogStatus("w1")
	return st
}

func (e *wdEnv) policyFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, watchdog.WatchdogSubdir, "policy.yaml"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// --- tests -------------------------------------------------------------------------

// A policy the server pushes on connect is stored by the main agent, acked, and
// applied live by the watchdog loop; an edit is pushed again and applied again.
func TestPolicyPushedStoredAckedAndApplied(t *testing.T) {
	e := newWdEnv(t)
	e.policy.set(1, policyYAML(1))
	e.startServer(t) // the connection triggers the push

	eventually(t, "first ack", func() bool { return e.status().InForce == 1 })
	if st := e.status(); !st.AckOK || st.Pushed != 1 {
		t.Fatalf("status %+v, want a successful ack of version 1", st)
	}
	if got := e.policyFile(t); got != policyYAML(1) {
		t.Fatalf("stored policy %q, want the pushed text", got)
	}
	if v := e.wd.PolicyVersion(); v != 0 {
		t.Fatalf("watchdog runs version %d before its next sample, want the baked-in 0", v)
	}
	e.wd.Step(context.Background()) // the loop's next tick picks the file up
	if v := e.wd.PolicyVersion(); v != 1 {
		t.Fatalf("watchdog applied version %d, want 1", v)
	}
	if got := e.wd.Engine().Policy().SampleInterval.D(); got != time.Second {
		t.Errorf("sample interval %v, want the pushed 1s", got)
	}

	// An edit: version 2 is pushed to the connected agent and applied live.
	e.policy.set(2, policyYAML(2))
	e.m.PushPolicies(context.Background())
	eventually(t, "second ack", func() bool { return e.status().InForce == 2 })
	e.wd.Step(context.Background())
	if v := e.wd.PolicyVersion(); v != 2 {
		t.Fatalf("watchdog applied version %d after the edit, want 2", v)
	}
	if err := e.m.PushPolicy(context.Background(), "nope"); err == nil {
		t.Error("PushPolicy for an unknown machine succeeded")
	}
}

// Invalid and stale policies are refused with a reason, the stored policy stays
// byte for byte what it was, and the watchdog keeps running the last-known one.
func TestInvalidPolicyRejectedLastKnownKeepsRunning(t *testing.T) {
	e := newWdEnv(t)
	rs := dialAgent(t, e.mt, "w1")
	push := func(id string, version int, yaml string) channel.PolicyAckPayload {
		t.Helper()
		rs.send(channel.TypePolicy, id, channel.PolicyPayload{Version: version, YAML: yaml})
		var ack channel.PolicyAckPayload
		if err := json.Unmarshal(rs.expect(channel.TypePolicyAck, id).Payload, &ack); err != nil {
			t.Fatal(err)
		}
		return ack
	}

	if ack := push("p-ok", 5, policyYAML(5)); !ack.OK || ack.Version != 5 {
		t.Fatalf("good policy: ack %+v, want ok with version 5", ack)
	}
	e.wd.Step(context.Background())
	if e.wd.PolicyVersion() != 5 {
		t.Fatal("watchdog did not apply version 5")
	}
	before := e.policyFile(t)

	for name, c := range map[string]struct {
		version int
		yaml    string
	}{
		"garbage":    {6, "this is: [not valid\n"},
		"bad value":  {6, "version: 6\nsample_interval: banana\n"},
		"no version": {6, "boot_window: 0s\n"},
		"stale":      {4, policyYAML(4)},
		"bad bounds": {6, "version: 6\nsample_interval: 1ms\n"},
	} {
		ack := push("p-"+strings.ReplaceAll(name, " ", "-"), c.version, c.yaml)
		if ack.OK || ack.Error == "" || ack.Version != 5 {
			t.Errorf("%s: ack %+v, want a refusal with a reason and version 5 still in force", name, ack)
		}
		if got := e.policyFile(t); got != before {
			t.Errorf("%s: the stored policy changed to %q", name, got)
		}
		e.wd.Step(context.Background())
		if v := e.wd.PolicyVersion(); v != 5 {
			t.Errorf("%s: watchdog runs version %d, want the last-known 5", name, v)
		}
	}
}

// A profile that parses but resolves to an invalid policy for this host (group
// and host overrides that conflict only together) is refused before it is stored.
func TestPolicyInvalidForThisHostIsNotStored(t *testing.T) {
	const conflict = "version: 3\noverrides:\n  groups:\n    edge:\n      sample_interval: 15s\n  hosts:\n    w1:\n      sustain: 10s\n"
	pf, err := watchdog.ParseProfile([]byte(conflict))
	if err != nil {
		t.Fatalf("the test profile must parse on its own: %v", err)
	}
	if _, err := pf.Effective("w1", []string{"edge"}); err == nil {
		t.Fatal("the test profile must not resolve for w1 in group edge")
	}

	e := newWdEnv(t, "edge")
	rs := dialAgent(t, e.mt, "w1")
	rs.send(channel.TypePolicy, "p", channel.PolicyPayload{Version: 3, YAML: conflict})
	var ack channel.PolicyAckPayload
	if err := json.Unmarshal(rs.expect(channel.TypePolicyAck, "p").Payload, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.OK || !strings.Contains(ack.Error, "resolve") || ack.Version != 0 {
		t.Errorf("ack %+v, want a refusal that names the resolution failure, version 0 in force", ack)
	}
	if got := e.policyFile(t); got != "" {
		t.Errorf("a policy that cannot run on this host was stored: %q", got)
	}
}

// Events the watchdog records in the data dir reach the server: one raised while
// the server is away waits and is forwarded when it connects, later ones follow,
// and the persisted cursor stops a restarted agent from repeating them.
func TestWatchdogEventsForwardedToServer(t *testing.T) {
	e := newWdEnv(t)
	t0 := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	e.wd.Engine().Emit(watchdog.Event{Time: t0, Kind: watchdog.EventBreach, Scope: "machine", Metric: "cpu", Value: 98, Detail: "while the server was away"})

	var cbMu sync.Mutex
	var cb []string
	e.policy.set(1, policyYAML(1))
	e.startServer(t)
	e.m.SetWatchdog(fleet.WatchdogSource{PolicyFor: e.policy.policyFor, OnEvent: func(machine string, ev fleet.WatchdogEvent) {
		cbMu.Lock()
		cb = append(cb, machine+":"+ev.Kind)
		cbMu.Unlock()
	}})
	eventually(t, "backlog event", func() bool { return len(e.m.WatchdogEvents("w1")) >= 1 })
	got := e.m.WatchdogEvents("w1")[0]
	if got.Machine != "w1" || got.Kind != watchdog.EventBreach || got.Metric != "cpu" || got.Value != 98 || !got.Time.Equal(t0) {
		t.Errorf("forwarded event %+v", got)
	}

	// Applying the pushed policy makes the watchdog emit a policy event too.
	eventually(t, "policy stored", func() bool { return e.policyFile(t) != "" })
	e.wd.Step(context.Background())
	e.wd.Engine().Emit(watchdog.Event{Time: t0.Add(time.Minute), Kind: watchdog.EventAction, Scope: "machine", Metric: "cpu", Step: "throttle", Target: "unit x.service"})
	eventually(t, "all three events", func() bool { return len(e.m.WatchdogEvents("w1")) == 3 })
	var kinds []string
	for _, ev := range e.m.WatchdogEvents("w1") {
		kinds = append(kinds, ev.Kind)
	}
	if strings.Join(kinds, ",") != "breach,policy,action" {
		t.Errorf("events arrived as %v, want breach, policy, action in order", kinds)
	}
	cbMu.Lock()
	if len(cb) == 0 || cb[len(cb)-1] != "w1:action" {
		t.Errorf("OnEvent saw %v", cb)
	}
	cbMu.Unlock()

	// The cursor catches up with the file, so a restart does not resend.
	wdir := filepath.Join(e.dir, watchdog.WatchdogSubdir)
	eventually(t, "cursor caught up", func() bool {
		ev, err1 := os.Stat(filepath.Join(wdir, "events.jsonl"))
		cur, err2 := os.ReadFile(filepath.Join(wdir, "events.offset"))
		return err1 == nil && err2 == nil && strings.TrimSpace(string(cur)) == strconv.FormatInt(ev.Size(), 10)
	})
}

// The policy-ack echoes the id of the policy envelope it answers.
func TestPolicyAckEchoesEnvelopeID(t *testing.T) {
	e := newWdEnv(t)
	rs := dialAgent(t, e.mt, "w1")
	rs.send(channel.TypePolicy, "p-1", channel.PolicyPayload{Version: 7, YAML: policyYAML(7)})
	ack := rs.expect(channel.TypePolicyAck, "p-1")
	var p channel.PolicyAckPayload
	if err := json.Unmarshal(ack.Payload, &p); err != nil || !p.OK || p.Version != 7 {
		t.Errorf("ack %s (%v), want ok version 7", ack.Payload, err)
	}
}

// Without a policy source the server pushes nothing and the agent keeps the
// baked-in default.
func TestNoPolicySourcePushesNothing(t *testing.T) {
	e := newWdEnv(t)
	e.startServer(t) // the holder is empty: PolicyFor reports no policy
	eventually(t, "online", func() bool {
		info, ok := e.m.Machine("w1")
		return ok && info.State == fleet.StateOnline
	})
	time.Sleep(300 * time.Millisecond)
	if _, ok := e.m.WatchdogStatus("w1"); ok {
		t.Error("a policy push was recorded without a policy")
	}
	if e.policyFile(t) != "" {
		t.Error("a policy was stored without a push")
	}
	e.wd.Step(context.Background())
	if v := e.wd.PolicyVersion(); v != 0 {
		t.Errorf("watchdog runs version %d, want the baked-in 0", v)
	}
}
