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
)

// Maneuver CRUD, preview, and execution API (internal/maneuver + internal/fleet, T-0063).
//
//	GET    /api/maneuvers                   list all Maneuvers
//	POST   /api/maneuvers                   create a Maneuver
//	POST   /api/maneuvers/preview           validate and materialize a Maneuver spec
//	GET    /api/maneuvers/runs              list active and recent Maneuver runs
//	POST   /api/maneuvers/runs/{runId}/stop stop a running/preparing Maneuver
//	GET    /api/maneuvers/{id}              fetch a single Maneuver
//	PUT    /api/maneuvers/{id}              update a Maneuver
//	POST   /api/maneuvers/{id}/duplicate    clone a Maneuver
//	POST   /api/maneuvers/{id}/start        start a synchronized Maneuver run
//	DELETE /api/maneuvers/{id}              delete a Maneuver
func (s *server) routesManeuver(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/maneuvers", s.live(s.listManeuvers))
	mux.HandleFunc("POST /api/maneuvers", s.live(s.createManeuver))
	mux.HandleFunc("POST /api/maneuvers/preview", s.live(s.previewManeuver))
	mux.HandleFunc("GET /api/maneuvers/runs", s.live(s.listManeuverRuns))
	mux.HandleFunc("POST /api/maneuvers/runs/{runId}/stop", s.live(s.stopManeuverRun))
	mux.HandleFunc("GET /api/maneuvers/{id}", s.live(s.getManeuver))
	mux.HandleFunc("PUT /api/maneuvers/{id}", s.live(s.updateManeuver))
	mux.HandleFunc("POST /api/maneuvers/{id}/duplicate", s.live(s.duplicateManeuver))
	mux.HandleFunc("POST /api/maneuvers/{id}/start", s.live(s.startManeuver))
	mux.HandleFunc("DELETE /api/maneuvers/{id}", s.live(s.deleteManeuver))
}

type maneuverRunner interface {
	StartManeuver(ctx context.Context, req fleet.StartManeuverRequest) (fleet.ManeuverRun, error)
	StopManeuver(ctx context.Context, runID, reason string) (fleet.ManeuverRun, error)
	ManeuverRuns() []fleet.ManeuverRun
	ManeuverRun(runID string) (fleet.ManeuverRun, bool)
}

type maneuverStoreProvider interface {
	ManeuverStore() *maneuver.Store
}

var (
	maneuverStoreMu sync.Mutex
	maneuverStores  = map[*server]*maneuver.Store{}
)

var errManeuverUnconfigured = errors.New("the maneuver store is not configured on this server")

func (s *server) maneuverStore() (*maneuver.Store, error) {
	if mp, ok := s.be.(maneuverStoreProvider); ok {
		if st := mp.ManeuverStore(); st != nil {
			return st, nil
		}
	}

	maneuverStoreMu.Lock()
	defer maneuverStoreMu.Unlock()
	if st, ok := maneuverStores[s]; ok {
		return st, nil
	}

	dataDir := s.registryDataDir()
	if dataDir == "" {
		return nil, errManeuverUnconfigured
	}
	st, err := maneuver.Open(dataDir)
	if err != nil {
		return nil, err
	}
	maneuverStores[s] = st
	return st, nil
}

func (s *server) writeManeuverErr(w http.ResponseWriter, err error) {
	var unresolved *baseline.UnresolvedPlaceholdersError
	var itemErr *maneuver.ItemResolutionError
	switch {
	case errors.Is(err, errManeuverUnconfigured), errors.Is(err, errBaselineUnconfigured):
		writeJSON(w, http.StatusServiceUnavailable, errBody(err.Error()))
	case errors.Is(err, maneuver.ErrNotFound), errors.Is(err, baseline.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errBody(err.Error()))
	case errors.Is(err, maneuver.ErrAlreadyExists):
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
	case errors.Is(err, maneuver.ErrPatternDeferred), errors.Is(err, maneuver.ErrInvalidManeuver),
		errors.Is(err, baseline.ErrInvalidItem), errors.Is(err, baseline.ErrInvalidTemplate):
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
	default:
		writeErr(w, err)
	}
}

type maneuverListResponse struct {
	Maneuvers []maneuver.Maneuver `json:"maneuvers"`
}

type maneuverResponse struct {
	Maneuver maneuver.Maneuver `json:"maneuver"`
}

type maneuverSpecBody struct {
	ID          string                  `json:"id,omitempty"`
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Target      maneuver.Target         `json:"target"`
	TotalSec    int                     `json:"total_sec"`
	Pattern     maneuver.PatternConfig  `json:"pattern"`
	Timeline    []maneuver.TimelineItem `json:"timeline"`
	Agents      []string                `json:"agents"`
}

func (b maneuverSpecBody) spec() maneuver.Spec {
	return maneuver.Spec{
		ID:          b.ID,
		Name:        b.Name,
		Description: b.Description,
		Target:      b.Target,
		TotalSec:    b.TotalSec,
		Pattern:     b.Pattern,
		Timeline:    b.Timeline,
		Agents:      b.Agents,
	}
}

func (s *server) validateAndMaterializeSpec(spec maneuver.Spec) (maneuver.MaterializedPlan, error) {
	if len(spec.Timeline) == 0 {
		norm, err := maneuver.ValidateSpec(spec, true)
		if err != nil {
			return maneuver.MaterializedPlan{}, err
		}
		return maneuver.MaterializedPlan{
			ManeuverID: norm.ID,
			Name:       norm.Name,
			Target:     norm.Target,
			TotalSec:   norm.TotalSec,
			Pattern:    norm.Pattern,
			Items:      []maneuver.MaterializedItem{},
			Agents:     norm.Agents,
		}, nil
	}
	bStore, err := s.baselineStore()
	if err != nil {
		return maneuver.MaterializedPlan{}, err
	}
	return maneuver.Materialize(spec, bStore)
}

func (s *server) listManeuvers(w http.ResponseWriter, _ *http.Request) {
	st, err := s.maneuverStore()
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	list := st.List()
	if list == nil {
		list = []maneuver.Maneuver{}
	}
	writeJSON(w, http.StatusOK, maneuverListResponse{Maneuvers: list})
}

func (s *server) getManeuver(w http.ResponseWriter, r *http.Request) {
	st, err := s.maneuverStore()
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	m, ok := st.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("maneuver not found"))
		return
	}
	writeJSON(w, http.StatusOK, maneuverResponse{Maneuver: m})
}

func (s *server) createManeuver(w http.ResponseWriter, r *http.Request) {
	st, err := s.maneuverStore()
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	var b maneuverSpecBody
	if !decode(w, r, &b) {
		return
	}
	spec := b.spec()
	if _, err := s.validateAndMaterializeSpec(spec); err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	op := operatorFrom(r)
	m, err := st.Create(spec, op)
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	s.log.Info("maneuver created", "component", "panel", "operator", op, "id", m.ID, "name", m.Name)
	writeJSON(w, http.StatusCreated, maneuverResponse{Maneuver: m})
}

func (s *server) updateManeuver(w http.ResponseWriter, r *http.Request) {
	st, err := s.maneuverStore()
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	var b maneuverSpecBody
	if !decode(w, r, &b) {
		return
	}
	id := r.PathValue("id")
	spec := b.spec()
	if spec.ID != "" && strings.TrimSpace(spec.ID) != id {
		writeJSON(w, http.StatusBadRequest, errBody("cannot change maneuver id"))
		return
	}
	if _, err := s.validateAndMaterializeSpec(spec); err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	op := operatorFrom(r)
	m, err := st.Update(id, spec, op)
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	s.log.Info("maneuver updated", "component", "panel", "operator", op, "id", m.ID, "name", m.Name)
	writeJSON(w, http.StatusOK, maneuverResponse{Maneuver: m})
}

type duplicateManeuverBody struct {
	Name string `json:"name,omitempty"`
}

func (s *server) duplicateManeuver(w http.ResponseWriter, r *http.Request) {
	st, err := s.maneuverStore()
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	var b duplicateManeuverBody
	if r.ContentLength != 0 && !decode(w, r, &b) {
		return
	}
	id := r.PathValue("id")
	op := operatorFrom(r)
	m, err := st.Duplicate(id, b.Name, op)
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	s.log.Info("maneuver duplicated", "component", "panel", "operator", op, "source", id, "id", m.ID)
	writeJSON(w, http.StatusCreated, maneuverResponse{Maneuver: m})
}

func (s *server) deleteManeuver(w http.ResponseWriter, r *http.Request) {
	st, err := s.maneuverStore()
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	if r.ContentLength != 0 {
		var empty struct{}
		if !decode(w, r, &empty) {
			return
		}
	}
	id := r.PathValue("id")
	if err := st.Delete(id); err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	s.log.Info("maneuver deleted", "component", "panel", "operator", operatorFrom(r), "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (s *server) previewManeuver(w http.ResponseWriter, r *http.Request) {
	var b maneuverSpecBody
	if !decode(w, r, &b) {
		return
	}
	plan, err := s.validateAndMaterializeSpec(b.spec())
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

type startManeuverBody struct {
	Confirm bool `json:"confirm,omitempty"`
	LeadMS  int  `json:"lead_ms,omitempty"`
}

func (s *server) listManeuverRuns(w http.ResponseWriter, _ *http.Request) {
	mr, ok := s.be.(maneuverRunner)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"runs": []fleet.ManeuverRun{}})
		return
	}
	runs := mr.ManeuverRuns()
	if runs == nil {
		runs = []fleet.ManeuverRun{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *server) startManeuver(w http.ResponseWriter, r *http.Request) {
	mr, ok := s.be.(maneuverRunner)
	if !ok {
		notConfigured(w, "maneuver execution")
		return
	}
	st, err := s.maneuverStore()
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	var b startManeuverBody
	if r.ContentLength != 0 && !decode(w, r, &b) {
		return
	}
	id := r.PathValue("id")
	mv, ok := st.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("maneuver not found"))
		return
	}
	plan, err := s.validateAndMaterializeSpec(maneuver.Spec{
		ID:          mv.ID,
		Name:        mv.Name,
		Description: mv.Description,
		Target:      mv.Target,
		TotalSec:    mv.TotalSec,
		Pattern:     mv.Pattern,
		Timeline:    mv.Timeline,
		Agents:      mv.Agents,
	})
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	req := fleet.StartManeuverRequest{
		ManeuverID: mv.ID,
		Plan:       &plan,
		User:       operatorFrom(r),
		Confirm:    b.Confirm,
	}
	if b.LeadMS != 0 {
		req.LeadTime = time.Duration(b.LeadMS) * time.Millisecond
	}
	run, err := mr.StartManeuver(r.Context(), req)
	if err != nil {
		var cr *fleet.ConfirmRequiredError
		if errors.As(err, &cr) {
			writeJSON(w, http.StatusConflict, gateResponse{
				Error:             cr.Error(),
				NeedsConfirmation: true,
				Pattern:           cr.Pattern,
				Command:           cr.Command,
				Machines:          []string{cr.Machine},
			})
			return
		}
		s.writeManeuverErr(w, err)
		return
	}
	s.log.Info("maneuver started", "component", "panel", "operator", req.User, "maneuver", mv.ID, "run", run.RunID)
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
}

func (s *server) stopManeuverRun(w http.ResponseWriter, r *http.Request) {
	mr, ok := s.be.(maneuverRunner)
	if !ok {
		notConfigured(w, "maneuver execution")
		return
	}
	if r.ContentLength != 0 {
		var empty struct{}
		if !decode(w, r, &empty) {
			return
		}
	}
	runID := r.PathValue("runId")
	op := operatorFrom(r)
	run, err := mr.StopManeuver(r.Context(), runID, "stopped by "+op)
	if err != nil {
		s.writeManeuverErr(w, err)
		return
	}
	s.log.Info("maneuver stopped", "component", "panel", "operator", op, "run", runID)
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}
