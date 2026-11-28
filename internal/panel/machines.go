package panel

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/version"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

// Machine service-control (stop/start) lives solely on the enrollment API
// (/api/enroll/machines/{id}/stop|start, internal/enroll).

type machineResponse struct {
	Machine       fleet.MachineInfo `json:"machine"`
	Watchdog      *hostState        `json:"watchdog,omitempty"`
	ServerVersion string            `json:"server_version"`
}

func (s *server) getMachine(w http.ResponseWriter, r *http.Request) {
	info, ok := s.be.Machine(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("unknown machine"))
		return
	}
	if info.PrivateIPs == nil {
		info.PrivateIPs = []string{}
	}
	writeJSON(w, http.StatusOK, machineResponse{
		Machine:       info,
		Watchdog:      s.machineWatchdogState(info),
		ServerVersion: s.serverVersion(),
	})
}

func (s *server) machineWatchdogState(m fleet.MachineInfo) *hostState {
	if m.Removed || m.Role != registry.RoleWorker {
		return nil
	}
	ob, ok := s.be.(observer)
	if !ok {
		return nil
	}
	o := ob.Observe()
	if o == nil {
		return nil
	}
	var pf *watchdog.Profile
	if o.Profile != nil {
		if cur, err := o.Profile.Current(); err != nil {
			s.log.Warn("watchdog: stored policy", "component", "panel", "err", err)
		} else {
			pf = cur
		}
	}
	pol := watchdog.DefaultPolicy()
	if pf != nil {
		if p, err := pf.Effective(m.ID, nil); err == nil {
			pol = p
		}
	}
	h := deriveHost(ob.WatchdogEvents(m.ID), pol.AutoResetMidnight, time.Now())
	h.Machine, h.Connection = m.ID, m.State
	h.RestartsX, h.RebootsX = pol.Caps.RestartsPerDay, pol.Caps.RebootsPerDay
	if st, ok := ob.WatchdogStatus(m.ID); ok {
		h.PolicyPushed, h.PolicyInForc, h.PolicyAcked, h.PolicyOK, h.PolicyErr = st.Pushed, st.InForce, st.Acked, st.AckOK, st.AckErr
	}
	if m.State != fleet.StateOnline {
		h.Condition = "not-reporting"
	}
	return &h
}

const updateDispatchTimeout = 15 * time.Second

func (s *server) serverVersion() string {
	type serverVersionProvider interface {
		ServerVersion() string
	}
	if p, ok := s.be.(serverVersionProvider); ok {
		if v := p.ServerVersion(); v != "" {
			return v
		}
	}
	return version.Version
}

func (s *server) updateMachine(w http.ResponseWriter, r *http.Request) {
	var b struct{}
	if r.ContentLength != 0 && !decode(w, r, &b) {
		return
	}
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), updateDispatchTimeout)
	defer cancel()
	out, err := s.be.UpdateAgent(ctx, id)
	if err != nil {
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
	s.log.Info("agent self-update triggered", "component", "panel", "operator", operatorFrom(r),
		"machine", id, "status", out.Status, "target_version", out.TargetVersion)
	writeJSON(w, http.StatusOK, out)
}

type updateOutdatedBody struct {
	WorkersOnly bool `json:"workers_only"`
}

func (s *server) updateOutdatedMachines(w http.ResponseWriter, r *http.Request) {
	var b updateOutdatedBody
	if r.ContentLength != 0 && !decode(w, r, &b) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), updateDispatchTimeout)
	defer cancel()
	results := s.be.UpdateOutdated(ctx, b.WorkersOnly)
	dispatched, failed := 0, 0
	for _, res := range results {
		switch res.Status {
		case "dispatched":
			dispatched++
		case "failed":
			failed++
		}
	}
	s.log.Info("bulk agent self-update triggered", "component", "panel", "operator", operatorFrom(r),
		"workers_only", b.WorkersOnly, "dispatched", dispatched, "failed", failed)
	writeJSON(w, http.StatusOK, map[string]any{
		"server_version": s.serverVersion(),
		"results":        results,
		"dispatched":     dispatched,
		"failed":         failed,
	})
}
