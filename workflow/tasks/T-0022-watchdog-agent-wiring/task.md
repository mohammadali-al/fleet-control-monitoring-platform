---
id: T-0022
title: watchdog-agent-wiring
state: ready
touches: [cmd/fleet-agent/main.go, internal/agent/watchdog_wire.go, internal/agent/watchdog_wire_test.go,
          internal/fleet/fleet.go, internal/config/config.go]
tests: [server, agent]
after: [T-0012, T-0018]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

T-0012 built the watchdog library + systemd units (the unit runs `fleet-agent watchdog`), but
`cmd/fleet-agent` has no `watchdog` subcommand and the agent never requests/applies policy or forwards
events. Wire it so a provisioned agent's watchdog actually runs under a pushed policy.

## What to build

**Backend (Go).**

1. **`fleet-agent watchdog` subcommand** (`cmd/fleet-agent/main.go`): runs the `internal/watchdog` loop
   as its own process (matching `fleet-watchdog.service`), reading `policy.yaml` from `DataDir` and
   applying it live (the watchdog already picks up policy changes).
2. **Policy push + event forward** (`internal/agent/watchdog_wire.go`): the main agent, on a `policy`
   envelope, writes it via `watchdog.StorePolicy` and replies `policy-ack`; watchdog events written to
   `DataDir` are forwarded to the server as `watchdog-ev`. (Policy/event envelope types exist from T-0012.)
3. **Server side** (`internal/fleet/fleet.go`): push the per-host/group watchdog policy to a machine on
   connect (and on edit), and store/surface incoming `watchdog-ev` events.

## Out of scope

- The watchdog logic/ladder/guards (T-0012, unchanged); the panel Watchdog page (T-0023); the Load
  Controller interaction (thresholds already sit above its ceiling).

## Done means

- In-process via `fleettest`: a pushed policy is stored + acked and the watchdog loop applies it; a
  watchdog event is forwarded to the server; an invalid policy is rejected and last-known keeps running.
- `cmd/fleet-agent watchdog --config ...` starts the loop + shuts down cleanly.
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server`+`agent` pass with `-race` on the
  touched packages.

## Facts this brief relies on

- Policy is **pushed over the control channel**, applied live; last-known keeps running:
  `Fleet_Control_Monitoring_Platform_Architecture.md:541`.
- The unit runs `fleet-agent watchdog`; watchdog library + `StorePolicy`/`PolicyAck`/`EventEnvelope`:
  `workflow/tasks/T-0012-watchdog/task.md`.
- Agent runtime + config-push pattern: `workflow/tasks/T-0018-agent-wiring/task.md`.

## Notes for the doer

- Follow T-0012's suggested split: main agent handles the policy envelope (StorePolicy) + forwards
  events; the watchdog runs as a separate process reading the data dir. Reuse `fleettest`.
