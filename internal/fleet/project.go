package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/maneuver"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/project"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// Errors returned by project test orchestration.
var (
	ErrMonitorRequired    = errors.New("fleet: a test needs a monitor (specify monitor_job_id or create_monitor)")
	ErrMonitorNotFound    = errors.New("fleet: monitor job not found")
	ErrMonitorUnavailable = errors.New("fleet: monitor jobs store is not configured")
)

// StartProjectTestRequest starts a planned project test: acquires the global
// single-active-test lock, attaches or creates the test's monitor job, and
// launches the test's Maneuver via StartManeuver.
type StartProjectTestRequest struct {
	ProjectID     string
	TestID        string
	MonitorJobID  string
	CreateMonitor bool
	ProbeTypes    []probe.Type
	Paths         []string
	IntervalMS    int
	User          string
	Confirm       bool
	LeadTime      time.Duration
	StartAt       *time.Time
}

// StartProjectTestResult is the outcome of StartProjectTest.
type StartProjectTestResult struct {
	Project project.Project         `json:"project"`
	Test    project.Test            `json:"test"`
	Run     ManeuverRun             `json:"run"`
	Active  *project.ActiveTestLock `json:"active"`
}

// StopProjectTestResult is the outcome of StopProjectTest.
type StopProjectTestResult struct {
	Project project.Project `json:"project"`
	Test    project.Test    `json:"test"`
	Run     *ManeuverRun    `json:"run,omitempty"`
}

// StartProjectTest enforces the system-wide single-active-test lock, attaches
// or creates a monitor job for the test's target, and runs the test's Maneuver
// using T-0063's StartManeuver path.
func (m *Manager) StartProjectTest(ctx context.Context, req StartProjectTestRequest) (StartProjectTestResult, error) {
	if m.projStore == nil || m.manStore == nil || m.baseStore == nil {
		return StartProjectTestResult{}, ErrUnavailable
	}
	if _, ok := m.projStore.Get(req.ProjectID); !ok {
		return StartProjectTestResult{}, fmt.Errorf("%w: %s", project.ErrNotFound, req.ProjectID)
	}
	t, ok := m.projStore.GetTest(req.ProjectID, req.TestID)
	if !ok {
		return StartProjectTestResult{}, fmt.Errorf("%w: %s", project.ErrTestNotFound, req.TestID)
	}

	obs := m.Observe()
	if obs == nil || obs.Jobs == nil {
		return StartProjectTestResult{}, ErrMonitorUnavailable
	}

	// Resolve and materialize the test's Maneuver before touching monitor jobs.
	mv, ok := m.manStore.Get(t.ManeuverID)
	if !ok {
		return StartProjectTestResult{}, fmt.Errorf("%w: %s", maneuver.ErrNotFound, t.ManeuverID)
	}
	plan, err := m.resolveManeuverPlan(StartManeuverRequest{ManeuverID: mv.ID})
	if err != nil {
		return StartProjectTestResult{}, err
	}

	// Validate monitor requirement before acquiring the lock or creating jobs.
	wantJobID := strings.TrimSpace(req.MonitorJobID)
	if wantJobID == "" {
		wantJobID = strings.TrimSpace(t.Monitor.JobID)
	}
	var monRef project.MonitorRef
	needCreateMonitor := false
	if wantJobID != "" {
		job, ok := obs.Jobs.Get(wantJobID)
		if !ok {
			return StartProjectTestResult{}, fmt.Errorf("%w: %s", ErrMonitorNotFound, wantJobID)
		}
		monRef = project.MonitorRef{
			JobID:      job.ID,
			HistoryKey: job.HistoryKey,
			IP:         job.IP,
			Origin:     job.Origin,
			Mode:       project.MonitorModeAttached,
		}
	} else if req.CreateMonitor {
		if existing, ok := obs.Jobs.Find(plan.Target.IP, plan.Target.Origin); ok {
			monRef = project.MonitorRef{
				JobID:      existing.ID,
				HistoryKey: existing.HistoryKey,
				IP:         existing.IP,
				Origin:     existing.Origin,
				Mode:       project.MonitorModeAttached,
			}
		} else {
			needCreateMonitor = true
		}
	} else {
		return StartProjectTestResult{}, ErrMonitorRequired
	}

	prevMonitor := t.Monitor
	startedAt := m.maneuverNow().UTC()

	// Acquire the global single-active-test lock atomically. Concurrent starts
	// are refused immediately with *project.ActiveTestError.
	proj, startedTest, err := m.projStore.BeginStart(req.ProjectID, req.TestID, project.StartParams{
		ManeuverName: mv.Name,
		Target: project.TargetRef{
			IP:     plan.Target.IP,
			Origin: plan.Target.Origin,
		},
		Agents:    plan.Agents,
		Monitor:   monRef,
		Actor:     req.User,
		StartedAt: startedAt,
	})
	if err != nil {
		return StartProjectTestResult{}, err
	}

	createdJobID := ""
	rollback := func() {
		if createdJobID != "" {
			if derr := obs.Jobs.Delete(createdJobID); derr == nil {
				m.PushMonitorConfigs(context.WithoutCancel(ctx))
			}
		}
		_ = m.projStore.RollbackStart(req.ProjectID, req.TestID, prevMonitor)
	}

	if needCreateMonitor {
		job, err := m.createTestMonitorJob(ctx, obs.Jobs, plan.Target, req)
		if err != nil {
			rollback()
			return StartProjectTestResult{}, err
		}
		createdJobID = job.ID
		monRef = project.MonitorRef{
			JobID:      job.ID,
			HistoryKey: job.HistoryKey,
			IP:         job.IP,
			Origin:     job.Origin,
			Mode:       project.MonitorModeCreated,
		}
		proj, startedTest, err = m.projStore.AttachMonitor(req.ProjectID, req.TestID, monRef)
		if err != nil {
			rollback()
			return StartProjectTestResult{}, err
		}
	}

	run, err := m.StartManeuver(ctx, StartManeuverRequest{
		ManeuverID: mv.ID,
		Plan:       &plan,
		User:       req.User,
		Confirm:    req.Confirm,
		LeadTime:   req.LeadTime,
		StartAt:    req.StartAt,
	})
	if err != nil {
		rollback()
		return StartProjectTestResult{}, err
	}

	proj, startedTest, err = m.projStore.AttachRun(req.ProjectID, req.TestID, run.RunID, plan.Agents)
	if err != nil {
		m.log.Warn("attach maneuver run to project test", "component", "fleet", "project", req.ProjectID, "test", req.TestID, "run", run.RunID, "err", err)
	}

	// Handle the rare case where the Maneuver reached a terminal state before
	// AttachRun recorded its RunID in projStore.
	if curRun, ok := m.ManeuverRun(run.RunID); ok {
		run = curRun
		if isTerminalRunState(curRun.State) {
			m.syncProjectTestFromRun(curRun)
			if p2, ok := m.projStore.Get(req.ProjectID); ok {
				proj = p2
			}
			if t2, ok := m.projStore.GetTest(req.ProjectID, req.TestID); ok {
				startedTest = t2
			}
		}
	}

	active, _ := m.projStore.ActiveLock()
	return StartProjectTestResult{
		Project: proj,
		Test:    startedTest,
		Run:     run,
		Active:  active,
	}, nil
}

func (m *Manager) createTestMonitorJob(ctx context.Context, jobs *monitor.Jobs, target maneuver.Target, req StartProjectTestRequest) (monitor.Job, error) {
	var monitorAgents []string
	for _, mc := range m.Machines() {
		if mc.Role == registry.RoleMonitor && !mc.Removed {
			monitorAgents = append(monitorAgents, mc.ID)
		}
	}
	spec := monitor.Spec{
		IP:         target.IP,
		Origin:     target.Origin,
		ProbeTypes: req.ProbeTypes,
	}
	for _, p := range req.Paths {
		if p = strings.TrimSpace(p); p != "" {
			spec.Paths = append(spec.Paths, p)
		}
	}
	job, err := jobs.Create(spec, monitorAgents)
	if err != nil {
		return monitor.Job{}, err
	}
	if req.IntervalMS > 0 {
		for _, tgt := range job.Targets {
			if err := jobs.SetTargetInterval(job.ID, tgt.Target.ID, time.Duration(req.IntervalMS)*time.Millisecond); err != nil {
				m.log.Warn("project test monitor interval override", "component", "fleet", "job", job.ID, "err", err)
			}
		}
		if updated, ok := jobs.Get(job.ID); ok {
			job = updated
		}
	}
	m.PushMonitorConfigs(context.WithoutCancel(ctx))
	return job, nil
}

// StopProjectTest stops a running test's Maneuver and records completion in
// the project store, releasing the global active-test lock.
func (m *Manager) StopProjectTest(ctx context.Context, projectID, testID, user, reason string) (StopProjectTestResult, error) {
	if m.projStore == nil {
		return StopProjectTestResult{}, ErrUnavailable
	}
	if _, ok := m.projStore.Get(projectID); !ok {
		return StopProjectTestResult{}, fmt.Errorf("%w: %s", project.ErrNotFound, projectID)
	}
	t, ok := m.projStore.GetTest(projectID, testID)
	if !ok {
		return StopProjectTestResult{}, fmt.Errorf("%w: %s", project.ErrTestNotFound, testID)
	}
	if t.Status != project.StatusRunning {
		return StopProjectTestResult{}, fmt.Errorf("%w: test %s is %s", project.ErrInvalidTransition, testID, t.Status)
	}
	if strings.TrimSpace(reason) == "" {
		if strings.TrimSpace(user) != "" {
			reason = "stopped by " + strings.TrimSpace(user)
		} else {
			reason = "stopped by operator"
		}
	}

	var stoppedRun *ManeuverRun
	var stopErrNote string
	if t.ManeuverRunID != "" {
		run, err := m.StopManeuver(ctx, t.ManeuverRunID, reason)
		if err != nil {
			if !errors.Is(err, ErrUnknown) {
				return StopProjectTestResult{}, err
			}
			stopErrNote = "maneuver run no longer active on server"
		} else {
			stoppedRun = &run
		}
	}

	endedAt := m.maneuverNow().UTC()
	if stoppedRun != nil && stoppedRun.FinishedAt != nil {
		endedAt = *stoppedRun.FinishedAt
	}
	proj, finishedTest, err := m.projStore.FinishTest(
		projectID, testID,
		project.StatusCompleted, project.OutcomeStopped,
		stopErrNote, user, endedAt,
	)
	if err != nil {
		return StopProjectTestResult{}, err
	}
	return StopProjectTestResult{
		Project: proj,
		Test:    finishedTest,
		Run:     stoppedRun,
	}, nil
}

func (m *Manager) syncProjectTestFromRun(r ManeuverRun) {
	if m.projStore == nil || !isTerminalRunState(r.State) {
		return
	}
	var status, outcome string
	switch r.State {
	case ManeuverStateCompleted:
		status, outcome = project.StatusCompleted, project.OutcomeCompleted
	case ManeuverStateStopped:
		status, outcome = project.StatusCompleted, project.OutcomeStopped
	case ManeuverStateAborted:
		status, outcome = project.StatusFailed, project.OutcomeAborted
	default:
		return
	}
	endedAt := r.UpdatedAt
	if r.FinishedAt != nil {
		endedAt = *r.FinishedAt
	}
	if _, _, _, err := m.projStore.FinishTestByRunID(r.RunID, status, outcome, r.Error, endedAt); err != nil {
		m.log.Warn("sync project test from maneuver run", "component", "fleet", "run", r.RunID, "err", err)
	}
}
