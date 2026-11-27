package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/backup"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/evidence"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// The Observe and Admin half of the manager: the backends behind the panel's
// Evidence, Backup, Monitor data and Watchdog pages. They are optional; a
// Manager without Observe simply answers those pages with "not configured".

// ErrBusy means the operation is already running (a pull for that machine, a
// backup, an import). The panel maps it to 409.
var ErrBusy = errors.New("fleet: already running")

// Observe is the set of backends the Observe/Admin pages read and drive. Any
// field may be nil (that page then reports that it is not configured).
type Observe struct {
	// Archive is the main-server evidence archive (internal/evidence).
	Archive *evidence.Archive
	// Backup is the backup service (internal/backup).
	Backup *backup.Service
	// Jobs are the monitor jobs; Series holds the recent probe results (it is
	// the live monitor sink). Store, when non-nil, is the persistent
	// time-series store (VictoriaMetrics) fed alongside Series and queried for
	// history ranges. Ingest is built from them by SetObserve.
	Jobs   *monitor.Jobs
	Series *Series
	Store  monitor.HistoryStore
	// Profile is the stored watchdog policy profile.
	Profile *PolicyStore
	// Log receives errors of background runs.
	Log *slog.Logger
}

// ObserveOptions tunes OpenObserveWithOptions.
type ObserveOptions struct {
	Logger       *slog.Logger
	MonitorStore config.MonitorStoreConfig
}

// OpenObserve opens every backend under dataDir (D-0006): the archive
// (archive/), the backup state (config/), the monitor jobs (monitor/) and the
// watchdog profile (watchdog/).
func OpenObserve(dataDir string, log *slog.Logger) (*Observe, error) {
	return OpenObserveWithOptions(dataDir, ObserveOptions{Logger: log})
}

// OpenObserveWithOptions opens the Observe backends with optional settings such
// as the persistent monitor time-series store.
func OpenObserveWithOptions(dataDir string, opt ObserveOptions) (*Observe, error) {
	log := opt.Logger
	if log == nil {
		log = slog.Default()
	}
	arch, err := evidence.OpenArchive(dataDir, evidence.ArchiveOptions{Log: log})
	if err != nil {
		return nil, err
	}
	bk, err := backup.Open(dataDir, backup.Options{Log: log})
	if err != nil {
		return nil, err
	}
	jobs, err := monitor.OpenJobs(dataDir)
	if err != nil {
		return nil, err
	}
	var tsdb monitor.HistoryStore
	if opt.MonitorStore.Addr != "" {
		vm, err := monitor.NewVMStore(monitor.VMStoreConfig{
			Addr:      opt.MonitorStore.Addr,
			Retention: opt.MonitorStore.RetentionOrDefault(),
		})
		if err != nil {
			return nil, err
		}
		tsdb = vm
	}
	return &Observe{Archive: arch, Backup: bk, Jobs: jobs, Series: NewSeries(0), Store: tsdb, Profile: NewPolicyStore(dataDir), Log: log}, nil
}

// SetObserve binds the Observe backends: it routes evidence sync traffic,
// binds the monitor jobs and sink (SetMonitor) and the watchdog profile
// (SetWatchdog), and starts the backup Run loop (it ends with the manager's
// context). Call it before Start, or after (the loop then starts at once).
func (m *Manager) SetObserve(o *Observe) {
	if o == nil {
		return
	}
	if o.Log == nil {
		o.Log = m.log
	}
	m.omx.Lock()
	m.obs = o
	m.omx.Unlock()

	if o.Jobs != nil {
		src := MonitorSource{ConfigFor: o.Jobs.ConfigFor}
		var sinks []monitor.Sink
		if o.Series != nil {
			sinks = append(sinks, o.Series)
		}
		if o.Store != nil {
			sinks = append(sinks, o.Store)
		}
		switch len(sinks) {
		case 1:
			src.Ingest = monitor.NewIngestor(sinks[0], o.Jobs.HistoryKeyFor)
		case 2:
			src.Ingest = monitor.NewIngestor(monitor.NewMultiSink(sinks...), o.Jobs.HistoryKeyFor)
		}
		m.SetMonitor(src)
	}
	if o.Profile != nil {
		m.SetWatchdog(WatchdogSource{
			PolicyFor: o.Profile.PolicyFor,
			OnEvent:   func(_ string, ev WatchdogEvent) { m.publishWatchdog(ev) },
		})
	}
	m.startObserveLoops()
}

// EnableObserve opens the backends under dataDir and binds them
// (OpenObserveWithOptions + SetObserve).
func (m *Manager) EnableObserve(dataDir string) error {
	o, err := OpenObserveWithOptions(dataDir, ObserveOptions{
		Logger:       m.log,
		MonitorStore: m.cfg.MonitorStore,
	})
	if err != nil {
		return err
	}
	m.SetObserve(o)
	return nil
}

// Observe returns the bound backends, or nil when none were bound.
func (m *Manager) Observe() *Observe {
	m.omx.Lock()
	defer m.omx.Unlock()
	return m.obs
}

// startObserveLoops starts the backup Run loop once, when the manager is
// running and a backup service is bound.
func (m *Manager) startObserveLoops() {
	m.mu.Lock()
	ctx, closed := m.ctx, m.closed
	m.mu.Unlock()
	m.omx.Lock()
	o := m.obs
	start := ctx != nil && !closed && o != nil && o.Backup != nil && !m.loopsStarted
	if start {
		m.loopsStarted = true
	}
	m.omx.Unlock()
	if !start {
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		o.Backup.Run(ctx)
	}()
}

// ---- evidence sync ----

// SyncStatus is the state of the evidence pull of one machine.
type SyncStatus struct {
	Machine  string    `json:"machine"`
	State    string    `json:"state"` // idle | pulling | done | failed
	Started  time.Time `json:"started,omitempty"`
	Finished time.Time `json:"finished,omitempty"`
	Trigger  string    `json:"trigger,omitempty"` // pull | push
	// The last finished sync: segments in the agent's manifest, fetched this
	// time, resumed, rejected (bad SHA-256) and already archived.
	Segments int    `json:"segments"`
	Fetched  int    `json:"fetched"`
	Resumed  int    `json:"resumed"`
	Rejected int    `json:"rejected"`
	Skipped  int    `json:"skipped"`
	Bytes    int64  `json:"bytes"`
	Error    string `json:"error,omitempty"`
}

const pullTimeout = 30 * time.Minute

// syncerFor returns the evidence syncer of a node, creating it on first use.
func (m *Manager) syncerFor(n *node) *evidence.Syncer {
	o := m.Observe()
	if o == nil || o.Archive == nil {
		return nil
	}
	m.omx.Lock()
	defer m.omx.Unlock()
	if s := m.syncers[n.id]; s != nil && s.node == n {
		return s.syncer
	}
	id := n.id
	sy, err := evidence.NewSyncer(evidence.SyncerConfig{
		Machine: id, Archive: o.Archive, Sender: senderFunc(func(ctx context.Context, env channel.Envelope) error { return n.link.Send(ctx, env) }),
		OnPush: func(res evidence.Result, err error) { m.finishSync(id, "push", time.Now(), res, err) },
		Log:    m.log,
	})
	if err != nil {
		m.log.Warn("evidence syncer", "component", "fleet", "machine", id, "err", err)
		return nil
	}
	m.syncers[id] = &nodeSyncer{node: n, syncer: sy}
	return sy
}

type nodeSyncer struct {
	node   *node
	syncer *evidence.Syncer
}

// evidenceHandle routes an ev-manifest or ev-chunk envelope from a worker
// agent to its syncer (a pull waiting for it, or an idle-push offer).
func (m *Manager) evidenceHandle(ctx context.Context, n *node, env channel.Envelope) {
	if sy := m.syncerFor(n); sy != nil {
		sy.Handle(ctx, env)
		return
	}
	m.log.Debug("evidence message dropped: no archive configured", "component", "fleet", "machine", n.id, "type", env.Type)
}

func (m *Manager) finishSync(machine, trigger string, started time.Time, res evidence.Result, err error) {
	st := SyncStatus{
		Machine: machine, State: "done", Started: started, Finished: time.Now(), Trigger: trigger,
		Segments: len(res.Fetched) + res.Skipped + len(res.Rejected), Fetched: len(res.Fetched), Resumed: len(res.Resumed),
		Rejected: len(res.Rejected), Skipped: res.Skipped, Bytes: res.Bytes,
	}
	if err != nil {
		st.State, st.Error = "failed", err.Error()
	}
	m.omx.Lock()
	m.sync[machine] = st
	m.omx.Unlock()
	m.log.Info("evidence sync finished", "component", "fleet", "machine", machine, "trigger", trigger, "state", st.State,
		"fetched", st.Fetched, "rejected", st.Rejected, "bytes", st.Bytes, "err", st.Error)
}

// PullEvidence starts a pull from one worker agent in the background: its
// manifest is compared with the archive and the missing or changed segments
// are fetched and verified. Progress and the outcome show in SyncStatuses.
func (m *Manager) PullEvidence(machine string) error {
	o := m.Observe()
	if o == nil || o.Archive == nil {
		return ErrUnavailable
	}
	n, err := m.target(machine)
	if err != nil {
		return err
	}
	if mc, ok := m.cfg.Registry.Get(machine); !ok || mc.Role != registry.RoleWorker {
		return ErrUnknown
	}
	sy := m.syncerFor(n)
	if sy == nil {
		return ErrUnavailable
	}
	if err := o.Archive.CheckIntake(); err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	started := time.Now()
	m.omx.Lock()
	if m.sync[machine].State == "pulling" {
		m.omx.Unlock()
		return ErrBusy
	}
	prev := m.sync[machine]
	m.sync[machine] = SyncStatus{Machine: machine, State: "pulling", Started: started, Trigger: "pull",
		Segments: prev.Segments, Fetched: prev.Fetched, Skipped: prev.Skipped, Rejected: prev.Rejected}
	m.omx.Unlock()

	m.mu.Lock()
	parent := m.ctx
	m.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(parent, pullTimeout)
		defer cancel()
		res, err := sy.Pull(ctx)
		m.finishSync(machine, "pull", started, res, err)
	}()
	return nil
}

// SyncStatuses returns the last known evidence sync state per machine.
func (m *Manager) SyncStatuses() map[string]SyncStatus {
	m.omx.Lock()
	defer m.omx.Unlock()
	out := make(map[string]SyncStatus, len(m.sync))
	for k, v := range m.sync {
		out[k] = v
	}
	return out
}

// ---- backup runs ----

// BackupJob is the state of the last operator-started backup or import.
type BackupJob struct {
	Kind     string    `json:"kind"`  // backup | import
	State    string    `json:"state"` // running | done | failed
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Summary  string    `json:"summary,omitempty"`
	Error    string    `json:"error,omitempty"`
	Uploaded int       `json:"uploaded"`
	Replaced int       `json:"replaced"`
	Same     int       `json:"unchanged"`
	Failed   int       `json:"failed"`
	Bytes    int64     `json:"bytes"`
	// Conflicts lists local files an import kept because they differ.
	Conflicts []string `json:"conflicts,omitempty"`
}

// StartBackup runs "back up now" in the background. ErrBusy if a backup or
// import started here is running; backup.ErrBackupRunning covers a scheduled
// run that is in progress.
func (m *Manager) StartBackup() error {
	return m.startBackupJob("backup", func(ctx context.Context, bk *backup.Service) BackupJob {
		res, err := bk.BackupNow(ctx)
		j := BackupJob{}
		if res != nil {
			j.Uploaded, j.Replaced, j.Same, j.Failed, j.Bytes = len(res.Uploaded), len(res.Replaced), res.Unchanged, len(res.Errors), res.Bytes
		}
		if err != nil {
			j.Error = err.Error()
		} else {
			j.Summary = fmt.Sprintf("%d file(s) uploaded, %d already on the backup", j.Uploaded, j.Same)
		}
		return j
	})
}

// StartImport restores from the backup server in the background.
func (m *Manager) StartImport(overwrite bool) error {
	return m.startBackupJob("import", func(ctx context.Context, bk *backup.Service) BackupJob {
		res, err := bk.Restore(ctx, backup.RestoreOptions{Overwrite: overwrite})
		j := BackupJob{}
		if res != nil {
			j.Uploaded, j.Same, j.Failed, j.Bytes, j.Conflicts = len(res.Restored), res.Unchanged, len(res.Errors), res.Bytes, res.Conflicts
		}
		if err != nil {
			j.Error = err.Error()
		} else {
			j.Summary = fmt.Sprintf("%d file(s) restored, %d already identical, %d kept (differ locally)", j.Uploaded, j.Same, len(j.Conflicts))
		}
		return j
	})
}

func (m *Manager) startBackupJob(kind string, run func(context.Context, *backup.Service) BackupJob) error {
	o := m.Observe()
	if o == nil || o.Backup == nil {
		return ErrUnavailable
	}
	if _, ok := o.Backup.Server(); !ok {
		return backup.ErrNoServer
	}
	started := time.Now()
	m.omx.Lock()
	if m.bjob != nil && m.bjob.State == "running" {
		m.omx.Unlock()
		return ErrBusy
	}
	m.bjob = &BackupJob{Kind: kind, State: "running", Started: started}
	m.omx.Unlock()

	m.mu.Lock()
	parent := m.ctx
	m.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		j := run(parent, o.Backup)
		j.Kind, j.Started, j.Finished = kind, started, time.Now()
		j.State = "done"
		if j.Error != "" {
			j.State = "failed"
		}
		m.omx.Lock()
		m.bjob = &j
		m.omx.Unlock()
		if j.Error != "" {
			m.log.Warn("backup job failed", "component", "fleet", "kind", kind, "err", j.Error)
		}
	}()
	return nil
}

// BackupJobStatus returns the last operator-started backup or import, if any.
func (m *Manager) BackupJobStatus() (BackupJob, bool) {
	m.omx.Lock()
	defer m.omx.Unlock()
	if m.bjob == nil {
		return BackupJob{}, false
	}
	return *m.bjob, true
}

// BackupReminder returns today's backup reminder (at most one per local
// calendar day: the service marks it delivered, so it is held here until the
// operator dismisses it or the day ends).
func (m *Manager) BackupReminder(now time.Time) (backup.Reminder, bool) {
	o := m.Observe()
	if o == nil || o.Backup == nil {
		return backup.Reminder{}, false
	}
	day := now.Format("2006-01-02")
	m.omx.Lock()
	defer m.omx.Unlock()
	if m.rem.day != day {
		m.rem = reminderState{day: day}
		if r, ok := o.Backup.Reminder(now); ok {
			m.rem.r, m.rem.have = r, true
		}
	}
	if !m.rem.have || m.rem.dismissed {
		return backup.Reminder{}, false
	}
	return m.rem.r, true
}

// DismissBackupReminder hides today's reminder (it comes back tomorrow if the
// backup is still due).
func (m *Manager) DismissBackupReminder(now time.Time) {
	day := now.Format("2006-01-02")
	m.omx.Lock()
	if m.rem.day == day {
		m.rem.dismissed = true
	}
	m.omx.Unlock()
}

type reminderState struct {
	day       string
	r         backup.Reminder
	have      bool
	dismissed bool
}

// observeState is the manager's Observe bookkeeping (guarded by omx).
type observeState struct {
	omx          sync.Mutex
	obs          *Observe
	loopsStarted bool
	syncers      map[string]*nodeSyncer
	sync         map[string]SyncStatus
	bjob         *BackupJob
	rem          reminderState
	wdsubs       map[int]chan WatchdogEvent
	wdnext       int
}

func newObserveState() observeState {
	return observeState{syncers: map[string]*nodeSyncer{}, sync: map[string]SyncStatus{}, wdsubs: map[int]chan WatchdogEvent{}}
}
