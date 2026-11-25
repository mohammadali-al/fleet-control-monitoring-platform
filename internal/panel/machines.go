package panel

import (
	"context"
	"net/http"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
)

const serviceControlTimeout = 15 * time.Second

// serviceController is the part of the backend that sends service-control
// actions to an agent (*fleet.Manager implements it).
type serviceController interface {
	ServiceControl(ctx context.Context, machine, action string) (channel.ServiceControlResultPayload, error)
}

type serviceControlResponse struct {
	Machine string `json:"machine"`
	Action  string `json:"action"`
	OK      bool   `json:"ok"`
	State   string `json:"state,omitempty"`
}

func (s *server) routesMachines(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/machines/{id}/stop", s.live(s.serviceControlMachine(channel.ServiceActionStop)))
	mux.HandleFunc("POST /api/machines/{id}/start", s.live(s.serviceControlMachine(channel.ServiceActionStart)))
	mux.HandleFunc("GET /api/machines/{id}/service-status", s.live(s.serviceControlMachine(channel.ServiceActionStatus)))
}

// serviceControlMachine runs stop/start/status on an enrolled/provisioned agent
// of any role (worker or monitor) over the control channel.
func (s *server) serviceControlMachine(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, ok := s.be.(serviceController)
		if !ok {
			notConfigured(w, "service control")
			return
		}
		if r.Method == http.MethodPost && r.ContentLength != 0 {
			var body struct{}
			if !decode(w, r, &body) {
				return
			}
		}
		id := r.PathValue("id")
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), serviceControlTimeout)
		defer cancel()
		s.log.Info("service control", "component", "panel", "operator", operatorFrom(r), "machine", id, "action", action)
		res, err := sc.ServiceControl(ctx, id, action)
		if err != nil {
			s.log.Warn("service control failed", "component", "panel", "machine", id, "action", action, "err", err)
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, serviceControlResponse{
			Machine: id,
			Action:  res.Action,
			OK:      res.OK,
			State:   res.State,
		})
	}
}
