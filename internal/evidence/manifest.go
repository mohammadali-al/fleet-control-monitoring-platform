// Package evidence moves captured evidence from the worker agents to the main
// server and out to the customer (architecture chapter 7): checksum-diff delta
// sync (panel pull and agent idle push, one protocol), the main-server archive,
// and export to pcapng / CSV / JSON. It consumes the segments and manifest
// produced by internal/capture (T-0008) and rides on internal/channel.
package evidence

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
)

// ArchiveManifestName is the server's manifest copy inside each machine's
// archive directory (DataDir/archive/<machine>/).
const ArchiveManifestName = "manifest.json"

// UnattributedSession is the archive session directory for a segment whose
// records name no (usable) session.
const UnattributedSession = "_unattributed"

// ---- wire payloads (carried in channel envelopes) ----

// ManifestMsg is the payload of ev-manifest: the agent's finalized segments.
type ManifestMsg struct {
	Machine  string                 `json:"machine"`
	Segments []capture.SegmentEntry `json:"segments"`
}

// RangeMsg is the payload of ev-range: send File from Offset to the end. SHA256
// is the digest the server expects; the agent refuses if its entry changed.
type RangeMsg struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Offset int64  `json:"offset"`
}

// ChunkMsg is the payload of ev-chunk. Chunks of one range arrive in order;
// EOF marks the last one. A non-empty Err aborts the transfer.
type ChunkMsg struct {
	File   string `json:"file"`
	Offset int64  `json:"offset"`
	Data   []byte `json:"data,omitempty"`
	EOF    bool   `json:"eof,omitempty"`
	Err    string `json:"err,omitempty"`
}

// VerifyMsg is the payload of ev-verify: the server's verdict on a segment.
type VerifyMsg struct {
	File string `json:"file"`
	OK   bool   `json:"ok"`
	Err  string `json:"err,omitempty"`
}

// ---- archive manifest (the server's copy) ----

// ArchivedSegment is a segment held in the archive: the agent's manifest entry
// plus where it was filed.
type ArchivedSegment struct {
	capture.SegmentEntry
	Session  string   `json:"session"`            // archive directory: first record's session
	Sessions []string `json:"sessions,omitempty"` // every session with a record in the segment
	Path     string   `json:"path"`               // <session>/<file>, relative to the machine dir
}

// ArchiveManifest is one machine's manifest copy on the main server.
type ArchiveManifest struct {
	SchemaVersion int               `json:"schema_version"`
	Machine       string            `json:"machine"`
	Segments      []ArchivedSegment `json:"segments"`

	mu  sync.Mutex
	dir string
}

func loadArchiveManifest(dir, machine string) (*ArchiveManifest, error) {
	m := &ArchiveManifest{SchemaVersion: capture.SchemaVersion, Machine: machine, dir: dir}
	data, err := os.ReadFile(filepath.Join(dir, ArchiveManifestName))
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("evidence: read archive manifest: %w", err)
	}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("evidence: parse archive manifest: %w", err)
	}
	m.dir = dir
	m.Machine = machine
	return m, nil
}

// has reports whether the archive holds exactly this segment (name and digest).
func (m *ArchiveManifest) has(e capture.SegmentEntry) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.Segments {
		if s.File == e.File && s.SHA256 == e.SHA256 {
			return true
		}
	}
	return false
}

// put records a segment, replacing any earlier version of the same file (a
// changed segment), and persists the manifest atomically.
func (m *ArchiveManifest) put(s ArchivedSegment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	replaced := false
	for i := range m.Segments {
		if m.Segments[i].File == s.File {
			m.Segments[i] = s
			replaced = true
			break
		}
	}
	if !replaced {
		m.Segments = append(m.Segments, s)
	}
	sort.SliceStable(m.Segments, func(i, j int) bool {
		a, b := m.Segments[i], m.Segments[j]
		if !a.StartTime.Equal(b.StartTime) {
			return a.StartTime.Before(b.StartTime)
		}
		return a.File < b.File
	})
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("evidence: encode archive manifest: %w", err)
	}
	tmp, err := os.CreateTemp(m.dir, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("evidence: archive manifest temp: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("evidence: write archive manifest: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, filepath.Join(m.dir, ArchiveManifestName)); err != nil {
		os.Remove(name)
		return fmt.Errorf("evidence: replace archive manifest: %w", err)
	}
	return nil
}

// snapshot returns a copy of the segments.
func (m *ArchiveManifest) snapshot() []ArchivedSegment {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ArchivedSegment, len(m.Segments))
	copy(out, m.Segments)
	return out
}

// Diff returns the entries of remote that the local manifest lacks or holds
// with a different digest: the missing/changed segments, in remote order.
func Diff(local *ArchiveManifest, remote []capture.SegmentEntry) []capture.SegmentEntry {
	var need []capture.SegmentEntry
	for _, e := range remote {
		if !local.has(e) {
			need = append(need, e)
		}
	}
	return need
}

// safeComponent reports whether s is usable as one path component.
func safeComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, "/\\\x00")
}
