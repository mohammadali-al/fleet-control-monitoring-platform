package panel

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/backup"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
)

// Backup page API (internal/backup, T-0013).
//
//	GET  /api/backup                    server record, last run, schedule, running job, today's reminder
//	GET  /api/backup/diff               what main has that the backup lacks (reads the backup server's index)
//	POST /api/backup/server             add or replace the backup server (password used once)
//	POST /api/backup/run                back up now (in the background; poll GET /api/backup)
//	POST /api/backup/import             restore from the backup server (confirm required)
//	POST /api/backup/schedule           {enabled, hour, minute}
//	POST /api/backup/reminder/dismiss   hide today's reminder
func (s *server) routesBackup(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/backup", s.observed(s.bkStatus))
	mux.HandleFunc("GET /api/backup/diff", s.observed(s.bkDiff))
	mux.HandleFunc("POST /api/backup/server", s.observed(s.bkAddServer))
	mux.HandleFunc("POST /api/backup/run", s.observed(s.bkRun))
	mux.HandleFunc("POST /api/backup/import", s.observed(s.bkImport))
	mux.HandleFunc("POST /api/backup/schedule", s.observed(s.bkSchedule))
	mux.HandleFunc("POST /api/backup/reminder/dismiss", s.observed(s.bkDismiss))
}

type bkServer struct {
	Host       string    `json:"host"`
	Port       int       `json:"port,omitempty"`
	User       string    `json:"user"`
	Auth       string    `json:"auth,omitempty"`
	HostKeyFP  string    `json:"host_key_fp,omitempty"`
	AddedAt    time.Time `json:"added_at"`
	RemotePath string    `json:"remote_path"`
}

func serverView(srv backup.Server) *bkServer {
	return &bkServer{Host: srv.Host, Port: srv.Port, User: srv.User, Auth: srv.Auth, HostKeyFP: srv.HostKeyFP,
		AddedAt: srv.AddedAt, RemotePath: srv.RemotePath()}
}

type bkSchedule struct {
	Enabled bool   `json:"enabled"`
	Hour    int    `json:"hour"`
	Minute  int    `json:"minute"`
	Text    string `json:"text"`
	Next    string `json:"next,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func scheduleView(sc backup.Schedule, withNext bool) bkSchedule {
	v := bkSchedule{Enabled: sc.Enabled, Hour: sc.Hour, Minute: sc.Minute, Text: sc.String(), Reason: sc.Reason}
	if withNext && sc.Enabled {
		v.Next = sc.Next(time.Now()).Format(time.RFC3339)
	}
	return v
}

func (s *server) bkStatus(w http.ResponseWriter, _ *http.Request, ob observer, o *fleet.Observe) {
	if o.Backup == nil {
		notConfigured(w, "the backup service")
		return
	}
	out := map[string]any{
		"configured": false,
		"schedule":   scheduleView(o.Backup.Schedule(), true),
		"suggested":  scheduleView(backup.SuggestSchedule(), false),
		"set":        backup.SetDirs(),
	}
	if srv, ok := o.Backup.Server(); ok {
		out["configured"] = true
		out["server"] = serverView(srv)
	}
	if last, ok := o.Backup.LastBackup(); ok {
		out["last_backup"] = last
	}
	if job, ok := ob.BackupJobStatus(); ok {
		out["job"] = job
	}
	if rem, ok := ob.BackupReminder(time.Now()); ok {
		out["reminder"] = map[string]string{"kind": rem.Kind, "message": rem.Message}
	}
	writeJSON(w, http.StatusOK, out)
}

// diff groups, in display order. The CA group reports counts only: nothing of
// the key material (not even file names) is shown.
var diffGroups = []struct{ dir, label string }{
	{"archive", "Evidence archive"},
	{"monitor", "Monitor data"},
	{"projects", "Maneuver and project metadata"},
	{"registry", "Machine registry"},
	{"ca", "Internal CA keys"},
}

type bkDiffGroup struct {
	Dir        string `json:"dir"`
	Label      string `json:"label"`
	Missing    int    `json:"missing"`
	Changed    int    `json:"changed"`
	Bytes      int64  `json:"bytes"`
	BackupOnly int    `json:"backup_only"`
}

func diffView(d *backup.DiffResult) map[string]any {
	groups := make([]bkDiffGroup, len(diffGroups))
	idx := map[string]int{}
	for i, g := range diffGroups {
		groups[i] = bkDiffGroup{Dir: g.dir, Label: g.label}
		idx[g.dir] = i
	}
	dirOf := func(p string) string { d, _, _ := strings.Cut(p, "/"); return d }
	for _, e := range d.Missing {
		if i, ok := idx[dirOf(e.Path)]; ok {
			groups[i].Missing++
			groups[i].Bytes += e.Size
		}
	}
	for _, e := range d.Changed {
		if i, ok := idx[dirOf(e.Path)]; ok {
			groups[i].Changed++
			groups[i].Bytes += e.Size
		}
	}
	for _, p := range d.BackupOnly {
		if i, ok := idx[dirOf(p)]; ok {
			groups[i].BackupOnly++
		}
	}
	return map[string]any{
		"computed": d.Computed, "in_sync": d.InSync(), "present": d.Present, "pending_bytes": d.Pending,
		"missing": len(d.Missing), "changed": len(d.Changed), "backup_only": len(d.BackupOnly),
		"scan_errors": len(d.ScanErrors), "groups": groups,
	}
}

func (s *server) bkDiff(w http.ResponseWriter, r *http.Request, _ observer, o *fleet.Observe) {
	if o.Backup == nil {
		notConfigured(w, "the backup service")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	d, err := o.Backup.Diff(ctx)
	if err != nil {
		// Unreachable backup server: say so; not an internal error.
		code := statusFor(err)
		writeJSON(w, code, map[string]any{"error": err.Error(), "reachable": false})
		return
	}
	v := diffView(d)
	v["reachable"] = true
	writeJSON(w, http.StatusOK, v)
}

type bkServerBody struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
}

func (s *server) bkAddServer(w http.ResponseWriter, r *http.Request, _ observer, o *fleet.Observe) {
	if o.Backup == nil {
		notConfigured(w, "the backup service")
		return
	}
	var b bkServerBody
	if !decode(w, r, &b) {
		return
	}
	b.Host, b.User = strings.TrimSpace(b.Host), strings.TrimSpace(b.User)
	switch {
	case b.Host == "" || b.User == "":
		writeJSON(w, http.StatusBadRequest, errBody("the backup server's address and username are required"))
		return
	case b.Password == "":
		writeJSON(w, http.StatusBadRequest, errBody("the password is required (it is used once, to install a key)"))
		return
	case b.Port < 0 || b.Port > 65535:
		writeJSON(w, http.StatusBadRequest, errBody("port is out of range"))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	// The password is never logged; the service zeroes the slice.
	s.log.Info("add backup server", "component", "panel", "operator", operatorFrom(r), "host", b.Host, "user", b.User)
	res, err := o.Backup.AddServer(ctx, backup.Server{Host: b.Host, Port: b.Port, User: b.User}, []byte(b.Password))
	b.Password = ""
	if err != nil {
		s.log.Warn("add backup server failed", "component", "panel", "host", b.Host, "err", err)
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	out := map[string]any{"server": serverView(res.Server), "suggested": scheduleView(res.Suggested, false)}
	if res.Diff != nil {
		out["diff"] = diffView(res.Diff)
	}
	if res.DiffErr != nil {
		out["diff_error"] = res.DiffErr.Error()
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *server) bkRun(w http.ResponseWriter, r *http.Request, ob observer, _ *fleet.Observe) {
	s.log.Info("back up now", "component", "panel", "operator", operatorFrom(r))
	if err := ob.StartBackup(); err != nil {
		writeErr(w, err)
		return
	}
	job, _ := ob.BackupJobStatus()
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

type importBody struct {
	Confirm   bool `json:"confirm"`
	Overwrite bool `json:"overwrite"`
}

func (s *server) bkImport(w http.ResponseWriter, r *http.Request, ob observer, _ *fleet.Observe) {
	var b importBody
	if !decode(w, r, &b) {
		return
	}
	if !b.Confirm {
		writeJSON(w, http.StatusBadRequest, errBody("confirm the import first"))
		return
	}
	s.log.Warn("import from backup", "component", "panel", "operator", operatorFrom(r), "overwrite", b.Overwrite)
	if err := ob.StartImport(b.Overwrite); err != nil {
		writeErr(w, err)
		return
	}
	job, _ := ob.BackupJobStatus()
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

type scheduleBody struct {
	Enabled bool `json:"enabled"`
	Hour    int  `json:"hour"`
	Minute  int  `json:"minute"`
}

func (s *server) bkSchedule(w http.ResponseWriter, r *http.Request, _ observer, o *fleet.Observe) {
	if o.Backup == nil {
		notConfigured(w, "the backup service")
		return
	}
	var b scheduleBody
	if !decode(w, r, &b) {
		return
	}
	if b.Hour < 0 || b.Hour > 23 || b.Minute < 0 || b.Minute > 59 {
		writeJSON(w, http.StatusBadRequest, errBody("the time must be a valid hour and minute"))
		return
	}
	sc := backup.Schedule{Enabled: b.Enabled, Hour: b.Hour, Minute: b.Minute}
	if err := o.Backup.SetSchedule(sc); err != nil {
		writeErr(w, err)
		return
	}
	s.log.Info("backup schedule set", "component", "panel", "operator", operatorFrom(r), "schedule", sc.String(), "enabled", sc.Enabled)
	writeJSON(w, http.StatusOK, map[string]any{"schedule": scheduleView(sc, true)})
}

func (s *server) bkDismiss(w http.ResponseWriter, _ *http.Request, ob observer, _ *fleet.Observe) {
	ob.DismissBackupReminder(time.Now())
	writeJSON(w, http.StatusOK, map[string]bool{"dismissed": true})
}
