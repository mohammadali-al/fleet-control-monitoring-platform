package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
)

// WebSocket endpoints.
//
//	/ws/events   machine state changes, one-shot command lifecycle and output
//	             (JSON text frames, see eventMsg), for every page.
//	/ws/shell    one interactive shell on a worker agent: the browser's xterm.js
//	             on one side, the agent's PTY on the other.
//
// The browser speaks to the main server only; the agent link stays behind the
// control channel. Origin is checked by the websocket library (same host).
func (s *server) routesWS(mux *http.ServeMux) {
	mux.HandleFunc("GET /ws/events", s.wsEvents)
	mux.HandleFunc("GET /ws/shell", s.wsShell)
}

const (
	wsWriteTimeout = 10 * time.Second
	wsReadLimit    = 64 << 10
)

// eventMsg is one frame on /ws/events.
//
//	{"type":"machine","machine":{...}}
//	{"type":"command","event":"queued|running|refused|exited", ...}
//	{"type":"chunk","session":..,"machine":..,"stream":"stdout|stderr","data":<base64>}
type eventMsg struct {
	Type    string             `json:"type"`
	Machine *fleet.MachineInfo `json:"machine,omitempty"`

	Event   string            `json:"event,omitempty"`
	Session string            `json:"session,omitempty"`
	On      string            `json:"on,omitempty"` // machine id of a command
	Command string            `json:"command,omitempty"`
	Mode    string            `json:"mode,omitempty"`
	User    string            `json:"user,omitempty"`
	State   string            `json:"state,omitempty"`
	Detail  string            `json:"detail,omitempty"`
	Exit    *exec.ExitPayload `json:"exit,omitempty"`
	Queued  *time.Time        `json:"queued_at,omitempty"`
	Started *time.Time        `json:"started_at,omitempty"`

	Stream string `json:"stream,omitempty"`
	Data   []byte `json:"data,omitempty"`
}

func toEventMsg(ev fleet.Event) (eventMsg, bool) {
	switch ev.Kind {
	case fleet.EventMachine:
		return eventMsg{Type: "machine", Machine: ev.Info}, ev.Info != nil
	case fleet.EventExec:
		x := ev.Exec
		if x == nil {
			return eventMsg{}, false
		}
		if x.Kind == exec.EventChunk {
			// Shell output goes only to that shell's own socket.
			if x.Chunk == nil || x.Entry.Mode == exec.ModePTY {
				return eventMsg{}, false
			}
			return eventMsg{Type: "chunk", Session: x.SessionID, On: ev.Machine, Stream: x.Chunk.Stream, Data: x.Chunk.Data}, true
		}
		m := eventMsg{
			Type: "command", Event: x.Kind, Session: x.SessionID, On: ev.Machine,
			Command: x.Entry.Command, Mode: x.Entry.Mode, User: x.Entry.User, State: x.Entry.State, Exit: x.Exit,
		}
		if !x.Entry.Queued.IsZero() {
			t := x.Entry.Queued
			m.Queued = &t
		}
		if !x.Entry.Started.IsZero() {
			t := x.Entry.Started
			m.Started = &t
		}
		if x.State != nil {
			m.Detail = x.State.Detail
		}
		return m, true
	}
	return eventMsg{}, false
}

func (s *server) wsEvents(w http.ResponseWriter, r *http.Request) {
	if s.be == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("the panel has no backend configured"))
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has already written the reply
	}
	defer c.CloseNow()
	c.SetReadLimit(wsReadLimit)
	ctx := c.CloseRead(r.Context())

	sub := s.be.Bus().Subscribe(nil)
	defer sub.Close()
	if err := writeJSONFrame(ctx, c, map[string]string{"type": "hello"}); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				_ = c.Close(websocket.StatusTryAgainLater, "lagging: reconnect and reload")
				return
			}
			msg, send := toEventMsg(ev)
			if !send {
				continue
			}
			if err := writeJSONFrame(ctx, c, msg); err != nil {
				return
			}
		}
	}
}

func writeJSONFrame(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, b)
}

// ---- interactive shell ----

// Frames on /ws/shell.
//
//	browser -> server  binary: keystrokes (sent to the PTY)
//	                   text:   {"type":"resize","cols":N,"rows":N}
//	                           {"type":"confirm","line":"..."}  (operator confirmed a refused line)
//	server -> browser  binary: terminal output
//	                   text:   {"type":"session","session":"s-..","machine":".."}
//	                           {"type":"state","state":"queued|running"}
//	                           {"type":"refused","line":"..","reason":".."}
//	                           {"type":"exit","code":N,"reason":".."}
//	                           {"type":"error","error":".."}
type shellCtl struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Line string `json:"line,omitempty"`
}

type shellOut struct {
	Type    string `json:"type"`
	Session string `json:"session,omitempty"`
	Machine string `json:"machine,omitempty"`
	State   string `json:"state,omitempty"`
	Line    string `json:"line,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Code    *int   `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (s *server) wsShell(w http.ResponseWriter, r *http.Request) {
	if s.be == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("the panel has no backend configured"))
		return
	}
	q := r.URL.Query()
	machine := q.Get("machine")
	cols, rows := dim(q.Get("cols")), dim(q.Get("rows"))
	user := operatorFrom(r)

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(wsReadLimit)
	ctx, stop := context.WithCancel(r.Context())
	defer stop()

	// Subscribe before queueing so the queued/running events cannot be missed.
	session := exec.NewSessionID()
	sub := s.be.Bus().Subscribe(func(ev fleet.Event) bool {
		return ev.Kind == fleet.EventExec && ev.Exec != nil && ev.Exec.SessionID == session
	})
	defer sub.Close()

	if _, err := s.be.OpenShell(fleet.ShellRequest{Machine: machine, User: user, Session: session, Cols: cols, Rows: rows}); err != nil {
		_ = writeJSONFrame(ctx, c, shellOut{Type: "error", Error: err.Error()})
		_ = c.Close(websocket.StatusNormalClosure, "not opened")
		return
	}
	s.log.Info("shell opened", "component", "panel", "operator", user, "machine", machine, "session", session)
	// However this handler ends, the shell ends with it.
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.be.Cancel(cctx, session) // unknown session = already over
		s.log.Info("shell closed", "component", "panel", "machine", machine, "session", session)
	}()

	if err := writeJSONFrame(ctx, c, shellOut{Type: "session", Session: session, Machine: machine}); err != nil {
		return
	}

	lt := &lineTracker{}
	done := make(chan struct{})
	go func() { // server -> browser
		defer close(done)
		s.pumpShell(ctx, c, sub, lt)
	}()

	// browser -> server
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		opCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		switch typ {
		case websocket.MessageBinary:
			lt.feed(data)
			err = s.be.ShellInput(opCtx, session, data)
		case websocket.MessageText:
			var ctl shellCtl
			if json.Unmarshal(data, &ctl) != nil {
				break
			}
			switch ctl.Type {
			case "resize":
				if ctl.Cols > 0 && ctl.Rows > 0 {
					err = s.be.ShellResize(opCtx, session, ctl.Cols, ctl.Rows)
				}
			case "confirm":
				s.log.Warn("dangerous shell line confirmed", "component", "panel", "operator", user, "machine", machine, "session", session, "line", ctl.Line)
				err = s.be.ShellConfirm(opCtx, session, ctl.Line)
			}
		}
		cancel()
		if err != nil {
			_ = writeJSONFrame(ctx, c, shellOut{Type: "error", Error: err.Error()})
		}
	}
	stop() // end the pump; the deferred cleanup then ends the session
	_ = c.Close(websocket.StatusNormalClosure, "")
	<-done
}

func dim(v string) uint16 {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 1000 {
		return 0
	}
	return uint16(n)
}

// pumpShell relays the session's events to the browser until the session ends
// or the socket does.
func (s *server) pumpShell(ctx context.Context, c *websocket.Conn, sub *fleet.Subscription, lt *lineTracker) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				_ = c.Close(websocket.StatusTryAgainLater, "lagging")
				return
			}
			x := ev.Exec
			var err error
			switch x.Kind {
			case exec.EventQueued:
				err = writeJSONFrame(ctx, c, shellOut{Type: "state", State: "queued"})
			case exec.EventRunning:
				err = writeJSONFrame(ctx, c, shellOut{Type: "state", State: "running"})
			case exec.EventChunk:
				if x.Chunk != nil {
					wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
					err = c.Write(wctx, websocket.MessageBinary, x.Chunk.Data)
					cancel()
				}
			case exec.EventRefused:
				reason := ""
				if x.State != nil {
					reason = x.State.Detail
				}
				err = writeJSONFrame(ctx, c, shellOut{Type: "refused", Line: lt.last(), Reason: reason})
			case exec.EventExited:
				out := shellOut{Type: "exit"}
				if x.Exit != nil {
					code := x.Exit.ExitCode
					out.Code, out.Reason, out.Error = &code, x.Exit.Reason, x.Exit.Error
				}
				_ = writeJSONFrame(ctx, c, out)
				_ = c.Close(websocket.StatusNormalClosure, "shell ended")
				return
			}
			if err != nil {
				return
			}
		}
	}
}

// lineTracker follows the line the operator is typing so that, when the agent
// refuses it, the confirmation dialog can show it. It is a best-effort guess
// (history and cursor keys defeat it): the token minted for the guessed line
// only works if the guess is exactly what the agent saw, so a wrong guess
// fails safe, on the agent.
type lineTracker struct {
	mu   sync.Mutex
	buf  []rune
	prev string // the most recently completed line
}

func (t *lineTracker) feed(data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range string(data) {
		switch {
		case r == '\r' || r == '\n':
			if len(t.buf) > 0 {
				t.prev = string(t.buf)
			}
			t.buf = t.buf[:0]
		case r == 0x7f || r == 0x08:
			if len(t.buf) > 0 {
				t.buf = t.buf[:len(t.buf)-1]
			}
		case r == 0x15 || r == 0x03: // Ctrl-U, Ctrl-C
			t.buf = t.buf[:0]
		case r >= 0x20 && r != 0x7f:
			t.buf = append(t.buf, r)
		}
	}
}

func (t *lineTracker) last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.prev
}
