package exec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
)

// Queue is the main-server side for ONE machine. One-shot commands (Mode A) run
// through a strict FIFO: strictly one at a time -- the next run envelope is not
// sent until the previous one-shot's exit has arrived. (Maneuver replicas are
// separate and owned by the Load Controller, not this queue.)
//
// Interactive shells (Mode B, PTY) are NOT part of that FIFO. A shell is its own
// concurrent, long-lived session: it starts as soon as it is submitted, never
// waits for a one-shot, and never makes a one-shot wait -- an open shell does not
// occupy the FIFO (architecture 4.6). Each shell keeps its own session id and
// cgroup on the agent, so concurrent sessions do not interfere.
//
// Multi-shell policy: several shells may be open at once on one machine (each an
// independent session), capped at MaxShells so a runaway client cannot pile up
// root PTYs. Submitting a shell beyond the cap fails with ErrTooManyShells.
//
// Wire it up as: q := NewQueue(machineID, dialer, onEvent); give q.Handle to
// the channel Dialer as its Handler; run q.Run(ctx) in a goroutine; call
// q.ConnectionLost() when the channel drops.
type Queue struct {
	machineID string
	tr        Sender
	onEvent   func(Event)

	mu       sync.Mutex
	pending  []*entry // one-shot FIFO, waiting
	current  *entry   // the running one-shot, if any
	busy     bool     // a one-shot is in flight (set with current, cleared when its goroutine ends)
	shells   []*entry // open shells (queued for an instant, then running), start order
	finished []Entry  // most recent first, capped
	wake     chan struct{}
}

type entry struct {
	Entry
	req  RunPayload
	done chan struct{}
}

// Entry is the observable state of a submitted command.
type Entry struct {
	SessionID  string
	Command    string
	User       string
	Mode       string
	State      string // queued | running | finished
	CgroupPath string
	Exit       *ExitPayload // set when finished
	Queued     time.Time
	Started    time.Time
	Finished   time.Time
}

// Event kinds surfaced by the queue.
const (
	EventQueued  = "queued"
	EventRunning = "running"
	EventRefused = "refused"
	EventChunk   = "chunk"
	EventExited  = "exited"
)

// Event is what the queue reports to its subscriber (the panel, later).
type Event struct {
	Kind      string
	SessionID string
	Entry     Entry
	State     *StatePayload
	Chunk     *ChunkPayload
	Exit      *ExitPayload
}

// Errors returned by the queue.
var (
	ErrUnknownSession = errors.New("exec: unknown session")
	ErrNotRunning     = errors.New("exec: session is not running")
	ErrTooManyShells  = errors.New("exec: too many open shells on this machine")
)

const (
	maxFinished = 100
	// MaxShells is the most interactive shells open at once on one machine.
	MaxShells = 8
	// slack added to a command's timeout before the queue gives up waiting for
	// the agent's exit message.
	exitSlack = 30 * time.Second
)

// NewQueue returns a queue for machineID that sends through tr. onEvent may be
// nil; it is called synchronously and must not block for long.
func NewQueue(machineID string, tr Sender, onEvent func(Event)) *Queue {
	return &Queue{machineID: machineID, tr: tr, onEvent: onEvent, wake: make(chan struct{}, 1)}
}

// NewSessionID returns a fresh random session id.
func NewSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "s-" + hex.EncodeToString(b[:])
}

// Submit queues a one-shot command on the machine's FIFO, or -- for a PTY shell
// -- registers it to start immediately outside the FIFO, and returns its session
// id. An empty req.SessionID is generated.
func (q *Queue) Submit(req RunPayload) (string, error) {
	if req.SessionID == "" {
		req.SessionID = NewSessionID()
	}
	if !ValidSessionID(req.SessionID) {
		return "", fmt.Errorf("exec: invalid session id %q", req.SessionID)
	}
	if req.Mode == "" {
		req.Mode = ModeOneShot
	}
	if req.Mode != ModeOneShot && req.Mode != ModePTY {
		return "", fmt.Errorf("exec: unknown mode %q", req.Mode)
	}
	e := &entry{
		Entry: Entry{SessionID: req.SessionID, Command: req.Command, User: req.User, Mode: req.Mode,
			State: StateQueued, Queued: time.Now()},
		req:  req,
		done: make(chan struct{}),
	}
	q.mu.Lock()
	if q.find(req.SessionID) != nil {
		q.mu.Unlock()
		return "", fmt.Errorf("exec: session id %q already in use", req.SessionID)
	}
	if req.Mode == ModePTY {
		if len(q.shells) >= MaxShells {
			q.mu.Unlock()
			return "", ErrTooManyShells
		}
		q.shells = append(q.shells, e) // started by Run right away, not queued behind one-shots
	} else {
		q.pending = append(q.pending, e)
	}
	snap := e.Entry
	q.mu.Unlock()

	q.emit(Event{Kind: EventQueued, SessionID: req.SessionID, Entry: snap})
	q.kick()
	return req.SessionID, nil
}

// kick wakes the Run loop.
func (q *Queue) kick() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// find returns the pending, current or shell entry with id. q.mu must be held.
func (q *Queue) find(id string) *entry {
	if q.current != nil && q.current.SessionID == id {
		return q.current
	}
	for _, e := range q.pending {
		if e.SessionID == id {
			return e
		}
	}
	return q.shell(id)
}

// shell returns the open shell with id, or nil. q.mu must be held.
func (q *Queue) shell(id string) *entry {
	for _, e := range q.shells {
		if e.SessionID == id {
			return e
		}
	}
	return nil
}

// active returns the running one-shot or open shell with id (what the agent may
// send messages for), or nil. q.mu must be held.
func (q *Queue) active(id string) *entry {
	if q.current != nil && q.current.SessionID == id {
		return q.current
	}
	return q.shell(id)
}

// Run dispatches until ctx is done: open shells start immediately and
// concurrently; one-shots go one at a time from the FIFO, whether or not a shell
// is open. It returns once every session it started has been closed out.
func (q *Queue) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		for _, e := range q.dispatch() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				q.execute(ctx, e)
				if e.Mode != ModePTY {
					q.mu.Lock()
					q.busy = false
					q.mu.Unlock()
					q.kick() // the FIFO may advance
				}
			}()
		}
		select {
		case <-q.wake:
		case <-ctx.Done():
			return
		}
	}
}

// dispatch marks what may start now as running and returns it: every shell not
// yet started, plus the head of the FIFO if no one-shot is in flight.
func (q *Queue) dispatch() []*entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []*entry
	now := time.Now()
	for _, s := range q.shells {
		if s.State == StateQueued {
			s.State, s.Started = StateRunning, now
			out = append(out, s)
		}
	}
	if !q.busy && len(q.pending) > 0 {
		e := q.pending[0]
		q.pending = q.pending[1:]
		e.State, e.Started = StateRunning, now
		q.current, q.busy = e, true
		out = append(out, e)
	}
	return out
}

func (q *Queue) execute(ctx context.Context, e *entry) {
	if err := send(ctx, q.tr, channel.TypeRun, e.SessionID, e.req); err != nil {
		q.finish(e, ExitPayload{SessionID: e.SessionID, ExitCode: -1, Reason: ReasonError, Error: err.Error()})
		return
	}
	var deadline <-chan time.Time
	if e.req.Mode == ModeOneShot || e.req.TimeoutSec > 0 {
		t := time.Duration(e.req.TimeoutSec) * time.Second
		if t <= 0 {
			t = DefaultTimeout
		}
		timer := time.NewTimer(t + exitSlack)
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case <-e.done:
	case <-deadline:
		q.finish(e, ExitPayload{SessionID: e.SessionID, ExitCode: -1, Reason: ReasonLost, Error: "no exit from agent"})
	case <-ctx.Done():
		q.finish(e, ExitPayload{SessionID: e.SessionID, ExitCode: -1, Reason: ReasonCancelled, Error: "queue stopped"})
	}
}

// finish records the end of e (idempotent) and emits the exited event.
func (q *Queue) finish(e *entry, x ExitPayload) {
	q.mu.Lock()
	if e.State == StateFinished {
		q.mu.Unlock()
		return
	}
	e.State, e.Finished, e.Exit = StateFinished, time.Now(), &x
	if q.current == e {
		q.current = nil
	}
	for i, s := range q.shells {
		if s == e {
			q.shells = append(q.shells[:i], q.shells[i+1:]...)
			break
		}
	}
	q.finished = append([]Entry{e.Entry}, q.finished...)
	if len(q.finished) > maxFinished {
		q.finished = q.finished[:maxFinished]
	}
	snap := e.Entry
	q.mu.Unlock()
	// Emit before releasing execute: the next one-shot cannot be dispatched until
	// this one's exit has been reported.
	q.emit(Event{Kind: EventExited, SessionID: e.SessionID, Entry: snap, Exit: &x})
	close(e.done)
}

// StateFinished is the Entry.State of a command that has ended.
const StateFinished = "finished"

// Handle is the channel.Handler for the main-server side: it takes the agent's
// state, chunk and exit envelopes. Messages for unknown or finished sessions
// are dropped.
func (q *Queue) Handle(_ context.Context, env channel.Envelope) {
	switch env.Type {
	case channel.TypeState:
		var p StatePayload
		if decode(env, &p) != nil {
			return
		}
		q.mu.Lock()
		e := q.active(p.SessionID)
		if e == nil {
			q.mu.Unlock()
			return
		}
		if p.CgroupPath != "" {
			e.CgroupPath = p.CgroupPath
		}
		snap := e.Entry
		q.mu.Unlock()
		kind := EventRunning
		if p.State == StateRefused {
			kind = EventRefused
		}
		q.emit(Event{Kind: kind, SessionID: p.SessionID, Entry: snap, State: &p})
	case channel.TypeChunk:
		var p ChunkPayload
		if decode(env, &p) != nil {
			return
		}
		q.mu.Lock()
		e := q.active(p.SessionID)
		ok := e != nil
		var snap Entry
		if ok {
			snap = e.Entry
		}
		q.mu.Unlock()
		if ok {
			q.emit(Event{Kind: EventChunk, SessionID: p.SessionID, Entry: snap, Chunk: &p})
		}
	case channel.TypeExit:
		var p ExitPayload
		if decode(env, &p) != nil {
			return
		}
		q.mu.Lock()
		e := q.active(p.SessionID)
		ok := e != nil
		q.mu.Unlock()
		if ok {
			q.finish(e, p)
		}
	}
}

// ConnectionLost ends every running session -- the one-shot and any open
// shells -- as lost (the agent's exit can no longer arrive) so the FIFO moves
// on. Call it when the channel drops.
func (q *Queue) ConnectionLost() {
	q.mu.Lock()
	var lost []*entry
	if q.current != nil {
		lost = append(lost, q.current)
	}
	for _, s := range q.shells {
		if s.State == StateRunning {
			lost = append(lost, s)
		}
	}
	q.mu.Unlock()
	for _, e := range lost {
		q.finish(e, ExitPayload{SessionID: e.SessionID, ExitCode: -1, Reason: ReasonLost, Error: "channel lost"})
	}
}

// Cancel removes a queued command, or terminates the running one (a running
// one-shot or an open shell).
func (q *Queue) Cancel(ctx context.Context, id string) error {
	q.mu.Lock()
	for i, e := range q.pending {
		if e.SessionID == id {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			q.mu.Unlock()
			q.finish(e, ExitPayload{SessionID: id, ExitCode: -1, Reason: ReasonCancelled})
			return nil
		}
	}
	if s := q.shell(id); s != nil && s.State == StateQueued { // not yet started
		q.mu.Unlock()
		q.finish(s, ExitPayload{SessionID: id, ExitCode: -1, Reason: ReasonCancelled})
		return nil
	}
	running := q.active(id) != nil
	q.mu.Unlock()
	if !running {
		return ErrUnknownSession
	}
	return q.Terminate(ctx, id)
}

// Terminate asks the agent to end the running session.
func (q *Queue) Terminate(ctx context.Context, id string) error {
	if err := q.requireRunning(id); err != nil {
		return err
	}
	return send(ctx, q.tr, channel.TypeTerminate, id, TerminatePayload{SessionID: id})
}

// Input sends typed bytes to the running PTY session. token (optional)
// authorises dangerous lines completed in data.
func (q *Queue) Input(ctx context.Context, id string, data []byte, token string) error {
	if err := q.requireRunning(id); err != nil {
		return err
	}
	return send(ctx, q.tr, channel.TypeInput, id, InputPayload{SessionID: id, Data: data, ConfirmToken: token})
}

// Resize sets the window size of the running PTY session.
func (q *Queue) Resize(ctx context.Context, id string, cols, rows uint16) error {
	if err := q.requireRunning(id); err != nil {
		return err
	}
	return send(ctx, q.tr, channel.TypeResize, id, ResizePayload{SessionID: id, Cols: cols, Rows: rows})
}

// Confirm sends a confirmation token (from Gate.Mint) for the running PTY
// session.
func (q *Queue) Confirm(ctx context.Context, id, token string) error {
	if err := q.requireRunning(id); err != nil {
		return err
	}
	return send(ctx, q.tr, channel.TypeConfirm, id, ConfirmPayload{SessionID: id, Token: token})
}

func (q *Queue) requireRunning(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active(id) == nil {
		return ErrNotRunning
	}
	return nil
}

// Snapshot lists the running one-shot, then the open shells (in start order),
// then the queued one-shots in order, then recently finished ones (most recent
// first).
func (q *Queue) Snapshot() []Entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Entry
	if q.current != nil {
		out = append(out, q.current.Entry)
	}
	for _, e := range q.shells {
		out = append(out, e.Entry)
	}
	for _, e := range q.pending {
		out = append(out, e.Entry)
	}
	return append(out, q.finished...)
}

func (q *Queue) emit(ev Event) {
	if q.onEvent != nil {
		q.onEvent(ev)
	}
}
