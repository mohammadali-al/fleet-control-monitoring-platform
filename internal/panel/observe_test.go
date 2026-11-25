package panel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/backup"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/evidence"
	exe "github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

// ---- a fleet whose agents are in-process evidence agents ----

// obsLink is the manager's link to a fake agent: envelopes the manager sends
// are handed to an in-process evidence.Agent (when the machine has one), and
// whatever the agent sends goes back through the manager's handler, as the
// channel would deliver it.
type obsLink struct {
	h  channel.Handler
	ag *evidence.Agent

	mu   sync.Mutex
	up   bool
	sent []channel.Envelope
}

func (l *obsLink) Send(ctx context.Context, env channel.Envelope) error {
	l.mu.Lock()
	up := l.up
	l.sent = append(l.sent, env)
	l.mu.Unlock()
	if !up {
		return channel.ErrNotConnected
	}
	if l.ag != nil {
		l.ag.Handle(ctx, env)
	}
	return nil
}
func (l *obsLink) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (l *obsLink) Online() bool                  { l.mu.Lock(); defer l.mu.Unlock(); return l.up }
func (l *obsLink) setUp(up bool)                 { l.mu.Lock(); l.up = up; l.mu.Unlock() }
func (l *obsLink) sentOfType(typ string) []channel.Envelope {
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

type obsSender struct{ l *obsLink }

func (s obsSender) Send(ctx context.Context, env channel.Envelope) error {
	s.l.h(ctx, env)
	return nil
}

type obsEnv struct {
	t        *testing.T
	srv      *httptest.Server
	m        *fleet.Manager
	reg      *registry.Registry
	obs      *fleet.Observe
	dataDir  string
	agentDir string // per-machine evidence dirs live under it
	mu       sync.Mutex
	links    map[string]*obsLink
	logs     *bytes.Buffer
}

// newObsEnv serves the panel over a manager holding workers w1 (online), w2
// (offline) and a monitor mon1, with the Observe backends bound under a temp
// data dir. mod may adjust the Observe before it is bound.
func newObsEnv(t *testing.T, mod func(*obsEnv, *fleet.Observe)) *obsEnv {
	t.Helper()
	e := &obsEnv{t: t, dataDir: t.TempDir(), agentDir: t.TempDir(), links: map[string]*obsLink{}, logs: &bytes.Buffer{}}
	e.reg = registry.New()
	for _, mc := range []registry.Machine{
		{ID: "w1", Role: registry.RoleWorker, PrivateIPs: []string{"10.0.0.1"}, Provisioned: true},
		{ID: "w2", Role: registry.RoleWorker, PrivateIPs: []string{"10.0.0.2"}, Provisioned: true},
		{ID: "mon1", Role: registry.RoleMonitor, PrivateIPs: []string{"10.0.0.3"}, Provisioned: true},
	} {
		if err := e.reg.Add(mc); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := exe.NewGate([]byte("test-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(e.logs, nil))
	m, err := fleet.New(fleet.Config{
		Registry: e.reg, Gate: gate, PollInterval: 10 * time.Millisecond, Logger: log,
		NewLink: func(mc registry.Machine, h channel.Handler) (fleet.Link, error) {
			l := &obsLink{h: h, up: mc.ID != "w2"}
			if mc.Role == registry.RoleWorker {
				ag, err := evidence.NewAgent(evidence.AgentConfig{
					Machine: mc.ID, Dir: filepath.Join(e.agentDir, mc.ID), Sender: obsSender{l},
					Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				})
				if err != nil {
					return nil, err
				}
				l.ag = ag
			}
			e.mu.Lock()
			e.links[mc.ID] = l
			e.mu.Unlock()
			return l, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	o, err := fleet.OpenObserve(e.dataDir, log)
	if err != nil {
		t.Fatal(err)
	}
	e.obs = o
	if mod != nil {
		mod(e, o)
	}
	m.SetObserve(o)
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	h, err := New(Options{Assets: web.FS, Templates: web.Templates, Backend: m, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(h)
	t.Cleanup(func() { e.srv.Close(); cancel(); m.Close() })
	e.waitState("w1", fleet.StateOnline)
	return e
}

func (e *obsEnv) waitState(id, state string) {
	e.t.Helper()
	eventually(e.t, id+" "+state, func() bool {
		info, ok := e.m.Machine(id)
		return ok && info.State == state
	})
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

func (e *obsEnv) req(method, path string, in any, out any) (int, http.Header) {
	e.t.Helper()
	var rd io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("%s %s: bad JSON %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode, resp.Header
}

func (e *obsEnv) raw(path string) (int, http.Header, []byte) {
	e.t.Helper()
	resp, err := http.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func (e *obsEnv) link(id string) *obsLink {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.links[id]
}

// ---- synthetic evidence ----

func synthRecord(t *testing.T, session string, ts time.Time, i int) capture.Record {
	t.Helper()
	ip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP,
		SrcIP: net.IPv4(10, 0, 0, 5), DstIP: net.IPv4(192, 0, 2, byte(1+i))}
	tcp := &layers.TCP{SrcPort: layers.TCPPort(40000 + i), DstPort: 443, Seq: uint32(1000 + i), Ack: uint32(i), Window: 65535, PSH: true, ACK: true, DataOffset: 5}
	if err := tcp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatal(err)
	}
	buf := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true},
		ip, tcp, gopacket.Payload([]byte("hello evidence"))); err != nil {
		t.Fatal(err)
	}
	rec, err := capture.BuildRecord(session, ts, buf.Bytes(), "10.0.0.5", 64)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// writeAgentSegment writes a finalized segment of n records of one session
// into the fake agent's evidence dir and indexes it in the agent manifest.
func (e *obsEnv) writeAgentSegment(machine, name, session string, t0 time.Time, n int) {
	e.t.Helper()
	dir := filepath.Join(e.agentDir, machine)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		e.t.Fatal(err)
	}
	var data []byte
	var first, last time.Time
	for i := 0; i < n; i++ {
		ts := t0.Add(time.Duration(i) * time.Millisecond)
		if i == 0 {
			first = ts
		}
		last = ts
		b, err := json.Marshal(synthRecord(e.t, session, ts, i))
		if err != nil {
			e.t.Fatal(err)
		}
		data = append(append(data, b...), '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o640); err != nil {
		e.t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	mf, err := capture.LoadManifest(dir, machine)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := mf.Add(capture.SegmentEntry{File: name, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)), Records: n,
		StartTime: first, EndTime: last, SchemaVersion: capture.SchemaVersion, Compression: capture.CompressionNone}); err != nil {
		e.t.Fatal(err)
	}
}

// ---- Evidence ----

func TestEvidenceLive(t *testing.T) {
	e := newObsEnv(t, nil)
	t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	e.writeAgentSegment("w1", "seg-0001.ndjson", "s-100", t0, 3)
	e.writeAgentSegment("w1", "seg-0002.ndjson", "s-200", t0.Add(time.Minute), 2)

	// Nothing is archived before a pull.
	var sess struct {
		Sessions []evSession `json:"sessions"`
	}
	if code, _ := e.req("GET", "/api/evidence/sessions", nil, &sess); code != 200 || len(sess.Sessions) != 0 {
		t.Fatalf("sessions before pull = %d %+v", code, sess)
	}

	// Pull: 202, then the sync status settles on "done" with both segments fetched.
	var pull struct {
		Results []pullResult `json:"results"`
	}
	if code, _ := e.req("POST", "/api/evidence/pull", map[string]string{"machine": "w1"}, &pull); code != 202 || len(pull.Results) != 1 || !pull.Results[0].Started {
		t.Fatalf("pull = %d %+v", code, pull)
	}
	var syncResp struct {
		Workers []evSyncRow    `json:"workers"`
		Disk    map[string]any `json:"disk"`
	}
	eventually(t, "pull to finish", func() bool {
		e.req("GET", "/api/evidence/sync", nil, &syncResp)
		for _, w := range syncResp.Workers {
			if w.Machine == "w1" && w.Sync.State == "done" {
				return true
			}
		}
		return false
	})
	if len(syncResp.Workers) != 2 {
		t.Fatalf("workers = %+v, want w1 and w2", syncResp.Workers)
	}
	for _, w := range syncResp.Workers {
		switch w.Machine {
		case "w1":
			if w.Sync.Fetched != 2 || w.Sync.Segments != 2 || w.Sync.Rejected != 0 || w.Archived != 2 || w.Bytes == 0 {
				t.Errorf("w1 sync = %+v archived=%d", w.Sync, w.Archived)
			}
		case "w2":
			if w.State != fleet.StateOffline || w.Sync.State != "idle" {
				t.Errorf("w2 = %+v", w)
			}
		}
	}
	if syncResp.Disk["state"] == nil || syncResp.Disk["warn_at"] == nil {
		t.Errorf("disk = %v", syncResp.Disk)
	}

	// A second pull has nothing to fetch.
	e.req("POST", "/api/evidence/pull", map[string]string{"machine": "w1"}, nil)
	eventually(t, "second pull", func() bool {
		e.req("GET", "/api/evidence/sync", nil, &syncResp)
		for _, w := range syncResp.Workers {
			if w.Machine == "w1" && w.Sync.State == "done" && w.Sync.Fetched == 0 && w.Sync.Skipped == 2 {
				return true
			}
		}
		return false
	})

	// The session browser lists what was archived, newest first, with filters.
	e.req("GET", "/api/evidence/sessions", nil, &sess)
	if len(sess.Sessions) != 2 || sess.Sessions[0].Session != "s-200" || sess.Sessions[1].Session != "s-100" {
		t.Fatalf("sessions = %+v", sess.Sessions)
	}
	if s := sess.Sessions[1]; s.Machine != "w1" || s.Packets != 3 || s.Segments != 1 || s.Shared || !s.Online {
		t.Errorf("s-100 = %+v", s)
	}
	e.req("GET", "/api/evidence/sessions?q=s-1", nil, &sess)
	if len(sess.Sessions) != 1 || sess.Sessions[0].Session != "s-100" {
		t.Errorf("filtered = %+v", sess.Sessions)
	}
	e.req("GET", "/api/evidence/sessions?machine=w2", nil, &sess)
	if len(sess.Sessions) != 0 {
		t.Errorf("machine filter = %+v", sess.Sessions)
	}

	// Packet records of one session.
	var recs struct {
		Records  []evRecord `json:"records"`
		More     bool       `json:"more"`
		Segments int        `json:"segments"`
	}
	if code, _ := e.req("GET", "/api/evidence/sessions/w1/s-100/records", nil, &recs); code != 200 || len(recs.Records) != 3 || recs.More || recs.Segments != 1 {
		t.Fatalf("records = %d %+v", code, recs)
	}
	if r := recs.Records[0]; r.Protocol != "TCP" || !strings.HasSuffix(r.Dst, ":443") || r.PayloadSHA256 == "" || r.SrcIPUsed != "10.0.0.5" {
		t.Errorf("record = %+v", r)
	}
	e.req("GET", "/api/evidence/sessions/w1/s-100/records?limit=2", nil, &recs)
	if len(recs.Records) != 2 || !recs.More {
		t.Errorf("limited records = %d more=%v", len(recs.Records), recs.More)
	}
	if code, _ := e.req("GET", "/api/evidence/sessions/w1/s-nope/records", nil, nil); code != 404 {
		t.Errorf("unknown session = %d, want 404", code)
	}

	// Manifest.
	var man struct {
		Segments []map[string]any `json:"segments"`
	}
	if code, _ := e.req("GET", "/api/evidence/manifest/w1", nil, &man); code != 200 || len(man.Segments) != 2 || man.Segments[0]["sha256"] == "" {
		t.Errorf("manifest = %d %+v", code, man)
	}

	// Exports: pcapng, CSV and JSON of one session, with download headers.
	code, hdr, b := e.raw("/api/evidence/export?format=pcapng&machine=w1&session=s-100")
	if code != 200 || !bytes.HasPrefix(b, []byte{0x0a, 0x0d, 0x0d, 0x0a}) || !strings.Contains(hdr.Get("Content-Disposition"), "evidence-w1-s-100.pcapng") {
		t.Errorf("pcapng export = %d %q %d bytes", code, hdr.Get("Content-Disposition"), len(b))
	}
	code, _, b = e.raw("/api/evidence/export?format=csv&machine=w1&session=s-100")
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); code != 200 || len(lines) != 4 || !strings.HasPrefix(lines[0], "machine,session_id") {
		t.Errorf("csv export = %d %d lines: %q", code, len(lines), b)
	}
	code, _, b = e.raw("/api/evidence/export?format=json&session=s-200")
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); code != 200 || err != nil || len(rows) != 2 || rows[0]["machine"] != "w1" {
		t.Errorf("json export = %d %v rows=%d", code, err, len(rows))
	}
	code, _, b = e.raw("/api/evidence/export?format=json&from=" + t0.Add(30*time.Second).Format(time.RFC3339))
	if err := json.Unmarshal(b, &rows); code != 200 || err != nil || len(rows) != 2 {
		t.Errorf("time-range export = %d %v rows=%d", code, err, len(rows)) // only the second segment is after t0+30s
	}
	if code, _, _ := e.raw("/api/evidence/export?format=xml"); code != 400 {
		t.Errorf("bad format = %d, want 400", code)
	}

	// Errors: offline agent, unknown machine, bad JSON.
	var perr struct{ Results []pullResult }
	if code, _ := e.req("POST", "/api/evidence/pull", map[string]string{"machine": "w2"}, &perr); code != 409 {
		t.Errorf("pull offline = %d, want 409", code)
	}
	if code, _ := e.req("POST", "/api/evidence/pull", map[string]string{"machine": "nope"}, nil); code != 404 {
		t.Errorf("pull unknown = %d, want 404", code)
	}
	if code, _ := e.req("POST", "/api/evidence/pull", map[string]string{"machine": "mon1"}, nil); code != 422 {
		t.Errorf("pull monitor = %d, want 422", code)
	}

	// "Pull from all" reaches every online worker.
	if code, _ := e.req("POST", "/api/evidence/pull", map[string]string{}, &pull); code != 202 || len(pull.Results) != 1 || pull.Results[0].Machine != "w1" {
		t.Errorf("pull all = %d %+v", code, pull)
	}

	// The page itself: live, no Mock chip, wired hooks present, workers rendered.
	_, _, html := e.raw("/evidence.html")
	for _, want := range []string{`id="ev-sessions"`, `id="ev-sync-rows"`, `id="ev-export-form"`, `<option value="w1">w1</option>`, "assets/js/live.js"} {
		if !strings.Contains(string(html), want) {
			t.Errorf("evidence page missing %q", want)
		}
	}
	if strings.Contains(string(html), "Mock data</span>") || strings.Contains(string(html), "wrk-0") {
		t.Error("evidence page still shows mock data")
	}
}

// An idle agent's push offer is routed to the syncer and archived, and shows
// as a push in the sync status.
func TestEvidenceIdlePush(t *testing.T) {
	e := newObsEnv(t, nil)
	e.writeAgentSegment("w1", "seg-0001.ndjson", "s-9", time.Now().Add(-time.Minute).UTC(), 2)
	// Offer the manifest the way an idle agent would (an unsolicited ev-manifest).
	mf, err := capture.LoadManifest(filepath.Join(e.agentDir, "w1"), "w1")
	if err != nil {
		t.Fatal(err)
	}
	env, err := channel.NewEnvelope(channel.TypeEvManifest, "push-1", evidence.ManifestMsg{Machine: "w1", Segments: mf.Entries()})
	if err != nil {
		t.Fatal(err)
	}
	// Deliver it through the manager's handler for w1.
	e.link("w1").h(context.Background(), env)
	eventually(t, "push to archive", func() bool {
		var syncResp struct {
			Workers []evSyncRow `json:"workers"`
		}
		e.req("GET", "/api/evidence/sync", nil, &syncResp)
		for _, w := range syncResp.Workers {
			if w.Machine == "w1" && w.Sync.State == "done" && w.Sync.Trigger == "push" && w.Sync.Fetched == 1 {
				return true
			}
		}
		return false
	})
}

// ---- Backup ----

// localRemote is a backup server that is a directory on this machine: every
// command runs in a POSIX shell in a temp "home".
type localRemote struct{ home string }

func (r localRemote) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = r.home
	c.Env = append(os.Environ(), "HOME="+r.home)
	c.Stdin = stdin
	c.Stdout = stdout
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &backup.RemoteError{Code: ee.ExitCode(), Stderr: stderr.String()}
		}
		return err
	}
	return nil
}
func (localRemote) Close() error { return nil }

func localBackupDial(home string) backup.DialFunc {
	return func(_ context.Context, srv backup.Server, _ backup.Credential) (backup.Remote, string, error) {
		if srv.Host == "unreachable.invalid" {
			return nil, "", errors.New("dial: no route to host")
		}
		return localRemote{home}, "SHA256:testfingerprint", nil
	}
}

func TestBackupLive(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum not available")
	}
	home := t.TempDir()
	e := newObsEnv(t, func(e *obsEnv, o *fleet.Observe) {
		bk, err := backup.Open(e.dataDir, backup.Options{Dial: localBackupDial(home), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		o.Backup = bk
		// Something to back up: an archive file and registry metadata.
		for p, content := range map[string]string{"archive/w1/seg-1.ndjson": "aaa\n", "monitor/jobs.json": "{}\n", "registry/machines.json": "[]\n"} {
			full := filepath.Join(e.dataDir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o640); err != nil {
				t.Fatal(err)
			}
		}
	})

	var st map[string]any
	if code, _ := e.req("GET", "/api/backup", nil, &st); code != 200 || st["configured"] != false {
		t.Fatalf("status = %d %v", code, st)
	}
	// No server yet: the once-a-day reminder says so; dismissing hides it.
	rem, _ := st["reminder"].(map[string]any)
	if rem["kind"] != backup.ReminderNoServer {
		t.Errorf("reminder = %v", st["reminder"])
	}
	e.req("GET", "/api/backup", nil, &st)
	if st["reminder"] == nil {
		t.Error("the reminder vanished on the second request: it must persist until dismissed")
	}
	if code, _ := e.req("POST", "/api/backup/reminder/dismiss", map[string]string{}, nil); code != 200 {
		t.Errorf("dismiss = %d", code)
	}
	st = nil // decoding into a non-nil map would keep the old keys
	e.req("GET", "/api/backup", nil, &st)
	if st["reminder"] != nil {
		t.Errorf("reminder after dismiss = %v", st["reminder"])
	}
	sg, _ := st["suggested"].(map[string]any)
	if sg["hour"].(float64) != 2 || sg["minute"].(float64) != 30 || sg["enabled"] != false {
		t.Errorf("suggested = %v", sg)
	}

	// Nothing to do without a server.
	if code, _ := e.req("POST", "/api/backup/run", map[string]string{}, nil); code != 409 {
		t.Errorf("run without server = %d, want 409", code)
	}
	if code, _ := e.req("GET", "/api/backup/diff", nil, nil); code != 409 {
		t.Errorf("diff without server = %d, want 409", code)
	}

	// Add a server: validation, an unreachable host, then success with a diff.
	if code, _ := e.req("POST", "/api/backup/server", map[string]any{"host": "127.0.0.1", "user": "backup"}, nil); code != 400 {
		t.Errorf("add without password = %d, want 400", code)
	}
	var bad errorBody
	if code, _ := e.req("POST", "/api/backup/server", map[string]any{"host": "unreachable.invalid", "user": "backup", "password": "pw-secret"}, &bad); code != 502 || !strings.Contains(bad.Error, "no route") {
		t.Errorf("add unreachable = %d %q", code, bad.Error)
	}
	var add struct {
		Server map[string]any `json:"server"`
		Diff   map[string]any `json:"diff"`
	}
	if code, _ := e.req("POST", "/api/backup/server", map[string]any{"host": "127.0.0.1", "user": "backup", "password": "pw-secret"}, &add); code != 201 {
		t.Fatalf("add server = %d", code)
	}
	if add.Server["host"] != "127.0.0.1" || add.Server["remote_path"] != backup.RemoteDataDir || add.Server["auth"] == nil {
		t.Errorf("server = %v", add.Server)
	}
	if add.Diff == nil || add.Diff["in_sync"] != false || add.Diff["missing"].(float64) < 3 {
		t.Errorf("diff after add = %v", add.Diff)
	}
	if strings.Contains(e.logs.String(), "pw-secret") {
		t.Error("the backup password reached the log")
	}
	groups := add.Diff["groups"].([]any)
	if len(groups) != 5 || groups[0].(map[string]any)["dir"] != "archive" || groups[0].(map[string]any)["missing"].(float64) != 1 {
		t.Errorf("diff groups = %v", groups)
	}

	// Back up now: runs in the background and the diff goes to in-sync.
	if code, _ := e.req("POST", "/api/backup/run", map[string]string{}, nil); code != 202 {
		t.Fatalf("run = %d", code)
	}
	eventually(t, "backup job to finish", func() bool {
		e.req("GET", "/api/backup", nil, &st)
		j, _ := st["job"].(map[string]any)
		return j != nil && j["state"] != "running"
	})
	job := st["job"].(map[string]any)
	if job["state"] != "done" || job["uploaded"].(float64) < 3 || job["kind"] != "backup" {
		t.Fatalf("job = %v", job)
	}
	last, _ := st["last_backup"].(map[string]any)
	if last == nil || last["ok"] != true {
		t.Errorf("last_backup = %v", st["last_backup"])
	}
	var diff map[string]any
	if code, _ := e.req("GET", "/api/backup/diff", nil, &diff); code != 200 || diff["in_sync"] != true || diff["reachable"] != true {
		t.Errorf("diff after backup = %d %v", code, diff)
	}
	if _, err := os.Stat(filepath.Join(home, backup.RemoteDataDir, "archive", "w1", "seg-1.ndjson")); err != nil {
		t.Errorf("the file is not on the backup server: %v", err)
	}

	// Schedule.
	var sch struct{ Schedule bkSchedule }
	if code, _ := e.req("POST", "/api/backup/schedule", map[string]any{"enabled": true, "hour": 3, "minute": 5}, &sch); code != 200 || !sch.Schedule.Enabled || sch.Schedule.Text != "daily at 03:05" || sch.Schedule.Next == "" {
		t.Errorf("schedule = %d %+v", code, sch)
	}
	if code, _ := e.req("POST", "/api/backup/schedule", map[string]any{"enabled": true, "hour": 25, "minute": 0}, nil); code != 400 {
		t.Errorf("bad schedule = %d, want 400", code)
	}
	e.req("GET", "/api/backup", nil, &st)
	if s := st["schedule"].(map[string]any); s["enabled"] != true || s["hour"].(float64) != 3 {
		t.Errorf("stored schedule = %v", s)
	}

	// Import: needs confirmation; with it, the data is already local so it is all "unchanged".
	if code, _ := e.req("POST", "/api/backup/import", map[string]any{"confirm": false}, nil); code != 400 {
		t.Errorf("import without confirm = %d, want 400", code)
	}
	if code, _ := e.req("POST", "/api/backup/import", map[string]any{"confirm": true}, nil); code != 202 {
		t.Fatalf("import = %d", code)
	}
	eventually(t, "import to finish", func() bool {
		e.req("GET", "/api/backup", nil, &st)
		j, _ := st["job"].(map[string]any)
		return j != nil && j["kind"] == "import" && j["state"] != "running"
	})
	if j := st["job"].(map[string]any); j["state"] != "done" || j["unchanged"].(float64) < 3 || j["failed"].(float64) != 0 {
		t.Errorf("import job = %v", j)
	}

	// The page is live.
	_, _, html := e.raw("/backup.html")
	for _, want := range []string{`id="bk-run"`, `id="bk-add-form"`, `id="bk-diff-rows"`, `id="bk-sched-form"`, "assets/js/live.js"} {
		if !strings.Contains(string(html), want) {
			t.Errorf("backup page missing %q", want)
		}
	}
	if strings.Contains(string(html), "Mock data</span>") || strings.Contains(string(html), "data-stub") {
		t.Error("backup page still shows mock data or stubs")
	}
}

// Without Observe backends the new APIs say 503 and the pages still render.
func TestObserveUnavailableWithoutBackends(t *testing.T) {
	srv := noBackend(t)
	for _, p := range []string{"/api/evidence/sessions", "/api/evidence/sync", "/api/backup", "/api/backup/diff"} {
		if code, _, _ := body(t, srv.URL+p); code != 503 {
			t.Errorf("GET %s = %d, want 503", p, code)
		}
	}
	for _, p := range []string{"evidence", "backup"} {
		if code, _, html := body(t, srv.URL+"/"+p+".html"); code != 200 || strings.Contains(html, "Mock data</span>") {
			t.Errorf("%s page = %d mock=%v", p, code, strings.Contains(html, "Mock data</span>"))
		}
	}
}
