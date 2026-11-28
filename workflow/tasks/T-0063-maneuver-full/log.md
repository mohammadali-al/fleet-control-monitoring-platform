# T-0063 maneuver-full: log

## Doer entry

- **A. Planning (`internal/config/config.go`, `internal/maneuver/`, `internal/panel/api.go`, `internal/panel/maneuver.go`, `internal/panel/maneuver_test.go`):**
  - Added `ManeuversFileName` (`maneuvers.json`, stored under `config.ProjectsDirName`) and `ManeuverDirName` (`maneuver`) in `internal/config/config.go`.
  - Created `internal/maneuver/` (`maneuver.go`, `store.go`, `maneuver_test.go`) with atomic JSON persistence, single `(ip, origin)` target validation (`ValidateTarget`), Load Controller pattern gating (`ValidatePattern`: `all` enabled; `linear` and `spikes` schema-ready and rejected with `ErrPatternDeferred`), timeline item bounds validation, and baseline materialization (`Materialize`) via `internal/baseline.RenderItem`.
  - Added `/api/maneuvers` CRUD + `/api/maneuvers/preview` behind `s.live` in `internal/panel/maneuver.go` and `internal/panel/api.go`.
- **B. Sync-start (`internal/channel/envelope.go`, `internal/fleet/bus.go`, `internal/fleet/fleet.go`, `internal/fleet/open.go`, `internal/fleet/maneuver.go`, `internal/agent/agent.go`, `internal/agent/worker.go`, `internal/agent/maneuver.go`):**
  - Added backward-tolerant typed envelopes `maneuver-prepare`, `maneuver-ack`, `maneuver-commit`, `maneuver-abort`, and `maneuver-status` in `internal/channel/envelope.go`.
  - Implemented server sync-start coordination in `internal/fleet/maneuver.go`: pushes materialized plan + future `start_at` (`T`), waits for all selected workers to ACK `ready`, commits `T` only when all ACK `ready` before `T`, and sends `maneuver-abort` without committing if any agent reports `missed`/`rejected`, drops offline, or times out.
  - Implemented agent sync-start handling in `internal/agent/maneuver.go`: rejects orders received at or after `T` as `missed` without starting late.
- **C. Continuation + wall-clock rejoin (`internal/agent/maneuver.go`, `internal/exec/executor.go`, `internal/fleet/maneuver.go`):**
  - Persisted committed Maneuver plans under `<agentDataDir>/maneuver/state.json`.
  - Worker continues scheduled/remaining timeline items autonomously during main-server outages and reconciles status on reconnect (`onConnect` -> `maneuver-status`; server reconstructs or enforces abort if stopped).
  - On agent boot/reboot mid-Maneuver, `maneuverRunner.start` reloads persisted state, skips elapsed item windows, starts in-progress items immediately with remaining wall-clock duration, and schedules upcoming items at their wall-clock offset.
- **D. Replica capture attribution (`internal/exec/messages.go`, `internal/exec/session.go`, `internal/exec/executor.go`, `internal/capture/records.go`, `internal/capture/capture.go`, `internal/agent/config_apply.go`, `internal/agent/worker.go`, `internal/agent/maneuver.go`):**
  - Added `ModeReplica` and attribution fields (`ManeuverID`, `RunID`, `ItemID`, `Replica`, `DurationMS`) to `exec.RunPayload`, `exec.StatePayload`, `exec.Session`, and `exec.SessionInfo`.
  - Added `capture.SessionMeta` and `Controller.CaptureWithMeta` stamping `ManeuverID`, `RunID`, `ItemID`, `Replica` onto `capture.Record`.
  - Each Maneuver replica runs via `Executor.RunSync` in its own tracked cgroup owned by its `session_id` and attaches `CaptureWithMeta`.
- **E. Front-end & execution API (`internal/panel/maneuver.go`, `internal/panel/ws.go`, `web/src/hooks/useManeuver.js`, `web/src/pages/ManeuverPage.jsx`, `web/src/routes.js`, rebuilt `web/assets/dist/app.js`):**
  - Added `/api/maneuvers/runs`, `/api/maneuvers/{id}/start`, `/api/maneuvers/runs/{runId}/stop`, and `maneuver` event frames on `/ws/events`.
  - Built React `ManeuverPage` (single `(ip, origin)` editor, interactive timeline with baseline placement & command preview, worker selector, `All-in-one` enabled and `Linear`/`Spikes` disabled with `"needs the Load Controller — deferred"`, Start/Stop with confirmation dialog and live `/ws/events` ACK/missed/running view) and registered it in `web/src/routes.js`.
  - Rebuilt bundle via `./web/build.sh` (`assets/dist/app.js` 528.8kb, `assets/dist/app.css` 39.8kb).
- **Verification:**
  - `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` passed without `node` in `PATH`, `./scripts/test.sh server` and `./scripts/test.sh agent` passed (covering planning CRUD/validation, sync-start ready/commit/missed, outage continuation + wall-clock rejoin, replica session/cgroup attribution, and panel API + `/ws/events`).

wf: T-0063 done
