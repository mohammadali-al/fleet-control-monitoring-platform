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
operator_check: "ROUND 4: the doer delivers EXACTLY 5 NEW standalone HTML samples (in workflow/design/panel-hacker-samples/round-4/) on the FIXED round-3 design system, differing ONLY in background pattern — a revised dot pattern, a couple more static patterns, some hacker-style backgrounds, and exactly one Matrix-style LIVE animated background. Each file is ALL-IN-ONE: contains both light + dark (in-page toggle) and the panel's sections browsable via the top-nav. The USER judges; no pass/fail gate. Nothing is wired into the real panel."
---

## Why

Explore a visual re-skin of the web panel into "a bit hacker-style UI + a hacker-style font".
This is a **look-and-feel exploration only** — the point is to see options, not to ship one.

**ROUND 4.** Rounds 1–3 are done and reviewed in **`review.md` (READ THE ROUND-4 SECTION FIRST)**. The
round-3 light/dark pair is the **chosen design system** and is now FIXED. Round 4 explores **background
patterns** on top of it; everything else stays identical to round 3.

## What to build

Produce **exactly 5 NEW** self-contained HTML samples (inline CSS; web fonts may load from Google Fonts;
inline JS allowed for the theme toggle, nav browsing, and the one animated background) under
**`workflow/design/panel-hacker-samples/round-4/`**, plus a short `README.md` in `round-4/` naming each
sample's background.

- **Keep all earlier rounds** untouched. Build on `round-3/light.html` + `round-3/dark.html` — reuse
  their fonts (Space Mono + Red Hat Mono), box-outlines/offset frames, components, layout, and both
  palettes **unchanged**.
- **The ONLY thing that varies between the 5 files is the background pattern.** Deliver:
  1. a **revised dot** pattern (refine round-2/3's dots: scale/spacing/contrast/grid alignment),
  2. one or two more **static** patterns (fine grid / graph-paper, crosshatch, or hairline rules),
  3. one or more **hacker-style** backgrounds (faint hex-dump / ASCII / circuit-trace / terminal-noise,
     subtle enough that content stays legible),
  4. **exactly one Matrix-movie-style LIVE animated background** — green falling code rain on a canvas
     (`requestAnimationFrame`), behind the content at low opacity, panel fully legible.
- **Each sample is ALL-IN-ONE:** a single file containing **BOTH light and dark** with an in-page
  **theme toggle**, and the panel's **other sections browsable via the working top-nav** (clicking
  Operate / Observe / Admin / Machines / Watchdog switches the shown screen within the mock, with mock
  data). The theme toggle + nav browsing are **demo chrome** — keep them minimal and unobtrusive; do not
  alter the panel's real controls/layout.

## Out of scope

- **No UX changes.** Keep the existing layout, information architecture, screens, nav and controls
  exactly as they are; change only the skin (colors, typography, borders, texture, small motion). Do not
  move, add, remove, rename, or re-flow any control or screen.
- **No touching the real app.** Do NOT modify anything under `web/` or any Go code; these are
  standalone comparison mockups, not an edit to the panel.
- Do not edit or delete any earlier-round files. No wiring, no build.

## Done means

- Exactly **5 new** standalone HTML samples + a README exist under
  `workflow/design/panel-hacker-samples/round-4/` and open correctly in a browser — all on the fixed
  round-3 design system, differing ONLY in background (revised dots, a couple more static patterns, some
  hacker-style, and exactly one Matrix-style live animated background).
- Each file is ALL-IN-ONE: both light + dark via an in-page toggle, and the panel's sections browsable
  via the top-nav; backgrounds stay subtle enough that content is fully legible in both themes.
- **There is no acceptance gate.** The supervisor applies no pass/fail checks; the user alone judges the
  samples. "Done" = the 5 round-4 samples are committed.

## Facts this brief relies on

- The panel is server-rendered HTML templates under `web/pages/*.html` (the doer may glance at them to
  mirror the current layout faithfully, but must not edit them).
- This is a design spike; `tests: []` and no `after` deps — it depends on nothing and nothing depends
  on it.

## Notes for the doer

- You MAY glance at `web/pages/*.html` to reproduce the current layout/controls in the mock (so "no UX
  change" holds), but you are rebuilding the structure in a standalone file, not editing the app.
- **Read the ROUND-4 section of `workflow/tasks/T-0034-panel-hacker-restyle-samples/review.md` first**,
  and start from `round-3/light.html` + `round-3/dark.html` — keep their whole design system fixed and
  change ONLY the background per sample.
- Keep each file self-contained (works opened directly); inline JS is fine for the theme toggle, nav
  browsing, and the one canvas Matrix background; no JS frameworks.
- Run `./scripts/wf start T-0034` before committing; commit the 5 round-4 samples + README on the branch.
- Do NOT push, merge, delete/rename branches, remove worktrees, or edit decisions / other tasks /
  `workflow/generated/*` / `QUESTIONS.md` / any `state:` except via `wf start`. Report when committed
  (branch + commit hash + a one-line description of each sample).
