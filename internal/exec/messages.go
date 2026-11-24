package exec

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
)

// Session modes.
const (
	ModeOneShot = "oneshot" // Mode A: run a command, stream, exit
	ModePTY     = "pty"     // Mode B: interactive shell over a PTY
)

// Stream names carried in ChunkPayload.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamPTY    = "pty" // merged terminal output
)

// Lifecycle states. "queued" is produced by the server-side Queue, "running"
// and "refused" by the agent; the final state is the exit message.
const (
	StateQueued  = "queued"
	StateRunning = "running"
	StateRefused = "refused"
)

// Exit reasons.
const (
	ReasonExited     = "exited"
	ReasonTimeout    = "timeout"
	ReasonTerminated = "terminated"
	ReasonRefused    = "refused"
	ReasonError      = "error"
	ReasonCancelled  = "cancelled" // removed from the queue before it ran
	ReasonLost       = "lost"      // channel dropped while it was running
)

// RunPayload is the payload of a run envelope (server -> agent). The envelope
// ID is the session_id.
type RunPayload struct {
	SessionID string `json:"session_id"`
	// Command is the shell command line (oneshot) or the shell program for a
	// PTY session (empty means /bin/sh).
	Command string `json:"command"`
	User    string `json:"user,omitempty"`
	Mode    string `json:"mode,omitempty"` // default oneshot
	// TimeoutSec is the per-command timeout; 0 means the agent default (300 s)
	// for oneshot and no timeout for PTY.
	TimeoutSec int `json:"timeout_sec,omitempty"`
	// ConfirmToken authorises a command that matches the dangerous-command
	// gate. See Gate.
	ConfirmToken string `json:"confirm_token,omitempty"`
	Cols         uint16 `json:"cols,omitempty"`
	Rows         uint16 `json:"rows,omitempty"`
}

// StatePayload is the payload of a state envelope (agent -> server).
type StatePayload struct {
	SessionID  string `json:"session_id"`
	State      string `json:"state"`
	CgroupPath string `json:"cgroup_path,omitempty"` // set on running
	Detail     string `json:"detail,omitempty"`      // refusal reason / refused line
}

// ChunkPayload is the payload of a chunk envelope (agent -> server): output.
type ChunkPayload struct {
	SessionID string `json:"session_id"`
	Stream    string `json:"stream"`
	Seq       uint64 `json:"seq"`
	Data      []byte `json:"data"` // base64 on the wire
}

// InputPayload is the payload of an input envelope (server -> agent): bytes
// typed into a PTY session. ConfirmToken authorises dangerous lines completed
// in this message.
type InputPayload struct {
	SessionID    string `json:"session_id"`
	Data         []byte `json:"data"`
	ConfirmToken string `json:"confirm_token,omitempty"`
}

// ExitPayload is the payload of an exit envelope (agent -> server).
type ExitPayload struct {
	SessionID  string `json:"session_id"`
	ExitCode   int    `json:"exit_code"` // 128+signal if killed by a signal
	DurationMS int64  `json:"duration_ms"`
	Reason     string `json:"reason"`
	Error      string `json:"error,omitempty"`
}

// ResizePayload is the payload of a resize envelope (server -> agent).
type ResizePayload struct {
	SessionID string `json:"session_id"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
}

// TerminatePayload is the payload of a terminate envelope (server -> agent).
type TerminatePayload struct {
	SessionID string `json:"session_id"`
}

// ConfirmPayload is the payload of a confirm envelope (server -> agent): a
// token pre-authorising the next matching line of a PTY session. The flow: a
// dangerous line is refused (state "refused"), the operator confirms, the
// server mints a token for that exact line and sends confirm, and the line is
// typed again.
type ConfirmPayload struct {
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
}

// Sender is the write half of a channel (*channel.Listener and
// *channel.Dialer both satisfy it).
type Sender interface {
	Send(ctx context.Context, env channel.Envelope) error
}

// send marshals payload into an envelope of type typ for session id.
func send(ctx context.Context, s Sender, typ, id string, payload any) error {
	env, err := channel.NewEnvelope(typ, id, payload)
	if err != nil {
		return err
	}
	return s.Send(ctx, env)
}

// decode unmarshals an envelope payload.
func decode(env channel.Envelope, v any) error {
	if err := json.Unmarshal(env.Payload, v); err != nil {
		return fmt.Errorf("exec: bad %s payload: %w", env.Type, err)
	}
	return nil
}
