package fleet_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

const wait = 15 * time.Second

type env struct {
	m    *fleet.Manager
	reg  *registry.Registry
	loop *fleettest.Loopback
	gate *exec.Gate
}

// newEnv builds a manager over a registry with a worker, a second worker, a
// monitor, an unprovisioned machine and an agent_dial machine. The gate list is
// the single pattern DANGER so no test runs anything destructive.
func newEnv(t *testing.T, offline ...string) *env {
	t.Helper()
	reg := registry.New()
	add := func(id, role, mode string, provisioned bool, ip string) {
		if err := reg.Add(registry.Machine{ID: id, Role: role, ConnectMode: mode, PrivateIPs: []string{ip}, Provisioned: provisioned}); err != nil {
			t.Fatal(err)
		}
	}
	add("w1", registry.RoleWorker, registry.ConnectModeServerDial, true, "10.0.0.1")
	add("w2", registry.RoleWorker, registry.ConnectModeServerDial, true, "10.0.0.2")
	add("mon1", registry.RoleMonitor, registry.ConnectModeServerDial, true, "10.0.0.3")
	add("fresh", registry.RoleWorker, registry.ConnectModeServerDial, false, "10.0.0.4")
	add("remote", registry.RoleWorker, registry.ConnectModeAgentDial, true, "10.0.0.5")

	gate, err := exec.NewGate([]byte("test-secret"), []string{`DANGER`})
	if err != nil {
		t.Fatal(err)
	}
	loop := fleettest.NewLoopback(gate)
	for _, id := range offline {
		loop.StartOffline(id)
	}
	m, err := fleet.New(fleet.Config{Registry: reg, Gate: gate, NewLink: loop.Factory(), PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })
	return &env{m: m, reg: reg, loop: loop, gate: gate}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *env) state(id string) string {
	info, ok := e.m.Machine(id)
	if !ok {
		return "?"
	}
	return info.State
}

// collect gathers exec events for one session until it exits.
func collect(t *testing.T, sub *fleet.Subscription, session string) (kinds []string, out string, exit exec.ExitPayload) {
	t.Helper()
	var sb strings.Builder
	timeout := time.After(wait)
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				t.Fatal("subscription closed")
			}
			if ev.Kind != fleet.EventExec || ev.Exec.SessionID != session {
				continue
			}
			if ev.Exec.Kind == exec.EventChunk {
				sb.Write(ev.Exec.Chunk.Data)
				continue
			}
			kinds = append(kinds, ev.Exec.Kind)
			if ev.Exec.Kind == exec.EventExited {
				return kinds, sb.String(), *ev.Exec.Exit
			}
		case <-timeout:
			t.Fatalf("timed out; events so far %v", kinds)
		}
	}
}

// waitRunning blocks until the agent reported the session as running (a
// command shown as "running" by the queue may not have reached the agent yet).
func waitRunning(t *testing.T, sub *fleet.Subscription, session string) {
	t.Helper()
	timeout := time.After(wait)
	for {
		select {
		case ev := <-sub.C:
			if ev.Kind == fleet.EventExec && ev.Exec.SessionID == session && ev.Exec.Kind == exec.EventRunning {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s to run", session)
		}
	}
}

func TestMachineStates(t *testing.T) {
	e := newEnv(t, "w2")
	eventually(t, "w1 online", func() bool { return e.state("w1") == fleet.StateOnline })
	if got := e.state("w2"); got != fleet.StateOffline {
		t.Errorf("never-connected w2 = %q, want offline", got)
	}
	if got := e.state("fresh"); got != fleet.StateOffline {
		t.Errorf("unprovisioned = %q, want offline", got)
	}
	if info, _ := e.m.Machine("remote"); info.State != fleet.StateOffline || !strings.Contains(info.Note, "agent_dial") {
		t.Errorf("agent_dial machine = %+v, want offline with an explanatory note", info)
	}

	// A drop shows as reconnecting; a return as online again.
	e.loop.SetOnline("w1", false)
	eventually(t, "w1 reconnecting", func() bool { return e.state("w1") == fleet.StateReconnecting })
	if mc, _ := e.reg.Get("w1"); mc.Online {
		t.Error("registry still says w1 is online")
	}
	e.loop.SetOnline("w1", true)
	eventually(t, "w1 online again", func() bool { return e.state("w1") == fleet.StateOnline })
	if info, _ := e.m.Machine("w1"); info.LastSeen == nil {
		t.Error("online machine has no last_seen")
	}

	s := fleet.Summarize(e.m.Machines())
	if s.Workers != 4 || s.WorkersOnline != 1 || s.Monitors != 1 {
		t.Errorf("summary = %+v", s)
	}
}

func TestMachineEventsOnBus(t *testing.T) {
	e := newEnv(t, "w2")
	sub := e.m.Bus().Subscribe(func(ev fleet.Event) bool { return ev.Kind == fleet.EventMachine && ev.Machine == "w2" })
	defer sub.Close()
	e.loop.SetOnline("w2", true)
	select {
	case ev := <-sub.C:
		if ev.Info == nil || ev.Info.State != fleet.StateOnline {
			t.Fatalf("event = %+v", ev.Info)
		}
	case <-time.After(wait):
		t.Fatal("no machine event")
	}
}

func TestSubmitStreamsAndExits(t *testing.T) {
	e := newEnv(t)
	eventually(t, "w1 online", func() bool { return e.state("w1") == fleet.StateOnline })
	sub := e.m.Bus().Subscribe(nil)
	defer sub.Close()

	id, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "echo to-out; echo to-err >&2; exit 3", User: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	kinds, out, x := collect(t, sub, id)
	if strings.Join(kinds, ",") != "queued,running,exited" {
		t.Errorf("lifecycle = %v", kinds)
	}
	if x.ExitCode != 3 || x.Reason != exec.ReasonExited {
		t.Errorf("exit = %+v", x)
	}
	if !strings.Contains(out, "to-out") || !strings.Contains(out, "to-err") {
		t.Errorf("streamed output = %q", out)
	}

	c, segs, ok := e.m.Command(id)
	if !ok || c.State != "finished" || c.Machine != "w1" || c.User != "alice" || c.Mode != exec.ModeOneShot {
		t.Fatalf("history entry = %+v ok=%v", c, ok)
	}
	var stdout, stderr string
	for _, s := range segs {
		switch s.Stream {
		case "stdout":
			stdout += s.Text
		case "stderr":
			stderr += s.Text
		}
	}
	if stdout != "to-out\n" || stderr != "to-err\n" {
		t.Errorf("stored output stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestSerialQueue(t *testing.T) {
	e := newEnv(t)
	eventually(t, "w1 online", func() bool { return e.state("w1") == fleet.StateOnline })
	sub := e.m.Bus().Subscribe(nil)
	defer sub.Close()

	first, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "sleep 0.4; echo first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "echo second"})
	if err != nil {
		t.Fatal(err)
	}
	// While the first runs, the second is queued at position 1.
	eventually(t, "first running and second queued", func() bool {
		var run, q bool
		for _, c := range e.m.Commands() {
			if c.Session == first && c.State == "running" {
				run = true
			}
			if c.Session == second && c.State == "queued" && c.Position == 1 {
				q = true
			}
		}
		return run && q
	})
	if info, _ := e.m.Machine("w1"); info.Running != 1 || info.Queued != 1 {
		t.Errorf("machine load = %d running, %d queued", info.Running, info.Queued)
	}
	// Strict FIFO: the second only starts after the first exited.
	var order []string
	timeout := time.After(wait)
	for len(order) < 4 {
		select {
		case ev := <-sub.C:
			if ev.Kind == fleet.EventExec && (ev.Exec.Kind == exec.EventRunning || ev.Exec.Kind == exec.EventExited) {
				order = append(order, ev.Exec.Kind+":"+map[string]string{first: "first", second: "second"}[ev.Exec.SessionID])
			}
		case <-timeout:
			t.Fatalf("timed out; saw %v", order)
		}
	}
	want := "running:first,exited:first,running:second,exited:second"
	if got := strings.Join(order, ","); got != want {
		t.Errorf("order = %s, want %s", got, want)
	}
}

func TestSubmitRefusals(t *testing.T) {
	e := newEnv(t, "w2")
	eventually(t, "w1 online", func() bool { return e.state("w1") == fleet.StateOnline })
	cases := []struct {
		name string
		req  fleet.SubmitRequest
		want error
	}{
		{"monitor has no shell", fleet.SubmitRequest{Machine: "mon1", Command: "true"}, fleet.ErrNoShell},
		{"offline machine", fleet.SubmitRequest{Machine: "w2", Command: "true"}, fleet.ErrOffline},
		{"unprovisioned machine", fleet.SubmitRequest{Machine: "fresh", Command: "true"}, fleet.ErrOffline},
		{"unknown machine", fleet.SubmitRequest{Machine: "nope", Command: "true"}, fleet.ErrUnknown},
		{"empty command", fleet.SubmitRequest{Machine: "w1", Command: ""}, fleet.ErrBadRequest},
		{"negative timeout", fleet.SubmitRequest{Machine: "w1", Command: "true", TimeoutSec: -1}, fleet.ErrBadRequest},
	}
	for _, c := range cases {
		if _, err := e.m.Submit(c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := e.m.OpenShell(fleet.ShellRequest{Machine: "mon1"}); !errors.Is(err, fleet.ErrNoShell) {
		t.Errorf("shell on a monitor: err = %v, want ErrNoShell", err)
	}
}

func TestDangerousCommandNeedsConfirmation(t *testing.T) {
	e := newEnv(t)
	eventually(t, "w1 online", func() bool { return e.state("w1") == fleet.StateOnline })
	sub := e.m.Bus().Subscribe(nil)
	defer sub.Close()

	var cr *fleet.ConfirmRequiredError
	if _, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "echo DANGER"}); !errors.As(err, &cr) {
		t.Fatalf("err = %v, want ConfirmRequiredError", err)
	}
	if cr.Pattern != "DANGER" {
		t.Errorf("pattern = %q", cr.Pattern)
	}
	if err := e.m.Preflight("w1", "echo DANGER"); !errors.As(err, &cr) {
		t.Errorf("Preflight = %v, want ConfirmRequiredError", err)
	}
	if got := e.m.Commands(); len(got) != 0 {
		t.Errorf("an unconfirmed command was queued: %+v", got)
	}

	// Confirmed: the manager mints the token and the agent accepts it.
	id, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "echo DANGER", Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	_, out, x := collect(t, sub, id)
	if x.Reason != exec.ReasonExited || x.ExitCode != 0 || !strings.Contains(out, "DANGER") {
		t.Errorf("confirmed command: exit %+v out %q", x, out)
	}
}

// The gate is the agent's: a command that reaches it without a token is
// refused there, however the panel behaved.
func TestAgentRefusesWithoutToken(t *testing.T) {
	e := newEnv(t)
	eventually(t, "w1 online", func() bool { return e.state("w1") == fleet.StateOnline })
	if e.gate.Valid("w1", "echo DANGER", "") {
		t.Fatal("empty token must not be valid")
	}
	// Bypass the manager's own gate check by claiming confirmation for a
	// different command: the token is bound to the exact command.
	sub := e.m.Bus().Subscribe(nil)
	defer sub.Close()
	id, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "echo DANGER", Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, sub, id)
	// Replaying the same token is refused (single use): mint-and-reuse by hand.
	tok := e.gate.Mint("w1", "echo DANGER", time.Minute)
	if err := e.gate.Authorize("w1", "echo DANGER", tok); err != nil {
		t.Fatal(err)
	}
	if err := e.gate.Authorize("w1", "echo DANGER", tok); err == nil {
		t.Error("a token was accepted twice")
	}
}

func TestCancelQueuedAndRunning(t *testing.T) {
	e := newEnv(t)
	eventually(t, "w1 online", func() bool { return e.state("w1") == fleet.StateOnline })
	sub := e.m.Bus().Subscribe(nil)
	defer sub.Close()

	long, _ := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "sleep 30"})
	queued, _ := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "echo never"})
	waitRunning(t, sub, long)
	ctx := context.Background()
	if err := e.m.Cancel(ctx, queued); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	if err := e.m.Cancel(ctx, long); err != nil {
		t.Fatalf("terminate running: %v", err)
	}
	_, _, x := collect(t, sub, long)
	if x.Reason != exec.ReasonTerminated {
		t.Errorf("running command ended %+v, want terminated", x)
	}
	if c, _, _ := e.m.Command(queued); c.State != "finished" || c.Exit == nil || c.Exit.Reason != exec.ReasonCancelled {
		t.Errorf("queued command after cancel = %+v", c)
	}
	if err := e.m.Cancel(ctx, "s-unknown"); !errors.Is(err, fleet.ErrUnknown) {
		t.Errorf("cancel unknown = %v", err)
	}
}

func TestConnectionLossEndsRunningCommand(t *testing.T) {
	e := newEnv(t)
	eventually(t, "w1 online", func() bool { return e.state("w1") == fleet.StateOnline })
	sub := e.m.Bus().Subscribe(nil)
	defer sub.Close()
	id, _ := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "sleep 30"})
	waitRunning(t, sub, id)
	e.loop.SetOnline("w1", false)
	_, _, x := collect(t, sub, id)
	if x.Reason != exec.ReasonLost {
		t.Errorf("exit = %+v, want lost", x)
	}
}

func TestShellRoundTrip(t *testing.T) {
	e := newEnv(t)
	eventually(t, "w1 online", func() bool { return e.state("w1") == fleet.StateOnline })
	session := exec.NewSessionID()
	sub := e.m.Bus().Subscribe(func(ev fleet.Event) bool { return ev.Kind == fleet.EventExec && ev.Exec.SessionID == session })
	defer sub.Close()
	if _, err := e.m.OpenShell(fleet.ShellRequest{Machine: "w1", User: "alice", Session: session, Cols: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		timeout := time.After(wait)
		for !cond() {
			select {
			case ev := <-sub.C:
				if ev.Exec.Kind == exec.EventChunk {
					out.Write(ev.Exec.Chunk.Data)
				}
			case <-timeout:
				t.Fatalf("timed out waiting for %s; output %q", what, out.String())
			}
		}
	}
	waitRunning(t, sub, session)
	ctx := context.Background()
	if err := e.m.ShellResize(ctx, session, 120, 40); err != nil {
		t.Fatal(err)
	}
	if err := e.m.ShellInput(ctx, session, []byte("echo shell-$((20+22))\r")); err != nil {
		t.Fatal(err)
	}
	waitFor("shell output", func() bool { return strings.Contains(out.String(), "shell-42") })

	// A dangerous typed line is refused by the agent and then confirmed by the operator.
	if err := e.m.ShellInput(ctx, session, []byte("echo DANGER-line\r")); err != nil {
		t.Fatal(err)
	}
	waitFor("refusal note", func() bool { return strings.Contains(out.String(), "refused") })
	if strings.Contains(strings.ReplaceAll(out.String(), "echo DANGER-line", ""), "DANGER-line") {
		t.Errorf("the refused line ran: %q", out.String())
	}
	if err := e.m.ShellConfirm(ctx, session, "echo DANGER-line"); err != nil {
		t.Fatal(err)
	}
	waitFor("confirmed line output", func() bool { return strings.Count(out.String(), "DANGER-line") >= 2 })
	if err := e.m.ShellConfirm(ctx, session, "echo harmless"); !errors.Is(err, fleet.ErrBadRequest) {
		t.Errorf("confirming a harmless line = %v, want ErrBadRequest", err)
	}

	if err := e.m.Cancel(ctx, session); err != nil {
		t.Fatal(err)
	}
	exited := false
	waitFor("exit", func() bool {
		for _, c := range e.m.Commands() {
			if c.Session == session && c.State == "finished" {
				exited = true
			}
		}
		return exited
	})
}

func TestShellScrollbackRingBufferBounded(t *testing.T) {
	reg := registry.New()
	if err := reg.Add(registry.Machine{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"10.0.0.1"}, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	gate, err := exec.NewGate([]byte("test-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	loop := fleettest.NewLoopback(gate)
	m, err := fleet.New(fleet.Config{
		Registry:             reg,
		Gate:                 gate,
		NewLink:              loop.Factory(),
		PollInterval:         10 * time.Millisecond,
		ShellScrollbackBytes: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })
	eventually(t, "w1 online", func() bool {
		info, ok := m.Machine("w1")
		return ok && info.State == fleet.StateOnline
	})

	session := exec.NewSessionID()
	sub := m.Bus().Subscribe(func(ev fleet.Event) bool {
		return ev.Kind == fleet.EventExec && ev.Exec != nil && ev.Exec.SessionID == session
	})
	defer sub.Close()
	if _, err := m.OpenShell(fleet.ShellRequest{Machine: "w1", User: "alice", Session: session, Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, sub, session)

	// Send >64 bytes while detached (no AttachShell active); server drains into bounded ring buffer.
	if err := m.ShellInput(ctx, session, []byte("echo old-0123456789abcdef0123456789abcdef; echo mid-0123456789abcdef0123456789abcdef; echo tail-$((1111+8888))\r")); err != nil {
		t.Fatal(err)
	}
	var seen strings.Builder
	deadline := time.After(wait)
	for !strings.Contains(seen.String(), "tail-9999") {
		select {
		case ev := <-sub.C:
			if ev.Exec.Kind == exec.EventChunk && ev.Exec.Chunk != nil {
				seen.Write(ev.Exec.Chunk.Data)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for shell output; got %q", seen.String())
		}
	}

	att, err := m.AttachShell(fleet.AttachRequest{Session: session, User: "alice", Machine: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	defer att.Detach()
	if len(att.Scrollback) != 64 {
		t.Fatalf("scrollback len = %d, want 64", len(att.Scrollback))
	}
	if !strings.Contains(string(att.Scrollback), "tail-9999") {
		t.Errorf("scrollback missing newest tail: %q", string(att.Scrollback))
	}
	if strings.Contains(string(att.Scrollback), "old-0123456789") {
		t.Errorf("scrollback did not evict oldest bytes: %q", string(att.Scrollback))
	}

	if err := m.Cancel(ctx, session); err != nil {
		t.Fatal(err)
	}
}

func TestOnboardingUnavailableWithoutProvisioner(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.m.Onboard(context.Background(), fleet.OnboardRequest{Host: "10.9.9.9", Role: "worker"}); !errors.Is(err, fleet.ErrUnavailable) {
		t.Errorf("Onboard = %v, want ErrUnavailable", err)
	}
	if err := e.m.Remove(context.Background(), "w1", fleet.Credentials{}, true); !errors.Is(err, fleet.ErrUnavailable) {
		t.Errorf("Remove = %v, want ErrUnavailable", err)
	}
}

func TestPublicIPDispatchAndRefresh(t *testing.T) {
	m, links := newWatchdogMgr(t, fleet.WatchdogSource{}, false)
	eventually(t, "w1 online", func() bool {
		info, ok := m.Machine("w1")
		return ok && info.State == fleet.StateOnline
	})

	sub := m.Bus().Subscribe(func(ev fleet.Event) bool {
		return ev.Kind == fleet.EventMachine && ev.Machine == "w1"
	})
	defer sub.Close()

	// Inbound TypePublicIP updates registry and publishes an EventMachine.
	at := time.Date(2026, 11, 27, 21, 0, 0, 0, time.UTC)
	ctx := context.Background()
	links["w1"].h(ctx, envOf(t, channel.TypePublicIP, channel.PublicIPPayload{
		IP:        "203.0.113.55",
		At:        at,
		Confirmed: true,
	}))

	eventually(t, "registry PublicIP updated", func() bool {
		info, ok := m.Machine("w1")
		return ok && info.PublicIP == "203.0.113.55" && info.PublicIPAt != nil && info.PublicIPAt.Equal(at)
	})
	select {
	case ev := <-sub.C:
		if ev.Info == nil || ev.Info.PublicIP != "203.0.113.55" {
			t.Fatalf("bus event = %+v, want PublicIP 203.0.113.55", ev.Info)
		}
	case <-time.After(wait):
		t.Fatal("timed out waiting for EventMachine after TypePublicIP")
	}

	// Invalid IP and unknown envelope types are ignored without breaking the machine.
	links["w1"].h(ctx, envOf(t, channel.TypePublicIP, channel.PublicIPPayload{IP: "not-an-ip", At: at}))
	links["w1"].h(ctx, envOf(t, "future-unknown-type", map[string]string{"x": "1"}))
	if info, _ := m.Machine("w1"); info.PublicIP != "203.0.113.55" {
		t.Errorf("PublicIP changed on invalid payload: %q", info.PublicIP)
	}

	// Outbound RefreshPublicIP sends TypePublicIPRefresh to online machine,
	// rejects unknown and offline machines.
	if err := m.RefreshPublicIP(ctx, "w1"); err != nil {
		t.Fatalf("RefreshPublicIP(w1): %v", err)
	}
	if sent := links["w1"].sentOfType(channel.TypePublicIPRefresh); len(sent) != 1 {
		t.Fatalf("w1 received %d TypePublicIPRefresh, want 1", len(sent))
	}
	if err := m.RefreshPublicIP(ctx, "missing"); !errors.Is(err, fleet.ErrUnknown) {
		t.Errorf("RefreshPublicIP(missing) = %v, want ErrUnknown", err)
	}

	e := newEnv(t, "w2")
	if err := e.m.RefreshPublicIP(ctx, "w2"); !errors.Is(err, fleet.ErrOffline) {
		t.Errorf("RefreshPublicIP(offline w2) = %v, want ErrOffline", err)
	}
}

func TestDiskStateDispatch(t *testing.T) {
	m, links := newWatchdogMgr(t, fleet.WatchdogSource{}, false)
	eventually(t, "w1 online", func() bool {
		info, ok := m.Machine("w1")
		return ok && info.State == fleet.StateOnline
	})

	sub := m.Bus().Subscribe(func(ev fleet.Event) bool {
		return ev.Kind == fleet.EventMachine && ev.Machine == "w1"
	})
	defer sub.Close()

	at := time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC)
	ctx := context.Background()
	links["w1"].h(ctx, envOf(t, channel.TypeDiskState, channel.DiskStatePayload{
		Status:     "warn",
		TotalBytes: 1000,
		FreeBytes:  150,
		UsedPct:    85,
		WarnAt:     80,
		PauseAt:    90,
		At:         at,
	}))

	eventually(t, "registry Disk updated", func() bool {
		info, ok := m.Machine("w1")
		return ok && info.Disk.Status == "warn" && info.Disk.TotalBytes == 1000 && info.Disk.FreeBytes == 150 && info.Disk.UpdatedAt != nil && info.Disk.UpdatedAt.Equal(at)
	})
	select {
	case ev := <-sub.C:
		if ev.Info == nil || ev.Info.Disk.Status != "warn" {
			t.Fatalf("bus event = %+v, want Disk.Status warn", ev.Info)
		}
	case <-time.After(wait):
		t.Fatal("timed out waiting for EventMachine after TypeDiskState")
	}

	// Invalid status or free > total are ignored without corrupting stored disk state.
	links["w1"].h(ctx, envOf(t, channel.TypeDiskState, channel.DiskStatePayload{Status: "bogus", TotalBytes: 1000, FreeBytes: 500, At: at}))
	links["w1"].h(ctx, envOf(t, channel.TypeDiskState, channel.DiskStatePayload{Status: "ok", TotalBytes: 100, FreeBytes: 200, At: at}))
	if info, _ := m.Machine("w1"); info.Disk.Status != "warn" || info.Disk.FreeBytes != 150 {
		t.Errorf("Disk changed on invalid payload: %+v", info.Disk)
	}
}

func TestCommandHistoryPersistenceAndReload(t *testing.T) {
	dataDir := t.TempDir()
	reg := registry.New()
	if err := reg.Add(registry.Machine{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"10.0.0.1"}, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	gate, err := exec.NewGate([]byte("test-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	loop := fleettest.NewLoopback(gate)
	m1, err := fleet.New(fleet.Config{
		Registry:     reg,
		Gate:         gate,
		NewLink:      loop.Factory(),
		PollInterval: 10 * time.Millisecond,
		DataDir:      dataDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	m1.Start(ctx1)
	eventually(t, "w1 online", func() bool {
		info, ok := m1.Machine("w1")
		return ok && info.State == fleet.StateOnline
	})

	sub := m1.Bus().Subscribe(nil)
	s1, err := m1.Submit(fleet.SubmitRequest{Machine: "w1", Command: "echo persist-out; echo persist-err >&2; exit 7", User: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, ex1 := collect(t, sub, s1)
	if ex1.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", ex1.ExitCode)
	}
	s2, err := m1.Submit(fleet.SubmitRequest{Machine: "w1", Command: "sleep 30", User: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, sub, s2)
	sub.Close()

	// Simulate server shutdown / restart.
	cancel1()
	m1.Close()

	loop2 := fleettest.NewLoopback(gate)
	m2, err := fleet.New(fleet.Config{
		Registry:     reg,
		Gate:         gate,
		NewLink:      loop2.Factory(),
		PollInterval: 10 * time.Millisecond,
		DataDir:      dataDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reloaded history is immediately available even before Start or machine reconnect.
	cmds := m2.Commands()
	if len(cmds) != 2 {
		t.Fatalf("reloaded Commands() len = %d, want 2: %+v", len(cmds), cmds)
	}
	c1, segs1, ok := m2.Command(s1)
	if !ok || c1.Command != "echo persist-out; echo persist-err >&2; exit 7" || c1.User != "alice" || c1.State != "finished" || c1.Exit == nil || c1.Exit.ExitCode != 7 {
		t.Fatalf("reloaded c1 = %+v ok=%v", c1, ok)
	}
	var outText string
	for _, seg := range segs1 {
		outText += seg.Stream + ":" + seg.Text
	}
	if !strings.Contains(outText, "stdout:persist-out") || !strings.Contains(outText, "stderr:persist-err") {
		t.Errorf("reloaded output = %q", outText)
	}
	c2, _, ok := m2.Command(s2)
	if !ok || c2.State != "finished" || c2.User != "bob" {
		t.Fatalf("reloaded interrupted c2 = %+v ok=%v", c2, ok)
	}
}
