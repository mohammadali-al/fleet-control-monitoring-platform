// Package exec is the command-execution layer: the worker-agent Executor (Mode
// A one-shot commands and Mode B PTY shells, each in a tracked per-session
// cgroup, behind the dangerous-command gate) and the main-server Queue (a
// per-machine strict FIFO). They talk over the channel (internal/channel) with
// the envelope types run, state, chunk, input, exit, resize, terminate and
// confirm; payload shapes are in messages.go.
package exec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	osexec "os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
)

// Defaults.
const (
	DefaultTimeout = 300 * time.Second // per one-shot command
	DefaultGrace   = 3 * time.Second   // SIGTERM -> SIGKILL on terminate/timeout
	chunkSize      = 16 << 10
	drainWait      = 2 * time.Second // wait for output to drain after exit
	exitRefused    = 126             // exit code reported for a refused command
	exitStartFail  = 127
)

// Config configures an Executor.
type Config struct {
	// MachineID is this agent's id; confirmation tokens are bound to it.
	MachineID string
	// Host is the host name recorded in each session (default os.Hostname).
	Host string
	// Sender writes envelopes to the main server (a *channel.Listener).
	Sender Sender
	// Gate is the dangerous-command gate. Required: the agent enforces it.
	Gate *Gate
	// CgroupRoot is the cgroup v2 directory under which each session gets its
	// own cgroup (e.g. /sys/fs/cgroup/fleet). Empty disables cgroups (dev and
	// unit tests without privileges); when set, failing to create the cgroup
	// fails the command rather than running it untracked.
	CgroupRoot string
	// KeepCgroup leaves the session cgroup in place after the command ends
	// (default: remove it).
	KeepCgroup bool
	// DefaultTimeout applies to one-shot commands with no timeout (default 300 s).
	DefaultTimeout time.Duration
	// TermGrace is the SIGTERM -> SIGKILL delay (default 3 s).
	TermGrace time.Duration
	// Shell runs one-shot commands as `Shell -c command` and is the default
	// PTY program (default /bin/sh).
	Shell string
	// AllowOpaquePTY relaxes PTY line gating for lines edited with keys the
	// agent cannot track. See lineFilter.
	AllowOpaquePTY bool
	// Logger receives refusals and errors (default slog.Default()).
	Logger *slog.Logger
	// OnSessionStart is called once a session's cgroup exists and the command
	// has started (evidence capture attaches here). OnSessionEnd after it ends.
	OnSessionStart func(*Session)
	OnSessionEnd   func(SessionInfo)
}

// Executor is the agent side. Use Handle as the channel handler.
type Executor struct {
	cfg  Config
	log  *slog.Logger
	base context.Context
	stop context.CancelFunc

	mu       sync.Mutex
	sessions map[string]*active
	wg       sync.WaitGroup
}

// active is a running session plus what is needed to control it.
type active struct {
	sess *Session
	cg   *Cgroup
	pid  int
	mode string

	ptmx   *os.File
	fmu    sync.Mutex // guards filter and serialises PTY writes
	filter *lineFilter

	stopOnce sync.Once
	stopCh   chan struct{}
	mu       sync.Mutex
	killedBy string
	seq      uint64
}

// New validates cfg and returns an Executor.
func New(cfg Config) (*Executor, error) {
	if cfg.Sender == nil {
		return nil, errors.New("exec: executor needs a Sender")
	}
	if cfg.Gate == nil {
		return nil, errors.New("exec: executor needs a Gate")
	}
	if cfg.MachineID == "" {
		return nil, errors.New("exec: executor needs a machine id")
	}
	if cfg.Host == "" {
		cfg.Host, _ = os.Hostname()
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = DefaultTimeout
	}
	if cfg.TermGrace <= 0 {
		cfg.TermGrace = DefaultGrace
	}
	if cfg.Shell == "" {
		cfg.Shell = "/bin/sh"
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	base, stop := context.WithCancel(context.Background())
	return &Executor{cfg: cfg, log: log, base: base, stop: stop, sessions: map[string]*active{}}, nil
}

// Shutdown terminates every session and waits for them to finish.
func (e *Executor) Shutdown() {
	e.mu.Lock()
	for _, a := range e.sessions {
		a.requestStop(ReasonTerminated)
	}
	e.mu.Unlock()
	e.wg.Wait()
	e.stop()
}

// Session returns a snapshot of a running session.
func (e *Executor) Session(id string) (SessionInfo, bool) {
	e.mu.Lock()
	a := e.sessions[id]
	e.mu.Unlock()
	if a == nil {
		return SessionInfo{}, false
	}
	return a.sess.Info(), true
}

// Handle is the channel.Handler for the agent: it dispatches envelopes from
// the main server. It never blocks on a running command.
func (e *Executor) Handle(_ context.Context, env channel.Envelope) {
	switch env.Type {
	case channel.TypeRun:
		var p RunPayload
		if err := decode(env, &p); err != nil {
			e.log.Warn("bad run", "err", err)
			return
		}
		if p.SessionID == "" {
			p.SessionID = env.ID
		}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.run(p)
		}()
	case channel.TypeInput:
		var p InputPayload
		if decode(env, &p) == nil {
			e.input(p)
		}
	case channel.TypeResize:
		var p ResizePayload
		if decode(env, &p) == nil {
			e.resize(p)
		}
	case channel.TypeTerminate:
		var p TerminatePayload
		if decode(env, &p) == nil {
			if p.SessionID == "" {
				p.SessionID = env.ID
			}
			if a := e.get(p.SessionID); a != nil {
				a.requestStop(ReasonTerminated)
			}
		}
	case channel.TypeConfirm:
		var p ConfirmPayload
		if decode(env, &p) == nil {
			if a := e.get(p.SessionID); a != nil {
				a.fmu.Lock()
				if a.filter != nil {
					a.filter.grant(p.Token)
				}
				a.fmu.Unlock()
			}
		}
	}
}

func (e *Executor) get(id string) *active {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessions[id]
}

// run executes one run request to completion and reports the exit.
func (e *Executor) run(p RunPayload) {
	if p.Mode == "" {
		p.Mode = ModeOneShot
	}
	if !ValidSessionID(p.SessionID) {
		e.log.Warn("run with invalid session id", "session_id", p.SessionID)
		e.sendExit(p.SessionID, ExitPayload{SessionID: p.SessionID, ExitCode: exitStartFail, Reason: ReasonError, Error: "invalid session id"})
		return
	}
	if p.Mode != ModeOneShot && p.Mode != ModePTY {
		e.sendExit(p.SessionID, ExitPayload{SessionID: p.SessionID, ExitCode: exitStartFail, Reason: ReasonError, Error: "unknown mode " + p.Mode})
		return
	}

	sess := &Session{ID: p.SessionID, Command: p.Command, User: p.User, Host: e.cfg.Host, Mode: p.Mode}
	a := &active{sess: sess, mode: p.Mode, stopCh: make(chan struct{})}
	e.mu.Lock()
	if _, dup := e.sessions[p.SessionID]; dup {
		e.mu.Unlock()
		e.log.Warn("duplicate session id", "session_id", p.SessionID)
		return // do not disturb the original session's stream
	}
	e.sessions[p.SessionID] = a
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.sessions, p.SessionID)
		e.mu.Unlock()
	}()

	// The gate runs here, in the agent, before anything executes. A PTY
	// session's program is the shell; its input lines are gated as typed.
	if p.Mode == ModeOneShot {
		if err := e.cfg.Gate.Authorize(e.cfg.MachineID, p.Command, p.ConfirmToken); err != nil {
			e.refuse(sess, p.Command, err.Error())
			return
		}
	}

	if e.cfg.CgroupRoot != "" {
		cg, err := newCgroup(e.cfg.CgroupRoot, p.SessionID)
		if err != nil {
			e.log.Error("cgroup", "session_id", p.SessionID, "err", err)
			e.sendExit(p.SessionID, ExitPayload{SessionID: p.SessionID, ExitCode: exitStartFail, Reason: ReasonError, Error: err.Error()})
			return
		}
		a.cg, sess.cgroup = cg, cg
	}

	var code int
	var runErr error
	sess.markStarted()
	if p.Mode == ModePTY {
		code, runErr = e.runPTY(a, p)
	} else {
		code, runErr = e.runOneShot(a, p)
	}
	d := sess.markFinished(code)

	reason := ReasonExited
	if k := a.killer(); k != "" {
		reason = k
	}
	x := ExitPayload{SessionID: p.SessionID, ExitCode: code, DurationMS: d.Milliseconds(), Reason: reason}
	if runErr != nil {
		x.Reason, x.Error = ReasonError, runErr.Error()
	}

	if a.cg != nil && !e.cfg.KeepCgroup {
		_ = a.cg.Remove()
	}
	// Exit goes after every chunk: the pumps have drained before we get here.
	e.sendExit(p.SessionID, x)
	if e.cfg.OnSessionEnd != nil {
		e.cfg.OnSessionEnd(sess.Info())
	}
}

// refuse logs a refused command and tells the server.
func (e *Executor) refuse(sess *Session, line, reason string) {
	e.log.Warn("dangerous command refused",
		"machine", e.cfg.MachineID, "session_id", sess.ID, "user", sess.User,
		"mode", sess.Mode, "command", line, "reason", reason)
	_ = send(e.base, e.cfg.Sender, channel.TypeState, sess.ID,
		StatePayload{SessionID: sess.ID, State: StateRefused, Detail: reason})
	if sess.Mode == ModeOneShot {
		e.sendExit(sess.ID, ExitPayload{SessionID: sess.ID, ExitCode: exitRefused, Reason: ReasonRefused, Error: reason})
	}
}

func (e *Executor) sendExit(id string, x ExitPayload) {
	if err := send(e.base, e.cfg.Sender, channel.TypeExit, id, x); err != nil {
		e.log.Warn("send exit", "session_id", id, "err", err)
	}
}

func (e *Executor) sendState(a *active, st StatePayload) {
	if err := send(e.base, e.cfg.Sender, channel.TypeState, a.sess.ID, st); err != nil {
		e.log.Warn("send state", "session_id", a.sess.ID, "err", err)
	}
}

// announceRunning reports the running state and notifies the capture hook.
func (e *Executor) announceRunning(a *active) {
	e.sendState(a, StatePayload{SessionID: a.sess.ID, State: StateRunning, CgroupPath: a.sess.CgroupPath()})
	if e.cfg.OnSessionStart != nil {
		e.cfg.OnSessionStart(a.sess)
	}
}

// pump copies r to the channel as chunks until EOF or error.
func (e *Executor) pump(a *active, r *os.File, stream string) {
	buf := make([]byte, chunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			a.mu.Lock()
			a.seq++
			seq := a.seq
			a.mu.Unlock()
			cp := ChunkPayload{SessionID: a.sess.ID, Stream: stream, Seq: seq, Data: data}
			if serr := send(e.base, e.cfg.Sender, channel.TypeChunk, a.sess.ID, cp); serr != nil {
				e.log.Warn("send chunk", "session_id", a.sess.ID, "err", serr)
			}
		}
		if err != nil {
			return
		}
	}
}

// runOneShot is Mode A.
func (e *Executor) runOneShot(a *active, p RunPayload) (int, error) {
	timeout := time.Duration(p.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = e.cfg.DefaultTimeout
	}
	cmd := osexec.Command(e.cfg.Shell, "-c", p.Command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if a.cg != nil {
		a.cg.attach(cmd.SysProcAttr)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return exitStartFail, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return exitStartFail, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		outR.Close()
		outW.Close()
		errR.Close()
		errW.Close()
		return exitStartFail, fmt.Errorf("start: %w", err)
	}
	outW.Close()
	errW.Close()
	a.pid = cmd.Process.Pid
	e.announceRunning(a)

	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() { defer pumps.Done(); e.pump(a, outR, StreamStdout) }()
	go func() { defer pumps.Done(); e.pump(a, errR, StreamStderr) }()

	code := e.wait(a, cmd, timeout, syscall.SIGTERM)

	e.drain(&pumps, outR, errR)
	return code, nil
}

// wait supervises cmd until it exits: it enforces the timeout and terminate
// (termSig, then SIGKILL of the whole tree after the grace period), and on
// exit kills any stragglers so the session ends with its command.
func (e *Executor) wait(a *active, cmd *osexec.Cmd, timeout time.Duration, termSig syscall.Signal) int {
	done := make(chan struct{})
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	go func() {
		select {
		case <-done:
			return
		case <-timer:
			a.setKilled(ReasonTimeout)
		case <-a.stopCh:
			a.setKilled(ReasonTerminated)
		}
		a.signal(termSig)
		select {
		case <-done:
		case <-time.After(e.cfg.TermGrace):
			a.killTree()
		}
	}()
	_ = cmd.Wait()
	close(done)
	a.killTree() // leave nothing running in the session
	return exitCode(cmd.ProcessState)
}

// drain waits briefly for the output pumps to hit EOF, then force-closes the
// read ends so a stray holder of the pipe cannot stall the session.
func (e *Executor) drain(pumps *sync.WaitGroup, files ...*os.File) {
	ch := make(chan struct{})
	go func() { pumps.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(drainWait):
	}
	for _, f := range files {
		f.Close()
	}
	<-ch
}

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return exitStartFail
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// requestStop asks the session to end (terminate).
func (a *active) requestStop(reason string) {
	a.stopOnce.Do(func() {
		a.setKilled(reason)
		close(a.stopCh)
	})
}

func (a *active) setKilled(reason string) {
	a.mu.Lock()
	if a.killedBy == "" {
		a.killedBy = reason
	}
	a.mu.Unlock()
}

func (a *active) killer() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.killedBy
}

// signal sends sig to the session's process group (the command and its
// children).
func (a *active) signal(sig syscall.Signal) {
	if a.pid > 0 {
		_ = syscall.Kill(-a.pid, sig)
	}
}

// killTree SIGKILLs everything in the session: the whole cgroup if there is
// one (it also catches processes that left the process group), else the
// process group.
func (a *active) killTree() {
	if a.cg != nil {
		_ = a.cg.Kill()
		return
	}
	a.signal(syscall.SIGKILL)
}
