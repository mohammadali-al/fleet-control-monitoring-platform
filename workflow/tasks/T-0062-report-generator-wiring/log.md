# T-0062 report-generator-wiring: log

## Doer entry

- **Report adapter (`internal/report/adapter.go`, `internal/report/adapter_test.go`):**
  - Added `SpecFromProjectTest`, `PlanFromManeuver`, and `SamplesFromHistory` inside `internal/report/` without altering any rendering logic in `report.go`/`html.go`/templates.
  - Maps `project.Test` (`result_pointers.evidence` + `result_pointers.monitor` + target/agents/timestamps), `maneuver.Maneuver`/`MaterializedPlan`, and `monitor.HistorySeries` (`fleet_probe_ok` + `fleet_probe_latency_ms`) into `report.Spec` and `[]monitor.Sample`, aligning millisecond boundaries for TSDB precision.
- **Panel report endpoint (`internal/panel/report.go`, `internal/panel/api.go`, `internal/panel/report_test.go`):**
  - Registered `GET /api/projects/{id}/tests/{testId}/report` behind `s.live`.
  - Returns `503` when no backend is configured and `404` for unknown project/test or incomplete (`planned`/`running`) tests.
  - For finished (`completed`/`failed`) tests, assembles `report.Spec` and `report.Sources` from `result_pointers`, evidence archive segments/records (both `s.be.Commands()` and archived replica sessions matching `maneuver_run_id`/window), `monitor.HistoryStore.QueryRange` (the T-0055 history read path, with `o.Series` in-memory fallback when no TSDB is configured), Maneuver plan/alignment, and watchdog events, then renders via `report.Gather` + `report.Render` as `text/html; charset=utf-8` (`inline` by default, `attachment` with `?download=1`, `filename="report-<project>-<test>.html"`).
  - Added tests covering 503, 404 (unknown + planned + running), full end-to-end HTML generation with fake VictoriaMetrics `VMStore` + evidence archive isolation, `?download=1` attachment headers, and SPA bundle wiring.
- **Front-end & bundle (`web/src/pages/ProjectsPage.jsx`, `web/assets/dist/app.js`):**
  - Wired completed/failed test rows in `ProjectsPage.jsx` with `Report` (opens interactive HTML report in a new tab) and download (`?download=1`) actions pointing to `/api/projects/{id}/tests/{testId}/report`.
  - Rebuilt the SPA bundle with `./web/build.sh` (`assets/dist/app.js` 592.5kb, `assets/dist/app.css` 39.8kb).
- **Verification:**
  - `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` passed with `node` removed from `PATH`, and `./scripts/test.sh server` passed (`internal/panel`, `internal/report`, and all server packages green).

wf: T-0062 done
