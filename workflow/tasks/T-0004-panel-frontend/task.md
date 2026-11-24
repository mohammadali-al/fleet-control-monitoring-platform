---
id: T-0004
title: panel-frontend
state: ready
touches: [workflow/design/panel.md]
tests: []
after: [T-0001]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Open the static panel front-end in a browser: the nav and all pages render in light and dark; the Maneuver/Load-Controller page shows its layout (no functionality)."
---

> **Two-phase, design-first task.** Phase 1 = design, then **hand back and stop** for the
> supervisor's review. Phase 2 (coding the rest) begins only after the supervisor's `review-1.md`
> approves the design. New files under `web/` are free by default; the brief's `touches` names only
> the design note.

## Why

The operator panel is the whole product surface. We want its **front-end** designed and built as a
coherent, good-looking static shell now — **no backend wiring, no functionality** — so later tasks
drop real data into finished pages. The design must not be improvised; it is produced with the help
of a UI/UX reference skill and reviewed before the bulk is built.

## What to build

**Front-end only. Static/mock. No backend, no network calls, no real data.**

### Phase 1 — design (hand back for review)
1. **Use the UI/UX reference skill** at `https://github.com/nextlevelbuilder/ui-ux-pro-max-skill`
   (clone/read it and apply its method). This is **your** reference to use.
2. Produce a **design note** at `workflow/design/panel.md` (status `exploring`): information
   architecture for every page below, the visual system (layout grid, nav, color tokens for **light
   and dark**, typography, spacing, core components — tables, forms, buttons, status chips, charts
   placeholder, terminal placeholder), and how it maps to the architecture's vocabulary.
3. Build **2 proof pages** as real static HTML/CSS(/JS) under `web/`: the **Machines** page and the
   **Maneuver/Load-Controller** page (the latter a non-functional stub). Include light/dark.
4. **Hand back** a short report: the approach, the skill applied, screenshots or the sample paths, and
   any open design questions. **Do not build the rest yet.**

### Phase 2 — build out (after approval)
5. Build the remaining pages to match the approved design, all static/mock:

**Pages (architecture §12.2):** Machines · Projects & tests · Command runner · Terminal (xterm.js
placeholder) · Baseline items · **Maneuver editor + Load-Controller view (non-functional stub,
D-0005)** · Monitor data (charts placeholder / embedded-Grafana frame) · Watchdog · Evidence · Backup.

## Out of scope

- Any backend, API, data, auth UI (the panel sits behind the auth proxy; no login screen).
- Real xterm.js terminal behavior, real charts, real Grafana — use tasteful placeholders.
- **Any Load-Controller / Maneuver execution logic** (D-0005) — the Maneuver page is layout only.
- Wiring into `fleet-server`'s Go `html/template` serving — a later task; keep this a standalone
  static front-end under `web/` for now (note where it will later be served, `:6789`).

## Done means

- Phase 1 handed back with a design note + 2 proof pages, and approved in `review-1.md`.
- All pages above exist as static files under `web/`, navigable, responsive, with working **light and
  dark** modes.
- UI text is **English** (D-0003). Vocabulary matches the architecture: main server, worker agent,
  monitor agent, project, Maneuver, baseline item, evidence log, monitor data.
- The Maneuver/Load-Controller page renders its full layout and controls but does nothing (stub).
- No backend calls; opening any page from the filesystem (or a static server) works offline.

## Facts this brief relies on

- The panel is one web app, served by `fleet-server`, listening on **:6789** behind the auth proxy;
  the page list is architecture §12.2: `Fleet_Control_Monitoring_Platform_Architecture.md:549`, `:560`.
- Target integration is server-rendered Go `html/template` + xterm.js + embedded Grafana:
  `workflow/decisions/D-0001-stack.md`.
- UI language is English: `workflow/decisions/D-0003-ui-language.md`.
- Panel port `:6789`: `workflow/decisions/D-0004-deployment-basics.md`.
- The Load Controller / Maneuver execution is **deferred**; UI stub only:
  `workflow/decisions/D-0005-defer-load-controller.md`.

## Notes for the doer

- Keep it a clean static front-end under `web/` (your structure; document it in the design note). Plain
  HTML/CSS/vanilla JS or a tiny build — your call, but **no heavy framework** unless the design note
  justifies it; favor something that ports cleanly to Go `html/template` later.
- Design tokens in CSS variables; light/dark via a theme attribute. Accessible, keyboard-navigable.
- This is **visible work** (architecture §12.3 "visible-by-default"): every action/surface has a place
  in the UI even though it does nothing yet.
