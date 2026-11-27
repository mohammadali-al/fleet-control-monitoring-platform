---
id: T-0048
title: operate-pages-react
state: ready
touches: [web/, internal/panel/server_test.go]
tests: [server]
after: [T-0047]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The Projects, Command runner, and Terminal pages are ported to React route components following the T-0047 foundation pattern: each renders inside the persistent SPA shell (no reload, rain keeps running), its client route works on deep-link/refresh, and its live behaviour works — Command runner streams command output live, Terminal runs an interactive xterm session, Projects renders its content. Their legacy .html full-load fallback is removed. No backend/API change."
---

## Why

T-0047 ports Machines as the reference; the remaining 9 pages still need porting from server-rendered HTML
to React route components. This task covers the **Operate** nav section: **Projects**, **Command runner**,
and **Terminal**. Terminal (vendored xterm) is the trickiest port, grouped here with two lighter pages.
**Front-end only — no backend/API change.**

## What to build

Following the route-component contract + patterns established by **T-0047** (read its reference Machines
component first), port each page to a React route rendered inside the persistent shell:

1. **Command runner** (`web/pages/commands.html` → React): the command form + **live output streaming**
   over the existing WS/SSE (`run`/`chunk`/`exit` envelopes via `/ws/events` / the run API), session
   lifecycle, dangerous-command confirmation — reuse the live-data hook pattern from the Machines port.
2. **Terminal** (`web/pages/terminal.html` → React): an interactive **xterm** session. **Reuse the
   vendored `web/assets/vendor/xterm/`** (do not replace/restyle it) — mount it in a React component,
   wire input/resize/output over the existing terminal WS, and clean up on unmount/route-change.
3. **Projects** (`web/pages/projects.html` → React): render its content as a route component (it is
   largely static today; keep parity, don't add features).

Register each on its client route (`/commands`, `/terminal`, `/projects`); remove their legacy full-load
fallback. Reuse the existing design-system CSS; preserve every `id`/`class`/`data-*` live logic depends on.

## Out of scope

- No backend/API/WS changes; no new features or redesign (same screens/controls); don't touch other
  sections' pages (T-0049/50/51) or the shell/router/build (that's T-0047); don't modify vendored xterm
  beyond wrapping/theming.

## Done means

- `/commands`, `/terminal`, `/projects` are React routes inside the persistent shell: no reload on nav, rain
  continuous, deep-link/refresh work. Command runner streams output live; Terminal runs a working xterm
  session (input/resize/output, clean teardown); Projects renders at parity. Legacy fallback removed.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes (tests assert the routes serve
  via the SPA shell). Manual pass of each page in both themes.

## Facts this brief relies on

- Foundation pattern + reference Machines component + route registry: `workflow/tasks/T-0047-panel-spa-foundation/task.md`
  and whatever it lands under `web/`. Legacy pages: `web/pages/{commands,terminal,projects}.html`. Live
  wiring today: `web/assets/js/live.js` (command streaming, xterm terminal) + `/ws/events` + the run/terminal
  APIs. Vendored terminal: `web/assets/vendor/xterm/`.

## Notes for the doer

- **Copy the T-0047 route-component contract exactly**; don't invent a second pattern. Terminal is the hard
  one — budget for it. Run `./scripts/wf start T-0048` before committing; commit inside touches + the log.
  Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
