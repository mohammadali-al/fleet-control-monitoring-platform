---
id: T-0031
title: agent-service-control
state: ready
touches: [internal/channel/envelope.go, internal/agent/servicecontrol.go, internal/agent/servicecontrol_test.go,
          internal/agent/worker.go, internal/agent/monitor.go, internal/fleet/fleet.go,
          internal/panel/machines.go, internal/panel/machines_servicecontrol_test.go]
tests: [server, agent]
after: [T-0021, T-0026]
shares: {}
model: {claude: "sonnet high"}
operator_check: "On the Machines page, Stop on a MONITOR agent now works remotely (disables its service; it stays down across reboot) and Start brings it back — same as a worker."
---

## Why

Token-enrollment Stop/Start works on workers (it rode the command path) but **returns 501 on monitors**,
which have no command executor. Add a dedicated **service-control** channel message both runtimes handle,
so remote Stop/Start works for monitors too (and cleanly for workers).

## What to build

**Backend (Go) + the Machines-page Stop/Start.**

1. **Channel message** (`internal/channel/envelope.go`): a `service-control` type (server → agent) with
   an action (`stop` | `start` | `status`).
2. **Agent handler** (`internal/agent/servicecontrol.go`), wired into **both** `worker.go` and
   `monitor.go`: on `stop` it **disables** the agent's own unit (`systemctl disable --now` /
   `launchctl disable+bootout`), on `start` re-enables it, reporting result — **independent of the
   command executor** so it works on monitors.
3. **Server** (`internal/fleet/fleet.go`): a `ServiceControl(ctx, machine, action)` that pushes it.
4. **Panel** (`internal/panel/machines.go`): the enrolled-agents Stop/Start use it for all roles; drop
   the monitor 501.

## Out of scope

- The enrollment/Detach flow (T-0026, unchanged except Stop now goes through this); the command executor.

## Done means

- In-process `fleettest`: a `service-control stop` to a **monitor** agent runs its disable path and the
  agent reports stopped; `start` re-enables; a worker uses the same path. The panel endpoint triggers it
  for both roles (no 501).
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server`+`agent` pass with `-race` on touched packages.

## Facts this brief relies on

- Stop = disable the unit (stays down across reboot), Start re-enables (local or remote):
  `workflow/tasks/T-0026-token-enrollment/task.md` (the monitor-Stop 501 gap is in its log).
- Monitor agents have no command executor: `Fleet_Control_Monitoring_Platform_Architecture.md:197`.
- Agent runtimes to extend (worker + monitor) + config-push pattern: `workflow/tasks/T-0018-agent-wiring/task.md`;
  `workflow/tasks/T-0021-monitor-agent-runtime/task.md`.

## Notes for the doer

- Put the actual `systemctl`/`launchctl` behind a small interface so the logic is testable without root.
  Reuse `fleettest`. If `internal/agent/agent.go` needs a hook you can't add inside touches, save a `.patch`.
