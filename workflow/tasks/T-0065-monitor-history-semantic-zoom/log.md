# T-0065 monitor-history-semantic-zoom: log

## Entry 1 (doer)

- **History query endpoint refinement (`internal/panel/monitor.go`, `internal/panel/monitor_test.go`):**
  - Extended `GET /api/monitor/history` (`monHistory`) to accept `step=auto` (picking a clean coarse-to-fine `step` via `chooseHistoryStep` bounded by optional `max_points`, default 120, range 12..500) and optional `aggregate=mean` (`mergeHistorySeriesMean`, averaging series values per timestamp into a single `agent: "all", target: "merged"` overview series), while preserving `step=0` raw export when `step` is omitted.
  - Added `TestMonitorHistoryAutoStepAndAggregateMean` and extended `TestMonitorSPARouteInBundle` in `internal/panel/monitor_test.go`.
- **History semantic-zoom hook (`web/src/hooks/useMonitorHistory.js`):**
  - Created `useMonitorHistory` querying `GET /api/monitor/history` for the active metric (`latency_ms`, `ok`, `loss`, `status`) and `ok` availability series in parallel using `AbortController` + monotonic sequence guards, handling 503 (`noStore`) and HTTP/network errors.
  - Implemented progressive resolution (`stepSecForRange`: `86400s`/`21600s`/`3600s` at whole-timeline overview -> `600s` at day -> `30s` at hour -> `5s`/`1s`/`0` raw at detail) and semantic zoom tier mapping (`overview` -> `day` -> `hour` -> `detail`), zoom history stack, preset zoom, pan, and reset.
- **Semantic-zoom History tab & bundle (`web/src/pages/MonitorPage.jsx`, `web/assets/dist/app.js`):**
  - Added the `History` tab (`#mtab-hist` / `#mp-hist`) to `MonitorPage` alongside untouched `Live`, `Grafana`, and `Probe config` tabs, updating `handleTabKeyDown` arrow/Home/End tab navigation.
  - Replaced the legacy History placeholder with a live VictoriaMetrics-backed semantic-zoom view that opens as a whole-timeline overview (default 7d, selectable 24h/7d/30d/90d) with collapsed merged status segments (`.zoom__bar`) and merged chart lines, progressively revealing per-target and per-agent/probe detail on zoom-in via brush range-drag, wheel zoom, segment click, zoom level radios, or `Zoom in`/`Zoom out`/pan buttons.
  - Uses CSS design tokens (`--ok`, `--warn`, `--danger`, `--series-1..5`, `--accent`) for readability in both dark and light themes, plus an accessible intervals/series `<details>` table.
  - Rebuilt the SPA bundle with `./web/build.sh` (`assets/dist/app.js` 560.1kb, `assets/dist/app.css` 39.8kb).
- **Verification:**
  - `gofmt -l .` empty; `go vet ./...` clean; `go build ./...` passed with `node` removed from `PATH`; `./scripts/test.sh server` passed across all server packages.

wf: T-0065 done
