---
id: T-0050
title: observe-pages-react
state: ready
touches: [web/, internal/panel/server_test.go]
tests: [server]
after: [T-0047]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The Monitor data, Watchdog, and Evidence pages are ported to React route components following the T-0047 foundation pattern: each renders inside the persistent SPA shell (no reload, rain keeps running), its client route works on deep-link/refresh, and its live behaviour works — Monitor shows live probe series, Watchdog shows live counters/stages + reset action, Evidence lists/streams/exports evidence. Legacy .html fallback removed. No backend/API change."
---

## Why

Port the **Observe** nav section — **Monitor data**, **Watchdog**, **Evidence** — from server-rendered HTML
to React route components, following T-0047. These are the most **live-data-heavy** pages (SSE/WS series,
counters, evidence streaming), so they lean hardest on the foundation's live-data hook. **Front-end only.**

## What to build

Following the route-component contract from **T-0047** (read its reference Machines component first):

1. **Monitor data** (`web/pages/monitor.html` → React): live probe series/charts fed by `/ws/events` /
   the monitor series API (bounded live series), at parity (keep existing chart placeholders/behaviour —
   no new history/Grafana work).
2. **Watchdog** (`web/pages/watchdog.html` → React): live counters, per-host stages/policy table, and the
   **Reset all counters** action (`watchdog-reset`), updating live over the channel — reuse the Machines
   live + action pattern.
3. **Evidence** (`web/pages/evidence.html` → React): evidence list, segment stream/preview, and
   export/download (pcapng/csv/json) at parity.

Register on client routes (`/monitor`, `/watchdog`, `/evidence`); remove legacy full-load fallback. Reuse
the design-system CSS; preserve every `id`/`class`/`data-*` existing live logic/tests rely on.

## Out of scope

- No backend/API/WS changes; no new features (no time-series store / Grafana / history beyond what exists);
  no redesign; don't touch other sections' pages or the shell/router/build.

## Done means

- `/monitor`, `/watchdog`, `/evidence` are React routes inside the persistent shell: no reload on nav, rain
  continuous, deep-link/refresh work. Monitor shows live series; Watchdog shows live counters/stages + reset
  works; Evidence lists/streams/exports. Legacy fallback removed.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes (incl. the existing
  watchdog/evidence/monitor panel tests, updated only for SPA serving). Manual pass in both themes.

## Facts this brief relies on

- Foundation pattern + reference component + route registry: `workflow/tasks/T-0047-panel-spa-foundation/task.md`.
  Legacy pages: `web/pages/{monitor,watchdog,evidence}.html`. Live wiring: `web/assets/js/live.js` +
  `/ws/events`; existing panel tests `internal/panel/{watchdog_test.go,observe_stream_test.go}`.

## Notes for the doer

- **Copy the T-0047 route-component contract exactly.** These three are live-data-heavy — verify updates
  stream without a reload. Run `./scripts/wf start T-0050` before committing; commit inside touches + the
  log. Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
