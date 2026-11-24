package evidence

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"golang.org/x/sys/unix"
)

// ArchiveDir is the archive's directory name under the base data dir (D-0006).
const ArchiveDir = "archive"

// partialDir holds in-flight (not yet verified) segment bytes per machine.
const partialDir = ".partial"

// Server disk-guard defaults (§7.3): warn at 80 %, pause intake at 90 %.
const (
	DefaultWarnPercent  = 80
	DefaultPausePercent = 90
)

// ErrIntakePaused is returned when the disk guard has paused evidence intake.
// The guard fails safe: it stops accepting, it never deletes (unlimited
// retention).
var ErrIntakePaused = errors.New("evidence: disk guard paused archive intake")

// DiskState is the disk guard's verdict.
type DiskState int

// Disk guard states.
const (
	DiskOK DiskState = iota
	DiskWarn
	DiskPaused
)

func (s DiskState) String() string {
	switch s {
	case DiskWarn:
		return "warn"
	case DiskPaused:
		return "paused"
	}
	return "ok"
}

// ArchiveOptions tunes the archive. The zero value selects the defaults.
type ArchiveOptions struct {
	WarnPercent  int                                // default 80
	PausePercent int                                // default 90
	DiskUsage    func(path string) (float64, error) // test hook; default statfs
	Log          *slog.Logger
}

// Archive is the main server's evidence archive under DataDir/archive: one
// directory per machine, one per session within it, mirroring the agent's
// segment structure; plaintext, unlimited retention.
type Archive struct {
	root string
	opts ArchiveOptions
	log  *slog.Logger

	mu        sync.Mutex
	manifests map[string]*ArchiveManifest
	state     DiskState
}

// OpenArchive creates (if needed) DataDir/archive and returns the archive.
func OpenArchive(dataDir string, opts ArchiveOptions) (*Archive, error) {
	if dataDir == "" {
		return nil, errors.New("evidence: archive needs a data dir")
	}
	if opts.WarnPercent <= 0 {
		opts.WarnPercent = DefaultWarnPercent
	}
	if opts.PausePercent <= 0 {
		opts.PausePercent = DefaultPausePercent
	}
	if opts.DiskUsage == nil {
		opts.DiskUsage = statfsUsagePercent
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	root := filepath.Join(dataDir, ArchiveDir)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("evidence: archive dir: %w", err)
	}
	return &Archive{root: root, opts: opts, log: log, manifests: map[string]*ArchiveManifest{}}, nil
}

// Root is DataDir/archive.
func (a *Archive) Root() string { return a.root }

func statfsUsagePercent(path string) (float64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("evidence: statfs %s: %w", path, err)
	}
	total := st.Blocks * uint64(st.Bsize)
	if total == 0 {
		return 0, nil
	}
	free := st.Bavail * uint64(st.Bsize)
	return float64(total-free) / float64(total) * 100, nil
}

// Guard reports the disk state and usage percent of the archive's filesystem,
// logging once per transition into warn or paused.
func (a *Archive) Guard() (DiskState, float64) {
	use, err := a.opts.DiskUsage(a.root)
	if err != nil {
		return DiskOK, 0 // fail open on a stat error, as the agent-side guard does
	}
	st := DiskOK
	switch {
	case use >= float64(a.opts.PausePercent):
		st = DiskPaused
	case use >= float64(a.opts.WarnPercent):
		st = DiskWarn
	}
	a.mu.Lock()
	prev := a.state
	a.state = st
	a.mu.Unlock()
	if st != prev {
		switch st {
		case DiskPaused:
			a.log.Warn("evidence: disk guard paused archive intake", "used_percent", use, "pause_at", a.opts.PausePercent)
		case DiskWarn:
			a.log.Warn("evidence: archive disk usage high", "used_percent", use, "warn_at", a.opts.WarnPercent)
		}
	}
	return st, use
}

// CheckIntake returns ErrIntakePaused when usage is at or past the pause
// threshold, else nil.
func (a *Archive) CheckIntake() error {
	if st, _ := a.Guard(); st == DiskPaused {
		return ErrIntakePaused
	}
	return nil
}

func (a *Archive) machineDir(machine string) (string, error) {
	if !safeComponent(machine) {
		return "", fmt.Errorf("evidence: unsafe machine id %q", machine)
	}
	return filepath.Join(a.root, machine), nil
}

// manifest returns the machine's manifest copy, loading it on first use.
func (a *Archive) manifest(machine string) (*ArchiveManifest, error) {
	dir, err := a.machineDir(machine)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if m, ok := a.manifests[machine]; ok {
		return m, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("evidence: machine dir: %w", err)
	}
	m, err := loadArchiveManifest(dir, machine)
	if err != nil {
		return nil, err
	}
	a.manifests[machine] = m
	return m, nil
}

// Need returns the entries of the agent's manifest the archive lacks or holds
// with a different digest.
func (a *Archive) Need(machine string, remote []capture.SegmentEntry) ([]capture.SegmentEntry, error) {
	m, err := a.manifest(machine)
	if err != nil {
		return nil, err
	}
	return Diff(m, remote), nil
}

// partialPath is where an in-flight segment accumulates. The digest is part of
// the name so a changed segment never resumes onto stale bytes.
func (a *Archive) partialPath(machine string, e capture.SegmentEntry) (string, error) {
	dir, err := a.machineDir(machine)
	if err != nil {
		return "", err
	}
	if !safeComponent(e.File) {
		return "", fmt.Errorf("evidence: unsafe segment name %q", e.File)
	}
	sum := e.SHA256
	if len(sum) > 16 {
		sum = sum[:16]
	}
	return filepath.Join(dir, partialDir, e.File+"."+sum+".part"), nil
}

// Commit files a verified partial segment under <machine>/<session>/ and
// records it in the machine's manifest copy. The caller has already verified
// the SHA-256; Commit only attributes and files it.
func (a *Archive) Commit(machine string, e capture.SegmentEntry, partial string) (ArchivedSegment, error) {
	m, err := a.manifest(machine)
	if err != nil {
		return ArchivedSegment{}, err
	}
	sessions, err := scanSessions(partial)
	if err != nil {
		return ArchivedSegment{}, err
	}
	session := UnattributedSession
	if len(sessions) > 0 && safeComponent(sessions[0]) {
		session = sessions[0]
	}
	dir, _ := a.machineDir(machine)
	destDir := filepath.Join(dir, session)
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return ArchivedSegment{}, fmt.Errorf("evidence: session dir: %w", err)
	}
	// A changed segment replaces the earlier version, possibly in another
	// session directory; remove the old file once the new one is in place.
	var old string
	for _, s := range m.snapshot() {
		if s.File == e.File {
			old = filepath.Join(dir, filepath.FromSlash(s.Path))
		}
	}
	dest := filepath.Join(destDir, e.File)
	if err := os.Rename(partial, dest); err != nil {
		return ArchivedSegment{}, fmt.Errorf("evidence: file segment: %w", err)
	}
	if old != "" && old != dest {
		_ = os.Remove(old)
	}
	seg := ArchivedSegment{SegmentEntry: e, Session: session, Sessions: sessions, Path: session + "/" + e.File}
	if err := m.put(seg); err != nil {
		return ArchivedSegment{}, err
	}
	return seg, nil
}

// scanSessions returns the distinct session ids of a segment's records, in
// first-seen order.
func scanSessions(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("evidence: scan segment: %w", err)
	}
	defer f.Close()
	var out []string
	seen := map[string]bool{}
	err = readRecords(f, func(r capture.Record) error {
		if r.SessionID != "" && !seen[r.SessionID] {
			seen[r.SessionID] = true
			out = append(out, r.SessionID)
		}
		return nil
	})
	return out, err
}

// readRecords decodes a segment's newline-delimited JSON records.
func readRecords(r io.Reader, fn func(capture.Record) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var rec capture.Record
			if jerr := json.Unmarshal(line, &rec); jerr != nil {
				return fmt.Errorf("evidence: bad record: %w", jerr)
			}
			if ferr := fn(rec); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("evidence: read segment: %w", err)
		}
	}
}

// Machines lists the machines that have archived segments, sorted.
func (a *Archive) Machines() ([]string, error) {
	ents, err := os.ReadDir(a.root)
	if err != nil {
		return nil, fmt.Errorf("evidence: list archive: %w", err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Segments lists a machine's archived segments, oldest first.
func (a *Archive) Segments(machine string) ([]ArchivedSegment, error) {
	m, err := a.manifest(machine)
	if err != nil {
		return nil, err
	}
	return m.snapshot(), nil
}

// OpenSegment opens an archived segment's bytes.
func (a *Archive) OpenSegment(machine string, s ArchivedSegment) (*os.File, error) {
	dir, err := a.machineDir(machine)
	if err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(dir, filepath.FromSlash(s.Path)))
}
