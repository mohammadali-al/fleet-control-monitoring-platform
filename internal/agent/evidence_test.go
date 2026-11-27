package agent_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/evidence"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
)

// evEnv is the server (with an evidence archive) and one real worker agent over
// mTLS; the worker's data dir is where the capture subsystem writes segments.
type evEnv struct {
	*mgrEnv
	dataDir string
	archive *evidence.Archive
}

func newEvEnv(t *testing.T) *evEnv {
	t.Helper()
	dataDir := t.TempDir()
	e := newMgrEnv(t, "s", "s", config.CaptureConfig{}, func(c *agent.Config) { c.DataDir = dataDir })
	arch, err := evidence.OpenArchive(t.TempDir(), evidence.ArchiveOptions{
		DiskUsage: func(string) (float64, error) { return 10, nil }})
	if err != nil {
		t.Fatal(err)
	}
	e.m.SetObserve(&fleet.Observe{Archive: arch})
	e.waitOnline(t)
	return &evEnv{mgrEnv: e, dataDir: dataDir, archive: arch}
}

// capturedSegments writes real capture segments (the SegmentWriter the capture
// subsystem uses) into the worker's evidence dir: n records of payloadLen random
// bytes, rolled at maxBytes. It returns the finalized segment entries.
func (e *evEnv) capturedSegments(t *testing.T, session string, n, payloadLen int, maxBytes int64) []capture.SegmentEntry {
	t.Helper()
	dir := capture.EvidenceMachineDir(e.dataDir, "w1")
	w, err := capture.NewSegmentWriter(dir, "w1", capture.SegmentConfig{MaxBytes: maxBytes}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < n; i++ {
		head := make([]byte, payloadLen)
		if _, err := rand.Read(head); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(head)
		if err := w.Write(capture.Record{
			SessionID: session, Timestamp: base.Add(time.Duration(i) * time.Millisecond), Protocol: "UDP",
			Src: "10.0.0.5:4000", Dst: "192.0.2.1:53", PayloadStatus: "full", PayloadBytesFull: payloadLen,
			PayloadSHA256: hex.EncodeToString(sum[:]), PayloadHead: head, SrcIPUsed: "10.0.0.5",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.Manifest().Entries()
}

func (e *evEnv) syncState() fleet.SyncStatus { return e.m.SyncStatuses()["w1"] }

func (e *evEnv) waitSync(t *testing.T, trigger string) fleet.SyncStatus {
	t.Helper()
	var st fleet.SyncStatus
	eventually(t, "evidence sync to finish", func() bool {
		st = e.syncState()
		return st.State == "done" || st.State == "failed"
	})
	if st.State != "done" || st.Error != "" {
		t.Fatalf("sync = %+v, want done", st)
	}
	if st.Trigger != trigger {
		t.Fatalf("sync trigger = %q, want %q", st.Trigger, trigger)
	}
	return st
}

func waitRunning(t *testing.T, sub *fleet.Subscription, session string) {
	t.Helper()
	timeout := time.After(wait)
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				t.Fatal("subscription closed")
			}
			if ev.Kind == fleet.EventExec && ev.Exec.SessionID == session && ev.Exec.Kind == exec.EventRunning {
				return
			}
		case <-timeout:
			t.Fatal("command never started")
		}
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// verifyArchived checks every worker segment is in the server archive with the
// same SHA-256 as the worker's file and as the manifest says.
func (e *evEnv) verifyArchived(t *testing.T, want []capture.SegmentEntry) {
	t.Helper()
	got, err := e.archive.Segments("w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("archive holds %d segments, want %d", len(got), len(want))
	}
	byFile := map[string]evidence.ArchivedSegment{}
	for _, g := range got {
		byFile[g.File] = g
	}
	for _, w := range want {
		g, ok := byFile[w.File]
		if !ok {
			t.Errorf("segment %s not archived", w.File)
			continue
		}
		path := filepath.Join(e.archive.Root(), "w1", filepath.FromSlash(g.Path))
		if sum := fileSHA256(t, path); sum != w.SHA256 {
			t.Errorf("archived %s sha256 = %s, want %s", w.File, sum, w.SHA256)
		}
		if local := fileSHA256(t, filepath.Join(capture.EvidenceMachineDir(e.dataDir, "w1"), w.File)); local != w.SHA256 {
			t.Errorf("worker %s sha256 = %s, manifest says %s", w.File, local, w.SHA256)
		}
		if g.SHA256 != w.SHA256 || g.Bytes != w.Bytes {
			t.Errorf("archived entry %+v differs from the worker's %+v", g.SegmentEntry, w)
		}
	}
}

// A server pull against a real worker agent (the real runtime over mTLS)
// transfers the segments the capture subsystem wrote: several segments, the
// first larger than one chunk, each verified against its SHA-256. A second pull
// has nothing to fetch.
func TestPullTransfersWorkerSegments(t *testing.T) {
	e := newEvEnv(t)
	want := e.capturedSegments(t, "sess-1", 160, 4096, 400<<10)
	if len(want) < 2 {
		t.Fatalf("test setup produced %d segments, want several", len(want))
	}
	if want[0].Bytes <= evidence.DefaultChunkSize {
		t.Fatalf("first segment is %d bytes; the test wants it to span several chunks", want[0].Bytes)
	}

	if err := e.m.PullEvidence("w1"); err != nil {
		t.Fatalf("PullEvidence: %v", err)
	}
	st := e.waitSync(t, "pull")
	if st.Segments != len(want) || st.Fetched != len(want) || st.Rejected != 0 || st.Bytes == 0 {
		t.Errorf("sync status = %+v, want %d fetched, none rejected", st, len(want))
	}
	e.verifyArchived(t, want)

	// Nothing new: the next pull transfers nothing.
	if err := e.m.PullEvidence("w1"); err != nil {
		t.Fatalf("second PullEvidence: %v", err)
	}
	eventually(t, "second pull to finish", func() bool {
		s := e.syncState()
		return s.State == "done" && s.Skipped == len(want) && s.Fetched == 0
	})
}

// A pull against a worker that never captured anything succeeds with nothing to
// transfer (an empty manifest, not the syncer timeout of the unwired agent).
func TestPullFromWorkerWithNoEvidence(t *testing.T) {
	e := newEvEnv(t)
	if err := e.m.PullEvidence("w1"); err != nil {
		t.Fatalf("PullEvidence: %v", err)
	}
	st := e.waitSync(t, "pull")
	if st.Segments != 0 || st.Fetched != 0 {
		t.Errorf("sync status = %+v, want an empty transfer", st)
	}
}

// A second pull fetches only the segment the worker captured since the first.
func TestPullFetchesNewSegmentsOnly(t *testing.T) {
	e := newEvEnv(t)
	first := e.capturedSegments(t, "sess-1", 20, 2048, 0)
	if err := e.m.PullEvidence("w1"); err != nil {
		t.Fatal(err)
	}
	e.waitSync(t, "pull")

	second := e.capturedSegments(t, "sess-2", 20, 2048, 0) // the manifest now lists both
	if len(second) != len(first)+1 {
		t.Fatalf("manifest has %d segments, want %d", len(second), len(first)+1)
	}
	if err := e.m.PullEvidence("w1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "second pull", func() bool {
		s := e.syncState()
		return s.State == "done" && s.Fetched == 1 && s.Skipped == len(first)
	})
	e.verifyArchived(t, second)
}

// An idle worker volunteers its segments: with no pull from the server they
// reach the archive, trigger "push", SHA-256 verified.
func TestIdleWorkerPushDelivers(t *testing.T) {
	defer agent.SetEvidencePushInterval(50 * time.Millisecond)()
	e := newEvEnv(t)
	want := e.capturedSegments(t, "sess-1", 40, 4096, 100<<10)

	eventually(t, "idle push to reach the archive", func() bool {
		segs, _ := e.archive.Segments("w1")
		return len(segs) == len(want)
	})
	e.verifyArchived(t, want)
	eventually(t, "push to be recorded", func() bool { return e.syncState().Trigger == "push" && e.syncState().State == "done" })
}

// A worker that is running a command is not idle: it holds its segments back
// and offers them once the command has ended.
func TestBusyWorkerDoesNotPush(t *testing.T) {
	defer agent.SetEvidencePushInterval(50 * time.Millisecond)()
	e := newEvEnv(t)
	sub := e.subscribe()
	defer sub.Close()

	id, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "sleep 1"})
	if err != nil {
		t.Fatal(err)
	}
	// The session is running once the server sees its running event; segments
	// written from here on must not be offered until it ends.
	waitRunning(t, sub, id)
	time.Sleep(100 * time.Millisecond) // let the agent's own session hook settle
	want := e.capturedSegments(t, "sess-1", 10, 1024, 0)

	time.Sleep(400 * time.Millisecond) // many push intervals, the command still runs
	if segs, _ := e.archive.Segments("w1"); len(segs) != 0 {
		t.Fatalf("a busy worker pushed %d segments", len(segs))
	}
	eventually(t, "idle push after the command ended", func() bool {
		segs, _ := e.archive.Segments("w1")
		return len(segs) == len(want)
	})
	e.verifyArchived(t, want)
}

// GAP-18: when a command is queued in the server's FIFO behind a running one,
// the server's queue-state signal keeps the agent from idle-pushing even
// during the transition between commands until the FIFO drains completely.
func TestQueuedServerCommandBlocksIdlePush(t *testing.T) {
	defer agent.SetEvidencePushInterval(20 * time.Millisecond)()
	e := newEvEnv(t)
	sub := e.subscribe()
	defer sub.Close()

	id1, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "sleep 0.4"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := e.m.Submit(fleet.SubmitRequest{Machine: "w1", Command: "sleep 0.4"})
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, sub, id1)
	want := e.capturedSegments(t, "sess-1", 10, 1024, 0)

	// Wait for cmd 1 to exit and cmd 2 to start, verifying no push happened in between.
	waitRunning(t, sub, id2)
	if segs, _ := e.archive.Segments("w1"); len(segs) != 0 {
		t.Fatalf("pushed %d segments while server FIFO queue still had a second command", len(segs))
	}

	// Wait for cmd 2 to finish; once the FIFO is empty, idle push delivers.
	eventually(t, "idle push after server FIFO queue drained", func() bool {
		segs, _ := e.archive.Segments("w1")
		return len(segs) == len(want)
	})
	e.verifyArchived(t, want)
}
