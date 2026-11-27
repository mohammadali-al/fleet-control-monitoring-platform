package panel

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

const refreshPublicIPTimeout = 15 * time.Second

func (s *server) refreshPublicIP(w http.ResponseWriter, r *http.Request) {
	var b struct{}
	if r.ContentLength != 0 && !decode(w, r, &b) {
		return
	}
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), refreshPublicIPTimeout)
	defer cancel()
	if err := s.be.RefreshPublicIP(ctx, id); err != nil {
		switch {
		case errors.Is(err, fleet.ErrUnknown), errors.Is(err, registry.ErrNotFound):
			writeJSON(w, http.StatusNotFound, errBody("unknown machine: "+id))
		case errors.Is(err, fleet.ErrOffline):
			writeJSON(w, http.StatusConflict, errBody("agent offline: "+id+" is not connected"))
		default:
			writeErr(w, err)
		}
		return
	}
	s.log.Info("public IP refresh requested", "component", "panel", "operator", operatorFrom(r), "machine", id)
	writeJSON(w, http.StatusOK, map[string]string{"machine": id, "status": "requested"})
}
