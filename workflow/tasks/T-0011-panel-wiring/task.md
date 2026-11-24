---
id: T-0011
title: panel-wiring
state: ready
touches: [internal/panel/server.go, internal/panel/templates.go, internal/panel/api.go,
          internal/panel/ws.go, internal/panel/api_test.go, web/_src/shell.html,
          internal/config/config.go, go.mod, go.sum]
tests: [server]
after: [T-0005, T-0006, T-0007]
shares: {}
model: {claude: "sonnet high"}
operator_check: "On http://localhost:6789: Machines shows the real registry; onboarding a machine from the form works; Command runner runs a one-shot on a worker agent with live streamed output and a confirmation gate on a dangerous command."
---

## Why

Turn the static panel into a live app for the **Operate** slice: serve it as Go `html/template` and
wire **Machines**, **Command runner**, and **Terminal** to real backend data (registry + executor over
the channel). The other pages (Monitor data, Watchdog, Evidence, Backup) wire as their backends land —
each is a later task.

## What to build

**Backend (Go), main-server side.**

1. **Templates** (`internal/panel/templates.go`): convert the stamped `web/` into Go `html/template` —
   the shell (`web/_src/shell.html`) becomes a layout, each page a `{{define "content"}}`, the active
   nav from a `.Page` field (the design note `workflow/design/panel.md` §6 describes this port). Keep the
   existing CSS/JS/assets. This supersedes T-0005's static serving.
2. **JSON API** (`internal/panel/api.go`): read/write endpoints for the Operate slice — list machines
   (from the registry, T-0006), onboard/remove/re-provision a machine, submit a one-shot command
   (executor, T-0007) with the confirmation token, fetch command history.
3. **WebSocket** (`internal/panel/ws.go`): live streams to the browser — machine online-state changes,
   command `queued→running→exited` + stdout/stderr chunks, and the interactive shell (xterm.js ⇄ this
   WS ⇄ the agent PTY).
4. **Wire the three Operate pages** to the above, replacing their mock data; keep the `data-stub`
   markers on anything not yet backed. Trust the forwarded auth header (no auth logic here).

## Out of scope

- Monitor data, Watchdog, Evidence, Backup page wiring (separate tasks as those backends land).
- Any Maneuver / Load Controller execution (deferred, D-0005) — leave that page a stub.
- New visual design — reuse the approved components; this is wiring, not a redesign.

## Done means

- The panel is served as `html/template` on `:6789`; all pages still render (light/dark) and the three
  Operate pages show **live** data.
- Machines reflects the real registry; onboarding from the form provisions a machine; remove/re-provision
  work.
- Command runner runs a one-shot on a selected worker agent with **live streamed output**, the serial
  queue is visible, and a dangerous command triggers the **confirmation gate** (enforced agent-side).
- Terminal opens an interactive shell to a worker agent (monitor agents are not selectable).
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes (httptest covers the API +
  template render; a WS test covers one command stream).

## Facts this brief relies on

- Panel is server-rendered Go `html/template` + xterm.js, served by `fleet-server` on `:6789`:
  `workflow/decisions/D-0001-stack.md`; `workflow/decisions/D-0004-deployment-basics.md`;
  `Fleet_Control_Monitoring_Platform_Architecture.md:549`.
- The front-end + its Go-port notes exist: `workflow/design/panel.md`; `workflow/tasks/T-0004-panel-frontend/task.md`.
- Registry + onboarding (T-0006) and executor + gate + PTY (T-0007):
  `workflow/tasks/T-0006-machine-registry-onboarding/task.md`; `workflow/tasks/T-0007-command-executor/task.md`.
- Static serving to build on (T-0005): `workflow/tasks/T-0005-serve-panel/task.md`.

## Notes for the doer

- This is broad; if it grows past one clean review, split by page (Machines first, then Command runner +
  Terminal) and tell the supervisor — do not silently drop scope.
- Keep `app.js` behavior; add `fetch`/WS only where a page goes live. Preserve the stamp workflow or
  replace it cleanly with templates (document which in the design note).
