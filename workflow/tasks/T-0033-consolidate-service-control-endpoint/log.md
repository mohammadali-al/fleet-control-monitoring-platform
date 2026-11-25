## T-0033 — consolidate-service-control-endpoint

- Unmounted `s.routesMachines(mux)` from `routesAPI` in `internal/panel/api.go`.
- Removed the unused `/api/machines/{id}/stop`, `/start`, and `/service-status` handlers and their orphaned helpers (`serviceControlTimeout`, `serviceController`, `serviceControlResponse`, `routesMachines`, `serviceControlMachine`) from `internal/panel/machines.go`.
- Retargeted `internal/panel/machines_servicecontrol_test.go` (`TestEnrollServiceControlMonitorAndWorkerFleettest`) to the surviving `/api/enroll/machines/{id}/stop` and `/start` endpoints wired to `Manager.ServiceControl` over mTLS (`fleettest.NewMTLS()`), verifying Stop/Start works with 200 OK (no 501) for both a monitor and a worker as well as error mapping (404 unknown, 502 offline, 415 non-JSON).
- Verified `go version` (`go1.26.8 linux/amd64`), `gofmt -l .` empty, `go vet ./...` clean, `grep -rn '/api/machines/' internal/panel` has no stop/start/service-status matches, and `./scripts/test.sh server` passes.

wf: T-0033 done
