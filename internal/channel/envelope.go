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
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
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
