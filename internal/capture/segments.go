package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Segment rolling and disk-guard defaults (§7.1, §6.5).
const (
	DefaultSegmentMaxBytes  = 50 << 20 // 50 MB
	DefaultSegmentMaxAge    = 5 * time.Minute
	DefaultDiskWarnPercent  = 80
	DefaultDiskPausePercent = 90
)

// ErrDiskPaused is returned by Write when the disk guard has paused writes
// (usage >= pause threshold, §6.5). The guard fails safe: it pauses, never
// deletes. The packet is not recorded; callers may account it.
var ErrDiskPaused = errors.New("capture: disk guard paused writes")

// diskUsageFunc reports a path's filesystem usage as a percentage (0..100).
// Overridable in tests; the production value uses statfs.
type diskUsageFunc func(path string) (float64, error)

func statfsUsagePercent(path string) (float64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("capture: statfs %s: %w", path, err)
	}
	total := st.Blocks * uint64(st.Bsize)
	if total == 0 {
		return 0, nil
	}
	free := st.Bavail * uint64(st.Bsize)
	used := total - free
	return float64(used) / float64(total) * 100, nil
}

// SegmentConfig tunes segment rolling and the disk guard.
type SegmentConfig struct {
	MaxBytes         int64         // roll at this size (default 50 MB)
	MaxAge           time.Duration // roll at this age (default 5 min)
	DiskWarnPercent  int           // warn threshold (default 80)
	DiskPausePercent int           // pause-writes threshold (default 90)
}

func (c *SegmentConfig) withDefaults() {
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultSegmentMaxBytes
	}
	if c.MaxAge <= 0 {
		c.MaxAge = DefaultSegmentMaxAge
	}
	if c.DiskWarnPercent <= 0 {
		c.DiskWarnPercent = DefaultDiskWarnPercent
	}
	if c.DiskPausePercent <= 0 {
		c.DiskPausePercent = DefaultDiskPausePercent
	}
}

// segCounter makes segment filenames unique across every SegmentWriter in the
// process: concurrent sessions share one machine directory, and two writers
// opening a segment in the same nanosecond must not pick the same name.
var segCounter atomic.Uint64

// openSegment is the currently-open segment file and its running accounting.
type openSegment struct {
	f       *os.File
	name    string
	hash    hash.Hash
	w       io.Writer
	bytes   int64
	records int
	start   time.Time
	end     time.Time
	opened  time.Time
}

// SegmentWriter appends records to rolling segment files under one machine's
// evidence directory and keeps the manifest in step. It is safe for concurrent
// use. Rolling happens on size (MaxBytes) or age (MaxAge), whichever first; the
// disk guard warns at DiskWarnPercent and pauses writes at DiskPausePercent.
type SegmentWriter struct {
	dir      string
	manifest *Manifest
	cfg      SegmentConfig
	diskUse  diskUsageFunc
	log      *slog.Logger
	now      func() time.Time

	mu             sync.Mutex
	cur            *openSegment
	pendingDropped uint64 // drops counted before/between segments
	paused         bool
	warned         bool
}

// NewSegmentWriter creates (if needed) the machine evidence dir, loads its
// manifest, and returns a writer. machineDir is DataDir/evidence/<machine>.
func NewSegmentWriter(machineDir, machine string, cfg SegmentConfig, log *slog.Logger) (*SegmentWriter, error) {
	if log == nil {
		log = slog.Default()
	}
	cfg.withDefaults()
	if err := os.MkdirAll(machineDir, 0o750); err != nil {
		return nil, fmt.Errorf("capture: evidence dir: %w", err)
	}
	mf, err := LoadManifest(machineDir, machine)
	if err != nil {
		return nil, err
	}
	return &SegmentWriter{
		dir:      machineDir,
		manifest: mf,
		cfg:      cfg,
		diskUse:  statfsUsagePercent,
		log:      log,
		now:      time.Now,
	}, nil
}

// Manifest returns the writer's manifest (for verification/reads).
func (w *SegmentWriter) Manifest() *Manifest { return w.manifest }

// AddDropped records best-effort dropped packets (§6.3b). They are attributed
// to the current segment, or to the next one opened if none is open yet.
func (w *SegmentWriter) AddDropped(n uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pendingDropped += n
}

// Write appends one record, rolling the segment first if it is full or stale.
// It returns ErrDiskPaused (and does not record) when the disk guard has paused
// writes.
func (w *SegmentWriter) Write(rec Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.checkDiskLocked(); err != nil {
		return err
	}
	now := w.now()
	if err := w.rollIfNeededLocked(now); err != nil {
		return err
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("capture: encode record: %w", err)
	}
	line = append(line, '\n')
	n, err := w.cur.w.Write(line)
	if err != nil {
		return fmt.Errorf("capture: write record: %w", err)
	}
	w.cur.bytes += int64(n)
	w.cur.records++
	if w.cur.records == 1 {
		w.cur.start = rec.Timestamp
	}
	w.cur.end = rec.Timestamp
	return nil
}

// checkDiskLocked applies the §6.5 guard: warn at DiskWarnPercent, pause writes
// at DiskPausePercent. It never deletes.
func (w *SegmentWriter) checkDiskLocked() error {
	use, err := w.diskUse(w.dir)
	if err != nil {
		return nil // fail open on a stat error: do not block capture on it
	}
	switch {
	case use >= float64(w.cfg.DiskPausePercent):
		if !w.paused {
			w.paused = true
			w.log.Warn("capture: disk guard paused writes", "used_percent", use, "pause_at", w.cfg.DiskPausePercent)
		}
		return ErrDiskPaused
	case use >= float64(w.cfg.DiskWarnPercent):
		w.paused = false
		if !w.warned {
			w.warned = true
			w.log.Warn("capture: disk usage high", "used_percent", use, "warn_at", w.cfg.DiskWarnPercent)
		}
	default:
		w.paused, w.warned = false, false
	}
	return nil
}

// rollIfNeededLocked opens the first segment, or finalizes and reopens when the
// current one is full (size) or stale (age).
func (w *SegmentWriter) rollIfNeededLocked(now time.Time) error {
	if w.cur == nil {
		return w.openLocked(now)
	}
	if w.cur.bytes >= w.cfg.MaxBytes || now.Sub(w.cur.opened) >= w.cfg.MaxAge {
		if err := w.finalizeLocked(); err != nil {
			return err
		}
		return w.openLocked(now)
	}
	return nil
}

// RollIfStale finalizes the current segment if it has reached MaxAge. The
// capture loop calls it on a ticker so time ranges stay bounded while idle.
func (w *SegmentWriter) RollIfStale() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur != nil && w.now().Sub(w.cur.opened) >= w.cfg.MaxAge {
		return w.finalizeLocked()
	}
	return nil
}

func (w *SegmentWriter) openLocked(now time.Time) error {
	name := fmt.Sprintf("seg-%020d-%06d.ndjson", now.UnixNano(), segCounter.Add(1))
	f, err := os.OpenFile(filepath.Join(w.dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("capture: open segment: %w", err)
	}
	h := sha256.New()
	w.cur = &openSegment{
		f:      f,
		name:   name,
		hash:   h,
		w:      io.MultiWriter(f, h),
		opened: now,
	}
	return nil
}

// finalizeLocked closes the current segment and records it in the manifest.
// Empty segments (no records) are removed, not indexed.
func (w *SegmentWriter) finalizeLocked() error {
	s := w.cur
	w.cur = nil
	if s == nil {
		return nil
	}
	if err := s.f.Sync(); err != nil {
		s.f.Close()
		return fmt.Errorf("capture: sync segment: %w", err)
	}
	if err := s.f.Close(); err != nil {
		return fmt.Errorf("capture: close segment: %w", err)
	}
	if s.records == 0 {
		// Keep any pending drops for the next segment; drop the empty file.
		os.Remove(filepath.Join(w.dir, s.name))
		return nil
	}
	dropped := w.pendingDropped
	w.pendingDropped = 0
	return w.manifest.Add(SegmentEntry{
		File:          s.name,
		SHA256:        hex.EncodeToString(s.hash.Sum(nil)),
		Bytes:         s.bytes,
		Records:       s.records,
		Dropped:       dropped,
		StartTime:     s.start,
		EndTime:       s.end,
		SchemaVersion: SchemaVersion,
		Compression:   CompressionNone,
	})
}

// Close finalizes the open segment and flushes the manifest. After Close the
// writer must not be used again.
func (w *SegmentWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.finalizeLocked()
}
