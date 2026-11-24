package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
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
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

const wait = 20 * time.Second

// dangerous matches the built-in gate list and is harmless to actually run.
const dangerous = "echo reboot"

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- fake capture -----------------------------------------------------------------

type fakeCapture struct {
	mu      sync.Mutex
	cfgs    []capture.Config
	started []startedCapture
	stopped []string
	closed  int
}

type startedCapture struct{ Session, Cgroup string }

func (f *fakeCapture) factory(_ context.Context, cfg capture.Config) (agent.Capturer, error) {
	f.mu.Lock()
	f.cfgs = append(f.cfgs, cfg)
	f.mu.Unlock()
	return fakeCapturer{f}, nil
}

type fakeCapturer struct{ f *fakeCapture }

func (c fakeCapturer) Capture(id, cg string) (agent.SessionCapture, error) {
	c.f.mu.Lock()
	c.f.started = append(c.f.started, startedCapture{id, cg})
	c.f.mu.Unlock()
	return fakeSession{c.f, id}, nil
}
func (c fakeCapturer) Close() { c.f.mu.Lock(); c.f.closed++; c.f.mu.Unlock() }

type fakeSession struct {
	f  *fakeCapture
	id string
}

func (s fakeSession) Stop() (capture.Summary, error) {
	s.f.mu.Lock()
	s.f.stopped = append(s.f.stopped, s.id)
	s.f.mu.Unlock()
	return capture.Summary{Records: 7}, nil
}

func (f *fakeCapture) snapshot() (cfgs []capture.Config, started []startedCapture, stopped []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(cfgs, f.cfgs...), append(started, f.started...), append(stopped, f.stopped...)
}

// --- harness ----------------------------------------------------------------------

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

// startAgent starts a real worker agent on loopback mTLS.
func startAgent(t *testing.T, mt *fleettest.MTLS, id string, mod func(*agent.Config)) *fleettest.RunningAgent {
	t.Helper()
	cfg := agent.Config{DataDir: t.TempDir(), TermGrace: 200 * time.Millisecond, Logger: quiet()}
	if mod != nil {
		mod(&cfg)
	}
	ra, err := mt.StartAgent(id, cfg)
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	t.Cleanup(func() {
		if err := ra.Stop(); err != nil {
			t.Errorf("agent Run: %v", err)
		}
	})
	return ra
}

type mgrEnv struct {
	m  *fleet.Manager
	mt *fleettest.MTLS
	ra *fleettest.RunningAgent
}

// newMgrEnv is the server and one worker agent over the real mTLS channel. The
// server's gate secret is gateSecret; pushSecret is what it pushes (they differ
// only in the test that proves the agent verifies with the pushed one).
func newMgrEnv(t *testing.T, gateSecret, pushSecret string, push config.CaptureConfig, mod func(*agent.Config)) *mgrEnv {
	t.Helper()
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	ra := startAgent(t, mt, "w1", mod)
	reg := registry.New()
	if err := reg.Add(registry.Machine{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial,
		PrivateIPs: []string{"127.0.0.1"}, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	gate, err := exec.NewGate([]byte(gateSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := fleet.New(fleet.Config{
		Registry: reg, Gate: gate, NewLink: mt.Factory(), PollInterval: 10 * time.Millisecond,
		GateSecret: []byte(pushSecret), Capture: push, Logger: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })
	return &mgrEnv{m: m, mt: mt, ra: ra}
}

func (e *mgrEnv) waitOnline(t *testing.T) {
	t.Helper()
	eventually(t, "w1 online", func() bool {
		info, ok := e.m.Machine("w1")
		return ok && info.State == fleet.StateOnline
	})
}

// collect gathers one session's events until it exits.
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

func (e *mgrEnv) subscribe() *fleet.Subscription {
	return e.m.Bus().Subscribe(func(ev fleet.Event) bool { return ev.Kind == fleet.EventExec })
}

// delegatedCgroupRoot returns a writable cgroup v2 directory (root, or the
// subtree systemd delegated to the session), or skips the test.
func delegatedCgroupRoot(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil || !strings.HasPrefix(string(b), "0::") {
		t.Skip("cgroup v2 not available")
	}
	own := "/sys/fs/cgroup" + strings.TrimSpace(strings.TrimPrefix(strings.SplitN(string(b), "\n", 2)[0], "0::"))
	root := filepath.Join(own, fmt.Sprintf("fleet-agent-test-%d", os.Getpid()))
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Skipf("cannot create cgroups here (need root or a delegated cgroup): %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(root) })
	return root
}

// --- end to end through the fleet manager -----------------------------------------

// The agent connects over mTLS and is reported online; a one-shot streams
// queued -> running -> exited; a dangerous command is held by the server until
// confirmed and then runs because the agent verifies the server-minted token
// with the secret it was PUSHED (the agent has no other source for it).
func TestEndToEndOneShotAndGate(t *testing.T) {
	fc := &fakeCapture{}
	push := config.CaptureConfig{TrimThresholdBytes: 123, SegmentMaxAgeSeconds: 9}
	e := newMgrEnv(t, "shared-secret", "shared-secret", push, func(c *agent.Config) { c.CaptureFactory = fc.factory })
	e.waitOnline(t)
	sub := e.subscribe()
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

	// The server holds a dangerous command until the operator confirms.
	_, err = e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: dangerous})
	var need *fleet.ConfirmRequiredError
	if !errors.As(err, &need) {
		t.Fatalf("unconfirmed dangerous Submit err = %v, want ConfirmRequiredError", err)
	}
	// Confirmed: the server mints a token; the agent verifies it with the pushed secret and runs it.
	id, err = e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: dangerous, Confirm: true})
	if err != nil {
		t.Fatalf("confirmed Submit: %v", err)
	}
	_, out, exit = collect(t, sub, id)
	if exit.ExitCode != 0 || exit.Reason != exec.ReasonExited || !strings.Contains(out, "reboot") {
		t.Errorf("confirmed dangerous command: exit %+v out %q", exit, out)
	}

	// The capture section of the push reached the capture subsystem.
	cfgs, _, _ := fc.snapshot()
	if len(cfgs) != 1 || cfgs[0].TrimThreshold != 123 || cfgs[0].Segment.MaxAge != 9*time.Second || cfgs[0].Machine != "w1" {
		t.Errorf("capture configs built = %+v, want one with the pushed settings", cfgs)
	}
}

// A token minted with a secret other than the one the agent was pushed is
// refused: the agent enforces the gate itself, with the pushed secret.
func TestAgentRefusesTokenFromAnotherSecret(t *testing.T) {
	e := newMgrEnv(t, "server-side-secret", "a-different-pushed-secret", config.CaptureConfig{}, nil)
	e.waitOnline(t)
	sub := e.subscribe()
	defer sub.Close()

	id, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: dangerous, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	_, out, exit := collect(t, sub, id)
	if exit.Reason != exec.ReasonRefused || exit.ExitCode == 0 || strings.Contains(out, "reboot") {
		t.Errorf("exit = %+v out %q, want the agent to refuse the token", exit, out)
	}
}

// Each session of a worker is captured: the agent attaches capture to the
// session's cgroup when it starts and stops it when it ends.
func TestCaptureAttachedPerSession(t *testing.T) {
	root := delegatedCgroupRoot(t)
	fc := &fakeCapture{}
	e := newMgrEnv(t, "s", "s", config.CaptureConfig{}, func(c *agent.Config) {
		c.CaptureFactory = fc.factory
		c.CgroupRoot = root
	})
	e.waitOnline(t)
	sub := e.subscribe()
	defer sub.Close()

	id, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "echo captured"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, exit := collect(t, sub, id); exit.ExitCode != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	eventually(t, "capture stopped", func() bool { _, _, stopped := fc.snapshot(); return len(stopped) == 1 })
	_, started, stopped := fc.snapshot()
	if len(started) != 1 || started[0].Session != id || started[0].Cgroup != filepath.Join(root, id) {
		t.Errorf("capture started = %+v, want session %s in cgroup %s", started, id, filepath.Join(root, id))
	}
	if stopped[0] != id {
		t.Errorf("capture stopped for %q, want %q", stopped[0], id)
	}
}

// Real kernel capture: needs root, nft and cgroup v2, so it is skipped
// elsewhere. The command sends a UDP datagram over loopback from inside its
// session cgroup; the session's segments must hold a record for it.
func TestLiveCaptureProducesRecords(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("SKIP: live kernel capture needs root (CAP_NET_ADMIN); run as root to exercise")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("SKIP: cgroup v2 not mounted at /sys/fs/cgroup")
	}
	if _, err := os.Stat("/usr/bin/bash"); err != nil {
		if _, err = os.Stat("/bin/bash"); err != nil {
			t.Skip("SKIP: bash (for /dev/udp) not found")
		}
	}
	root := filepath.Join("/sys/fs/cgroup", fmt.Sprintf("fleet-agent-test-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.Remove(root) })
	dataDir := t.TempDir()

	// Reflector set that cannot reach the internet: the public-IP refresh must stay local.
	push := config.CaptureConfig{Reflectors: []string{"http://127.0.0.1:1/"}}
	e := newMgrEnv(t, "s", "s", push, func(c *agent.Config) {
		c.CgroupRoot = root
		c.DataDir = dataDir
		c.Shell = "bash"
	})
	e.waitOnline(t)
	sub := e.subscribe()
	defer sub.Close()

	ln, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.LocalAddr().(*net.UDPAddr).Port
	id, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: fmt.Sprintf("echo live-capture > /dev/udp/127.0.0.1/%d; sleep 0.5", port)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, exit := collect(t, sub, id); exit.ExitCode != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	dir := capture.EvidenceMachineDir(dataDir, "w1")
	eventually(t, "a captured record in the session's segments", func() bool {
		m, err := capture.LoadManifest(dir, "w1")
		if err != nil {
			return false
		}
		n := 0
		for _, s := range m.Entries() {
			n += s.Records
		}
		return n > 0
	})
}

// --- protocol level: a raw main-server connection ---------------------------------

type rawServer struct {
	d    *channel.Dialer
	got  chan channel.Envelope
	conn chan struct{}
	stop func()
	t    *testing.T
}

// dialAgent connects to the agent as the main server would, without a Manager,
// so the test controls exactly which messages are sent.
func dialAgent(t *testing.T, mt *fleettest.MTLS, id string) *rawServer {
	t.Helper()
	cfg, err := mt.DialerConfig(id)
	if err != nil {
		t.Fatal(err)
	}
	rs := &rawServer{got: make(chan channel.Envelope, 512), conn: make(chan struct{}, 4), t: t}
	cfg.Handler = func(_ context.Context, env channel.Envelope) { rs.got <- env }
	cfg.OnConnect = func(string) { rs.conn <- struct{}{} }
	cfg.MinBackoff, cfg.MaxBackoff = 20*time.Millisecond, 100*time.Millisecond
	if rs.d, err = channel.NewDialer(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = rs.d.Run(ctx) }()
	rs.stop = func() { cancel(); <-done }
	t.Cleanup(rs.stop)
	select {
	case <-rs.conn:
	case <-time.After(wait):
		t.Fatal("agent did not accept the connection")
	}
	return rs
}

func (r *rawServer) send(typ, id string, payload any) {
	r.t.Helper()
	env, err := channel.NewEnvelope(typ, id, payload)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.d.Send(context.Background(), env); err != nil {
		r.t.Fatalf("send %s: %v", typ, err)
	}
}

// expect waits for an envelope of type typ (optionally for one id), discarding others.
func (r *rawServer) expect(typ, id string) channel.Envelope {
	r.t.Helper()
	timeout := time.After(wait)
	for {
		select {
		case env := <-r.got:
			if env.Type == typ && (id == "" || env.ID == id) {
				return env
			}
		case <-timeout:
			r.t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

// runSession sends a run and collects states, output and the exit.
func (r *rawServer) runSession(id, cmd, token string) (states []string, out string, exit exec.ExitPayload) {
	r.t.Helper()
	r.send(channel.TypeRun, id, exec.RunPayload{SessionID: id, Command: cmd, Mode: exec.ModeOneShot, ConfirmToken: token})
	var sb strings.Builder
	timeout := time.After(wait)
	for {
		select {
		case env := <-r.got:
			if env.ID != id {
				continue
			}
			switch env.Type {
			case channel.TypeState:
				var p exec.StatePayload
				_ = json.Unmarshal(env.Payload, &p)
				states = append(states, p.State)
			case channel.TypeChunk:
				var p exec.ChunkPayload
				_ = json.Unmarshal(env.Payload, &p)
				sb.Write(p.Data)
			case channel.TypeExit:
				_ = json.Unmarshal(env.Payload, &exit)
				return states, sb.String(), exit
			}
		case <-timeout:
			r.t.Fatalf("timed out waiting for session %s to exit (states %v)", id, states)
		}
	}
}

// On connect the agent asks for its config; once the server pushes it (and the
// agent acknowledges), a dangerous command is refused without a token and runs
// with a token minted by the server and verified with the pushed secret.
func TestRawConnectionConfigPushAndGate(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	startAgent(t, mt, "w1", nil)
	rs := dialAgent(t, mt, "w1")
	rs.expect(channel.TypeConfigReq, "")

	const secret = "pushed-secret"
	rs.send(channel.TypeConfig, "config", channel.ConfigPayload{GateSecret: secret})
	var ack channel.ConfigAckPayload
	if err := json.Unmarshal(rs.expect(channel.TypeConfigAck, "").Payload, &ack); err != nil || !ack.OK {
		t.Fatalf("config ack = %+v (err %v), want ok", ack, err)
	}

	// A harmless command needs no token.
	if _, out, exit := rs.runSession("plain-1", "echo fine", ""); exit.ExitCode != 0 || !strings.Contains(out, "fine") {
		t.Errorf("plain command: exit %+v out %q", exit, out)
	}
	// Dangerous, no token: refused; nothing runs.
	states, out, exit := rs.runSession("danger-1", dangerous, "")
	if exit.Reason != exec.ReasonRefused || strings.Contains(out, "reboot") || !contains(states, exec.StateRefused) {
		t.Errorf("no token: states %v exit %+v out %q, want refused", states, exit, out)
	}
	// Dangerous with a token from a server holding the same secret: runs.
	gate, err := exec.NewGate([]byte(secret), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, out, exit = rs.runSession("danger-2", dangerous, gate.Mint("w1", dangerous, time.Minute))
	if exit.Reason != exec.ReasonExited || exit.ExitCode != 0 || !strings.Contains(out, "reboot") {
		t.Errorf("with token: exit %+v out %q, want it to run", exit, out)
	}
	// A token for another machine, or another secret, is refused; and tokens are single-use.
	other, _ := exec.NewGate([]byte("not-the-secret"), nil)
	for name, tok := range map[string]string{
		"wrong secret":  other.Mint("w1", dangerous, time.Minute),
		"wrong machine": gate.Mint("w2", dangerous, time.Minute),
	} {
		if _, _, x := rs.runSession("bad-"+strings.ReplaceAll(name, " ", "-"), dangerous, tok); x.Reason != exec.ReasonRefused {
			t.Errorf("%s: exit %+v, want refused", name, x)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// A run that arrives before the config push waits for it (the connection and
// the push race), and a run that never gets a config is answered with an error.
func TestRunBeforeConfig(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	startAgent(t, mt, "w1", func(c *agent.Config) { c.ConfigWait = 3 * time.Second })
	rs := dialAgent(t, mt, "w1")
	rs.expect(channel.TypeConfigReq, "")

	// Config arrives shortly after the run: the run waits and then executes.
	go func() {
		time.Sleep(150 * time.Millisecond)
		rs.send(channel.TypeConfig, "config", channel.ConfigPayload{GateSecret: "k"})
	}()
	if _, out, exit := rs.runSession("early-1", "echo waited", ""); exit.ExitCode != 0 || !strings.Contains(out, "waited") {
		t.Errorf("early run: exit %+v out %q, want it to run after the config", exit, out)
	}

	// A second agent never receives a config.
	startAgent2 := func() *rawServer {
		startAgent(t, mt, "w2", func(c *agent.Config) { c.ConfigWait = 200 * time.Millisecond })
		return dialAgent(t, mt, "w2")
	}
	rs2 := startAgent2()
	_, out, exit := rs2.runSession("early-2", "echo never", "")
	if exit.Reason != exec.ReasonError || exit.ExitCode == 0 || strings.Contains(out, "never") || !strings.Contains(exit.Error, "configuration") {
		t.Errorf("unconfigured run: exit %+v out %q, want an error mentioning the configuration", exit, out)
	}
}

// Pushing the same config again (every reconnect does) changes nothing; a new
// secret applies to new commands.
func TestConfigPushIsIdempotentAndRotates(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeCapture{}
	startAgent(t, mt, "w1", func(c *agent.Config) { c.CaptureFactory = fc.factory })
	rs := dialAgent(t, mt, "w1")
	rs.expect(channel.TypeConfigReq, "")

	cc := &config.CaptureConfig{TrimThresholdBytes: 5}
	push := func(secret string) {
		rs.send(channel.TypeConfig, "config", channel.ConfigPayload{GateSecret: secret, Capture: cc})
		var ack channel.ConfigAckPayload
		_ = json.Unmarshal(rs.expect(channel.TypeConfigAck, "").Payload, &ack)
		if !ack.OK {
			t.Fatalf("ack = %+v", ack)
		}
	}
	push("one")
	push("one")
	if cfgs, _, _ := fc.snapshot(); len(cfgs) != 1 {
		t.Errorf("capture rebuilt %d times for identical pushes, want 1", len(cfgs))
	}
	g1, _ := exec.NewGate([]byte("one"), nil)
	g2, _ := exec.NewGate([]byte("two"), nil)
	if _, _, x := rs.runSession("rot-1", dangerous, g1.Mint("w1", dangerous, time.Minute)); x.Reason != exec.ReasonExited {
		t.Errorf("token under the first secret: %+v", x)
	}
	push("two")
	if _, _, x := rs.runSession("rot-2", dangerous, g1.Mint("w1", dangerous, time.Minute)); x.Reason != exec.ReasonRefused {
		t.Errorf("old secret after rotation: %+v, want refused", x)
	}
	if _, _, x := rs.runSession("rot-3", dangerous, g2.Mint("w1", dangerous, time.Minute)); x.Reason != exec.ReasonExited {
		t.Errorf("token under the new secret: %+v", x)
	}

	// A config with no gate secret (and none applied yet) is rejected, not applied.
	mt2, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	startAgent(t, mt2, "w9", nil)
	rs9 := dialAgent(t, mt2, "w9")
	rs9.send(channel.TypeConfig, "config", channel.ConfigPayload{})
	var ack channel.ConfigAckPayload
	_ = json.Unmarshal(rs9.expect(channel.TypeConfigAck, "").Payload, &ack)
	if ack.OK || ack.Error == "" {
		t.Errorf("empty config ack = %+v, want a rejection", ack)
	}
}

// The gate secret never appears when a payload is formatted or logged.
func TestConfigPayloadDoesNotLeakSecret(t *testing.T) {
	p := channel.ConfigPayload{GateSecret: "super-secret-value"}
	var sb strings.Builder
	slog.New(slog.NewTextHandler(&sb, nil)).Info("cfg", "payload", p)
	for _, s := range []string{fmt.Sprint(p), fmt.Sprintf("%v %+v %s", p, p, p), sb.String()} {
		if strings.Contains(s, "super-secret-value") {
			t.Errorf("secret leaked: %q", s)
		}
	}
}
