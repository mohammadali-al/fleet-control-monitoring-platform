package backup

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// DiffResult is the data diff between main and the backup (architecture 7.5):
// what main has that the backup lacks (or holds with a different digest).
type DiffResult struct {
	Computed   time.Time
	Missing    []FileEntry // on main, not on the backup
	Changed    []FileEntry // on both, different SHA-256 (main's entry)
	BackupOnly []string    // on the backup only (kept: the backup is never pruned)
	Present    int         // on both with the same SHA-256
	Pending    int64       // bytes a backup would send (Missing + Changed)
	ScanErrors []FileError // main files that could not be read
}

// InSync reports whether a backup would have nothing to send.
func (d *DiffResult) InSync() bool { return len(d.Missing) == 0 && len(d.Changed) == 0 }

// Diff compares main's backup set with what the backup server's index says it
// holds. It sends nothing and changes nothing. The index is written by the
// main server after each verified upload, so the diff is cheap (no hashing on
// the backup); a backup run still probes the real file before skipping it.
func (s *Service) Diff(ctx context.Context) (*DiffResult, error) {
	srv, ok := s.Server()
	if !ok {
		return nil, ErrNoServer
	}
	rem, err := s.connect(ctx, srv)
	if err != nil {
		return nil, err
	}
	defer rem.Close()
	ix, err := readIndex(ctx, rem)
	if err != nil {
		return nil, err
	}
	local, scanErrs := s.scan()
	d := diffEntries(local, ix.Files)
	d.Computed = s.now()
	d.ScanErrors = scanErrs
	return d, nil
}

func diffEntries(local, remote []FileEntry) *DiffResult {
	d := &DiffResult{}
	rm := make(map[string]FileEntry, len(remote))
	for _, r := range remote {
		rm[r.Path] = r
	}
	seen := make(map[string]bool, len(local))
	for _, l := range local {
		seen[l.Path] = true
		r, ok := rm[l.Path]
		switch {
		case !ok:
			d.Missing = append(d.Missing, l)
			d.Pending += l.Size
		case r.SHA256 != l.SHA256:
			d.Changed = append(d.Changed, l)
			d.Pending += l.Size
		default:
			d.Present++
		}
	}
	for p := range rm {
		if !seen[p] {
			d.BackupOnly = append(d.BackupOnly, p)
		}
	}
	sort.Strings(d.BackupOnly)
	return d
}

// ---- schedule ----

// Schedule is a daily backup time (local time of the main server).
type Schedule struct {
	Enabled bool   `json:"enabled"`
	Hour    int    `json:"hour"`
	Minute  int    `json:"minute"`
	Reason  string `json:"reason,omitempty"`
}

// Suggested schedule: around the middle of the night, when no test is likely
// to be running and the archive is quiet.
const (
	SuggestedHour   = 2
	SuggestedMinute = 30
)

// SuggestSchedule returns the suggested nightly schedule (02:30 local time),
// not enabled; SetSchedule accepts it.
func SuggestSchedule() Schedule {
	return Schedule{Hour: SuggestedHour, Minute: SuggestedMinute,
		Reason: "around the middle of the night, when the archive is quiet"}
}

// String renders the schedule as "daily at HH:MM".
func (s Schedule) String() string { return fmt.Sprintf("daily at %02d:%02d", s.Hour, s.Minute) }

// Next returns the first run time strictly after from, in from's location.
func (s Schedule) Next(from time.Time) time.Time {
	n := time.Date(from.Year(), from.Month(), from.Day(), s.Hour, s.Minute, 0, 0, from.Location())
	if !n.After(from) {
		n = time.Date(from.Year(), from.Month(), from.Day()+1, s.Hour, s.Minute, 0, 0, from.Location())
	}
	return n
}

// Schedule returns the configured schedule (zero value, disabled, if none).
func (s *Service) Schedule() Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Schedule == nil {
		return Schedule{}
	}
	return *s.st.Schedule
}

// SetSchedule stores the nightly schedule. A running Run loop picks it up at
// its next wake-up (within a minute).
func (s *Service) SetSchedule(sc Schedule) error {
	if sc.Hour < 0 || sc.Hour > 23 || sc.Minute < 0 || sc.Minute > 59 {
		return fmt.Errorf("backup: invalid schedule time %02d:%02d", sc.Hour, sc.Minute)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Schedule = &sc
	return s.saveLocked()
}

// Run executes the nightly schedule until ctx ends: when the schedule is
// enabled and a server is configured it calls BackupNow at the scheduled time.
// A failed run is logged and retried the next night; the operator can always
// use BackupNow.
func (s *Service) Run(ctx context.Context) {
	for {
		sc := s.Schedule()
		wait := time.Minute // re-read the schedule at least once a minute
		var next time.Time
		if _, ok := s.Server(); ok && sc.Enabled {
			next = sc.Next(s.now())
			if d := next.Sub(s.now()); d < wait {
				wait = d
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if next.IsZero() || s.now().Before(next) {
			continue
		}
		if _, err := s.BackupNow(ctx); err != nil {
			s.log.Warn("backup: scheduled run did not complete", "err", err)
		}
	}
}

// ---- reminder ----

// Reminder kinds.
const (
	ReminderNoServer = "no-server" // no backup server is configured
	ReminderStale    = "stale"     // no successful backup in the last 24 hours
)

// Reminder is a non-intrusive nudge for the panel to show (for example as a
// badge or a line on the Backup page). It never blocks and never interrupts.
type Reminder struct {
	Kind    string
	Message string
}

// ReminderInterval is how recent a successful backup must be to need no nudge.
const ReminderInterval = 24 * time.Hour

// Reminder returns a reminder when one is due and, at most once per local
// calendar day, marks it as delivered. It returns immediately: it takes only
// the state mutex (never held across a backup, a scan or the network), so a
// running backup does not delay it, and a failure to persist the date is
// ignored (the worst case is one extra reminder after a restart).
func (s *Service) Reminder(now time.Time) (Reminder, bool) {
	day := now.Format("2006-01-02")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.LastReminder == day {
		return Reminder{}, false
	}
	var r Reminder
	switch {
	case s.st.Server == nil:
		r = Reminder{Kind: ReminderNoServer, Message: "No backup server is configured; the archive exists in one place only."}
	case s.st.LastBackup == nil || !s.st.LastBackup.OK || now.Sub(s.st.LastBackup.Finished) > ReminderInterval:
		r = Reminder{Kind: ReminderStale, Message: "There has been no successful backup in the last day."}
	default:
		return Reminder{}, false
	}
	s.st.LastReminder = day
	if err := s.saveLocked(); err != nil {
		s.log.Debug("backup: could not record the reminder date", "err", err)
	}
	return r, true
}
