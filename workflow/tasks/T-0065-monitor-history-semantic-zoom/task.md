---
id: T-0065
title: monitor-history-semantic-zoom
state: merged
touches: [web/src/pages/MonitorPage.jsx, web/src/hooks/, internal/panel/monitor.go, internal/panel/monitor_test.go]
tests: [server]
after: [T-0055, T-0058]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The Monitor page has a working History tab (not a placeholder): it reads the VictoriaMetrics history API and opens as an OVERVIEW of the entire timeline with dense detail collapsed into merged labels; zooming in progressively reveals detail (semantic zoom), per the architecture. The live 10-min window tab is unchanged."
---

## Why

The Monitor History tab is a static sample placeholder (audit GAP-23; §8.4 L368 / §12.2#7: "the history
view opens as an overview of the entire timeline, with dense detail collapsed into merged labels; zooming
in progressively reveals detail — semantic zoom"). The backend history read path now exists
(`GET /api/monitor/history`, T-0055); build the real semantic-zoom History UI on the React Monitor page.

## What to build

1. **History data hook.** Query `GET /api/monitor/history` (T-0055) for a metric + time range; support
   **downsampled overview** queries (coarse `step`) and progressively **finer `step`** as the operator zooms
   in (`web/src/hooks/`). If the query endpoint needs an aggregation/step refinement for clean overviews,
   extend `internal/panel/monitor.go` minimally (the store is Prometheus-compatible; `query_range` with a
   step already exists).
2. **Semantic-zoom History tab (React).** In `web/src/pages/MonitorPage.jsx`, replace the History
   placeholder with a tab that: opens as a **whole-timeline overview** (merged/aggregated labels, not the
   60s live window), and on **zoom-in** (brush/scroll/range-select) re-queries at finer resolution,
   progressively revealing per-interval detail. Keep it readable in both themes; reuse the existing chart
   primitives/design system. Rebuild + commit the web bundle (`./web/build.sh`).

## Out of scope

- No change to the VictoriaMetrics store/ingest or retention (T-0055); no Grafana (T-0064); no change to the
  live-window tab; no new metrics. This is the in-panel history view only.

## Done means

- The History tab loads real data from the history API, opens as a full-timeline overview with merged
  labels, and zooming in progressively reveals finer detail; the live window tab still works.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` passes
  (any monitor.go query change covered by a test); web bundle rebuilt + committed.

## Facts this brief relies on

- Audit: GAP-23 §8.4 L368, §12.2#7 L566 (`web/pages/monitor.html:80-138` sample placeholder). History read
  API: `GET /api/monitor/history` (`internal/panel/monitor.go`, T-0055) over the VictoriaMetrics store
  (query_range + step). SPA Monitor page: `web/src/pages/MonitorPage.jsx` (T-0058).

## Notes for the doer

- Lean on `query_range` + `step` for the overview→detail levels; keep overview queries cheap. Rebuild +
  commit the bundle. Run `./scripts/wf start T-0065` before committing; commit inside touches + the log. Do
  NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
