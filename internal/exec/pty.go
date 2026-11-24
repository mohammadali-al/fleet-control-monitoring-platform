package exec

import (
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
)

const (
	defaultCols = 80
	defaultRows = 24
)

// runPTY is Mode B: an interactive shell on a PTY. Output is streamed as
// "pty" chunks; input arrives through input envelopes (see input); resize and
// terminate work per session. The shell is in the session cgroup like any
// other command.
func (e *Executor) runPTY(a *active, p RunPayload) (int, error) {
	argv := strings.Fields(p.Command)
	if len(argv) == 0 {
		argv = []string{e.cfg.Shell}
	}
	cmd := osexec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.SysProcAttr = &syscall.SysProcAttr{} // StartWithSize adds Setsid+Setctty
	if a.cg != nil {
		a.cg.attach(cmd.SysProcAttr)
	}
	cols, rows := p.Cols, p.Rows
	if cols == 0 {
		cols = defaultCols
	}
	if rows == 0 {
		rows = defaultRows
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return exitStartFail, fmt.Errorf("start pty: %w", err)
	}
	a.fmu.Lock()
	a.filter = &lineFilter{gate: e.cfg.Gate, machineID: e.cfg.MachineID, allowOpaque: e.cfg.AllowOpaquePTY}
	a.ptmx = ptmx
	a.fmu.Unlock()
	a.pid = cmd.Process.Pid // Setsid: the shell leads its own process group
	e.announceRunning(a)

	var pumps sync.WaitGroup
	pumps.Add(1)
	go func() { defer pumps.Done(); e.pump(a, ptmx, StreamPTY) }()

	var timeout time.Duration // PTY sessions have no default timeout
	if p.TimeoutSec > 0 {
		timeout = time.Duration(p.TimeoutSec) * time.Second
	}
	code := e.wait(a, cmd, timeout, syscall.SIGHUP)

	e.drain(&pumps, ptmx)
	return code, nil
}

// input feeds typed bytes to a PTY session through the agent-side line gate.
func (e *Executor) input(p InputPayload) {
	a := e.get(p.SessionID)
	if a == nil || a.mode != ModePTY {
		return
	}
	a.fmu.Lock()
	if a.ptmx == nil {
		a.fmu.Unlock()
		return
	}
	fwd, refusals := a.filter.feed(p.Data, p.ConfirmToken)
	var err error
	if len(fwd) > 0 {
		_, err = a.ptmx.Write(fwd)
	}
	a.fmu.Unlock()
	if err != nil {
		e.log.Warn("pty write", "session_id", a.sess.ID, "err", err)
	}
	for _, r := range refusals {
		e.refuse(a.sess, r.Line, r.Reason)
		// Tell the terminal why the line vanished.
		msg := fmt.Sprintf("\r\n[fleet] refused: %s\r\n", r.Reason)
		cp := ChunkPayload{SessionID: a.sess.ID, Stream: StreamPTY, Data: []byte(msg)}
		a.mu.Lock()
		a.seq++
		cp.Seq = a.seq
		a.mu.Unlock()
		_ = send(e.base, e.cfg.Sender, channel.TypeChunk, a.sess.ID, cp)
	}
}

// resize applies a window size to a PTY session.
func (e *Executor) resize(p ResizePayload) {
	a := e.get(p.SessionID)
	if a == nil || a.mode != ModePTY || p.Cols == 0 || p.Rows == 0 {
		return
	}
	a.fmu.Lock()
	defer a.fmu.Unlock()
	if a.ptmx == nil {
		return
	}
	if err := pty.Setsize(a.ptmx, &pty.Winsize{Rows: p.Rows, Cols: p.Cols}); err != nil {
		e.log.Warn("pty resize", "session_id", a.sess.ID, "err", err)
	}
}
