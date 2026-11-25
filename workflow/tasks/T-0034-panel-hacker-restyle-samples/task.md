---
id: T-0034
title: panel-hacker-restyle-samples
state: merged
touches: [workflow/design/panel-hacker-samples/]
tests: []
after: []
shares: {}
model: {claude: "sonnet high"}
kind: design-spike
operator_check: "The doer delivers EXACTLY 3 distinct standalone HTML samples re-skinning the panel in a hacker aesthetic (monospace/hacker font), with the same layout, screens and controls as today. The USER picks a direction — there is no pass/fail gate and the supervisor applies no acceptance checks. Nothing is wired into the real panel."
---

## Why

Explore a visual re-skin of the web panel into "a bit hacker-style UI + a hacker-style font".
This is a **look-and-feel exploration only** — the point is to see options, not to ship one.

## What to build

Produce **exactly 3** different sample re-designs of the panel, each a **self-contained static HTML
file** (inline CSS; a web font may load from Google Fonts), under
`workflow/design/panel-hacker-samples/`:

- `sample-1.html`, `sample-2.html`, `sample-3.html`
- plus `README.md` in that folder: one paragraph per sample naming the aesthetic, the font(s), and the
  accent/background colors, so the user can compare at a glance.

Each sample renders a **representative slice of the panel** (top nav/header + the Machines/Operate view
with a few mock rows, a couple of stat tiles, and a terminal-style panel) using **mock data** — enough
to judge the style. All 3 show the **same** representative screen so they compare fairly.

Three *distinct* hacker takes (free to choose), e.g.: (1) green-on-black phosphor/CRT terminal,
(2) amber retro-terminal, (3) modern "cyber" dark with a neon accent. Use a monospace/terminal font
(JetBrains Mono, Fira Code, IBM Plex Mono, Share Tech Mono, VT323, …) — a different font/treatment per
sample is encouraged.

## Out of scope

- **No UX changes.** Keep the existing layout, information architecture, screens, nav and controls
  exactly as they are; change only the skin (colors, typography, borders, texture, small motion). Do not
  move, add, remove, rename, or re-flow any control or screen.
- **No touching the real app.** Do NOT modify anything under `web/` or any Go code; these are
  standalone comparison mockups, not an edit to the panel.
- No 4th sample, no "winner" picked, no wiring, no build.

## Done means

- The 3 standalone HTML samples + README exist under `workflow/design/panel-hacker-samples/` and open
  correctly in a browser, each a visually distinct hacker skin with a monospace/hacker font, same
  layout/controls as today, mock data only.
- **There is no acceptance gate.** The supervisor does NOT review for pass/fail and applies no checks;
  the user alone judges the samples and may commission a follow-up to apply one. "Done" = the 3 samples
  are committed.

## Facts this brief relies on

- The panel is server-rendered HTML templates under `web/pages/*.html` (the doer may glance at them to
  mirror the current layout faithfully, but must not edit them).
- This is a design spike; `tests: []` and no `after` deps — it depends on nothing and nothing depends
  on it.

## Notes for the doer

- You MAY glance at `web/pages/*.html` to reproduce the current layout/controls in the mock (so "no UX
  change" holds), but you are rebuilding the structure in a standalone file, not editing the app.
- Keep each file self-contained (works opened directly); tiny inline JS only if it aids the demo; no JS
  frameworks.
- Run `./scripts/wf start T-0034` before committing; commit the 3 samples + README on the branch.
- Do NOT push, merge, delete/rename branches, remove worktrees, or edit decisions / other tasks /
  `workflow/generated/*` / `QUESTIONS.md` / any `state:` except via `wf start`. Report when committed
  (branch + commit hash + a one-line description of each sample).
