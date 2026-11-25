---
id: T-0025
title: agent-dial-channel
state: merged
touches: [internal/channel/agentdial.go, internal/channel/agentdial_test.go,
          internal/fleet/fleet.go, internal/fleet/agentdial.go, internal/config/config.go,
          cmd/fleet-server/main.go]
tests: [server, agent]
after: [T-0003]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

T-0003 built only the **`server_dial`** direction (the agent listens, the main server dials). Token
enrollment (T-0026) and any firewalled/NAT'd agent need **`agent_dial`**: the agent dials **out** to the
server, which runs a WS **listener**. Build that direction so it's selectable per machine via
`connect_mode`.

## What to build

**Backend (Go).** Reuse the existing `Envelope`, hello/ping handshake, and mTLS (`internal/pki`).

1. **Server listener** (`internal/fleet/agentdial.go` + `cmd/fleet-server`): an mTLS WS **server**
   endpoint that accepts **agent-initiated** connections, identifies the machine by its verified client
   cert, and hands the connection to the `Manager` as a link **indistinguishable** from a dialed one
   (same send/recv, same online/offline, config-push, command/monitor/evidence flows).
2. **Agent dialer** (`internal/channel/agentdial.go`): the agent dials the server's WS endpoint over
   mTLS with **capped backoff + jitter** reconnect (mirror T-0003's dialer semantics, opposite role);
   on connect it runs the normal handshake and serves the protocol.
3. **`connect_mode` selects direction** (`internal/config`): `server_dial` (existing) vs `agent_dial`
   (new) per machine; the agent runtime (worker/monitor) starts the dialer instead of the listener when
   `agent_dial`.

## Out of scope

- Token enrollment (T-0026); onboarding changes; the panel. Just the channel direction + Manager glue.

## Done means

- In-process `fleettest`: an `agent_dial` agent dials the server, is reported online, and a one-shot /
  monitor-data / config-push round-trip works exactly as under `server_dial`; the dialer reconnects on
  its own after the server restarts; a different-CA agent is rejected.
- `server_dial` behavior is unchanged.
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server`+`agent` pass with `-race` on
  `internal/channel` and `internal/fleet`.

## Facts this brief relies on

- `connect_mode` selects who dials; both directions supported by the same binary; default `server_dial`:
  `Fleet_Control_Monitoring_Platform_Architecture.md:152`, `:158`, `:164`, `:584`.
- The existing channel (Envelope, handshake, backoff) + `server_dial` Listener/Dialer:
  `workflow/tasks/T-0003-control-channel/task.md`.
- The Manager + runtime to plug into: `workflow/tasks/T-0018-agent-wiring/task.md`.

## Notes for the doer

- Keep one link abstraction so Manager code doesn't branch on dial direction. Reuse `fleettest`.
- If `cmd/fleet-server/main.go` needs more than a small addition, save a `.patch` in the task folder and report it.
