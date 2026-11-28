---
id: T-0075
title: baseline-new-item-layout
state: ready            # draft | ready | running | blocked | review | changes | merged | closed
touches: [web/src/pages/BaselinePage.jsx, web/assets/css/components.css, web/assets/dist/, web/dist/index.html]
tests: [server]
after: [T-0054]
shares: {}
model: {claude: "sonnet high"}
operator_check: "On the Baseline items page, the New-item / editor form is a full-width section ABOVE the Library table (no longer a narrow right-hand rail). Each field carries a clear, plain-English explanation of what it is and how it's used when the item is placed into a Maneuver. Creating/editing/duplicating/deleting items and the live substitution preview all still work exactly as before."
---

## Why

The Baseline items page currently puts the **Library** table and the **New item / editor** form
side-by-side in a two-column `.split` (library wide on the left, editor in a cramped ~20rem sticky rail on
the right). In the rail the form fields are squeezed, the field hints are thin (most fields have none), and
the empty-state even says "Use the form on the **right**". The operator wants the **New item section placed
above the Library, full width**, with **more detailed per-field explanations** so someone creating their
first command preset understands what each field does and how it feeds a Maneuver.

## What to build — FRONTEND (design-first, two-phase)

**This is front-end work: design it first, do not jump to code.** Produce a short design (layout sketch +
the exact per-field explanatory copy) and hand it back for supervisor review (`review-1.md`). On approval,
implement. Use the reference repo **https://github.com/nextlevelbuilder/ui-ux-pro-max-skill** for the
design approach (you, the doer — pull patterns from it).

1. **Re-stack the layout.** Replace the two-column `.split` (`BaselinePage.jsx:324`) with a single-column
   flow: the **editor card first (full width, on top)**, the **Library card second (full width, below)**.
   Drop the `.sticky-rail` on the editor (`:453`) — it was a right-rail affordance. The library table then
   has the full width it wants. A full-width form means the current 2-column `form-grid` field groups
   (`:531`, `:560`, `:616`) can breathe — lay fields out so the detailed hints are readable (don't just
   stretch inputs edge-to-edge; cap input width and give each field room for its explanation).

2. **Add a clear explanation to every field.** Today only Command template has a real hint (`:503`). Give
   each field a plain-English `field__hint` (or equivalent) describing *what it is* and *how it's used when
   the item is placed into a Maneuver*. Cover at least:
   - **Name** — what the operator sees when picking this preset in a Maneuver; make it recognizable.
   - **Command template** — the actual command run on each target; `{placeholder}` tokens are filled in
     per-Maneuver (or from the defaults below); braces escape with `{{` / `}}`. (Keep the existing detected-
     placeholders readout.)
   - **Description** — optional operator note; shown under the name in the library and when choosing the item.
   - **Warm-up (startup lag, s)** — how long the tool takes to ramp before it's doing real work, so the
     Load Controller / Maneuver timing can account for it. (Explain the unit and that 0 means none.)
   - **Tags** — free-form labels for filtering/grouping in the library (comma-separated).
   - **Default inputs** — per-placeholder default values used when a Maneuver doesn't override them; blank =
     must be supplied at placement time.
   - **Substitution preview** — explain it renders the final command with the test values, to sanity-check
     the template before saving.
   Keep copy concise (one or two lines per field), consistent in voice with the rest of the panel, English
   (D-0003).

3. **Fix the now-stale references.** The empty-state text "Use the form on the **right**"
   (`BaselinePage.jsx:365`) must change to match the new position (e.g. "the form above"). Check the page
   header blurb and any other "right/rail" wording.

4. **Rebuild + commit the SPA bundle** (`./web/build.sh`): regenerate `web/assets/dist/app.{js,css}` and
   `web/dist/index.html` and commit them with the source.

## Out of scope

- **No behavior / data / API changes.** Create, edit, duplicate, delete, the preview POST
  (`/api/baseline/preview`), placeholder detection, defaults handling, and the delete `<dialog>` all keep
  working byte-for-byte — this is layout + copy only.
- No new fields, no schema change, no backend edits (`internal/baseline`, `internal/panel/baseline.go`
  untouched).
- No change to other pages or to shared components beyond, at most, one or two small layout utility classes
  in `components.css` if genuinely needed (prefer reusing existing `.stack` / `.card` / `.field` / `.form-grid`).
- Don't change how items are stored or the library columns' meaning.

## Done means

- The New-item/editor form renders **full width above** the Library table; the Library is full width below;
  no right rail. Responsive down to narrow widths without horizontal scroll.
- Every field listed above shows a clear, correct explanation; the "form on the right" empty-state copy is
  gone.
- All existing interactions verified working: create, edit (row click + edit icon), duplicate, delete
  (dialog), live substitution preview with test inputs, default-input add/remove, search/filter, the
  `noBackend` and load-error banners.
- `go build ./...` ok, `scripts/test.sh server` passes (baseline API tests unaffected), `check-app` clean
  on the Baseline page; web bundle rebuilt + committed.
- A `review-1.md`-approved design preceded the code (two-phase).

## Facts this brief relies on

- Current layout: `.split` container at `web/src/pages/BaselinePage.jsx:324`; Library card `:326`; editor
  card with `.sticky-rail` `:453`. `.split` CSS = 2-col grid `minmax(0,1fr) 20rem`, collapses to 1-col
  under 1200px: `web/assets/css/base.css:159-172`; `.sticky-rail` `web/assets/css/components.css:473-474`.
- Fields & current hints: Name `:471` (no hint), Command template `:487` (hint `:503`), Description `:519`
  (placeholder only), Warm-up `:532`, Tags `:544`, Default inputs block `:557`, Substitution preview block
  `:608`. Empty-state "form on the right": `:365`.
- Page purpose (for accurate copy): header blurb `:291-296` — baseline items are reusable command presets
  placed into a Maneuver; target fields are filled when placed. Backend contract unchanged (T-0054).
- Committed bundle artifacts to regenerate: `web/assets/dist/app.{js,css}`, `web/dist/index.html`
  (`web/build.sh`).

## Notes for the doer

- Design first: hand back a brief design (layout + the exact field copy) for `review-1.md`; wait for
  approval before coding. Pull layout/spacing/hint patterns from the reference repo
  `https://github.com/nextlevelbuilder/ui-ux-pro-max-skill`.
- Run `./scripts/wf start T-0075` before committing; commit only inside `touches` + append `log.md` ending
  in the `wf:` footer. Do NOT push/merge/branch-delete, edit `state:`, or touch decisions/generated/other
  tasks.
- Keep the diff tight: it's a re-stack + copy pass, not a rewrite of the component's logic/state.
