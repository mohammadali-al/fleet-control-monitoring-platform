---
id: T-0028
title: watchdog-reset-counters
state: ready
touches: [internal/channel/envelope.go, internal/fleet/watchdog.go, internal/agent/watchdog_wire.go,
          internal/watchdog/guards.go, internal/panel/watchdog.go, internal/panel/watchdog_reset_test.go,
          web/pages/watchdog.html]
tests: [server, agent]
after: [T-0022]
shares: {}
model: {claude: "sonnet high"}
operator_check: "On the Watchdog page, 'Reset counters' for a host actually clears its daily restart/reboot caps and alert-only latch (no longer a stub)."
---

## Why

T-0023 left the Watchdog **"Reset counters"** button a stub — there's no channel path to tell an agent's
watchdog to reset its daily caps / alert-only latch. Wire it end-to-end.

## What to build

**Backend (Go) + the panel button.**

1. **Channel message** (`internal/channel/envelope.go`): a `watchdog-reset` type (server → agent).
2. **Server** (`internal/fleet/watchdog.go`): a `ResetCounters(ctx, machine)` that pushes it to that
   machine.
3. **Agent** (`internal/agent/watchdog_wire.go` + `internal/watchdog/guards.go`): on `watchdog-reset`,
   run the equivalent of `Guards.Reset` (clear restart/reboot counters, cooldowns-keep-or-clear per the
   existing `ResetCounters` semantics, and the alert-only latch) against the running watchdog (via the
   data dir the watchdog process reads).
4. **Panel** (`internal/panel/watchdog.go` + `web/pages/watchdog.html`): wire the button to call it;
   drop the `data-stub`.

## Out of scope

- The watchdog ladder/guards logic (T-0012, unchanged beyond exposing reset); the heartbeat work (T-0027).

## Done means

- In-process `fleettest`: a reset pushed from the server clears the agent watchdog's counters + alert-only
  latch (verified via the watchdog state); the panel endpoint triggers it.
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server`+`agent` pass with `-race` on touched packages.

## Facts this brief relies on

- Daily caps + alert-only + operator counter reset exist in the watchdog: `Fleet_Control_Monitoring_Platform_Architecture.md:534`;
  `workflow/tasks/T-0012-watchdog/task.md`.
- Watchdog agent wiring (policy push/StorePolicy/event forward) to extend: `workflow/tasks/T-0022-watchdog-agent-wiring/task.md`.
- The stub button was flagged in: `workflow/tasks/T-0023-panel-live-wiring/log.md`.

## Notes for the doer

- Reuse the T-0022 policy-push plumbing for the new message. If a change outside touches is needed
  (e.g. `cmd/fleet-server`), save a `.patch` and report it.
