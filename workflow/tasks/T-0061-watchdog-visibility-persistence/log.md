# T-0061 watchdog-visibility-persistence: log

## Entry 1 (doer)

- **Persistence location & restart survival:**
  - Chose `registry.Machine.WatchdogState` (`*registry.WatchdogState` in `internal/registry/registry.go`, serialized as `watchdog_state,omitempty` in `<DataDir>/registry/machines.json`), with deep-copying in `Machine.clone()` and validation in `Machine.Validate()`.
  - `internal/fleet/fleet.go`: added `regStore` (`Provisioner.Store` when set, else `registry.NewStore(cfg.DataDir)` when `cfg.DataDir != ""`), `ApplyWatchdogEvent`, `WatchdogEventFromRecord`, and `handleWatchdogEvent`. As `TypeWatchdogEv` events arrive (and pass deduplication against both the in-memory ring and `WatchdogState.LastEvent`), `handleWatchdogEvent` calls `recordWatchdogEvent` for the live stream/bus, folds the event into `mc.WatchdogState` (`Day`, `Stage`, `RestartsToday`, `RebootsToday`, `AlertOnly`, `Breaches`, `Throttled`, `Events`, `LastEvent`), and saves `machines.json` atomically via `registry.Store.Save`.
  - `internal/panel/watchdog.go`: refactored `deriveHost` to share `fleet.ApplyWatchdogEvent` + `hostStateFromWatchdogState` (no duplicated folding logic). `wdHosts` reads `m.WatchdogState` when present (keeping `RestartsToday`/`RebootsToday` scoped to today's local date when `ws.Day == today`) and falls back to `deriveHost(ob.WatchdogEvents(m.ID), ...)` when absent.
- **Monitor-agent watchdog visibility:**
  - `internal/fleet/fleet.go`: updated `agentHandler` (`TypeWatchdogEv`) to accept events from both `registry.RoleWorker` and `registry.RoleMonitor`.
  - `internal/panel/watchdog.go`: added `Role` and `Headless` fields on `hostState`, included `registry.RoleMonitor` machines in `wdHosts` (`GET /api/watchdog`, included in summary tiles; excluded from worker policy-push counters `workers`/`in_force` and remote counter reset) and `wdEvents` (`GET /api/watchdog/events`).
  - `web/src/pages/WatchdogPage.jsx` + rebuilt `web/assets/dist/app.js`: labeled monitor-agent rows as `Monitor agent (headless)` in the Hosts table and Events table, displayed `headless` in the Policy column, replaced the remote Reset counters button on monitor rows with a `Headless` label, and updated the summary/table notes.
- **Tests & verification:**
  - Updated `internal/fleet/watchdog_test.go` (`TestWatchdogEventsStoredUnderVerifiedIDAndDeduplicated` + added `TestWatchdogStatePersistedAndReloaded`), `internal/registry/registry_test.go` (`TestValidateRejects`, `TestStoreRoundTrip` with `WatchdogState`), `internal/panel/observe_stream_test.go` (`TestWatchdogLive` with `mon1` included), and added `internal/panel/watchdog_test.go` (`TestWatchdogMonitorAgentsVisible`, `TestWatchdogHostStatePersistedAcrossRestart`).
  - `./web/build.sh` rebuilt `web/assets/dist/app.js` (439.4kb) and `web/assets/dist/app.css` (39.8kb).
  - `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` passed with `node` removed from `PATH`, `./scripts/test.sh server` passed all packages.

wf: T-0061 done
