package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
)

const maneuverAckTimeout = 10 * time.Second

type maneuverRunner struct {
	w   *worker
	now func() time.Time

	mu      sync.Mutex
	baseCtx context.Context
	staged  *stagedManeuver
	active  *activeManeuver
	last    *channel.ManeuverStatusPayload
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
	return &maneuverRunner{
		w:       w,
		now:     nowFn,
		baseCtx: w.base,
	}
}

func (r *maneuverRunner) start(ctx context.Context) {
	r.mu.Lock()
	r.baseCtx = ctx
	r.mu.Unlock()
}

func (r *maneuverRunner) onConnect() {
	// Wired in Part C for reconnect status reconciliation.
}

func (r *maneuverRunner) shutdown() {
	r.mu.Lock()
	st := r.staged
	r.staged = nil
	act := r.active
	r.active = nil
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

	// Ensure executor/gate is ready before ACKing.
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

	// Verify dangerous-command gate for every materialized command up front.
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

	// Re-check clock after waiting for config/gate.
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

	// Expire uncommitted staged plan if commit does not arrive before StartAt.
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
		r.sendAck(channel.ManeuverAckPayload{
			RunID:      p.RunID,
			Status:     channel.ManeuverAckMissed,
			Reason:     "received maneuver-commit at or after start_at",
			ReceivedAt: now,
			StartAt:    st.plan.StartAt,
		})
		return
	}

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
	}
	if act != nil {
		if act.cancel != nil {
			act.cancel()
		}
		<-act.done
		r.sendStatus(channel.ManeuverStatusPayload{
			RunID:      act.plan.RunID,
			ManeuverID: act.plan.ManeuverID,
			State:      "stopped",
			Error:      p.Reason,
			At:         r.now().UTC(),
		})
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
		r.sendStatus(channel.ManeuverStatusPayload{
			RunID:      plan.RunID,
			ManeuverID: plan.ManeuverID,
			State:      "completed",
			ElapsedSec: float64(plan.TotalSec),
			At:         now,
		})
		return
	}

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

	// Wait until all item windows finish (or ctx is cancelled).
	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	finNow := r.now().UTC()
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

	ex := r.w.current()
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
		Mode:         exec.ModeOneShot,
		TimeoutSec:   timeoutSec,
		ConfirmToken: it.ConfirmToken,
	})
	itemCancel()
	<-done
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
