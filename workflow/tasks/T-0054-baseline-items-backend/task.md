---
id: T-0054
title: baseline-items-backend
state: merged
touches: [internal/baseline/, internal/config/config.go, internal/panel/api.go, internal/panel/baseline.go,
          web/src/pages/BaselinePage.jsx, web/src/routes.js, web/src/hooks/, internal/panel/baseline_test.go]
tests: [server]
after: [T-0047]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Baseline items are a real, persisted global library: operators can create/edit/duplicate/delete reusable command templates (command_template + default inputs) via the panel, backed by a server store and CRUD API, with placeholder substitution into command_template. The Baseline page is a real React route (web/src/pages/BaselinePage.jsx), not a stub. Items are usable across all projects. Survives restart."
---

## Why

Baseline items are a stub page today with no backend (audit GAP-25; §9.2). A baseline item is a reusable
command template + default inputs, in a **global library** usable across all projects, with placeholder
substitution into `command_template`. Build it for real on the **React SPA** (D-0011) — this is the
foundation Maneuvers consume, and it **supersedes** the "port the baseline stub" scope of the T-0049 SPA
port task.

## What to build

**Backend:**
1. A persisted **baseline store** (`internal/baseline/`): a baseline item = id, name, `command_template`,
   default inputs/placeholders, metadata. Persist under the data dir (reload on start); global (not
   per-project).
2. **CRUD API** (`internal/panel/api.go` + `internal/panel/baseline.go`): list/create/update/duplicate/
   delete baseline items, behind the panel auth like other `/api/*` routes.
3. **Placeholder substitution engine:** render `command_template` with supplied inputs (the same engine
   Maneuvers will use to materialize an item into a concrete command). Define + validate placeholder
   syntax; reject unresolved placeholders.

**Front-end (React, per T-0047 pattern):**
4. `web/src/pages/BaselinePage.jsx` + register in `web/src/routes.js` (set its `ROUTES` entry
   `component: BaselinePage`, replacing the `null` legacy fallback): real UI for the library —
   list, create/edit form (name + template + default inputs), duplicate, delete, and a live
   substitution preview. Use the established data/hook pattern (`web/src/hooks/`, `web/src/lib/`). Rebuild
   the web bundle (`./web/build.sh`).

## Out of scope

- No Projects, Maneuver, or Load Controller work (separate tasks); no load patterns. Keep the item schema
  general enough for Maneuvers to consume but don't build Maneuver wiring here.

## Done means

- Baseline items persist and reload; CRUD + duplicate work via the API and the React BaselinePage; template
  substitution renders inputs and rejects unresolved placeholders; the library is global; survives restart.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` passes
  with store + API + substitution tests; web bundle rebuilt and committed.

## Facts this brief relies on

- Audit evidence: GAP-25 — no baseline backend exists; `internal/config/config.go:42` defines an unused
  `ProjectsDirName`; §9.2 L382-398 (`Architecture_Gap_Audit.md`). SPA route pattern: `web/src/routes.js`,
  `web/src/pages/MachinesPage.jsx`, `web/src/hooks/useMachines.js` (T-0047). Data-dir persistence pattern:
  existing `internal/registry` store.

## Notes for the doer

- Follow the T-0047 route-component contract exactly (one page file + one `routes.js` registration). Design
  the item schema + substitution engine so Maneuvers can reuse them. Rebuild + commit the web bundle. Run
  `./scripts/wf start T-0054` before committing; commit inside touches + the log. Do NOT push/merge/
  branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
