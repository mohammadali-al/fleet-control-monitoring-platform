---
id: T-0034
title: panel-hacker-restyle-samples
state: ready
touches: [workflow/design/panel-hacker-samples/]
tests: []
after: []
shares: {}
model: {claude: "sonnet high"}
kind: design-spike
operator_check: "ROUND 5: the doer delivers EXACTLY 4 NEW standalone HTML samples (in workflow/design/panel-hacker-samples/round-5/) on the FIXED round-3 design system, all-in-one (light+dark toggle + browsable nav), differing ONLY in background — one REFINED Matrix-rain whose DARK-mode rain is grey/muted (not green), plus 3 more creative/professional backgrounds (no plain dots/grids), all restrained and legible. The USER judges; no pass/fail gate. Nothing is wired into the real panel."
---

## Why

Explore a visual re-skin of the web panel into "a bit hacker-style UI + a hacker-style font".
This is a **look-and-feel exploration only** — the point is to see options, not to ship one.

**ROUND 5.** Rounds 1–4 are done and reviewed in **`review.md` (READ THE ROUND-5 SECTION FIRST)**. The
round-3 light/dark pair is the **fixed** design system and the round-4 all-in-one format is kept. Round 5
refines the Matrix background and explores a few **more creative / professional** backgrounds — restrained.

## What to build

Produce **exactly 4 NEW** self-contained HTML samples (inline CSS; web fonts may load from Google Fonts;
inline JS allowed for the theme toggle, nav browsing, and animated backgrounds) under
**`workflow/design/panel-hacker-samples/round-5/`**, plus a short `README.md` in `round-5/` naming each
sample's background.

- **Keep all earlier rounds** untouched. Build on `round-4/` — reuse the fixed round-3 design system
  (Space Mono + Red Hat Mono, box-outlines/offset frames, components, layout, both palettes) and the
  all-in-one format (light+dark toggle + browsable top-nav) **unchanged**.
- **The ONLY thing that varies between the 4 files is the background.** Deliver:
  1. a **REFINED Matrix-rain** — same live canvas code-rain concept as `round-4/05-matrix-rain.html`, but
     the **dark-mode rain is grey / muted (monochrome), NOT green** (closer to the light-mode feel and the
     graphite/bone palette); light-mode consistent; low opacity, panel fully legible, `prefers-reduced-motion`
     respected.
  2–4. **three more creative or professional backgrounds** — richer and more considered than round-4's
     plain dots/grids (the operator found those too simple), e.g. subtle topographic contour lines, a faint
     blueprint, a slow constellation/particle field, a gentle signal/aurora gradient, or a soft isometric
     grid. Pick tasteful ones; **keep them subtle and restrained — do not overdo it**; content stays fully
     legible in both themes. Avoid plain dots/grids this round.
- **Each sample is ALL-IN-ONE:** a single file containing **BOTH light and dark** with an in-page
  **theme toggle**, and the panel's sections **browsable via the working top-nav**. The toggle + nav are
  **demo chrome** — minimal and unobtrusive; do not alter the panel's real controls/layout.

## Out of scope

- **No UX changes.** Keep the existing layout, information architecture, screens, nav and controls
  exactly as they are; change only the skin (colors, typography, borders, texture, small motion). Do not
  move, add, remove, rename, or re-flow any control or screen.
- **No touching the real app.** Do NOT modify anything under `web/` or any Go code; these are
  standalone comparison mockups, not an edit to the panel.
- Do not edit or delete any earlier-round files. No wiring, no build.

## Done means

- Exactly **4 new** standalone HTML samples + a README exist under
  `workflow/design/panel-hacker-samples/round-5/` and open correctly in a browser — all on the fixed
  round-3 design system + all-in-one format, differing ONLY in background: one refined Matrix-rain with
  **grey/muted dark-mode rain (not green)**, plus three more creative/professional backgrounds (no plain
  dots/grids), all restrained.
- Each file is ALL-IN-ONE: both light + dark via an in-page toggle, and the panel's sections browsable
  via the top-nav; backgrounds stay subtle enough that content is fully legible in both themes.
- **There is no acceptance gate.** The supervisor applies no pass/fail checks; the user alone judges the
  samples. "Done" = the 4 round-5 samples are committed.

## Facts this brief relies on

- The panel is server-rendered HTML templates under `web/pages/*.html` (the doer may glance at them to
  mirror the current layout faithfully, but must not edit them).
- This is a design spike; `tests: []` and no `after` deps — it depends on nothing and nothing depends
  on it.

## Notes for the doer

- You MAY glance at `web/pages/*.html` to reproduce the current layout/controls in the mock (so "no UX
  change" holds), but you are rebuilding the structure in a standalone file, not editing the app.
- **Read the ROUND-5 section of `workflow/tasks/T-0034-panel-hacker-restyle-samples/review.md` first**,
  and start from a `round-4/` file (e.g. `05-matrix-rain.html` for the refined Matrix one) — keep the
  whole design system + all-in-one format fixed and change ONLY the background per sample.
- Keep each file self-contained (works opened directly); inline JS is fine for the theme toggle, nav
  browsing, and any canvas/animated background; no JS frameworks.
- Run `./scripts/wf start T-0034` before committing; commit the 4 round-5 samples + README on the branch.
- Do NOT push, merge, delete/rename branches, remove worktrees, or edit decisions / other tasks /
  `workflow/generated/*` / `QUESTIONS.md` / any `state:` except via `wf start`. Report when committed
  (branch + commit hash + a one-line description of each sample).
