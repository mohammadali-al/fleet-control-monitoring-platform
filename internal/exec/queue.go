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

// Queue is the main-server side for ONE machine: a strict FIFO. Manual
// commands (and PTY sessions) run strictly one at a time -- the next run
// envelope is not sent until the previous session's exit has arrived. (Maneuver
// replicas are separate and owned by the Load Controller, not this queue.)
//
// Wire it up as: q := NewQueue(machineID, dialer, onEvent); give q.Handle to
// the channel Dialer as its Handler; run q.Run(ctx) in a goroutine; call
// q.ConnectionLost() when the channel drops.
type Queue struct {
	machineID string
	tr        Sender
	onEvent   func(Event)

	mu       sync.Mutex
	pending  []*entry
	current  *entry
	finished []Entry // most recent first, capped
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
	ErrNotRunning     = errors.New("exec: session is not the running one")
)

const (
	maxFinished = 100
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

// Submit appends a command to the machine's FIFO and returns its session id.
// An empty req.SessionID is generated.
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
	q.pending = append(q.pending, e)
	snap := e.Entry
	q.mu.Unlock()

	q.emit(Event{Kind: EventQueued, SessionID: req.SessionID, Entry: snap})
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return req.SessionID, nil
}

// find returns the pending or current entry with id. q.mu must be held.
func (q *Queue) find(id string) *entry {
	if q.current != nil && q.current.SessionID == id {
		return q.current
	}
	for _, e := range q.pending {
		if e.SessionID == id {
			return e
		}
	}
	return nil
}

// Run dispatches queued commands one at a time until ctx is done.
func (q *Queue) Run(ctx context.Context) {
	for {
		e := q.next(ctx)
		if e == nil {
			return
		}
		q.execute(ctx, e)
	}
}

// next blocks for the head of the FIFO and makes it current.
func (q *Queue) next(ctx context.Context) *entry {
	for {
		q.mu.Lock()
		if len(q.pending) > 0 {
			e := q.pending[0]
			q.pending = q.pending[1:]
			e.State, e.Started = StateRunning, time.Now()
			q.current = e
			q.mu.Unlock()
			return e
		}
		q.mu.Unlock()
		select {
		case <-q.wake:
		case <-ctx.Done():
			return nil
		}
	}
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
	q.finished = append([]Entry{e.Entry}, q.finished...)
	if len(q.finished) > maxFinished {
		q.finished = q.finished[:maxFinished]
	}
	snap := e.Entry
	q.mu.Unlock()
	close(e.done)
	q.emit(Event{Kind: EventExited, SessionID: e.SessionID, Entry: snap, Exit: &x})
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
		e := q.current
		if e == nil || e.SessionID != p.SessionID {
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
		e := q.current
		ok := e != nil && e.SessionID == p.SessionID
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
		e := q.current
		ok := e != nil && e.SessionID == p.SessionID
		q.mu.Unlock()
		if ok {
			q.finish(e, p)
		}
	}
}

// ConnectionLost ends the running session as lost (the agent's exit can no
// longer arrive) so the FIFO moves on. Call it when the channel drops.
func (q *Queue) ConnectionLost() {
	q.mu.Lock()
	e := q.current
	q.mu.Unlock()
	if e != nil {
		q.finish(e, ExitPayload{SessionID: e.SessionID, ExitCode: -1, Reason: ReasonLost, Error: "channel lost"})
	}
}

// Cancel removes a queued command, or terminates the running one.
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
	running := q.current != nil && q.current.SessionID == id
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
	if q.current == nil || q.current.SessionID != id {
		return ErrNotRunning
	}
	return nil
}

// Snapshot lists the running entry, then the queued ones in order, then
// recently finished ones (most recent first).
func (q *Queue) Snapshot() []Entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Entry
	if q.current != nil {
		out = append(out, q.current.Entry)
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
