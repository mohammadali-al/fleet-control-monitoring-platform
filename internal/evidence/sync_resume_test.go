package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
)

func TestPullResumesMidSegment(t *testing.T) {
	r := newRig(t)
	a := synthSegment(t, r.evDir, testAgent, "seg-1.ndjson", "sess-A", t0, 3, 300)
	b := synthSegment(t, r.evDir, testAgent, "seg-2.ndjson", "sess-A", t0.Add(time.Minute), 40, 300)
	c := synthSegment(t, r.evDir, testAgent, "seg-3.ndjson", "sess-A", t0.Add(2*time.Minute), 3, 300)
	const chunk = 1024
	// Cut just before the 6th chunk: seg-1 is whole, seg-2 is part-way.
	ag1 := r.startAgent("127.0.0.1:0", agentOpts{chunk: chunk, cutAt: 6})

	res, err := r.pull()
	if err == nil {
		t.Fatalf("pull over a cut connection should fail, got %+v", res)
	}
	if !sameSet(res.Fetched, "seg-1.ndjson") {
		t.Fatalf("only seg-1 should be complete after the cut: %+v", res)
	}
	part, perr := r.archive.partialPath(testAgent, b)
	if perr != nil {
		t.Fatal(perr)
	}
	st, serr := os.Stat(part)
	if serr != nil {
		t.Fatalf("no partial left for the interrupted segment: %v", serr)
	}
	have := st.Size()
	if have <= 0 || have >= b.Bytes {
		t.Fatalf("partial size %d should be strictly inside (0,%d)", have, b.Bytes)
	}
	if need, _ := r.archive.Need(testAgent, []capture.SegmentEntry{b}); len(need) != 1 {
		t.Fatalf("interrupted segment must not be in the archive manifest")
	}

	// The agent comes back (a fresh process on the same address) and the
	// server pulls again.
	ag1.stop()
	ag2 := r.startAgent(r.addr, agentOpts{chunk: chunk})
	res = r.mustPull()
	if !sameSet(res.Fetched, "seg-2.ndjson", "seg-3.ndjson") || !sameSet(res.Resumed, "seg-2.ndjson") || res.Skipped != 1 || len(res.Rejected) != 0 {
		t.Fatalf("resume pull: %+v", res)
	}
	// seg-1 (complete) was not re-downloaded; seg-2 resumed from the partial.
	if want := (b.Bytes - have) + c.Bytes; ag2.BytesServed() != want || res.Bytes != want {
		t.Fatalf("after reconnect agent served %d (result %d), want %d (seg-2 remainder %d + seg-3 %d; seg-1 %d must not be resent)",
			ag2.BytesServed(), res.Bytes, want, b.Bytes-have, c.Bytes, a.Bytes)
	}
	if got := fileSHA(t, filepath.Join(r.archive.Root(), testAgent, "sess-A", "seg-2.ndjson")); got != b.SHA256 {
		t.Errorf("resumed segment sha %s != %s", got, b.SHA256)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Errorf("partial should be gone once committed: %v", err)
	}
}

func TestCorruptSegmentIsRejected(t *testing.T) {
	r := newRig(t)
	good := synthSegment(t, r.evDir, testAgent, "seg-1.ndjson", "sess-A", t0, 10, 300)
	bad := synthSegment(t, r.evDir, testAgent, "seg-2.ndjson", "sess-A", t0.Add(time.Minute), 10, 300)
	// Corrupt seg-2 on the agent's disk after it was indexed: same size, other bytes.
	p := filepath.Join(r.evDir, "seg-2.ndjson")
	data := mustRead(t, p)
	data[len(data)/2] ^= 0xFF
	if err := os.WriteFile(p, data, 0o640); err != nil {
		t.Fatal(err)
	}
	r.startAgent("127.0.0.1:0", agentOpts{chunk: 1024})

	res := r.mustPull()
	if !sameSet(res.Fetched, good.File) || !sameSet(res.Rejected, bad.File) {
		t.Fatalf("want seg-1 fetched and seg-2 rejected: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(r.archive.Root(), testAgent, "sess-A", bad.File)); !os.IsNotExist(err) {
		t.Errorf("corrupt segment must not reach the archive: %v", err)
	}
	segs, _ := r.archive.Segments(testAgent)
	if len(segs) != 1 || segs[0].File != good.File {
		t.Errorf("archive manifest should hold only the good segment: %+v", segs)
	}
	part, _ := r.archive.partialPath(testAgent, bad)
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Errorf("rejected bytes should be discarded: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCorruptPartialRestartsFromZero(t *testing.T) {
	r := newRig(t)
	e := synthSegment(t, r.evDir, testAgent, "seg-1.ndjson", "sess-A", t0, 30, 300)
	// A damaged partial from an earlier attempt (right size prefix, wrong bytes).
	part, _ := r.archive.partialPath(testAgent, e)
	if err := os.MkdirAll(filepath.Dir(part), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, make([]byte, e.Bytes/2), 0o640); err != nil {
		t.Fatal(err)
	}
	ag := r.startAgent("127.0.0.1:0", agentOpts{chunk: 1024})

	res := r.mustPull()
	if !sameSet(res.Fetched, e.File) || len(res.Rejected) != 0 {
		t.Fatalf("a bad partial should heal by refetching: %+v", res)
	}
	// Resumed the second half (the partial was bad), then the whole segment again.
	if want := (e.Bytes - e.Bytes/2) + e.Bytes; ag.BytesServed() != want {
		t.Errorf("served %d, want %d", ag.BytesServed(), want)
	}
	if got := fileSHA(t, filepath.Join(r.archive.Root(), testAgent, "sess-A", e.File)); got != e.SHA256 {
		t.Errorf("archived sha mismatch after heal")
	}
}

func TestIdlePushUsesSameProtocol(t *testing.T) {
	r := newRig(t)
	e1 := synthSegment(t, r.evDir, testAgent, "seg-1.ndjson", "sess-A", t0, 15, 300)
	e2 := synthSegment(t, r.evDir, testAgent, "seg-2.ndjson", "sess-B", t0.Add(time.Minute), 15, 300)
	var idle atomic.Bool
	ag := r.startAgent("127.0.0.1:0", agentOpts{chunk: 1024, idle: idle.Load})
	ctx := context.Background()

	// Busy: nothing is offered and nothing arrives.
	idle.Store(false)
	if pushed, err := ag.PushIfIdle(ctx); pushed || err != nil {
		t.Fatalf("busy agent must not push: pushed=%v err=%v", pushed, err)
	}
	if n := ag.sender.count(channel.TypeEvManifest); n != 0 {
		t.Fatalf("busy agent sent %d manifests", n)
	}
	select {
	case o := <-r.pushes:
		t.Fatalf("server saw a push from a busy agent: %+v", o)
	case <-time.After(200 * time.Millisecond):
	}
	if segs, _ := r.archive.Segments(testAgent); len(segs) != 0 {
		t.Fatalf("archive should be empty, has %d", len(segs))
	}

	// Idle: it offers its manifest; the server fetches via the pull machinery.
	idle.Store(true)
	pushed, err := ag.PushIfIdle(ctx)
	if err != nil || !pushed {
		t.Fatalf("idle agent should push: pushed=%v err=%v", pushed, err)
	}
	o := waitFor(t, r.pushes, "push outcome")
	if o.err != nil || !sameSet(o.res.Fetched, e1.File, e2.File) || len(o.res.Rejected) != 0 {
		t.Fatalf("push outcome: %+v", o)
	}
	if want := e1.Bytes + e2.Bytes; ag.BytesServed() != want {
		t.Errorf("served %d, want %d", ag.BytesServed(), want)
	}
	if got := fileSHA(t, filepath.Join(r.archive.Root(), testAgent, "sess-B", e2.File)); got != e2.SHA256 {
		t.Errorf("pushed segment sha mismatch")
	}
	// The same range/chunk/verify exchange as a pull carried it.
	if ag.sender.count(channel.TypeEvChunk) == 0 {
		t.Errorf("no ev-chunk sent")
	}

	// Once the server has confirmed everything, an idle agent has nothing pending.
	deadline := time.Now().Add(testWait)
	for {
		if pushed, _ = ag.PushIfIdle(ctx); !pushed {
			break
		}
		// The verdict had not arrived yet: the server answers the redundant
		// offer with an empty sync. Drain it and retry.
		waitFor(t, r.pushes, "redundant push outcome")
		if time.Now().After(deadline) {
			t.Fatal("agent keeps pushing confirmed segments")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestArchiveLayoutPerMachineSession(t *testing.T) {
	r := newRig(t)
	synthSegment(t, r.evDir, testAgent, "seg-1.ndjson", "sess-A", t0, 5, 100)
	// One segment holding two sessions (concurrent session captures share a dir).
	mixed := []capture.Record{
		synthRecord(t, "sess-B", t0.Add(time.Minute), 1, 100),
		synthRecord(t, "sess-C", t0.Add(time.Minute+time.Millisecond), 2, 100),
	}
	writeSegment(t, r.evDir, testAgent, "seg-2.ndjson", mixed)
	r.startAgent("127.0.0.1:0", agentOpts{chunk: 1024})
	r.mustPull()

	root := r.archive.Root()
	for _, p := range []string{
		filepath.Join(testAgent, "sess-A", "seg-1.ndjson"),
		filepath.Join(testAgent, "sess-B", "seg-2.ndjson"),
		filepath.Join(testAgent, ArchiveManifestName),
	} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Errorf("expected %s in the archive: %v", p, err)
		}
	}
	segs, _ := r.archive.Segments(testAgent)
	if len(segs) != 2 || !sameSet(segs[1].Sessions, "sess-B", "sess-C") {
		t.Errorf("archive manifest sessions wrong: %+v", segs)
	}
	// A new archive over the same dir sees the persisted manifest.
	again, err := OpenArchive(filepath.Dir(root), ArchiveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := again.Segments(testAgent); len(got) != 2 {
		t.Errorf("reopened archive has %d segments, want 2", len(got))
	}
	if m, _ := again.Machines(); !sameSet(m, testAgent) {
		t.Errorf("machines: %v", m)
	}
}

func TestDiskGuardWarnsThenPausesIntake(t *testing.T) {
	r := newRig(t)
	e := synthSegment(t, r.evDir, testAgent, "seg-1.ndjson", "sess-A", t0, 10, 100)
	r.startAgent("127.0.0.1:0", agentOpts{chunk: 1024})

	r.disk.Store(85.0)
	if st, _ := r.archive.Guard(); st != DiskWarn {
		t.Fatalf("85%% should warn, got %v", st)
	}
	r.disk.Store(90.0)
	if st, _ := r.archive.Guard(); st != DiskPaused {
		t.Fatalf("90%% should pause, got %v", st)
	}
	res, err := r.pull()
	if !errors.Is(err, ErrIntakePaused) || len(res.Fetched) != 0 {
		t.Fatalf("pull at 90%%: res=%+v err=%v, want ErrIntakePaused and nothing fetched", res, err)
	}
	if segs, _ := r.archive.Segments(testAgent); len(segs) != 0 {
		t.Fatalf("paused intake must not archive anything")
	}

	// A warn-level disk still accepts intake (warn only).
	r.disk.Store(85.0)
	if res := r.mustPull(); !sameSet(res.Fetched, e.File) {
		t.Fatalf("warn level should still sync: %+v", res)
	}
}

func TestAgentRefusesUnknownAndChangedSegments(t *testing.T) {
	r := newRig(t)
	e := synthSegment(t, r.evDir, testAgent, "seg-1.ndjson", "sess-A", t0, 5, 100)
	r.startAgent("127.0.0.1:0", agentOpts{chunk: 1024})

	// Ask for a file outside the manifest, a stale digest and a bad offset.
	for _, req := range []RangeMsg{
		{File: "../../etc/passwd", Offset: 0},
		{File: "nope.ndjson", Offset: 0},
		{File: e.File, SHA256: "deadbeef", Offset: 0},
		{File: e.File, SHA256: e.SHA256, Offset: e.Bytes + 1},
	} {
		id := "rng-test"
		mb := r.syncer.register(id)
		if err := send(context.Background(), &r.toAgent, channel.TypeEvRange, id, req); err != nil {
			t.Fatal(err)
		}
		env, err := r.syncer.next(context.Background(), mb)
		r.syncer.unregister(id)
		if err != nil {
			t.Fatalf("%+v: %v", req, err)
		}
		var c ChunkMsg
		_ = json.Unmarshal(env.Payload, &c)
		if c.Err == "" || len(c.Data) != 0 {
			t.Errorf("request %+v should be refused, got %+v", req, c)
		}
	}
}
