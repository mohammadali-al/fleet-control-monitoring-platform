package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
)

const (
	// FileName is the registry file inside DataDir/registry/.
	FileName      = "machines.json"
	formatVersion = 1
)

type fileFormat struct {
	Version  int       `json:"version"`
	Machines []Machine `json:"machines"`
}

// Store persists a Registry as JSON under <DataDir>/registry/ (D-0006). Writes
// are atomic (temp file in the same directory + fsync + rename), so a crash
// or error never leaves a partial file.
type Store struct {
	dir string
	// rename is replaceable in tests to simulate a failure at the last step.
	rename func(oldpath, newpath string) error
}

// NewStore returns a store rooted at <dataDir>/registry. Nothing is created
// until Save.
func NewStore(dataDir string) *Store {
	return &Store{dir: filepath.Join(dataDir, config.RegistryDirName), rename: os.Rename}
}

// Dir is the registry directory.
func (s *Store) Dir() string { return s.dir }

// Path is the registry file.
func (s *Store) Path() string { return filepath.Join(s.dir, FileName) }

// Save writes the registry atomically. Machines are sorted by ID so the file
// is stable. Online state is not persisted.
func (s *Store) Save(r *Registry) error {
	data, err := json.MarshalIndent(fileFormat{Version: formatVersion, Machines: r.List()}, "", "  ")
	if err != nil {
		return fmt.Errorf("registry: encode: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("registry: create %s: %w", s.dir, err)
	}
	tmp, err := os.CreateTemp(s.dir, FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("registry: temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("registry: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("registry: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("registry: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("registry: close temp file: %w", err)
	}
	if err := s.rename(tmpName, s.Path()); err != nil {
		cleanup()
		return fmt.Errorf("registry: replace %s: %w", s.Path(), err)
	}
	if d, err := os.Open(s.dir); err == nil { // best effort: persist the rename
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Load reads the registry file. A missing file yields an empty registry. A
// corrupt file, an invalid machine, or duplicate IDs are errors.
func (s *Store) Load() (*Registry, error) {
	r := New()
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry: read %s: %w", s.Path(), err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("registry: parse %s: %w", s.Path(), err)
	}
	if f.Version != formatVersion {
		return nil, fmt.Errorf("registry: %s has unsupported version %d", s.Path(), f.Version)
	}
	if err := Validate(f.Machines); err != nil {
		return nil, fmt.Errorf("registry: %s: %w", s.Path(), err)
	}
	r.replaceAll(f.Machines)
	return r, nil
}
