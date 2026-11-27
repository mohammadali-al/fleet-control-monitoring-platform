---
id: T-0061
title: watchdog-visibility-persistence
state: ready
touches: [internal/watchdog/watchdog.go, internal/fleet/fleet.go, internal/panel/watchdog.go,
          internal/registry/registry.go, web/src/pages/WatchdogPage.jsx, internal/panel/watchdog_test.go]
tests: [server]
after: [T-0047, T-0050]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The Watchdog panel shows monitor-agent (headless) watchdogs too, not just workers; and a machine's watchdog stage + daily counters survive a fleet-server restart (they are persisted/rebuilt, not lost). Live behaviour otherwise unchanged."
---

## Why

Two watchdog panel gaps (audit GAP-35; §12.2#8/§12.3 "no hidden state"): (1) monitor agents run a headless
watchdog but are **not shown** in the panel (`internal/onboard/ssh.go:794-797` sets
`FLEET_WATCHDOG_MODE=headless`; `web/pages/watchdog.html` "Monitor agents … are not shown"); (2) host stage
+ daily counters are **reconstructed in memory from the event stream** (`internal/panel/watchdog.go:94-97`)
and are **lost on server restart**. Make monitor watchdogs visible and persist the host state.

## What to build

1. **Persist host watchdog state.** Persist each machine's watchdog **stage + daily counters** (restarts/
   reboots today, last ladder step, alert-only flag) so they survive a `fleet-server` restart — e.g. store
   on `registry.Machine` (a `WatchdogState` field) or a small sidecar store, updated as events arrive and
   reloaded on start, instead of only rebuilding from the in-memory event stream
   (`internal/panel/watchdog.go:94-97`, `internal/fleet/fleet.go`). Keep live updates working.
2. **Show monitor-agent watchdogs.** Include monitor-profile agents' headless watchdog status/events in the
   panel data and render them on the React Watchdog page (`web/src/pages/WatchdogPage.jsx`) — labeled as
   monitor agents — so no watchdog is hidden.
3. Rebuild + commit the web bundle (`./web/build.sh`).

## Out of scope

- No change to the watchdog ladder/escalation logic, the liveness timer, or group assignment (GAP-31/
  T-0060); no new watchdog actions. Counters are "today"-scoped as today — just make them durable.

## Done means

- Monitor-agent watchdogs appear in the Watchdog panel; a machine's stage + daily counters persist across a
  fleet-server restart (verified by a reload test); live updates still work.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` passes
  with tests for persistence/reload of stage+counters and monitor-agent visibility; bundle rebuilt + committed.

## Facts this brief relies on

- Audit: GAP-35 — `internal/panel/watchdog.go:94-97` (in-memory reconstruction; lost on restart),
  `internal/onboard/ssh.go:794-797` (monitors headless), `web/pages/watchdog.html:85,124` (counters reset /
  monitors not shown). Registry: `internal/registry/registry.go`. React Watchdog page:
  `web/src/pages/WatchdogPage.jsx` (T-0050). Watchdog event flow: `internal/fleet/fleet.go`,
  `internal/watchdog/watchdog.go`.

## Notes for the doer

- Decide persistence location (registry field vs sidecar) and note it; keep the event-stream live path as
  the source of truth for updates, persistence as the durable backing. Rebuild + commit the bundle. Run
  `./scripts/wf start T-0061` before committing; commit inside touches + the log. Do NOT push/merge/
  branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
