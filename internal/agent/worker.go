package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/evidence"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
)

// exitNotConfigured is the exit code reported for a run the agent could not
// start because no config has arrived (same value the executor uses when a
// command cannot be started).
const exitNotConfigured = 127

// lazySender is the executor's Sender before the listener exists: the worker is
// built first (the listener needs its handler), the listener is attached after.
type lazySender struct {
	mu sync.Mutex
	s  exec.Sender
}

func (l *lazySender) set(s exec.Sender) {
	l.mu.Lock()
	l.s = s
	l.mu.Unlock()
}

func (l *lazySender) Send(ctx context.Context, env channel.Envelope) error {
	l.mu.Lock()
	s := l.s
	l.mu.Unlock()
	if s == nil {
		return channel.ErrNotConnected
	}
	return s.Send(ctx, env)
}

// worker is the worker-agent role: it serves the control protocol on the
// channel. The channel Handler is handle; everything the main server sends
// (config, run, input, resize, terminate, confirm) arrives there.
//
// The executor needs the dangerous-command gate and the gate needs the secret,
// which only the main server's config message carries; so the executor is
// created when the first config is applied (config_apply.go). A run that
// arrives before that waits briefly for the config rather than failing the
// race between the connection and the push.
type worker struct {
	cfg    Config
	log    *slog.Logger
	sender *lazySender

	base context.Context
	stop context.CancelFunc

	mu        sync.Mutex // guards everything below
	closed    bool
	secret    string // the applied gate secret (memory only, never logged)
	ex        *exec.Executor
	retired   []*exec.Executor // executors replaced by a secret change, kept until their sessions end
	capCfg    config.CaptureConfig
	capTried  bool // capCfg has been applied (successfully or not)
	capt      Capturer
	retCapt   []Capturer
	ready     chan struct{} // closed when the first executor exists
	readyOnce sync.Once

	cmu  sync.Mutex // guards caps and live
	caps map[string]SessionCapture
	live map[string]struct{} // sessions running now (the idle-push condition)

	ev     *evidence.Agent // evidence sync endpoint (evidence.go); nil when unavailable
	evDone chan struct{}   // closed when the idle-push loop has ended

	svcMu sync.Mutex // guards svc
	svc   ServiceController

	pip *publicIPReporter
	nip *netInfoReporter

	diskMu       sync.Mutex // guards diskSrc, lastDisk, diskReported
	diskSrc      diskStateSource
	lastDisk     capture.DiskStatus
	diskReported bool

	man *maneuverRunner
	upd *agentUpdater

	wg sync.WaitGroup // runs waiting for the first config
}

func newWorker(cfg Config) *worker {
	if cfg.ConfigWait <= 0 {
		cfg.ConfigWait = defaultConfigWait
	}
	if cfg.CaptureFactory == nil {
		cfg.CaptureFactory = liveFactory
	}
	base, stop := context.WithCancel(context.Background())
	w := &worker{
		cfg: cfg, log: cfg.Logger, sender: &lazySender{},
		base: base, stop: stop,
		ready: make(chan struct{}),
		caps:  map[string]SessionCapture{},
		live:  map[string]struct{}{},
		svc:   defaultServiceController(),
	}
	w.pip = newPublicIPReporter(w.sender, w.log, func() context.Context {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.base
	})
	w.nip = newNetInfoReporter(w.sender, w.log)
	w.man = newManeuverRunner(w)
	w.upd = newAgentUpdater(cfg.DataDir, cfg.AgentVersion, w.log, w.sender, func() context.Context {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.base
	}, w.serviceController, cfg.Update)
	w.initEvidence()
	return w
}

func (w *worker) serviceController() ServiceController {
	w.svcMu.Lock()
	defer w.svcMu.Unlock()
	return w.svc
}

// start attaches the worker to the agent's lifetime context. Called once,
// before the listener serves.
func (w *worker) start(ctx context.Context) {
	w.stop()
	w.base, w.stop = context.WithCancel(ctx)
	w.startEvidence()
	w.startDiskMonitor()
	w.wg.Add(1)
	go func() { defer w.wg.Done(); w.nip.run(w.base) }()
	w.man.start(w.base)
	w.upd.start(w.base)
}

// onConnect runs when the main server's connection is established: it asks for
// the config (the server answers with a config message).
func (w *worker) onConnect(peer string) {
	w.log.Info("main server connected", "component", "fleet-agent", "peer", peer)
	env, err := channel.NewEnvelope(channel.TypeConfigReq, "config", nil)
	if err == nil {
		ctx, cancel := context.WithTimeout(w.base, 10*time.Second)
		defer cancel()
		err = w.sender.Send(ctx, env)
	}
	if err != nil {
		w.log.Warn("request config", "component", "fleet-agent", "err", err)
	}
	reportAgentVersion(w.base, w.sender, w.log, w.cfg.AgentVersion)
	w.upd.onConnect()
	// An idle agent offers segments the server lacks as soon as it is connected.
	go w.offerEvidence()
	w.reportPublicIPOnConnect()
	w.reportDiskOnConnect()
	w.nip.reportNow(w.base)
	w.man.onConnect()
}

// handle is the channel.Handler. It never blocks on a running command.
func (w *worker) handle(ctx context.Context, env channel.Envelope) {
	switch env.Type {
	case channel.TypeConfig:
		w.handleConfig(env)
	case channel.TypeRun:
		if ex := w.current(); ex != nil {
			ex.Handle(ctx, env)
			return
		}
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.runWhenConfigured(env)
		}()
	case channel.TypeEvManifestReq, channel.TypeEvRange, channel.TypeEvVerify, channel.TypeQueueState:
		w.handleEvidence(ctx, env) // evidence sync: pull, idle-push and queue-state exchanges
	case channel.TypeServiceControl:
		handleServiceControl(ctx, w.log, w.sender, w.serviceController(), env)
	case channel.TypePublicIPRefresh:
		w.handlePublicIPRefresh()
	case channel.TypeManeuverPrepare, channel.TypeManeuverCommit, channel.TypeManeuverAbort:
		w.man.handle(ctx, env)
	case channel.TypeAgentUpdate, channel.TypeAgentUpdateChunk:
		w.upd.handleEnvelope(ctx, env)
	default:
		// input, resize, terminate, confirm: addressed by session id, so every
		// executor gets them and the one that owns the session acts.
		for _, ex := range w.executors() {
			ex.Handle(ctx, env)
		}
	}
}

func (w *worker) current() *exec.Executor {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ex
}

func (w *worker) executors() []*exec.Executor {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := append([]*exec.Executor(nil), w.retired...)
	if w.ex != nil {
		out = append(out, w.ex)
	}
	return out
}

// runWhenConfigured holds a run that beat the first config push: it is handed
// to the executor once the config is applied, or refused if none comes.
func (w *worker) runWhenConfigured(env channel.Envelope) {
	t := time.NewTimer(w.cfg.ConfigWait)
	defer t.Stop()
	select {
	case <-w.ready:
		if ex := w.current(); ex != nil {
			ex.Handle(w.base, env)
		}
	case <-t.C:
		var p exec.RunPayload
		_ = json.Unmarshal(env.Payload, &p)
		if p.SessionID == "" {
			p.SessionID = env.ID
		}
		w.log.Warn("run before config: no gate secret received from the main server", "component", "fleet-agent", "session_id", p.SessionID)
		x := exec.ExitPayload{
			SessionID: p.SessionID, ExitCode: exitNotConfigured, Reason: exec.ReasonError,
			Error: "agent has not received its configuration (gate secret) from the main server",
		}
		if out, err := channel.NewEnvelope(channel.TypeExit, p.SessionID, x); err == nil {
			_ = w.sender.Send(w.base, out)
		}
	case <-w.base.Done():
	}
}

// shutdown ends the worker: every session is terminated, every capture stopped.
func (w *worker) shutdown() {
	w.stop()
	w.unbindPublicIP()
	w.unbindDiskState()
	w.man.shutdown()
	w.mu.Lock()
	w.closed = true
	exs := append([]*exec.Executor(nil), w.retired...)
	if w.ex != nil {
		exs = append(exs, w.ex)
	}
	capts := append([]Capturer(nil), w.retCapt...)
	if w.capt != nil {
		capts = append(capts, w.capt)
	}
	w.mu.Unlock()

	w.wg.Wait()
	if w.evDone != nil {
		<-w.evDone
	}
	for _, ex := range exs {
		ex.Shutdown() // ends sessions; OnSessionEnd stops their captures
	}
	w.cmu.Lock()
	left := w.caps
	w.caps = map[string]SessionCapture{}
	w.cmu.Unlock()
	for id, sc := range left {
		w.stopCapture(id, sc)
	}
	for _, c := range capts {
		c.Close()
	}
	w.setServerQueueEmpty(true)
}

// --- evidence capture ------------------------------------------------------------

// sessionStart is the executor's OnSessionStart hook: attach capture to the
// session's cgroup. Capture is best-effort and never fails the command.
func (w *worker) sessionStart(s *exec.Session) {
	w.cmu.Lock()
	w.live[s.ID] = struct{}{}
	w.cmu.Unlock()
	w.mu.Lock()
	c := w.capt
	w.mu.Unlock()
	if c == nil {
		return
	}
	cg := s.CgroupPath()
	if cg == "" {
		w.log.Debug("evidence capture skipped: session has no cgroup", "component", "fleet-agent", "session_id", s.ID)
		return
	}
	var (
		sc  SessionCapture
		err error
	)
	if mc, ok := c.(metaCapturer); ok && (s.ManeuverID != "" || s.Mode == exec.ModeReplica) {
		sc, err = mc.CaptureWithMeta(s.ID, cg, capture.SessionMeta{
			Mode:       s.Mode,
			ManeuverID: s.ManeuverID,
			RunID:      s.RunID,
			ItemID:     s.ItemID,
			Replica:    s.Replica,
		})
	} else {
		sc, err = c.Capture(s.ID, cg)
	}
	if err != nil {
		w.log.Warn("evidence capture not started; the command runs uncaptured",
			"component", "fleet-agent", "session_id", s.ID, "err", err)
		return
	}
	w.cmu.Lock()
	w.caps[s.ID] = sc
	w.cmu.Unlock()
}

// sessionEnd is the executor's OnSessionEnd hook: stop the session's capture.
func (w *worker) sessionEnd(info exec.SessionInfo) {
	w.cmu.Lock()
	delete(w.live, info.ID)
	sc := w.caps[info.ID]
	delete(w.caps, info.ID)
	w.cmu.Unlock()
	if sc != nil {
		w.stopCapture(info.ID, sc)
	}
}

func (w *worker) stopCapture(id string, sc SessionCapture) {
	sum, err := sc.Stop()
	if err != nil {
		w.log.Warn("stop evidence capture", "component", "fleet-agent", "session_id", id, "err", err)
		return
	}
	w.log.Info("evidence captured", "component", "fleet-agent", "session_id", id,
		"records", sum.Records, "dropped", sum.Dropped, "segments", sum.Segments)
}

// Capturer is the evidence-capture subsystem as the worker uses it: it starts
// a capture per session and is closed when the agent stops.
type Capturer interface {
	Capture(sessionID, cgroupPath string) (SessionCapture, error)
	Close()
}

// metaCapturer is an optional extension of Capturer that receives Maneuver
// replica attribution metadata alongside sessionID and cgroupPath.
type metaCapturer interface {
	CaptureWithMeta(sessionID, cgroupPath string, meta capture.SessionMeta) (SessionCapture, error)
}

// SessionCapture is one running per-session capture.
type SessionCapture interface {
	Stop() (capture.Summary, error)
}

// CaptureFactory builds a Capturer from the capture configuration. The default
// is live kernel capture (liveFactory); tests substitute their own.
type CaptureFactory func(ctx context.Context, cfg capture.Config) (Capturer, error)
