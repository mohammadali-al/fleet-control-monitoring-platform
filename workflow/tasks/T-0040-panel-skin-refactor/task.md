---
id: T-0040
title: panel-skin-refactor
state: merged
touches: [web/_src/shell.html, web/pages/backup.html, web/pages/baseline.html, web/pages/commands.html,
          web/pages/evidence.html, web/pages/fragments.html, web/pages/machines.html, web/pages/maneuver.html,
          web/pages/monitor.html, web/pages/projects.html, web/pages/terminal.html, web/pages/watchdog.html,
          web/styleguide.html, web/assets/, web/embed.go,
          internal/panel/server_test.go, internal/panel/machines_test.go, internal/panel/observe_stream_test.go,
          internal/panel/watchdog_test.go]
tests: [server]
after: [T-0023]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Every real panel page renders in the round-5 skin (Space Mono + Red Hat Mono, box-outlines/offset frames, dark palette), DEFAULTS TO DARK, has a client-side theme toggle persisted in localStorage (no server state, no flash on reload), and shows the grey Matrix-rain as a swappable background layer on all pages. All existing panel behaviour + live updates still work and panel tests pass. NO backend/Go-logic change."
---

## Why

Re-skin the real web panel to the chosen design (D-0010) from
`workflow/design/panel-hacker-samples/round-5/`, with **dark as the default theme** and a **client-side,
localStorage-persisted** theme toggle (not server state). The samples are the **visual guide** — the
overall atmosphere/patterns to copy — but they only mock a representative screen, so some real pages /
components they didn't show must still be fully covered. **Front-end only: no backend change.**

## What to build

**Front-end refactor of the real panel (`web/`), guided by `round-5/` — copy its patterns, not its files.**

1. **Base layout (`web/_src/shell.html`):**
   - Default theme **dark** (`<html data-theme="dark">`), with an **inline head script** that reads
     `localStorage.getItem('fleet-theme')` and applies it **before first paint** (no FOUC), falling back
     to dark. The theme toggle writes `localStorage` and flips `data-theme` **client-side only** — no
     server round-trip, no cookie, no Go change. Wrap storage access in try/catch.
   - Add the **background as a shared, swappable layer** present on every page: default = the grey/muted
     **Matrix-rain** (from `round-5/01-matrix-rain.html`) on a `pointer-events:none`, `aria-hidden`
     `<canvas>`, DPR-capped, reduced-motion → static frame, and **paused on `document.hidden`**
     (visibilitychange) to save battery. Keep it a single swappable module (e.g. a `data-bg` attr / one
     JS+CSS unit) so the other sample backgrounds can be switched in later.
2. **Design system across ALL pages** (`web/pages/*.html` + `fragments.html` + `styleguide.html`): apply
   the round-5 fonts (**self-hosted** Space Mono + Red Hat Mono under `web/assets/` — **do NOT fetch from
   Google Fonts**; the panel must work offline with no external calls), box-outlines + offset-shadow
   frames, spacing, and the light/dark palettes. Cover **every** page and every component the real panel
   has (nav, stat tiles, tables, tabs, dialogs, forms, the xterm terminal chrome, watchdog policy editor,
   evidence, backup, maneuver, monitor charts) — including anything the samples omitted.
3. **Assets (`web/assets/`):** the self-hosted font files, the shared skin CSS, and the background module
   JS. Update `web/embed.go` only if new asset paths need embedding.

## Out of scope

- **No backend / Go-logic change:** do not touch panel handlers, routes, `templates.go`, `server.go`, or
  any non-front-end Go. Only `*_test.go` assertions that check cosmetic page markup may be updated to
  match the new skin (keep every functional hook the live JS relies on).
- No new pages/features, no layout/IA/UX changes (this is a restyle — same screens, same controls, same
  behaviour). The deferred Load Controller UI stays as-is.
- Do not restyle or edit vendored code under `web/assets/vendor/` (e.g. xterm.js) beyond wrapping/theming.

## Done means

- All 11 pages + fragments render in the round-5 skin; **default is dark**; the toggle persists via
  `localStorage` and there is **no light-flash on reload**; the grey Matrix-rain shows on every page as a
  swappable layer that doesn't block clicks and pauses when the tab is hidden.
- **Nothing functional breaks:** live SSE/WS updates, xterm terminals, forms/endpoints, dialogs, tabs,
  and Grafana embeds still work; **no backend change**; fonts are self-hosted (no external requests).
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes (panel still serves every
  page; update only cosmetic test assertions, preserving functional ids/hooks). A quick manual pass of
  each page in both themes.

## Facts this brief relies on

- Panel is server-rendered `html/template`: `web/_src/shell.html` (base) + `web/pages/*.html` +
  `web/pages/fragments.html`, parsed per page in `internal/panel/templates.go`; assets + live JS under
  `web/assets/` (incl. vendored `web/assets/vendor/xterm/`); embedded via `web/embed.go`.
- The chosen skin + dark default + client-side localStorage theme + grey-matrix default background:
  `workflow/decisions/D-0010-panel-skin.md`; `workflow/design/panel-hacker-samples/round-5/` (+ its
  `README.md`); the earlier round READMEs for the fonts/outline/palette system.
- Pages/live wiring that must keep working: `workflow/tasks/T-0023-serve-panel/task.md` and the panel
  feature tasks (machines, watchdog, monitor, evidence, terminal, backup).

## Notes for the doer

- **Broad — split in logical commits:** (a) base layout + theme (dark default + localStorage, no flash)
  + self-hosted fonts + the swappable background layer; (b) then page-by-page skinning; commit and report
  the split. Preserve every id / class / `data-*` the existing JS + tests depend on; reskin around them.
- The samples are **reference only** — reproduce their look in the real templates; never wire a sample
  file into the app. Run `./scripts/wf start T-0040` before committing; commit inside touches + the log.
  Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via
  `wf start`). If a backend hook genuinely must change, STOP and report rather than editing backend Go.
