---
id: T-0049
title: plan-pages-react
state: closed
canceled: "Superseded by the real React feature tasks (D-0012). Baseline items are built for real by T-0054 (baseline-items-backend); the Maneuver editor becomes its own React feature task (store + (ip,origin) scope + agent selection + patterns, Linear/Spikes disabled). Porting the Plan stub pages is throwaway, so this task is canceled by operator 2026-11-28."
touches: [web/, internal/panel/server_test.go]
tests: [server]
after: [T-0047]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The Baseline items and Maneuver pages are ported to React route components following the T-0047 foundation pattern: each renders inside the persistent SPA shell (no reload, rain keeps running), its client route works on deep-link/refresh, and parity with the current page is preserved. Legacy .html full-load fallback removed. No backend/API change."
---

## Why

Port the **Plan** nav section — **Baseline items** and **Maneuver** — from server-rendered HTML to React
route components, following the T-0047 foundation. Both are largely static/stub screens today (the Maneuver
nav item is marked "Stub"); keep parity, don't build new behaviour. **Front-end only.**

## What to build

Following the route-component contract from **T-0047** (read its reference Machines component first):

1. **Baseline items** (`web/pages/baseline.html` → React): render as a route component at parity.
2. **Maneuver** (`web/pages/maneuver.html` → React): render as a route component at parity (keep the
   existing stub/placeholder state and any `data-stub` controls as-is — do not wire new backend behaviour).

Register on client routes (`/baseline`, `/maneuver`); remove their legacy full-load fallback. Reuse the
design-system CSS; preserve every `id`/`class`/`data-*` existing logic/tests rely on.

## Out of scope

- No backend/API/WS changes; no new features (don't implement the deferred Maneuver/Load behaviour — this
  is a view port only); no redesign; don't touch other sections' pages or the shell/router/build.

## Done means

- `/baseline` and `/maneuver` are React routes inside the persistent shell: no reload on nav, rain
  continuous, deep-link/refresh work, parity preserved. Legacy fallback removed.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes (routes serve via the SPA
  shell). Manual pass in both themes.

## Facts this brief relies on

- Foundation pattern + reference component + route registry: `workflow/tasks/T-0047-panel-spa-foundation/task.md`.
  Legacy pages: `web/pages/{baseline,maneuver}.html`. Maneuver is a stub today (nav badge "Stub";
  `data-stub` controls).

## Notes for the doer

- **Copy the T-0047 route-component contract exactly.** This is the lightest of the four port tasks. Run
  `./scripts/wf start T-0049` before committing; commit inside touches + the log. Do NOT push/merge/
  branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
