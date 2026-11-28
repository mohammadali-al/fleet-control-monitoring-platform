package project

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
	Version    int             `json:"version"`
	ActiveLock *ActiveTestLock `json:"active_lock,omitempty"`
	Projects   []Project       `json:"projects"`
}

// StartParams carries the runtime bindings recorded when a Test acquires the
// global active-test lock and starts.
type StartParams struct {
	ManeuverName string
	Target       TargetRef
	Agents       []string
	Monitor      MonitorRef
	Actor        string
	StartedAt    time.Time
}

// Store persists projects, their tests, and the system-wide active-test lock
// under <dataDir>/projects/projects.json. All methods are safe for concurrent
// use; writes are atomic (temp file + fsync + rename).
type Store struct {
	dir    string
	now    func() time.Time
	rename func(oldpath, newpath string) error

	mu     sync.RWMutex
	byID   map[string]Project
	order  []string
	active *ActiveTestLock
}

// Open opens the project store under dataDir, reloading persisted projects and
// any active-test lock from <dataDir>/projects/projects.json.
func Open(dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("project: data directory is required")
	}
	s := &Store{
		dir:    filepath.Join(dataDir, config.ProjectsDirName),
		now:    time.Now,
		rename: os.Rename,
		byID:   map[string]Project{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path returns the persisted projects JSON file path.
func (s *Store) Path() string { return filepath.Join(s.dir, config.ProjectsFileName) }

// SetNowForTest overrides the clock used by Store (for deterministic unit tests).
func (s *Store) SetNowForTest(fn func() time.Time) {
	s.mu.Lock()
	if fn == nil {
		fn = time.Now
	}
	s.now = fn
	s.mu.Unlock()
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("project: read %s: %w", s.Path(), err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("project: parse %s: %w", s.Path(), err)
	}
	if f.Version != formatVersion {
		return fmt.Errorf("project: %s has unsupported version %d", s.Path(), f.Version)
	}
	seenProj := make(map[string]bool, len(f.Projects))
	loaded := make(map[string]Project, len(f.Projects))
	var activeFromTests *ActiveTestLock
	for i, raw := range f.Projects {
		if !isValidID(raw.ID) {
			return fmt.Errorf("project: %s item %d (%q): invalid id", s.Path(), i, raw.ID)
		}
		if seenProj[raw.ID] {
			return fmt.Errorf("project: %s duplicate id %q", s.Path(), raw.ID)
		}
		norm, err := ValidateProjectSpec(ProjectSpec{
			ID:          raw.ID,
			Name:        raw.Name,
			Description: raw.Description,
			Metadata:    raw.Metadata,
		}, true)
		if err != nil {
			return fmt.Errorf("project: %s item %d (%q): %w", s.Path(), i, raw.ID, err)
		}
		seenTests := make(map[string]bool, len(raw.Tests))
		tests := make([]Test, 0, len(raw.Tests))
		for ti, rt := range raw.Tests {
			if !isValidID(rt.ID) {
				return fmt.Errorf("project: %s project %q test %d (%q): invalid id", s.Path(), raw.ID, ti, rt.ID)
			}
			if seenTests[rt.ID] {
				return fmt.Errorf("project: %s project %q duplicate test id %q", s.Path(), raw.ID, rt.ID)
			}
			if strings.TrimSpace(rt.ManeuverID) == "" {
				return fmt.Errorf("project: %s project %q test %q missing maneuver_id", s.Path(), raw.ID, rt.ID)
			}
			if !IsValidStatus(rt.Status) {
				return fmt.Errorf("project: %s project %q test %q has invalid status %q", s.Path(), raw.ID, rt.ID, rt.Status)
			}
			seenTests[rt.ID] = true
			cp := cloneTest(rt)
			cp.ProjectID = raw.ID
			if cp.Status == StatusRunning && activeFromTests == nil {
				acq := cp.CreatedAt
				if cp.StartedAt != nil {
					acq = *cp.StartedAt
				}
				activeFromTests = &ActiveTestLock{
					ProjectID:     raw.ID,
					ProjectName:   norm.Name,
					TestID:        cp.ID,
					TestName:      cp.Name,
					ManeuverID:    cp.ManeuverID,
					ManeuverRunID: cp.ManeuverRunID,
					MonitorJobID:  cp.Monitor.JobID,
					StartedBy:     cp.StartedBy,
					AcquiredAt:    acq,
				}
			}
			tests = append(tests, cp)
		}
		seenProj[raw.ID] = true
		loaded[raw.ID] = Project{
			ID:          raw.ID,
			Name:        norm.Name,
			Description: norm.Description,
			Metadata:    norm.Metadata,
			Tests:       tests,
			CreatedAt:   raw.CreatedAt,
			UpdatedAt:   raw.UpdatedAt,
			CreatedBy:   raw.CreatedBy,
			UpdatedBy:   raw.UpdatedBy,
		}
	}

	var active *ActiveTestLock
	if f.ActiveLock != nil {
		if p, ok := loaded[f.ActiveLock.ProjectID]; ok {
			if idx := findTestIndex(p.Tests, f.ActiveLock.TestID); idx >= 0 && p.Tests[idx].Status == StatusRunning {
				cp := *f.ActiveLock
				active = &cp
			}
		}
	}
	if active == nil && activeFromTests != nil {
		active = activeFromTests
	}

	s.byID = loaded
	s.active = active
	s.rebuildOrderLocked()
	return nil
}

// List returns copies of all Projects sorted by name (case-insensitive) then ID.
func (s *Store) List() []Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Project, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, cloneProject(s.byID[id]))
	}
	return out
}

// Get returns a copy of the Project with id.
func (s *Store) Get(id string) (Project, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	if !ok {
		return Project{}, false
	}
	return cloneProject(p), true
}

// ActiveLock returns a copy of the active-test lock, if a test is running.
func (s *Store) ActiveLock() (*ActiveTestLock, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return nil, false
	}
	cp := *s.active
	return &cp, true
}

// ActiveTest returns the active lock, the owning Project, and the active Test.
func (s *Store) ActiveTest() (*ActiveTestLock, Project, Test, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return nil, Project{}, Test{}, false
	}
	p, ok := s.byID[s.active.ProjectID]
	if !ok {
		return nil, Project{}, Test{}, false
	}
	idx := findTestIndex(p.Tests, s.active.TestID)
	if idx < 0 {
		return nil, Project{}, Test{}, false
	}
	cp := *s.active
	return &cp, cloneProject(p), cloneTest(p.Tests[idx]), true
}

// CreateProject validates spec, persists a new empty Project, and returns it.
func (s *Store) CreateProject(spec ProjectSpec, actor string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	norm, err := ValidateProjectSpec(spec, true)
	if err != nil {
		return Project{}, err
	}
	id := norm.ID
	if id == "" {
		id = s.uniqueProjectIDLocked(norm.Name)
	} else if _, exists := s.byID[id]; exists {
		return Project{}, fmt.Errorf("%w: %s", ErrAlreadyExists, id)
	}
	now := s.now().UTC()
	actor = strings.TrimSpace(actor)
	p := Project{
		ID:          id,
		Name:        norm.Name,
		Description: norm.Description,
		Metadata:    cloneStringMap(norm.Metadata),
		Tests:       []Test{},
		CreatedAt:   now,
		UpdatedAt:   now,
		CreatedBy:   actor,
		UpdatedBy:   actor,
	}
	if err := s.saveWithMutationLocked(func() { s.byID[id] = p }); err != nil {
		return Project{}, err
	}
	return cloneProject(p), nil
}

// UpdateProject updates the mutable metadata of Project id while preserving its tests.
func (s *Store) UpdateProject(id string, spec ProjectSpec, actor string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev, ok := s.byID[id]
	if !ok {
		return Project{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if spec.ID != "" && strings.TrimSpace(spec.ID) != id {
		return Project{}, fmt.Errorf("%w: cannot change project id from %q to %q", ErrInvalidProject, id, spec.ID)
	}
	norm, err := ValidateProjectSpec(spec, false)
	if err != nil {
		return Project{}, err
	}
	updated := cloneProject(prev)
	updated.Name = norm.Name
	updated.Description = norm.Description
	updated.Metadata = cloneStringMap(norm.Metadata)
	updated.UpdatedAt = s.now().UTC()
	updated.UpdatedBy = strings.TrimSpace(actor)

	if err := s.saveWithMutationLocked(func() {
		s.byID[id] = updated
		if s.active != nil && s.active.ProjectID == id {
			s.active.ProjectName = updated.Name
		}
	}); err != nil {
		return Project{}, err
	}
	return cloneProject(updated), nil
}

// DeleteProject removes Project id unless its test holds the active-test lock.
func (s *Store) DeleteProject(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.byID[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if s.active != nil && s.active.ProjectID == id {
		return &ActiveTestError{Active: *s.active}
	}
	return s.saveWithMutationLocked(func() { delete(s.byID, id) })
}

// CreateTest adds a new planned Test inside projectID.
func (s *Store) CreateTest(projectID string, spec TestSpec, actor string) (Project, Test, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.byID[projectID]
	if !ok {
		return Project{}, Test{}, fmt.Errorf("%w: %s", ErrNotFound, projectID)
	}
	norm, err := ValidateTestSpec(spec, true)
	if err != nil {
		return Project{}, Test{}, err
	}
	name := norm.Name
	if name == "" {
		base := norm.ManeuverName
		if base == "" {
			base = norm.ManeuverID
		}
		name = fmt.Sprintf("%s #%d", base, len(p.Tests)+1)
	}
	id := norm.ID
	if id == "" {
		id = uniqueTestID(p.Tests, name)
	} else if findTestIndex(p.Tests, id) >= 0 {
		return Project{}, Test{}, fmt.Errorf("%w: test %s already exists in project %s", ErrAlreadyExists, id, projectID)
	}

	now := s.now().UTC()
	actor = strings.TrimSpace(actor)
	t := Test{
		ID:           id,
		ProjectID:    projectID,
		Name:         name,
		Description:  norm.Description,
		ManeuverID:   norm.ManeuverID,
		ManeuverName: norm.ManeuverName,
		Target:       norm.Target,
		Agents:       append([]string(nil), norm.Agents...),
		Monitor:      norm.Monitor,
		Status:       StatusPlanned,
		CreatedAt:    now,
		UpdatedAt:    now,
		CreatedBy:    actor,
	}

	updated := cloneProject(p)
	updated.Tests = append(updated.Tests, t)
	updated.UpdatedAt = now
	if actor != "" {
		updated.UpdatedBy = actor
	}
	if err := s.saveWithMutationLocked(func() { s.byID[projectID] = updated }); err != nil {
		return Project{}, Test{}, err
	}
	return cloneProject(updated), cloneTest(t), nil
}

// GetTest returns a copy of testID inside projectID.
func (s *Store) GetTest(projectID, testID string) (Test, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[projectID]
	if !ok {
		return Test{}, false
	}
	idx := findTestIndex(p.Tests, testID)
	if idx < 0 {
		return Test{}, false
	}
	return cloneTest(p.Tests[idx]), true
}

// DeleteTest removes testID from projectID unless it is currently active/running.
func (s *Store) DeleteTest(projectID, testID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.byID[projectID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, projectID)
	}
	idx := findTestIndex(p.Tests, testID)
	if idx < 0 {
		return fmt.Errorf("%w: %s", ErrTestNotFound, testID)
	}
	if s.active != nil && s.active.ProjectID == projectID && s.active.TestID == testID {
		return &ActiveTestError{Active: *s.active}
	}
	if p.Tests[idx].Status == StatusRunning {
		return fmt.Errorf("%w: test %s is running", ErrActiveTestConflict, testID)
	}

	updated := cloneProject(p)
	updated.Tests = append(updated.Tests[:idx], updated.Tests[idx+1:]...)
	updated.UpdatedAt = s.now().UTC()
	return s.saveWithMutationLocked(func() { s.byID[projectID] = updated })
}

// BeginStart atomically acquires the global single-active-test lock and
// transitions a planned test to running. If any test is already active anywhere
// in the system, *ActiveTestError (wrapping ErrActiveTestConflict) is returned.
func (s *Store) BeginStart(projectID, testID string, params StartParams) (Project, Test, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.byID[projectID]
	if !ok {
		return Project{}, Test{}, fmt.Errorf("%w: %s", ErrNotFound, projectID)
	}
	idx := findTestIndex(p.Tests, testID)
	if idx < 0 {
		return Project{}, Test{}, fmt.Errorf("%w: %s", ErrTestNotFound, testID)
	}
	if s.active != nil {
		return Project{}, Test{}, &ActiveTestError{Active: *s.active}
	}
	t := cloneTest(p.Tests[idx])
	if t.Status != StatusPlanned {
		return Project{}, Test{}, fmt.Errorf("%w: test %s is %s (must be planned to start)", ErrInvalidTransition, testID, t.Status)
	}

	startedAt := params.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = s.now().UTC()
	}
	actor := strings.TrimSpace(params.Actor)

	if params.ManeuverName != "" {
		t.ManeuverName = params.ManeuverName
	}
	if params.Target.IP != "" {
		t.Target = params.Target
	}
	if len(params.Agents) > 0 {
		t.Agents = append([]string(nil), params.Agents...)
	}
	if params.Monitor.JobID != "" {
		t.Monitor = params.Monitor
	}
	t.Status = StatusRunning
	t.Outcome = ""
	t.Error = ""
	t.StartedAt = &startedAt
	t.EndedAt = nil
	t.StartedBy = actor
	t.UpdatedAt = startedAt
	t.ResultPointers = &ResultPointers{
		Evidence: &EvidenceWindow{
			From:     startedAt,
			Machines: append([]string(nil), t.Agents...),
		},
		Monitor: &MonitorWindow{
			JobID:      t.Monitor.JobID,
			HistoryKey: t.Monitor.HistoryKey,
			From:       startedAt,
		},
	}

	updated := cloneProject(p)
	updated.Tests[idx] = t
	updated.UpdatedAt = startedAt
	if actor != "" {
		updated.UpdatedBy = actor
	}

	lock := &ActiveTestLock{
		ProjectID:    projectID,
		ProjectName:  updated.Name,
		TestID:       t.ID,
		TestName:     t.Name,
		ManeuverID:   t.ManeuverID,
		MonitorJobID: t.Monitor.JobID,
		StartedBy:    actor,
		AcquiredAt:   startedAt,
	}

	if err := s.saveWithMutationLocked(func() {
		s.byID[projectID] = updated
		s.active = lock
	}); err != nil {
		return Project{}, Test{}, err
	}
	return cloneProject(updated), cloneTest(t), nil
}

// AttachRun records the started Maneuver run ID on the currently running test.
func (s *Store) AttachRun(projectID, testID, runID string, agents []string) (Project, Test, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.byID[projectID]
	if !ok {
		return Project{}, Test{}, fmt.Errorf("%w: %s", ErrNotFound, projectID)
	}
	idx := findTestIndex(p.Tests, testID)
	if idx < 0 {
		return Project{}, Test{}, fmt.Errorf("%w: %s", ErrTestNotFound, testID)
	}
	t := cloneTest(p.Tests[idx])
	t.ManeuverRunID = strings.TrimSpace(runID)
	if len(agents) > 0 {
		t.Agents = append([]string(nil), agents...)
	}
	if t.ResultPointers == nil {
		t.ResultPointers = &ResultPointers{}
	}
	if t.ResultPointers.Evidence != nil {
		t.ResultPointers.Evidence.ManeuverRunID = t.ManeuverRunID
		if len(t.Agents) > 0 {
			t.ResultPointers.Evidence.Machines = append([]string(nil), t.Agents...)
		}
	}
	t.UpdatedAt = s.now().UTC()

	updated := cloneProject(p)
	updated.Tests[idx] = t

	if err := s.saveWithMutationLocked(func() {
		s.byID[projectID] = updated
		if s.active != nil && s.active.ProjectID == projectID && s.active.TestID == testID {
			s.active.ManeuverRunID = t.ManeuverRunID
		}
	}); err != nil {
		return Project{}, Test{}, err
	}
	return cloneProject(updated), cloneTest(t), nil
}

// AttachMonitor updates the monitor reference and monitor result pointer on a
// running test after a monitor job has been attached or created.
func (s *Store) AttachMonitor(projectID, testID string, mon MonitorRef) (Project, Test, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.byID[projectID]
	if !ok {
		return Project{}, Test{}, fmt.Errorf("%w: %s", ErrNotFound, projectID)
	}
	idx := findTestIndex(p.Tests, testID)
	if idx < 0 {
		return Project{}, Test{}, fmt.Errorf("%w: %s", ErrTestNotFound, testID)
	}
	t := cloneTest(p.Tests[idx])
	t.Monitor = mon
	if t.ResultPointers == nil {
		t.ResultPointers = &ResultPointers{}
	}
	if t.ResultPointers.Monitor == nil {
		from := t.UpdatedAt
		if t.StartedAt != nil {
			from = *t.StartedAt
		}
		t.ResultPointers.Monitor = &MonitorWindow{From: from}
	}
	t.ResultPointers.Monitor.JobID = mon.JobID
	t.ResultPointers.Monitor.HistoryKey = mon.HistoryKey
	t.UpdatedAt = s.now().UTC()

	updated := cloneProject(p)
	updated.Tests[idx] = t

	if err := s.saveWithMutationLocked(func() {
		s.byID[projectID] = updated
		if s.active != nil && s.active.ProjectID == projectID && s.active.TestID == testID {
			s.active.MonitorJobID = mon.JobID
		}
	}); err != nil {
		return Project{}, Test{}, err
	}
	return cloneProject(updated), cloneTest(t), nil
}

// RollbackStart reverts a test back to planned and releases the active-test lock
// if Maneuver startup fails before execution begins.
func (s *Store) RollbackStart(projectID, testID string, prevMonitor MonitorRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.byID[projectID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, projectID)
	}
	idx := findTestIndex(p.Tests, testID)
	if idx < 0 {
		return fmt.Errorf("%w: %s", ErrTestNotFound, testID)
	}
	t := cloneTest(p.Tests[idx])
	if t.Status != StatusRunning {
		return nil
	}
	t.Status = StatusPlanned
	t.Outcome = ""
	t.Error = ""
	t.ManeuverRunID = ""
	t.StartedAt = nil
	t.EndedAt = nil
	t.StartedBy = ""
	t.ResultPointers = nil
	t.Monitor = prevMonitor
	t.UpdatedAt = s.now().UTC()

	updated := cloneProject(p)
	updated.Tests[idx] = t

	return s.saveWithMutationLocked(func() {
		s.byID[projectID] = updated
		if s.active != nil && s.active.ProjectID == projectID && s.active.TestID == testID {
			s.active = nil
		}
	})
}

// FinishTest records completion or failure for a running test and releases the
// global active-test lock. Calling FinishTest on an already-finished test is
// idempotent (preserving timestamps and updating StoppedBy if newly supplied).
func (s *Store) FinishTest(projectID, testID, status, outcome, errMsg, actor string, endedAt time.Time) (Project, Test, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishTestLocked(projectID, testID, status, outcome, errMsg, actor, endedAt)
}

// FinishTestByRunID finalizes whichever running test is associated with runID.
// Returns (proj, test, true, nil) if a matching running test was finalized.
func (s *Store) FinishTestByRunID(runID, status, outcome, errMsg string, endedAt time.Time) (Project, Test, bool, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return Project{}, Test{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var targetProj, targetTest string
	if s.active != nil && s.active.ManeuverRunID == runID {
		targetProj, targetTest = s.active.ProjectID, s.active.TestID
	} else {
		for pid, p := range s.byID {
			for _, t := range p.Tests {
				if t.Status == StatusRunning && t.ManeuverRunID == runID {
					targetProj, targetTest = pid, t.ID
					break
				}
			}
			if targetProj != "" {
				break
			}
		}
	}
	if targetProj == "" {
		return Project{}, Test{}, false, nil
	}
	p, t, err := s.finishTestLocked(targetProj, targetTest, status, outcome, errMsg, "", endedAt)
	if err != nil {
		return Project{}, Test{}, false, err
	}
	return p, t, true, nil
}

func (s *Store) finishTestLocked(projectID, testID, status, outcome, errMsg, actor string, endedAt time.Time) (Project, Test, error) {
	p, ok := s.byID[projectID]
	if !ok {
		return Project{}, Test{}, fmt.Errorf("%w: %s", ErrNotFound, projectID)
	}
	idx := findTestIndex(p.Tests, testID)
	if idx < 0 {
		return Project{}, Test{}, fmt.Errorf("%w: %s", ErrTestNotFound, testID)
	}
	if status != StatusCompleted && status != StatusFailed {
		return Project{}, Test{}, fmt.Errorf("%w: finish status must be completed or failed, got %q", ErrInvalidTransition, status)
	}

	t := cloneTest(p.Tests[idx])
	actor = strings.TrimSpace(actor)
	if t.Status != StatusRunning {
		if !IsFinishedStatus(t.Status) {
			return Project{}, Test{}, fmt.Errorf("%w: test %s is %s (cannot finish)", ErrInvalidTransition, testID, t.Status)
		}
		if actor != "" && t.StoppedBy == "" {
			t.StoppedBy = actor
			updated := cloneProject(p)
			updated.Tests[idx] = t
			if err := s.saveWithMutationLocked(func() { s.byID[projectID] = updated }); err != nil {
				return Project{}, Test{}, err
			}
			return cloneProject(updated), cloneTest(t), nil
		}
		return cloneProject(p), t, nil
	}

	end := endedAt.UTC()
	if end.IsZero() {
		end = s.now().UTC()
	}
	t.Status = status
	t.Outcome = strings.TrimSpace(outcome)
	t.Error = strings.TrimSpace(errMsg)
	t.EndedAt = &end
	if actor != "" {
		t.StoppedBy = actor
	}
	t.UpdatedAt = end
	if t.ResultPointers == nil {
		t.ResultPointers = &ResultPointers{}
	}
	if t.ResultPointers.Evidence != nil {
		t.ResultPointers.Evidence.To = &end
	}
	if t.ResultPointers.Monitor != nil {
		t.ResultPointers.Monitor.To = &end
	}

	updated := cloneProject(p)
	updated.Tests[idx] = t
	updated.UpdatedAt = end
	if actor != "" {
		updated.UpdatedBy = actor
	}

	if err := s.saveWithMutationLocked(func() {
		s.byID[projectID] = updated
		if s.active != nil && s.active.ProjectID == projectID && s.active.TestID == testID {
			s.active = nil
		}
	}); err != nil {
		return Project{}, Test{}, err
	}
	return cloneProject(updated), cloneTest(t), nil
}

// CompletedByDate groups finished tests (completed or failed) of a project by
// UTC date (YYYY-MM-DD), newest date first and newest test first within a date.
func CompletedByDate(tests []Test) []DateGroup {
	byDate := map[string][]Test{}
	for _, t := range tests {
		if !IsFinishedStatus(t.Status) {
			continue
		}
		refTime := t.UpdatedAt
		if t.EndedAt != nil {
			refTime = *t.EndedAt
		} else if t.StartedAt != nil {
			refTime = *t.StartedAt
		}
		day := refTime.UTC().Format("2006-01-02")
		byDate[day] = append(byDate[day], cloneTest(t))
	}
	dates := make([]string, 0, len(byDate))
	for d := range byDate {
		dates = append(dates, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))

	groups := make([]DateGroup, 0, len(dates))
	for _, d := range dates {
		list := byDate[d]
		sort.Slice(list, func(i, j int) bool {
			ti, tj := testSortTime(list[i]), testSortTime(list[j])
			if !ti.Equal(tj) {
				return ti.After(tj)
			}
			return list[i].ID < list[j].ID
		})
		groups = append(groups, DateGroup{Date: d, Tests: list})
	}
	return groups
}

func testSortTime(t Test) time.Time {
	if t.EndedAt != nil {
		return *t.EndedAt
	}
	if t.StartedAt != nil {
		return *t.StartedAt
	}
	return t.CreatedAt
}

func (s *Store) saveWithMutationLocked(mutate func()) error {
	snapshot := make(map[string]Project, len(s.byID))
	for k, v := range s.byID {
		snapshot[k] = v
	}
	var activeSnap *ActiveTestLock
	if s.active != nil {
		cp := *s.active
		activeSnap = &cp
	}
	mutate()
	s.rebuildOrderLocked()
	if err := s.saveLocked(); err != nil {
		s.byID = snapshot
		s.active = activeSnap
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
	items := make([]Project, 0, len(ids))
	for _, id := range ids {
		items = append(items, s.byID[id])
	}
	data, err := json.MarshalIndent(fileFormat{
		Version:    formatVersion,
		ActiveLock: s.active,
		Projects:   items,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("project: encode: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("project: create %s: %w", s.dir, err)
	}
	tmp, err := os.CreateTemp(s.dir, config.ProjectsFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("project: temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("project: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("project: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("project: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("project: close temp file: %w", err)
	}
	if err := s.rename(tmpName, s.Path()); err != nil {
		cleanup()
		return fmt.Errorf("project: replace %s: %w", s.Path(), err)
	}
	if d, err := os.Open(s.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *Store) uniqueProjectIDLocked(name string) string {
	base := slugify(name)
	if base == "" {
		base = "project"
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

func uniqueTestID(tests []Test, name string) string {
	seen := make(map[string]bool, len(tests))
	for _, t := range tests {
		seen[t.ID] = true
	}
	base := slugify(name)
	if base == "" {
		base = "test"
	}
	if !seen[base] {
		return base
	}
	for n := 2; ; n++ {
		cand := fmt.Sprintf("%s-%d", base, n)
		if !seen[cand] {
			return cand
		}
	}
}

func findTestIndex(tests []Test, id string) int {
	for i := range tests {
		if tests[i].ID == id {
			return i
		}
	}
	return -1
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

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneTest(t Test) Test {
	out := t
	if t.Agents != nil {
		out.Agents = append([]string(nil), t.Agents...)
	}
	if t.StartedAt != nil {
		v := *t.StartedAt
		out.StartedAt = &v
	}
	if t.EndedAt != nil {
		v := *t.EndedAt
		out.EndedAt = &v
	}
	if t.ResultPointers != nil {
		rp := &ResultPointers{}
		if t.ResultPointers.Evidence != nil {
			ev := *t.ResultPointers.Evidence
			if ev.To != nil {
				v := *ev.To
				ev.To = &v
			}
			if ev.Machines != nil {
				ev.Machines = append([]string(nil), ev.Machines...)
			}
			rp.Evidence = &ev
		}
		if t.ResultPointers.Monitor != nil {
			mv := *t.ResultPointers.Monitor
			if mv.To != nil {
				v := *mv.To
				mv.To = &v
			}
			rp.Monitor = &mv
		}
		out.ResultPointers = rp
	}
	return out
}

func cloneProject(p Project) Project {
	out := p
	out.Metadata = cloneStringMap(p.Metadata)
	out.Tests = make([]Test, len(p.Tests))
	for i, t := range p.Tests {
		out.Tests[i] = cloneTest(t)
	}
	return out
}
