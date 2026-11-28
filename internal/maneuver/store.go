package maneuver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
)

const formatVersion = 1

type fileFormat struct {
	Version   int        `json:"version"`
	Maneuvers []Maneuver `json:"maneuvers"`
}

// Store persists Maneuver definitions under <dataDir>/projects/maneuvers.json.
// All methods are safe for concurrent use; writes are atomic (temp + fsync + rename).
type Store struct {
	dir    string
	now    func() time.Time
	rename func(oldpath, newpath string) error

	mu    sync.RWMutex
	byID  map[string]Maneuver
	order []string
}

// Open opens the Maneuver store under dataDir, reloading any persisted plans.
func Open(dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("maneuver: data directory is required")
	}
	s := &Store{
		dir:    filepath.Join(dataDir, config.ProjectsDirName),
		now:    time.Now,
		rename: os.Rename,
		byID:   map[string]Maneuver{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path returns the persisted Maneuvers JSON file path.
func (s *Store) Path() string { return filepath.Join(s.dir, config.ManeuversFileName) }

func (s *Store) load() error {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("maneuver: read %s: %w", s.Path(), err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("maneuver: parse %s: %w", s.Path(), err)
	}
	if f.Version != formatVersion {
		return fmt.Errorf("maneuver: %s has unsupported version %d", s.Path(), f.Version)
	}
	seen := make(map[string]bool, len(f.Maneuvers))
	loaded := make(map[string]Maneuver, len(f.Maneuvers))
	for i, raw := range f.Maneuvers {
		if !isValidID(raw.ID) {
			return fmt.Errorf("maneuver: %s item %d (%q): invalid id", s.Path(), i, raw.ID)
		}
		if seen[raw.ID] {
			return fmt.Errorf("maneuver: %s duplicate id %q", s.Path(), raw.ID)
		}
		norm, err := ValidateSpec(Spec{
			ID:          raw.ID,
			Name:        raw.Name,
			Description: raw.Description,
			Target:      raw.Target,
			TotalSec:    raw.TotalSec,
			Pattern:     raw.Pattern,
			Timeline:    raw.Timeline,
			Agents:      raw.Agents,
		}, true)
		if err != nil {
			return fmt.Errorf("maneuver: %s item %d (%q): %w", s.Path(), i, raw.ID, err)
		}
		seen[raw.ID] = true
		loaded[raw.ID] = Maneuver{
			ID:          raw.ID,
			Name:        norm.Name,
			Description: norm.Description,
			Target:      norm.Target,
			TotalSec:    norm.TotalSec,
			Pattern:     norm.Pattern,
			Timeline:    norm.Timeline,
			Agents:      norm.Agents,
			CreatedAt:   raw.CreatedAt,
			UpdatedAt:   raw.UpdatedAt,
			CreatedBy:   raw.CreatedBy,
			UpdatedBy:   raw.UpdatedBy,
		}
	}
	s.byID = loaded
	s.rebuildOrderLocked()
	return nil
}

// List returns a copy of all Maneuvers sorted by name (case-insensitive) then ID.
func (s *Store) List() []Maneuver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Maneuver, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, cloneManeuver(s.byID[id]))
	}
	return out
}

// Get returns a copy of the Maneuver with id.
func (s *Store) Get(id string) (Maneuver, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.byID[id]
	if !ok {
		return Maneuver{}, false
	}
	return cloneManeuver(m), true
}

// Create validates spec, assigns an ID if omitted, persists, and returns it.
func (s *Store) Create(spec Spec, actor string) (Maneuver, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	norm, err := ValidateSpec(spec, true)
	if err != nil {
		return Maneuver{}, err
	}
	id := norm.ID
	if id == "" {
		id = s.uniqueIDLocked(norm.Name)
	} else if _, exists := s.byID[id]; exists {
		return Maneuver{}, fmt.Errorf("%w: %s", ErrAlreadyExists, id)
	}
	now := s.now().UTC()
	actor = strings.TrimSpace(actor)
	m := Maneuver{
		ID:          id,
		Name:        norm.Name,
		Description: norm.Description,
		Target:      norm.Target,
		TotalSec:    norm.TotalSec,
		Pattern:     norm.Pattern,
		Timeline:    norm.Timeline,
		Agents:      norm.Agents,
		CreatedAt:   now,
		UpdatedAt:   now,
		CreatedBy:   actor,
		UpdatedBy:   actor,
	}
	if err := s.saveWithMutationLocked(func() { s.byID[id] = m }); err != nil {
		return Maneuver{}, err
	}
	return cloneManeuver(m), nil
}

// Update replaces the mutable fields of the Maneuver identified by id.
func (s *Store) Update(id string, spec Spec, actor string) (Maneuver, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev, ok := s.byID[id]
	if !ok {
		return Maneuver{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if spec.ID != "" && strings.TrimSpace(spec.ID) != id {
		return Maneuver{}, fmt.Errorf("%w: cannot change maneuver id from %q to %q", ErrInvalidManeuver, id, spec.ID)
	}
	norm, err := ValidateSpec(spec, false)
	if err != nil {
		return Maneuver{}, err
	}
	updated := Maneuver{
		ID:          id,
		Name:        norm.Name,
		Description: norm.Description,
		Target:      norm.Target,
		TotalSec:    norm.TotalSec,
		Pattern:     norm.Pattern,
		Timeline:    norm.Timeline,
		Agents:      norm.Agents,
		CreatedAt:   prev.CreatedAt,
		UpdatedAt:   s.now().UTC(),
		CreatedBy:   prev.CreatedBy,
		UpdatedBy:   strings.TrimSpace(actor),
	}
	if err := s.saveWithMutationLocked(func() { s.byID[id] = updated }); err != nil {
		return Maneuver{}, err
	}
	return cloneManeuver(updated), nil
}

// Duplicate clones an existing Maneuver under a new unique ID and name.
func (s *Store) Duplicate(id, newName, actor string) (Maneuver, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	orig, ok := s.byID[id]
	if !ok {
		return Maneuver{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	name := strings.TrimSpace(newName)
	if name == "" {
		name = s.uniqueCopyNameLocked(orig.Name)
	}
	if len(name) > 120 {
		return Maneuver{}, fmt.Errorf("%w: name exceeds 120 characters", ErrInvalidManeuver)
	}
	newID := s.uniqueIDLocked(name)
	now := s.now().UTC()
	actor = strings.TrimSpace(actor)
	cp := cloneManeuver(orig)
	cp.ID = newID
	cp.Name = name
	cp.CreatedAt = now
	cp.UpdatedAt = now
	cp.CreatedBy = actor
	cp.UpdatedBy = actor

	if err := s.saveWithMutationLocked(func() { s.byID[newID] = cp }); err != nil {
		return Maneuver{}, err
	}
	return cloneManeuver(cp), nil
}

// Delete removes the Maneuver identified by id.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return s.saveWithMutationLocked(func() { delete(s.byID, id) })
}

func (s *Store) saveWithMutationLocked(mutate func()) error {
	snapshot := make(map[string]Maneuver, len(s.byID))
	for k, v := range s.byID {
		snapshot[k] = v
	}
	mutate()
	s.rebuildOrderLocked()
	if err := s.saveLocked(); err != nil {
		s.byID = snapshot
		s.rebuildOrderLocked()
		return err
	}
	return nil
}

func (s *Store) rebuildOrderLocked() {
	order := make([]string, 0, len(s.byID))
	for id := range s.byID {
		order = append(order, id)
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := s.byID[order[i]], s.byID[order[j]]
		la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if la != lb {
			return la < lb
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	s.order = order
}

func (s *Store) saveLocked() error {
	ids := make([]string, 0, len(s.byID))
	for id := range s.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]Maneuver, 0, len(ids))
	for _, id := range ids {
		items = append(items, s.byID[id])
	}
	data, err := json.MarshalIndent(fileFormat{Version: formatVersion, Maneuvers: items}, "", "  ")
	if err != nil {
		return fmt.Errorf("maneuver: encode: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("maneuver: create %s: %w", s.dir, err)
	}
	tmp, err := os.CreateTemp(s.dir, config.ManeuversFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("maneuver: temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("maneuver: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("maneuver: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("maneuver: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("maneuver: close temp file: %w", err)
	}
	if err := s.rename(tmpName, s.Path()); err != nil {
		cleanup()
		return fmt.Errorf("maneuver: replace %s: %w", s.Path(), err)
	}
	if d, err := os.Open(s.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *Store) uniqueIDLocked(name string) string {
	base := slugify(name)
	if base == "" {
		base = "maneuver"
	}
	if _, exists := s.byID[base]; !exists {
		return base
	}
	for n := 2; ; n++ {
		cand := fmt.Sprintf("%s-%d", base, n)
		if _, exists := s.byID[cand]; !exists {
			return cand
		}
	}
}

func (s *Store) uniqueCopyNameLocked(origName string) string {
	names := make(map[string]bool, len(s.byID))
	for _, m := range s.byID {
		names[strings.ToLower(m.Name)] = true
	}
	cand := origName + " (copy)"
	if !names[strings.ToLower(cand)] {
		return cand
	}
	for n := 2; ; n++ {
		c := fmt.Sprintf("%s (copy %d)", origName, n)
		if !names[strings.ToLower(c)] {
			return c
		}
	}
}

func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash && b.Len() > 0:
			b.WriteByte('-')
			prevDash = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func isValidID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if i == 0 {
			if !isAlnum {
				return false
			}
			continue
		}
		if !isAlnum && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func cloneManeuver(m Maneuver) Maneuver {
	out := m
	if m.Agents != nil {
		out.Agents = append([]string(nil), m.Agents...)
	}
	if m.Timeline != nil {
		out.Timeline = make([]TimelineItem, len(m.Timeline))
		for i, ti := range m.Timeline {
			cp := ti
			if ti.Inputs != nil {
				cp.Inputs = make(map[string]string, len(ti.Inputs))
				for k, v := range ti.Inputs {
					cp.Inputs[k] = v
				}
			}
			out.Timeline[i] = cp
		}
	}
	return out
}
