// Package channel is the persistent mTLS WebSocket between the main server and
// an agent. This first cut covers the connection, the envelope, the hello
// handshake and ping/pong, in the default server_dial direction (the agent
// listens, the main server dials). Real message types ride on top later.
package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
)

// Built-in envelope types.
const (
	TypeHello    = "hello"
	TypeHelloAck = "hello-ack"
	TypePing     = "ping"
	TypePong     = "pong"
)

// Command-execution envelope types (payload shapes live in internal/exec).
// The envelope ID of each is the session_id it belongs to.
const (
	TypeRun       = "run"       // server -> agent: start a one-shot or PTY session
	TypeState     = "state"     // agent -> server: lifecycle (running, refused)
	TypeChunk     = "chunk"     // agent -> server: command output
	TypeInput     = "input"     // server -> agent: PTY input bytes
	TypeExit      = "exit"      // agent -> server: session ended (code, duration)
	TypeResize    = "resize"    // server -> agent: PTY window size
	TypeTerminate = "terminate" // server -> agent: end a session
	TypeConfirm   = "confirm"   // server -> agent: dangerous-command confirmation token
)

// Evidence-sync envelope types (payload shapes live in internal/evidence). Pull
// and idle push share one protocol: the agent's manifest is either requested
// (pull) or offered unprompted (push); either way the main server then diffs it
// against its archive and requests byte ranges of the segments it lacks.
const (
	TypeEvManifestReq = "ev-manifest-req" // server -> agent: send me your manifest
	TypeEvManifest    = "ev-manifest"     // agent -> server: manifest (reply to a request, or an idle-push offer)
	TypeEvRange       = "ev-range"        // server -> agent: send bytes of one segment from an offset
	TypeEvChunk       = "ev-chunk"        // agent -> server: a slice of a segment (last one is flagged)
	TypeEvVerify      = "ev-verify"       // server -> agent: the segment verified (or was rejected)
	TypeQueueState    = "queue-state"     // server -> agent: whether the server FIFO queue is empty (QueueStatePayload)
)

// QueueStatePayload reports whether the main server's per-machine command FIFO
// queue is empty (§7.2 idle-push gate).
type QueueStatePayload struct {
	Empty bool `json:"empty"`
}

// Monitor envelope types (payload shapes live in internal/probe and
// internal/monitor). Monitor data is a separate path from evidence: it never
// rides the ev-* types.
const (
	TypeMonConfig = "mon-config" // server -> monitor agent: probe targets, frequency, timeouts
	TypeMonData   = "mon-data"   // monitor agent -> server: a flushed batch of probe results
)

// Config-push envelope types. The main server owns configuration and pushes it
// to an agent over the channel: the agent asks for it when a connection is
// established (config-req), the server answers with a config message, and the
// agent applies it and acknowledges (config-ack). The same pattern later carries
// watchdog policy (§11.6) and monitor configuration (§8.1); config is the
// worker-agent flavour (gate secret, capture settings) and is extended by
// adding optional sections to ConfigPayload.
const (
	TypeConfigReq = "config-req" // agent -> server: I just connected, push my config
	TypeConfig    = "config"     // server -> agent: configuration to apply (ConfigPayload)
	TypeConfigAck = "config-ack" // agent -> server: applied, or why not (ConfigAckPayload)
)

// ConfigPayload is the payload of a config envelope (server -> agent). Every
// section is optional: an absent section leaves the agent's current setting
// as it is, so a push may carry only what changed. Applying a payload is
// idempotent; the server re-sends it on every connection.
type ConfigPayload struct {
	// GateSecret is the dangerous-command confirmation secret: the agent
	// verifies the tokens the main server mints with it (exec.Gate). It is a
	// secret: it is held in memory only and never logged.
	GateSecret string `json:"gate_secret,omitempty"`
	// Capture is the per-node evidence-capture configuration.
	Capture *config.CaptureConfig `json:"capture,omitempty"`
}

// String and LogValue keep the gate secret out of logs and error messages.
func (p ConfigPayload) String() string {
	return fmt.Sprintf("ConfigPayload{gate_secret:%s capture:%v}", redacted(p.GateSecret), p.Capture != nil)
}

// LogValue implements slog.LogValuer.
func (p ConfigPayload) LogValue() slog.Value {
	return slog.GroupValue(slog.String("gate_secret", redacted(p.GateSecret)), slog.Bool("capture", p.Capture != nil))
}

func redacted(s string) string {
	if s == "" {
		return "unset"
	}
	return "[redacted]"
}

// ConfigAckPayload is the payload of a config-ack envelope (agent -> server).
type ConfigAckPayload struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Watchdog envelope types (architecture 11.6; payload semantics live in
// internal/watchdog). The server pushes a version-stamped YAML policy profile;
// the agent validates it, applies it live, and acknowledges. A profile that
// fails validation is refused and the last-known one keeps running.
const (
	TypePolicy     = "policy"      // server -> agent: a policy profile (PolicyPayload)
	TypePolicyAck  = "policy-ack"  // agent -> server: applied, or why not (PolicyAckPayload)
	TypeWatchdogEv = "watchdog-ev" // agent -> server: a watchdog event to notify the panel (WatchdogEventPayload)
	// TypeWatchdogReset is the operator's counter reset (panel button): the
	// agent clears the daily restart/reboot counters, the alert-only latch and
	// the ladder stage of its watchdog. Cooldowns are kept. The payload is
	// empty; the watchdog answers with a counters-reset watchdog-ev.
	TypeWatchdogReset = "watchdog-reset" // server -> agent
)

// Service-control envelope types (server -> agent, handled by both worker and
// monitor runtimes independently of the command executor).
const (
	TypeServiceControl       = "service-control"        // server -> agent: stop | start | status (ServiceControlPayload)
	TypeServiceControlResult = "service-control-result" // agent -> server: result of service-control (ServiceControlResultPayload)
)

// Service-control actions.
const (
	ServiceActionStop   = "stop"
	ServiceActionStart  = "start"
	ServiceActionStatus = "status"
)

// ServiceControlPayload is the payload of a service-control envelope (server -> agent).
type ServiceControlPayload struct {
	Action string `json:"action"` // stop | start | status
}

// ServiceControlResultPayload is the payload of a service-control-result envelope (agent -> server).
type ServiceControlResultPayload struct {
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	State  string `json:"state,omitempty"`
	Error  string `json:"error,omitempty"`
}

// PolicyPayload is the payload of a policy envelope (server -> agent).
type PolicyPayload struct {
	// Version is the profile's version stamp; it must match the "version" key
	// inside YAML. Informational for logs and the ack.
	Version int `json:"version"`
	// YAML is the policy profile text.
	YAML string `json:"yaml"`
}

// PolicyAckPayload is the payload of a policy-ack envelope (agent -> server).
type PolicyAckPayload struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Version is the profile version now in force on the agent.
	Version int `json:"version"`
}

// WatchdogEventPayload is the payload of a watchdog-ev envelope (agent ->
// server): breach, action, alert-only and similar notifications.
type WatchdogEventPayload struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	Scope  string    `json:"scope,omitempty"`
	Metric string    `json:"metric,omitempty"`
	Value  float64   `json:"value,omitempty"`
	Target string    `json:"target,omitempty"`
	Step   string    `json:"step,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Path is the URL path the agent serves the channel on.
const Path = "/v1/channel"

const (
	// maxMessageBytes caps one envelope. Bulk transfer is a separate concern.
	maxMessageBytes = 4 << 20
	// handshakeTimeout bounds the hello/hello-ack exchange.
	handshakeTimeout = 10 * time.Second
	// writeTimeout bounds a single envelope write.
	writeTimeout = 10 * time.Second
)

// Errors returned by the channel.
var (
	ErrNotConnected = errors.New("channel: not connected")
	ErrBadHandshake = errors.New("channel: bad handshake")
)

// Envelope is the one message shape on the wire.
type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// HelloPayload is the payload of hello (sent by the main server) and of
// hello-ack (sent by the agent): the sender's claimed machine id. Each side
// checks the claim against the id in the peer's verified certificate.
type HelloPayload struct {
	MachineID string `json:"machine_id"`
}

// Handler receives every envelope that is not consumed by the channel itself
// (hello/hello-ack are handshake-only and ping is answered internally; pong
// is delivered so a caller can observe it).
type Handler func(ctx context.Context, env Envelope)

// NewEnvelope builds an envelope, marshalling payload (nil means no payload).
func NewEnvelope(typ, id string, payload any) (Envelope, error) {
	env := Envelope{Type: typ, ID: id}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Envelope{}, fmt.Errorf("channel: marshal payload: %w", err)
		}
		env.Payload = b
	}
	return env, nil
}

// WriteEnvelope writes one envelope as a text message.
func WriteEnvelope(ctx context.Context, c *websocket.Conn, env Envelope) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return wsjson.Write(ctx, c, env)
}

// ReadEnvelope reads one envelope. It blocks until one arrives, the
// connection fails, or ctx is done.
func ReadEnvelope(ctx context.Context, c *websocket.Conn) (Envelope, error) {
	var env Envelope
	if err := wsjson.Read(ctx, c, &env); err != nil {
		return Envelope{}, err
	}
	if env.Type == "" {
		return Envelope{}, errors.New("channel: envelope has no type")
	}
	return env, nil
}

// helloID decodes the claimed machine id from a hello or hello-ack envelope.
func helloID(env Envelope) (string, error) {
	var p HelloPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return "", fmt.Errorf("%w: bad %s payload: %v", ErrBadHandshake, env.Type, err)
	}
	return p.MachineID, nil
}

// readLoop reads envelopes until the connection ends, answering ping with a
// pong (same ID and payload) and passing everything else to h. It returns the
// error that ended the loop.
func readLoop(ctx context.Context, c *websocket.Conn, h Handler) error {
	for {
		env, err := ReadEnvelope(ctx, c)
		if err != nil {
			return err
		}
		switch env.Type {
		case TypePing:
			if err := WriteEnvelope(ctx, c, Envelope{Type: TypePong, ID: env.ID, Payload: env.Payload}); err != nil {
				return err
			}
		case TypeHello, TypeHelloAck:
			// A repeated handshake mid-session is a protocol error.
			return fmt.Errorf("%w: unexpected %s after handshake", ErrBadHandshake, env.Type)
		default:
			if h != nil {
				h(ctx, env)
			}
		}
	}
}
