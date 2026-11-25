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
operator_check: "ROUND 2: the doer delivers EXACTLY 5 NEW standalone HTML samples (in workflow/design/panel-hacker-samples/round-2/) that act on review.md — much more novel/experimental than round 1, same layout/controls, monospace/hacker font. The USER picks a direction; no pass/fail gate, no supervisor acceptance checks. Nothing is wired into the real panel."
---

## Why

Explore a visual re-skin of the web panel into "a bit hacker-style UI + a hacker-style font".
This is a **look-and-feel exploration only** — the point is to see options, not to ship one.

**ROUND 2.** Round 1 shipped 3 samples; the operator reviewed them in **`review.md` (READ IT FIRST)**.
Summary: sample-3 rejected; sample-2 semi-good but boring; sample-1 good but a bit noisy; and **none had
any novelty**. This round acts on that feedback.

## What to build

Produce **exactly 5 NEW** sample re-designs of the panel, each a **self-contained static HTML file**
(inline CSS; a web font may load from Google Fonts), under
**`workflow/design/panel-hacker-samples/round-2/`**:

- `sample-1.html` … `sample-5.html` (inside `round-2/`), plus a `README.md` in `round-2/`: one paragraph
  per sample naming the aesthetic, the font(s), and the accent/background colors.
- **Keep the round-1 files** (`workflow/design/panel-hacker-samples/sample-1..3.html`) untouched for
  reference.

Each sample renders the **same representative slice of the panel** (top nav/header + the Machines/Operate
view with a few mock rows, a couple of stat tiles, and a terminal-style panel) using **mock data**, so
they compare fairly.

Act on the review:
- **Push hard for NOVELTY** — this is the main ask. Avoid generic "hacker UI" tropes; take real
  aesthetic risks (distinct typographic systems, unusual layout texture/density, original color logic,
  restrained motion, a fresh metaphor) while staying usable and legible.
- Carry forward what worked: the **phosphor-green legibility** done *cleaner* (less scanline/glow/texture
  noise than round-1 sample-1), and the **calm retro feel** but less boring than round-1 sample-2.
- **Do NOT** reprise round-1 sample-3's cyber-dark tactical direction.
- Each a distinct monospace/hacker font/treatment (JetBrains Mono, Fira Code, IBM Plex Mono, Share Tech
  Mono, VT323, Space Mono, Martian Mono, …) — variety encouraged.

## Out of scope

- **No UX changes.** Keep the existing layout, information architecture, screens, nav and controls
  exactly as they are; change only the skin (colors, typography, borders, texture, small motion). Do not
  move, add, remove, rename, or re-flow any control or screen.
- **No touching the real app.** Do NOT modify anything under `web/` or any Go code; these are
  standalone comparison mockups, not an edit to the panel.
- Do not edit or delete the round-1 files. No "winner" picked, no wiring, no build.

## Done means

- Exactly **5 new** standalone HTML samples + a README exist under
  `workflow/design/panel-hacker-samples/round-2/` and open correctly in a browser — each a visually
  distinct, **noticeably more novel** hacker skin with a monospace/hacker font, same layout/controls as
  today, mock data only, and clearly acting on `review.md` (cleaner than r1-sample-1, not boring like
  r1-sample-2, not reprising r1-sample-3).
- **There is no acceptance gate.** The supervisor applies no pass/fail checks; the user alone judges the
  samples. "Done" = the 5 round-2 samples are committed.

## Facts this brief relies on

- The panel is server-rendered HTML templates under `web/pages/*.html` (the doer may glance at them to
  mirror the current layout faithfully, but must not edit them).
- This is a design spike; `tests: []` and no `after` deps — it depends on nothing and nothing depends
  on it.

## Notes for the doer

- You MAY glance at `web/pages/*.html` to reproduce the current layout/controls in the mock (so "no UX
  change" holds), but you are rebuilding the structure in a standalone file, not editing the app.
- **Read `workflow/tasks/T-0034-panel-hacker-restyle-samples/review.md` first** and look at the round-1
  samples it critiques, so round 2 genuinely improves on them.
- Keep each file self-contained (works opened directly); tiny inline JS only if it aids the demo; no JS
  frameworks.
- Run `./scripts/wf start T-0034` before committing; commit the 5 round-2 samples + README on the branch.
- Do NOT push, merge, delete/rename branches, remove worktrees, or edit decisions / other tasks /
  `workflow/generated/*` / `QUESTIONS.md` / any `state:` except via `wf start`. Report when committed
  (branch + commit hash + a one-line description of each sample).
