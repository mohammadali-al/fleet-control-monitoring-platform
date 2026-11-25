---
id: T-0023
title: panel-live-wiring
state: ready
touches: [internal/panel/api.go, internal/panel/ws.go, internal/panel/templates.go,
          internal/panel/observe_test.go, internal/fleet/fleet.go,
          web/pages/evidence.html, web/pages/backup.html, web/pages/monitor.html, web/pages/watchdog.html,
          web/assets/js/live.js]
tests: [server]
after: [T-0011, T-0009, T-0013, T-0021, T-0022]
shares: {}
model: {claude: "sonnet high"}
operator_check: "On http://localhost:6789: Evidence lists real sessions with a working pull + export; Backup adds a server + shows diff + back-up-now + import; Monitor data shows live charts; Watchdog shows policy + events."
---

## Why

T-0011 wired the Operate pages. The four **Observe/Admin** pages are still mock. Wire them to their
live backends now that those exist.

## What to build

**Backend (Go) + the four page templates.** Reuse the API/WS + template approach from T-0011.

1. **Evidence** (`internal/evidence`, T-0009): session browser + per-session packet records, the
   **pull** button with sync status, and exports (pcapng/CSV/JSON).
2. **Backup** (`internal/backup`, T-0013): add backup server, diff view, suggested schedule, "back up
   now", import, the once-a-day reminder; start the backup `Run` loop from the server.
3. **Monitor data** (`internal/monitor` ingest/sink, T-0021): merged + per-machine + per-target charts,
   60 s live window, the command-overlay hover, the probe-config editor.
4. **Watchdog** (T-0022): policy editor (push on save), per-host stage + event history, counter reset.

Keep `data-stub` only on anything still unbacked. Trust the forwarded auth header (no auth logic).

## Out of scope

- The Maneuver/Baseline/Projects pages (coupled to the deferred Load Controller); new visual design
  (reuse the approved components); auth.

## Done means

- Each of the four pages shows **live** data from its backend (Evidence, Backup, Monitor data,
  Watchdog), verified with httptest for the API/template render and a WS test for one live stream.
- The panel still serves every page (light/dark); `gofmt -l .` empty, `go vet ./...` clean,
  `scripts/test.sh server` passes.

## Facts this brief relies on

- Page contents (Monitor data, Watchdog, Evidence, Backup): `Fleet_Control_Monitoring_Platform_Architecture.md:566`, `:567`.
- Backends: evidence (`workflow/tasks/T-0009-evidence-sync-export/task.md`), backup
  (`workflow/tasks/T-0013-backup/task.md`), monitor runtime (`workflow/tasks/T-0021-monitor-agent-runtime/task.md`),
  watchdog wiring (`workflow/tasks/T-0022-watchdog-agent-wiring/task.md`).
- The panel wiring pattern to reuse: `workflow/tasks/T-0011-panel-wiring/task.md`; `workflow/design/panel-wiring.md`.

## Notes for the doer

- **Broad — do it page by page.** Evidence + Backup first (their backends are already merged); then
  Monitor data + Watchdog. If it grows past one clean review, commit the first pages and report the
  split to the supervisor — don't jam it all in untested.
- If wiring needs a change in `cmd/fleet-server/main.go` outside touches, save it as a `*.patch` in the
  task folder (like T-0011/T-0018) and report it — the supervisor applies it.
