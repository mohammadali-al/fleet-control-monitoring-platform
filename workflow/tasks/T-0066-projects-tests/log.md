# T-0066 projects-tests: log

## Doer entry

- **Backend store & config (`internal/config/config.go`, `internal/project/`):**
  - Added `ProjectsFileName = "projects.json"` inside `config.ProjectsDirName` (`projects/`).
  - Created `internal/project/` (`project.go`, `store.go`, `project_test.go`) with atomic JSON persistence (`temp + fsync + rename`), Project (`id`, `name`, `description`, `metadata`, `tests`), Test (`id`, `project_id`, `name`, `description`, `maneuver_id`, `maneuver_name`, `target`, `agents`, `monitor`, `status` [`planned`/`running`/`completed`/`failed`], `outcome`, `error`, `maneuver_run_id`, timestamps, actors, `result_pointers` with `evidence` + `monitor` windows), `ActiveTestLock`, and `CompletedByDate` grouping.
  - Server-side global lock (`BeginStart` / `AttachRun` / `AttachMonitor` / `RollbackStart` / `FinishTest` / `FinishTestByRunID`) guarantees only one test can be active across the system at a time.
- **Fleet execution & monitor wiring (`internal/fleet/fleet.go`, `internal/fleet/maneuver.go`, `internal/fleet/project.go`, `internal/fleet/project_test.go`):**
  - Opened `project.Store` on `fleet.Manager` when `DataDir` is set and exposed `ProjectStore()`.
  - Implemented `StartProjectTest` (acquires global lock, attaches an existing monitor job or finds/creates a monitor job for the Maneuver's `(ip, origin)` and pushes monitor configs, then starts the Maneuver via T-0063's `StartManeuver`; rolls back lock and newly created monitor job on synchronous failure) and `StopProjectTest` (stops the Maneuver via `StopManeuver`, records completion + closes result pointer windows, releases lock).
  - Wired `publishManeuver` -> `syncProjectTestFromRun` so terminal Maneuver transitions (`completed`, `stopped`, `aborted`) automatically finalize the active project test and release the lock.
- **Panel API (`internal/panel/api.go`, `internal/panel/project.go`, `internal/panel/project_test.go`):**
  - Registered `/api/projects`, `/api/projects/active`, `/api/projects/{id}`, `/api/projects/{id}/tests`, `/api/projects/{id}/tests/{testId}`, `/api/projects/{id}/tests/{testId}/start`, `/api/projects/{id}/tests/{testId}/stop` behind `s.live`.
  - Refuses concurrent start with `409 Conflict` (`active_test` payload) and missing monitor choice with `400 Bad Request`.
- **Front-end & bundle (`web/src/hooks/useProjects.js`, `web/src/pages/ProjectsPage.jsx`, `web/src/routes.js`, `web/assets/dist/app.js`):**
  - Created `useProjects` hook (projects/active/maneuvers/runs/monitorJobs + live `/ws/events` updates) and `ProjectsPage` (projects list + search, create/edit/delete project with metadata, per-project planned/running tests, Start test monitor attach/create dialog, global active-test lock banner + Stop button, and completed-tests-by-date table with result filter and evidence/monitor window links).
  - Registered `component: ProjectsPage` in `web/src/routes.js` and rebuilt the bundle via `./web/build.sh` (`assets/dist/app.js` 563.0kb, `assets/dist/app.css` 39.8kb).
- **Verification:**
  - `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` passed without `node` in `PATH`, and `./scripts/test.sh server` passed (including `internal/project`, `internal/fleet`, and `internal/panel` tests for store persistence, active-test lock concurrency refusal, start->Maneuver-run + monitor attach wiring, and SPA bundle route coverage).

wf: T-0066 done
