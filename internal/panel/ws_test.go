package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

func dialWithHeaders(t *testing.T, url string, hdr http.Header) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*wait)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c, ctx
}

func openAndWaitShell(t *testing.T, e *testEnv, path string, hdr http.Header) (*websocket.Conn, context.Context, shellOut) {
	t.Helper()
	c, ctx := dialWithHeaders(t, e.wsURL(path), hdr)
	var first shellOut
	if err := readJSON(ctx, c, &first); err != nil || first.Type != "session" || first.Session == "" {
		t.Fatalf("first frame = %+v err=%v", first, err)
	}
	waitShellRunning(t, ctx, c)
	return c, ctx, first
}

func TestWSShellDisconnectDoesNotCancel(t *testing.T) {
	e := newEnv(t)
	c, ctx, first := openAndWaitShell(t, e, "/ws/shell?machine=w1&cols=80&rows=24", nil)

	if err := c.Write(ctx, websocket.MessageBinary, []byte("echo stay-alive-check\r")); err != nil {
		t.Fatal(err)
	}
	readShellUntil(t, ctx, c, "stay-alive-check")

	// Browser disconnects without calling cancel.
	_ = c.Close(websocket.StatusNormalClosure, "tab closed")
	time.Sleep(150 * time.Millisecond)

	var list commandsResponse
	e.call(t, "GET", "/api/commands?mode=pty&active=1", nil, &list)
	if len(list.Commands) != 1 || list.Commands[0].Session != first.Session || list.Commands[0].State != "running" {
		t.Fatalf("expected active running session after disconnect, got %+v", list.Commands)
	}

	// Explicit terminate cleans up.
	if st := e.call(t, "POST", "/api/commands/"+first.Session+"/cancel", map[string]any{}, nil); st != http.StatusOK {
		t.Fatalf("cancel = %d", st)
	}
}

func TestWSShellReattachReplaysBufferAndStreams(t *testing.T) {
	e := newEnv(t)
	hdr := http.Header{"Remote-User": {"alice"}}
	c1, ctx1, first := openAndWaitShell(t, e, "/ws/shell?machine=w1&cols=80&rows=24", hdr)

	if err := c1.Write(ctx1, websocket.MessageBinary, []byte("echo before-detach-marker\r")); err != nil {
		t.Fatal(err)
	}
	readShellUntil(t, ctx1, c1, "before-detach-marker")

	// Subscribe to the fleet bus so we can deterministically observe output
	// drained while no WebSocket is attached.
	sub := e.m.Bus().Subscribe(func(ev fleet.Event) bool {
		return ev.Kind == fleet.EventExec && ev.Exec != nil && ev.Exec.SessionID == first.Session && ev.Exec.Kind == exec.EventChunk
	})
	defer sub.Close()

	_ = c1.Close(websocket.StatusNormalClosure, "navigate away")
	time.Sleep(80 * time.Millisecond)

	if err := e.m.ShellInput(context.Background(), first.Session, []byte("echo while-detached-marker\r")); err != nil {
		t.Fatal(err)
	}
	var detachedBuf strings.Builder
	deadline := time.After(wait)
	for !strings.Contains(detachedBuf.String(), "while-detached-marker") {
		select {
		case ev := <-sub.C:
			if ev.Exec.Chunk != nil {
				detachedBuf.Write(ev.Exec.Chunk.Data)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for detached output; saw %q", detachedBuf.String())
		}
	}

	// Reattach via ?session=<id>.
	c2, ctx2, re := openAndWaitShell(t, e, "/ws/shell?session="+first.Session+"&cols=100&rows=30", hdr)
	if re.Session != first.Session || re.Machine != "w1" {
		t.Fatalf("reattach session frame = %+v, want session=%s machine=w1", re, first.Session)
	}

	// Scrollback replay delivers both pre-detach and while-detached output.
	replayed := readShellUntil(t, ctx2, c2, "while-detached-marker")
	if !strings.Contains(replayed, "before-detach-marker") {
		t.Errorf("replayed scrollback missing before-detach-marker: %q", replayed)
	}

	// Live input + resize still work after reattach.
	if err := c2.Write(ctx2, websocket.MessageText, []byte(`{"type":"resize","cols":110,"rows":35}`)); err != nil {
		t.Fatal(err)
	}
	if err := c2.Write(ctx2, websocket.MessageBinary, []byte("echo after-reattach-marker\r")); err != nil {
		t.Fatal(err)
	}
	readShellUntil(t, ctx2, c2, "after-reattach-marker")

	if st := e.call(t, "POST", "/api/commands/"+first.Session+"/cancel", map[string]any{}, nil); st != http.StatusOK {
		t.Fatalf("cancel = %d", st)
	}
}

func TestWSShellReattachOwnershipAndErrors(t *testing.T) {
	e := newEnv(t)
	aliceHdr := http.Header{"Remote-User": {"alice"}}
	c1, ctx1, first := openAndWaitShell(t, e, "/ws/shell?machine=w1", aliceHdr)
	_ = c1.Close(websocket.StatusNormalClosure, "detach")
	_ = ctx1
	time.Sleep(80 * time.Millisecond)

	// Another operator cannot attach to alice's session.
	bobConn, bobCtx := dialWithHeaders(t, e.wsURL("/ws/shell?session="+first.Session), http.Header{"Remote-User": {"bob"}})
	var m shellOut
	if err := readJSON(bobCtx, bobConn, &m); err != nil || m.Type != "error" || !strings.Contains(m.Error, "another operator") {
		t.Errorf("bob attach = %+v err=%v, want ownership error", m, err)
	}

	// Wrong machine query param cannot attach.
	wrongMach, wmCtx := dialWithHeaders(t, e.wsURL("/ws/shell?session="+first.Session+"&machine=w2"), aliceHdr)
	m = shellOut{}
	if err := readJSON(wmCtx, wrongMach, &m); err != nil || m.Type != "error" {
		t.Errorf("wrong machine attach = %+v err=%v, want error", m, err)
	}

	// Unknown session returns error.
	unkConn, unkCtx := dialWithHeaders(t, e.wsURL("/ws/shell?session=s-doesnotexist"), aliceHdr)
	m = shellOut{}
	if err := readJSON(unkCtx, unkConn, &m); err != nil || m.Type != "error" {
		t.Errorf("unknown session attach = %+v err=%v, want error", m, err)
	}

	// Original owner can still reattach cleanly.
	c2, _, re := openAndWaitShell(t, e, "/ws/shell?session="+first.Session, aliceHdr)
	if re.Session != first.Session {
		t.Errorf("alice reattach = %+v", re)
	}
	_ = c2.Close(websocket.StatusNormalClosure, "done")
	_ = e.call(t, "POST", "/api/commands/"+first.Session+"/cancel", map[string]any{}, nil)
}

func TestWSShellSecondAttachTakesOver(t *testing.T) {
	e := newEnv(t)
	c1, ctx1, first := openAndWaitShell(t, e, "/ws/shell?machine=w1", nil)

	// Second attach takes over while first viewer is still connected.
	c2, ctx2, re := openAndWaitShell(t, e, "/ws/shell?session="+first.Session, nil)
	if re.Session != first.Session {
		t.Fatalf("second attach session = %+v", re)
	}

	// First viewer receives takeover notice and closes without killing the PTY.
	deadline := time.Now().Add(wait)
	gotEvicted := false
	for time.Now().Before(deadline) {
		typ, data, err := c1.Read(ctx1)
		if err != nil {
			break
		}
		if typ == websocket.MessageText {
			var m shellOut
			if json.Unmarshal(data, &m) == nil && m.Type == "error" && strings.Contains(m.Error, "elsewhere") {
				gotEvicted = true
				break
			}
		}
	}
	if !gotEvicted {
		t.Errorf("first viewer did not receive takeover notice")
	}
	_ = c1.CloseNow()

	// Second viewer remains live.
	if err := c2.Write(ctx2, websocket.MessageBinary, []byte("echo takeover-alive\r")); err != nil {
		t.Fatal(err)
	}
	readShellUntil(t, ctx2, c2, "takeover-alive")

	_ = e.call(t, "POST", "/api/commands/"+first.Session+"/cancel", map[string]any{}, nil)
}

func TestWSShellExplicitTerminateEndsSession(t *testing.T) {
	e := newEnv(t)
	c, ctx, first := openAndWaitShell(t, e, "/ws/shell?machine=w1", nil)

	if st := e.call(t, "POST", "/api/commands/"+first.Session+"/cancel", map[string]any{}, nil); st != http.StatusOK {
		t.Fatalf("cancel = %d", st)
	}

	// Attached viewer receives exit frame.
	gotExit := false
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		typ, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		if typ == websocket.MessageText {
			var m shellOut
			if json.Unmarshal(data, &m) == nil && m.Type == "exit" {
				gotExit = true
				break
			}
		}
	}
	if !gotExit {
		t.Fatalf("attached viewer did not receive exit frame on terminate")
	}

	// Active list becomes empty and reattach is refused.
	var list commandsResponse
	deadline = time.Now().Add(wait)
	for time.Now().Before(deadline) {
		e.call(t, "GET", "/api/commands?mode=pty&active=1", nil, &list)
		if len(list.Commands) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(list.Commands) != 0 {
		t.Fatalf("session still active after terminate: %+v", list.Commands)
	}

	c2, ctx2 := dialWithHeaders(t, e.wsURL("/ws/shell?session="+first.Session), nil)
	var m shellOut
	if err := readJSON(ctx2, c2, &m); err != nil || m.Type != "error" {
		t.Errorf("reattach after terminate = %+v err=%v, want error", m, err)
	}
}

func TestWSShellIdleReaperEndsDetachedSession(t *testing.T) {
	reg := registry.New()
	if err := reg.Add(registry.Machine{ID: "w1", Role: registry.RoleWorker, PrivateIPs: []string{"10.0.0.1"}, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	gate, err := exec.NewGate([]byte("test-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	loop := fleettest.NewLoopback(gate)
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	m, err := fleet.New(fleet.Config{
		Registry:         reg,
		Gate:             gate,
		NewLink:          loop.Factory(),
		Logger:           logger,
		PollInterval:     10 * time.Millisecond,
		ShellIdleTimeout: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	h, err := New(Options{Assets: web.FS, Templates: web.Templates, Backend: m, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); cancel(); m.Close() })

	e := &testEnv{srv: srv, m: m, reg: reg, loop: loop, logs: logs}
	e.waitState(t, "w1", fleet.StateOnline)

	c, shCtx, first := openAndWaitShell(t, e, "/ws/shell?machine=w1", nil)
	_ = shCtx

	// While attached, the session must NOT be reaped even past ShellIdleTimeout.
	time.Sleep(160 * time.Millisecond)
	var list commandsResponse
	e.call(t, "GET", "/api/commands?mode=pty&active=1", nil, &list)
	if len(list.Commands) != 1 || list.Commands[0].Session != first.Session {
		t.Fatalf("attached session was reaped prematurely: %+v", list.Commands)
	}

	// Detach; reaper must terminate and log it after ShellIdleTimeout.
	_ = c.Close(websocket.StatusNormalClosure, "detach")
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		e.call(t, "GET", "/api/commands?mode=pty&active=1", nil, &list)
		if len(list.Commands) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(list.Commands) != 0 {
		t.Fatalf("detached session was not reaped: %+v", list.Commands)
	}
	if !strings.Contains(logs.String(), "reaping idle shell session") || !strings.Contains(logs.String(), first.Session) {
		t.Errorf("reap log missing from logs: %s", logs.String())
	}
}
