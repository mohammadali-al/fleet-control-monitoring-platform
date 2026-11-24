---
id: T-0014
title: test-report
state: merged
touches: [internal/report/report.go, internal/report/html.go, internal/report/report_test.go,
          internal/report/template.gohtml]
tests: [server]
after: [T-0009, T-0010]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

Each test ends in a **self-contained interactive HTML report** the operator can keep or hand to a
customer — the human-readable summary of what a test did, drawn from the evidence archive and monitor
data after the fact.

## What to build

**Backend (Go), main-server side.** A report generator producing one standalone HTML file per test.

1. **Gather** (`report.go`): for a given test, collect from the archive (T-0009) and monitor data
   (T-0010): project/test identity and target `(ip, origin)`; the Maneuver plan + load pattern;
   participating worker agents; per-worker and aggregate **resource usage over the window** (**cores
   used**, **GB RAM**); commands run with **packet/flow counts**; monitor charts for the window
   (availability, response time) with the **command overlay**; evidence **hashes / manifest references**;
   **start-alignment** and any **"missed" agents**; watchdog events during the window; capture **drop
   counts**.
2. **Render** (`html.go` + `template.gohtml`): a **single self-contained** HTML file — inline CSS/JS,
   embedded data (no external requests), interactive where it helps (collapsible sections, the monitor
   chart with hover overlay). Reuse the panel's visual language where practical. Generated **after the
   fact**, not live.

## Out of scope

- Live/streaming report generation — this runs after the test window closes.
- Panel wiring of a "Generate report" button (a later wiring task; the static buttons are stubs).
- Changing evidence/monitor storage — read what T-0008/T-0009/T-0010 produced.

## Done means

- Given a fixture test (synthetic archive + monitor data), the generator produces **one HTML file** that
  opens offline (no external requests) and contains every section listed above, with correct
  **cores/GB** units and packet/flow counts.
- The monitor chart renders with the command overlay; sections are navigable; hashes/manifest refs are
  present.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes (asserts the HTML is
  self-contained — no `http(s)://` asset refs — and that key fields render).

## Facts this brief relies on

- Per-test **self-contained interactive HTML** report, generated from the archive + monitor data after
  the fact: `Fleet_Control_Monitoring_Platform_Architecture.md:578`, `:580`.
- Report contents (identity, target, Maneuver plan, resource usage cores/GB, commands + packet/flow
  counts, monitor charts + overlay, hashes, start-alignment/missed, watchdog events, drop counts):
  `Fleet_Control_Monitoring_Platform_Architecture.md:579`.
- Reads evidence (T-0009) and monitor data (T-0010): `workflow/tasks/T-0009-evidence-sync-export/task.md`;
  `workflow/tasks/T-0010-monitors/task.md`.

## Notes for the doer

- Embed everything (CSS/JS/data) so the file is portable and works from `file://` — the test must verify
  there are no external asset references.
- For a non-Maneuver test (just manual commands), the Maneuver/load-pattern section is omitted cleanly;
  the report still works (Maneuver execution is deferred, D-0005).
