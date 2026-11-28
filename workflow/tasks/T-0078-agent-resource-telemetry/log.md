## 2026-11-28 — Phase 1: Backend telemetry pipeline + Frontend widget design

- Implemented `TypeResourceState` and `ResourceStatePayload` in `internal/channel/envelope.go`, with encode/decode round-trip tests in `internal/channel/envelope_test.go`.
- Added machine-level worker resource reporter (`startResourceMonitor`, `reportResourceOnConnect`, holding one `watchdog.NewSystemSampler("", "")` for CPU% deltas) in `internal/agent/resource.go` and wired into `internal/agent/worker.go`, with unit tests in `internal/agent/resource_test.go`.
- Added live in-memory `ResourceState` and `MachineInfo.Resource` (`json:"resource,omitempty"`) in `internal/fleet/fleet.go`, dispatching `TypeResourceState` to `recordResourceState` (range validation, zero-timestamp stamping, `publishMachine`), with tests in `internal/fleet/resource_test.go`.
- Confirmed `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok, `go test -race ./internal/channel/...` ok, `./scripts/test.sh agent` ok, `./scripts/test.sh server` ok, and `-race` targeted tests on `channel`/`agent`/`fleet` ok.
- Wrote the design-first frontend widget specification in `workflow/tasks/T-0078-agent-resource-telemetry/review-1.md` and stopped before writing frontend code, awaiting supervisor review.

<!-- wf: commits=a76268b left=- tests=agent:pass,server:pass blocked=no -->
