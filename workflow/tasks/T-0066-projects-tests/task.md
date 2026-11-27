---
id: T-0066
title: projects-tests
state: ready
touches: [internal/project/, internal/config/config.go, internal/fleet/, internal/panel/api.go,
          internal/panel/project.go, web/src/pages/ProjectsPage.jsx, web/src/routes.js, web/src/hooks/,
          internal/panel/project_test.go]
tests: [server]
after: [T-0063]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Projects group tests, and a test = a project + a Maneuver + a monitor. Operators can create projects, create/start/stop a test (starting it runs the test's Maneuver and attaches/creates its monitor), and only ONE test may be active across the whole system at a time (a system-wide lock, enforced). Completed tests are listed by date within each project. Real React Projects page, not a stub."
---

## Why

Projects/tests are a static stub with no backend (audit GAP-24; §9.1 L376-378: "a project groups tests …
only one test may be active across the whole system at a time … completed tests are listed by date within
each project"). Build it for real on the React SPA (D-0011), on top of the Maneuver stack (T-0063) — a test
runs a Maneuver. This also unblocks the report generator (T-0062).

## What to build

**Backend (`internal/project/`, `internal/panel/project.go`, `internal/config/config.go`, `internal/fleet/`):**
1. A persisted **project/test store**: a Project = name + metadata + its tests; a **Test** = reference to a
   **Maneuver** (T-0063) + a **monitor** + status (planned/running/completed/failed) + timestamps +
   result pointers (evidence window + monitor window, for the later report). Use the unused
   `ProjectsDirName` (`internal/config/config.go:42`). CRUD API behind `internal/panel/api.go` (`s.live`).
2. **Single active-test lock (system-wide):** starting a test acquires a global lock; a second start is
   refused while one is active. **Start** runs the test's Maneuver (via T-0063's run path) and attaches or
   creates its monitor; **Stop** ends the Maneuver + records completion. Completed tests are listed by date
   per project.

**Front-end (React, per T-0047 contract):**
3. `web/src/pages/ProjectsPage.jsx` + register `component: ProjectsPage` in `web/src/routes.js`: projects
   list, create project, per-project tests (create/start/stop), the active-test indicator + lock feedback,
   and the completed-tests-by-date list. (A test needs a monitor — attach/select one on start; the
   deferred confirm-to-start dialog, GAP-20, is out of scope under the safe-operator assumption.) Rebuild +
   commit the web bundle (`./web/build.sh`).

## Out of scope

- No Maneuver editor/execution (T-0063 owns it — this consumes its run path); no report generation
  (T-0062, unblocked by this); no Load Controller; no confirm-to-start dialog (GAP-20, deferred).

## Done means

- Projects + tests persist; a test references a Maneuver + monitor; **starting a test runs its Maneuver +
  attaches its monitor**, and the **system-wide single-active-test lock** refuses a concurrent start;
  completed tests list by date; the React Projects page drives all of it.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` passes
  with tests for store persistence, the active-test lock (concurrent-start refused), and start→Maneuver-run
  wiring. Web bundle rebuilt + committed.

## Facts this brief relies on

- Audit: GAP-24 §9.1 L376-378 (`web/pages/projects.html` stub; `internal/config/config.go:42`
  `ProjectsDirName` unused). Maneuver run path to consume: T-0063 (`internal/maneuver/`, its run/dispatch).
  SPA route contract: `web/src/routes.js`, `web/src/pages/MachinesPage.jsx` (T-0047). Data-dir store
  pattern: `internal/registry` / `internal/baseline` (T-0054).

## Notes for the doer

- Reuse the Maneuver run API from T-0063; don't reimplement execution. Enforce the active-test lock on the
  server (not just UI). Rebuild + commit the bundle. Run `./scripts/wf start T-0066` before committing;
  commit inside touches + the log. Do NOT push/merge/branch-delete or edit decisions/generated/other
  tasks/`state:` (except via `wf start`).
