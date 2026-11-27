# T-0050 observe-pages-react: log

## Entry 1 (doer)

- **Watchdog (`web/src/pages/WatchdogPage.jsx`):**
  - Ported the Watchdog page to a React SPA route component (`usePageTitle('Watchdog')`) with live summary tiles (`#wd-n-normal`, `#wd-n-acting`, `#wd-n-alert`, `#wd-n-off`), per-host escalation stage/ladder/policy/counter table (`#wd-hosts`), policy YAML + effective values editor (`#wd-policy-form`, `#wd-yaml`, `#wd-effective`, `#wd-revert`), and events log (`#wd-events`).
  - Wired counter resets (`#wd-reset-all` and per-host `Reset counters` buttons) to `POST /api/watchdog/reset` with confirmation dialog and status banner (`#wd-status`), removing the legacy per-host `data-stub` marker.
  - Subscribed to `/ws/watchdog` (for `event` frames with exponential-capped reconnect and clean unmount teardown) and `/ws/events` via `useEvents()` (for `machine` online/offline updates and reconnect re-sync), plus a 15s relative-time tick.
- **Evidence (`web/src/pages/EvidencePage.jsx`):**
  - Ported the Evidence page to a React SPA route component (`usePageTitle('Evidence')`) with the three tabbed views (`Sessions`, `Sync (pull)`, `Export`), worker lists from `useMachines()`, and live `useEvents()` sync/session refreshes on `machine` and finished `command` events.
  - Wired the Sessions browser (`GET /api/evidence/sessions` with search/machine/kind filters), exact packet counts (removing the stale `≤ ` shared-segment prefix per T-0052), packet record preview (`GET /api/evidence/sessions/{machine}/{session}/records`) displaying decoded `payload_head` (`[data-payload-head]`) alongside payload status/bytes and SHA-256, Sync/Pull (`GET /api/evidence/sync`, `POST /api/evidence/pull`, 2s active-pull polling, archive disk meter `#ev-disk`, segment manifest `#ev-manifest`), and Export (`GET /api/evidence/export` for `pcapng`/`csv`/`json` across session/machine/range scopes + download history `#ev-exports`).
- **Route registration & shared helpers (`web/src/routes.js`, `web/src/lib/fmt.js`, `web/src/components/StatusAlert.jsx`, `web/assets/css/live.css`):**
  - Registered `WatchdogPage` (`/watchdog`) and `EvidencePage` (`/evidence`) in `ROUTES`, removing the `<LegacyFallback />` and `.html` full-load links for both while keeping `Monitor data` (`component: null`) untouched.
  - Added `fmtTimeMs`, `fmtDateTime`, and `decodePayloadHead` in `web/src/lib/fmt.js`, shared `StatusAlert` in `web/src/components/StatusAlert.jsx`, and `.payload-head` styling in `web/assets/css/live.css`.
- **Bundle & tests (`web/assets/dist/app.js`, `web/assets/dist/app.css`, `internal/panel/server_test.go`):**
  - Rebuilt the SPA bundle via `./web/build.sh` (`web/assets/dist/app.js` 402.4kb, `web/assets/dist/app.css` 39.8kb).
  - Updated `internal/panel/server_test.go` (`TestServesPanel`, `TestSPAShellAndCatchAll`, `TestObserveSPARoutesInBundle`) to verify `/watchdog` and `/evidence` serve the SPA shell and the built bundle includes the ported routes and `payload_head` wiring.
- **Verification:**
  - `./web/build.sh` succeeded cleanly.
  - `gofmt -l .` empty; `go vet ./...` clean.
  - `go build ./...` succeeded with `node` removed from `PATH`.
  - `./scripts/test.sh server` passed across all packages (`internal/panel` and all server packages passed).

wf: T-0050 done
