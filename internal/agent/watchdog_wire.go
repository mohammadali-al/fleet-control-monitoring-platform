package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

// This file wires the watchdog (internal/watchdog) into the agent binary. The
// watchdog is its own process (fleet-watchdog.service runs `fleet-agent
// watchdog`) so it keeps running when the main agent is busy or down; the two
// processes share only the data directory (D-0006):
//
//	server --policy--> main agent --StorePolicy--> <DataDir>/watchdog/policy.yaml
//	                                                      |  (the loop reloads it live)
//	server <-watchdog-ev- main agent <--tail-- <DataDir>/watchdog/events.jsonl <-- watchdog process
//
// The main agent never runs the watchdog loop and the watchdog process never
// opens the channel.

// File names inside <DataDir>/watchdog, next to the policy and the counters.
const (
	eventsFileName   = "events.jsonl"  // one JSON watchdog-ev payload per line, appended by the watchdog
	eventsCursorName = "events.offset" // byte offset in events.jsonl up to which the main agent has forwarded
	eventsMaxBytes   = 1 << 20         // the writer rotates events.jsonl to events.jsonl.1 beyond this

	defaultEventPoll = 2 * time.Second
	eventSendTimeout = 10 * time.Second
)

func watchdogDir(dataDir string) string { return filepath.Join(dataDir, watchdog.WatchdogSubdir) }

// --- the watchdog process --------------------------------------------------------

// WatchdogRunOptions replaces the real host sampler and actuator (tests).
type WatchdogRunOptions struct {
	Sampler  watchdog.Sampler
	Actuator watchdog.Actuator
	// BootTime overrides the boot time used for the boot-stability window.
	BootTime func() time.Time
}

// NewWatchdog builds the watchdog loop for the agent config: the policy and
// counters come from dataDir, the metrics from this host, the actions from
// systemctl/cgroups/tc (root), and every event is also appended to the events
// file the main agent forwards.
func NewWatchdog(ac config.AgentConfig, dataDir string, log *slog.Logger, opts WatchdogRunOptions) (*watchdog.Watchdog, error) {
	if log == nil {
		log = slog.Default()
	}
	cg := ac.CgroupRoot
	if cg == "" {
		cg = DefaultCgroupRoot()
	}
	sampler, actuator := opts.Sampler, opts.Actuator
	if sampler == nil {
		sampler = watchdog.NewSystemSampler(cg, "")
	}
	if actuator == nil {
		actuator = &watchdog.SystemActuator{SessionRoot: cg, Iface: ac.Watchdog.EgressInterface}
	}
	ew := NewEventWriter(dataDir, log)
	return watchdog.New(watchdog.Config{
		DataDir:   dataDir,
		MachineID: ac.MachineID,
		Groups:    ac.Watchdog.Groups,
		Sampler:   sampler,
		Actuator:  actuator,
		BootTime:  opts.BootTime,
		Notify:    ew.Write,
		Log:       log,
	})
}

// RunWatchdog runs the watchdog loop until ctx is cancelled (the
// `fleet-agent watchdog` subcommand). Cancellation is a clean stop, not an error.
func RunWatchdog(ctx context.Context, ac config.AgentConfig, dataDir string, log *slog.Logger, opts WatchdogRunOptions) error {
	w, err := NewWatchdog(ac, dataDir, log, opts)
	if err != nil {
		return err
	}
	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// EventWriter appends watchdog events to <DataDir>/watchdog/events.jsonl, the
// hand-off to the main agent. A failed write is logged and never disturbs the
// watchdog: the event is still in the log and the panel just misses it.
type EventWriter struct {
	dir string
	log *slog.Logger
	mu  sync.Mutex
}

// NewEventWriter returns a writer for the events file under dataDir.
func NewEventWriter(dataDir string, log *slog.Logger) *EventWriter {
	if log == nil {
		log = slog.Default()
	}
	return &EventWriter{dir: watchdogDir(dataDir), log: log}
}

// Write appends one event. It is the watchdog's Notify callback.
func (e *EventWriter) Write(ev watchdog.Event) {
	line, err := json.Marshal(channel.WatchdogEventPayload(ev))
	if err == nil {
		err = e.append(append(line, '\n'))
	}
	if err != nil {
		e.log.Warn("record watchdog event for the main agent", "component", "fleet-watchdog", "err", err)
	}
}

func (e *EventWriter) append(line []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := os.MkdirAll(e.dir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(e.dir, eventsFileName)
	if st, err := os.Stat(path); err == nil && st.Size() > eventsMaxBytes {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	_, werr := f.Write(line)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// --- the main agent --------------------------------------------------------------

// WatchdogOptions configures the main agent's half of the watchdog protocol.
type WatchdogOptions struct {
	// Groups are this host's policy-override groups (agent config
	// watchdog.groups); a pushed profile must resolve for this host.
	Groups []string
	// EventPoll is how often the events file is checked for new events
	// (default 2 s). A connection to the server also triggers a check.
	EventPoll time.Duration
}

// watchdogWire is the main agent's half: policy envelopes in, events out.
type watchdogWire struct {
	machine string
	dataDir string
	groups  []string
	log     *slog.Logger
	poll    time.Duration

	pmu  sync.Mutex // serialises policy stores
	kick chan struct{}
	fwd  *eventForwarder
}

func newWatchdogWire(cfg Config, opts WatchdogOptions) *watchdogWire {
	poll := opts.EventPoll
	if poll <= 0 {
		poll = defaultEventPoll
	}
	return &watchdogWire{
		machine: cfg.MachineID, dataDir: cfg.DataDir, groups: opts.Groups, log: cfg.Logger, poll: poll,
		kick: make(chan struct{}, 1),
		fwd:  &eventForwarder{dir: watchdogDir(cfg.DataDir), log: cfg.Logger},
	}
}

// handlePolicy stores a pushed policy profile and replies policy-ack. The
// profile is validated first (including that it resolves for this host); an
// invalid or older one is refused, nothing on disk changes and the last-known
// policy keeps running. The running watchdog notices the file change and
// applies it live.
func (w *watchdogWire) handlePolicy(ctx context.Context, env channel.Envelope, send exec.Sender) {
	ack := watchdog.PolicyAck(env, w.store, w.inForce)
	var p channel.PolicyAckPayload
	if err := json.Unmarshal(ack.Payload, &p); err == nil {
		if p.OK {
			w.log.Info("watchdog policy stored", "component", "fleet-agent", "version", p.Version)
		} else {
			w.log.Warn("watchdog policy rejected; last-known keeps running", "component", "fleet-agent", "err", p.Error, "version", p.Version)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, eventSendTimeout)
	defer cancel()
	if err := send.Send(ctx, ack); err != nil {
		w.log.Warn("send policy ack", "component", "fleet-agent", "err", err)
	}
}

func (w *watchdogWire) store(raw []byte, version int) error {
	w.pmu.Lock()
	defer w.pmu.Unlock()
	pf, err := watchdog.ParseProfile(raw)
	if err != nil {
		return err
	}
	if version != 0 && pf.Version != version {
		w.log.Warn("policy payload version differs from the profile's", "component", "fleet-agent", "payload", version, "profile", pf.Version)
	}
	if _, err := pf.Effective(w.machine, w.groups); err != nil {
		return fmt.Errorf("policy does not resolve for this host: %w", err)
	}
	_, err = watchdog.StorePolicy(w.dataDir, raw, -1)
	return err
}

// inForce is the version of the stored (last-known) policy; 0 is the baked-in default.
func (w *watchdogWire) inForce() int {
	pf, err := watchdog.LoadLastKnown(w.dataDir)
	if err != nil || pf == nil {
		return 0
	}
	return pf.Version
}

// forwardLoop forwards new watchdog events until ctx ends. Events the server
// could not take stay in the file and go out, oldest first, once it is back.
func (w *watchdogWire) forwardLoop(ctx context.Context, send exec.Sender) {
	t := time.NewTicker(w.poll)
	defer t.Stop()
	for {
		if n, err := w.fwd.forward(ctx, send); err != nil && !errors.Is(err, channel.ErrNotConnected) && ctx.Err() == nil {
			w.log.Warn("forward watchdog events", "component", "fleet-agent", "err", err)
		} else if n > 0 {
			w.log.Debug("forwarded watchdog events", "component", "fleet-agent", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.kick:
		}
	}
}

func (w *watchdogWire) kickNow() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// eventForwarder tails events.jsonl and sends each new line as a watchdog-ev.
// The forwarded offset is persisted, so an agent restart neither repeats nor
// loses events (delivery is at-least-once: a crash between a send and the
// offset write repeats at most the last pass).
type eventForwarder struct {
	dir string
	log *slog.Logger

	mu      sync.Mutex
	loaded  bool
	off     int64
	rotated bool        // events.jsonl was rotated: the rest of events.jsonl.1 is still owed
	ident   os.FileInfo // events.jsonl as last seen, to notice a rotation
}

func (f *eventForwarder) loadCursor() {
	if f.loaded {
		return
	}
	f.loaded = true
	b, err := os.ReadFile(filepath.Join(f.dir, eventsCursorName))
	if err != nil {
		return
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n >= 0 {
		f.off = n
	}
}

func (f *eventForwarder) saveCursor() {
	path := filepath.Join(f.dir, eventsCursorName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(f.off, 10)+"\n"), 0o640); err == nil {
		if err := os.Rename(tmp, path); err == nil {
			return
		}
	}
	_ = os.Remove(tmp)
	f.log.Warn("could not persist the watchdog event cursor; events may be repeated after a restart", "component", "fleet-agent")
}

// forward sends every complete line after the cursor and returns how many were
// sent. It stops at the first send error (the server is away) and keeps the
// cursor there.
func (f *eventForwarder) forward(ctx context.Context, send exec.Sender) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadCursor()
	start := f.off
	sent := 0
	defer func() {
		if f.off != start {
			f.saveCursor()
		}
	}()

	cur := filepath.Join(f.dir, eventsFileName)
	st, err := os.Stat(cur)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// The writer rotated the file under us if it is a different file than
	// the one we read last time (or, after a restart, shorter than the cursor).
	if (f.ident != nil && !os.SameFile(f.ident, st)) || st.Size() < f.off {
		f.rotated = true
	}
	f.ident = st
	if f.rotated {
		n, done, err := f.drain(ctx, cur+".1", send)
		sent += n
		if err != nil {
			return sent, err
		}
		if done {
			f.rotated, f.off = false, 0
		}
	}
	n, err := f.drainCurrent(ctx, cur, send)
	return sent + n, err
}

// drain forwards the rest of a rotated file from the cursor. done is true when
// it was fully forwarded (or is gone).
func (f *eventForwarder) drain(ctx context.Context, path string, send exec.Sender) (n int, done bool, err error) {
	n, err = f.sendFrom(ctx, path, send)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, true, nil
	}
	return n, err == nil, err
}

func (f *eventForwarder) drainCurrent(ctx context.Context, path string, send exec.Sender) (int, error) {
	n, err := f.sendFrom(ctx, path, send)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return n, err
}

// sendFrom sends the complete lines of path after f.off, advancing f.off.
func (f *eventForwarder) sendFrom(ctx context.Context, path string, send exec.Sender) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	if _, err := file.Seek(f.off, io.SeekStart); err != nil {
		return 0, err
	}
	data, err := io.ReadAll(io.LimitReader(file, 2*eventsMaxBytes))
	if err != nil {
		return 0, err
	}
	sent := 0
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return sent, nil // an unfinished line waits for its newline
		}
		line := data[:i]
		data = data[i+1:]
		adv := int64(i + 1)
		if len(bytes.TrimSpace(line)) > 0 {
			var p channel.WatchdogEventPayload
			if err := json.Unmarshal(line, &p); err != nil {
				f.log.Warn("skipping unreadable watchdog event line", "component", "fleet-agent", "err", err)
			} else {
				env, err := channel.NewEnvelope(channel.TypeWatchdogEv, "", p)
				if err != nil {
					return sent, err
				}
				sctx, cancel := context.WithTimeout(ctx, eventSendTimeout)
				err = send.Send(sctx, env)
				cancel()
				if err != nil {
					return sent, err
				}
				sent++
			}
		}
		f.off += adv
	}
}

// --- the agent with the watchdog wiring ---------------------------------------------

// WatchdogAgent is a worker Agent that also takes part in the watchdog
// protocol: it stores the policy profiles the main server pushes (policy ->
// policy-ack) and forwards the events the watchdog process records
// (watchdog-ev). It is New plus that wiring; everything else is the Agent.
type WatchdogAgent struct {
	*Agent
	ww *watchdogWire
}

// NewWithWatchdog is New with the watchdog wiring.
func NewWithWatchdog(cfg Config, opts WatchdogOptions) (*WatchdogAgent, error) {
	if cfg.MachineID == "" {
		return nil, errors.New("agent: machine id is required")
	}
	if cfg.TLS == nil {
		return nil, errors.New("agent: a TLS config is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("agent: a data dir is required")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = fmt.Sprintf(":%d", config.DefaultChannelPort)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	w := newWorker(cfg)
	ww := newWatchdogWire(cfg, opts)
	ln, err := channel.Listen(channel.ListenerConfig{
		Addr:      cfg.ListenAddr,
		TLS:       cfg.TLS,
		MachineID: cfg.MachineID,
		Handler: func(ctx context.Context, env channel.Envelope) {
			if env.Type == channel.TypePolicy {
				ww.handlePolicy(ctx, env, w.sender)
				return
			}
			w.handle(ctx, env)
		},
		OnConnect: func(peer string) {
			w.onConnect(peer)
			ww.kickNow()
		},
	})
	if err != nil {
		return nil, err
	}
	w.sender.set(ln)
	return &WatchdogAgent{Agent: &Agent{log: cfg.Logger, ln: ln, w: w}, ww: ww}, nil
}

// WorkerFromConfig is FromConfig with the watchdog wiring: the group list comes
// from the agent config's watchdog section.
func WorkerFromConfig(ac config.AgentConfig, dataDir string, log *slog.Logger) (*WatchdogAgent, error) {
	if ac.MachineID == "" {
		return nil, errors.New("agent: machine_id is not set in the agent config")
	}
	if ac.ConnectMode == config.ConnectModeAgentDial {
		return nil, errors.New("agent: connect_mode agent_dial is not supported yet (only server_dial)")
	}
	tlsCfg, err := LoadTLS(ac.CAFile, ac.CertFile, ac.KeyFile)
	if err != nil {
		return nil, err
	}
	cg := ac.CgroupRoot
	if cg == "" {
		cg = DefaultCgroupRoot()
	}
	return NewWithWatchdog(Config{
		MachineID:  ac.MachineID,
		DataDir:    dataDir,
		ListenAddr: ac.ListenAddrOrDefault(),
		TLS:        tlsCfg,
		CgroupRoot: cg,
		Capture:    ac.Capture,
		Logger:     log,
	}, WatchdogOptions{Groups: ac.Watchdog.Groups})
}

// Run serves the channel and forwards watchdog events until ctx is cancelled.
func (a *WatchdogAgent) Run(ctx context.Context) error {
	fctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.ww.forwardLoop(fctx, a.w.sender)
	}()
	err := a.Agent.Run(ctx)
	cancel()
	wg.Wait()
	return err
}
