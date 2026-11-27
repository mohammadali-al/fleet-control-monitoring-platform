package panel

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/baseline"
)

// Baseline items API (internal/baseline, T-0054).
//
//	GET    /api/baseline                list all global baseline items
//	POST   /api/baseline                create a new baseline item
//	POST   /api/baseline/preview        validate and render a template with inputs
//	GET    /api/baseline/{id}           fetch a single baseline item
//	PUT    /api/baseline/{id}           update a baseline item
//	POST   /api/baseline/{id}/duplicate clone an existing baseline item
//	DELETE /api/baseline/{id}           delete a baseline item
func (s *server) routesBaseline(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/baseline", s.live(s.listBaseline))
	mux.HandleFunc("POST /api/baseline", s.live(s.createBaseline))
	mux.HandleFunc("POST /api/baseline/preview", s.live(s.previewBaseline))
	mux.HandleFunc("GET /api/baseline/{id}", s.live(s.getBaseline))
	mux.HandleFunc("PUT /api/baseline/{id}", s.live(s.updateBaseline))
	mux.HandleFunc("POST /api/baseline/{id}/duplicate", s.live(s.duplicateBaseline))
	mux.HandleFunc("DELETE /api/baseline/{id}", s.live(s.deleteBaseline))
}

type baselineProvider interface {
	Baseline() *baseline.Store
}

type dataDirProvider interface {
	DataDir() string
}

var (
	baselineStoreMu sync.Mutex
	baselineStores  = map[*server]*baseline.Store{}
)

var errBaselineUnconfigured = errors.New("the baseline library is not configured on this server")

func (s *server) baselineStore() (*baseline.Store, error) {
	if bp, ok := s.be.(baselineProvider); ok {
		if st := bp.Baseline(); st != nil {
			return st, nil
		}
	}

	baselineStoreMu.Lock()
	defer baselineStoreMu.Unlock()
	if st, ok := baselineStores[s]; ok {
		return st, nil
	}

	dataDir := ""
	if dp, ok := s.be.(dataDirProvider); ok {
		dataDir = strings.TrimSpace(dp.DataDir())
	}
	if dataDir == "" {
		if ob, ok := s.be.(observer); ok {
			if o := ob.Observe(); o != nil {
				switch {
				case o.Jobs != nil && o.Jobs.Dir() != "":
					dataDir = filepath.Dir(o.Jobs.Dir())
				case o.Archive != nil && o.Archive.Root() != "":
					dataDir = filepath.Dir(o.Archive.Root())
				}
			}
		}
	}
	if dataDir == "" {
		return nil, errBaselineUnconfigured
	}
	st, err := baseline.Open(dataDir)
	if err != nil {
		return nil, err
	}
	baselineStores[s] = st
	return st, nil
}

func (s *server) writeBaselineErr(w http.ResponseWriter, err error) {
	var unresolved *baseline.UnresolvedPlaceholdersError
	switch {
	case errors.Is(err, errBaselineUnconfigured):
		writeJSON(w, http.StatusServiceUnavailable, errBody(err.Error()))
	case errors.Is(err, baseline.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errBody(err.Error()))
	case errors.Is(err, baseline.ErrAlreadyExists):
		writeJSON(w, http.StatusConflict, errBody(err.Error()))
	case errors.As(err, &unresolved):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":      unresolved.Error(),
			"unresolved": unresolved.Missing,
		})
	case errors.Is(err, baseline.ErrInvalidItem), errors.Is(err, baseline.ErrInvalidTemplate):
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
	default:
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
	}
}

type baselineListResponse struct {
	Items []baseline.Item `json:"items"`
}

type baselineItemResponse struct {
	Item baseline.Item `json:"item"`
}

type baselineSpecBody struct {
	ID              string            `json:"id,omitempty"`
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	CommandTemplate string            `json:"command_template"`
	Defaults        map[string]string `json:"defaults,omitempty"`
	WarmupSec       float64           `json:"warmup_sec,omitempty"`
	Tags            []string          `json:"tags,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

func (b baselineSpecBody) spec() baseline.ItemSpec {
	return baseline.ItemSpec{
		ID:              b.ID,
		Name:            b.Name,
		Description:     b.Description,
		CommandTemplate: b.CommandTemplate,
		Defaults:        b.Defaults,
		WarmupSec:       b.WarmupSec,
		Tags:            b.Tags,
		Metadata:        b.Metadata,
	}
}

func (s *server) listBaseline(w http.ResponseWriter, _ *http.Request) {
	st, err := s.baselineStore()
	if err != nil {
		s.writeBaselineErr(w, err)
		return
	}
	items := st.List()
	if items == nil {
		items = []baseline.Item{}
	}
	writeJSON(w, http.StatusOK, baselineListResponse{Items: items})
}

func (s *server) getBaseline(w http.ResponseWriter, r *http.Request) {
	st, err := s.baselineStore()
	if err != nil {
		s.writeBaselineErr(w, err)
		return
	}
	id := r.PathValue("id")
	item, ok := st.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("baseline item not found"))
		return
	}
	writeJSON(w, http.StatusOK, baselineItemResponse{Item: item})
}

func (s *server) createBaseline(w http.ResponseWriter, r *http.Request) {
	st, err := s.baselineStore()
	if err != nil {
		s.writeBaselineErr(w, err)
		return
	}
	var b baselineSpecBody
	if !decode(w, r, &b) {
		return
	}
	op := operatorFrom(r)
	item, err := st.Create(b.spec(), op)
	if err != nil {
		s.writeBaselineErr(w, err)
		return
	}
	s.log.Info("baseline item created", "component", "panel", "operator", op, "id", item.ID, "name", item.Name)
	writeJSON(w, http.StatusCreated, baselineItemResponse{Item: item})
}

func (s *server) updateBaseline(w http.ResponseWriter, r *http.Request) {
	st, err := s.baselineStore()
	if err != nil {
		s.writeBaselineErr(w, err)
		return
	}
	var b baselineSpecBody
	if !decode(w, r, &b) {
		return
	}
	id := r.PathValue("id")
	op := operatorFrom(r)
	item, err := st.Update(id, b.spec(), op)
	if err != nil {
		s.writeBaselineErr(w, err)
		return
	}
	s.log.Info("baseline item updated", "component", "panel", "operator", op, "id", item.ID, "name", item.Name)
	writeJSON(w, http.StatusOK, baselineItemResponse{Item: item})
}

type duplicateBaselineBody struct {
	Name string `json:"name,omitempty"`
}

func (s *server) duplicateBaseline(w http.ResponseWriter, r *http.Request) {
	st, err := s.baselineStore()
	if err != nil {
		s.writeBaselineErr(w, err)
		return
	}
	var b duplicateBaselineBody
	if r.ContentLength != 0 && !decode(w, r, &b) {
		return
	}
	id := r.PathValue("id")
	op := operatorFrom(r)
	item, err := st.Duplicate(id, b.Name, op)
	if err != nil {
		s.writeBaselineErr(w, err)
		return
	}
	s.log.Info("baseline item duplicated", "component", "panel", "operator", op, "source", id, "id", item.ID)
	writeJSON(w, http.StatusCreated, baselineItemResponse{Item: item})
}

func (s *server) deleteBaseline(w http.ResponseWriter, r *http.Request) {
	st, err := s.baselineStore()
	if err != nil {
		s.writeBaselineErr(w, err)
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
		s.writeBaselineErr(w, err)
		return
	}
	s.log.Info("baseline item deleted", "component", "panel", "operator", operatorFrom(r), "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

type previewBaselineBody struct {
	ID              string            `json:"id,omitempty"`
	CommandTemplate string            `json:"command_template,omitempty"`
	Defaults        map[string]string `json:"defaults,omitempty"`
	Inputs          map[string]string `json:"inputs,omitempty"`
}

func (s *server) previewBaseline(w http.ResponseWriter, r *http.Request) {
	st, err := s.baselineStore()
	if err != nil {
		s.writeBaselineErr(w, err)
		return
	}
	var b previewBaselineBody
	if !decode(w, r, &b) {
		return
	}

	tmpl := b.CommandTemplate
	defs := b.Defaults
	if strings.TrimSpace(tmpl) == "" && strings.TrimSpace(b.ID) != "" {
		item, ok := st.Get(strings.TrimSpace(b.ID))
		if !ok {
			writeJSON(w, http.StatusNotFound, errBody("baseline item not found"))
			return
		}
		tmpl = item.CommandTemplate
		if defs == nil {
			defs = item.Defaults
		}
	}

	res, err := baseline.Render(tmpl, defs, b.Inputs)
	if err != nil {
		var unresolved *baseline.UnresolvedPlaceholdersError
		if errors.As(err, &unresolved) {
			placeholders, _ := baseline.ValidateTemplate(tmpl)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":        unresolved.Error(),
				"unresolved":   unresolved.Missing,
				"placeholders": placeholders,
			})
			return
		}
		s.writeBaselineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
