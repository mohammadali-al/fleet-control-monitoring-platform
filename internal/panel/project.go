package panel

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/baseline"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/maneuver"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/project"
)

// Projects & tests API (internal/project + internal/fleet, T-0066).
//
//	GET    /api/projects                              list projects + active lock
//	GET    /api/projects/active                       get active-test lock + live run
//	POST   /api/projects                              create a project
//	GET    /api/projects/{id}                         fetch project + completed-by-date
//	PUT    /api/projects/{id}                         update project metadata
//	DELETE /api/projects/{id}                         delete project
//	GET    /api/projects/{id}/tests                   list tests + completed-by-date
//	POST   /api/projects/{id}/tests                   create a planned test
//	GET    /api/projects/{id}/tests/{testId}          fetch a single test + maneuver run
//	DELETE /api/projects/{id}/tests/{testId}          delete a test
//	POST   /api/projects/{id}/tests/{testId}/start    acquire lock, attach/create monitor, start maneuver
//	POST   /api/projects/{id}/tests/{testId}/stop     stop maneuver + record completion + release lock
func (s *server) routesProject(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects", s.live(s.listProjects))
	mux.HandleFunc("GET /api/projects/active", s.live(s.getActiveProjectTest))
	mux.HandleFunc("POST /api/projects", s.live(s.createProject))
	mux.HandleFunc("GET /api/projects/{id}", s.live(s.getProject))
	mux.HandleFunc("PUT /api/projects/{id}", s.live(s.updateProject))
	mux.HandleFunc("DELETE /api/projects/{id}", s.live(s.deleteProject))

	mux.HandleFunc("GET /api/projects/{id}/tests", s.live(s.listProjectTests))
	mux.HandleFunc("POST /api/projects/{id}/tests", s.live(s.createProjectTest))
	mux.HandleFunc("GET /api/projects/{id}/tests/{testId}", s.live(s.getProjectTest))
	mux.HandleFunc("DELETE /api/projects/{id}/tests/{testId}", s.live(s.deleteProjectTest))
	mux.HandleFunc("POST /api/projects/{id}/tests/{testId}/start", s.live(s.startProjectTest))
	mux.HandleFunc("POST /api/projects/{id}/tests/{testId}/stop", s.live(s.stopProjectTest))
}

type projectStoreProvider interface {
	ProjectStore() *project.Store
}

type projectRunner interface {
	StartProjectTest(ctx context.Context, req fleet.StartProjectTestRequest) (fleet.StartProjectTestResult, error)
	StopProjectTest(ctx context.Context, projectID, testID, user, reason string) (fleet.StopProjectTestResult, error)
}

var (
	projectStoreMu sync.Mutex
	projectStores  = map[*server]*project.Store{}
)

var errProjectUnconfigured = errors.New("the project store is not configured on this server")

func (s *server) projectStore() (*project.Store, error) {
	if pp, ok := s.be.(projectStoreProvider); ok {
		if st := pp.ProjectStore(); st != nil {
			return st, nil
		}
	}

	projectStoreMu.Lock()
	defer projectStoreMu.Unlock()
	if st, ok := projectStores[s]; ok {
		return st, nil
	}

	dataDir := s.registryDataDir()
	if dataDir == "" {
		return nil, errProjectUnconfigured
	}
	st, err := project.Open(dataDir)
	if err != nil {
		return nil, err
	}
	projectStores[s] = st
	return st, nil
}

func (s *server) writeProjectErr(w http.ResponseWriter, err error) {
	var activeErr *project.ActiveTestError
	var confirmErr *fleet.ConfirmRequiredError
	var unresolved *baseline.UnresolvedPlaceholdersError
	var itemErr *maneuver.ItemResolutionError

	switch {
	case errors.Is(err, errProjectUnconfigured), errors.Is(err, errManeuverUnconfigured),
		errors.Is(err, errBaselineUnconfigured), errors.Is(err, fleet.ErrMonitorUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, errBody(err.Error()))
	case errors.As(err, &activeErr):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":       activeErr.Error(),
			"active_test": activeErr.Active,
		})
	case errors.As(err, &confirmErr):
		writeJSON(w, http.StatusConflict, gateResponse{
			Error:             confirmErr.Error(),
			NeedsConfirmation: true,
			Pattern:           confirmErr.Pattern,
			Command:           confirmErr.Command,
			Machines:          []string{confirmErr.Machine},
		})
	case errors.Is(err, project.ErrNotFound), errors.Is(err, project.ErrTestNotFound),
		errors.Is(err, maneuver.ErrNotFound), errors.Is(err, baseline.ErrNotFound),
		errors.Is(err, fleet.ErrMonitorNotFound):
		writeJSON(w, http.StatusNotFound, errBody(err.Error()))
	case errors.Is(err, project.ErrAlreadyExists), errors.Is(err, project.ErrActiveTestConflict),
		errors.Is(err, project.ErrInvalidTransition), errors.Is(err, monitor.ErrNoMonitorAgent),
		errors.Is(err, monitor.ErrNoCapacity), errors.Is(err, monitor.ErrDuplicate):
		writeJSON(w, http.StatusConflict, errBody(err.Error()))
	case errors.As(err, &unresolved):
		resp := map[string]any{
			"error":      err.Error(),
			"unresolved": unresolved.Missing,
		}
		if errors.As(err, &itemErr) {
			resp["item_id"] = itemErr.ItemID
			resp["baseline_id"] = itemErr.BaselineID
			resp["item_index"] = itemErr.Index
		}
		writeJSON(w, http.StatusUnprocessableEntity, resp)
	case errors.Is(err, project.ErrInvalidProject), errors.Is(err, project.ErrInvalidTest),
		errors.Is(err, fleet.ErrMonitorRequired), errors.Is(err, maneuver.ErrInvalidManeuver),
		errors.Is(err, maneuver.ErrPatternDeferred), errors.Is(err, monitor.ErrBadIP),
		errors.Is(err, monitor.ErrBadOrigin), errors.Is(err, monitor.ErrBadPath),
		errors.Is(err, monitor.ErrBadProbeType), errors.Is(err, monitor.ErrOriginRequired):
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
	default:
		writeErr(w, err)
	}
}

type projectSpecBody struct {
	ID          string            `json:"id,omitempty"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

func (b projectSpecBody) spec() project.ProjectSpec {
	return project.ProjectSpec{
		ID:          b.ID,
		Name:        b.Name,
		Description: b.Description,
		Metadata:    b.Metadata,
	}
}

type createTestBody struct {
	ID           string `json:"id,omitempty"`
	Name         string `json:"name,omitempty"`
	Description  string `json:"description,omitempty"`
	ManeuverID   string `json:"maneuver_id"`
	MonitorJobID string `json:"monitor_job_id,omitempty"`
}

type startTestBody struct {
	MonitorJobID  string   `json:"monitor_job_id,omitempty"`
	CreateMonitor bool     `json:"create_monitor,omitempty"`
	ProbeTypes    []string `json:"probe_types,omitempty"`
	Paths         []string `json:"paths,omitempty"`
	IntervalMS    int      `json:"interval_ms,omitempty"`
	Confirm       bool     `json:"confirm,omitempty"`
	LeadMS        int      `json:"lead_ms,omitempty"`
}

type stopTestBody struct {
	Reason string `json:"reason,omitempty"`
}

func (s *server) activeSnapshot(st *project.Store) (*project.ActiveTestLock, *project.Test, *fleet.ManeuverRun) {
	lock, _, activeTest, ok := st.ActiveTest()
	if !ok || lock == nil {
		return nil, nil, nil
	}
	var runPtr *fleet.ManeuverRun
	if lock.ManeuverRunID != "" {
		if mr, ok := s.be.(maneuverRunner); ok {
			if run, ok := mr.ManeuverRun(lock.ManeuverRunID); ok {
				runPtr = &run
			}
		}
	}
	return lock, &activeTest, runPtr
}

func (s *server) listProjects(w http.ResponseWriter, _ *http.Request) {
	st, err := s.projectStore()
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	list := st.List()
	if list == nil {
		list = []project.Project{}
	}
	lock, _, _ := s.activeSnapshot(st)
	writeJSON(w, http.StatusOK, map[string]any{
		"projects": list,
		"active":   lock,
	})
}

func (s *server) getActiveProjectTest(w http.ResponseWriter, _ *http.Request) {
	st, err := s.projectStore()
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	lock, testPtr, runPtr := s.activeSnapshot(st)
	writeJSON(w, http.StatusOK, map[string]any{
		"active": lock,
		"test":   testPtr,
		"run":    runPtr,
	})
}

func (s *server) getProject(w http.ResponseWriter, r *http.Request) {
	st, err := s.projectStore()
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	p, ok := st.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("project not found"))
		return
	}
	lock, _, _ := s.activeSnapshot(st)
	writeJSON(w, http.StatusOK, map[string]any{
		"project":           p,
		"completed_by_date": project.CompletedByDate(p.Tests),
		"active":            lock,
	})
}

func (s *server) createProject(w http.ResponseWriter, r *http.Request) {
	st, err := s.projectStore()
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	var b projectSpecBody
	if !decode(w, r, &b) {
		return
	}
	op := operatorFrom(r)
	p, err := st.CreateProject(b.spec(), op)
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	s.log.Info("project created", "component", "panel", "operator", op, "id", p.ID, "name", p.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"project": p})
}

func (s *server) updateProject(w http.ResponseWriter, r *http.Request) {
	st, err := s.projectStore()
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	var b projectSpecBody
	if !decode(w, r, &b) {
		return
	}
	id := r.PathValue("id")
	if b.ID != "" && strings.TrimSpace(b.ID) != id {
		writeJSON(w, http.StatusBadRequest, errBody("cannot change project id"))
		return
	}
	op := operatorFrom(r)
	p, err := st.UpdateProject(id, b.spec(), op)
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	s.log.Info("project updated", "component", "panel", "operator", op, "id", p.ID, "name", p.Name)
	writeJSON(w, http.StatusOK, map[string]any{"project": p})
}

func (s *server) deleteProject(w http.ResponseWriter, r *http.Request) {
	st, err := s.projectStore()
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	if r.ContentLength != 0 {
		var empty struct{}
		if !decode(w, r, &empty) {
			return
		}
	}
	id := r.PathValue("id")
	if err := st.DeleteProject(id); err != nil {
		s.writeProjectErr(w, err)
		return
	}
	s.log.Info("project deleted", "component", "panel", "operator", operatorFrom(r), "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (s *server) listProjectTests(w http.ResponseWriter, r *http.Request) {
	st, err := s.projectStore()
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	p, ok := st.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("project not found"))
		return
	}
	statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))
	tests := make([]project.Test, 0, len(p.Tests))
	for _, t := range p.Tests {
		if statusFilter != "" && t.Status != statusFilter {
			continue
		}
		tests = append(tests, t)
	}
	lock, _, _ := s.activeSnapshot(st)
	writeJSON(w, http.StatusOK, map[string]any{
		"tests":             tests,
		"completed_by_date": project.CompletedByDate(tests),
		"active":            lock,
	})
}

func (s *server) createProjectTest(w http.ResponseWriter, r *http.Request) {
	st, err := s.projectStore()
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	var b createTestBody
	if !decode(w, r, &b) {
		return
	}
	projID := r.PathValue("id")
	if _, ok := st.Get(projID); !ok {
		writeJSON(w, http.StatusNotFound, errBody("project not found"))
		return
	}

	spec := project.TestSpec{
		ID:          b.ID,
		Name:        b.Name,
		Description: b.Description,
		ManeuverID:  strings.TrimSpace(b.ManeuverID),
	}
	if spec.ManeuverID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("maneuver_id is required"))
		return
	}

	ms, err := s.maneuverStore()
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	mv, ok := ms.Get(spec.ManeuverID)
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("maneuver not found"))
		return
	}
	spec.ManeuverName = mv.Name
	spec.Target = project.TargetRef{IP: mv.Target.IP, Origin: mv.Target.Origin}
	spec.Agents = append([]string(nil), mv.Agents...)

	if jobID := strings.TrimSpace(b.MonitorJobID); jobID != "" {
		ob, isObs := s.be.(observer)
		var o *fleet.Observe
		if isObs {
			o = ob.Observe()
		}
		if o == nil || o.Jobs == nil {
			writeJSON(w, http.StatusServiceUnavailable, errBody("monitor jobs store is not configured"))
			return
		}
		job, ok := o.Jobs.Get(jobID)
		if !ok {
			writeJSON(w, http.StatusNotFound, errBody("monitor job not found"))
			return
		}
		spec.Monitor = project.MonitorRef{
			JobID:      job.ID,
			HistoryKey: job.HistoryKey,
			IP:         job.IP,
			Origin:     job.Origin,
			Mode:       project.MonitorModeAttached,
		}
	}

	op := operatorFrom(r)
	proj, test, err := st.CreateTest(projID, spec, op)
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	s.log.Info("project test created", "component", "panel", "operator", op, "project", projID, "test", test.ID, "maneuver", test.ManeuverID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"test":    test,
		"project": proj,
	})
}

func (s *server) getProjectTest(w http.ResponseWriter, r *http.Request) {
	st, err := s.projectStore()
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	projID := r.PathValue("id")
	testID := r.PathValue("testId")
	if _, ok := st.Get(projID); !ok {
		writeJSON(w, http.StatusNotFound, errBody("project not found"))
		return
	}
	t, ok := st.GetTest(projID, testID)
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("test not found"))
		return
	}
	var runPtr *fleet.ManeuverRun
	if t.ManeuverRunID != "" {
		if mr, ok := s.be.(maneuverRunner); ok {
			if run, ok := mr.ManeuverRun(t.ManeuverRunID); ok {
				runPtr = &run
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"test":         t,
		"maneuver_run": runPtr,
	})
}

func (s *server) deleteProjectTest(w http.ResponseWriter, r *http.Request) {
	st, err := s.projectStore()
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	if r.ContentLength != 0 {
		var empty struct{}
		if !decode(w, r, &empty) {
			return
		}
	}
	projID := r.PathValue("id")
	testID := r.PathValue("testId")
	if err := st.DeleteTest(projID, testID); err != nil {
		s.writeProjectErr(w, err)
		return
	}
	s.log.Info("project test deleted", "component", "panel", "operator", operatorFrom(r), "project", projID, "test", testID)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": testID})
}

func (s *server) startProjectTest(w http.ResponseWriter, r *http.Request) {
	pr, ok := s.be.(projectRunner)
	if !ok {
		notConfigured(w, "project test execution")
		return
	}
	var b startTestBody
	if r.ContentLength != 0 && !decode(w, r, &b) {
		return
	}
	if b.IntervalMS != 0 && !validInterval(b.IntervalMS) {
		writeJSON(w, http.StatusBadRequest, errBody("monitor interval_ms is out of range"))
		return
	}
	var probeTypes []probe.Type
	for _, raw := range b.ProbeTypes {
		if pt := strings.TrimSpace(raw); pt != "" {
			probeTypes = append(probeTypes, probe.Type(pt))
		}
	}
	op := operatorFrom(r)
	req := fleet.StartProjectTestRequest{
		ProjectID:     r.PathValue("id"),
		TestID:        r.PathValue("testId"),
		MonitorJobID:  b.MonitorJobID,
		CreateMonitor: b.CreateMonitor,
		ProbeTypes:    probeTypes,
		Paths:         b.Paths,
		IntervalMS:    b.IntervalMS,
		User:          op,
		Confirm:       b.Confirm,
	}
	if b.LeadMS != 0 {
		req.LeadTime = time.Duration(b.LeadMS) * time.Millisecond
	}
	res, err := pr.StartProjectTest(r.Context(), req)
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	s.log.Info("project test started", "component", "panel", "operator", op,
		"project", res.Project.ID, "test", res.Test.ID, "maneuver_run", res.Run.RunID, "monitor_job", res.Test.Monitor.JobID)
	writeJSON(w, http.StatusAccepted, res)
}

func (s *server) stopProjectTest(w http.ResponseWriter, r *http.Request) {
	pr, ok := s.be.(projectRunner)
	if !ok {
		notConfigured(w, "project test execution")
		return
	}
	var b stopTestBody
	if r.ContentLength != 0 && !decode(w, r, &b) {
		return
	}
	op := operatorFrom(r)
	res, err := pr.StopProjectTest(r.Context(), r.PathValue("id"), r.PathValue("testId"), op, b.Reason)
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	s.log.Info("project test stopped", "component", "panel", "operator", op,
		"project", res.Project.ID, "test", res.Test.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"project": res.Project,
		"test":    res.Test,
		"run":     res.Run,
		"active":  nil,
	})
}
