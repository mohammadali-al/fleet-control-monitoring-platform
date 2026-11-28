package panel

import (
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
		ServerVersion: version.Version,
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
