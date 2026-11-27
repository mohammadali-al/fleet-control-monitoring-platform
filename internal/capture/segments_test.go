package capture

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rec(t *testing.T, sess string, ts time.Time, payload []byte) Record {
	t.Helper()
	pkt := tcpv4(t, "10.0.0.2", "93.184.216.34", 40000, 80, false, true, payload)
	r, err := BuildRecord(sess, ts, pkt, "203.0.113.9", 0)
	if err != nil {
		t.Fatalf("BuildRecord: %v", err)
	}
	return r
}

func newWriter(t *testing.T, cfg SegmentConfig) *SegmentWriter {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "evidence", "machine-1")
	w, err := NewSegmentWriter(dir, "machine-1", cfg, nil)
	if err != nil {
		t.Fatalf("NewSegmentWriter: %v", err)
	}
	// Default to plenty of free disk unless a test overrides it.
	w.diskUse = func(string) (float64, error) { return 10, nil }
	return w
}

func TestSegmentRollBySize(t *testing.T) {
	w := newWriter(t, SegmentConfig{MaxBytes: 300, MaxAge: time.Hour})
	ts := time.Now()
	for i := 0; i < 20; i++ {
		if err := w.Write(rec(t, "s", ts.Add(time.Duration(i)*time.Millisecond), []byte("hello world payload"))); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	entries := w.Manifest().Entries()
	if len(entries) < 2 {
		t.Fatalf("expected multiple segments from size rolling, got %d", len(entries))
	}
	total := 0
	for _, e := range entries {
		total += e.Records
		if e.Bytes <= 0 || e.SHA256 == "" {
			t.Errorf("segment %s has bad bytes/sha", e.File)
		}
		if e.SchemaVersion != SchemaVersion || e.Compression != CompressionNone {
			t.Errorf("segment %s schema/compression wrong", e.File)
		}
	}
	if total != 20 {
		t.Errorf("records across segments = %d, want 20", total)
	}
	if err := w.Manifest().Verify(); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestSegmentRollByAge(t *testing.T) {
	w := newWriter(t, SegmentConfig{MaxBytes: 1 << 30, MaxAge: 5 * time.Minute})
	now := time.Now()
	w.now = func() time.Time { return now }

	if err := w.Write(rec(t, "s", now, []byte("a"))); err != nil {
		t.Fatal(err)
	}
	// Advance past MaxAge; the next write must open a new segment.
	now = now.Add(6 * time.Minute)
	if err := w.Write(rec(t, "s", now, []byte("b"))); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(w.Manifest().Entries()); n != 2 {
		t.Errorf("age rolling produced %d segments, want 2", n)
	}
}

func TestManifestVerifyDetectsTampering(t *testing.T) {
	w := newWriter(t, SegmentConfig{MaxBytes: 1 << 30, MaxAge: time.Hour})
	ts := time.Now()
	for i := 0; i < 5; i++ {
		if err := w.Write(rec(t, "s", ts, []byte("payload"))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	mf := w.Manifest()
	if err := mf.Verify(); err != nil {
		t.Fatalf("Verify clean: %v", err)
	}

	// Re-load the manifest from disk and confirm it still verifies.
	reloaded, err := LoadManifest(w.dir, "machine-1")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(reloaded.Entries()) != len(mf.Entries()) {
		t.Fatalf("reloaded manifest has %d entries, want %d", len(reloaded.Entries()), len(mf.Entries()))
	}
	if err := reloaded.Verify(); err != nil {
		t.Fatalf("reloaded Verify: %v", err)
	}

	// Tamper with a segment: Verify must now fail.
	seg := filepath.Join(w.dir, reloaded.Entries()[0].File)
	if err := os.WriteFile(seg, []byte("tampered\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Verify(); err == nil {
		t.Error("Verify should fail on a tampered segment")
	}
}

func TestDiskGuardPauses(t *testing.T) {
	w := newWriter(t, SegmentConfig{MaxBytes: 1 << 30, MaxAge: time.Hour, DiskWarnPercent: 80, DiskPausePercent: 90})
	ts := time.Now()

	usage := 50.0
	w.diskUse = func(string) (float64, error) { return usage, nil }
	if err := w.Write(rec(t, "s", ts, []byte("ok"))); err != nil {
		t.Fatalf("write at 50%%: %v", err)
	}

	usage = 95.0
	if err := w.Write(rec(t, "s", ts, []byte("nope"))); err != ErrDiskPaused {
		t.Fatalf("write at 95%% = %v, want ErrDiskPaused", err)
	}

	// Recover below the pause threshold: writes resume.
	usage = 50.0
	if err := w.Write(rec(t, "s", ts, []byte("again"))); err != nil {
		t.Fatalf("write after recovery: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Two records written, one paused.
	total := 0
	for _, e := range w.Manifest().Entries() {
		total += e.Records
	}
	if total != 2 {
		t.Errorf("recorded %d, want 2 (one was paused)", total)
	}
}

func TestDroppedCountRecorded(t *testing.T) {
	w := newWriter(t, SegmentConfig{MaxBytes: 1 << 30, MaxAge: time.Hour})
	w.AddDropped(7)
	if err := w.Write(rec(t, "s", time.Now(), []byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	e := w.Manifest().Entries()
	if len(e) != 1 || e[0].Dropped != 7 {
		t.Errorf("dropped = %v, want 7 in one segment", e)
	}
}

func TestDiskGuardCallbackTransitions(t *testing.T) {
	w := newWriter(t, SegmentConfig{MaxBytes: 1 << 30, MaxAge: time.Hour, DiskWarnPercent: 80, DiskPausePercent: 90})
	ts := time.Now()

	var events []DiskStatus
	w.SetOnDiskState(func(st DiskStatus) {
		// Verify callback runs without holding w.mu by calling AddDropped.
		w.AddDropped(0)
		events = append(events, st)
	})

	usage := 50.0
	w.diskUse = func(string) (float64, error) { return usage, nil }
	if err := w.Write(rec(t, "s", ts, []byte("ok1"))); err != nil {
		t.Fatal(err)
	}
	// Same status again -> no duplicate transition event.
	if err := w.Write(rec(t, "s", ts, []byte("ok2"))); err != nil {
		t.Fatal(err)
	}

	usage = 85.0
	if err := w.Write(rec(t, "s", ts, []byte("warn1"))); err != nil {
		t.Fatal(err)
	}

	usage = 92.0
	if err := w.Write(rec(t, "s", ts, []byte("crit1"))); err != ErrDiskPaused {
		t.Fatalf("err = %v, want ErrDiskPaused", err)
	}

	usage = 40.0
	if err := w.Write(rec(t, "s", ts, []byte("ok3"))); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()

	want := []string{DiskStatusOK, DiskStatusWarn, DiskStatusCritical, DiskStatusOK}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want %v", events, want)
	}
	for i, wStatus := range want {
		if events[i].Status != wStatus {
			t.Errorf("event[%d].Status = %q, want %q", i, events[i].Status, wStatus)
		}
	}

	st, err := CheckDisk(t.TempDir(), 0, 0)
	if err != nil {
		t.Fatalf("CheckDisk: %v", err)
	}
	if st.TotalBytes == 0 || st.WarnAt != DefaultDiskWarnPercent || st.PauseAt != DefaultDiskPausePercent {
		t.Errorf("CheckDisk = %+v", st)
	}
}
