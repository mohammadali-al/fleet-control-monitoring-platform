# T-0048 operate-pages-react: log

## Entry 1 (doer)

- **Command runner (`web/src/pages/CommandsPage.jsx`):**
  - Ported the Command runner view into the React SPA route component pattern (`usePageTitle('Command runner')`, `useMachines()` for worker targets + live machine updates, `useEvents()` on `/ws/events` for `chunk` base64 UTF-8 streaming via `TextDecoder` and `command` `oneshot` `queued`/`running`/`exited` lifecycle transitions, plus reconnect re-sync).
  - Preserved all form/card/dialog/history IDs, classes, and `data-*` attributes (`#run-form`, `#targets`, `#cmd`, `#to`, `#runs`, `#history-rows`, `#dlg-gate`, `#gate-form`, `data-run-*`, `data-gate-*`, etc.), target selection (`Select all online` / `Clear`), history filtering, `Re-run` action, run cancellation (`POST /api/commands/{session}/cancel`), and the dangerous-command confirmation modal (`409 needs_confirmation` -> `I understand` -> `confirm: true`).
  - **GAP-10 copy fix:** replaced `"Runs as the agent's service user on each selected worker agent."` with `"Runs as root on each selected worker agent."`
- **Terminal (`web/src/pages/TerminalPage.jsx` + `web/src/lib/xterm.js`):**
  - Added `ensureXterm()` in `web/src/lib/xterm.js` to load the self-hosted vendored `web/assets/vendor/xterm/{xterm.css,xterm.js,addon-fit.js}` once per SPA session without modifying vendored xterm files or bloating `app.js`.
  - Ported the interactive Terminal page (`usePageTitle('Terminal')`), mounting each session's `window.Terminal` + `FitAddon` inside `<TerminalTabPanel />`, wiring binary PTY I/O, resize (`cols`/`rows`), dangerous-line confirmation (`#dlg-tgate` -> `{ type: 'confirm', line }`), theme updates (`--term-bg` / `--term-fg`), tab switching/keyboard navigation, copy/terminate controls, active PTY sessions list (`GET /api/commands?mode=pty&active=1`), and `?machine=<id>` deep-link handling.
  - On tab close or route-change unmount, each `<TerminalTabPanel />` removes its resize listener, disposes xterm event subscriptions and `Terminal.dispose()`, and closes its `/ws/shell` WebSocket so the backend cancels the PTY session cleanly.
  - **GAP-10 copy fix:** dropped the false `"shares the machine's serial queue / starts when the machine is free"` text and updated the subtitle to describe Mode B shells as concurrent long-lived sessions.
- **Route registration & shared formatting (`web/src/routes.js`, `web/src/lib/fmt.js`):**
  - Registered `CommandsPage` (`/commands`) and `TerminalPage` (`/terminal`) in `ROUTES`, removing the SPA `<LegacyFallback />` and full-load `.html` sidebar links for these two routes while leaving `Projects` (`component: null`) untouched.
  - Added `fmtTime` and `fmtDur` to `web/src/lib/fmt.js`.
- **Bundle & tests (`web/assets/dist/app.js`, `internal/panel/server_test.go`):**
  - Rebuilt the SPA bundle via `./web/build.sh` (`web/assets/dist/app.js` 353.4kb, `web/assets/dist/app.css` 39.6kb, `web/dist/index.html`).
  - Updated `internal/panel/server_test.go` (`TestServesPanel`, `TestSPAShellAndCatchAll`, `TestOperateSPARoutesInBundle`) to verify `/commands` and `/terminal` serve the SPA shell and the built bundle carries the ported wiring and corrected GAP-10 copy.
- **Verification:**
  - `./web/build.sh` succeeded cleanly.
  - `gofmt -l .` empty; `go vet ./...` clean.
  - `go build ./...` succeeded with `node` removed from `PATH`.
  - `./scripts/test.sh server` passed (`cmd/fleet-server` and `internal/panel` ran uncached and passed).

wf: T-0048 done
