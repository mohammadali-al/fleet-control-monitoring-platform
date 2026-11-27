# T-0054 baseline-items-backend: log

## Entry 1 (doer)

- **Config (`internal/config/config.go`):**
  - Added `BaselineDirName = "baseline"` under the base data directory constants (`<DataDir>/baseline/`).
- **Global baseline store & placeholder substitution engine (`internal/baseline/`):**
  - `internal/baseline/template.go`: defined `{name}` placeholder syntax (`[a-zA-Z_][a-zA-Z0-9_-]*` with `{{`/`}}` literal brace escaping), `ParseTemplate`, `ValidateTemplate`, `Render`, and `RenderItem`. Performs single-pass substitution with defaults overlaid by caller inputs and rejects malformed templates (`ErrInvalidTemplate`) and missing/empty placeholders (`*UnresolvedPlaceholdersError`, wrapping `ErrUnresolvedPlaceholder`).
  - `internal/baseline/store.go`: implemented global persisted `Store` (`<DataDir>/baseline/items.json`, `formatVersion = 1`) with atomic temp-file + `fsync` + `rename` writes, reload on `Open`, and CRUD + `Duplicate` operations (`Item`: `id`, `name`, `description`, `command_template`, `placeholders`, `defaults`, `warmup_sec`, `tags`, `metadata`, `created_at`, `updated_at`, `created_by`, `updated_by`).
- **Panel HTTP API (`internal/panel/baseline.go`, `internal/panel/api.go`):**
  - Registered `/api/baseline` routes behind `s.live(...)`: `GET /api/baseline`, `POST /api/baseline`, `POST /api/baseline/preview`, `GET /api/baseline/{id}`, `PUT /api/baseline/{id}`, `POST /api/baseline/{id}/duplicate`, and `DELETE /api/baseline/{id}`.
  - Resolves the store from `s.be` (`Baseline()`, `DataDir()`, or the `observer` data directory via `Jobs.Dir()` / `Archive.Root()`) so `fleet-server` persists global baseline items without modifying out-of-touches packages.
  - Returns `422 Unprocessable Entity` with `unresolved` placeholder list when previewing a template with missing inputs, `400` on invalid syntax/fields, `404` when missing, `409` on duplicate custom ID, and `503` when no backend is configured.
- **React SPA (`web/src/hooks/useBaseline.js`, `web/src/pages/BaselinePage.jsx`, `web/src/routes.js`, `web/assets/dist/app.js`):**
  - Added `useBaseline` hook (`GET /api/baseline`, CRUD, duplicate, live preview, WS reconnect refresh).
  - Implemented `BaselinePage` with library search/list, create/edit form (`name`, `command_template`, `description`, `warmup_sec`, `tags`, dynamic per-placeholder default inputs), duplicate, delete confirmation dialog, and debounced live substitution preview showing rendered output or unresolved-placeholder rejections.
  - Registered `BaselinePage` in `web/src/routes.js` (`component: BaselinePage`) and rebuilt the bundle via `./web/build.sh` (`assets/dist/app.js` 438.9kb, `assets/dist/app.css` 39.8kb).
- **Tests & verification:**
  - `internal/baseline/baseline_test.go`: template validation, escaped braces, single-pass substitution, unresolved placeholder rejection, store CRUD/duplicate/persistence/reload, corrupt JSON handling.
  - `internal/panel/baseline_test.go`: no-backend `503`, non-JSON `415`, CRUD + duplicate + preview (`200` & `422` unresolved) + restart reload against `fleet.Open` + `EnableObserve`, and `/baseline` SPA route + bundle checks.
  - `gofmt -l .` empty; `go vet ./...` clean; `go build ./...` succeeded with `node` removed from `PATH`; `./scripts/test.sh server` passed; `go test -race ./internal/baseline/... ./internal/panel/...` passed.

wf: T-0054 done
