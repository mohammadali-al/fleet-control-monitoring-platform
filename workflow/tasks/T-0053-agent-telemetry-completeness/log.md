# T-0053 agent-telemetry-completeness: log

## Entry 1 (doer)

- **GAP-17 — disk telemetry to panel:**
  - `internal/channel/envelope.go`: added `TypeDiskState` (`"disk-state"`) and `DiskStatePayload` (`status`, `total_bytes`, `free_bytes`, `used_percent`, `warn_at`, `pause_at`, `at`), mirroring the backward-tolerant `TypePublicIP` / `PublicIPPayload` pattern.
  - `internal/capture/segments.go` & `internal/capture/capture.go`: added `DiskStatus`, `CheckDisk`, `statfsDisk`, and `SetOnDiskState` on `SegmentWriter` and `capture.Controller`. Guard transitions (`ok` / `warn` at 80% / `critical` at 90%) notify outside the writer lock immediately (including when `ErrDiskPaused` pauses writes).
  - `internal/agent/diskstate.go`, `internal/agent/worker.go`, `internal/agent/config_apply.go`: added `diskStateSource` on `liveCapturer` and worker disk telemetry reporting on connect, on guard-state transition, and on the periodic monitor interval (with `DataDir` `statfs` fallback when live capture is off).
  - `internal/registry/registry.go` & `internal/fleet/fleet.go`: updated `registry.DiskState` (plus `Disk.Status` validation and deep-copying in `clone`) and added `TypeDiskState` dispatch (`recordDiskState`) updating `registry.Machine.Disk` and publishing `EventMachine` on the fleet bus.
  - `web/src/hooks/useMachines.js` & `web/src/pages/MachinesPage.jsx` + rebuilt `web/assets/dist/app.js`: patched live `/ws/events` `machine` updates directly into the React Machines state, honored reported `m.disk.status` (`warn` / `critical`) alongside `diskView`, updated the Disk guard summary tile (`All reported disks OK` once reports arrive), and updated the table footer note.
- **GAP-16 — public-IP network-change trigger:**
  - `internal/capture/srcip.go`, `internal/capture/netchange_linux.go`, `internal/capture/netchange_other.go`: added best-effort `watchNetworkChanges` wired into `PublicIPCache.Start` / `Stop`. On Linux it subscribes to `NETLINK_ROUTE` (`RTMGRP_IPV4_ROUTE | RTMGRP_IPV6_ROUTE | RTMGRP_IPV4_IFADDR | RTMGRP_IPV6_IFADDR`) with `SO_RCVTIMEO` and debounced `TriggerRefresh()`; on unsupported/restricted environments it notes and skips without affecting boot, 15-min periodic, or manual refresh triggers.
- **GAP-13 — monitor-agent reflector:**
  - `internal/agent/publicip.go`, `internal/agent/monitor.go`, `internal/agent/agentdial.go`: extracted shared `publicIPReporter` and instantiated `capture.PublicIPCache` + `publicIPReporter` on both `Monitor` (`server_dial`) and `DialingMonitor` (`agent_dial`), reporting cached IP on connect, refreshing on `TypePublicIPRefresh`, and forwarding `Reflectors` from `AgentConfig.Capture`.
- **GAP-08 — persistent command history:**
  - `internal/fleet/fleet.go`: added `DataDir` to `fleet.Config` (also inferred from `Provisioner.Store` for `fleet.Open`), kept the in-memory fast path (`history`, `outputs`, `sessions`, `order` capped at `maxSessions = 500`), persisted session lifecycle updates + one-shot outputs to `<DataDir>/commands/history.jsonl` (with compaction beyond `2*maxSessions`), and reloaded history on `New` (normalizing interrupted pre-restart sessions to `finished`/`lost`).
- **Tests & verification:**
  - Added tests in `internal/capture/segments_test.go`, `internal/capture/srcip_test.go`, `internal/capture/netchange_linux_test.go`, `internal/agent/diskstate_test.go` (mTLS agent->registry disk telemetry round-trip), `internal/agent/monitor_test.go` (monitor reflector boot + refresh), `internal/fleet/fleet_test.go` (`TestDiskStateDispatch`, `TestCommandHistoryPersistenceAndReload`), and `internal/panel/machines_test.go`.
  - `./web/build.sh` rebuilt `web/assets/dist/app.js` (422.5kb) and `web/assets/dist/app.css` (39.8kb).
  - `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` passed with `node` removed from `PATH`, `./scripts/test.sh server` passed all packages, and `go test ./cmd/fleet-agent/...` passed.

wf: T-0053 done
