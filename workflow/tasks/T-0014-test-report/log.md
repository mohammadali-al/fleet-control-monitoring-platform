
## 2026-11-24 doer: T-0014 test-report

Built `internal/report/`: a per-test, self-contained interactive HTML report generator.

- `report.go`: `Gather(Spec, Sources) -> *Data`. The archive (`evidence.Archive`) supplies packet/flow counts per command and per machine (records scanned from archived segments, flow = distinct machine+protocol+src+dst), manifest hashes/paths, per-segment drop counts, and a manifest digest (SHA-256 over sorted "file sha256" lines per machine, then over the machine digests). Monitor samples (`monitor.Sample`, filtered by the target's `HistoryKey` and the window) give availability / response-time series per probe type, bucketed to at most 240 points. Control-plane facts the archive/monitor store do not hold (identity, target, Maneuver plan + pattern, workers, usage samples in cores / GB, commands, start-alignment, watchdog events) come in through `Spec` for the later panel wiring. Read-only: only machines already in the archive are queried (Archive.Segments would create a directory).
- `html.go` + `template.gohtml` + `report.css` + `report.js` (embedded): one standalone file, inline CSS/JS, data embedded as JSON, no external request. Charts are drawn server-side as inline SVG (works without JS) with the command overlay (one band per command) and watchdog markers; JS adds the hover readout, section nav, collapse-all, theme toggle. Panel tokens reused. `Generate`, `WriteFile` (atomic).
- A test with no `Plan` omits the Maneuver/pattern section, nav link and chart cleanly; with no alignment the section says so.
- Self-check: `gofmt -l .` empty, `go vet ./...` clean, `scripts/build.sh` ok, `scripts/test.sh server` pass (report package asserts no http(s):// outside the target data, no src/href/url()/@import/fetch, all sections, cores/GB, packet/flow counts, overlay bands, hashes, drops, hostile-text escaping). Looked at a rendered fixture in headless Chrome.
- Note for review (R-08): the brief cites architecture lines :578/:579/:580; in the current file the same content is at :579/:580/:581 (off by one). Content matches.
- `TestWriteSample` writes the fixture report to `$REPORT_SAMPLE_OUT` (skipped otherwise) for eyeballing.

<!-- wf: commits=14b2ce3 left=- tests=server:pass blocked=no -->
