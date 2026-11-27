---
id: T-0048
title: operate-pages-react
state: ready
touches: [web/, internal/panel/server_test.go]
tests: [server]
after: [T-0047]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The Command runner and Terminal pages are ported to React route components following the T-0047 foundation pattern: each renders inside the persistent SPA shell (no reload, rain keeps running), its client route works on deep-link/refresh, and its live behaviour works — Command runner streams command output live, Terminal runs an interactive xterm session. The wrong UI helper text (serial-FIFO / service-user) is corrected. Legacy .html fallback removed for these two. No backend/API change."
---

## Why

Port the **Operate** section pages that are real parity ports — **Command runner** and **Terminal** — from
legacy to React route components (per T-0047). (Projects is a stub with no backend; it is built for real as
a separate React feature task per D-0012, not a stub port, so it is **removed from this task**.) Terminal
(vendored xterm) is the tricky port. **Front-end only — no backend/API change.**

## What to build

Following the route-component contract from **T-0047** (read `web/src/routes.js` + the reference
`web/src/pages/MachinesPage.jsx` first), port each page to a React route under `web/src/pages/`, registered
in `web/src/routes.js` (set its `ROUTES` entry `component:` instead of `null`):

1. **Command runner → `web/src/pages/CommandsPage.jsx`:** the command form + **live output streaming**
   over the existing WS/SSE (`run`/`chunk`/`exit` via `/ws/events` / the run API), session lifecycle, and
   dangerous-command confirmation — reuse the live-data hook pattern (`web/src/lib/events.jsx`). **Fix
   GAP-10 copy:** the Command runner must not claim it "Runs as the agent's service user" (it runs as
   `root`, `internal/onboard/ssh.go` `User=root`).
2. **Terminal → `web/src/pages/TerminalPage.jsx`:** an interactive **xterm** session. **Reuse the vendored
   `web/assets/vendor/xterm/`** (don't replace/restyle it) — mount it in a React component, wire
   input/resize/output over the existing terminal WS, clean up on unmount/route-change. **Fix GAP-10
   copy:** drop the false "shares the machine's serial queue / starts when the machine is free" text — a
   shell (Mode B) is its own concurrent long-lived session (§4.6).

Remove the legacy full-load fallback for `/commands` and `/terminal`. Reuse the design-system CSS; preserve
every id/class/data-* live logic depends on. Rebuild the web bundle (`./web/build.sh`).

## Out of scope

- **Projects** (separate React feature task); no backend/API/WS changes; no new features/redesign; don't
  touch other sections' pages or the shell/router/build (T-0047); don't modify vendored xterm beyond
  wrapping/theming. Dangerous-command server-config (GAP-11) is a separate backend task, not this port.

## Done means

- `/commands` and `/terminal` are React routes inside the persistent shell: no reload on nav, rain
  continuous, deep-link/refresh work. Command runner streams output live; Terminal runs a working xterm
  session (input/resize/output, clean teardown); the GAP-10 copy is corrected. Legacy fallback removed.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` passes;
  web bundle rebuilt + committed. Manual pass of each page in both themes.

## Facts this brief relies on

- Foundation pattern + reference component + route registry: `web/src/routes.js`,
  `web/src/pages/MachinesPage.jsx`, `web/src/hooks/useMachines.js`, `web/src/lib/events.jsx` (T-0047).
  Legacy pages (reference for parity only): `web/pages/{commands,terminal}.html`. Vendored terminal:
  `web/assets/vendor/xterm/`. GAP-10 copy evidence: `Architecture_Gap_Audit.md` (terminal serial-FIFO /
  commands service-user vs `internal/onboard/ssh.go` `User=root`).

## Notes for the doer

- **Copy the T-0047 route-component contract exactly.** Terminal is the hard one — budget for it. Rebuild +
  commit the web bundle. Run `./scripts/wf start T-0048` before committing; commit inside touches + the log.
  Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
