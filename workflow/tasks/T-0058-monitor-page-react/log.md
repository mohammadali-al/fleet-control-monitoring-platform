# T-0058 monitor-page-react: log

## Entry 1 (doer)

- **React SPA Monitor page (`web/src/hooks/useMonitor.js`, `web/src/pages/MonitorPage.jsx`, `web/src/routes.js`, `web/assets/dist/app.js`):**
  - Created `useMonitor` hook loading `GET /api/monitor/live?window=<sec>`, subscribing to `/ws/monitor` for live `samples` frames with exponential-capped reconnect, polling on a 5 s / 1 s cadence while unpaused, subscribing to `/ws/events` for machine updates, and managing probe config via `GET /api/monitor/config`, `POST /api/monitor/settings`, `POST /api/monitor/jobs`, `POST /api/monitor/jobs/{id}/delete`, and `POST /api/monitor/jobs/{id}/interval`.
  - Implemented `MonitorPage` at parity with the legacy Live and Probe config tabs (omitting out-of-scope History tab and Grafana embed): view mode (`merged`/`machine`/`target`), target filter, live window (`60`/`120`/`300` s), Pause/Resume, KPI cards, SVG polyline chart with failure ticks, 12-column hover overlay, command overlay markers, collapsible agent data table, global defaults form, jobs table with frequency override & delete actions, and job-create form with GAP-19 probe-type selector (`ICMP`, `TCP`, `HTTP`, `DNS`) + optional DNS query name/resolver inputs.
  - Registered `component: MonitorPage` on the `monitor` entry in `web/src/routes.js` and rebuilt the bundle with `./web/build.sh` (`assets/dist/app.js` 472.9kb, `assets/dist/app.css` 39.8kb).
- **GAP-19 DNS probe targets (`internal/monitor/jobs.go`, `internal/panel/monitor.go`):**
  - Extended `monitor.Spec` and `monitor.Job` with `ProbeTypes`, `DNSName`, and `DNSServer` (plus `ErrBadProbeType`, `ErrOriginRequired`, `ErrDNSNameRequired`, `ErrBadDNSServer` mapped to HTTP 400 in `monitorStatus`), and updated `initialTargets` + `dnsTarget` to create `probe.TypeDNS` targets (`<job>/dns`) while preserving default `[icmp]` / `[icmp, tcp, http]` behavior when `ProbeTypes` is omitted.
  - Extended `POST /api/monitor/jobs` (`jobBody`) to accept `probe_types` / `probe_type`, `dns_name`, and `dns_server`.
- **GAP-22 overlay packet counts (`internal/panel/monitor.go`, `web/src/pages/MonitorPage.jsx`):**
  - Fixed `archivedPackets` to use `sessionPacketCounts` on shared segments so only the target session's records are counted instead of the full segment's `sg.Records`.
  - Added `overlayIntervals = 12`, `monInterval` (`intervals` on `GET /api/monitor/live`), and `monCommand.IntervalPackets` (`interval_packets`) computed by `populateCommandPacketStats`, counting per-session packets per interval across `[since, now]` and per-session totals accurately across shared segments.
  - Updated `MonitorPage` hover tooltips on both `.plot__col` intervals and `.ovl__mark` command markers to display per-interval packet counts.
- **Tests & verification:**
  - `internal/monitor/jobs_test.go`: added `TestDNSTargetCreationAndValidation`.
  - `internal/panel/monitor_test.go`: added `TestMonitorCreateJobDNSAndProbeTypes`, `TestMonitorOverlaySharedSegmentAndIntervalCounts`, and `TestMonitorSPARouteInBundle`.
  - `gofmt -l .` empty; `go vet ./...` clean; `go build ./...` passed with `node` removed from `PATH`; `./scripts/test.sh server` passed; `go test -race ./internal/monitor/... ./internal/panel/...` passed.

wf: T-0058 done
