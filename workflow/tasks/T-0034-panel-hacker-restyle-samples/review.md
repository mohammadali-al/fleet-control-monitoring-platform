# T-0034 — Review of round 1 (operator feedback, 2026-11-25)

Round-1 samples live in `workflow/design/panel-hacker-samples/` (sample-1/2/3.html). Verdict:

- **sample-3.html** (cyber-dark tactical, JetBrains Mono) — **bad.** Reject this direction.
- **sample-2.html** (amber retro-terminal) — **semi-good but boring.** Competent, lacks interest.
- **sample-1.html** (green phosphor CRT) — **good, but a bit noisy.** Closest to right; dial the noise
  (scanlines/glow/texture) back so content stays legible.
- **Across all three: no novelty.** They lean on generic "hacker UI" tropes. **Deviate much more** —
  take real aesthetic risks, surprising but still usable.

## Next round (why this task is re-opened)

Produce **5 NEW samples** (round 2) that act on the above: keep what worked (phosphor-green legibility
done cleaner, the calmer retro feel) but push for **originality** — distinct typographic systems, layout
texture, color logic, motion, or metaphor that we haven't seen in round 1. Same hard rules as before:
visual only, no UX/layout change, monospace/hacker font, standalone mock of the same representative
panel screen. Keep round-1 files for reference; put round 2 in its own subfolder.

---

# Round 2 review (operator feedback) → Round 3

Round-2 samples live in `workflow/design/panel-hacker-samples/round-2/`. Verdict:

- **round-2 sample-2** (Daylight Technical Broadsheet — light paper) — **good.**
- **round-2 sample-5** (Swiss Brutalist Monochrome + Signal Red — dark) — **good.**
- These two are the winners and can become the **light** and **dark** theme of ONE design system.

## Round-3 directive

Make **2 new samples** = a matched **light** and **dark** pair, one coherent design system built from
the two winners:

- **Fonts:** use **round-2 sample-2's fonts for BOTH** (Space Mono + Red Hat Mono) — not sample-5's.
- **Box outlines / borders:** use **round-2 sample-2's box-outline treatment for BOTH** (its carbon-ink
  rules + offset block-shadow frames), on the light and the dark variant alike.
- **Light variant:** sample-2's warm-paper palette. **Dark variant:** sample-5's black / bone-white +
  signal-red palette. Same layout, components and styling across both — only the theme (colors) differs,
  so they read as light/dark of the same thing.
- Keep sample-2's and sample-5's strengths; same hard rules (visual only, no UX/layout change, standalone
  mock of the same screen). Keep all earlier rounds for reference.

---

# Round 3 review (operator feedback) → Round 4

The round-3 light/dark pair (`round-3/light.html` + `dark.html`) is the **chosen design system** — keep
it as the baseline: same fonts (Space Mono + Red Hat Mono), same box-outlines / offset-shadow frames,
same two palettes (warm-paper light, void-black + signal-red dark). Round 4 explores **backgrounds** on
top of that fixed system.

## Round-4 directive

- **Revise the dot background pattern** — the dot texture carried from round 2/3 isn't quite right;
  refine it (scale, spacing, contrast, alignment to the grid).
- Make a handful of samples that differ **ONLY in the background pattern** — everything else (fonts,
  box-outlines, components, both palettes, layout) stays identical to the round-3 system. Include:
  - the **revised dot** pattern,
  - a couple more **static** patterns (e.g. fine grid / graph-paper, crosshatch, hairline rules),
  - some **hacker-style** backgrounds (faint hex-dump / ASCII / circuit-trace / terminal-noise texture,
    kept subtle so content stays legible),
  - **exactly one with a MATRIX-movie-style LIVE animated background** — green falling code rain (canvas
    `requestAnimationFrame`), sitting BEHIND the content at low opacity so the panel stays fully legible.
- **Each sample is now ALL-IN-ONE:** a single self-contained file that contains **BOTH light and dark**
  with an in-page **theme toggle**, and makes the panel's **other sections browsable via the working
  top-nav menus** (click the nav to switch Operate / Observe / Admin / Machines / Watchdog within the
  mock). The only thing that varies between the round-4 files is the **background pattern**.
- Same hard rules: visual only, no change to the panel's real controls/layout; the theme toggle + nav
  browsing are demo chrome, kept minimal and unobtrusive. Keep all earlier rounds for reference.

---

# Round 4 review (operator feedback) → Round 5

- **`round-4/05-matrix-rain.html` was good** — keep the Matrix live-rain concept. BUT in **dark mode the
  rain was green**; try making it **grey / muted (monochrome)** instead, closer to the light-mode feel
  and the design's graphite/bone palette. (Deliver a refined Matrix sample with grey dark-mode rain.)
- The **plain static patterns felt too simple.** Make more samples with **more creative OR professional
  backgrounds** — richer textures or subtle motion that feel considered, not generic. **But don't
  overdo it:** restrained, tasteful, content stays fully legible; no loud or busy backgrounds.

## Round-5 directive

- Keep the fixed round-3 design system and the all-in-one format (both light+dark via toggle, browsable
  top-nav), same as round 4 — only the background varies.
- Deliver a **refined Matrix-rain** whose **dark-mode rain is grey/muted (not green)**, light-mode
  consistent, reduced-motion respected.
- Deliver a handful more backgrounds that are **creative or professional** (e.g. subtle topographic
  contour lines, a faint blueprint, a slow constellation/particle field, a gentle signal/aurora gradient,
  a soft isometric grid) — pick tasteful ones, keep them subtle. Avoid plain dots/grids this round.
- Keep it modest in count and intensity ("do not make it too much"). Keep all earlier rounds for
  reference.
