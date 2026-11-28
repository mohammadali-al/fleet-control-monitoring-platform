## 2026-11-28 — Phase 1: Backend telemetry pipeline + Frontend widget design

- Implemented `TypeResourceState` and `ResourceStatePayload` in `internal/channel/envelope.go`, with encode/decode round-trip tests in `internal/channel/envelope_test.go`.
- Added machine-level worker resource reporter (`startResourceMonitor`, `reportResourceOnConnect`, holding one `watchdog.NewSystemSampler("", "")` for CPU% deltas) in `internal/agent/resource.go` and wired into `internal/agent/worker.go`, with unit tests in `internal/agent/resource_test.go`.
- Added live in-memory `ResourceState` and `MachineInfo.Resource` (`json:"resource,omitempty"`) in `internal/fleet/fleet.go`, dispatching `TypeResourceState` to `recordResourceState` (range validation, zero-timestamp stamping, `publishMachine`), with tests in `internal/fleet/resource_test.go`.
- Confirmed `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok, `go test -race ./internal/channel/...` ok, `./scripts/test.sh agent` ok, `./scripts/test.sh server` ok, and `-race` targeted tests on `channel`/`agent`/`fleet` ok.
- Wrote the design-first frontend widget specification in `workflow/tasks/T-0078-agent-resource-telemetry/review-1.md` and stopped before writing frontend code, awaiting supervisor review.

<!-- wf: commits=a76268b left=- tests=agent:pass,server:pass blocked=no -->

## 2026-11-28 — Phase 2: Frontend Machines page resource widget

- Implemented `ResourceMeter`, `ResourceCell`, and `DetailResourceSection` in `web/src/pages/MachinesPage.jsx` per `review-1.md`:
  - Added the `Resources` column before `Disk` (updating empty-row `colSpan` to `10`), displaying compact stacked CPU and MEM `.meter` bars (`minWidth: 9.5rem`), 75%/90% warn/danger thresholds, Load/CPU + Egress + Updated tooltip, reduced opacity when offline-but-held, and `Unreported` when `!m.resource`.
  - Added `DetailResourceSection` above `DetailDiskSection` in `MachineDetailsDrawer` with status chip, accessible `progressbar` meters, and `Load / CPU`, `Egress`, and `Updated` `<dl>` rows.
- Rebuilt SPA bundle via `./web/build.sh` (`web/assets/dist/app.js`).
- Ran `gofmt -l .` (clean), `go vet ./...`, `go build ./...`, `./scripts/test.sh server`, and `./scripts/test.sh agent` (all passed). `check-app` is not installed in this environment.

<!-- wf: commits=a76268b,a3d396e,e11454e left=- tests=agent:pass,server:pass blocked=no -->
