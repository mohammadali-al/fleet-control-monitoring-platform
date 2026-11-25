package panel

import (
	"context"
	"net/http"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/backup"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
)

// observer is what the Observe/Admin pages (Evidence, Backup, Monitor data,
// Watchdog) need beyond Backend. *fleet.Manager implements it; a Backend that
// does not, or a manager with no Observe backends bound, answers those APIs
// with 503 and the pages render empty.
type observer interface {
	Observe() *fleet.Observe

	PullEvidence(machine string) error
	SyncStatuses() map[string]fleet.SyncStatus

	StartBackup() error
	StartImport(overwrite bool) error
	BackupJobStatus() (fleet.BackupJob, bool)
	BackupReminder(now time.Time) (backup.Reminder, bool)
	DismissBackupReminder(now time.Time)

	PushMonitorConfigs(ctx context.Context)
	WatchdogEvents(machine string) []fleet.WatchdogEvent
	WatchdogStatus(machine string) (fleet.WatchdogStatus, bool)
	PushPolicies(ctx context.Context)
	SubscribeWatchdog() (<-chan fleet.WatchdogEvent, func())
}

func (s *server) routesObserve(mux *http.ServeMux) {
	s.routesEvidence(mux)
	s.routesBackup(mux)
}

// observed wraps a handler of an Observe/Admin API: it needs the live backend
// and a bound Observe set.
func (s *server) observed(h func(w http.ResponseWriter, r *http.Request, ob observer, o *fleet.Observe)) http.HandlerFunc {
	return s.live(func(w http.ResponseWriter, r *http.Request) {
		ob, ok := s.be.(observer)
		var o *fleet.Observe
		if ok {
			o = ob.Observe()
		}
		if o == nil {
			writeJSON(w, http.StatusServiceUnavailable, errBody("this server has no evidence, backup, monitor or watchdog backend configured"))
			return
		}
		h(w, r, ob, o)
	})
}

// notConfigured answers 503 for a backend this server did not bind.
func notConfigured(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusServiceUnavailable, errBody(what+" is not configured on this server"))
}
