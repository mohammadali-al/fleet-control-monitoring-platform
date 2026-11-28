// Package fleet is the main server's live view of its machines: it owns one
// control-channel link and one command queue per machine, derives the online
// state the registry shows, runs the dangerous-command confirmation (minting
// the token the agent verifies), and fans events out to the panel.
//
// The panel (internal/panel) talks to a *Manager; nothing here knows about
// HTTP. Transports are pluggable (LinkFactory) so the manager is tested with
// an in-process executor and run in production over the mTLS channel
// (dialer.go).
package fleet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/baseline"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/maneuver"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/onboard"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/project"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

// Machine states shown in the panel.
const (
	StateOnline       = "online"
	StateReconnecting = "reconnecting" // was connected since the server started, now down, retrying
	StateOffline      = "offline"
)

// Errors returned to the panel; it maps them to HTTP statuses.
var (
	ErrUnavailable = errors.New("fleet: provisioning is not available on this server")
	ErrNoShell     = errors.New("fleet: monitor agents have no shell or command role")
	ErrOffline     = errors.New("fleet: machine is not online")
	ErrUnknown     = errors.New("fleet: unknown machine or session")
	ErrBadRequest  = errors.New("fleet: bad request")
	ErrForbidden   = errors.New("fleet: session belongs to another operator or machine")
)

// ConfirmRequiredError is returned by Submit when the command matches the
// dangerous-command list and the operator has not confirmed it yet.
type ConfirmRequiredError struct {
	Machine string
	Command string
	Pattern string
}

func (e *ConfirmRequiredError) Error() string {
	return fmt.Sprintf("fleet: command on %s matches dangerous pattern %q and needs confirmation", e.Machine, e.Pattern)
}

// Link is the connection to one machine's agent.
type Link interface {
	exec.Sender
	// Run connects and reconnects until ctx is cancelled.
	Run(ctx context.Context) error
	// Online reports whether the agent is connected and verified.
	Online() bool
}

// LinkFactory builds the link to a machine. h receives the agent's state,
// chunk and exit envelopes.
type LinkFactory func(m registry.Machine, h channel.Handler) (Link, error)

// Config configures a Manager.
type Config struct {
	Registry    *registry.Registry
	Provisioner *onboard.Provisioner // nil: onboarding reports ErrUnavailable
	Gate        *exec.Gate
	NewLink     LinkFactory
	// AgentDial, when set, is the listener agent_dial machines connect to: the
	// manager builds their links from it (instead of NewLink) and serves it
	// from Start. Nil leaves agent_dial machines without a link (offline).
	AgentDial *AgentListener
	Logger    *slog.Logger
	// PollInterval is how often link state is sampled (default 1 s).
	PollInterval time.Duration
	// ConfirmTTL is how long a minted confirmation token stays valid
	// (default 10 minutes: a confirmed command may wait in the queue).
	ConfirmTTL time.Duration
	// GateSecret is the dangerous-command secret the Gate mints tokens with.
	// When set, the manager pushes it to every worker agent over the channel
	// (config-push) so the agent can verify the tokens; empty means nothing is
	// pushed (tests with an in-process executor that shares the Gate).
	GateSecret []byte
	// Capture is the per-node evidence-capture configuration pushed with it
	// (an all-zero value pushes no capture section).
	Capture config.CaptureConfig
	// Monitor wires the monitor-agent data path (see MonitorSource). The zero
	// value pushes no monitor config and drops mon-data; SetMonitor can bind it
	// later.
	Monitor MonitorSource
	// MonitorStore configures the persistent time-series store opened by
	// EnableObserve. Empty disables external TSDB persistence.
	MonitorStore config.MonitorStoreConfig
	// Watchdog wires the watchdog policy push and event intake (see
	// WatchdogSource). The zero value pushes no policy; the agent then keeps
	// running its last-known (or baked-in default) policy. SetWatchdog can bind
	// it later.
	Watchdog WatchdogSource
	// DataDir is the server's base data directory (D-0006). When set (or
	// inferred from Provisioner.Store), command session history is persisted
	// under <DataDir>/commands/history.jsonl and reloaded on start.
	DataDir string
	// ShellIdleTimeout is how long a detached interactive shell (Mode B) may
	// remain with no active browser attachment before the reaper terminates it
	// (default 15 minutes).
	ShellIdleTimeout time.Duration
	// ShellScrollbackBytes is the per-session bounded scrollback ring buffer
	// size for interactive shells (default 128 KiB).
	ShellScrollbackBytes int
}

// MonitorIngestor receives the mon-data envelopes of a monitor agent;
// monitor.Ingestor implements it. agentID is the machine's verified id.
type MonitorIngestor interface {
	Handle(ctx context.Context, agentID string, env channel.Envelope) error
}

// MonitorSource connects the manager to the monitor jobs and the sample sink:
// ConfigFor is monitor.Jobs.ConfigFor (the targets, frequency and timeouts of
// one monitor agent) and Ingest is a monitor.Ingestor writing into the Sink.
type MonitorSource struct {
	ConfigFor func(agent string) probe.Config
	Ingest    MonitorIngestor
}

// MachineInfo is a registry entry plus its live state.
type MachineInfo struct {
	registry.Machine
	State    string     `json:"state"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
	Running  int        `json:"running"`
	Queued   int        `json:"queued"`
	// Note explains a state the operator might not expect.
	Note string `json:"note,omitempty"`
}

// Summary is the fleet tiles on the Machines page.
type Summary struct {
	Workers        int      `json:"workers"`
	WorkersOnline  int      `json:"workers_online"`
	Monitors       int      `json:"monitors"`
	MonitorsOnline int      `json:"monitors_online"`
	NeedsAttention []string `json:"needs_attention"`
	DiskWarn       []string `json:"disk_warn"`
	DiskCritical   []string `json:"disk_critical"`
}

// Command is one entry of the command history (a queued, running or finished
// command or shell).
type Command struct {
	Session    string            `json:"session"`
	Machine    string            `json:"machine"`
	Command    string            `json:"command"`
	User       string            `json:"user,omitempty"`
	Mode       string            `json:"mode"`
	State      string            `json:"state"` // queued | running | finished
	Position   int               `json:"position,omitempty"`
	CgroupPath string            `json:"cgroup_path,omitempty"`
	Exit       *exec.ExitPayload `json:"exit,omitempty"`
	Queued     time.Time         `json:"queued_at"`
	Started    *time.Time        `json:"started_at,omitempty"`
	Finished   *time.Time        `json:"finished_at,omitempty"`
}

// OutputSegment is a run of output on one stream.
type OutputSegment struct {
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

const (
	maxSessions                 = 500 // sessions remembered for output lookup
	maxOutputBytes              = 512 << 10
	defaultPoll                 = time.Second
	defaultConfirm              = 10 * time.Minute
	maxCommandBytes             = 16 << 10
	defaultShellIdleTimeout     = 15 * time.Minute
	defaultShellScrollbackBytes = 128 << 10
	shellSubBuffer              = 4096
)

type node struct {
	id       string
	key      string // what the link was built for: the machine's IP, or keyAgentDial; a change restarts it
	link     Link
	q        *exec.Queue
	qmu      sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	online   bool
	everUp   bool
	lastSeen time.Time
}

type outBuf struct {
	segs      []OutputSegment
	size      int
	truncated bool
}

// Manager is the live fleet.
type Manager struct {
	cfg Config
	log *slog.Logger
	bus *Bus

	mu     sync.Mutex
	ctx    context.Context
	stop   context.CancelFunc
	nodes  map[string]*node
	closed bool
	mon    MonitorSource
	wsrc   WatchdogSource

	wmu sync.Mutex // guards wd
	wd  map[string]*wdState

	scMu      sync.Mutex // guards scPending
	scPending map[string]chan channel.ServiceControlResultPayload

	observeState // the Observe/Admin backends (observe.go)
	maneuverState

	baseStore *baseline.Store
	manStore  *maneuver.Store
	projStore *project.Store

	shMu   sync.Mutex
	shells map[string]*shellSession // live interactive shells (in-memory only; not persisted across restart)

	omu          sync.Mutex
	outputs      map[string]*outBuf
	sessions     map[string]string  // session id -> machine id
	history      map[string]Command // session id -> latest Command snapshot
	order        []string           // session ids, oldest first
	historyPath  string
	hmu          sync.Mutex // serializes appends/compactions to historyPath
	historyLines int
	regStore     *registry.Store
	regSaveMu    sync.Mutex
	wg           sync.WaitGroup
}

// New returns a Manager. Call Start to bring the links up.
func New(cfg Config) (*Manager, error) {
	if cfg.Registry == nil {
		return nil, errors.New("fleet: manager needs a registry")
	}
	if cfg.Gate == nil {
		return nil, errors.New("fleet: manager needs a gate")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPoll
	}
	if cfg.ConfirmTTL <= 0 {
		cfg.ConfirmTTL = defaultConfirm
	}
	if cfg.ShellIdleTimeout <= 0 {
		cfg.ShellIdleTimeout = defaultShellIdleTimeout
	}
	if cfg.ShellScrollbackBytes <= 0 {
		cfg.ShellScrollbackBytes = defaultShellScrollbackBytes
	}
	if cfg.DataDir == "" && cfg.Provisioner != nil && cfg.Provisioner.Store != nil {
		if dir := cfg.Provisioner.Store.Dir(); dir != "" {
			cfg.DataDir = filepath.Dir(dir)
		}
	}
	var historyPath string
	if cfg.DataDir != "" {
		historyPath = filepath.Join(cfg.DataDir, "commands", "history.jsonl")
	}
	var regStore *registry.Store
	if cfg.Provisioner != nil && cfg.Provisioner.Store != nil {
		regStore = cfg.Provisioner.Store
	} else if cfg.DataDir != "" {
		regStore = registry.NewStore(cfg.DataDir)
	}
	mgr := &Manager{
		cfg: cfg, log: cfg.Logger, bus: newBus(), mon: cfg.Monitor, wsrc: cfg.Watchdog,
		wd:            map[string]*wdState{},
		scPending:     map[string]chan channel.ServiceControlResultPayload{},
		observeState:  newObserveState(),
		maneuverState: newManeuverState(),
		nodes:         map[string]*node{},
		shells:        map[string]*shellSession{},
		outputs:       map[string]*outBuf{},
		sessions:      map[string]string{},
		history:       map[string]Command{},
		historyPath:   historyPath,
		regStore:      regStore,
	}
	if cfg.DataDir != "" {
		if bs, err := baseline.Open(cfg.DataDir); err == nil {
			mgr.baseStore = bs
		} else {
			cfg.Logger.Warn("open baseline store", "component", "fleet", "err", err)
		}
		if ms, err := maneuver.Open(cfg.DataDir); err == nil {
			mgr.manStore = ms
		} else {
			cfg.Logger.Warn("open maneuver store", "component", "fleet", "err", err)
		}
		if ps, err := project.Open(cfg.DataDir); err == nil {
			mgr.projStore = ps
		} else {
			cfg.Logger.Warn("open project store", "component", "fleet", "err", err)
		}
	}
	if err := mgr.loadHistory(); err != nil {
		cfg.Logger.Warn("load command history", "component", "fleet", "err", err)
	}
	return mgr, nil
}

// DataDir returns the manager's configured base data directory, if any.
func (m *Manager) DataDir() string { return m.cfg.DataDir }

// Baseline returns the opened baseline store, if DataDir was configured.
func (m *Manager) Baseline() *baseline.Store { return m.baseStore }

// ManeuverStore returns the opened maneuver store, if DataDir was configured.
func (m *Manager) ManeuverStore() *maneuver.Store { return m.manStore }

// ProjectStore returns the opened project store, if DataDir was configured.
func (m *Manager) ProjectStore() *project.Store { return m.projStore }

// Bus is the event bus for the panel's WebSocket streams.
func (m *Manager) Bus() *Bus { return m.bus }

// Start launches the links for every eligible machine and the state poller.
// It returns immediately; ctx ends everything.
func (m *Manager) Start(ctx context.Context) {
	ctx, stop := context.WithCancel(ctx)
	m.mu.Lock()
	m.ctx, m.stop = ctx, stop
	m.mu.Unlock()
	m.reconcile()
	m.startObserveLoops()
	if al := m.cfg.AgentDial; al != nil {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			if err := al.Serve(ctx); err != nil {
				m.log.Error("agent_dial listener", "component", "fleet", "err", err)
			}
		}()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t := time.NewTicker(m.cfg.PollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.poll()
			}
		}
	}()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		reapInterval := m.cfg.ShellIdleTimeout / 4
		if reapInterval > 5*time.Second {
			reapInterval = 5 * time.Second
		}
		if reapInterval < 5*time.Millisecond {
			reapInterval = 5 * time.Millisecond
		}
		t := time.NewTicker(reapInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.reapIdleShells(ctx, time.Now())
			}
		}
	}()
}

// Close stops every link and waits for the goroutines.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	nodes := m.nodes
	m.nodes = map[string]*node{}
	stop := m.stop
	m.mu.Unlock()
	if stop != nil {
		stop()
	}
	for _, n := range nodes {
		n.cancel()
	}
	for _, n := range nodes {
		<-n.done
	}
	m.wg.Wait()
}

// keyAgentDial is the node key of an agent_dial machine (it needs no address).
const keyAgentDial = "agent_dial"

// wanted reports whether a machine should have a live link, and the key that
// link is built for (a server_dial machine's IP, or keyAgentDial); a machine
// whose key changes gets a new link. agentDial is whether this manager has an
// agent_dial listener; without one those machines stay offline.
func wanted(mc registry.Machine, agentDial bool) (key string, ok bool) {
	if !mc.Provisioned || mc.Removed {
		return "", false
	}
	switch mc.ConnectMode {
	case registry.ConnectModeServerDial:
		if len(mc.PrivateIPs) == 0 {
			return "", false
		}
		return mc.PrivateIPs[0], true
	case registry.ConnectModeAgentDial:
		return keyAgentDial, agentDial
	}
	return "", false
}

// reconcile starts links for machines that need one and stops links that no
// longer do (removed, de-provisioned, address changed). It is cheap and is
// called after every registry change.
func (m *Manager) reconcile() {
	m.mu.Lock()
	if m.closed || m.ctx == nil || (m.cfg.NewLink == nil && m.cfg.AgentDial == nil) {
		m.mu.Unlock()
		return
	}
	var stop []*node
	seen := map[string]bool{}
	for _, mc := range m.cfg.Registry.List() {
		key, ok := wanted(mc, m.cfg.AgentDial != nil)
		if !ok {
			continue
		}
		seen[mc.ID] = true
		if n := m.nodes[mc.ID]; n != nil {
			if n.key == key {
				continue
			}
			stop = append(stop, n)
			delete(m.nodes, mc.ID)
		}
		n, err := m.startNode(mc, key)
		if err != nil {
			m.log.Error("start link", "component", "fleet", "machine", mc.ID, "err", err)
			continue
		}
		m.nodes[mc.ID] = n
	}
	for id, n := range m.nodes {
		if !seen[id] {
			stop = append(stop, n)
			delete(m.nodes, id)
		}
	}
	m.mu.Unlock()

	for _, n := range stop {
		n.cancel()
		<-n.done
		n.q.ConnectionLost()
		_ = m.cfg.Registry.SetOnline(n.id, false)
	}
}

// startNode builds the queue and link for mc and runs them. m.mu is held.
func (m *Manager) startNode(mc registry.Machine, key string) (*node, error) {
	newLink := m.cfg.NewLink
	if mc.ConnectMode == registry.ConnectModeAgentDial {
		newLink = m.cfg.AgentDial.Factory() // wanted() ensured there is a listener
	}
	if newLink == nil {
		return nil, fmt.Errorf("no way to link a %s machine", mc.ConnectMode)
	}
	n := &node{id: mc.ID, key: key, done: make(chan struct{})}
	n.q = exec.NewQueue(mc.ID, senderFunc(func(ctx context.Context, env channel.Envelope) error {
		return n.link.Send(ctx, env)
	}), func(ev exec.Event) { m.onExec(mc.ID, ev) })
	ctx, cancel := context.WithCancel(m.ctx)
	n.cancel = cancel
	if mc.Role == registry.RoleWorker {
		n.q.SetOnQueueChange(func(bool) {
			go m.sendQueueState(ctx, n)
		})
	}
	link, err := newLink(mc, m.agentHandler(n, mc.Role, n.q.Handle))
	if err != nil {
		cancel()
		return nil, err
	}
	n.link = link
	go func() {
		defer close(n.done)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); n.q.Run(ctx) }()
		go func() { defer wg.Done(); _ = link.Run(ctx) }()
		wg.Wait()
	}()
	return n, nil
}

// agentHandler wraps the queue's handler with the config-push exchange: an
// agent that has just connected sends config-req, the manager answers with the
// config message, and the agent's config-ack is logged. Everything else goes to
// next (the queue). The handler runs on the link's read loop.
func (m *Manager) agentHandler(n *node, role string, next channel.Handler) channel.Handler {
	return func(ctx context.Context, env channel.Envelope) {
		switch env.Type {
		case channel.TypeConfigReq:
			switch role {
			case registry.RoleWorker:
				m.pushConfig(ctx, n)
				m.pushPolicy(ctx, n)
				m.sendQueueState(ctx, n)
			case registry.RoleMonitor:
				m.pushMonitorConfig(ctx, n)
			}
		case channel.TypeMonData:
			if role == registry.RoleMonitor {
				m.ingestMonitorData(ctx, n, env)
			}
		case channel.TypePolicyAck:
			if role == registry.RoleWorker {
				m.recordPolicyAck(n, env)
			}
		case channel.TypeWatchdogEv:
			if role == registry.RoleWorker || role == registry.RoleMonitor {
				m.handleWatchdogEvent(n, env)
			}
		case channel.TypeEvManifest, channel.TypeEvChunk:
			if role == registry.RoleWorker {
				m.evidenceHandle(ctx, n, env)
			}
		case channel.TypeConfigAck:
			var ack channel.ConfigAckPayload
			if err := json.Unmarshal(env.Payload, &ack); err != nil {
				m.log.Warn("bad config ack", "component", "fleet", "machine", n.id, "err", err)
			} else if !ack.OK {
				m.log.Warn("agent rejected config", "component", "fleet", "machine", n.id, "reason", ack.Error)
			} else {
				m.log.Info("agent applied config", "component", "fleet", "machine", n.id)
			}
		case channel.TypeServiceControlResult:
			m.recordServiceControlResult(n, env)
		case channel.TypePublicIP:
			m.recordPublicIP(n, env)
		case channel.TypeDiskState:
			m.recordDiskState(n, env)
		case channel.TypeManeuverAck:
			if role == registry.RoleWorker {
				m.handleManeuverAck(n, env)
			}
		case channel.TypeManeuverStatus:
			if role == registry.RoleWorker {
				m.handleManeuverStatus(n, env)
			}
		default:
			next(ctx, env)
		}
	}
}

// sendQueueState tells a connected worker agent whether its server FIFO queue
// is empty so idle evidence push stays gated on both session and queue state.
func (m *Manager) sendQueueState(ctx context.Context, n *node) {
	n.qmu.Lock()
	defer n.qmu.Unlock()
	if n.link == nil || !n.link.Online() {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	env, err := channel.NewEnvelope(channel.TypeQueueState, "", channel.QueueStatePayload{Empty: n.q.FIFOEmpty()})
	if err == nil {
		err = n.link.Send(sctx, env)
	}
	if err != nil && !errors.Is(err, channel.ErrNotConnected) && !errors.Is(err, context.Canceled) {
		m.log.Debug("send queue state", "component", "fleet", "machine", n.id, "err", err)
	}
}

// pushConfig sends a worker agent its configuration: the gate secret (so the
// agent can verify the tokens this manager mints) and the capture settings.
// Nothing is sent when the manager has no secret to push. The payload carries
// a secret and is never logged.
func (m *Manager) pushConfig(ctx context.Context, n *node) {
	if len(m.cfg.GateSecret) == 0 {
		m.log.Debug("no gate secret to push", "component", "fleet", "machine", n.id)
		return
	}
	p := channel.ConfigPayload{GateSecret: string(m.cfg.GateSecret)}
	if !m.cfg.Capture.IsZero() {
		c := m.cfg.Capture
		p.Capture = &c
	}
	env, err := channel.NewEnvelope(channel.TypeConfig, "config", p)
	if err == nil {
		err = n.link.Send(ctx, env)
	}
	if err != nil {
		m.log.Warn("push config", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	m.log.Info("pushed config", "component", "fleet", "machine", n.id)
}

// SetMonitor binds (or replaces) the monitor jobs and sink. Monitor agents
// that connect afterwards get their config from it; call PushMonitorConfigs to
// bring already-connected ones up to date.
func (m *Manager) SetMonitor(src MonitorSource) {
	m.mu.Lock()
	m.mon = src
	m.mu.Unlock()
}

func (m *Manager) monitorSource() MonitorSource {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mon
}

// pushMonitorConfig sends a monitor agent its mon-config: the targets assigned
// to it, with the global frequency and timeouts (monitor jobs ConfigFor). The
// agent applies it idempotently, so it is re-sent on every connection.
func (m *Manager) pushMonitorConfig(ctx context.Context, n *node) {
	src := m.monitorSource()
	if src.ConfigFor == nil {
		m.log.Debug("no monitor jobs to push", "component", "fleet", "machine", n.id)
		return
	}
	cfg := src.ConfigFor(n.id)
	env, err := channel.NewEnvelope(channel.TypeMonConfig, "mon-config", cfg)
	if err == nil {
		err = n.link.Send(ctx, env)
	}
	if err != nil {
		m.log.Warn("push monitor config", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	m.log.Info("pushed monitor config", "component", "fleet", "machine", n.id, "targets", len(cfg.Targets))
}

// PushMonitorConfigs re-sends mon-config to every connected monitor agent, for
// after the jobs, targets or probe settings changed. Agents that are offline
// get theirs when they connect.
func (m *Manager) PushMonitorConfigs(ctx context.Context) {
	m.mu.Lock()
	var nodes []*node
	for id, n := range m.nodes {
		if mc, ok := m.cfg.Registry.Get(id); ok && mc.Role == registry.RoleMonitor && n.link != nil && n.link.Online() {
			nodes = append(nodes, n)
		}
	}
	m.mu.Unlock()
	for _, n := range nodes {
		m.pushMonitorConfig(ctx, n)
	}
}

// ingestMonitorData hands one mon-data envelope to the ingest path. The agent
// identity is the machine this link is verified against, never a field of the
// batch. It runs on the link's read loop, so batches arrive in order.
func (m *Manager) ingestMonitorData(ctx context.Context, n *node, env channel.Envelope) {
	ing := m.monitorSource().Ingest
	if ing == nil {
		m.log.Debug("mon-data dropped: no ingest configured", "component", "fleet", "machine", n.id)
		return
	}
	if err := ing.Handle(ctx, n.id, env); err != nil {
		m.log.Warn("ingest mon-data", "component", "fleet", "machine", n.id, "err", err)
	}
}

type senderFunc func(ctx context.Context, env channel.Envelope) error

func (f senderFunc) Send(ctx context.Context, env channel.Envelope) error { return f(ctx, env) }

// poll samples every link and publishes state transitions.
func (m *Manager) poll() {
	type change struct {
		id     string
		online bool
		q      *exec.Queue
	}
	var changes []change
	m.mu.Lock()
	now := time.Now()
	for id, n := range m.nodes {
		up := n.link.Online()
		if up {
			n.lastSeen = now
			n.everUp = true
		}
		if up != n.online {
			n.online = up
			changes = append(changes, change{id, up, n.q})
		}
	}
	m.mu.Unlock()
	for _, c := range changes {
		_ = m.cfg.Registry.SetOnline(c.id, c.online)
		if !c.online {
			c.q.ConnectionLost() // the agent's exit can no longer arrive
			m.maneuverMachineOffline(c.id)
		}
		m.publishMachine(c.id)
	}
}

func (m *Manager) publishMachine(id string) {
	if info, ok := m.Machine(id); ok {
		m.bus.publish(Event{Kind: EventMachine, Machine: id, Info: &info})
	}
}

// --- machines ----------------------------------------------------------------

func (m *Manager) info(mc registry.Machine, n *node) MachineInfo {
	in := MachineInfo{Machine: mc, State: StateOffline}
	switch {
	case mc.Removed:
		in.Note = "Decommissioned."
	case !mc.Provisioned:
		in.Note = "Not provisioned yet."
	case mc.ConnectMode == registry.ConnectModeAgentDial && n == nil:
		in.Note = "agent_dial: this server's agent listener is not enabled (set server.agent_dial_addr)."
	case n == nil:
		in.Note = "No link."
	default:
		switch {
		case n.online:
			in.State = StateOnline
		case n.everUp:
			in.State = StateReconnecting
		}
		if !n.lastSeen.IsZero() {
			t := n.lastSeen
			in.LastSeen = &t
		}
		for _, e := range n.q.Snapshot() {
			switch e.State {
			case exec.StateQueued:
				in.Queued++
			case exec.StateRunning:
				in.Running++
			}
		}
	}
	in.Machine.Online = in.State == StateOnline
	return in
}

// Machines lists the registry with live state. Decommissioned machines are
// kept in the registry as records but are listed last.
func (m *Manager) Machines() []MachineInfo {
	list := m.cfg.Registry.List()
	m.mu.Lock()
	out := make([]MachineInfo, 0, len(list))
	for _, mc := range list {
		out = append(out, m.info(mc, m.nodes[mc.ID]))
	}
	m.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return !out[i].Removed && out[j].Removed })
	return out
}

// Machine returns one machine with live state.
func (m *Manager) Machine(id string) (MachineInfo, bool) {
	mc, ok := m.cfg.Registry.Get(id)
	if !ok {
		return MachineInfo{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.info(mc, m.nodes[id]), true
}

// Summarize computes the Machines tiles (decommissioned machines are ignored).
func Summarize(ms []MachineInfo) Summary {
	s := Summary{NeedsAttention: []string{}, DiskWarn: []string{}, DiskCritical: []string{}}
	for _, in := range ms {
		if in.Removed {
			continue
		}
		switch in.Role {
		case registry.RoleWorker:
			s.Workers++
			if in.State == StateOnline {
				s.WorkersOnline++
			}
		case registry.RoleMonitor:
			s.Monitors++
			if in.State == StateOnline {
				s.MonitorsOnline++
			}
		}
		if in.Provisioned && in.State != StateOnline {
			s.NeedsAttention = append(s.NeedsAttention, in.ID+" "+in.State)
		}
		switch in.Disk.Status {
		case "warn":
			s.DiskWarn = append(s.DiskWarn, in.ID)
		case "critical":
			s.DiskCritical = append(s.DiskCritical, in.ID)
		}
	}
	return s
}

// --- onboarding ----------------------------------------------------------------

// Credentials are the one-off SSH credentials of a provisioning request. They
// are used for the request and never stored or logged.
type Credentials struct {
	User       string
	Password   string
	PrivateKey string // PEM
	Port       int
}

func (c Credentials) auth() onboard.Auth {
	return onboard.Auth{User: c.User, Password: c.Password, PrivateKeyPEM: []byte(c.PrivateKey)}
}

// OnboardRequest describes a machine to onboard.
type OnboardRequest struct {
	Host        string
	Role        string
	ConnectMode string
	Credentials
}

// Onboard provisions a new machine over SSH.
func (m *Manager) Onboard(ctx context.Context, req OnboardRequest) (MachineInfo, *onboard.Result, error) {
	if m.cfg.Provisioner == nil {
		return MachineInfo{}, nil, ErrUnavailable
	}
	res, err := m.cfg.Provisioner.Provision(ctx, onboard.Target{
		Host: req.Host, Port: req.Port, Auth: req.auth(), Role: req.Role, ConnectMode: req.ConnectMode,
	})
	m.reconcile() // a failed run may still have added the registry entry
	if err != nil {
		return MachineInfo{}, nil, err
	}
	m.publishMachine(res.Machine.ID)
	info, _ := m.Machine(res.Machine.ID)
	return info, res, nil
}

// Reprovision re-runs onboarding for a known machine (idempotent: same ID and
// identity; a cert is issued only if missing).
func (m *Manager) Reprovision(ctx context.Context, id string, creds Credentials) (MachineInfo, *onboard.Result, error) {
	if m.cfg.Provisioner == nil {
		return MachineInfo{}, nil, ErrUnavailable
	}
	mc, ok := m.cfg.Registry.Get(id)
	if !ok {
		return MachineInfo{}, nil, ErrUnknown
	}
	if len(mc.PrivateIPs) == 0 {
		return MachineInfo{}, nil, fmt.Errorf("%w: machine %s has no known address", ErrBadRequest, id)
	}
	res, err := m.cfg.Provisioner.Provision(ctx, onboard.Target{
		Host: mc.PrivateIPs[0], Port: creds.Port, Auth: creds.auth(),
		Role: mc.Role, ConnectMode: mc.ConnectMode, MachineID: id,
	})
	m.reconcile()
	if err != nil {
		return MachineInfo{}, nil, err
	}
	m.publishMachine(id)
	info, _ := m.Machine(id)
	return info, res, nil
}

// Remove decommissions a machine: stops its agent over SSH (unless skipRemote)
// and marks it removed in the registry, recording the certificate to revoke.
func (m *Manager) Remove(ctx context.Context, id string, creds Credentials, skipRemote bool) error {
	if m.cfg.Provisioner == nil {
		return ErrUnavailable
	}
	if _, ok := m.cfg.Registry.Get(id); !ok {
		return ErrUnknown
	}
	err := m.cfg.Provisioner.Remove(ctx, id, creds.auth(), onboard.RemoveOptions{Port: creds.Port, SkipRemote: skipRemote})
	m.reconcile()
	if err != nil {
		return err
	}
	m.publishMachine(id)
	return nil
}

// --- commands --------------------------------------------------------------------

// SubmitRequest is a one-shot command for one machine.
type SubmitRequest struct {
	Machine    string
	Command    string
	User       string
	TimeoutSec int
	// Confirm is the operator's confirmation of a dangerous command. The
	// manager then mints the token the agent verifies; the agent refuses the
	// command without it, whatever the browser says.
	Confirm bool
}

func (m *Manager) target(id string) (*node, error) {
	mc, ok := m.cfg.Registry.Get(id)
	if !ok || mc.Removed {
		return nil, ErrUnknown
	}
	if mc.Role != registry.RoleWorker {
		return nil, ErrNoShell
	}
	m.mu.Lock()
	n := m.nodes[id]
	up := n != nil && n.online
	m.mu.Unlock()
	if !up {
		return nil, ErrOffline
	}
	return n, nil
}

// Preflight checks that a command could be submitted to a machine, and
// whether it needs confirmation, without submitting it.
func (m *Manager) Preflight(machine, command string) error {
	if _, err := m.target(machine); err != nil {
		return err
	}
	if pat, hit := m.cfg.Gate.Match(command); hit {
		return &ConfirmRequiredError{Machine: machine, Command: command, Pattern: pat}
	}
	return nil
}

// Submit queues a one-shot command on a worker agent and returns its session
// id. Commands on one machine run strictly one at a time, in order.
func (m *Manager) Submit(req SubmitRequest) (string, error) {
	if req.Command == "" || len(req.Command) > maxCommandBytes {
		return "", fmt.Errorf("%w: command must be 1 to %d bytes", ErrBadRequest, maxCommandBytes)
	}
	if req.TimeoutSec < 0 {
		return "", fmt.Errorf("%w: negative timeout", ErrBadRequest)
	}
	n, err := m.target(req.Machine)
	if err != nil {
		return "", err
	}
	run := exec.RunPayload{Command: req.Command, User: req.User, Mode: exec.ModeOneShot, TimeoutSec: req.TimeoutSec}
	if pat, hit := m.cfg.Gate.Match(req.Command); hit {
		if !req.Confirm {
			return "", &ConfirmRequiredError{Machine: req.Machine, Command: req.Command, Pattern: pat}
		}
		run.ConfirmToken = m.cfg.Gate.Mint(req.Machine, req.Command, m.cfg.ConfirmTTL)
	}
	run.SessionID = exec.NewSessionID()
	m.remember(run.SessionID, req.Machine)
	if _, err := n.q.Submit(run); err != nil {
		return "", err
	}
	return run.SessionID, nil
}

// ShellRequest is an interactive shell for one machine.
type ShellRequest struct {
	Machine string
	User    string
	// Session is the session id to use. The caller sets it when it must
	// subscribe to the session's events before the shell is queued; empty
	// means generate one.
	Session    string
	Cols, Rows uint16
}

// AttachRequest attaches a viewer to an existing live shell session.
type AttachRequest struct {
	Session string
	User    string
	// Machine, when non-empty, must match the session's machine.
	Machine string
}

// ShellAttachment is one active browser attachment to a live shell session.
// Call Detach when the viewer disconnects; the underlying PTY stays running.
type ShellAttachment struct {
	Session    string
	Machine    string
	User       string
	State      string
	Scrollback []byte
	Events     <-chan exec.Event
	Evicted    <-chan struct{}

	mgr  *Manager
	id   uint64
	once sync.Once
}

// Detach releases this attachment without terminating the underlying PTY
// session. Safe to call multiple times.
func (a *ShellAttachment) Detach() {
	if a == nil || a.mgr == nil {
		return
	}
	a.once.Do(func() {
		a.mgr.detachShell(a.Session, a.id)
	})
}

type shellSession struct {
	id         string
	machine    string
	user       string
	state      string
	ring       *ringBuf
	attached   bool
	attachID   uint64
	subCh      chan exec.Event
	evicted    chan struct{}
	detachedAt time.Time
	reaped     bool
}

type ringBuf struct {
	buf   []byte
	cap   int
	start int
	size  int
}

func newRingBuf(capacity int) *ringBuf {
	if capacity <= 0 {
		capacity = defaultShellScrollbackBytes
	}
	return &ringBuf{buf: make([]byte, capacity), cap: capacity}
}

func (r *ringBuf) write(p []byte) {
	if r == nil || r.cap <= 0 || len(p) == 0 {
		return
	}
	if len(p) >= r.cap {
		copy(r.buf, p[len(p)-r.cap:])
		r.start = 0
		r.size = r.cap
		return
	}
	end := (r.start + r.size) % r.cap
	first := r.cap - end
	if first > len(p) {
		first = len(p)
	}
	copy(r.buf[end:], p[:first])
	if rem := len(p) - first; rem > 0 {
		copy(r.buf[0:], p[first:])
	}
	if r.size+len(p) <= r.cap {
		r.size += len(p)
	} else {
		overflow := r.size + len(p) - r.cap
		r.start = (r.start + overflow) % r.cap
		r.size = r.cap
	}
}

func (r *ringBuf) bytes() []byte {
	if r == nil || r.size == 0 {
		return nil
	}
	out := make([]byte, r.size)
	first := r.cap - r.start
	if first > r.size {
		first = r.size
	}
	copy(out, r.buf[r.start:r.start+first])
	if rem := r.size - first; rem > 0 {
		copy(out[first:], r.buf[:rem])
	}
	return out
}

func (r *ringBuf) clear() {
	if r == nil {
		return
	}
	r.buf = nil
	r.start = 0
	r.size = 0
}

func normalizeOperator(user string) string {
	if user == "" {
		return "operator"
	}
	return user
}

// OpenShell queues an interactive shell (Mode B) on a worker agent and returns
// its session id. The shell shares the machine's FIFO with one-shot commands.
func (m *Manager) OpenShell(req ShellRequest) (string, error) {
	n, err := m.target(req.Machine)
	if err != nil {
		return "", err
	}
	if req.Session == "" {
		req.Session = exec.NewSessionID()
	}
	m.shMu.Lock()
	if _, exists := m.shells[req.Session]; exists {
		m.shMu.Unlock()
		return "", fmt.Errorf("%w: session %s already exists", ErrBadRequest, req.Session)
	}
	m.shells[req.Session] = &shellSession{
		id:         req.Session,
		machine:    req.Machine,
		user:       normalizeOperator(req.User),
		state:      exec.StateQueued,
		ring:       newRingBuf(m.cfg.ShellScrollbackBytes),
		detachedAt: time.Now(),
	}
	m.shMu.Unlock()

	run := exec.RunPayload{SessionID: req.Session, User: req.User, Mode: exec.ModePTY, Cols: req.Cols, Rows: req.Rows}
	m.remember(run.SessionID, req.Machine)
	if _, err := n.q.Submit(run); err != nil {
		m.shMu.Lock()
		delete(m.shells, req.Session)
		m.shMu.Unlock()
		return "", err
	}
	return run.SessionID, nil
}

// AttachShell attaches a viewer to an open interactive shell session (Mode B),
// returning a snapshot of its bounded scrollback ring buffer and a live event
// channel.
//
// Single active attach per session: a second attach takes over by evicting the
// previous attachment (the underlying PTY stays alive). Live shell sessions
// and their scrollback buffers are kept in memory only and are not persisted
// across a fleet-server restart.
func (m *Manager) AttachShell(req AttachRequest) (*ShellAttachment, error) {
	if req.Session == "" {
		return nil, fmt.Errorf("%w: session is required", ErrBadRequest)
	}
	m.shMu.Lock()
	defer m.shMu.Unlock()
	s := m.shells[req.Session]
	if s == nil || s.state == exec.StateFinished || s.reaped {
		return nil, ErrUnknown
	}
	if req.Machine != "" && req.Machine != s.machine {
		m.log.Warn("shell attach refused: machine mismatch", "component", "fleet", "session", req.Session, "want_machine", s.machine, "got_machine", req.Machine, "operator", req.User)
		return nil, ErrForbidden
	}
	if normalizeOperator(req.User) != normalizeOperator(s.user) {
		m.log.Warn("shell attach refused: operator mismatch", "component", "fleet", "session", req.Session, "owner", s.user, "operator", req.User)
		return nil, ErrForbidden
	}
	mc, ok := m.cfg.Registry.Get(s.machine)
	if !ok || mc.Removed {
		return nil, ErrUnknown
	}
	if s.attached && s.evicted != nil {
		close(s.evicted)
	}
	s.attachID++
	subCh := make(chan exec.Event, shellSubBuffer)
	evicted := make(chan struct{})
	s.attached = true
	s.subCh = subCh
	s.evicted = evicted
	s.detachedAt = time.Time{}

	return &ShellAttachment{
		Session:    s.id,
		Machine:    s.machine,
		User:       s.user,
		State:      s.state,
		Scrollback: s.ring.bytes(),
		Events:     subCh,
		Evicted:    evicted,
		mgr:        m,
		id:         s.attachID,
	}, nil
}

func (m *Manager) detachShell(session string, attachID uint64) {
	m.shMu.Lock()
	defer m.shMu.Unlock()
	s := m.shells[session]
	if s == nil || s.attachID != attachID || !s.attached {
		return
	}
	s.attached = false
	s.subCh = nil
	s.evicted = nil
	s.detachedAt = time.Now()
}

func (m *Manager) reapIdleShells(ctx context.Context, now time.Time) {
	type target struct {
		id, machine, user string
		idle              time.Duration
	}
	var reap []target
	m.shMu.Lock()
	for _, s := range m.shells {
		if !s.attached && s.state != exec.StateFinished && !s.reaped && !s.detachedAt.IsZero() && now.Sub(s.detachedAt) >= m.cfg.ShellIdleTimeout {
			s.reaped = true
			reap = append(reap, target{
				id:      s.id,
				machine: s.machine,
				user:    s.user,
				idle:    now.Sub(s.detachedAt),
			})
		}
	}
	m.shMu.Unlock()

	for _, t := range reap {
		m.log.Info("reaping idle shell session", "component", "fleet", "machine", t.machine, "session", t.id, "operator", t.user, "idle", t.idle.Round(time.Millisecond))
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := m.Cancel(cctx, t.id)
		cancel()
		if err != nil {
			m.shMu.Lock()
			if errors.Is(err, ErrUnknown) {
				delete(m.shells, t.id)
			} else if s := m.shells[t.id]; s != nil {
				s.reaped = false
			}
			m.shMu.Unlock()
			if !errors.Is(err, ErrUnknown) {
				m.log.Warn("reap idle shell failed", "component", "fleet", "machine", t.machine, "session", t.id, "err", err)
			}
		}
	}
}

func (m *Manager) queueFor(session string) (*exec.Queue, string, error) {
	m.omu.Lock()
	machine, ok := m.sessions[session]
	m.omu.Unlock()
	if !ok {
		return nil, "", ErrUnknown
	}
	m.mu.Lock()
	n := m.nodes[machine]
	m.mu.Unlock()
	if n == nil {
		return nil, "", ErrUnknown
	}
	return n.q, machine, nil
}

// Cancel removes a queued command or terminates the running one (a shell or a
// one-shot).
func (m *Manager) Cancel(ctx context.Context, session string) error {
	q, _, err := m.queueFor(session)
	if err != nil {
		return err
	}
	if err := q.Cancel(ctx, session); err != nil {
		if errors.Is(err, exec.ErrUnknownSession) {
			m.shMu.Lock()
			delete(m.shells, session)
			m.shMu.Unlock()
			return ErrUnknown
		}
		return err
	}
	return nil
}

// ShellInput sends typed bytes to a running shell.
func (m *Manager) ShellInput(ctx context.Context, session string, data []byte) error {
	q, _, err := m.queueFor(session)
	if err != nil {
		return err
	}
	return q.Input(ctx, session, data, "")
}

// ShellResize changes the window size of a running shell.
func (m *Manager) ShellResize(ctx context.Context, session string, cols, rows uint16) error {
	q, _, err := m.queueFor(session)
	if err != nil {
		return err
	}
	return q.Resize(ctx, session, cols, rows)
}

// ShellConfirm is the operator confirming a dangerous line the agent refused:
// it mints a token for exactly that line on that machine and sends the line
// again with the token. The agent re-checks everything.
func (m *Manager) ShellConfirm(ctx context.Context, session, line string) error {
	q, machine, err := m.queueFor(session)
	if err != nil {
		return err
	}
	if _, hit := m.cfg.Gate.Match(line); !hit {
		return fmt.Errorf("%w: line does not match the dangerous-command list", ErrBadRequest)
	}
	token := m.cfg.Gate.Mint(machine, line, m.cfg.ConfirmTTL)
	return q.Input(ctx, session, []byte(line+"\r"), token)
}

// Commands lists queued, running and recently finished commands and shells of
// every machine, newest first. History is kept in memory for fast reads and
// persisted under <DataDir>/commands/history.jsonl across restarts.
func (m *Manager) Commands() []Command {
	m.mu.Lock()
	nodes := make([]*node, 0, len(m.nodes))
	for _, n := range m.nodes {
		nodes = append(nodes, n)
	}
	m.mu.Unlock()

	seen := make(map[string]struct{})
	var out []Command
	for _, n := range nodes {
		pos := 0
		for _, e := range n.q.Snapshot() {
			c := commandFromEntry(n.id, e)
			if e.State == exec.StateQueued {
				pos++
				c.Position = pos
			}
			seen[c.Session] = struct{}{}
			out = append(out, c)
		}
	}
	m.omu.Lock()
	for id, c := range m.history {
		if _, ok := seen[id]; !ok {
			out = append(out, cloneCommand(c))
		}
	}
	m.omu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Queued.After(out[j].Queued) })
	return out
}

func commandFromEntry(machine string, e exec.Entry) Command {
	c := Command{
		Session: e.SessionID, Machine: machine, Command: e.Command, User: e.User, Mode: e.Mode,
		State: e.State, CgroupPath: e.CgroupPath, Exit: e.Exit, Queued: e.Queued,
	}
	if !e.Started.IsZero() {
		t := e.Started
		c.Started = &t
	}
	if !e.Finished.IsZero() {
		t := e.Finished
		c.Finished = &t
	}
	return c
}

func cloneCommand(c Command) Command {
	if c.Exit != nil {
		x := *c.Exit
		c.Exit = &x
	}
	if c.Started != nil {
		t := *c.Started
		c.Started = &t
	}
	if c.Finished != nil {
		t := *c.Finished
		c.Finished = &t
	}
	return c
}

// Command returns one command with its captured output.
func (m *Manager) Command(session string) (Command, []OutputSegment, bool) {
	for _, c := range m.Commands() {
		if c.Session == session {
			return c, m.Output(session), true
		}
	}
	return Command{}, nil, false
}

// Output returns what a one-shot command printed (bounded; see maxOutputBytes).
func (m *Manager) Output(session string) []OutputSegment {
	m.omu.Lock()
	defer m.omu.Unlock()
	b := m.outputs[session]
	if b == nil {
		return nil
	}
	out := append([]OutputSegment(nil), b.segs...)
	if b.truncated {
		out = append(out, OutputSegment{Stream: "note", Text: "[output truncated]"})
	}
	return out
}

// remember records that a session belongs to a machine and evicts the oldest.
func (m *Manager) remember(session, machine string) {
	m.omu.Lock()
	defer m.omu.Unlock()
	if _, ok := m.sessions[session]; ok {
		return
	}
	m.sessions[session] = machine
	m.order = append(m.order, session)
	for len(m.order) > maxSessions {
		old := m.order[0]
		m.order = m.order[1:]
		delete(m.sessions, old)
		delete(m.outputs, old)
		delete(m.history, old)
	}
}

// onExec runs on the channel read loop: record one-shot output, drain PTY
// output into bounded per-session scrollback, update command history, and
// publish.
func (m *Manager) onExec(machine string, ev exec.Event) {
	if ev.Entry.Mode == exec.ModePTY {
		m.handleShellExec(ev)
	}
	if ev.Kind == exec.EventChunk && ev.Chunk != nil && ev.Entry.Mode == exec.ModeOneShot {
		m.appendOutput(ev.SessionID, ev.Chunk)
	}
	if ev.Kind == exec.EventQueued || ev.Kind == exec.EventRunning || ev.Kind == exec.EventExited {
		rec, persist := m.recordCommandHistory(machine, ev)
		if persist {
			m.appendHistory(rec)
		}
	}
	m.bus.publish(Event{Kind: EventExec, Machine: machine, Exec: &ev})
}

func (m *Manager) handleShellExec(ev exec.Event) {
	m.shMu.Lock()
	defer m.shMu.Unlock()
	s := m.shells[ev.SessionID]
	if s == nil {
		return
	}
	switch ev.Kind {
	case exec.EventQueued:
		s.state = exec.StateQueued
	case exec.EventRunning:
		s.state = exec.StateRunning
	case exec.EventChunk:
		if ev.Chunk != nil && len(ev.Chunk.Data) > 0 {
			s.ring.write(ev.Chunk.Data)
		}
	case exec.EventExited:
		s.state = exec.StateFinished
	}
	if s.attached && s.subCh != nil {
		select {
		case s.subCh <- ev:
		default:
			// Lagging browser: drop its subscription so the agent read loop
			// never blocks on backpressure; the session stays alive detached.
			close(s.subCh)
			s.subCh = nil
			s.evicted = nil
			s.attached = false
			s.detachedAt = time.Now()
		}
	}
	if ev.Kind == exec.EventExited {
		if s.subCh != nil {
			close(s.subCh)
			s.subCh = nil
		}
		s.evicted = nil
		s.attached = false
		s.ring.clear()
		delete(m.shells, ev.SessionID)
	}
}

type persistedCommand struct {
	Command   Command         `json:"command"`
	Output    []OutputSegment `json:"output,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

func (m *Manager) recordCommandHistory(machine string, ev exec.Event) (persistedCommand, bool) {
	c := commandFromEntry(machine, ev.Entry)
	if ev.Kind == exec.EventExited && ev.Exit != nil && c.Exit == nil {
		x := *ev.Exit
		c.Exit = &x
	}
	m.omu.Lock()
	m.history[ev.SessionID] = cloneCommand(c)
	var rec persistedCommand
	if m.historyPath != "" {
		rec.Command = cloneCommand(c)
		if ev.Kind == exec.EventExited {
			if b := m.outputs[ev.SessionID]; b != nil {
				rec.Output = append([]OutputSegment(nil), b.segs...)
				rec.Truncated = b.truncated
			}
		}
	}
	m.omu.Unlock()
	return rec, m.historyPath != ""
}

func (m *Manager) appendHistory(rec persistedCommand) {
	line, err := json.Marshal(rec)
	if err != nil {
		m.log.Warn("encode command history", "component", "fleet", "session", rec.Command.Session, "err", err)
		return
	}
	line = append(line, '\n')

	m.hmu.Lock()
	defer m.hmu.Unlock()
	if err := os.MkdirAll(filepath.Dir(m.historyPath), 0o750); err != nil {
		m.log.Warn("create command history dir", "component", "fleet", "err", err)
		return
	}
	f, err := os.OpenFile(m.historyPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		m.log.Warn("open command history", "component", "fleet", "err", err)
		return
	}
	_, werr := f.Write(line)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		m.log.Warn("write command history", "component", "fleet", "err", werr)
		return
	}
	m.historyLines++
	if m.historyLines > 2*maxSessions {
		m.compactHistoryLocked()
	}
}

func (m *Manager) compactHistoryLocked() {
	m.omu.Lock()
	order := append([]string(nil), m.order...)
	recs := make([]persistedCommand, 0, len(order))
	for _, id := range order {
		c, ok := m.history[id]
		if !ok {
			continue
		}
		rec := persistedCommand{Command: cloneCommand(c)}
		if b := m.outputs[id]; b != nil {
			rec.Output = append([]OutputSegment(nil), b.segs...)
			rec.Truncated = b.truncated
		}
		recs = append(recs, rec)
	}
	m.omu.Unlock()

	dir := filepath.Dir(m.historyPath)
	tmp, err := os.CreateTemp(dir, "history.*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return
	}
	for _, rec := range recs {
		b, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		if _, err := tmp.Write(append(b, '\n')); err != nil {
			_ = tmp.Close()
			cleanup()
			return
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return
	}
	if err := os.Rename(tmpName, m.historyPath); err != nil {
		cleanup()
		return
	}
	m.historyLines = len(recs)
}

func (m *Manager) loadHistory() error {
	if m.historyPath == "" {
		return nil
	}
	f, err := os.Open(m.historyPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	byID := make(map[string]persistedCommand)
	var seq []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 2*maxOutputBytes)
	lines := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		lines++
		var rec persistedCommand
		if err := json.Unmarshal(line, &rec); err != nil || rec.Command.Session == "" {
			continue
		}
		id := rec.Command.Session
		prev, exists := byID[id]
		if !exists {
			seq = append(seq, id)
		} else if len(rec.Output) == 0 && len(prev.Output) > 0 {
			rec.Output = prev.Output
			rec.Truncated = prev.Truncated
		}
		byID[id] = rec
	}
	if err := sc.Err(); err != nil {
		return err
	}

	sort.SliceStable(seq, func(i, j int) bool {
		return byID[seq[i]].Command.Queued.Before(byID[seq[j]].Command.Queued)
	})
	if len(seq) > maxSessions {
		seq = seq[len(seq)-maxSessions:]
	}

	now := time.Now()
	m.omu.Lock()
	for _, id := range seq {
		rec := byID[id]
		c := rec.Command
		if c.State != exec.StateFinished {
			c.State = exec.StateFinished
			if c.Finished == nil {
				t := now
				c.Finished = &t
			}
			if c.Exit == nil {
				c.Exit = &exec.ExitPayload{
					SessionID: c.Session,
					ExitCode:  -1,
					Reason:    exec.ReasonLost,
					Error:     "server restarted",
				}
			}
		}
		m.sessions[id] = c.Machine
		m.order = append(m.order, id)
		m.history[id] = c
		if len(rec.Output) > 0 || rec.Truncated {
			size := 0
			for _, seg := range rec.Output {
				size += len(seg.Text)
			}
			m.outputs[id] = &outBuf{segs: rec.Output, size: size, truncated: rec.Truncated}
		}
	}
	m.omu.Unlock()
	m.historyLines = lines
	return nil
}

func (m *Manager) appendOutput(session string, c *exec.ChunkPayload) {
	m.omu.Lock()
	defer m.omu.Unlock()
	b := m.outputs[session]
	if b == nil {
		b = &outBuf{}
		m.outputs[session] = b
	}
	if b.size >= maxOutputBytes {
		b.truncated = true
		return
	}
	data := c.Data
	if room := maxOutputBytes - b.size; len(data) > room {
		data, b.truncated = data[:room], true
	}
	b.size += len(data)
	if n := len(b.segs); n > 0 && b.segs[n-1].Stream == c.Stream {
		b.segs[n-1].Text += string(data)
		return
	}
	b.segs = append(b.segs, OutputSegment{Stream: c.Stream, Text: string(data)})
}

// --- service control ---------------------------------------------------------

// ServiceControl sends a service-control action (stop, start, or status) to an
// online agent of any role (worker or monitor) over the control channel and
// waits for the agent's result. It is independent of the command executor.
func (m *Manager) ServiceControl(ctx context.Context, machine, action string) (channel.ServiceControlResultPayload, error) {
	switch action {
	case channel.ServiceActionStop, channel.ServiceActionStart, channel.ServiceActionStatus:
	default:
		return channel.ServiceControlResultPayload{}, fmt.Errorf("%w: invalid service-control action %q", ErrBadRequest, action)
	}
	mc, ok := m.cfg.Registry.Get(machine)
	if !ok || mc.Removed {
		return channel.ServiceControlResultPayload{}, ErrUnknown
	}
	m.mu.Lock()
	n := m.nodes[machine]
	mctx := m.ctx
	up := n != nil && n.link != nil && n.link.Online()
	m.mu.Unlock()
	if !up {
		return channel.ServiceControlResultPayload{}, ErrOffline
	}

	reqID := exec.NewSessionID()
	env, err := channel.NewEnvelope(channel.TypeServiceControl, reqID, channel.ServiceControlPayload{Action: action})
	if err != nil {
		return channel.ServiceControlResultPayload{}, err
	}

	ch := make(chan channel.ServiceControlResultPayload, 1)
	m.scMu.Lock()
	m.scPending[reqID] = ch
	m.scMu.Unlock()
	defer func() {
		m.scMu.Lock()
		delete(m.scPending, reqID)
		m.scMu.Unlock()
	}()

	if err := n.link.Send(ctx, env); err != nil {
		return channel.ServiceControlResultPayload{}, err
	}

	var mgrDone <-chan struct{}
	if mctx != nil {
		mgrDone = mctx.Done()
	}
	select {
	case res := <-ch:
		if !res.OK {
			msg := res.Error
			if msg == "" {
				msg = "service-control failed"
			}
			return res, fmt.Errorf("fleet: service-control %s on %s: %s", action, machine, msg)
		}
		switch action {
		case channel.ServiceActionStop:
			_ = m.cfg.Registry.Update(machine, func(mc *registry.Machine) { mc.Stopped = true })
			m.publishMachine(machine)
		case channel.ServiceActionStart:
			_ = m.cfg.Registry.Update(machine, func(mc *registry.Machine) { mc.Stopped = false })
			m.publishMachine(machine)
		}
		m.log.Info("service-control completed", "component", "fleet", "machine", machine, "action", action, "state", res.State)
		return res, nil
	case <-ctx.Done():
		return channel.ServiceControlResultPayload{}, ctx.Err()
	case <-mgrDone:
		return channel.ServiceControlResultPayload{}, mctx.Err()
	}
}

func (m *Manager) recordServiceControlResult(n *node, env channel.Envelope) {
	var res channel.ServiceControlResultPayload
	if err := json.Unmarshal(env.Payload, &res); err != nil {
		m.log.Warn("bad service-control result", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	m.scMu.Lock()
	ch := m.scPending[env.ID]
	delete(m.scPending, env.ID)
	m.scMu.Unlock()
	if ch != nil {
		select {
		case ch <- res:
		default:
		}
	}
}

// --- public IP ---------------------------------------------------------------

func (m *Manager) recordPublicIP(n *node, env channel.Envelope) {
	var p channel.PublicIPPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		m.log.Warn("bad public-ip payload", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	if p.IP == "" || net.ParseIP(p.IP) == nil {
		m.log.Warn("invalid public-ip", "component", "fleet", "machine", n.id, "ip", p.IP)
		return
	}
	at := p.At
	if at.IsZero() {
		at = time.Now()
	}
	if err := m.cfg.Registry.Update(n.id, func(mc *registry.Machine) {
		t := at
		mc.PublicIP = p.IP
		mc.PublicIPAt = &t
	}); err != nil {
		m.log.Warn("update public-ip", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	m.publishMachine(n.id)
}

// --- disk state --------------------------------------------------------------

func (m *Manager) recordDiskState(n *node, env channel.Envelope) {
	var p channel.DiskStatePayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		m.log.Warn("bad disk-state payload", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	switch p.Status {
	case "ok", "warn", "critical":
	default:
		m.log.Warn("invalid disk-state status", "component", "fleet", "machine", n.id, "status", p.Status)
		return
	}
	if p.FreeBytes > p.TotalBytes {
		m.log.Warn("invalid disk-state bytes", "component", "fleet", "machine", n.id, "total", p.TotalBytes, "free", p.FreeBytes)
		return
	}
	at := p.At
	if at.IsZero() {
		at = time.Now()
	}
	if err := m.cfg.Registry.Update(n.id, func(mc *registry.Machine) {
		t := at
		mc.Disk = registry.DiskState{
			Status:     p.Status,
			TotalBytes: p.TotalBytes,
			FreeBytes:  p.FreeBytes,
			UpdatedAt:  &t,
		}
	}); err != nil {
		m.log.Warn("update disk-state", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	m.publishMachine(n.id)
}

// RefreshPublicIP asks an online agent to re-run its reflector public-IP check
// immediately. Returns ErrUnknown if the machine does not exist or is removed,
// and ErrOffline if the agent is not currently connected.
func (m *Manager) RefreshPublicIP(ctx context.Context, machine string) error {
	mc, ok := m.cfg.Registry.Get(machine)
	if !ok || mc.Removed {
		return ErrUnknown
	}
	m.mu.Lock()
	n := m.nodes[machine]
	up := n != nil && n.link != nil && n.link.Online()
	m.mu.Unlock()
	if !up {
		return ErrOffline
	}
	env, err := channel.NewEnvelope(channel.TypePublicIPRefresh, "", nil)
	if err == nil {
		err = n.link.Send(ctx, env)
	}
	if err != nil {
		m.log.Warn("push public-ip refresh", "component", "fleet", "machine", machine, "err", err)
		return err
	}
	m.log.Info("pushed public-ip refresh", "component", "fleet", "machine", machine)
	return nil
}

// --- watchdog host state persistence -----------------------------------------

func (m *Manager) handleWatchdogEvent(n *node, env channel.Envelope) {
	var p channel.WatchdogEventPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		m.recordWatchdogEvent(n, env)
		return
	}
	for _, old := range m.WatchdogEvents(n.id) {
		if sameWatchdogPayload(old.WatchdogEventPayload, p) {
			return
		}
	}
	m.recordWatchdogEvent(n, env)

	evs := m.WatchdogEvents(n.id)
	if len(evs) == 0 {
		return
	}
	ev := evs[len(evs)-1]

	autoReset := true
	if mc, ok := m.cfg.Registry.Get(n.id); ok {
		autoReset = m.watchdogAutoReset(mc.ID, mc.Role)
	}
	now := time.Now()
	if err := m.cfg.Registry.Update(n.id, func(mc *registry.Machine) {
		if mc.WatchdogState == nil {
			mc.WatchdogState = &registry.WatchdogState{}
		}
		if mc.WatchdogState.LastEvent != nil && sameWatchdogPayload(WatchdogEventFromRecord(*mc.WatchdogState.LastEvent).WatchdogEventPayload, ev.WatchdogEventPayload) {
			return
		}
		ApplyWatchdogEvent(mc.WatchdogState, ev, autoReset, now)
	}); err != nil {
		m.log.Warn("update watchdog state", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	m.persistRegistry()
}

func (m *Manager) persistRegistry() {
	if m.regStore == nil {
		return
	}
	m.regSaveMu.Lock()
	defer m.regSaveMu.Unlock()
	if err := m.regStore.Save(m.cfg.Registry); err != nil {
		m.log.Warn("persist watchdog state", "component", "fleet", "err", err)
	}
}

func (m *Manager) watchdogAutoReset(machine, role string) bool {
	def := watchdog.DefaultPolicy().AutoResetMidnight
	if role != registry.RoleWorker {
		return def
	}
	o := m.Observe()
	if o == nil || o.Profile == nil {
		return def
	}
	pf, err := o.Profile.Current()
	if err != nil || pf == nil {
		return def
	}
	pol, err := pf.Effective(machine, nil)
	if err != nil {
		return def
	}
	return pol.AutoResetMidnight
}

func sameWatchdogPayload(a, b channel.WatchdogEventPayload) bool {
	return a == b || (a.Time.Equal(b.Time) && a.Kind == b.Kind && a.Scope == b.Scope &&
		a.Metric == b.Metric && a.Target == b.Target && a.Step == b.Step && a.Detail == b.Detail && a.Value == b.Value)
}

// WatchdogEventFromRecord converts a persisted registry.WatchdogEventRecord
// back into a fleet.WatchdogEvent.
func WatchdogEventFromRecord(r registry.WatchdogEventRecord) WatchdogEvent {
	return WatchdogEvent{
		Machine: r.Machine,
		WatchdogEventPayload: channel.WatchdogEventPayload{
			Time:   r.Time,
			Kind:   r.Kind,
			Scope:  r.Scope,
			Metric: r.Metric,
			Value:  r.Value,
			Target: r.Target,
			Step:   r.Step,
			Detail: r.Detail,
		},
		Received: r.Received,
	}
}

// ApplyWatchdogEvent folds one watchdog event into ws at now, keeping daily
// restart/reboot counters scoped to now's local date.
func ApplyWatchdogEvent(ws *registry.WatchdogState, ev WatchdogEvent, autoReset bool, now time.Time) {
	today := now.In(time.Local).Format("2006-01-02")
	if ws.Day != "" && ws.Day != today {
		ws.RestartsToday, ws.RebootsToday = 0, 0
	}
	ws.Day = today
	if ws.Stage == "" {
		ws.Stage = "none"
	}
	ws.Events++
	ws.LastEvent = &registry.WatchdogEventRecord{
		Machine:  ev.Machine,
		Time:     ev.Time,
		Kind:     ev.Kind,
		Scope:    ev.Scope,
		Metric:   ev.Metric,
		Value:    ev.Value,
		Target:   ev.Target,
		Step:     ev.Step,
		Detail:   ev.Detail,
		Received: ev.Received,
	}
	switch ev.Kind {
	case watchdog.EventBreach:
		ws.Breaches = addUniqueString(ws.Breaches, ev.Scope+"/"+ev.Metric)
	case watchdog.EventRecovered:
		ws.Breaches = removeString(ws.Breaches, ev.Scope+"/"+ev.Metric)
	case watchdog.EventAction:
		ws.Stage = ev.Step
		day := ev.Time.In(time.Local).Format("2006-01-02")
		switch ev.Step {
		case "throttle":
			ws.Throttled = addUniqueString(ws.Throttled, ev.Target)
		case "restart":
			if day == today {
				ws.RestartsToday++
			}
		case "reboot":
			if day == today {
				ws.RebootsToday++
			}
		}
	case watchdog.EventLifted:
		ws.Throttled = removeString(ws.Throttled, ev.Target)
	case watchdog.EventAlertOnly:
		ws.AlertOnly = true
	case watchdog.EventDay:
		ws.RestartsToday, ws.RebootsToday = 0, 0
		ws.Stage = "none"
		if autoReset {
			ws.AlertOnly = false
		}
	case watchdog.EventCounters:
		ws.RestartsToday, ws.RebootsToday = 0, 0
		ws.Stage = "none"
		ws.AlertOnly = false
	}
}

func addUniqueString(list []string, val string) []string {
	for _, x := range list {
		if x == val {
			return list
		}
	}
	return append(list, val)
}

func removeString(list []string, val string) []string {
	out := list[:0]
	for _, x := range list {
		if x != val {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
