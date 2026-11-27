# T-0059 machine-details-view: log

## Entry 1 (doer)

- **Details endpoint (`internal/panel/api.go`, `internal/panel/machines.go`):**
  - Added `Machine(id string) (fleet.MachineInfo, bool)` to `panel.Backend` and registered `GET /api/machines/{id}` behind `s.live(...)` (returns `503` without backend, `404` JSON error for unknown machine IDs, and `200` with `{"machine": fleet.MachineInfo, "watchdog": *hostState}` without exposing secrets or private keys).
  - `machineWatchdogState` derives the worker's watchdog stage, condition, daily restart/reboot counters vs caps, policy ack status, and active breaches when an observe backend is bound (omitted for monitors and decommissioned machines).
- **Details drawer & live hook (`web/src/hooks/useMachineDetails.js`, `web/src/pages/MachinesPage.jsx`):**
  - Added `useMachineDetails(machineId, listMachine)`: renders immediately from the list's `MachineInfo` object, fetches `GET /api/machines/{id}` for watchdog stage + full snapshot, updates live via `/ws/events` (`machine` frames + reconnect) and `/ws/watchdog` (when watchdog is active), and ticks every 15s.
  - Replaced the `Details` row-menu stub toast with `MachineDetailsDrawer` (read-only slide-over side-panel accessible from both the row `Details` menu item and clicking the machine ID, with Escape/backdrop close, focus restoration, and selected-row highlight).
  - Displays machine ID, role, connection state + command queue counts, watchdog stage + ladder + condition + counters + policy, private & public IPs + `public_ip_at`, disk status + capacity meter + `disk.updated_at`, enrollment status + cert/revoked/SSH host key fingerprints, last-seen, host specs (when present), and registry timestamps.
- **Bundle & verification (`web/assets/dist/app.js`, `internal/panel/machines_test.go`):**
  - Added `TestMachinesPageHasDetailsDrawerInBundle`, `TestGetMachineDetailsEndpoint`, and `TestGetMachineDetailsWatchdog`.
  - `./web/build.sh` rebuilt `web/assets/dist/app.js` (453.9kb) and `web/assets/dist/app.css` (39.8kb).
  - `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` passed with `node` removed from `PATH`, and `./scripts/test.sh server` passed all packages.

wf: T-0059 done
