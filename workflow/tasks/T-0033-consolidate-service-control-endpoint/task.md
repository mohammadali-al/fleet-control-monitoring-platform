---
id: T-0033
title: consolidate-service-control-endpoint
state: merged
touches: [internal/panel/machines.go, internal/panel/machines_servicecontrol_test.go, internal/panel/api.go]
tests: [server]
after: [T-0031]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Stop/Start on the Machines page still works for workers and monitors; there is exactly ONE server endpoint behind it (the enrollment path), and the duplicate /api/machines/{id}/stop|start routes are gone."
---

## Why

T-0031 left **two** server paths for the same action: the live enrollment endpoints
`/api/enroll/machines/{id}/stop|start` (what the Machines-page UI actually calls, wired to
`Manager.ServiceControl`), and a second, **unused** set `/api/machines/{id}/stop|start` +
`/api/machines/{id}/service-status` that T-0031 added in `panel/machines.go` (`routesMachines`,
mounted in `api.go`). Dead, duplicated surface. Collapse to one.

## What to build

- **Remove** the unused `routesMachines` handlers from `internal/panel/machines.go` (the
  `/api/machines/{id}/stop`, `/start`, `/service-status` routes) and **unmount** them from
  `routesAPI` in `internal/panel/api.go`.
- Keep everything else in `machines.go` (the Machines page handler, enrolled-agents table) intact.
- Update/trim `internal/panel/machines_servicecontrol_test.go`: drop the cases that exercised the
  removed `/api/machines/...` routes; **keep** coverage that Stop/Start works for a worker and a
  **monitor** (no 501) — if that coverage only existed against the removed routes, retarget it to the
  surviving enrollment path (or confirm the enrollment-path test already covers both roles and remove
  the now-redundant file).
- The surviving path is the enrollment one (`internal/enroll`), which this task does **not** modify.

## Out of scope

- The enrollment Stop/Start endpoints and `Manager.ServiceControl` (T-0031, unchanged); any UI change
  (the UI already calls the surviving path).

## Done means

- No `/api/machines/{id}/stop|start|service-status` routes remain; `grep` for them is empty.
- Stop/Start coverage for worker + monitor still passes (via the enrollment path).
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes; the panel still serves all pages.

## Facts this brief relies on

- T-0031 added the duplicate routes and wired the enrollment path; the UI calls the enrollment path:
  `workflow/tasks/T-0031-agent-service-control/task.md` and its log + `wiring-enroll-servicecontrol.patch`.

## Notes for the doer

- Pure removal + test trim; no behavior change for the user. If removing the routes orphans a helper in
  `machines.go`, remove that helper too (still inside touches).
