package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/enroll"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/version"
)

const (
	defaultUpdateChunkBytes = 512 << 10
	maxUpdateChunkBytes     = 1 << 20

	// agentUpdateStallTimeout is how long an in-progress self-update may go
	// without an agent progress report before it is swept to failed. 5 minutes
	// is comfortably longer than the agent's own ReconnectTimeout (60s) +
	// GraceWindow (120s) + a channel download, so it only fires on genuine
	// non-progress (e.g. an older agent build that ignores TypeAgentUpdate),
	// never on a healthy slow update or a legitimate restart gap.
	agentUpdateStallTimeout = 5 * time.Minute

	agentUpdateStallError = "timed out: agent did not report update progress"
)

// AgentUpdateState tracks the latest push-triggered self-update state for one
// machine in memory (surfaced via MachineInfo and /ws/events).
type AgentUpdateState struct {
	RequestID     string    `json:"request_id,omitempty"`
	TargetVersion string    `json:"target_version,omitempty"`
	Status        string    `json:"status"` // requested | downloading | applying | ok | failed
	Error         string    `json:"error,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`

	// progressAt is the server-side time of the last dispatch/agent report. The
	// stall sweep uses it (falling back to UpdatedAt when zero) so a skewed
	// agent clock in an agent-supplied UpdatedAt cannot cause a false timeout.
	progressAt time.Time
}

func (s *AgentUpdateState) clone() *AgentUpdateState {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

// AgentUpdateOutcome describes the dispatch result of an update trigger.
type AgentUpdateOutcome struct {
	Machine        string `json:"machine"`
	Status         string `json:"status"` // dispatched | noop | failed
	TargetVersion  string `json:"target_version"`
	CurrentVersion string `json:"current_version,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	Error          string `json:"error,omitempty"`
}

// ServerVersion returns the version string the manager compares agents against
// (Config.ServerVersion if set, else internal/version.Version).
func (m *Manager) ServerVersion() string {
	if v := strings.TrimSpace(m.cfg.ServerVersion); v != "" {
		return v
	}
	return version.Version
}

func (m *Manager) binaryResolver() func(goos, arch string) (string, error) {
	if m.cfg.AgentBinaryResolver != nil {
		return m.cfg.AgentBinaryResolver
	}
	dir := m.cfg.AgentBinaryDir
	fallback := m.cfg.AgentBinaryPath
	if dir == "" && fallback == "" && m.cfg.Provisioner != nil {
		dir = m.cfg.Provisioner.AgentBinaryDir
		fallback = m.cfg.Provisioner.AgentBinaryPath
	}
	return enroll.BinaryDir(dir, fallback, runtime.GOOS, runtime.GOARCH)
}

// ResolveAgentBinary resolves the matching fleet-agent-<os>-<arch> binary and
// its SHA-256 checksum for the given platform.
func (m *Manager) ResolveAgentBinary(goos, arch string) (enroll.BinaryInfo, error) {
	return enroll.ResolveBinary(m.binaryResolver(), goos, arch)
}

func (m *Manager) updateState(id string) *AgentUpdateState {
	m.updMu.Lock()
	defer m.updMu.Unlock()
	return m.updates[id].clone()
}

func (m *Manager) setUpdateState(id string, st AgentUpdateState) {
	m.updMu.Lock()
	cp := st
	m.updates[id] = &cp
	m.updMu.Unlock()
}

// sweepStalledUpdates flips any requested/downloading/applying update whose
// last progress is older than the stall timeout to failed (timed out) and
// publishes the machine so the panel updates live. Terminal ok/failed states
// are never touched. Ids are collected under the lock and published after.
func (m *Manager) sweepStalledUpdates(now time.Time) {
	timeout := m.cfg.AgentUpdateStallTimeout
	if timeout <= 0 {
		timeout = agentUpdateStallTimeout
	}
	var stalled []string
	m.updMu.Lock()
	for id, st := range m.updates {
		if st == nil {
			continue
		}
		switch st.Status {
		case "requested", "downloading", "applying":
		default:
			continue
		}
		last := st.progressAt
		if last.IsZero() {
			last = st.UpdatedAt
		}
		if now.Sub(last) < timeout {
			continue
		}
		m.updates[id] = &AgentUpdateState{
			RequestID:     st.RequestID,
			TargetVersion: st.TargetVersion,
			Status:        "failed",
			Error:         agentUpdateStallError,
			UpdatedAt:     now.UTC(),
			progressAt:    now.UTC(),
		}
		stalled = append(stalled, id)
	}
	m.updMu.Unlock()
	for _, id := range stalled {
		m.log.Warn("agent self-update timed out", "component", "fleet", "machine", id, "timeout", timeout)
		m.publishMachine(id)
	}
}

func (m *Manager) machinePlatform(id string, mc registry.Machine) (goos, arch string) {
	m.mu.Lock()
	n := m.nodes[id]
	if n != nil {
		goos, arch = n.agentOS, n.agentArch
	}
	m.mu.Unlock()
	if goos == "" && mc.Specs != nil {
		goos = mc.Specs.OS
	}
	if arch == "" && mc.Specs != nil {
		arch = mc.Specs.Arch
	}
	if goos == "" {
		goos = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	if normOS, normArch, err := config.NormalizeAgentPlatform(goos, arch); err == nil {
		return normOS, normArch
	}
	return goos, arch
}

// UpdateAgent triggers a self-update on one online agent by sending
// TypeAgentUpdate with the matching binary metadata and checksum. If the agent
// is already running the server's version, it returns status "noop" without
// restarting the agent.
func (m *Manager) UpdateAgent(ctx context.Context, machine string) (AgentUpdateOutcome, error) {
	mc, ok := m.cfg.Registry.Get(machine)
	if !ok || mc.Removed {
		return AgentUpdateOutcome{}, ErrUnknown
	}
	m.mu.Lock()
	n := m.nodes[machine]
	up := n != nil && n.link != nil && n.link.Online()
	m.mu.Unlock()
	if !up {
		return AgentUpdateOutcome{}, ErrOffline
	}

	target := m.ServerVersion()
	if mc.AgentVersion != "" && mc.AgentVersion == target {
		return AgentUpdateOutcome{
			Machine:        machine,
			Status:         "noop",
			TargetVersion:  target,
			CurrentVersion: mc.AgentVersion,
		}, nil
	}

	goos, arch := m.machinePlatform(machine, mc)
	bin, err := m.ResolveAgentBinary(goos, arch)
	if err != nil {
		return AgentUpdateOutcome{}, err
	}

	reqID := exec.NewSessionID()
	p := channel.AgentUpdatePayload{
		TargetVersion: target,
		SHA256:        bin.SHA256,
		Size:          bin.Size,
		OS:            goos,
		Arch:          arch,
	}
	if base := strings.TrimRight(strings.TrimSpace(m.cfg.UpdateBaseURL), "/"); base != "" {
		p.URL = base + "/enroll/update/agent?os=" + url.QueryEscape(goos) + "&arch=" + url.QueryEscape(arch)
		p.Method = "http"
	} else {
		p.Method = "channel"
	}

	env, err := channel.NewEnvelope(channel.TypeAgentUpdate, reqID, p)
	if err != nil {
		return AgentUpdateOutcome{}, err
	}
	if err := n.link.Send(ctx, env); err != nil {
		if errors.Is(err, channel.ErrNotConnected) {
			return AgentUpdateOutcome{}, ErrOffline
		}
		return AgentUpdateOutcome{}, err
	}

	m.setUpdateState(machine, AgentUpdateState{
		RequestID:     reqID,
		TargetVersion: target,
		Status:        "requested",
		UpdatedAt:     time.Now().UTC(),
	})
	m.publishMachine(machine)
	m.log.Info("dispatched agent self-update", "component", "fleet", "machine", machine, "target_version", target, "os", goos, "arch", arch, "method", p.Method)

	return AgentUpdateOutcome{
		Machine:        machine,
		Status:         "dispatched",
		TargetVersion:  target,
		CurrentVersion: mc.AgentVersion,
		RequestID:      reqID,
	}, nil
}

// UpdateOnlineWorkers triggers TypeAgentUpdate on all online, non-removed
// worker agents whose reported version differs from ServerVersion().
func (m *Manager) UpdateOnlineWorkers(ctx context.Context) []AgentUpdateOutcome {
	return m.UpdateOutdated(ctx, true)
}

// UpdateOutdated triggers TypeAgentUpdate on all online, non-removed agents
// (or workers only when workersOnly is true) whose reported version differs
// from ServerVersion().
func (m *Manager) UpdateOutdated(ctx context.Context, workersOnly bool) []AgentUpdateOutcome {
	target := m.ServerVersion()
	list := m.cfg.Registry.List()

	m.mu.Lock()
	var ids []string
	for _, mc := range list {
		if mc.Removed {
			continue
		}
		if workersOnly && mc.Role != registry.RoleWorker {
			continue
		}
		if mc.AgentVersion == target {
			continue
		}
		if n := m.nodes[mc.ID]; n != nil && n.link != nil && n.link.Online() {
			ids = append(ids, mc.ID)
		}
	}
	m.mu.Unlock()

	outcomes := make([]AgentUpdateOutcome, 0, len(ids))
	for _, id := range ids {
		out, err := m.UpdateAgent(ctx, id)
		if err != nil {
			outcomes = append(outcomes, AgentUpdateOutcome{
				Machine:       id,
				Status:        "failed",
				TargetVersion: target,
				Error:         err.Error(),
			})
			continue
		}
		outcomes = append(outcomes, out)
	}
	return outcomes
}

func (m *Manager) recordAgentUpdateResult(n *node, env channel.Envelope) {
	var p channel.AgentUpdateResultPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		m.log.Warn("bad agent-update-result payload", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	switch p.Status {
	case channel.AgentUpdateDownloading, channel.AgentUpdateApplying, channel.AgentUpdateOK, channel.AgentUpdateFailed:
	default:
		m.log.Warn("invalid agent-update-result status", "component", "fleet", "machine", n.id, "status", p.Status)
		return
	}
	at := p.At
	if at.IsZero() {
		at = time.Now().UTC()
	}

	m.updMu.Lock()
	cur := m.updates[n.id]
	if cur != nil && cur.RequestID != "" && env.ID != "" && cur.RequestID != env.ID {
		// Stale update report from an earlier request.
		m.updMu.Unlock()
		return
	}
	target := p.TargetVersion
	if target == "" && cur != nil {
		target = cur.TargetVersion
	}
	reqID := env.ID
	if reqID == "" && cur != nil {
		reqID = cur.RequestID
	}
	m.updates[n.id] = &AgentUpdateState{
		RequestID:     reqID,
		TargetVersion: target,
		Status:        p.Status,
		Error:         p.Error,
		UpdatedAt:     at,
		progressAt:    time.Now().UTC(),
	}
	m.updMu.Unlock()

	if p.Status == channel.AgentUpdateOK && strings.TrimSpace(p.CurrentVersion) != "" {
		ver := strings.TrimSpace(p.CurrentVersion)
		var changed bool
		if err := m.cfg.Registry.Update(n.id, func(mc *registry.Machine) {
			if mc.AgentVersion != ver {
				changed = true
				mc.AgentVersion = ver
			}
		}); err == nil && changed {
			m.persistRegistry()
		}
	}

	if p.Status == channel.AgentUpdateFailed {
		m.log.Warn("agent self-update failed", "component", "fleet", "machine", n.id, "target", target, "err", p.Error)
	} else {
		m.log.Info("agent self-update progress", "component", "fleet", "machine", n.id, "status", p.Status, "target", target)
	}
	m.publishMachine(n.id)
}

func (m *Manager) handleAgentUpdateChunkReq(ctx context.Context, n *node, env channel.Envelope) {
	var req channel.AgentUpdateChunkReqPayload
	sendErr := func(msg string) {
		out, err := channel.NewEnvelope(channel.TypeAgentUpdateChunk, env.ID, channel.AgentUpdateChunkPayload{
			Offset: req.Offset,
			Error:  msg,
		})
		if err == nil {
			_ = n.link.Send(ctx, out)
		}
	}
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		sendErr("bad agent-update-chunk-req: " + err.Error())
		return
	}
	if req.Offset < 0 {
		sendErr("negative chunk offset")
		return
	}
	goos, arch := req.OS, req.Arch
	if goos == "" || arch == "" {
		mc, _ := m.cfg.Registry.Get(n.id)
		goos, arch = m.machinePlatform(n.id, mc)
	}
	bin, err := m.ResolveAgentBinary(goos, arch)
	if err != nil {
		sendErr(err.Error())
		return
	}
	length := req.Length
	if length <= 0 {
		length = defaultUpdateChunkBytes
	}
	if length > maxUpdateChunkBytes {
		length = maxUpdateChunkBytes
	}

	f, err := os.Open(bin.Path)
	if err != nil {
		sendErr("open binary: " + err.Error())
		return
	}
	defer f.Close()

	if _, err := f.Seek(req.Offset, io.SeekStart); err != nil {
		sendErr("seek binary: " + err.Error())
		return
	}
	buf := make([]byte, length)
	nr, rerr := io.ReadFull(f, buf)
	if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
		sendErr("read binary: " + rerr.Error())
		return
	}
	buf = buf[:nr]
	eof := req.Offset+int64(nr) >= bin.Size || errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF)
	resp := channel.AgentUpdateChunkPayload{
		Offset: req.Offset,
		Total:  bin.Size,
		SHA256: bin.SHA256,
		Data:   buf,
		EOF:    eof,
	}
	out, err := channel.NewEnvelope(channel.TypeAgentUpdateChunk, env.ID, resp)
	if err != nil {
		sendErr("encode chunk: " + err.Error())
		return
	}
	if err := n.link.Send(ctx, out); err != nil {
		m.log.Debug("send agent-update-chunk", "component", "fleet", "machine", n.id, "err", err)
	}
}
