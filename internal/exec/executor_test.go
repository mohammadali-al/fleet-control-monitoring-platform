package exec

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

const (
	testMachine = "agent-01"
	waitFor     = 15 * time.Second
)

// ---- harness: an Executor and a Queue joined by an in-memory link ----

// link is a Sender that hands each envelope straight to a handler, in order.
type link struct {
	mu sync.Mutex
	fn func(channel.Envelope)
}

func (l *link) Send(_ context.Context, env channel.Envelope) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fn(env)
	return nil
}

type recorder struct {
	mu  sync.Mutex
	evs []Event
}

func (r *recorder) on(ev Event) {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.evs...)
}

func (r *recorder) kinds(id string) []string {
	var k []string
	for _, ev := range r.snapshot() {
		if ev.SessionID == id && ev.Kind != EventChunk {
			k = append(k, ev.Kind)
		}
	}
	return k
}

func (r *recorder) index(id, kind string) int {
	for i, ev := range r.snapshot() {
		if ev.SessionID == id && ev.Kind == kind {
			return i
		}
	}
	return -1
}

func (r *recorder) stream(id, stream string) string {
	var b strings.Builder
	for _, ev := range r.snapshot() {
		if ev.Kind == EventChunk && ev.SessionID == id && (stream == "" || ev.Chunk.Stream == stream) {
			b.Write(ev.Chunk.Data)
		}
	}
	return b.String()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (r *recorder) waitKind(t *testing.T, id, kind string) {
	t.Helper()
	eventually(t, kind+" of "+id, func() bool { return r.index(id, kind) >= 0 })
}

func (r *recorder) waitExit(t *testing.T, id string) ExitPayload {
	t.Helper()
	r.waitKind(t, id, EventExited)
	for _, ev := range r.snapshot() {
		if ev.SessionID == id && ev.Kind == EventExited {
			return *ev.Exit
		}
	}
	panic("unreachable")
}

func (r *recorder) waitOutput(t *testing.T, id, stream, want string) {
	t.Helper()
	eventually(t, fmt.Sprintf("%q in %s output of %s", want, stream, id), func() bool {
		return strings.Contains(r.stream(id, stream), want)
	})
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type testEnv struct {
	t    *testing.T
	ex   *Executor
	q    *Queue
	rec  *recorder
	gate *Gate
	logs *syncBuf
	dir  string
}

// newEnv wires an executor and queue. The gate list is the single pattern
// DANGER so that tests never run a genuinely destructive command.
func newEnv(t *testing.T, mutate func(*Config)) *testEnv {
	t.Helper()
	te := &testEnv{t: t, rec: &recorder{}, logs: &syncBuf{}, dir: t.TempDir()}
	te.gate = newTestGate(t, []string{`DANGER`})
	toAgent, toServer := &link{}, &link{}
	cfg := Config{
		MachineID: testMachine, Host: "testhost", Sender: toServer, Gate: te.gate,
		TermGrace: 500 * time.Millisecond,
		Logger:    slog.New(slog.NewTextHandler(te.logs, nil)),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	ex, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	te.ex = ex
	te.q = NewQueue(testMachine, toAgent, te.rec.on)
	toAgent.fn = func(env channel.Envelope) { ex.Handle(context.Background(), env) }
	toServer.fn = func(env channel.Envelope) { te.q.Handle(context.Background(), env) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); te.q.Run(ctx) }()
	t.Cleanup(func() {
		ex.Shutdown()
		cancel()
		<-done
	})
	return te
}

func (te *testEnv) submit(req RunPayload) string {
	te.t.Helper()
	id, err := te.q.Submit(req)
	if err != nil {
		te.t.Fatalf("Submit: %v", err)
	}
	return id
}

func (te *testEnv) runShell(command string, timeoutSec int) (string, ExitPayload) {
	te.t.Helper()
	id := te.submit(RunPayload{Command: command, User: "op", TimeoutSec: timeoutSec})
	return id, te.rec.waitExit(te.t, id)
}

// ---- Mode A ----

func TestOneShotStreamsAndExit(t *testing.T) {
	te := newEnv(t, nil)
	id, x := te.runShell("echo to-out; echo to-err >&2; sleep 0.2; exit 3", 0)
	if x.Reason != ReasonExited || x.ExitCode != 3 {
		t.Fatalf("exit = %+v, want reason exited code 3", x)
	}
	if x.DurationMS < 150 || x.DurationMS > 5000 {
		t.Errorf("duration %d ms, want roughly 200", x.DurationMS)
	}
	if got := te.rec.stream(id, StreamStdout); got != "to-out\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := te.rec.stream(id, StreamStderr); got != "to-err\n" {
		t.Errorf("stderr = %q", got)
	}
	want := []string{EventQueued, EventRunning, EventExited}
	if got := te.rec.kinds(id); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("lifecycle = %v, want %v", got, want)
	}
	// Chunks arrive before the exit.
	if te.rec.index(id, EventExited) < len(te.rec.snapshot())-1 {
		t.Errorf("events after exit: %v", te.rec.kinds(id))
	}
}

func TestOneShotZeroExit(t *testing.T) {
	te := newEnv(t, nil)
	_, x := te.runShell("true", 0)
	if x.ExitCode != 0 || x.Reason != ReasonExited {
		t.Fatalf("exit = %+v", x)
	}
}

func TestOneShotLargeOutput(t *testing.T) {
	te := newEnv(t, nil)
	id, x := te.runShell("head -c 300000 /dev/zero | tr '\\0' x", 0)
	if x.ExitCode != 0 {
		t.Fatalf("exit = %+v", x)
	}
	if got := len(te.rec.stream(id, StreamStdout)); got != 300000 {
		t.Errorf("streamed %d bytes, want 300000", got)
	}
}

func TestOneShotTimeout(t *testing.T) {
	te := newEnv(t, nil)
	start := time.Now()
	_, x := te.runShell("sleep 30", 1)
	if x.Reason != ReasonTimeout {
		t.Fatalf("exit = %+v, want timeout", x)
	}
	if x.ExitCode == 0 {
		t.Errorf("timed-out command reported exit 0")
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("timeout took %v to bite", d)
	}
}

func TestOneShotTimeoutKillsChildren(t *testing.T) {
	te := newEnv(t, nil)
	marker := filepath.Join(te.dir, "child-survived")
	// The grandchild would write the marker after 2 s if it outlived the timeout.
	_, x := te.runShell("(sleep 2; touch "+marker+") & wait", 1)
	if x.Reason != ReasonTimeout {
		t.Fatalf("exit = %+v", x)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("child process survived the timeout")
	}
}

func TestOneShotDefaultTimeoutApplied(t *testing.T) {
	te := newEnv(t, func(c *Config) { c.DefaultTimeout = time.Second })
	_, x := te.runShell("sleep 30", 0)
	if x.Reason != ReasonTimeout {
		t.Fatalf("exit = %+v, want the configured default timeout to apply", x)
	}
	ex, _ := New(Config{MachineID: "m", Sender: &link{}, Gate: te.gate})
	if ex.cfg.DefaultTimeout != 300*time.Second {
		t.Errorf("default timeout = %v, want 300 s", ex.cfg.DefaultTimeout)
	}
}

func TestOneShotTerminate(t *testing.T) {
	te := newEnv(t, nil)
	id := te.submit(RunPayload{Command: "echo up; sleep 30"})
	te.rec.waitOutput(t, id, StreamStdout, "up")
	if err := te.q.Terminate(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	x := te.rec.waitExit(t, id)
	if x.Reason != ReasonTerminated {
		t.Fatalf("exit = %+v, want terminated", x)
	}
	if x.ExitCode != 128+15 {
		t.Errorf("exit code = %d, want 143 (SIGTERM)", x.ExitCode)
	}
}

func TestTerminateEscalatesToKill(t *testing.T) {
	te := newEnv(t, nil)
	// The shell ignores SIGTERM; after the grace period it is killed.
	id := te.submit(RunPayload{Command: "trap '' TERM; echo up; while :; do sleep 0.1; done"})
	te.rec.waitOutput(t, id, StreamStdout, "up")
	start := time.Now()
	_ = te.q.Terminate(context.Background(), id)
	x := te.rec.waitExit(t, id)
	if x.Reason != ReasonTerminated {
		t.Fatalf("exit = %+v", x)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("SIGKILL escalation took %v", d)
	}
}

func TestInvalidSessionID(t *testing.T) {
	te := newEnv(t, nil)
	if _, err := te.q.Submit(RunPayload{SessionID: "../escape", Command: "true"}); err == nil {
		t.Error("queue accepted a path-traversal session id")
	}
	// The agent rejects one that reaches it anyway.
	te.ex.run(RunPayload{SessionID: "../escape", Command: "true"})
}

func TestSessionMetadataAndHooks(t *testing.T) {
	var started SessionInfo
	var startedMu sync.Mutex
	ended := make(chan SessionInfo, 1)
	te := newEnv(t, func(c *Config) {
		c.OnSessionStart = func(s *Session) { startedMu.Lock(); started = s.Info(); startedMu.Unlock() }
		c.OnSessionEnd = func(i SessionInfo) { ended <- i }
	})
	id, _ := te.runShell("exit 2", 0)
	info := <-ended
	startedMu.Lock()
	defer startedMu.Unlock()
	if started.ID != id || started.Command != "exit 2" || started.User != "op" || started.Host != "testhost" || started.Mode != ModeOneShot {
		t.Errorf("session at start = %+v", started)
	}
	if !info.Finished || info.ExitCode != 2 || info.End.Before(info.Start) || info.Start.IsZero() {
		t.Errorf("session at end = %+v", info)
	}
}

// ---- cgroups (skipped when the environment gives us none) ----

// testCgroupRoot returns a writable cgroup v2 directory for a test, or skips.
// Root can use /sys/fs/cgroup; an unprivileged user can use the subtree
// systemd delegated to their session.
func testCgroupRoot(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil || !strings.HasPrefix(string(b), "0::") {
		t.Skip("cgroup v2 not available; skipping cgroup test")
	}
	own := "/sys/fs/cgroup" + strings.TrimSpace(strings.TrimPrefix(strings.SplitN(string(b), "\n", 2)[0], "0::"))
	root := filepath.Join(own, fmt.Sprintf("fleet-test-%d", os.Getpid()))
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Skipf("cannot create cgroups here (need root or a delegated cgroup): %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(root) })
	return root
}

func TestOneShotRunsInSessionCgroup(t *testing.T) {
	root := testCgroupRoot(t)
	var hookPath string
	var mu sync.Mutex
	te := newEnv(t, func(c *Config) {
		c.CgroupRoot = root
		c.OnSessionStart = func(s *Session) { mu.Lock(); hookPath = s.CgroupPath(); mu.Unlock() }
	})
	id, x := te.runShell("cat /proc/self/cgroup", 0)
	if x.ExitCode != 0 {
		t.Fatalf("exit = %+v", x)
	}
	want := filepath.Join(root, id)
	got := te.rec.stream(id, StreamStdout)
	if !strings.HasSuffix(strings.TrimSpace(got), strings.TrimPrefix(want, "/sys/fs/cgroup")) {
		t.Errorf("command saw cgroup %q, want it to end with %q", strings.TrimSpace(got), strings.TrimPrefix(want, "/sys/fs/cgroup"))
	}
	mu.Lock()
	if hookPath != want {
		t.Errorf("Session.CgroupPath = %q, want %q", hookPath, want)
	}
	mu.Unlock()
	// The running state advertised the path to the server.
	for _, ev := range te.rec.snapshot() {
		if ev.Kind == EventRunning && ev.SessionID == id && ev.State.CgroupPath != want {
			t.Errorf("running state cgroup = %q, want %q", ev.State.CgroupPath, want)
		}
	}
	// Cleaned up afterwards.
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Errorf("session cgroup %s still exists (err=%v)", want, err)
	}
}

func TestCgroupTimeoutKillsEscapedChildren(t *testing.T) {
	root := testCgroupRoot(t)
	te := newEnv(t, func(c *Config) { c.CgroupRoot = root })
	marker := filepath.Join(te.dir, "escaped-survived")
	// setsid puts the grandchild in a new process group and session; only the
	// cgroup can still reach it.
	_, x := te.runShell("setsid sh -c 'sleep 2; touch "+marker+"' & sleep 30", 1)
	if x.Reason != ReasonTimeout {
		t.Fatalf("exit = %+v", x)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a process that left the process group survived the cgroup kill")
	}
}

func TestCgroupFailureFailsCommand(t *testing.T) {
	// A cgroup root that cannot be created must not silently run untracked.
	te := newEnv(t, func(c *Config) { c.CgroupRoot = "/proc/definitely/not/a/cgroup" })
	marker := filepath.Join(te.dir, "ran")
	_, x := te.runShell("touch "+marker, 0)
	if x.Reason != ReasonError {
		t.Fatalf("exit = %+v, want error", x)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("command ran even though its cgroup could not be created")
	}
}

// ---- serial queue ----

func TestQueueRunsStrictlyInOrder(t *testing.T) {
	te := newEnv(t, nil)
	log := filepath.Join(te.dir, "order.log")
	script := func(name, sleep string) string {
		return fmt.Sprintf("echo start-%s >> %s; sleep %s; echo end-%s >> %s", name, log, sleep, name, log)
	}
	// Submit all three at once; A is slowest, so any concurrency would show.
	a := te.submit(RunPayload{Command: script("A", "0.5")})
	b := te.submit(RunPayload{Command: script("B", "0.1")})
	c := te.submit(RunPayload{Command: script("C", "0")})

	// B and C are visibly queued while A runs.
	snap := te.q.Snapshot()
	if len(snap) < 3 || snap[0].State != StateRunning && snap[0].State != StateQueued {
		t.Errorf("snapshot = %+v", snap)
	}
	for _, id := range []string{a, b, c} {
		te.rec.waitExit(t, id)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "start-A\nend-A\nstart-B\nend-B\nstart-C\nend-C\n"
	if string(got) != want {
		t.Errorf("execution order:\n%s\nwant:\n%s", got, want)
	}
	// Event order too: each command's running comes after the previous exit.
	if !(te.rec.index(a, EventExited) < te.rec.index(b, EventRunning) &&
		te.rec.index(b, EventExited) < te.rec.index(c, EventRunning)) {
		t.Error("a later command started running before the previous one exited")
	}
	for _, id := range []string{a, b, c} {
		if k := strings.Join(te.rec.kinds(id), ","); k != "queued,running,exited" {
			t.Errorf("%s lifecycle = %s", id, k)
		}
	}
	// Finished entries are listed most recent first.
	if s := te.q.Snapshot(); len(s) != 3 || s[0].SessionID != c || s[0].State != StateFinished || s[0].Exit == nil {
		t.Errorf("final snapshot = %+v", s)
	}
}

func TestQueueCancelQueued(t *testing.T) {
	te := newEnv(t, nil)
	marker := filepath.Join(te.dir, "cancelled-ran")
	a := te.submit(RunPayload{Command: "sleep 0.5"})
	b := te.submit(RunPayload{Command: "touch " + marker})
	if err := te.q.Cancel(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if x := te.rec.waitExit(t, b); x.Reason != ReasonCancelled {
		t.Errorf("exit = %+v", x)
	}
	te.rec.waitExit(t, a)
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("cancelled command ran")
	}
	if err := te.q.Cancel(context.Background(), "nope"); err != ErrUnknownSession {
		t.Errorf("Cancel(unknown) = %v", err)
	}
}

func TestQueueConnectionLost(t *testing.T) {
	// The agent never answers: the running entry is closed out as lost so the
	// FIFO can move on.
	rec := &recorder{}
	black := &link{fn: func(channel.Envelope) {}}
	q := NewQueue("m", black, rec.on)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	id, _ := q.Submit(RunPayload{Command: "x"})
	eventually(t, "entry running", func() bool { s := q.Snapshot(); return len(s) > 0 && s[0].State == StateRunning })
	q.ConnectionLost()
	if x := rec.waitExit(t, id); x.Reason != ReasonLost {
		t.Errorf("exit = %+v", x)
	}
}

// ---- dangerous-command gate, Mode A ----

func TestGateRefusesOneShotWithoutToken(t *testing.T) {
	te := newEnv(t, nil)
	marker := filepath.Join(te.dir, "danger-ran")
	cmd := "echo DANGER > " + marker

	id, x := te.runShell(cmd, 0)
	if x.Reason != ReasonRefused || x.ExitCode != exitRefused {
		t.Fatalf("exit = %+v, want refused", x)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("refused command ran")
	}
	if k := strings.Join(te.rec.kinds(id), ","); k != "queued,refused,exited" {
		t.Errorf("lifecycle = %s", k)
	}
	logs := te.logs.String()
	for _, want := range []string{"dangerous command refused", "session_id=" + id, "user=op", "machine=" + testMachine} {
		if !strings.Contains(logs, want) {
			t.Errorf("refusal log missing %q:\n%s", want, logs)
		}
	}
}

func TestGateBrowserOkCannotBypass(t *testing.T) {
	te := newEnv(t, nil)
	marker := filepath.Join(te.dir, "danger-ran")
	cmd := "echo DANGER > " + marker
	other, _ := NewGate([]byte("browser-guess-browser-guess-0000"), []string{`DANGER`})
	for _, tok := range []string{"ok", "true", "yes", "confirmed", other.Mint(testMachine, cmd, time.Minute), te.gate.Mint("other-machine", cmd, time.Minute)} {
		id := te.submit(RunPayload{Command: cmd, ConfirmToken: tok})
		if x := te.rec.waitExit(t, id); x.Reason != ReasonRefused {
			t.Fatalf("token %.12q: exit = %+v, want refused", tok, x)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a non-server token let the dangerous command run")
	}
}

func TestGateRunsOneShotWithToken(t *testing.T) {
	te := newEnv(t, nil)
	marker := filepath.Join(te.dir, "danger-ran")
	cmd := "echo DANGER > " + marker
	tok := te.gate.Mint(testMachine, cmd, time.Minute)

	id := te.submit(RunPayload{Command: cmd, ConfirmToken: tok})
	if x := te.rec.waitExit(t, id); x.Reason != ReasonExited || x.ExitCode != 0 {
		t.Fatalf("exit = %+v, want it to run", x)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("confirmed command did not run: %v", err)
	}
	// The token is spent.
	_ = os.Remove(marker)
	id = te.submit(RunPayload{Command: cmd, ConfirmToken: tok})
	if x := te.rec.waitExit(t, id); x.Reason != ReasonRefused {
		t.Errorf("replay: exit = %+v, want refused", x)
	}
}

func TestSafeCommandIgnoresGate(t *testing.T) {
	te := newEnv(t, nil)
	_, x := te.runShell("echo fine", 0)
	if x.Reason != ReasonExited {
		t.Fatalf("exit = %+v", x)
	}
}

// ---- Mode B: PTY ----

func startPTY(te *testEnv) string {
	te.t.Helper()
	id := te.submit(RunPayload{Mode: ModePTY, User: "op", Cols: 80, Rows: 24})
	te.rec.waitKind(te.t, id, EventRunning)
	return id
}

func input(t *testing.T, te *testEnv, id, data, token string) {
	t.Helper()
	if err := te.q.Input(context.Background(), id, []byte(data), token); err != nil {
		t.Fatalf("Input: %v", err)
	}
}

func TestPTYRoundTripResizeTerminate(t *testing.T) {
	te := newEnv(t, nil)
	id := startPTY(te)

	input(t, te, id, "echo hello-$((1+2))\n", "")
	te.rec.waitOutput(t, id, StreamPTY, "hello-3")

	if err := te.q.Resize(context.Background(), id, 100, 40); err != nil {
		t.Fatal(err)
	}
	input(t, te, id, "stty size\n", "")
	te.rec.waitOutput(t, id, StreamPTY, "40 100")

	if err := te.q.Terminate(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if x := te.rec.waitExit(t, id); x.Reason != ReasonTerminated {
		t.Fatalf("exit = %+v, want terminated", x)
	}
}

func TestPTYInitialSizeAndShellExit(t *testing.T) {
	te := newEnv(t, nil)
	id := te.submit(RunPayload{Mode: ModePTY, Cols: 132, Rows: 50})
	te.rec.waitKind(t, id, EventRunning)
	input(t, te, id, "stty size; exit 5\n", "")
	te.rec.waitOutput(t, id, StreamPTY, "50 132")
	x := te.rec.waitExit(t, id)
	if x.Reason != ReasonExited || x.ExitCode != 5 {
		t.Fatalf("exit = %+v, want exited 5", x)
	}
}

func TestPTYSessionTaggedLikeOneShot(t *testing.T) {
	var got SessionInfo
	var mu sync.Mutex
	te := newEnv(t, func(c *Config) {
		c.OnSessionStart = func(s *Session) { mu.Lock(); got = s.Info(); mu.Unlock() }
	})
	id := startPTY(te)
	input(t, te, id, "exit\n", "")
	te.rec.waitExit(t, id)
	mu.Lock()
	defer mu.Unlock()
	if got.ID != id || got.Mode != ModePTY || got.User != "op" || got.Host != "testhost" {
		t.Errorf("pty session = %+v", got)
	}
}

func TestPTYInSessionCgroup(t *testing.T) {
	root := testCgroupRoot(t)
	te := newEnv(t, func(c *Config) { c.CgroupRoot = root })
	id := startPTY(te)
	input(t, te, id, "cat /proc/self/cgroup; exit\n", "")
	te.rec.waitExit(t, id)
	if out := te.rec.stream(id, StreamPTY); !strings.Contains(out, "/"+filepath.Base(root)+"/"+id) {
		t.Errorf("pty shell not in the session cgroup; output:\n%s", out)
	}
}

func TestPTYGateRefusesLineWithoutToken(t *testing.T) {
	te := newEnv(t, nil)
	marker := filepath.Join(te.dir, "pty-danger-ran")
	line := "echo DANGER > " + marker
	id := startPTY(te)

	input(t, te, id, line+"\n", "")
	te.rec.waitKind(t, id, EventRefused)
	te.rec.waitOutput(t, id, StreamPTY, "[fleet] refused")
	// A browser "ok" is not a token either.
	input(t, te, id, line+"\n", "ok")
	eventually(t, "second refusal", func() bool {
		n := 0
		for _, ev := range te.rec.snapshot() {
			if ev.Kind == EventRefused {
				n++
			}
		}
		return n == 2
	})
	// Prove the shell is still alive and nothing executed.
	input(t, te, id, "echo alive-$((2*3))\n", "")
	te.rec.waitOutput(t, id, StreamPTY, "alive-6")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("refused PTY line was executed")
	}
	if !strings.Contains(te.logs.String(), "dangerous command refused") {
		t.Errorf("PTY refusal not logged:\n%s", te.logs.String())
	}
	input(t, te, id, "exit\n", "")
	te.rec.waitExit(t, id)
}

func TestPTYGateRunsLineWithInlineToken(t *testing.T) {
	te := newEnv(t, nil)
	marker := filepath.Join(te.dir, "pty-danger-ran")
	line := "echo DANGER > " + marker
	id := startPTY(te)

	input(t, te, id, line+"\n", te.gate.Mint(testMachine, line, time.Minute))
	eventually(t, "confirmed PTY line to run", func() bool { _, err := os.Stat(marker); return err == nil })
	input(t, te, id, "exit\n", "")
	te.rec.waitExit(t, id)
}

func TestPTYGateConfirmEnvelopeFlow(t *testing.T) {
	te := newEnv(t, nil)
	marker := filepath.Join(te.dir, "pty-danger-ran")
	line := "echo DANGER > " + marker
	id := startPTY(te)

	// 1. refused; 2. operator confirms, server mints + sends confirm; 3. retype.
	input(t, te, id, line+"\n", "")
	te.rec.waitKind(t, id, EventRefused)
	if err := te.q.Confirm(context.Background(), id, te.gate.Mint(testMachine, line, time.Minute)); err != nil {
		t.Fatal(err)
	}
	input(t, te, id, line+"\n", "")
	eventually(t, "confirmed PTY line to run", func() bool { _, err := os.Stat(marker); return err == nil })
	input(t, te, id, "exit\n", "")
	te.rec.waitExit(t, id)
}

func TestQueueSerialisesPTYAndOneShot(t *testing.T) {
	te := newEnv(t, nil)
	id := startPTY(te)
	next := te.submit(RunPayload{Command: "echo after-pty"})
	time.Sleep(300 * time.Millisecond)
	if te.rec.index(next, EventRunning) >= 0 {
		t.Fatal("a command started while the PTY session held the machine")
	}
	input(t, te, id, "exit\n", "")
	te.rec.waitExit(t, id)
	te.rec.waitOutput(t, next, StreamStdout, "after-pty")
}

// ---- over the real channel ----

func TestOverRealChannel(t *testing.T) {
	ca, err := pki.NewCA(pki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	loop := []net.IP{net.ParseIP("127.0.0.1")}
	agentCert, agentKey, err := ca.IssuePeer(testMachine, loop, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mainCert, mainKey, err := ca.IssuePeer("main-server", loop, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srvTLS, err := pki.ServerTLSConfig(agentCert, agentKey, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	dialTLS, err := pki.DialTLSConfig(mainCert, mainKey, ca.CertPEM(), loop[0])
	if err != nil {
		t.Fatal(err)
	}

	gate := newTestGate(t, []string{`DANGER`})
	var ex *Executor
	connected := make(chan struct{}, 4)
	l, err := channel.Listen(channel.ListenerConfig{
		Addr: "127.0.0.1:0", TLS: srvTLS, MachineID: testMachine,
		Handler:   func(ctx context.Context, env channel.Envelope) { ex.Handle(ctx, env) },
		OnConnect: func(string) { connected <- struct{}{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	ex, err = New(Config{MachineID: testMachine, Sender: l, Gate: gate, TermGrace: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Shutdown()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srvDone := make(chan struct{})
	go func() { defer close(srvDone); _ = l.Serve(ctx) }()
	defer func() { cancel(); <-srvDone }()

	rec := &recorder{}
	var q *Queue
	d, err := channel.NewDialer(channel.DialerConfig{
		Addr: l.Addr().String(), TLS: dialTLS, MachineID: "main-server",
		Handler: func(ctx context.Context, env channel.Envelope) { q.Handle(ctx, env) },
	})
	if err != nil {
		t.Fatal(err)
	}
	q = NewQueue(testMachine, d, rec.on)
	dialDone := make(chan struct{})
	go func() { defer close(dialDone); _ = d.Run(ctx) }()
	defer func() { cancel(); <-dialDone }()
	go q.Run(ctx)

	select {
	case <-connected:
	case <-time.After(waitFor):
		t.Fatal("channel did not connect")
	}
	// The agent side is up first; wait until the dialer side can send too.
	eventually(t, "dialer connection", func() bool { return d.AgentID() != "" })
	id, err := q.Submit(RunPayload{Command: "echo over-the-wire; exit 4"})
	if err != nil {
		t.Fatal(err)
	}
	x := rec.waitExit(t, id)
	if x.ExitCode != 4 || x.Reason != ReasonExited {
		t.Errorf("exit = %+v", x)
	}
	if got := rec.stream(id, StreamStdout); got != "over-the-wire\n" {
		t.Errorf("stdout = %q", got)
	}
	// And a refusal crosses the wire too.
	id2, _ := q.Submit(RunPayload{Command: "echo DANGER"})
	if x := rec.waitExit(t, id2); x.Reason != ReasonRefused {
		t.Errorf("exit = %+v, want refused", x)
	}
}
