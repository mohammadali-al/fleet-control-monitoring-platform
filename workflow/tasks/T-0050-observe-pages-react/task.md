---
id: T-0050
title: observe-pages-react
state: merged
touches: [web/, internal/panel/server_test.go]
tests: [server]
after: [T-0047, T-0052]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The Watchdog and Evidence pages are ported to React route components following the T-0047 foundation pattern: each renders inside the persistent SPA shell (no reload, rain keeps running), its client route works on deep-link/refresh, and its live behaviour works — Watchdog shows live counters/stages + reset action, Evidence lists/streams/exports evidence and shows payload_head (from the T-0052 API). Legacy .html fallback removed for these two. No backend/API change here."
---

## Why

Port the **Observe** parity pages — **Watchdog** and **Evidence** — from legacy to React route components
(per T-0047). (**Monitor** is excluded: its real build — time-series store T-0055, Grafana embed, and the
History/semantic-zoom UI — is handled by the separate monitoring tasks, not a stub port.) **Front-end only
— no backend/API change in this task.**

## What to build

Following the route-component contract from **T-0047** (read `web/src/routes.js` + `web/src/pages/MachinesPage.jsx`
first), port each page to a React route under `web/src/pages/`, registered in `web/src/routes.js`:

1. **Watchdog → `web/src/pages/WatchdogPage.jsx`:** live counters, per-host stages/policy table, and the
   **Reset all counters** action (`watchdog-reset`), updating live over the channel — reuse the Machines
   live + action pattern. Parity with today's page.
2. **Evidence → `web/src/pages/EvidencePage.jsx`:** evidence list, segment stream/preview, export/download
   (pcapng/csv/json), and **display `payload_head`** per record (the Evidence record API gained
   `payload_head` + exact shared-segment counts in T-0052 — consume them here).

Remove the legacy full-load fallback for `/watchdog` and `/evidence`. Reuse the design-system CSS; preserve
every id/class/data-* existing live logic/tests rely on. Rebuild the web bundle (`./web/build.sh`).

## Out of scope

- **Monitor** page and any time-series / Grafana / history work (separate tasks); no backend/API/WS changes
  here; no new features/redesign; don't touch other sections' pages or the shell/router/build. Watchdog
  monitor-agent visibility + counters-surviving-restart (GAP-35) is a separate backend item — this port
  renders what the backend currently exposes.

## Done means

- `/watchdog` and `/evidence` are React routes inside the persistent shell: no reload on nav, rain
  continuous, deep-link/refresh work. Watchdog shows live counters/stages + reset works; Evidence
  lists/streams/exports and shows `payload_head`. Legacy fallback removed.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` passes
  (incl. existing watchdog/evidence panel tests, updated only for SPA serving); web bundle rebuilt +
  committed. Manual pass in both themes.

## Facts this brief relies on

- Foundation pattern + reference component + route registry: `web/src/routes.js`,
  `web/src/pages/MachinesPage.jsx`, `web/src/lib/events.jsx` (T-0047). Legacy pages (parity reference):
  `web/pages/{watchdog,evidence}.html`; existing tests `internal/panel/{watchdog_test.go,observe_stream_test.go}`.
  Evidence `payload_head` + exact counts come from T-0052 (`internal/panel/evidence.go`).

## Notes for the doer

- **Copy the T-0047 route-component contract exactly.** Watchdog is live-data; verify updates stream
  without a reload. Rebuild + commit the web bundle. Run `./scripts/wf start T-0050` before committing;
  commit inside touches + the log. Do NOT push/merge/branch-delete or edit decisions/generated/other
  tasks/`state:` (except via `wf start`).
