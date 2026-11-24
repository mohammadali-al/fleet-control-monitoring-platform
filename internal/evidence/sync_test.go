package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

const (
	testAgent = "agent-01"
	testMain  = "main-server"
	testWait  = 10 * time.Second
)

var loopback = net.ParseIP("127.0.0.1")

// ---- synthetic evidence ----

// synthRecord builds a real §6.3 record from a synthetic TCP packet.
func synthRecord(t *testing.T, session string, ts time.Time, i, payloadLen int) capture.Record {
	t.Helper()
	ip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP,
		SrcIP: net.IPv4(10, 0, 0, 5), DstIP: net.IPv4(192, 0, 2, byte(1+i%200))}
	tcp := &layers.TCP{SrcPort: layers.TCPPort(40000 + i%1000), DstPort: 443, Seq: uint32(1000 + i), Ack: uint32(i),
		Window: 65535, PSH: true, ACK: true, DataOffset: 5}
	if err := tcp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, payloadLen)
	for k := range payload {
		payload[k] = byte(i + k)
	}
	buf := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true},
		ip, tcp, gopacket.Payload(payload)); err != nil {
		t.Fatal(err)
	}
	rec, err := capture.BuildRecord(session, ts, buf.Bytes(), "10.0.0.5", 64)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// writeSegment writes a finalized segment (NDJSON) into an agent evidence dir
// and indexes it in the manifest, as the T-0008 writer would.
func writeSegment(t *testing.T, dir, machine, name string, recs []capture.Record) capture.SegmentEntry {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	var data []byte
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, b...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o640); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	e := capture.SegmentEntry{
		File: name, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)), Records: len(recs),
		StartTime: recs[0].Timestamp, EndTime: recs[len(recs)-1].Timestamp,
		SchemaVersion: capture.SchemaVersion, Compression: capture.CompressionNone,
	}
	mf, err := capture.LoadManifest(dir, machine)
	if err != nil {
		t.Fatal(err)
	}
	if err := mf.Add(e); err != nil {
		t.Fatal(err)
	}
	return e
}

// synthSegment writes a segment of n records for one session starting at t0.
func synthSegment(t *testing.T, dir, machine, name, session string, t0 time.Time, n, payloadLen int) capture.SegmentEntry {
	t.Helper()
	recs := make([]capture.Record, n)
	for i := range recs {
		recs[i] = synthRecord(t, session, t0.Add(time.Duration(i)*time.Millisecond), i, payloadLen)
	}
	return writeSegment(t, dir, machine, name, recs)
}

// rewriteManifest replaces the agent manifest's entries (to model a changed segment).
func rewriteManifest(t *testing.T, dir, machine string, entries []capture.SegmentEntry) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"schema_version": capture.SchemaVersion, "machine": machine, "segments": entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, capture.ManifestName), b, 0o640); err != nil {
		t.Fatal(err)
	}
}

// replaceSegment rewrites an existing segment with new records and swaps its
// manifest entry (a segment whose digest changed since the server last saw it).
func replaceSegment(t *testing.T, dir, machine, name string, recs []capture.Record) capture.SegmentEntry {
	t.Helper()
	mf, err := capture.LoadManifest(dir, machine)
	if err != nil {
		t.Fatal(err)
	}
	old := mf.Entries()
	tmp := t.TempDir()
	e := writeSegment(t, tmp, machine, name, recs)
	data, err := os.ReadFile(filepath.Join(tmp, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o640); err != nil {
		t.Fatal(err)
	}
	for i := range old {
		if old[i].File == name {
			old[i] = e
		}
	}
	rewriteManifest(t, dir, machine, old)
	return e
}

// ---- loopback rig: both ends in-process over the real mTLS channel ----

type lateSender struct {
	mu sync.Mutex
	s  Sender
}

func (l *lateSender) set(s Sender) { l.mu.Lock(); l.s = s; l.mu.Unlock() }
func (l *lateSender) Send(ctx context.Context, env channel.Envelope) error {
	l.mu.Lock()
	s := l.s
	l.mu.Unlock()
	if s == nil {
		return channel.ErrNotConnected
	}
	return s.Send(ctx, env)
}

// cutSender drops the agent's connection just before sending the Nth ev-chunk
// (counting from 1), simulating an interrupted transfer.
type cutSender struct {
	inner  Sender
	cutAt  int32 // 0 = never
	chunks atomic.Int32
	sent   map[string]int // envelopes by type sent through it
	mu     sync.Mutex
	cut    func()
}

func (c *cutSender) Send(ctx context.Context, env channel.Envelope) error {
	c.mu.Lock()
	if c.sent == nil {
		c.sent = map[string]int{}
	}
	c.sent[env.Type]++
	c.mu.Unlock()
	if env.Type == channel.TypeEvChunk && c.cutAt > 0 && c.chunks.Add(1) == c.cutAt {
		c.cut()
		return errors.New("test: connection cut")
	}
	return c.inner.Send(ctx, env)
}

func (c *cutSender) count(typ string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent[typ]
}

type rig struct {
	t        *testing.T
	ca       *pki.CA
	evDir    string // agent: DataDir/evidence/agent-01
	archive  *Archive
	syncer   *Syncer
	dialer   *channel.Dialer
	toAgent  lateSender
	dialConn chan string
	pushes   chan pushOutcome
	addr     string
	disk     atomic.Value // float64, drives the archive disk guard
}

type pushOutcome struct {
	res Result
	err error
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ca, err := pki.NewCA(pki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, ca: ca, evDir: filepath.Join(t.TempDir(), "evidence", testAgent),
		dialConn: make(chan string, 16), pushes: make(chan pushOutcome, 16)}
	r.disk.Store(10.0)
	arch, err := OpenArchive(t.TempDir(), ArchiveOptions{DiskUsage: func(string) (float64, error) {
		return r.disk.Load().(float64), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.archive = arch
	r.syncer, err = NewSyncer(SyncerConfig{Machine: testAgent, Archive: arch, Sender: &r.toAgent, Timeout: 700 * time.Millisecond,
		OnPush: func(res Result, err error) { r.pushes <- pushOutcome{res, err} }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) issue(id string) (cert, key []byte) {
	r.t.Helper()
	c, k, err := r.ca.IssuePeer(id, []net.IP{loopback}, time.Hour)
	if err != nil {
		r.t.Fatal(err)
	}
	return c, k
}

type agentOpts struct {
	chunk int
	cutAt int32
	idle  func() bool
}

type runningAgent struct {
	*Agent
	sender    *cutSender
	listener  *channel.Listener
	connected chan string
	stop      func()
}

// startAgent starts an agent listener (addr "127.0.0.1:0" the first time, the
// same address to model an agent restart) and, the first time, the server dialer.
func (r *rig) startAgent(addr string, o agentOpts) *runningAgent {
	r.t.Helper()
	cert, key := r.issue(testAgent)
	tlsCfg, err := pki.ServerTLSConfig(cert, key, r.ca.CertPEM())
	if err != nil {
		r.t.Fatal(err)
	}
	ra := &runningAgent{connected: make(chan string, 4)}
	var late lateSender
	ra.sender = &cutSender{inner: &late, cutAt: o.cutAt}
	ra.Agent, err = NewAgent(AgentConfig{Machine: testAgent, Dir: r.evDir, Sender: ra.sender, ChunkSize: o.chunk, Idle: o.idle})
	if err != nil {
		r.t.Fatal(err)
	}
	l, err := channel.Listen(channel.ListenerConfig{
		Addr: addr, TLS: tlsCfg, MachineID: testAgent,
		Handler:   func(ctx context.Context, env channel.Envelope) { ra.Agent.Handle(ctx, env) },
		OnConnect: func(id string) { ra.connected <- id },
	})
	if err != nil {
		r.t.Fatal(err)
	}
	ra.listener = l
	late.set(l)
	ra.sender.cut = func() { _ = l.Close() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = l.Serve(ctx) }()
	ra.stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testWait):
			r.t.Error("agent Serve did not stop")
		}
	}
	r.t.Cleanup(ra.stop)

	if r.dialer == nil {
		r.addr = l.Addr().String()
		dcert, dkey := r.issue(testMain)
		dtls, err := pki.DialTLSConfig(dcert, dkey, r.ca.CertPEM(), loopback)
		if err != nil {
			r.t.Fatal(err)
		}
		r.dialer, err = channel.NewDialer(channel.DialerConfig{
			Addr: r.addr, TLS: dtls, MachineID: testMain,
			Handler:    func(ctx context.Context, env channel.Envelope) { r.syncer.Handle(ctx, env) },
			OnConnect:  func(id string) { r.dialConn <- id },
			MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
		})
		if err != nil {
			r.t.Fatal(err)
		}
		r.toAgent.set(r.dialer)
		dctx, dcancel := context.WithCancel(context.Background())
		ddone := make(chan struct{})
		go func() { defer close(ddone); _ = r.dialer.Run(dctx) }()
		r.t.Cleanup(func() {
			dcancel()
			select {
			case <-ddone:
			case <-time.After(testWait):
				r.t.Error("dialer Run did not stop")
			}
		})
	}
	// Both ends see the handshake before the test proceeds.
	waitFor(r.t, ra.connected, "agent handshake")
	waitFor(r.t, r.dialConn, "dialer handshake")
	return ra
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func (r *rig) pull() (Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return r.syncer.Pull(ctx)
}

func (r *rig) mustPull() Result {
	r.t.Helper()
	res, err := r.pull()
	if err != nil {
		r.t.Fatalf("Pull: %v", err)
	}
	return res
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	m := map[string]bool{}
	for _, g := range got {
		m[g] = true
	}
	for _, w := range want {
		if !m[w] {
			return false
		}
	}
	return true
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

var t0 = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

// ---- tests ----

func TestPullTransfersOnlyMissingAndChanged(t *testing.T) {
	r := newRig(t)
	e1 := synthSegment(t, r.evDir, testAgent, "seg-1.ndjson", "sess-A", t0, 20, 300)
	e2 := synthSegment(t, r.evDir, testAgent, "seg-2.ndjson", "sess-A", t0.Add(time.Minute), 20, 300)
	e3 := synthSegment(t, r.evDir, testAgent, "seg-3.ndjson", "sess-B", t0.Add(2*time.Minute), 20, 300)
	ag := r.startAgent("127.0.0.1:0", agentOpts{chunk: 1024})

	res := r.mustPull()
	if !sameSet(res.Fetched, "seg-1.ndjson", "seg-2.ndjson", "seg-3.ndjson") || res.Skipped != 0 || len(res.Rejected) != 0 {
		t.Fatalf("first pull: %+v", res)
	}
	if want := e1.Bytes + e2.Bytes + e3.Bytes; res.Bytes != want || ag.BytesServed() != want {
		t.Fatalf("first pull bytes: result %d, agent served %d, want %d", res.Bytes, ag.BytesServed(), want)
	}
	// Every archived file matches the agent's SHA-256, per machine/session.
	for _, e := range []capture.SegmentEntry{e1, e2} {
		p := filepath.Join(r.archive.Root(), testAgent, "sess-A", e.File)
		if got := fileSHA(t, p); got != e.SHA256 {
			t.Errorf("%s: archived sha %s != %s", e.File, got, e.SHA256)
		}
	}
	if got := fileSHA(t, filepath.Join(r.archive.Root(), testAgent, "sess-B", e3.File)); got != e3.SHA256 {
		t.Errorf("seg-3 archived sha mismatch")
	}

	// Nothing new: nothing moves.
	servedBefore := ag.BytesServed()
	res = r.mustPull()
	if len(res.Fetched) != 0 || res.Skipped != 3 || res.Bytes != 0 || ag.BytesServed() != servedBefore {
		t.Fatalf("idle pull should transfer nothing: %+v (served %d -> %d)", res, servedBefore, ag.BytesServed())
	}

	// Two new segments and one changed segment: exactly those three move.
	e4 := synthSegment(t, r.evDir, testAgent, "seg-4.ndjson", "sess-B", t0.Add(3*time.Minute), 10, 300)
	e5 := synthSegment(t, r.evDir, testAgent, "seg-5.ndjson", "sess-B", t0.Add(4*time.Minute), 10, 300)
	recs := make([]capture.Record, 7)
	for i := range recs {
		recs[i] = synthRecord(t, "sess-A", t0.Add(time.Minute+time.Duration(i)*time.Millisecond), i, 300)
	}
	changed := replaceSegment(t, r.evDir, testAgent, "seg-2.ndjson", recs)

	servedBefore = ag.BytesServed()
	res = r.mustPull()
	if !sameSet(res.Fetched, "seg-2.ndjson", "seg-4.ndjson", "seg-5.ndjson") || res.Skipped != 2 {
		t.Fatalf("delta pull: %+v", res)
	}
	if want := changed.Bytes + e4.Bytes + e5.Bytes; ag.BytesServed()-servedBefore != want {
		t.Fatalf("delta pull served %d bytes, want %d", ag.BytesServed()-servedBefore, want)
	}
	if got := fileSHA(t, filepath.Join(r.archive.Root(), testAgent, "sess-A", "seg-2.ndjson")); got != changed.SHA256 {
		t.Errorf("changed segment not replaced in archive")
	}
	segs, _ := r.archive.Segments(testAgent)
	if len(segs) != 5 {
		t.Errorf("archive manifest has %d segments, want 5", len(segs))
	}
}
