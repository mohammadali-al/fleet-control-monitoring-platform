# Review — round 1 (REJECTED: not consistent with the live panel)

The v1 samples look panel-ish but use **ad-hoc markup/classes** (`bad`, `good`, `dup`, `grid2`, `mut`,
`name`, `priv`, `tag`, `x`, `chip ok`) and bespoke inline CSS. The operator's feedback: **the samples are
not consistent, in paradigm, with the current UI/UX of the panel.** A mockup that doesn't use the real
components reads as a different product and can't be judged as "how it would look in the panel."

## What to change (round 2)

**Use the REAL panel design system — do not invent classes.** The samples must be visually
indistinguishable from the live Machines page, just demonstrating the label ideas.

1. **Reuse the actual CSS.** Inline (or `<style>`-embed) the real token + component CSS so each file is
   standalone: `web/assets/css/tokens.css`, `web/assets/css/base.css`, `web/assets/css/components.css`
   (copy their contents in; don't approximate). Keep dark default.
2. **Use the real class names + markup** exactly as the live panel does (see `web/src/pages/MachinesPage.jsx`):
   - Cards: `card` / `card__head` / `card__body` / `card__foot`; stat tiles: `card stat col-3 ...`.
   - Chips: `chip chip--ok | chip--warn | chip--danger | chip--info | chip--accent | chip--outline | chip--mono`
     (e.g. online/stage/version chips) — NOT a custom `chip ok`.
   - Buttons: `btn` / `btn btn--sm` / `btn--primary` / `btn--danger`; icon buttons: `icon-btn icon-btn--sm`.
   - Tables: the real panel table markup/classes (sticky-first etc.), disk meters `meter__bar/meter__fill/meter__text`,
     row menus `menu / menu__pop / menu__item`.
   - The details view should mirror the real **details drawer** structure (the T-0059 `card`/`dl` sections),
     not a bespoke box.
3. **Match the real Machines row** as the baseline, then show the label change against it — so the
   "today vs proposed" comparison is literally the real row style in both columns.
4. Keep everything else from the brief: the 3 suggestions, the collision data (web-01/web-02/db-01 all
   `root` @ `46.34.170.235`, differing by private IP; `ci-runner`/`mon-edge` as non-colliding controls),
   standalone (no external fetch), and the README.

Goal: an operator opening the files should feel they're looking at the actual panel with the new label
treatment dropped in — same cards, chips, buttons, table, drawer.
