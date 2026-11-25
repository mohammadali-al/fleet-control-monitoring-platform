# T-0031 agent-service-control: log

## 2026-11-25: service-control channel message + worker/monitor handlers + fleet/panel integration (Go 1.26.8)

**Built (inside `touches`).**

- **`internal/channel/envelope.go`.** Added `TypeServiceControl` (`service-control`), `TypeServiceControlResult` (`service-control-result`), `ServiceActionStop|Start|Status`, `ServiceControlPayload`, and `ServiceControlResultPayload`.
- **`internal/agent/servicecontrol.go`, `worker.go`, `monitor.go`.** Added `ServiceController` interface (`Stop`, `Start`, `Status`) with `osServiceController` (`systemctl disable --now --no-block fleet-agent.service` / `systemctl enable --now` / `systemctl is-active` on Linux; `launchctl disable` + `launchctl bootout` / `launchctl enable` + `launchctl bootstrap` / `launchctl print` on macOS). Added `handleServiceControl` and `SetServiceController` for both `Agent` and `Monitor`, wired into `worker.handle` and `Monitor.handle` independently of the command executor.
- **`internal/fleet/fleet.go`.** Added `Manager.ServiceControl(ctx, machine, action)` and `recordServiceControlResult` in `agentHandler` for all roles (worker and monitor), updating `Registry` `Stopped` on stop/start and publishing machine state.
- **`internal/panel/machines.go`.** Added `routesMachines` (`POST /api/machines/{id}/stop`, `POST /api/machines/{id}/start`, `GET /api/machines/{id}/service-status`) using `Manager.ServiceControl` for all roles with no monitor 501.
- **Tests.** Added `internal/agent/servicecontrol_os_test.go`, `internal/agent/servicecontrol_test.go`, and `internal/panel/machines_servicecontrol_test.go` covering OS command rendering (Linux/macOS) and in-process `fleettest.MTLS` end-to-end stop/status/start on both monitor and worker agents (including worker without `GateSecret` / executor) and via the panel HTTP endpoints.

**Outside `touches` (saved as `.patch` under this task folder; `git apply --check` clean).**

- `workflow/tasks/T-0031-agent-service-control/wiring-agentdial-panel.patch`: wires `TypeServiceControl` into `DialingMonitor.handle` (`internal/agent/agentdial.go`) and mounts `s.routesMachines(mux)` in `routesAPI` (`internal/panel/api.go`).
- `workflow/tasks/T-0031-agent-service-control/wiring-enroll-servicecontrol.patch`: wires token-enrollment Stop/Start (`cmd/fleet-server/enroll.go`, `internal/enroll/enroll.go`, `internal/enroll/api.go`) to `Manager.ServiceControl` instead of `Submit`, removing the monitor 501 there as well.

**Verification.**

- `gofmt -l .` empty, `go vet ./...` clean, `./scripts/test.sh server` and `./scripts/test.sh agent` pass, and `go test -race ./internal/channel ./internal/agent ./internal/fleet ./internal/panel` passes.

<!-- wf: left=- tests=server:pass,agent:pass blocked=no -->
wf: T-0031 done
