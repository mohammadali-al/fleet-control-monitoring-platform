# T-0051 administer-pages-react: log

## Entry 1 (doer)

- **Backup (`web/src/pages/BackupPage.jsx`):**
  - Ported the Backup page to a React SPA route component (`usePageTitle('Backup')`) inside the persistent shell, keeping all element IDs, classes, `data-f` fields, copy, and `aria-*` attributes at parity with `web/pages/backup.html` and `web/assets/js/live.js`.
  - Wired status and actions to the existing backup API:
    - `GET /api/backup` on mount, every 2s while a job is `running`, and every 30s in the background when visible; renders server details (`#bk-server` / `#bk-noserver`), last run & active panel job table (`#bk-runs`), schedule form (`#bk-sched-form`, `#bk-suggest`, `#bk-sched-next`), and daily reminder banner (`#bk-reminder`).
    - `GET /api/backup/diff` on initial load when configured, on job completion, and on `#bk-diff-btn` click; updates `#bk-reach` (`Reachable` / `Not reachable`), `#bk-diff-rows`, and `#bk-diff-foot`.
    - `POST /api/backup/run` (`#bk-run`), `POST /api/backup/import` (`#bk-import-open` + `#dlg-import` confirmation dialog with `confirm` and `overwrite`), `POST /api/backup/schedule`, `POST /api/backup/server` (`#bk-add-form`, clearing password after submit), and `POST /api/backup/reminder/dismiss` (`#bk-reminder-dismiss`).
- **Route registration & legacy fallback check (`web/src/routes.js`):**
  - Registered `BackupPage` for `/backup` in `ROUTES` (`component: BackupPage`), removing `/backup`'s `<LegacyFallback />` and sidebar `.html` link.
  - Not the final port: four routes in `ROUTES` still have `component: null` (`projects`, `baseline`, `maneuver`, `monitor`), so shared `<LegacyFallback />`, `web/pages/*.html`, and `internal/panel` template plumbing were left intact.
- **Bundle & tests (`web/assets/dist/app.js`, `web/assets/dist/app.css`, `internal/panel/server_test.go`):**
  - Rebuilt the SPA bundle via `./web/build.sh` (`web/assets/dist/app.js` 421.8kb, `web/assets/dist/app.css` 39.8kb).
  - Updated `internal/panel/server_test.go` (`TestServesPanel`, `TestSPAShellAndCatchAll`, `TestAdministerSPARouteInBundle`) to verify `/backup` serves the SPA shell and the bundle contains the Backup page copy and `/api/backup*` routes.
- **Verification:**
  - `./web/build.sh` succeeded.
  - `go build ./...` succeeded with `node` removed from `PATH`.
  - `gofmt -l .` empty; `go vet ./...` clean.
  - `./scripts/test.sh server` passed across all server packages including `internal/panel`.

wf: T-0051 done
