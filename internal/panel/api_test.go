package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/onboard"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

const wait = 15 * time.Second

func registryDisk(total, free uint64) registry.DiskState {
	return registry.DiskState{TotalBytes: total, FreeBytes: free}
}

// fakeRemote answers every remote command successfully: the machine looks
// freshly installed (no cert yet, so one is issued) and nothing real runs.
type fakeRemote struct{ cmds *[]string }

func (f fakeRemote) Run(_ context.Context, cmd string, _ []byte) ([]byte, error) {
	*f.cmds = append(*f.cmds, cmd)
	return nil, nil
}
func (f fakeRemote) Close() error { return nil }

type testEnv struct {
	srv  *httptest.Server
	m    *fleet.Manager
	reg  *registry.Registry
	loop *fleettest.Loopback
	ssh  *[]string // commands the fake SSH remote received
	logs *bytes.Buffer
}

// newEnv serves the panel over a manager with a registry holding worker w1, a
// monitor mon1 and a second worker w2 that starts offline. The dangerous list
// is the single pattern DANGER.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	reg := registry.New()
	for _, m := range []registry.Machine{
		{ID: "w1", Role: registry.RoleWorker, PrivateIPs: []string{"10.0.0.1"}, Provisioned: true},
		{ID: "mon1", Role: registry.RoleMonitor, PrivateIPs: []string{"10.0.0.3"}, Provisioned: true},
		{ID: "w2", Role: registry.RoleWorker, PrivateIPs: []string{"10.0.0.2"}, Provisioned: true},
	} {
		if err := reg.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := exec.NewGate([]byte("test-secret"), []string{`DANGER`})
	if err != nil {
		t.Fatal(err)
	}
	loop := fleettest.NewLoopback(gate)
	loop.StartOffline("w2")

	ca, err := pki.NewCA(pki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ssh := new([]string)
	prov := &onboard.Provisioner{
		CA: ca, Registry: reg, AgentBinaryPath: bin,
		Dial: func(context.Context, onboard.Target, string) (onboard.Remote, string, error) {
			return fakeRemote{ssh}, "SHA256:testhostkey", nil
		},
	}
	m, err := fleet.New(fleet.Config{Registry: reg, Provisioner: prov, Gate: gate, NewLink: loop.Factory(), PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)

	logs := &bytes.Buffer{}
	h, err := New(Options{Assets: web.FS, Templates: web.Templates, Backend: m, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); cancel(); m.Close() })

	e := &testEnv{srv: srv, m: m, reg: reg, loop: loop, ssh: ssh, logs: logs}
	e.waitState(t, "w1", fleet.StateOnline)
	return e
}

func (e *testEnv) waitState(t *testing.T, id, state string) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if info, ok := e.m.Machine(id); ok && info.State == state {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("machine %s never became %s", id, state)
}

// call does a JSON request and decodes the reply into out (if not nil).
func (e *testEnv) call(t *testing.T, method, path string, in any, out any, hdr ...string) int {
	t.Helper()
	var rd io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: bad JSON %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

func (e *testEnv) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(e.srv.URL, "http") + path
}

// ---- machines ----

func TestMachinesAPIReflectsRegistry(t *testing.T) {
	e := newEnv(t)
	var got machinesResponse
	if st := e.call(t, "GET", "/api/machines", nil, &got); st != 200 {
		t.Fatalf("status %d", st)
	}
	states := map[string]string{}
	for _, m := range got.Machines {
		states[m.ID] = m.State
	}
	if states["w1"] != "online" || states["w2"] != "offline" || states["mon1"] != "online" {
		t.Errorf("states = %v", states)
	}
	if got.Summary.Workers != 2 || got.Summary.WorkersOnline != 1 || got.Summary.Monitors != 1 {
		t.Errorf("summary = %+v", got.Summary)
	}
}

func TestMachinesPageShowsRegistry(t *testing.T) {
	e := newEnv(t)
	_, _, html := body(t, e.srv.URL+"/machines.html")
	for _, want := range []string{`data-id="w1"`, `data-id="mon1"`, `data-id="w2"`, "Worker agent", "Monitor agent", "server_dial", "10.0.0.1",
		`href="terminal.html?machine=w1"`, "Monitor agents have no shell", "Terminal unavailable: machine is offline"} {
		if !strings.Contains(html, want) {
			t.Errorf("machines page missing %q", want)
		}
	}
	// Live data, not the mock rows.
	if strings.Contains(html, "wrk-01") || strings.Contains(html, "Mock data</span>") {
		t.Error("the machines page still shows mock data")
	}
	// A monitor never gets an enabled terminal link.
	if strings.Contains(html, "terminal.html?machine=mon1") {
		t.Error("a monitor agent has a terminal link")
	}
}

func TestFragments(t *testing.T) {
	e := newEnv(t)
	_, hdr, rows := body(t, e.srv.URL+"/fragments/machines-rows")
	if !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") || !strings.Contains(rows, `data-id="w1"`) || strings.Contains(rows, "<html") {
		t.Errorf("machines-rows fragment = %.200s", rows)
	}
	_, _, tiles := body(t, e.srv.URL+"/fragments/machines-tiles")
	if !strings.Contains(tiles, "Worker agents") {
		t.Errorf("machines-tiles fragment = %.200s", tiles)
	}
	_, _, targets := body(t, e.srv.URL+"/fragments/command-targets")
	if !strings.Contains(targets, `value="w1"`) || strings.Contains(targets, `value="mon1"`) {
		t.Errorf("command targets must list workers only: %.300s", targets)
	}
	if !strings.Contains(targets, `value="w2" disabled`) {
		t.Errorf("offline worker should be disabled: %.400s", targets)
	}
	_, _, hosts := body(t, e.srv.URL+"/fragments/shell-hosts")
	if strings.Contains(hosts, "mon1") {
		t.Errorf("monitor agents must not be selectable for a shell: %s", hosts)
	}
	_, _, pre := body(t, e.srv.URL+"/fragments/shell-hosts?machine=w1")
	if !strings.Contains(pre, `value="w1" selected`) {
		t.Errorf("preselected host missing: %s", pre)
	}
}

func TestOnboardRemoveReprovision(t *testing.T) {
	e := newEnv(t)
	req := map[string]any{"host": "10.0.0.9", "port": 22, "user": "provision", "password": "s3cret-pass", "role": "worker", "connect_mode": "server_dial"}

	var res provisionResponse
	if st := e.call(t, "POST", "/api/machines", req, &res); st != http.StatusCreated {
		t.Fatalf("onboard status %d", st)
	}
	id := res.Machine.ID
	if id == "" || !res.Created || !res.CertIssued || len(res.Steps) == 0 {
		t.Fatalf("onboard result = %+v", res)
	}
	if !res.Machine.Provisioned || res.Machine.Role != "worker" || res.Machine.CertFingerprint == "" {
		t.Errorf("machine = %+v", res.Machine.Machine)
	}
	// The new machine is in the registry, comes online over its link, and shows on the page.
	e.waitState(t, id, fleet.StateOnline)
	_, _, html := body(t, e.srv.URL+"/machines.html")
	if !strings.Contains(html, `data-id="`+id+`"`) || !strings.Contains(html, "10.0.0.9") {
		t.Error("onboarded machine is not on the Machines page")
	}
	// The password never reaches a log or a remote command line.
	if strings.Contains(e.logs.String(), "s3cret-pass") {
		t.Error("the SSH password was logged")
	}
	for _, c := range *e.ssh {
		if strings.Contains(c, "s3cret-pass") {
			t.Errorf("password on a remote command line: %s", c)
		}
	}

	// Re-provision is idempotent: same ID, no new entry, no new certificate.
	var again provisionResponse
	if st := e.call(t, "POST", "/api/machines/"+id+"/reprovision", map[string]any{"user": "provision", "password": "x"}, &again); st != 200 {
		t.Fatalf("reprovision status %d", st)
	}
	if again.Machine.ID != id || again.Created {
		t.Errorf("reprovision = %+v", again)
	}
	var list machinesResponse
	e.call(t, "GET", "/api/machines", nil, &list)
	n := 0
	for _, m := range list.Machines {
		if m.ID == id {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d registry entries for %s after re-provision", n, id)
	}

	// Remove: decommissioned, kept as a record, its link is gone.
	if st := e.call(t, "POST", "/api/machines/"+id+"/remove", map[string]any{"user": "provision", "password": "x"}, nil); st != 200 {
		t.Fatalf("remove status %d", st)
	}
	info, _ := e.m.Machine(id)
	if !info.Removed || info.State != fleet.StateOffline || info.RevokedFingerprint == "" {
		t.Errorf("after remove: %+v", info)
	}
	// A removed machine takes no commands.
	var sub submitResponse
	if st := e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{id}, "command": "true"}, &sub); st != http.StatusNotFound {
		t.Errorf("command on a removed machine = %d, want 404", st)
	}
	// Removing without reaching the machine (registry only).
	if st := e.call(t, "POST", "/api/machines/w2/remove", map[string]any{"skip_remote": true}, nil); st != 200 {
		t.Errorf("registry-only remove status %d", st)
	}
	if st := e.call(t, "POST", "/api/machines/nope/remove", map[string]any{"skip_remote": true}, nil); st != http.StatusNotFound {
		t.Errorf("remove unknown = %d, want 404", st)
	}
}

func TestOnboardValidation(t *testing.T) {
	e := newEnv(t)
	good := func() map[string]any {
		return map[string]any{"host": "10.0.0.9", "user": "u", "password": "p", "role": "worker"}
	}
	mut := func(k string, v any) map[string]any { m := good(); m[k] = v; return m }
	cases := []struct {
		name string
		body any
	}{
		{"not an IP", mut("host", "machine.example")},
		{"bad role", mut("role", "boss")},
		{"bad connect mode", mut("connect_mode", "both")},
		{"no user", mut("user", "")},
		{"no credentials", mut("password", "")},
		{"bad port", mut("port", 70000)},
		{"unknown field", mut("shell", "x")},
	}
	for _, c := range cases {
		var out errorBody
		if st := e.call(t, "POST", "/api/machines", c.body, &out); st != http.StatusBadRequest || out.Error == "" {
			t.Errorf("%s: status %d body %+v, want 400 with an error", c.name, st, out)
		}
	}
	var n machinesResponse
	e.call(t, "GET", "/api/machines", nil, &n)
	if len(n.Machines) != 3 {
		t.Errorf("a rejected request changed the registry: %d machines", len(n.Machines))
	}
}

func TestOnboardWithoutAgentBinary(t *testing.T) {
	e := newEnv(t)
	e.m.Close() // detach: build a second panel whose provisioner has no binary
	reg := registry.New()
	ca, _ := pki.NewCA(pki.Options{})
	gate, _ := exec.NewGate([]byte("k"), nil)
	m, err := fleet.New(fleet.Config{Registry: reg, Gate: gate,
		Provisioner: &onboard.Provisioner{CA: ca, Registry: reg}})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := New(Options{Assets: web.FS, Templates: web.Templates, Backend: m})
	srv := httptest.NewServer(h)
	defer srv.Close()
	b, _ := json.Marshal(map[string]any{"host": "10.0.0.9", "user": "u", "password": "p", "role": "worker"})
	resp, err := http.Post(srv.URL+"/api/machines", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), "agent binary") {
		t.Errorf("status %d body %s, want 503 naming the agent binary", resp.StatusCode, raw)
	}
}

func TestStateChangingAPIRequiresJSON(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Post(e.srv.URL+"/api/commands", "application/x-www-form-urlencoded", strings.NewReader("command=id"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("form POST = %d, want 415", resp.StatusCode)
	}
}

// ---- commands ----

func TestCommandSubmitHistoryAndGate(t *testing.T) {
	e := newEnv(t)
	var sub submitResponse
	st := e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"w1"}, "command": "echo hello-panel", "timeout_sec": 30}, &sub, "Remote-User", "alice")
	if st != http.StatusAccepted || len(sub.Results) != 1 || sub.Results[0].Session == "" {
		t.Fatalf("submit = %d %+v", st, sub)
	}
	session := sub.Results[0].Session
	var got commandResponse
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		e.call(t, "GET", "/api/commands/"+session, nil, &got)
		if got.Command.State == "finished" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Command.State != "finished" || got.Command.Exit == nil || got.Command.Exit.ExitCode != 0 {
		t.Fatalf("command = %+v", got.Command)
	}
	if got.Command.User != "alice" {
		t.Errorf("user = %q, want the forwarded auth header's alice", got.Command.User)
	}
	if len(got.Output) != 1 || got.Output[0].Stream != "stdout" || got.Output[0].Text != "hello-panel\n" {
		t.Errorf("output = %+v", got.Output)
	}

	var hist commandsResponse
	e.call(t, "GET", "/api/commands?mode=oneshot", nil, &hist)
	if len(hist.Commands) != 1 || hist.Commands[0].Session != session {
		t.Errorf("history = %+v", hist.Commands)
	}
	e.call(t, "GET", "/api/commands?mode=pty", nil, &hist)
	if len(hist.Commands) != 0 {
		t.Errorf("pty filter returned %d one-shot entries", len(hist.Commands))
	}
	var nf errorBody
	if st := e.call(t, "GET", "/api/commands/s-unknown", nil, &nf); st != http.StatusNotFound {
		t.Errorf("unknown session = %d", st)
	}

	// The gate: no confirmation means nothing is queued and the panel asks.
	var gate gateResponse
	st = e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"w1"}, "command": "echo DANGER"}, &gate)
	if st != http.StatusConflict || !gate.NeedsConfirmation || gate.Pattern != "DANGER" || len(gate.Machines) != 1 {
		t.Fatalf("gate = %d %+v", st, gate)
	}
	e.call(t, "GET", "/api/commands?mode=oneshot", nil, &hist)
	if len(hist.Commands) != 1 {
		t.Errorf("an unconfirmed dangerous command was queued: %+v", hist.Commands)
	}
	// Confirmed: it runs, carrying the server-minted token the agent verified.
	st = e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"w1"}, "command": "echo DANGER", "confirm": true}, &sub)
	if st != http.StatusAccepted {
		t.Fatalf("confirmed submit = %d", st)
	}
	deadline = time.Now().Add(wait)
	for time.Now().Before(deadline) {
		e.call(t, "GET", "/api/commands/"+sub.Results[0].Session, nil, &got)
		if got.Command.State == "finished" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Command.Exit == nil || got.Command.Exit.Reason != exec.ReasonExited || len(got.Output) == 0 || !strings.Contains(got.Output[0].Text, "DANGER") {
		t.Errorf("confirmed command = %+v output %+v", got.Command, got.Output)
	}
	if !strings.Contains(e.logs.String(), "dangerous command confirmed") {
		t.Error("the confirmation was not logged")
	}
}

func TestCommandTargetErrors(t *testing.T) {
	e := newEnv(t)
	var out submitResponse
	if st := e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"mon1"}, "command": "true"}, &out); st != http.StatusUnprocessableEntity {
		t.Errorf("command on a monitor = %d, want 422", st)
	}
	if len(out.Results) != 1 || !strings.Contains(out.Results[0].Error, "monitor") {
		t.Errorf("monitor refusal = %+v", out.Results)
	}
	if st := e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"w2"}, "command": "true"}, &out); st != http.StatusConflict {
		t.Errorf("command on an offline machine = %d, want 409", st)
	}
	// Fan-out: the online worker still runs while the offline one reports its error.
	if st := e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"w1", "w2"}, "command": "true"}, &out); st != http.StatusAccepted {
		t.Errorf("fan-out status = %d, want 202", st)
	}
	if len(out.Results) != 2 || out.Results[0].Session == "" || out.Results[1].Error == "" {
		t.Errorf("fan-out results = %+v", out.Results)
	}
	var bad errorBody
	if st := e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{}, "command": "true"}, &bad); st != http.StatusBadRequest {
		t.Errorf("no targets = %d, want 400", st)
	}
	if st := e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"w1"}, "command": "  "}, &bad); st != http.StatusBadRequest {
		t.Errorf("blank command = %d, want 400", st)
	}
}

func TestQueueVisibleAndCancel(t *testing.T) {
	e := newEnv(t)
	var a, b submitResponse
	e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"w1"}, "command": "sleep 30"}, &a)
	e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"w1"}, "command": "echo behind"}, &b)
	var list commandsResponse
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		e.call(t, "GET", "/api/commands?active=1", nil, &list)
		if len(list.Commands) == 2 && list.Commands[1].State == "running" && list.Commands[0].State == "queued" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(list.Commands) != 2 || list.Commands[0].Position != 1 || list.Commands[0].Session != b.Results[0].Session {
		t.Fatalf("queue = %+v, want the second command queued at position 1 behind the first", list.Commands)
	}
	for _, s := range []string{b.Results[0].Session, a.Results[0].Session} {
		if st := e.call(t, "POST", "/api/commands/"+s+"/cancel", map[string]any{}, nil); st != 200 {
			t.Errorf("cancel %s = %d", s, st)
		}
	}
	deadline = time.Now().Add(wait)
	for time.Now().Before(deadline) {
		e.call(t, "GET", "/api/commands?active=1", nil, &list)
		if len(list.Commands) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("commands still active after cancel: %+v", list.Commands)
}

// ---- WebSockets ----

func dial(t *testing.T, url string) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*wait)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c, ctx
}

// One command streamed over /ws/events: queued, running, chunks, exited.
func TestWSCommandStream(t *testing.T) {
	e := newEnv(t)
	c, ctx := dial(t, e.wsURL("/ws/events"))

	var hello map[string]string
	if err := readJSON(ctx, c, &hello); err != nil || hello["type"] != "hello" {
		t.Fatalf("first frame = %v %v", hello, err)
	}

	var sub submitResponse
	cmd := "echo line-one; echo line-two >&2; sleep 0.2; echo line-three; exit 2"
	if st := e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"w1"}, "command": cmd}, &sub); st != http.StatusAccepted {
		t.Fatalf("submit = %d", st)
	}
	session := sub.Results[0].Session

	var events []string
	var stdout, stderr strings.Builder
	var exit *exec.ExitPayload
	for exit == nil {
		var m eventMsg
		if err := readJSON(ctx, c, &m); err != nil {
			t.Fatalf("read: %v (events so far %v)", err, events)
		}
		if m.Session != session {
			continue // machine events and other sessions
		}
		switch m.Type {
		case "command":
			events = append(events, m.Event)
			if m.On != "w1" || m.Mode != "oneshot" || m.Command != cmd {
				t.Errorf("command frame = %+v", m)
			}
			if m.Event == "exited" {
				exit = m.Exit
			}
		case "chunk":
			if m.Stream == "stdout" {
				stdout.Write(m.Data) // []byte: base64 on the wire
			} else {
				stderr.Write(m.Data)
			}
		}
	}
	if strings.Join(events, ",") != "queued,running,exited" {
		t.Errorf("lifecycle = %v", events)
	}
	if exit.ExitCode != 2 || exit.Reason != exec.ReasonExited {
		t.Errorf("exit = %+v", exit)
	}
	if stdout.String() != "line-one\nline-three\n" || stderr.String() != "line-two\n" {
		t.Errorf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

func TestWSEventsCarryMachineState(t *testing.T) {
	e := newEnv(t)
	c, ctx := dial(t, e.wsURL("/ws/events"))
	var hello map[string]string
	_ = readJSON(ctx, c, &hello)
	e.loop.SetOnline("w2", true)
	for {
		var m eventMsg
		if err := readJSON(ctx, c, &m); err != nil {
			t.Fatalf("no machine event: %v", err)
		}
		if m.Type == "machine" && m.Machine.ID == "w2" && m.Machine.State == "online" {
			return
		}
	}
}

func TestWSEventsDoNotLeakShellOutput(t *testing.T) {
	e := newEnv(t)
	ev, evCtx := dial(t, e.wsURL("/ws/events"))
	var hello map[string]string
	_ = readJSON(evCtx, ev, &hello)

	sh, shCtx := dial(t, e.wsURL("/ws/shell?machine=w1&cols=80&rows=24"))
	waitShellRunning(t, shCtx, sh)
	if err := sh.Write(shCtx, websocket.MessageBinary, []byte("echo secret-shell-output\r")); err != nil {
		t.Fatal(err)
	}
	readShellUntil(t, shCtx, sh, "secret-shell-output")

	// Everything the events socket saw about the shell is lifecycle only.
	evCtx2, cancel := context.WithTimeout(evCtx, 300*time.Millisecond)
	defer cancel()
	for {
		typ, data, err := ev.Read(evCtx2)
		if err != nil {
			break
		}
		if typ == websocket.MessageText && strings.Contains(string(data), `"chunk"`) {
			t.Fatalf("shell output leaked to /ws/events: %s", data)
		}
	}
}

func readJSON(ctx context.Context, c *websocket.Conn, v any) error {
	_, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// waitShellRunning consumes frames until the shell is running.
func waitShellRunning(t *testing.T, ctx context.Context, c *websocket.Conn) {
	t.Helper()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for the shell: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var m shellOut
		_ = json.Unmarshal(data, &m)
		if m.Type == "error" {
			t.Fatalf("shell error: %s", m.Error)
		}
		if m.Type == "state" && m.State == "running" {
			return
		}
	}
}

// readShellUntil reads terminal output until want appears.
func readShellUntil(t *testing.T, ctx context.Context, c *websocket.Conn, want string) string {
	t.Helper()
	var out strings.Builder
	for !strings.Contains(out.String(), want) {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v (output %q)", want, err, out.String())
		}
		if typ == websocket.MessageBinary {
			out.Write(data)
		}
	}
	return out.String()
}

func TestWSShell(t *testing.T) {
	e := newEnv(t)
	c, ctx := dial(t, e.wsURL("/ws/shell?machine=w1&cols=100&rows=30"))

	// First frame names the session.
	var first shellOut
	if err := readJSON(ctx, c, &first); err != nil || first.Type != "session" || first.Machine != "w1" || first.Session == "" {
		t.Fatalf("first frame = %+v %v", first, err)
	}
	waitShellRunning(t, ctx, c)

	// It is listed as an open shell, and keys reach the PTY.
	var list commandsResponse
	e.call(t, "GET", "/api/commands?mode=pty&active=1", nil, &list)
	if len(list.Commands) != 1 || list.Commands[0].Session != first.Session {
		t.Errorf("open shells = %+v", list.Commands)
	}
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`))
	_ = c.Write(ctx, websocket.MessageBinary, []byte("echo pty-$((6*7))\r"))
	readShellUntil(t, ctx, c, "pty-42")

	// A dangerous line is refused by the agent; the panel offers it for confirmation.
	_ = c.Write(ctx, websocket.MessageBinary, []byte("echo DANGER-typed\r"))
	var refused shellOut
	for refused.Type != "refused" {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for refusal: %v", err)
		}
		if typ == websocket.MessageText {
			_ = json.Unmarshal(data, &refused)
		}
	}
	if refused.Line != "echo DANGER-typed" {
		t.Errorf("refused line = %q", refused.Line)
	}
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"confirm","line":"echo DANGER-typed"}`))
	out := readShellUntil(t, ctx, c, "DANGER-typed")
	_ = out

	// Closing the socket detaches without cancelling the session.
	_ = c.Close(websocket.StatusNormalClosure, "bye")
	time.Sleep(100 * time.Millisecond)
	e.call(t, "GET", "/api/commands?mode=pty&active=1", nil, &list)
	if len(list.Commands) != 1 || list.Commands[0].Session != first.Session {
		t.Fatalf("shell disappeared after socket closed: %+v", list.Commands)
	}

	// Explicit cancel ends the session.
	if st := e.call(t, "POST", "/api/commands/"+first.Session+"/cancel", map[string]any{}, nil); st != http.StatusOK {
		t.Fatalf("cancel = %d", st)
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		e.call(t, "GET", "/api/commands?mode=pty&active=1", nil, &list)
		if len(list.Commands) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("shell still open after explicit cancel: %+v", list.Commands)
}

func TestWSShellRefusesMonitorAndOffline(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct{ machine, want string }{{"mon1", "monitor"}, {"w2", "not online"}, {"nope", "unknown"}} {
		c, ctx := dial(t, e.wsURL("/ws/shell?machine="+tc.machine))
		var m shellOut
		if err := readJSON(ctx, c, &m); err != nil || m.Type != "error" || !strings.Contains(m.Error, tc.want) {
			t.Errorf("%s: frame = %+v err %v, want an error mentioning %q", tc.machine, m, err, tc.want)
		}
	}
}

func TestWSRejectsForeignOrigin(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, _, err := websocket.Dial(ctx, e.wsURL("/ws/events"), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if err == nil {
		t.Error("a cross-origin WebSocket was accepted")
	}
}

func TestLineTracker(t *testing.T) {
	var lt lineTracker
	lt.feed([]byte("ls -l\r"))
	if lt.last() != "ls -l" {
		t.Errorf("last = %q", lt.last())
	}
	lt.feed([]byte("echo helx\x7flo\r"))
	if lt.last() != "echo hello" {
		t.Errorf("backspace: last = %q", lt.last())
	}
	lt.feed([]byte("junk\x15rm x\r"))
	if lt.last() != "rm x" {
		t.Errorf("ctrl-u: last = %q", lt.last())
	}
	lt.feed([]byte("é\r"))
	if lt.last() != "é" {
		t.Errorf("utf-8: last = %q", lt.last())
	}
}

func TestOperatorFrom(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if got := operatorFrom(r); got != "operator" {
		t.Errorf("no header = %q", got)
	}
	r.Header.Set("X-Forwarded-User", "bob")
	if got := operatorFrom(r); got != "bob" {
		t.Errorf("X-Forwarded-User = %q", got)
	}
	r.Header.Set("Remote-User", "alice")
	if got := operatorFrom(r); got != "alice" {
		t.Errorf("Remote-User wins = %q", got)
	}
}

// Shutdown must not hang on open WebSockets (they are hijacked connections that
// http.Server.Shutdown does not track).
func TestServeShutsDownWithOpenWebSockets(t *testing.T) {
	e := newEnv(t)
	h, err := New(Options{Assets: web.FS, Templates: web.Templates, Backend: e.m})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, h, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	url := "ws://" + ln.Addr().String()
	events, evCtx := dial(t, url+"/ws/events")
	var hello map[string]string
	_ = readJSON(evCtx, events, &hello)
	shell, shCtx := dial(t, url+"/ws/shell?machine=w1")
	waitShellRunning(t, shCtx, shell)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return with WebSockets open")
	}
}
