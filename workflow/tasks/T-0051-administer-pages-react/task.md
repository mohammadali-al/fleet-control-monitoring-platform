---
id: T-0051
title: administer-pages-react
state: ready
touches: [web/, internal/panel/server_test.go]
tests: [server]
after: [T-0047]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The Backup page is ported to a React route component following the T-0047 foundation pattern: it renders inside the persistent SPA shell (no reload, rain keeps running), its client route works on deep-link/refresh, and its behaviour (backup list/status/actions) works at parity. Legacy .html fallback removed. With this merged, every panel page is a React route and no .html routes remain. No backend/API change."
---

## Why

Port the **Administer** nav section — **Backup** — from server-rendered HTML to a React route component,
following T-0047. This is the last page; once merged, the whole panel is a React SPA and no `.html` routes
remain. **Front-end only.**

## What to build

Following the route-component contract from **T-0047** (read its reference Machines component first):

1. **Backup** (`web/pages/backup.html` → React): the backup list, status, and any trigger/download
   actions, wired to the existing backup API at parity.

Register on its client route (`/backup`); remove its legacy full-load fallback. **This is the final port**
— also remove the now-unused legacy full-load fallback path / leftover `web/pages/*.html` + template
plumbing that T-0047 left in place for the transition (confirm no route still depends on it), so only the
SPA remains. Reuse the design-system CSS; preserve every `id`/`class`/`data-*` existing logic/tests rely on.

## Out of scope

- No backend/API/WS changes; no new features or redesign; don't touch other sections' pages before they
  are ported (if any port task is still outstanding, DO NOT remove its legacy fallback — only remove
  plumbing for routes that are fully ported). Don't change the shell/router/build (T-0047).

## Done means

- `/backup` is a React route inside the persistent shell: no reload on nav, rain continuous,
  deep-link/refresh work, parity preserved.
- If T-0048–T-0050 are merged, the legacy server-rendered page plumbing is removed and **no `.html` routes
  remain**; otherwise only `/backup`'s fallback is removed (note what was left for still-pending ports).
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes. Manual pass in both themes.

## Facts this brief relies on

- Foundation pattern + reference component + route registry + the transitional legacy fallback:
  `workflow/tasks/T-0047-panel-spa-foundation/task.md`. Legacy page: `web/pages/backup.html`; template
  plumbing in `internal/panel/templates.go` / `server.go`; embedding in `web/embed.go`.

## Notes for the doer

- **Copy the T-0047 route-component contract exactly.** Be careful removing legacy plumbing — only for
  fully-ported routes; if other port tasks are still open, leave their fallback intact and say so. Run
  `./scripts/wf start T-0051` before committing; commit inside touches + the log. Do NOT push/merge/
  branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
