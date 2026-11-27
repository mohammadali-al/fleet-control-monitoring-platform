# T-0060 watchdog-groups: log

## Entry 1 (doer)

- **Registry & validation (`internal/registry/registry.go`):**
  - Added `Groups []string` (`json:"groups,omitempty"`) to `registry.Machine` next to `WatchdogState` (leaving `WatchdogState` untouched).
  - Deep-copied `Groups` in `Machine.clone()` (normalizing empty slices to `nil` for clean JSON round-trips).
  - Added `ValidateGroupName` and `ValidateGroups` (shared `validateIdent` helper with `validateID`: `[A-Za-z0-9._-]`, non-empty, max 64 chars, no duplicates) and enforced in `Machine.Validate()`.
- **Override precedence & watchdog resolution (`internal/watchdog/watchdog.go`, `internal/panel/watchdog.go`, `internal/onboard/ssh.go`):**
  - Confirmed `Profile.Effective(machineID, groups)` in `internal/watchdog/policy.go:421-432` layers base -> each group in order -> host override, so precedence is `host > group > default`.
  - `internal/watchdog/watchdog.go`: copied `cfg.Groups` safely, added `Groups()` and `SetGroups(groups)` for thread-safe live re-resolution of the active profile against `w.groups`, and logged `groups` on startup.
  - `internal/panel/watchdog.go`: updated `wdHosts` (`GET /api/watchdog`) to call `pf.Effective(m.ID, m.Groups)` instead of `nil`, and added `Groups` + `EffectivePolicy` to `hostState`.
  - `internal/onboard/ssh.go`: added optional `Target.Groups`, validated in `validateTarget`, stored/preserved in `resolveMachine`, and rendered into `agent.watchdog.groups` in `renderAgentConfig`.
- **Assignment API & UI (`internal/panel/api.go`, `web/src/pages/MachinesPage.jsx`, `web/src/pages/WatchdogPage.jsx`):**
  - Added `POST /api/machines/{id}/groups` (`setMachineGroups`) in `internal/panel/api.go` validating/trimming group names, rejecting unknown/removed machines (`404`/`409`), updating the registry, and persisting atomically to `<DataDir>/registry/machines.json`.
  - Surfaced group badges and search filtering in `web/src/pages/MachinesPage.jsx` plus an inline Watchdog groups editor (add/remove/quick-pick from policy `overrides.groups`/save/reset) inside `MachineDetailsDrawer`.
  - Displayed per-host group chips, resolved effective-policy summary in the Hosts table, and a host selector on the Effective values tab in `web/src/pages/WatchdogPage.jsx`.
  - Rebuilt web bundle via `./web/build.sh` (`web/assets/dist/app.js` 493.7kb, `web/assets/dist/app.css` 39.8kb).
- **Tests & verification:**
  - Added unit/integration tests in `internal/registry/registry_test.go`, `internal/watchdog/watchdog_test.go`, `internal/onboard/ssh_test.go`, and `internal/panel/watchdog_test.go` (`TestWatchdogGroupsAssignmentPersistenceAndResolution`, covering assignment persistence across server restart and group/host effective-policy resolution).
  - `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` succeeded with `node` removed from `PATH`, and `./scripts/test.sh server` passed across all packages.

wf: T-0060 done
