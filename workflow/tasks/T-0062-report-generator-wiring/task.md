---
id: T-0062
title: report-generator-wiring
state: blocked
blocked_on: "Needs the Projects/tests construct (GAP-24, not yet briefed): a test report is generated from a completed test's archive + monitor data, and there are no tests to report on yet. Brief + build Projects first, then run this."
touches: [internal/panel/api.go, internal/panel/report.go, internal/report/,
          web/src/pages/ProjectsPage.jsx, internal/panel/report_test.go]
tests: [server]
after: [T-0047, T-0055]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Each completed test can produce the self-contained interactive HTML report (via internal/report) from its evidence archive + monitor data, downloadable/viewable from the panel. The Projects page Report buttons actually generate a report instead of showing a stub. (Requires Projects/tests to exist first.)"
---

## Why

The HTML test-report generator (`internal/report/report.go`, `internal/report/html.go`) is fully built but
**has no callers** — nothing in `cmd/`, `internal/fleet/`, or `internal/panel/` imports it, and the
Projects page Report buttons are stubs (audit GAP-36; §13 "each test produces a self-contained interactive
HTML report, generated from the archive + monitor data after the fact"). Wire it into the panel so reports
can be produced and viewed.

## Blocked / dependency

A report is "generated from a completed **test's** archive + monitor data." Tests live inside Projects,
which don't exist yet (GAP-24, decided-build but not briefed). **This task is blocked until the Projects/
tests task is built** — it needs a completed-test record to point the generator at. Keep `state: blocked`
until then; when Projects exists, set `after:` to include it and move to `ready`.

## What to build (once unblocked)

1. **Report route.** `internal/panel/report.go` + register in `internal/panel/api.go`: an endpoint that,
   for a completed test, assembles its inputs (the test's evidence archive segments + the monitor data for
   its window) and calls `internal/report` to render the self-contained HTML report; stream it for
   view/download behind the panel auth. Pull monitor data from the T-0055 history read path
   (`GET /api/monitor/history`) for the test window.
2. **Front-end.** Wire the Projects page Report buttons (React `web/src/pages/ProjectsPage.jsx`, once it
   exists) to request/download the report. Rebuild + commit the web bundle.

## Out of scope

- No change to the `internal/report` rendering library itself (it's done); no Load Controller data in the
  report beyond what exists; don't build Projects/tests here (that's GAP-24).

## Done means

- A completed test produces its interactive HTML report from archive + monitor data, viewable/downloadable
  from the panel; the Report buttons work. `gofmt`/`go vet`/`go build` clean; `scripts/test.sh server`
  passes with a report-generation test; bundle rebuilt.

## Facts this brief relies on

- Audit: GAP-36 — `internal/report/report.go`, `internal/report/html.go` exist but are unimported;
  `web/pages/projects.html:56,62,69,76` Report buttons are `data-stub`. Monitor history data source:
  `GET /api/monitor/history` (T-0055). Projects/tests dependency: GAP-24.

## Notes for the doer

- Do NOT start until Projects/tests exist and this task is moved to `ready`. When building, reuse the
  existing `internal/report` API as-is. Run `./scripts/wf start T-0062` before committing; commit inside
  touches + the log. Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:`
  (except via `wf start`).
