package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
)

const (
	maneuverAckTimeout   = 10 * time.Second
	maneuverStateFile    = "state.json"
	maneuverStateVersion = 1
)

type persistedManeuverState struct {
	Version   int                            `json:"version"`
	Plan      channel.ManeuverPreparePayload `json:"plan"`
	Committed bool                           `json:"committed"`
	State     string                         `json:"state"` // prepared | scheduled | running | completed | stopped
	Error     string                         `json:"error,omitempty"`
	UpdatedAt time.Time                      `json:"updated_at"`
}

type maneuverRunner struct {
	w        *worker
	now      func() time.Time
	stateDir string

	mu         sync.Mutex
	baseCtx    context.Context
	staged     *stagedManeuver
	active     *activeManeuver
	last       *channel.ManeuverStatusPayload
	fallbackEx *exec.Executor
}

type stagedManeuver struct {
	plan   channel.ManeuverPreparePayload
	cancel context.CancelFunc
}

type activeManeuver struct {
	plan   channel.ManeuverPreparePayload
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	sessions map[string]struct{}
}

func newManeuverRunner(w *worker) *maneuverRunner {
	nowFn := w.cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	var dir string
	if w.cfg.DataDir != "" {
		dir = filepath.Join(w.cfg.DataDir, config.ManeuverDirName)
	}
	return &maneuverRunner{
		w:        w,
		now:      nowFn,
		stateDir: dir,
		baseCtx:  w.base,
	}
}

func (r *maneuverRunner) statePath() string {
	if r.stateDir == "" {
		return ""
	}
	return filepath.Join(r.stateDir, maneuverStateFile)
}

func (r *maneuverRunner) saveState(st persistedManeuverState) {
	p := r.statePath()
	if p == "" {
		return
	}
	st.Version = maneuverStateVersion
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = r.now().UTC()
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		r.w.log.Warn("encode maneuver state", "component", "fleet-agent", "err", err)
		return
	}
	data = append(data, '\n')
	if err := os.MkdirAll(r.stateDir, 0o750); err != nil {
		r.w.log.Warn("create maneuver state dir", "component", "fleet-agent", "err", err)
		return
	}
	tmp, err := os.CreateTemp(r.stateDir, maneuverStateFile+".*.tmp")
	if err != nil {
		r.w.log.Warn("create maneuver temp state", "component", "fleet-agent", "err", err)
		return
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return
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
	if err := os.Rename(tmpName, p); err != nil {
		cleanup()
		r.w.log.Warn("save maneuver state", "component", "fleet-agent", "err", err)
	}
}

func (r *maneuverRunner) loadState() (persistedManeuverState, bool) {
	p := r.statePath()
	if p == "" {
		return persistedManeuverState{}, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.w.log.Warn("read maneuver state", "component", "fleet-agent", "err", err)
		}
		return persistedManeuverState{}, false
	}
	var st persistedManeuverState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != maneuverStateVersion {
		r.w.log.Warn("ignore invalid maneuver state", "component", "fleet-agent", "err", err)
		return persistedManeuverState{}, false
	}
	return st, true
}

func (r *maneuverRunner) clearState() {
	p := r.statePath()
	if p == "" {
		return
	}
	_ = os.Remove(p)
}

func (r *maneuverRunner) start(ctx context.Context) {
	r.mu.Lock()
	r.baseCtx = ctx
	r.mu.Unlock()

	st, ok := r.loadState()
	if !ok {
		return
	}
	if !st.Committed {
		// Never resume an uncommitted prepare across reboot.
		r.clearState()
		return
	}
	if st.State == "completed" || st.State == "stopped" {
		r.mu.Lock()
		r.last = &channel.ManeuverStatusPayload{
			RunID:      st.Plan.RunID,
			ManeuverID: st.Plan.ManeuverID,
			State:      st.State,
			Error:      st.Error,
			At:         st.UpdatedAt,
		}
		r.mu.Unlock()
		return
	}
	if st.State != "scheduled" && st.State != "running" {
		return
	}
	if err := validatePreparePayload(st.Plan); err != nil {
		r.clearState()
		return
	}

	now := r.now().UTC()
	endAt := st.Plan.StartAt.Add(time.Duration(st.Plan.TotalSec) * time.Second)
	if !now.Before(endAt) {
		st.State = "completed"
		st.UpdatedAt = now
		r.saveState(st)
		r.mu.Lock()
		r.last = &channel.ManeuverStatusPayload{
			RunID:      st.Plan.RunID,
			ManeuverID: st.Plan.ManeuverID,
			State:      "completed",
			ElapsedSec: float64(st.Plan.TotalSec),
			At:         now,
		}
		r.mu.Unlock()
		return
	}

	r.w.log.Info("rejoining committed maneuver at wall-clock position", "component", "fleet-agent",
		"run_id", st.Plan.RunID, "start_at", st.Plan.StartAt, "elapsed_sec", now.Sub(st.Plan.StartAt).Seconds())
	r.launchRunner(st.Plan)
}

func (r *maneuverRunner) onConnect() {
	r.mu.Lock()
	act := r.active
	var last *channel.ManeuverStatusPayload
	if r.last != nil {
		cp := *r.last
		last = &cp
	}
	r.mu.Unlock()

	if act != nil {
		act.mu.Lock()
		ids := make([]string, 0, len(act.sessions))
		for id := range act.sessions {
			ids = append(ids, id)
		}
		act.mu.Unlock()
		now := r.now().UTC()
		state := "running"
		var elapsed float64
		if now.Before(act.plan.StartAt) {
			state = "scheduled"
		} else {
			elapsed = now.Sub(act.plan.StartAt).Seconds()
		}
		r.sendStatus(channel.ManeuverStatusPayload{
			RunID:          act.plan.RunID,
			ManeuverID:     act.plan.ManeuverID,
			State:          state,
			ActiveReplicas: len(ids),
			SessionIDs:     ids,
			ElapsedSec:     elapsed,
			At:             now,
		})
		return
	}

	if last != nil {
		r.sendStatus(*last)
	}
}

func (r *maneuverRunner) shutdown() {
	r.mu.Lock()
	st := r.staged
	r.staged = nil
	act := r.active
	r.active = nil
	fb := r.fallbackEx
	r.fallbackEx = nil
	r.mu.Unlock()

	if st != nil && st.cancel != nil {
		st.cancel()
	}
	if act != nil {
		if act.cancel != nil {
			act.cancel()
		}
		<-act.done
	}
	if fb != nil {
		fb.Shutdown()
	}
}

func (r *maneuverRunner) handle(ctx context.Context, env channel.Envelope) {
	switch env.Type {
	case channel.TypeManeuverPrepare:
		var p channel.ManeuverPreparePayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			r.w.log.Warn("bad maneuver-prepare payload", "component", "fleet-agent", "err", err)
			return
		}
		if p.RunID == "" {
			p.RunID = env.ID
		}
		go r.handlePrepare(p)
	case channel.TypeManeuverCommit:
		var p channel.ManeuverCommitPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			r.w.log.Warn("bad maneuver-commit payload", "component", "fleet-agent", "err", err)
			return
		}
		if p.RunID == "" {
			p.RunID = env.ID
		}
		r.handleCommit(p)
	case channel.TypeManeuverAbort:
		var p channel.ManeuverAbortPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			r.w.log.Warn("bad maneuver-abort payload", "component", "fleet-agent", "err", err)
			return
		}
		if p.RunID == "" {
			p.RunID = env.ID
		}
		r.handleAbort(p)
	}
	_ = ctx
}

func (r *maneuverRunner) handlePrepare(p channel.ManeuverPreparePayload) {
	now := r.now().UTC()
	if !now.Before(p.StartAt) {
		r.w.log.Warn("maneuver prepare missed start window", "component", "fleet-agent",
			"run_id", p.RunID, "start_at", p.StartAt, "now", now)
		r.sendAck(channel.ManeuverAckPayload{
			RunID:      p.RunID,
			Status:     channel.ManeuverAckMissed,
			Reason:     "received maneuver-prepare at or after start_at",
			ReceivedAt: now,
			StartAt:    p.StartAt,
		})
		return
	}

	if err := validatePreparePayload(p); err != nil {
		r.sendAck(channel.ManeuverAckPayload{
			RunID:      p.RunID,
			Status:     channel.ManeuverAckRejected,
			Reason:     err.Error(),
			ReceivedAt: now,
			StartAt:    p.StartAt,
		})
		return
	}

	ex, err := r.waitExecutor(p.StartAt)
	if err != nil {
		r.sendAck(channel.ManeuverAckPayload{
			RunID:      p.RunID,
			Status:     channel.ManeuverAckRejected,
			Reason:     err.Error(),
			ReceivedAt: r.now().UTC(),
			StartAt:    p.StartAt,
		})
		return
	}

	for _, it := range p.Items {
		if err := ex.AuthorizeOneShot(it.Command, it.ConfirmToken); err != nil {
			r.sendAck(channel.ManeuverAckPayload{
				RunID:      p.RunID,
				Status:     channel.ManeuverAckRejected,
				Reason:     fmt.Sprintf("item %s refused by gate: %v", it.ItemID, err),
				ReceivedAt: r.now().UTC(),
				StartAt:    p.StartAt,
			})
			return
		}
	}

	now = r.now().UTC()
	if !now.Before(p.StartAt) {
		r.sendAck(channel.ManeuverAckPayload{
			RunID:      p.RunID,
			Status:     channel.ManeuverAckMissed,
			Reason:     "start window elapsed before agent ready",
			ReceivedAt: now,
			StartAt:    p.StartAt,
		})
		return
	}

	r.mu.Lock()
	if r.active != nil {
		r.mu.Unlock()
		r.sendAck(channel.ManeuverAckPayload{
			RunID:      p.RunID,
			Status:     channel.ManeuverAckRejected,
			Reason:     fmt.Sprintf("another maneuver (%s) is already active", r.active.plan.RunID),
			ReceivedAt: now,
			StartAt:    p.StartAt,
		})
		return
	}
	if r.staged != nil && r.staged.cancel != nil {
		r.staged.cancel()
	}
	stageCtx, stageCancel := context.WithCancel(r.baseCtx)
	r.staged = &stagedManeuver{plan: p, cancel: stageCancel}
	r.mu.Unlock()

	r.saveState(persistedManeuverState{
		Plan:      p,
		Committed: false,
		State:     "prepared",
		UpdatedAt: now,
	})

	go r.expireUncommitted(stageCtx, p.RunID, p.StartAt)

	r.sendAck(channel.ManeuverAckPayload{
		RunID:      p.RunID,
		Status:     channel.ManeuverAckReady,
		ReceivedAt: now,
		StartAt:    p.StartAt,
	})
}

func (r *maneuverRunner) expireUncommitted(ctx context.Context, runID string, startAt time.Time) {
	wait := startAt.Sub(r.now().UTC())
	if wait < 0 {
		wait = 0
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
		r.mu.Lock()
		if r.staged != nil && r.staged.plan.RunID == runID {
			r.staged = nil
			r.w.log.Info("maneuver uncommitted plan expired at start_at", "component", "fleet-agent", "run_id", runID)
			r.mu.Unlock()
			r.clearState()
			return
		}
		r.mu.Unlock()
	}
}

func (r *maneuverRunner) handleCommit(p channel.ManeuverCommitPayload) {
	now := r.now().UTC()
	r.mu.Lock()
	st := r.staged
	if st == nil || st.plan.RunID != p.RunID {
		r.mu.Unlock()
		r.w.log.Warn("maneuver commit ignored: no matching staged plan", "component", "fleet-agent", "run_id", p.RunID)
		return
	}
	r.staged = nil
	r.mu.Unlock()

	if st.cancel != nil {
		st.cancel()
	}

	if !now.Before(st.plan.StartAt) {
		r.w.log.Warn("maneuver commit missed start window", "component", "fleet-agent",
			"run_id", p.RunID, "start_at", st.plan.StartAt, "now", now)
		r.clearState()
		r.sendAck(channel.ManeuverAckPayload{
			RunID:      p.RunID,
			Status:     channel.ManeuverAckMissed,
			Reason:     "received maneuver-commit at or after start_at",
			ReceivedAt: now,
			StartAt:    st.plan.StartAt,
		})
		return
	}

	r.saveState(persistedManeuverState{
		Plan:      st.plan,
		Committed: true,
		State:     "scheduled",
		UpdatedAt: now,
	})
	r.launchRunner(st.plan)
}

func (r *maneuverRunner) handleAbort(p channel.ManeuverAbortPayload) {
	r.mu.Lock()
	var st *stagedManeuver
	if r.staged != nil && (p.RunID == "" || r.staged.plan.RunID == p.RunID) {
		st = r.staged
		r.staged = nil
	}
	var act *activeManeuver
	if r.active != nil && (p.RunID == "" || r.active.plan.RunID == p.RunID) {
		act = r.active
	}
	r.mu.Unlock()

	if st != nil && st.cancel != nil {
		st.cancel()
		r.clearState()
	}
	if act != nil {
		if act.cancel != nil {
			act.cancel()
		}
		<-act.done
		now := r.now().UTC()
		r.saveState(persistedManeuverState{
			Plan:      act.plan,
			Committed: true,
			State:     "stopped",
			Error:     p.Reason,
			UpdatedAt: now,
		})
		r.sendStatus(channel.ManeuverStatusPayload{
			RunID:      act.plan.RunID,
			ManeuverID: act.plan.ManeuverID,
			State:      "stopped",
			Error:      p.Reason,
			At:         now,
		})
	} else if persisted, ok := r.loadState(); ok && (p.RunID == "" || persisted.Plan.RunID == p.RunID) {
		persisted.State = "stopped"
		persisted.Error = p.Reason
		persisted.UpdatedAt = r.now().UTC()
		r.saveState(persisted)
	}
}

func (r *maneuverRunner) launchRunner(plan channel.ManeuverPreparePayload) {
	r.mu.Lock()
	ctx, cancel := context.WithCancel(r.baseCtx)
	act := &activeManeuver{
		plan:     plan,
		cancel:   cancel,
		done:     make(chan struct{}),
		sessions: map[string]struct{}{},
	}
	r.active = act
	r.mu.Unlock()

	go r.runActive(ctx, act)
}

func (r *maneuverRunner) runActive(ctx context.Context, act *activeManeuver) {
	defer func() {
		r.mu.Lock()
		if r.active == act {
			r.active = nil
		}
		r.mu.Unlock()
		close(act.done)
	}()

	plan := act.plan
	now := r.now().UTC()
	if now.Before(plan.StartAt) {
		r.sendStatus(channel.ManeuverStatusPayload{
			RunID:      plan.RunID,
			ManeuverID: plan.ManeuverID,
			State:      "scheduled",
			At:         now,
		})
		t := time.NewTimer(plan.StartAt.Sub(now))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		t.Stop()
	}

	now = r.now().UTC()
	endAt := plan.StartAt.Add(time.Duration(plan.TotalSec) * time.Second)
	if !now.Before(endAt) {
		r.saveState(persistedManeuverState{
			Plan:      plan,
			Committed: true,
			State:     "completed",
			UpdatedAt: now,
		})
		r.sendStatus(channel.ManeuverStatusPayload{
			RunID:      plan.RunID,
			ManeuverID: plan.ManeuverID,
			State:      "completed",
			ElapsedSec: float64(plan.TotalSec),
			At:         now,
		})
		return
	}

	r.saveState(persistedManeuverState{
		Plan:      plan,
		Committed: true,
		State:     "running",
		UpdatedAt: now,
	})
	r.sendStatus(channel.ManeuverStatusPayload{
		RunID:      plan.RunID,
		ManeuverID: plan.ManeuverID,
		State:      "running",
		ElapsedSec: now.Sub(plan.StartAt).Seconds(),
		At:         now,
	})

	var wg sync.WaitGroup
	for _, it := range plan.Items {
		itemStart := plan.StartAt.Add(time.Duration(it.StartSec) * time.Second)
		itemEnd := itemStart.Add(time.Duration(it.DurationSec) * time.Second)
		cur := r.now().UTC()
		if !cur.Before(itemEnd) {
			continue
		}
		replicas := it.Replicas
		if replicas < 1 {
			replicas = 1
		}
		for rep := 1; rep <= replicas; rep++ {
			wg.Add(1)
			go func(it channel.ManeuverPlanItem, rep int, itemStart, itemEnd time.Time) {
				defer wg.Done()
				r.runItemReplica(ctx, act, it, rep, itemStart, itemEnd)
			}(it, rep, itemStart, itemEnd)
		}
	}

	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	finNow := r.now().UTC()
	r.saveState(persistedManeuverState{
		Plan:      plan,
		Committed: true,
		State:     "completed",
		UpdatedAt: finNow,
	})
	r.sendStatus(channel.ManeuverStatusPayload{
		RunID:      plan.RunID,
		ManeuverID: plan.ManeuverID,
		State:      "completed",
		ElapsedSec: finNow.Sub(plan.StartAt).Seconds(),
		At:         finNow,
	})
}

func (r *maneuverRunner) runItemReplica(ctx context.Context, act *activeManeuver, it channel.ManeuverPlanItem, rep int, itemStart, itemEnd time.Time) {
	now := r.now().UTC()
	if now.Before(itemStart) {
		t := time.NewTimer(itemStart.Sub(now))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		t.Stop()
	}

	now = r.now().UTC()
	rem := itemEnd.Sub(now)
	if rem <= 0 {
		return
	}

	ex, confirmTok := r.executorForReplica(it.Command, it.ConfirmToken)
	if ex == nil {
		return
	}

	sessID := newReplicaSessionID()
	act.mu.Lock()
	act.sessions[sessID] = struct{}{}
	activeCount := len(act.sessions)
	sessionIDs := make([]string, 0, activeCount)
	for id := range act.sessions {
		sessionIDs = append(sessionIDs, id)
	}
	act.mu.Unlock()

	defer func() {
		act.mu.Lock()
		delete(act.sessions, sessID)
		act.mu.Unlock()
	}()

	r.sendStatus(channel.ManeuverStatusPayload{
		RunID:          act.plan.RunID,
		ManeuverID:     act.plan.ManeuverID,
		State:          "running",
		ActiveReplicas: activeCount,
		SessionIDs:     sessionIDs,
		ElapsedSec:     now.Sub(act.plan.StartAt).Seconds(),
		At:             now,
	})

	timeoutSec := int((rem + time.Second - 1) / time.Second)
	if timeoutSec < 1 {
		timeoutSec = 1
	}

	itemCtx, itemCancel := context.WithTimeout(ctx, rem)
	defer itemCancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		<-itemCtx.Done()
		r.terminateReplica(ex, sessID)
	}()

	ex.RunSync(exec.RunPayload{
		SessionID:    sessID,
		Command:      it.Command,
		User:         "maneuver",
		Mode:         exec.ModeReplica,
		TimeoutSec:   timeoutSec,
		DurationMS:   rem.Milliseconds(),
		ConfirmToken: confirmTok,
		ManeuverID:   act.plan.ManeuverID,
		RunID:        act.plan.RunID,
		ItemID:       it.ItemID,
		Replica:      rep,
	})
	itemCancel()
	<-done
}

// executorForReplica returns the worker's configured executor when available, or
// an ephemeral in-memory executor for pre-authorized persisted Maneuver plans
// resuming right after a reboot before the main server reconnects.
func (r *maneuverRunner) executorForReplica(cmd, token string) (*exec.Executor, string) {
	if ex := r.w.current(); ex != nil && ex.AuthorizeOneShot(cmd, token) == nil {
		return ex, token
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fallbackEx == nil {
		var secret [16]byte
		_, _ = rand.Read(secret[:])
		gate, err := exec.NewGate([]byte(hex.EncodeToString(secret[:])), nil)
		if err != nil {
			return nil, ""
		}
		// Ensure capture subsystem is initialized with local config if server hasn't pushed yet.
		r.w.mu.Lock()
		if !r.w.capTried {
			r.w.applyCaptureLocked(nil)
		}
		r.w.mu.Unlock()

		ex, err := exec.New(exec.Config{
			MachineID:      r.w.cfg.MachineID,
			Sender:         r.w.sender,
			Gate:           gate,
			CgroupRoot:     r.w.cfg.CgroupRoot,
			TermGrace:      r.w.cfg.TermGrace,
			Shell:          r.w.cfg.Shell,
			Logger:         r.w.log,
			OnSessionStart: r.w.sessionStart,
			OnSessionEnd:   r.w.sessionEnd,
		})
		if err != nil {
			return nil, ""
		}
		r.fallbackEx = ex
	}
	tok, _ := r.fallbackEx.MintForAuthorized(cmd)
	return r.fallbackEx, tok
}

func (r *maneuverRunner) terminateReplica(ex *exec.Executor, sessID string) {
	if ex == nil {
		return
	}
	if env, err := channel.NewEnvelope(channel.TypeTerminate, sessID, exec.TerminatePayload{SessionID: sessID}); err == nil {
		ex.Handle(context.Background(), env)
	}
}

func (r *maneuverRunner) waitExecutor(startAt time.Time) (*exec.Executor, error) {
	if ex := r.w.current(); ex != nil {
		return ex, nil
	}
	wait := r.w.cfg.ConfigWait
	if until := startAt.Sub(r.now().UTC()); until < wait {
		wait = until
	}
	if wait <= 0 {
		return nil, fmt.Errorf("agent executor not ready before start_at")
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-r.w.ready:
		if ex := r.w.current(); ex != nil {
			return ex, nil
		}
		return nil, fmt.Errorf("agent executor unavailable")
	case <-t.C:
		return nil, fmt.Errorf("timed out waiting for agent config/gate secret")
	case <-r.baseCtx.Done():
		return nil, r.baseCtx.Err()
	}
}

func (r *maneuverRunner) sendAck(ack channel.ManeuverAckPayload) {
	env, err := channel.NewEnvelope(channel.TypeManeuverAck, ack.RunID, ack)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.baseCtx, maneuverAckTimeout)
	defer cancel()
	if err := r.w.sender.Send(ctx, env); err != nil {
		r.w.log.Warn("send maneuver-ack", "component", "fleet-agent", "run_id", ack.RunID, "err", err)
	}
}

func (r *maneuverRunner) sendStatus(st channel.ManeuverStatusPayload) {
	r.mu.Lock()
	cp := st
	r.last = &cp
	r.mu.Unlock()

	env, err := channel.NewEnvelope(channel.TypeManeuverStatus, st.RunID, st)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.baseCtx, maneuverAckTimeout)
	defer cancel()
	if err := r.w.sender.Send(ctx, env); err != nil {
		r.w.log.Debug("send maneuver-status", "component", "fleet-agent", "run_id", st.RunID, "err", err)
	}
}

func validatePreparePayload(p channel.ManeuverPreparePayload) error {
	if strings.TrimSpace(p.RunID) == "" {
		return fmt.Errorf("missing run_id")
	}
	if p.TotalSec <= 0 {
		return fmt.Errorf("invalid total_sec %d", p.TotalSec)
	}
	if len(p.Items) == 0 {
		return fmt.Errorf("empty timeline items")
	}
	if p.Pattern != "" && p.Pattern != "all" {
		return fmt.Errorf("pattern %q requires the Load Controller (deferred)", p.Pattern)
	}
	for _, it := range p.Items {
		if strings.TrimSpace(it.Command) == "" {
			return fmt.Errorf("item %s has empty command", it.ItemID)
		}
		if it.StartSec < 0 || it.DurationSec <= 0 || it.StartSec+it.DurationSec > p.TotalSec {
			return fmt.Errorf("item %s window [%d,%d) outside total_sec %d", it.ItemID, it.StartSec, it.StartSec+it.DurationSec, p.TotalSec)
		}
	}
	return nil
}

func newReplicaSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "mr-" + hex.EncodeToString(b[:])
}
