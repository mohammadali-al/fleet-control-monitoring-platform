package capture

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestConcurrentSessionsShareManifest runs several SegmentWriters (one per
// capture session, as T-0008 creates them) over the same machine directory at
// once, each rolling many segments. Run with -race: the manifest must be updated
// without a data race, every segment of every session must be indexed, and the
// manifest must verify against the files on disk.
func TestConcurrentSessionsShareManifest(t *testing.T) {
	const (
		sessions   = 8
		perSession = 60
	)
	dir := filepath.Join(t.TempDir(), "evidence", "machine-1")
	writers := make([]*SegmentWriter, sessions)
	for i := range writers {
		w, err := NewSegmentWriter(dir, "machine-1", SegmentConfig{MaxBytes: 400, MaxAge: time.Hour}, nil)
		if err != nil {
			t.Fatalf("NewSegmentWriter %d: %v", i, err)
		}
		w.diskUse = func(string) (float64, error) { return 10, nil }
		writers[i] = w
	}

	// Build records up front: rec uses t.Fatalf, which must stay off goroutines.
	recs := make([][]Record, sessions)
	ts := time.Now()
	for i := range recs {
		sess := fmt.Sprintf("sess-%d", i)
		for n := 0; n < perSession; n++ {
			recs[i] = append(recs[i], rec(t, sess, ts.Add(time.Duration(n)*time.Millisecond), []byte("concurrent evidence payload")))
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, sessions)
	for i, w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n, r := range recs[i] {
				if err := w.Write(r); err != nil {
					errs <- fmt.Errorf("session %d write %d: %w", i, n, err)
					return
				}
			}
			if err := w.Close(); err != nil {
				errs <- fmt.Errorf("session %d close: %w", i, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Every closed segment file on disk must be indexed.
	files, err := filepath.Glob(filepath.Join(dir, "seg-*.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < sessions*2 {
		t.Fatalf("only %d segment files; the test needs rolling to exercise the manifest", len(files))
	}

	// A fresh load from disk is the source of truth.
	mf, err := LoadManifest(dir, "machine-1")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	entries := mf.Entries()
	if len(entries) != len(files) {
		t.Fatalf("manifest has %d entries but %d segment files are on disk: entries were lost", len(entries), len(files))
	}
	seen := map[string]bool{}
	records := 0
	for _, e := range entries {
		if seen[e.File] {
			t.Errorf("duplicate manifest entry for %s", e.File)
		}
		seen[e.File] = true
		records += e.Records
	}
	for _, f := range files {
		if !seen[filepath.Base(f)] {
			t.Errorf("segment %s is missing from the manifest", filepath.Base(f))
		}
	}
	if records != sessions*perSession {
		t.Errorf("manifest records = %d, want %d", records, sessions*perSession)
	}
	if err := mf.Verify(); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestConcurrentManifestAdd hammers Manifest.Add directly from independent
// Manifest values bound to one directory (the shared-manifest hazard in
// isolation, without segment I/O).
func TestConcurrentManifestAdd(t *testing.T) {
	const adders, each = 10, 25
	dir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, adders)
	for a := 0; a < adders; a++ {
		mf, err := LoadManifest(dir, "m")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < each; n++ {
				e := SegmentEntry{File: fmt.Sprintf("seg-%d-%d.ndjson", a, n), SHA256: "x", Records: 1,
					SchemaVersion: SchemaVersion, Compression: CompressionNone}
				if err := mf.Add(e); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	mf, err := LoadManifest(dir, "m")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(mf.Entries()); got != adders*each {
		t.Errorf("manifest has %d entries, want %d (lost updates)", got, adders*each)
	}
}
