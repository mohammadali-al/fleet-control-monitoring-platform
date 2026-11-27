package baseline

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
)

const (
	// FileName is the baseline library file inside <DataDir>/baseline/.
	FileName      = "items.json"
	formatVersion = 1
)

// Item is a reusable command preset stored in the global baseline library.
// Maneuvers reference or copy these items and fill in target/runtime inputs.
type Item struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	CommandTemplate string            `json:"command_template"`
	Placeholders    []string          `json:"placeholders,omitempty"`
	Defaults        map[string]string `json:"defaults,omitempty"`
	WarmupSec       float64           `json:"warmup_sec,omitempty"`
	Tags            []string          `json:"tags,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	CreatedBy       string            `json:"created_by,omitempty"`
	UpdatedBy       string            `json:"updated_by,omitempty"`
}

// ItemSpec holds the mutable fields used to create or update a baseline item.
type ItemSpec struct {
	ID              string            `json:"id,omitempty"`
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	CommandTemplate string            `json:"command_template"`
	Defaults        map[string]string `json:"defaults,omitempty"`
	WarmupSec       float64           `json:"warmup_sec,omitempty"`
	Tags            []string          `json:"tags,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

type fileFormat struct {
	Version int    `json:"version"`
	Items   []Item `json:"items"`
}

// Store persists the global library of baseline items as JSON under
// <DataDir>/baseline/items.json (D-0006). All methods are safe for concurrent
// use; writes are atomic (temp file + fsync + rename).
type Store struct {
	dir    string
	now    func() time.Time
	rename func(oldpath, newpath string) error

	mu    sync.RWMutex
	byID  map[string]Item
	order []string // IDs sorted by Name (case-insensitive), then ID
}

// Open opens the baseline library under dataDir, reloading any persisted items
// from <dataDir>/baseline/items.json. A missing file starts an empty library.
// A corrupt file, unsupported format version, or invalid item returns an error.
func Open(dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("baseline: data directory is required")
	}
	s := &Store{
		dir:    filepath.Join(dataDir, config.BaselineDirName),
		now:    time.Now,
		rename: os.Rename,
		byID:   map[string]Item{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir returns the baseline storage directory (<dataDir>/baseline).
func (s *Store) Dir() string { return s.dir }

// Path returns the persisted library file (<dataDir>/baseline/items.json).
func (s *Store) Path() string { return filepath.Join(s.dir, FileName) }

func (s *Store) load() error {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("baseline: read %s: %w", s.Path(), err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("baseline: parse %s: %w", s.Path(), err)
	}
	if f.Version != formatVersion {
		return fmt.Errorf("baseline: %s has unsupported version %d", s.Path(), f.Version)
	}
	seen := make(map[string]bool, len(f.Items))
	loaded := make(map[string]Item, len(f.Items))
	for i, raw := range f.Items {
		item, err := normalizeLoadedItem(raw)
		if err != nil {
			return fmt.Errorf("baseline: %s item %d (%q): %w", s.Path(), i, raw.ID, err)
		}
		if seen[item.ID] {
			return fmt.Errorf("baseline: %s has duplicate item id %q", s.Path(), item.ID)
		}
		seen[item.ID] = true
		loaded[item.ID] = item
	}
	s.byID = loaded
	s.rebuildOrderLocked()
	return nil
}

// List returns a copy of all baseline items sorted by name (then ID).
func (s *Store) List() []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Item, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, cloneItem(s.byID[id]))
	}
	return out
}

// Get returns a copy of the baseline item with the given ID.
func (s *Store) Get(id string) (Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.byID[id]
	if !ok {
		return Item{}, false
	}
	return cloneItem(it), true
}

// Create validates spec, assigns an ID if omitted, persists the new item, and
// returns it.
func (s *Store) Create(spec ItemSpec, actor string) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	norm, placeholders, err := validateSpec(spec, true)
	if err != nil {
		return Item{}, err
	}
	id := norm.ID
	if id == "" {
		id = s.uniqueIDLocked(norm.Name)
	} else if _, exists := s.byID[id]; exists {
		return Item{}, fmt.Errorf("%w: %s", ErrAlreadyExists, id)
	}

	now := s.now().UTC()
	actor = strings.TrimSpace(actor)
	item := Item{
		ID:              id,
		Name:            norm.Name,
		Description:     norm.Description,
		CommandTemplate: norm.CommandTemplate,
		Placeholders:    placeholders,
		Defaults:        norm.Defaults,
		WarmupSec:       norm.WarmupSec,
		Tags:            norm.Tags,
		Metadata:        norm.Metadata,
		CreatedAt:       now,
		UpdatedAt:       now,
		CreatedBy:       actor,
		UpdatedBy:       actor,
	}
	if err := s.saveWithMutationLocked(func() {
		s.byID[id] = item
	}); err != nil {
		return Item{}, err
	}
	return cloneItem(item), nil
}

// Update replaces the mutable fields of the item identified by id and persists
// the updated library.
func (s *Store) Update(id string, spec ItemSpec, actor string) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev, ok := s.byID[id]
	if !ok {
		return Item{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if spec.ID != "" && strings.TrimSpace(spec.ID) != id {
		return Item{}, fmt.Errorf("%w: cannot change item id from %q to %q", ErrInvalidItem, id, spec.ID)
	}
	norm, placeholders, err := validateSpec(spec, false)
	if err != nil {
		return Item{}, err
	}

	updated := Item{
		ID:              id,
		Name:            norm.Name,
		Description:     norm.Description,
		CommandTemplate: norm.CommandTemplate,
		Placeholders:    placeholders,
		Defaults:        norm.Defaults,
		WarmupSec:       norm.WarmupSec,
		Tags:            norm.Tags,
		Metadata:        norm.Metadata,
		CreatedAt:       prev.CreatedAt,
		UpdatedAt:       s.now().UTC(),
		CreatedBy:       prev.CreatedBy,
		UpdatedBy:       strings.TrimSpace(actor),
	}
	if err := s.saveWithMutationLocked(func() {
		s.byID[id] = updated
	}); err != nil {
		return Item{}, err
	}
	return cloneItem(updated), nil
}

// Duplicate clones an existing baseline item under a new unique ID and name
// (defaulting to "<name> (copy)" when newName is empty) and persists it.
func (s *Store) Duplicate(id, newName, actor string) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	orig, ok := s.byID[id]
	if !ok {
		return Item{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	name := strings.TrimSpace(newName)
	if name == "" {
		name = s.uniqueCopyNameLocked(orig.Name)
	}
	if len(name) > 120 {
		return Item{}, fmt.Errorf("%w: name exceeds 120 characters", ErrInvalidItem)
	}

	newID := s.uniqueIDLocked(name)
	now := s.now().UTC()
	actor = strings.TrimSpace(actor)
	copyItem := cloneItem(orig)
	copyItem.ID = newID
	copyItem.Name = name
	copyItem.CreatedAt = now
	copyItem.UpdatedAt = now
	copyItem.CreatedBy = actor
	copyItem.UpdatedBy = actor

	if err := s.saveWithMutationLocked(func() {
		s.byID[newID] = copyItem
	}); err != nil {
		return Item{}, err
	}
	return cloneItem(copyItem), nil
}

// Delete removes the item with the given ID and persists the updated library.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.byID[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return s.saveWithMutationLocked(func() {
		delete(s.byID, id)
	})
}

func (s *Store) saveWithMutationLocked(mutate func()) error {
	snapshot := make(map[string]Item, len(s.byID))
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
	// Persist in stable ID order so diffs are deterministic.
	ids := make([]string, 0, len(s.byID))
	for id := range s.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]Item, 0, len(ids))
	for _, id := range ids {
		items = append(items, s.byID[id])
	}
	data, err := json.MarshalIndent(fileFormat{Version: formatVersion, Items: items}, "", "  ")
	if err != nil {
		return fmt.Errorf("baseline: encode: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("baseline: create %s: %w", s.dir, err)
	}
	tmp, err := os.CreateTemp(s.dir, FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("baseline: temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("baseline: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("baseline: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("baseline: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("baseline: close temp file: %w", err)
	}
	if err := s.rename(tmpName, s.Path()); err != nil {
		cleanup()
		return fmt.Errorf("baseline: replace %s: %w", s.Path(), err)
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
		base = "item"
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
	for _, it := range s.byID {
		names[strings.ToLower(it.Name)] = true
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

func validateSpec(spec ItemSpec, allowID bool) (ItemSpec, []string, error) {
	out := ItemSpec{
		ID:              strings.TrimSpace(spec.ID),
		Name:            strings.TrimSpace(spec.Name),
		Description:     strings.TrimSpace(spec.Description),
		CommandTemplate: strings.TrimSpace(spec.CommandTemplate),
		WarmupSec:       spec.WarmupSec,
	}
	if allowID && out.ID != "" && !isValidID(out.ID) {
		return ItemSpec{}, nil, fmt.Errorf("%w: invalid id %q (must start with alphanumeric and contain only letters, digits, '-' or '_')", ErrInvalidItem, out.ID)
	}
	if out.Name == "" {
		return ItemSpec{}, nil, fmt.Errorf("%w: name is required", ErrInvalidItem)
	}
	if len(out.Name) > 120 {
		return ItemSpec{}, nil, fmt.Errorf("%w: name exceeds 120 characters", ErrInvalidItem)
	}
	if math.IsNaN(out.WarmupSec) || math.IsInf(out.WarmupSec, 0) || out.WarmupSec < 0 {
		return ItemSpec{}, nil, fmt.Errorf("%w: warmup_sec must be >= 0", ErrInvalidItem)
	}
	placeholders, err := ValidateTemplate(out.CommandTemplate)
	if err != nil {
		return ItemSpec{}, nil, err
	}
	if len(spec.Defaults) > 0 {
		defs := make(map[string]string, len(spec.Defaults))
		for k, v := range spec.Defaults {
			key := strings.TrimSpace(k)
			if !IsPlaceholderName(key) {
				return ItemSpec{}, nil, fmt.Errorf("%w: invalid default placeholder key %q", ErrInvalidItem, k)
			}
			if strings.IndexByte(v, 0) >= 0 {
				return ItemSpec{}, nil, fmt.Errorf("%w: default value for %q contains NUL byte", ErrInvalidItem, key)
			}
			if v != "" {
				defs[key] = v
			}
		}
		if len(defs) > 0 {
			out.Defaults = defs
		}
	}
	if len(spec.Tags) > 0 {
		seen := map[string]bool{}
		for _, t := range spec.Tags {
			tag := strings.TrimSpace(t)
			if tag == "" || seen[tag] {
				continue
			}
			seen[tag] = true
			out.Tags = append(out.Tags, tag)
		}
	}
	if len(spec.Metadata) > 0 {
		meta := make(map[string]string, len(spec.Metadata))
		for k, v := range spec.Metadata {
			key := strings.TrimSpace(k)
			if key == "" {
				return ItemSpec{}, nil, fmt.Errorf("%w: metadata key cannot be empty", ErrInvalidItem)
			}
			meta[key] = v
		}
		if len(meta) > 0 {
			out.Metadata = meta
		}
	}
	return out, placeholders, nil
}

func normalizeLoadedItem(raw Item) (Item, error) {
	if !isValidID(raw.ID) {
		return Item{}, fmt.Errorf("%w: invalid id %q", ErrInvalidItem, raw.ID)
	}
	norm, placeholders, err := validateSpec(ItemSpec{
		ID:              raw.ID,
		Name:            raw.Name,
		Description:     raw.Description,
		CommandTemplate: raw.CommandTemplate,
		Defaults:        raw.Defaults,
		WarmupSec:       raw.WarmupSec,
		Tags:            raw.Tags,
		Metadata:        raw.Metadata,
	}, true)
	if err != nil {
		return Item{}, err
	}
	return Item{
		ID:              raw.ID,
		Name:            norm.Name,
		Description:     norm.Description,
		CommandTemplate: norm.CommandTemplate,
		Placeholders:    placeholders,
		Defaults:        norm.Defaults,
		WarmupSec:       norm.WarmupSec,
		Tags:            norm.Tags,
		Metadata:        norm.Metadata,
		CreatedAt:       raw.CreatedAt,
		UpdatedAt:       raw.UpdatedAt,
		CreatedBy:       raw.CreatedBy,
		UpdatedBy:       raw.UpdatedBy,
	}, nil
}

func cloneItem(it Item) Item {
	out := it
	if it.Placeholders != nil {
		out.Placeholders = append([]string(nil), it.Placeholders...)
	}
	if it.Defaults != nil {
		out.Defaults = make(map[string]string, len(it.Defaults))
		for k, v := range it.Defaults {
			out.Defaults[k] = v
		}
	}
	if it.Tags != nil {
		out.Tags = append([]string(nil), it.Tags...)
	}
	if it.Metadata != nil {
		out.Metadata = make(map[string]string, len(it.Metadata))
		for k, v := range it.Metadata {
			out.Metadata[k] = v
		}
	}
	return out
}
