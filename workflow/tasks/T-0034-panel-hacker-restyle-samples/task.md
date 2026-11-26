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
operator_check: "ROUND 3: the doer delivers EXACTLY 2 NEW standalone HTML samples (a matched LIGHT and DARK pair, in workflow/design/panel-hacker-samples/round-3/) built as ONE design system from round-2 winners — sample-2's fonts (Space Mono + Red Hat Mono) and box-outlines for BOTH, light=sample-2 palette, dark=sample-5 palette. Same layout/controls. The USER judges; no pass/fail gate. Nothing is wired into the real panel."
---

## Why

Explore a visual re-skin of the web panel into "a bit hacker-style UI + a hacker-style font".
This is a **look-and-feel exploration only** — the point is to see options, not to ship one.

**ROUND 3.** Rounds 1–2 are done and reviewed in **`review.md` (READ THE ROUND-2 SECTION FIRST)**. The
operator picked **round-2 sample-2** (Daylight Technical Broadsheet, light) and **round-2 sample-5**
(Swiss Brutalist Monochrome + Signal Red, dark) as the winners, to become the **light** and **dark**
theme of one design system. This round builds that pair.

## What to build

Produce **exactly 2 NEW** self-contained static HTML samples (inline CSS; web fonts may load from Google
Fonts) under **`workflow/design/panel-hacker-samples/round-3/`**:

- `light.html` and `dark.html`, plus a short `README.md` in `round-3/` describing the shared design
  system and the two themes.
- **Keep all earlier rounds** (`workflow/design/panel-hacker-samples/sample-1..3.html` and `round-2/`)
  untouched for reference. Look at `round-2/sample-2.html` and `round-2/sample-5.html` as the basis.

Both render the **same representative slice of the panel** (top nav/header + the Machines/Operate view
with a few mock rows, a couple of stat tiles, and a terminal-style panel) using **mock data**.

Build them as ONE design system (per review.md round-3 directive):
- **Fonts for BOTH = round-2 sample-2's fonts** (Space Mono + Red Hat Mono). Do not use sample-5's font.
- **Box outlines / borders for BOTH = round-2 sample-2's treatment** (its carbon-ink rules + offset
  block-shadow frames), applied identically to light and dark.
- **`light.html` = sample-2's warm-paper palette; `dark.html` = sample-5's black / bone-white +
  signal-red palette.** Same layout, same components, same border/typography system — **only the theme
  colors differ**, so the two read as light/dark of the same UI.
- Keep the strengths of sample-2 and sample-5.

## Out of scope

- **No UX changes.** Keep the existing layout, information architecture, screens, nav and controls
  exactly as they are; change only the skin (colors, typography, borders, texture, small motion). Do not
  move, add, remove, rename, or re-flow any control or screen.
- **No touching the real app.** Do NOT modify anything under `web/` or any Go code; these are
  standalone comparison mockups, not an edit to the panel.
- Do not edit or delete the round-1/round-2 files. No wiring, no build.

## Done means

- Exactly **2 new** standalone HTML samples (`light.html`, `dark.html`) + a README exist under
  `workflow/design/panel-hacker-samples/round-3/` and open correctly in a browser — a matched light/dark
  pair of ONE design system: both using round-2 sample-2's fonts (Space Mono + Red Hat Mono) and its
  box-outline treatment, light on sample-2's palette and dark on sample-5's palette, same layout/controls
  as today, mock data only.
- **There is no acceptance gate.** The supervisor applies no pass/fail checks; the user alone judges the
  samples. "Done" = the 2 round-3 samples are committed.

## Facts this brief relies on

- The panel is server-rendered HTML templates under `web/pages/*.html` (the doer may glance at them to
  mirror the current layout faithfully, but must not edit them).
- This is a design spike; `tests: []` and no `after` deps — it depends on nothing and nothing depends
  on it.

## Notes for the doer

- You MAY glance at `web/pages/*.html` to reproduce the current layout/controls in the mock (so "no UX
  change" holds), but you are rebuilding the structure in a standalone file, not editing the app.
- **Read the ROUND-3 section of `workflow/tasks/T-0034-panel-hacker-restyle-samples/review.md` first**,
  and open `round-2/sample-2.html` + `round-2/sample-5.html` (the winners) — reuse sample-2's fonts and
  box-outlines for both, light=sample-2 palette, dark=sample-5 palette.
- Keep each file self-contained (works opened directly); tiny inline JS only if it aids the demo; no JS
  frameworks.
- Run `./scripts/wf start T-0034` before committing; commit the 2 round-3 samples + README on the branch.
- Do NOT push, merge, delete/rename branches, remove worktrees, or edit decisions / other tasks /
  `workflow/generated/*` / `QUESTIONS.md` / any `state:` except via `wf start`. Report when committed
  (branch + commit hash + a one-line description of each sample).
