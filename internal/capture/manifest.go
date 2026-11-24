package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ManifestName is the manifest filename inside each machine's evidence dir.
const ManifestName = "manifest.json"

// CompressionNone is the only compression currently written. The zstd flag is
// reserved (off) so a later change can compress segments without a format break
// (§7.1).
const CompressionNone = "none"

// SegmentEntry describes one finalized segment file in the manifest (§7.1).
type SegmentEntry struct {
	File          string    `json:"file"`           // basename, relative to the machine dir
	SHA256        string    `json:"sha256"`         // hex SHA-256 of the segment bytes
	Bytes         int64     `json:"bytes"`          // segment size on disk
	Records       int       `json:"records"`        // records in the segment
	Dropped       uint64    `json:"dropped"`        // best-effort dropped-packet count (§6.3b)
	StartTime     time.Time `json:"start_time"`     // first record timestamp
	EndTime       time.Time `json:"end_time"`       // last record timestamp
	SchemaVersion int       `json:"schema_version"` // record/segment schema
	Compression   string    `json:"compression"`    // "none" (zstd reserved, off)
}

// Manifest is one machine's evidence index (§7.1). It is persisted as JSON in
// the machine's evidence directory and is safe for concurrent use.
type Manifest struct {
	SchemaVersion int            `json:"schema_version"`
	Machine       string         `json:"machine"`
	Segments      []SegmentEntry `json:"segments"`

	mu  sync.Mutex `json:"-"`
	dir string     // machine evidence dir; "" for an in-memory manifest
}

// LoadManifest reads the manifest in dir, or returns a fresh one bound to dir
// when none exists yet. dir is the machine evidence directory.
func LoadManifest(dir, machine string) (*Manifest, error) {
	m := &Manifest{SchemaVersion: SchemaVersion, Machine: machine, dir: dir}
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("capture: read manifest: %w", err)
	}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("capture: parse manifest: %w", err)
	}
	m.dir = dir
	if m.Machine == "" {
		m.Machine = machine
	}
	return m, nil
}

// Add appends an entry and persists the manifest atomically.
func (m *Manifest) Add(e SegmentEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Segments = append(m.Segments, e)
	return m.saveLocked()
}

// Entries returns a copy of the current segment entries.
func (m *Manifest) Entries() []SegmentEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SegmentEntry, len(m.Segments))
	copy(out, m.Segments)
	return out
}

// saveLocked writes the manifest via a temp file + rename (atomic replace).
func (m *Manifest) saveLocked() error {
	if m.dir == "" {
		return nil
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("capture: encode manifest: %w", err)
	}
	tmp, err := os.CreateTemp(m.dir, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("capture: manifest temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("capture: write manifest: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("capture: sync manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(m.dir, ManifestName)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("capture: replace manifest: %w", err)
	}
	return nil
}

// Verify re-reads every listed segment from dir and checks that its on-disk
// SHA-256 and byte size match the manifest. It returns an error naming the
// first segment that fails (missing, resized, or tampered).
func (m *Manifest) Verify() error {
	m.mu.Lock()
	entries := make([]SegmentEntry, len(m.Segments))
	copy(entries, m.Segments)
	dir := m.dir
	m.mu.Unlock()

	for _, e := range entries {
		path := filepath.Join(dir, e.File)
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("capture: verify %s: %w", e.File, err)
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("capture: verify %s: %w", e.File, err)
		}
		if n != e.Bytes {
			return fmt.Errorf("capture: verify %s: size %d != manifest %d", e.File, n, e.Bytes)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
			return fmt.Errorf("capture: verify %s: sha256 %s != manifest %s", e.File, got, e.SHA256)
		}
	}
	return nil
}
