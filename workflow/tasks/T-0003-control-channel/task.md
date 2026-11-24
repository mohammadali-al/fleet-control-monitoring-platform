---
id: T-0003
title: control-channel
state: merged
touches: [internal/channel/envelope.go, internal/channel/envelope_test.go,
          internal/channel/server.go, internal/channel/agent.go,
          internal/channel/channel_test.go, go.mod, go.sum]
tests: [server]
after: [T-0002]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

The persistent mTLS WebSocket is the spine every feature rides on (commands, telemetry, monitor data,
file transfer). This task establishes the connection and the message envelope — nothing else yet.
Scope is the **default `server_dial`** direction only; `agent_dial` is a separate later task.

## What to build

**Backend (Go), package `internal/channel`.** Uses `internal/pki` (T-0002) for TLS.

Under `server_dial`: the **agent listens** (mTLS WS **server**) and the **main server dials** (mTLS WS
**client**). Control messages flow main → agent either way, but here the *connection* is opened by the
main server.

1. **Envelope (`envelope.go`).** `Envelope{ Type string; ID string; Payload json.RawMessage }` with
   helpers to write/read one envelope over a `*websocket.Conn` (use `coder/websocket`;
   `wsjson` is fine). A tiny built-in handshake: on connect the main sends `hello`, the agent replies
   `hello-ack` carrying its machine id; and a `ping`/`pong` pair.

2. **Agent listener (`agent.go`).** `type Listener` — starts an mTLS WS server (via
   `pki.ServerTLSConfig`) on a configured address, accepts **one** main-server connection at a time,
   serves envelopes, exposes `Send(env)` and a received-message handler. It identifies the peer (the
   main server) from its verified client cert (`pki.PeerID`).

3. **Server dialer (`server.go`).** `type Dialer` — dials an agent's WS endpoint over mTLS (via
   `pki.DialTLSConfig` with the agent's expected IP), performs the `hello` handshake, exposes
   `Send(env)` and a handler, and **reconnects with capped backoff + jitter** on drop (architecture
   §4.3). It records the agent's verified id from the agent's server cert.

## Out of scope

- The `agent_dial` direction (later task).
- Any real message types beyond hello/ping/pong (commands, telemetry, etc. are later tasks).
- Wiring into the `fleet-server` / `fleet-agent` binaries' real config/startup (a thin later task);
  keep this a library exercised by tests.
- Backpressure/flow-control tuning, large-file transfer.

## Done means

- In-test (one process): a `Listener` with an agent peer cert and a `Dialer` with a main peer cert
  (both from a throwaway CA via `internal/pki`) complete the `hello`/`hello-ack` handshake over mTLS
  on `127.0.0.1`; each side reads the other's verified id.
- A `ping` gets a `pong`; an arbitrary `Envelope` round-trips with its `Type`/`ID`/`Payload` intact.
- Killing the listener and restarting it: the `Dialer` **reconnects on its own** (backoff) and the
  handshake completes again.
- A dialer presenting a cert from a **different** CA is **rejected** by the listener.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes.

## Facts this brief relies on

- One persistent **WebSocket (TLS/mTLS)** per machine carries everything; first-class Go client
  `coder/websocket`: `Fleet_Control_Monitoring_Platform_Architecture.md:147`, `:148`, `:596`.
- Default **`server_dial`**; under it the **agent listens** and the main server dials:
  `Fleet_Control_Monitoring_Platform_Architecture.md:152`, `:584`.
- Reconnect with **backoff + jitter** on outage: `Fleet_Control_Monitoring_Platform_Architecture.md:164`, `:175`.
- Channel client is a shared module on both roles: `Fleet_Control_Monitoring_Platform_Architecture.md:232`.
- PKI helpers (`ServerTLSConfig`, `DialTLSConfig`, `PeerID`): `workflow/tasks/T-0002-internal-ca/task.md`.

## Notes for the doer

- Add the `coder/websocket` dependency (updates `go.mod`/`go.sum`) — this task **needs a module
  download**. If the sandbox has no network and the fetch fails, **stop and report blocked** (do not
  hand-roll a WebSocket); note it in the log footer.
- Keep `server.go`/`agent.go` free of business logic — just connection, handshake, send/recv, and the
  dialer's reconnect loop. Context-first signatures; clean shutdown on context cancel.
- Tests must generate their own CA/certs at runtime (D-0002); bind to `127.0.0.1` on an ephemeral port.
