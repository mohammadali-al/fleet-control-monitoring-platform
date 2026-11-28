// Package project persists projects, their tests, and the system-wide
// single-active-test lock under <dataDir>/projects/projects.json.
package project

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Test lifecycle statuses.
const (
	StatusPlanned   = "planned"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Test completion outcomes.
const (
	OutcomeCompleted = "completed"
	OutcomeStopped   = "stopped"
	OutcomeAborted   = "aborted"
	OutcomeFailed    = "failed"
)

// Monitor attachment modes.
const (
	MonitorModeAttached = "attached"
	MonitorModeCreated  = "created"
)

// Sentinel errors returned by project validation and store operations.
var (
	ErrNotFound           = errors.New("project: not found")
	ErrTestNotFound       = errors.New("project: test not found")
	ErrAlreadyExists      = errors.New("project: already exists")
	ErrInvalidProject     = errors.New("project: invalid project")
	ErrInvalidTest        = errors.New("project: invalid test")
	ErrActiveTestConflict = errors.New("project: another test is already active")
	ErrInvalidTransition  = errors.New("project: invalid test state transition")
)

// ActiveTestError reports that a start or delete was refused because another
// test holds the global active-test lock.
type ActiveTestError struct {
	Active ActiveTestLock
}

func (e *ActiveTestError) Error() string {
	return fmt.Sprintf("%s: project %q test %q (%s) is already active",
		ErrActiveTestConflict, e.Active.ProjectID, e.Active.TestID, e.Active.TestName)
}

func (e *ActiveTestError) Unwrap() error { return ErrActiveTestConflict }

// TargetRef snapshots the single (ip, origin) target of a test's Maneuver.
type TargetRef struct {
	IP     string `json:"ip,omitempty"`
	Origin string `json:"origin,omitempty"`
}

// MonitorRef records the monitor job attached to or created for a test.
type MonitorRef struct {
	JobID      string `json:"job_id,omitempty"`
	HistoryKey string `json:"history_key,omitempty"`
	IP         string `json:"ip,omitempty"`
	Origin     string `json:"origin,omitempty"`
	Mode       string `json:"mode,omitempty"` // attached | created
}

// EvidenceWindow points to the evidence archive slice for a test run.
type EvidenceWindow struct {
	From          time.Time  `json:"from"`
	To            *time.Time `json:"to,omitempty"`
	Machines      []string   `json:"machines,omitempty"`
	ManeuverRunID string     `json:"maneuver_run_id,omitempty"`
}

// MonitorWindow points to the monitor time-series slice for a test run.
type MonitorWindow struct {
	JobID      string     `json:"job_id,omitempty"`
	HistoryKey string     `json:"history_key,omitempty"`
	From       time.Time  `json:"from"`
	To         *time.Time `json:"to,omitempty"`
}

// ResultPointers stores the evidence and monitor windows for later report generation.
type ResultPointers struct {
	Evidence *EvidenceWindow `json:"evidence,omitempty"`
	Monitor  *MonitorWindow  `json:"monitor,omitempty"`
}

// Test is one planned or executed Maneuver test within a Project.
type Test struct {
	ID             string          `json:"id"`
	ProjectID      string          `json:"project_id"`
	Name           string          `json:"name"`
	Description    string          `json:"description,omitempty"`
	ManeuverID     string          `json:"maneuver_id"`
	ManeuverName   string          `json:"maneuver_name,omitempty"`
	Target         TargetRef       `json:"target,omitempty"`
	Agents         []string        `json:"agents,omitempty"`
	Monitor        MonitorRef      `json:"monitor,omitempty"`
	Status         string          `json:"status"`
	Outcome        string          `json:"outcome,omitempty"`
	Error          string          `json:"error,omitempty"`
	ManeuverRunID  string          `json:"maneuver_run_id,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	EndedAt        *time.Time      `json:"ended_at,omitempty"`
	CreatedBy      string          `json:"created_by,omitempty"`
	StartedBy      string          `json:"started_by,omitempty"`
	StoppedBy      string          `json:"stopped_by,omitempty"`
	ResultPointers *ResultPointers `json:"result_pointers,omitempty"`
}

// Project groups tests and engagement metadata.
type Project struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Tests       []Test            `json:"tests"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	CreatedBy   string            `json:"created_by,omitempty"`
	UpdatedBy   string            `json:"updated_by,omitempty"`
}

// ProjectSpec holds the mutable fields for creating or updating a Project.
type ProjectSpec struct {
	ID          string            `json:"id,omitempty"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// TestSpec holds the fields for creating a planned Test inside a Project.
type TestSpec struct {
	ID           string     `json:"id,omitempty"`
	Name         string     `json:"name,omitempty"`
	Description  string     `json:"description,omitempty"`
	ManeuverID   string     `json:"maneuver_id"`
	ManeuverName string     `json:"maneuver_name,omitempty"`
	Target       TargetRef  `json:"target,omitempty"`
	Agents       []string   `json:"agents,omitempty"`
	Monitor      MonitorRef `json:"monitor,omitempty"`
}

// ActiveTestLock records the single system-wide active test.
type ActiveTestLock struct {
	ProjectID     string    `json:"project_id"`
	ProjectName   string    `json:"project_name"`
	TestID        string    `json:"test_id"`
	TestName      string    `json:"test_name"`
	ManeuverID    string    `json:"maneuver_id"`
	ManeuverRunID string    `json:"maneuver_run_id,omitempty"`
	MonitorJobID  string    `json:"monitor_job_id,omitempty"`
	StartedBy     string    `json:"started_by,omitempty"`
	AcquiredAt    time.Time `json:"acquired_at"`
}

// DateGroup groups completed/failed tests of a project under YYYY-MM-DD.
type DateGroup struct {
	Date  string `json:"date"`
	Tests []Test `json:"tests"`
}

// IsValidStatus reports whether st is a recognized test status.
func IsValidStatus(st string) bool {
	switch st {
	case StatusPlanned, StatusRunning, StatusCompleted, StatusFailed:
		return true
	default:
		return false
	}
}

// IsFinishedStatus reports whether st is a terminal test status (completed or failed).
func IsFinishedStatus(st string) bool {
	return st == StatusCompleted || st == StatusFailed
}

// ValidateProjectSpec validates and normalizes a ProjectSpec.
func ValidateProjectSpec(spec ProjectSpec, allowID bool) (ProjectSpec, error) {
	id := strings.TrimSpace(spec.ID)
	if allowID && id != "" && !isValidID(id) {
		return ProjectSpec{}, fmt.Errorf("%w: invalid id %q", ErrInvalidProject, id)
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return ProjectSpec{}, fmt.Errorf("%w: name is required", ErrInvalidProject)
	}
	if len(name) > 120 {
		return ProjectSpec{}, fmt.Errorf("%w: name exceeds 120 characters", ErrInvalidProject)
	}
	var meta map[string]string
	if len(spec.Metadata) > 0 {
		meta = make(map[string]string, len(spec.Metadata))
		for k, v := range spec.Metadata {
			key := strings.TrimSpace(k)
			if key == "" {
				return ProjectSpec{}, fmt.Errorf("%w: metadata key cannot be empty", ErrInvalidProject)
			}
			if len(key) > 64 {
				return ProjectSpec{}, fmt.Errorf("%w: metadata key %q exceeds 64 characters", ErrInvalidProject, key)
			}
			if strings.IndexByte(v, 0) >= 0 {
				return ProjectSpec{}, fmt.Errorf("%w: metadata %q contains NUL byte", ErrInvalidProject, key)
			}
			val := strings.TrimSpace(v)
			if val != "" {
				meta[key] = val
			}
		}
		if len(meta) == 0 {
			meta = nil
		}
	}
	return ProjectSpec{
		ID:          id,
		Name:        name,
		Description: strings.TrimSpace(spec.Description),
		Metadata:    meta,
	}, nil
}

// ValidateTestSpec validates and normalizes a TestSpec.
func ValidateTestSpec(spec TestSpec, allowID bool) (TestSpec, error) {
	id := strings.TrimSpace(spec.ID)
	if allowID && id != "" && !isValidID(id) {
		return TestSpec{}, fmt.Errorf("%w: invalid id %q", ErrInvalidTest, id)
	}
	manID := strings.TrimSpace(spec.ManeuverID)
	if manID == "" {
		return TestSpec{}, fmt.Errorf("%w: maneuver_id is required", ErrInvalidTest)
	}
	name := strings.TrimSpace(spec.Name)
	if len(name) > 120 {
		return TestSpec{}, fmt.Errorf("%w: name exceeds 120 characters", ErrInvalidTest)
	}
	agents := make([]string, 0, len(spec.Agents))
	seen := make(map[string]bool, len(spec.Agents))
	for _, raw := range spec.Agents {
		a := strings.TrimSpace(raw)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		agents = append(agents, a)
	}
	if len(agents) == 0 {
		agents = nil
	}
	return TestSpec{
		ID:           id,
		Name:         name,
		Description:  strings.TrimSpace(spec.Description),
		ManeuverID:   manID,
		ManeuverName: strings.TrimSpace(spec.ManeuverName),
		Target: TargetRef{
			IP:     strings.TrimSpace(spec.Target.IP),
			Origin: strings.TrimSpace(spec.Target.Origin),
		},
		Agents: agents,
		Monitor: MonitorRef{
			JobID:      strings.TrimSpace(spec.Monitor.JobID),
			HistoryKey: strings.TrimSpace(spec.Monitor.HistoryKey),
			IP:         strings.TrimSpace(spec.Monitor.IP),
			Origin:     strings.TrimSpace(spec.Monitor.Origin),
			Mode:       strings.TrimSpace(spec.Monitor.Mode),
		},
	}, nil
}
