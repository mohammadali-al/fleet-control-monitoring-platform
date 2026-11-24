## T-0003 control-channel (doer, claude sonnet high)

Built package `internal/channel` for the `server_dial` direction (agent listens, main dials). The
`coder/websocket` v1.8.15 download worked in the sandbox (go.mod/go.sum updated).

- `envelope.go`: `Envelope{Type, ID, Payload json.RawMessage}`, `NewEnvelope`, `WriteEnvelope`/`ReadEnvelope`
  (wsjson, text frames, 4 MiB read cap), built-in types `hello`, `hello-ack`, `ping`, `pong`, URL path
  `/v1/channel`, and the shared `readLoop` (auto-answers `ping` with `pong` of same ID/payload; `pong` and
  all other types go to the handler; a hello after the handshake is a protocol error).
- `agent.go`: `Listener` (`Listen` binds, `Serve(ctx)` blocks, `Addr`, `Close`, `Peer`, `Send`). mTLS via the
  caller-supplied `pki.ServerTLSConfig`; peer id from `pki.PeerID(r.TLS)`. One connection at a time: a
  second gets HTTP 409. Replies `hello-ack{machine_id}`. Cancel closes the listener and the live connection.
- `server.go`: `Dialer` (`NewDialer`, `Run(ctx)`, `Send`, `AgentID`). Dials `wss://addr/v1/channel` with the
  `pki.DialTLSConfig` supplied; agent id is `pki.PeerID(resp.TLS)` (verified server cert CN). Reconnect
  loop: delay doubles from 500ms to a 30s cap (configurable), jitter spreads it over [d/2, d], reset after a
  completed handshake. A WebSocket ping every 20s (configurable) drops a dead connection so the loop replaces it.
- Decisions: (1) hello carries `machine_id` and each side requires it to equal the peer's verified cert id
  (mismatch = handshake failure); the cert, not the message, is the identity. (2) `pong` is delivered to the
  handler so callers can observe it; `ping` is answered internally. (3) TLS configs are passed in by the
  caller (built with `internal/pki`), so the channel holds no key material of its own. (4) No business logic
  in server.go/agent.go.
- Tests (`channel_test.go`, `envelope_test.go`; throwaway CAs at runtime, 127.0.0.1 ephemeral port):
  handshake with verified ids both ways; ping/pong both directions; envelope round trip (Type/ID/Payload,
  both directions, and absent payload); reconnect after the listener is killed and restarted on the same
  address (new handshake, and a message flows after); dialer with a cert from a different CA (trusting our CA,
  so only the listener can be rejecting) is refused and no handshake completes; reverse case (agent cert from
  another CA) refused by the dialer; second concurrent connection refused with the first undisturbed; send
  before connect returns `ErrNotConnected`; jitter bounds; cancel closes a live connection.
- Self-check: `gofmt -l .` empty; `go vet ./...` clean; `go test -race -count=5 ./internal/channel` and
  `scripts/test.sh server` pass; `scripts/build.sh` ok (bin/ removed afterwards).

<!-- wf: commits=PENDING left=- tests=server:pass blocked=no -->
