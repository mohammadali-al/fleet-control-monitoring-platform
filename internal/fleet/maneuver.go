package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/maneuver"
)

// Maneuver run states.
const (
	ManeuverStatePreparing = "preparing"
	ManeuverStateCommitted = "committed"
	ManeuverStateRunning   = "running"
	ManeuverStateCompleted = "completed"
	ManeuverStateAborted   = "aborted"
	ManeuverStateStopped   = "stopped"
)

const (
	defaultManeuverLeadTime = 3 * time.Second
	maxRememberedRuns       = 100
	maneuverSendTimeout     = 5 * time.Second
)

// AgentRunState tracks one worker agent's participation in a Maneuver run.
type AgentRunState struct {
	Machine        string    `json:"machine"`
	State          string    `json:"state"` // pending | ready | missed | rejected | running | completed | stopped | error | offline
	Reason         string    `json:"reason,omitempty"`
	ActiveReplicas int       `json:"active_replicas,omitempty"`
	SessionIDs     []string  `json:"session_ids,omitempty"`
	ElapsedSec     float64   `json:"elapsed_sec,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ManeuverRun is the live + recent state of one Maneuver execution.
type ManeuverRun struct {
	RunID      string                   `json:"run_id"`
	ManeuverID string                   `json:"maneuver_id"`
	Name       string                   `json:"name"`
	Target     maneuver.Target          `json:"target"`
	TotalSec   int                      `json:"total_sec"`
	Pattern    string                   `json:"pattern"`
	State      string                   `json:"state"`
	StartAt    time.Time                `json:"start_at"`
	CreatedAt  time.Time                `json:"created_at"`
	UpdatedAt  time.Time                `json:"updated_at"`
	FinishedAt *time.Time               `json:"finished_at,omitempty"`
	User       string                   `json:"user,omitempty"`
	Error      string                   `json:"error,omitempty"`
	Agents     map[string]AgentRunState `json:"agents"`
}

// StartManeuverRequest initiates a synchronized Maneuver run.
type StartManeuverRequest struct {
	ManeuverID string
	// Plan allows callers/tests with a materialized plan to run directly
	// without looking up ManeuverID in ManeuverStore/Baseline.
	Plan     *maneuver.MaterializedPlan
	User     string
	Confirm  bool
	LeadTime time.Duration
	StartAt  *time.Time // optional explicit T (used in tests for future/past T)
}

type maneuverState struct {
	mu      sync.Mutex
	runs    map[string]*maneuverRunEntry
	order   []string
	nowFunc func() time.Time
}

type maneuverRunEntry struct {
	run          ManeuverRun
	abortSent    map[string]bool
	handshakeCh  chan struct{}
	handshakeEnd sync.Once
	cancelCoord  context.CancelFunc
}

func newManeuverState() maneuverState {
	return maneuverState{
		runs:    map[string]*maneuverRunEntry{},
		nowFunc: time.Now,
	}
}

func (m *Manager) maneuverNow() time.Time {
	if m.maneuverState.nowFunc != nil {
		return m.maneuverState.nowFunc()
	}
	return time.Now()
}

// ManeuverRuns lists recent and active Maneuver runs, newest first.
func (m *Manager) ManeuverRuns() []ManeuverRun {
	m.maneuverState.mu.Lock()
	defer m.maneuverState.mu.Unlock()
	out := make([]ManeuverRun, 0, len(m.maneuverState.order))
	for i := len(m.maneuverState.order) - 1; i >= 0; i-- {
		id := m.maneuverState.order[i]
		if e, ok := m.maneuverState.runs[id]; ok {
			out = append(out, cloneManeuverRun(e.run))
		}
	}
	return out
}

// ManeuverRun returns a snapshot of a single Maneuver run by RunID.
func (m *Manager) ManeuverRun(runID string) (ManeuverRun, bool) {
	m.maneuverState.mu.Lock()
	defer m.maneuverState.mu.Unlock()
	e, ok := m.maneuverState.runs[runID]
	if !ok {
		return ManeuverRun{}, false
	}
	return cloneManeuverRun(e.run), true
}

// StartManeuver validates the plan, checks worker availability and dangerous-command
// confirmation, pushes maneuver-prepare + future start T, collects ready ACKs,
// and commits T only if all selected workers ACK ready before T.
func (m *Manager) StartManeuver(ctx context.Context, req StartManeuverRequest) (ManeuverRun, error) {
	plan, err := m.resolveManeuverPlan(req)
	if err != nil {
		return ManeuverRun{}, err
	}
	if plan.Pattern.Type != "" && plan.Pattern.Type != maneuver.PatternAllInOne {
		return ManeuverRun{}, fmt.Errorf("%w: pattern %q requires the Load Controller (deferred)", ErrBadRequest, plan.Pattern.Type)
	}
	if len(plan.Items) == 0 {
		return ManeuverRun{}, fmt.Errorf("%w: maneuver has no timeline items", ErrBadRequest)
	}
	if len(plan.Agents) == 0 {
		return ManeuverRun{}, fmt.Errorf("%w: choose at least one worker agent", ErrBadRequest)
	}

	nodes := make(map[string]*node, len(plan.Agents))
	for _, id := range plan.Agents {
		n, err := m.target(id)
		if err != nil {
			return ManeuverRun{}, err
		}
		nodes[id] = n
	}

	lead := req.LeadTime
	if lead == 0 && req.StartAt == nil {
		lead = defaultManeuverLeadTime
	}
	now := m.maneuverNow().UTC()
	startAt := now.Add(lead)
	if req.StartAt != nil {
		startAt = req.StartAt.UTC()
	}

	tokenTTL := m.cfg.ConfirmTTL
	needTTL := time.Until(startAt) + time.Duration(plan.TotalSec)*time.Second + 5*time.Minute
	if needTTL > tokenTTL {
		tokenTTL = needTTL
	}

	// Gate check across all selected workers before sending anything.
	tokens := make(map[string]map[string]string, len(plan.Agents)) // agent -> itemID -> token
	for _, agentID := range plan.Agents {
		tokens[agentID] = map[string]string{}
		for _, it := range plan.Items {
			if pat, hit := m.cfg.Gate.Match(it.Command); hit {
				if !req.Confirm {
					return ManeuverRun{}, &ConfirmRequiredError{Machine: agentID, Command: it.Command, Pattern: pat}
				}
				tokens[agentID][it.ItemID] = m.cfg.Gate.Mint(agentID, it.Command, tokenTTL)
			}
		}
	}

	runID := "m-" + strings.TrimPrefix(exec.NewSessionID(), "s-")
	agentsMap := make(map[string]AgentRunState, len(plan.Agents))
	for _, agentID := range plan.Agents {
		agentsMap[agentID] = AgentRunState{Machine: agentID, State: "pending", UpdatedAt: now}
	}

	m.mu.Lock()
	mgrCtx := m.ctx
	m.mu.Unlock()
	if mgrCtx == nil {
		mgrCtx = context.Background()
	}
	coordCtx, cancelCoord := context.WithCancel(mgrCtx)

	run := ManeuverRun{
		RunID:      runID,
		ManeuverID: plan.ManeuverID,
		Name:       plan.Name,
		Target:     plan.Target,
		TotalSec:   plan.TotalSec,
		Pattern:    maneuver.PatternAllInOne,
		State:      ManeuverStatePreparing,
		StartAt:    startAt,
		CreatedAt:  now,
		UpdatedAt:  now,
		User:       req.User,
		Agents:     agentsMap,
	}

	entry := &maneuverRunEntry{
		run:         run,
		abortSent:   map[string]bool{},
		handshakeCh: make(chan struct{}),
		cancelCoord: cancelCoord,
	}

	m.maneuverState.mu.Lock()
	m.maneuverState.runs[runID] = entry
	m.maneuverState.order = append(m.maneuverState.order, runID)
	if len(m.maneuverState.order) > maxRememberedRuns {
		drop := m.maneuverState.order[0]
		m.maneuverState.order = m.maneuverState.order[1:]
		if old := m.maneuverState.runs[drop]; old != nil && isTerminalRunState(old.run.State) {
			delete(m.maneuverState.runs, drop)
		}
	}
	m.maneuverState.mu.Unlock()

	m.publishManeuver(run)

	// Push maneuver-prepare to all selected worker agents concurrently.
	baseItems := make([]channel.ManeuverPlanItem, len(plan.Items))
	for i, it := range plan.Items {
		baseItems[i] = channel.ManeuverPlanItem{
			ItemID:      it.ItemID,
			BaselineID:  it.BaselineID,
			Name:        it.Name,
			Command:     it.Command,
			StartSec:    it.StartSec,
			DurationSec: it.DurationSec,
			Replicas:    it.Replicas,
		}
	}

	for _, agentID := range plan.Agents {
		n := nodes[agentID]
		itemsCopy := append([]channel.ManeuverPlanItem(nil), baseItems...)
		for i := range itemsCopy {
			if tok := tokens[agentID][itemsCopy[i].ItemID]; tok != "" {
				itemsCopy[i].ConfirmToken = tok
			}
		}
		prep := channel.ManeuverPreparePayload{
			RunID:        runID,
			ManeuverID:   plan.ManeuverID,
			Name:         plan.Name,
			TargetIP:     plan.Target.IP,
			TargetOrigin: plan.Target.Origin,
			StartAt:      startAt,
			TotalSec:     plan.TotalSec,
			Pattern:      maneuver.PatternAllInOne,
			Items:        itemsCopy,
		}
		go func(agentID string, n *node, prep channel.ManeuverPreparePayload) {
			env, err := channel.NewEnvelope(channel.TypeManeuverPrepare, runID, prep)
			if err == nil {
				sctx, cancel := context.WithTimeout(coordCtx, maneuverSendTimeout)
				err = n.link.Send(sctx, env)
				cancel()
			}
			if err != nil {
				m.recordPrepareSendFail(runID, agentID, err)
			}
		}(agentID, n, prep)
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.coordinateManeuverStart(coordCtx, runID, startAt)
	}()

	_ = ctx
	return cloneManeuverRun(run), nil
}

func (m *Manager) resolveManeuverPlan(req StartManeuverRequest) (maneuver.MaterializedPlan, error) {
	if req.Plan != nil {
		return *req.Plan, nil
	}
	if m.manStore == nil || m.baseStore == nil {
		return maneuver.MaterializedPlan{}, ErrUnavailable
	}
	mv, ok := m.manStore.Get(req.ManeuverID)
	if !ok {
		return maneuver.MaterializedPlan{}, fmt.Errorf("%w: maneuver %s", ErrUnknown, req.ManeuverID)
	}
	return maneuver.Materialize(maneuver.Spec{
		ID:          mv.ID,
		Name:        mv.Name,
		Description: mv.Description,
		Target:      mv.Target,
		TotalSec:    mv.TotalSec,
		Pattern:     mv.Pattern,
		Timeline:    mv.Timeline,
		Agents:      mv.Agents,
	}, m.baseStore)
}

func (m *Manager) coordinateManeuverStart(ctx context.Context, runID string, startAt time.Time) {
	waitDur := time.Until(startAt)
	if waitDur < 0 {
		waitDur = 0
	}
	timer := time.NewTimer(waitDur)
	defer timer.Stop()

	m.maneuverState.mu.Lock()
	entry := m.maneuverState.runs[runID]
	m.maneuverState.mu.Unlock()
	if entry == nil {
		return
	}

	select {
	case <-entry.handshakeCh:
	case <-timer.C:
		m.abortUnackedRun(runID, "ready ACK timeout before start_at")
		return
	case <-ctx.Done():
		return
	}

	m.maneuverState.mu.Lock()
	entry = m.maneuverState.runs[runID]
	if entry == nil || entry.run.State != ManeuverStatePreparing {
		m.maneuverState.mu.Unlock()
		return
	}
	now := m.maneuverNow().UTC()
	if !now.Before(startAt) {
		m.maneuverState.mu.Unlock()
		m.abortUnackedRun(runID, "start window elapsed before commit")
		return
	}

	var agentIDs []string
	for id, st := range entry.run.Agents {
		if st.State != channel.ManeuverAckReady {
			m.maneuverState.mu.Unlock()
			m.abortUnackedRun(runID, fmt.Sprintf("agent %s not ready (%s)", id, st.State))
			return
		}
		agentIDs = append(agentIDs, id)
	}
	entry.run.State = ManeuverStateCommitted
	entry.run.UpdatedAt = now
	snap := cloneManeuverRun(entry.run)
	m.maneuverState.mu.Unlock()

	m.publishManeuver(snap)

	// Send commit only after ALL agents ACKed ready.
	for _, agentID := range agentIDs {
		m.sendManeuverEnvelope(ctx, agentID, channel.TypeManeuverCommit, runID, channel.ManeuverCommitPayload{
			RunID:   runID,
			StartAt: startAt,
		})
	}

	// Transition from committed -> running when T arrives (unless stopped/completed early).
	rem := startAt.Sub(m.maneuverNow().UTC())
	if rem > 0 {
		t2 := time.NewTimer(rem)
		select {
		case <-t2.C:
		case <-ctx.Done():
			t2.Stop()
			return
		}
		t2.Stop()
	}

	m.maneuverState.mu.Lock()
	entry = m.maneuverState.runs[runID]
	if entry != nil && entry.run.State == ManeuverStateCommitted {
		entry.run.State = ManeuverStateRunning
		entry.run.UpdatedAt = m.maneuverNow().UTC()
		for id, st := range entry.run.Agents {
			if st.State == channel.ManeuverAckReady {
				st.State = ManeuverStateRunning
				st.UpdatedAt = entry.run.UpdatedAt
				entry.run.Agents[id] = st
			}
		}
		snap = cloneManeuverRun(entry.run)
		m.maneuverState.mu.Unlock()
		m.publishManeuver(snap)
	} else {
		m.maneuverState.mu.Unlock()
	}
}

func (m *Manager) recordPrepareSendFail(runID, agentID string, err error) {
	m.maneuverState.mu.Lock()
	entry := m.maneuverState.runs[runID]
	if entry == nil || entry.run.State != ManeuverStatePreparing {
		m.maneuverState.mu.Unlock()
		return
	}
	now := m.maneuverNow().UTC()
	st := entry.run.Agents[agentID]
	st.State = "offline"
	st.Reason = err.Error()
	st.UpdatedAt = now
	entry.run.Agents[agentID] = st
	m.maneuverState.mu.Unlock()

	m.abortUnackedRun(runID, fmt.Sprintf("agent %s unreachable: %v", agentID, err))
}

func (m *Manager) handleManeuverAck(n *node, env channel.Envelope) {
	var ack channel.ManeuverAckPayload
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		m.log.Warn("bad maneuver-ack payload", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	if ack.RunID == "" {
		ack.RunID = env.ID
	}

	var shouldAbort string
	var snap *ManeuverRun

	m.maneuverState.mu.Lock()
	entry := m.maneuverState.runs[ack.RunID]
	if entry == nil {
		m.maneuverState.mu.Unlock()
		return
	}
	st, ok := entry.run.Agents[n.id]
	if !ok {
		m.maneuverState.mu.Unlock()
		return
	}
	now := m.maneuverNow().UTC()
	st.State = ack.Status
	st.Reason = ack.Reason
	st.UpdatedAt = now
	entry.run.Agents[n.id] = st
	entry.run.UpdatedAt = now

	if entry.run.State == ManeuverStatePreparing {
		switch ack.Status {
		case channel.ManeuverAckReady:
			allReady := true
			for _, ast := range entry.run.Agents {
				if ast.State != channel.ManeuverAckReady {
					allReady = false
					break
				}
			}
			if allReady {
				entry.handshakeEnd.Do(func() { close(entry.handshakeCh) })
			}
		case channel.ManeuverAckMissed, channel.ManeuverAckRejected:
			shouldAbort = fmt.Sprintf("agent %s reported %s", n.id, ack.Status)
			if ack.Reason != "" {
				shouldAbort += ": " + ack.Reason
			}
		}
	} else if ack.Status == channel.ManeuverAckMissed {
		// Late commit rejection by agent: mark agent missed.
		cp := cloneManeuverRun(entry.run)
		snap = &cp
	}
	if shouldAbort == "" && snap == nil {
		cp := cloneManeuverRun(entry.run)
		snap = &cp
	}
	m.maneuverState.mu.Unlock()

	if shouldAbort != "" {
		m.abortUnackedRun(ack.RunID, shouldAbort)
		return
	}
	if snap != nil {
		m.publishManeuver(*snap)
	}
}

func (m *Manager) abortUnackedRun(runID, reason string) {
	var agentIDs []string
	var snap ManeuverRun

	m.maneuverState.mu.Lock()
	entry := m.maneuverState.runs[runID]
	if entry == nil || (entry.run.State != ManeuverStatePreparing && entry.run.State != ManeuverStateCommitted) {
		m.maneuverState.mu.Unlock()
		return
	}
	now := m.maneuverNow().UTC()
	entry.run.State = ManeuverStateAborted
	entry.run.Error = reason
	entry.run.UpdatedAt = now
	entry.run.FinishedAt = &now
	for id, st := range entry.run.Agents {
		if st.State == "pending" {
			st.State = channel.ManeuverAckMissed
			st.Reason = reason
			st.UpdatedAt = now
			entry.run.Agents[id] = st
		}
		agentIDs = append(agentIDs, id)
	}
	entry.handshakeEnd.Do(func() { close(entry.handshakeCh) })
	snap = cloneManeuverRun(entry.run)
	m.maneuverState.mu.Unlock()

	m.publishManeuver(snap)
	for _, id := range agentIDs {
		m.sendManeuverEnvelope(context.Background(), id, channel.TypeManeuverAbort, runID, channel.ManeuverAbortPayload{
			RunID:  runID,
			Reason: reason,
		})
	}
}

func (m *Manager) maneuverMachineOffline(machineID string) {
	var abortIDs []string
	m.maneuverState.mu.Lock()
	for runID, entry := range m.maneuverState.runs {
		if entry.run.State != ManeuverStatePreparing {
			continue
		}
		if st, ok := entry.run.Agents[machineID]; ok && st.State == "pending" {
			st.State = "offline"
			st.Reason = "connection dropped during sync-start"
			st.UpdatedAt = m.maneuverNow().UTC()
			entry.run.Agents[machineID] = st
			abortIDs = append(abortIDs, runID)
		}
	}
	m.maneuverState.mu.Unlock()

	for _, runID := range abortIDs {
		m.abortUnackedRun(runID, fmt.Sprintf("agent %s went offline before ACK", machineID))
	}
}

// StopManeuver stops a preparing, committed, or running Maneuver across all
// selected worker agents.
func (m *Manager) StopManeuver(ctx context.Context, runID, reason string) (ManeuverRun, error) {
	if reason == "" {
		reason = "stopped by operator"
	}
	var agentIDs []string
	var snap ManeuverRun
	var cancel context.CancelFunc

	m.maneuverState.mu.Lock()
	entry, ok := m.maneuverState.runs[runID]
	if !ok {
		m.maneuverState.mu.Unlock()
		return ManeuverRun{}, ErrUnknown
	}
	now := m.maneuverNow().UTC()
	if !isTerminalRunState(entry.run.State) {
		entry.run.State = ManeuverStateStopped
		entry.run.Error = reason
		entry.run.UpdatedAt = now
		entry.run.FinishedAt = &now
		entry.handshakeEnd.Do(func() { close(entry.handshakeCh) })
		cancel = entry.cancelCoord
	}
	for id := range entry.run.Agents {
		agentIDs = append(agentIDs, id)
	}
	snap = cloneManeuverRun(entry.run)
	m.maneuverState.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	m.publishManeuver(snap)
	for _, id := range agentIDs {
		m.sendManeuverEnvelope(ctx, id, channel.TypeManeuverAbort, runID, channel.ManeuverAbortPayload{
			RunID:  runID,
			Reason: reason,
		})
	}
	return snap, nil
}

func (m *Manager) handleManeuverStatus(n *node, env channel.Envelope) {
	var p channel.ManeuverStatusPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		m.log.Warn("bad maneuver-status payload", "component", "fleet", "machine", n.id, "err", err)
		return
	}
	if p.RunID == "" {
		p.RunID = env.ID
	}

	var sendAbort bool
	var abortReason string
	var snap *ManeuverRun

	m.maneuverState.mu.Lock()
	entry := m.maneuverState.runs[p.RunID]
	if entry == nil {
		// Reconstruct a run record if the main server restarted while the
		// worker continued autonomously and now reconnects with its status.
		now := m.maneuverNow().UTC()
		estStart := now
		if p.ElapsedSec > 0 {
			estStart = now.Add(-time.Duration(p.ElapsedSec * float64(time.Second)))
		}
		entry = &maneuverRunEntry{
			run: ManeuverRun{
				RunID:      p.RunID,
				ManeuverID: p.ManeuverID,
				Name:       p.ManeuverID,
				Pattern:    maneuver.PatternAllInOne,
				State:      p.State,
				StartAt:    estStart,
				CreatedAt:  estStart,
				UpdatedAt:  now,
				Agents: map[string]AgentRunState{
					n.id: {Machine: n.id, State: p.State, UpdatedAt: now},
				},
			},
			abortSent:   map[string]bool{},
			handshakeCh: make(chan struct{}),
		}
		close(entry.handshakeCh)
		m.maneuverState.runs[p.RunID] = entry
		m.maneuverState.order = append(m.maneuverState.order, p.RunID)
	}

	// If this run was aborted or stopped on the server while the agent was
	// unreachable and the agent is still running/scheduled, push abort now.
	if (entry.run.State == ManeuverStateStopped || entry.run.State == ManeuverStateAborted) &&
		(p.State == "scheduled" || p.State == "running") {
		sendAbort = true
		abortReason = entry.run.Error
	}

	now := m.maneuverNow().UTC()
	if st, ok := entry.run.Agents[n.id]; ok {
		st.State = p.State
		st.Reason = p.Error
		st.ActiveReplicas = p.ActiveReplicas
		st.SessionIDs = append([]string(nil), p.SessionIDs...)
		st.ElapsedSec = p.ElapsedSec
		st.UpdatedAt = now
		entry.run.Agents[n.id] = st
		entry.run.UpdatedAt = now

		if !isTerminalRunState(entry.run.State) {
			if p.State == ManeuverStateRunning && entry.run.State == ManeuverStateCommitted {
				entry.run.State = ManeuverStateRunning
			}
			allDone := true
			for _, ast := range entry.run.Agents {
				if ast.State != ManeuverStateCompleted && ast.State != ManeuverStateStopped && ast.State != "error" && ast.State != channel.ManeuverAckMissed {
					allDone = false
					break
				}
			}
			if allDone {
				entry.run.State = ManeuverStateCompleted
				entry.run.FinishedAt = &now
			}
		}
		cp := cloneManeuverRun(entry.run)
		snap = &cp
	}
	m.maneuverState.mu.Unlock()

	if sendAbort {
		m.sendManeuverEnvelope(context.Background(), n.id, channel.TypeManeuverAbort, p.RunID, channel.ManeuverAbortPayload{
			RunID:  p.RunID,
			Reason: abortReason,
		})
	}
	if snap != nil {
		m.publishManeuver(*snap)
	}
}

func (m *Manager) sendManeuverEnvelope(ctx context.Context, machineID, typ, id string, payload any) {
	m.mu.Lock()
	n := m.nodes[machineID]
	up := n != nil && n.link != nil && n.link.Online()
	m.mu.Unlock()
	if !up {
		return
	}
	env, err := channel.NewEnvelope(typ, id, payload)
	if err != nil {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, maneuverSendTimeout)
	defer cancel()
	if err := n.link.Send(sctx, env); err != nil && !errors.Is(err, channel.ErrNotConnected) {
		m.log.Debug("send maneuver envelope", "component", "fleet", "machine", machineID, "type", typ, "err", err)
	}
}

func (m *Manager) publishManeuver(r ManeuverRun) {
	cp := cloneManeuverRun(r)
	m.bus.publish(Event{Kind: EventManeuver, Maneuver: &cp})
}

func isTerminalRunState(st string) bool {
	return st == ManeuverStateCompleted || st == ManeuverStateAborted || st == ManeuverStateStopped
}

func cloneManeuverRun(r ManeuverRun) ManeuverRun {
	out := r
	if r.FinishedAt != nil {
		t := *r.FinishedAt
		out.FinishedAt = &t
	}
	if r.Agents != nil {
		out.Agents = make(map[string]AgentRunState, len(r.Agents))
		for k, v := range r.Agents {
			cp := v
			if v.SessionIDs != nil {
				cp.SessionIDs = append([]string(nil), v.SessionIDs...)
			}
			out.Agents[k] = cp
		}
	}
	return out
}

// SetManeuverNowForTest overrides the clock used by Maneuver coordination.
func (m *Manager) SetManeuverNowForTest(fn func() time.Time) {
	m.maneuverState.mu.Lock()
	if fn == nil {
		fn = time.Now
	}
	m.maneuverState.nowFunc = fn
	m.maneuverState.mu.Unlock()
}

// SortAgents returns sorted agent IDs from a ManeuverRun.
func (r ManeuverRun) SortedAgents() []string {
	ids := make([]string, 0, len(r.Agents))
	for id := range r.Agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
