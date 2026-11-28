---
id: T-0077
title: baseline-editor-modal
state: merged
touches: [web/src/pages/BaselinePage.jsx, web/assets/css/components.css, web/assets/dist/, web/dist/index.html]
tests: [server]
after: [T-0075]
shares: {}
model: {claude: "sonnet high"}
operator_check: "On the Baseline items page the Library is the page; the New-item / Edit form now lives in a modal dialog that opens from the 'New item' button (create) or a row / Edit (edit) and CLOSES via its ✕, Esc, Discard, or after a successful save — it is no longer a permanent panel that is always open. The per-field hints are still there but quieter and more compact (one concise line each), so the form reads neat instead of noisy. Create / edit / duplicate / delete / live preview all still work."
---

## Why

T-0075 moved the New-item/editor form to a full-width section above the Library AND made every field hint
always-visible and expanded. Operator feedback: it is **too noisy**, and the form is **always open with no
way to dismiss it** ("it never closes, its dialog is always open"). Revise the page so the editor is a
**modal dialog** the operator opens and closes, and make the hints **light and compact** so the form reads
neatly.

The design direction below is **operator-specified** (chosen directly via a layout/hint question), so this
is not an open design exploration — implement exactly this. Pull form/modal spacing patterns from the
reference repo for polish, but the two decisions are already made.

## Design (locked by the operator)

1. **Modal dialog (overlay).** The Library is the whole page. "New item" and Edit open a centered modal
   with the form; **✕ / Esc / Discard / a successful save** all close it. Reuse the panel's existing
   `<dialog>` modal pattern — the delete-confirmation dialog in this same file already does `showModal()` /
   `close()` and uses `.dialog` / `.dialog__head` / `.dialog__body` / `.dialog__foot`.
2. **Hints: keep but lighter & compact.** Keep a hint on every field, but trim each to **one concise
   line** and style it quieter (smaller, muted, tight) so it sits as secondary text under the control and
   doesn't compete with the fields. Nothing hidden, no tooltips, no reveal-on-focus.

## What to build — FRONTEND only

1. **Library becomes the page.** Remove the `.stack` that stacks the editor above the Library
   (`BaselinePage.jsx:324`); the Library `<section>` (`:611`) is the always-visible page content, full width.
   The editor `<section>` (`:326`) is lifted out of the inline flow and into the modal (below).

2. **Editor in a modal `<dialog>`.** Add an `editorOpen` state and a second `<dialog>` (alongside the
   existing delete one), mirroring the delete dialog's sync pattern (`:85-99` useEffect →
   `showModal()`/`close()`; `:740` markup). Structure it with `.dialog__head` (title `New item` / `Edit item`
   + the `{id}` subtitle in edit mode, and a **✕ close button**), `.dialog__body` (the full form — name,
   description, command template, warm-up, tags, default inputs, substitution preview), and `.dialog__foot`
   (Create item / Save changes, Discard, plus Duplicate + Delete in edit mode). Keep **all existing
   handlers and state** — `handleSave`, `handleDuplicate`, the preview effect, default-input add/remove,
   `selectedId`/`draft`/`previewInputs` — only their container changes.

3. **Open / close wiring.**
   - Open create: the "New item" button (`:302`, `startNewItem`) → reset draft + open the modal.
   - Open edit: row click / the Edit icon (`selectItem`, `:190`) → load draft + open the modal.
   - Close: the ✕, Esc (`<dialog onCancel>`), Discard (`discardChanges` then close), and **after a successful
     create/save** (close + keep the existing success toast). Closing while busy (`saving`) is blocked, like
     the delete dialog does with `deleting`.

4. **Dialog sizing + scroll.** The editor form is bigger than the 34rem delete dialog. Add a
   `.dialog--wide` modifier in `components.css` (e.g. `width: min(60rem, calc(100vw - 2rem))`) and make
   `.dialog__body` scroll for this dialog (a `max-height` ~`calc(100vh - 10rem)` + `overflow:auto`) so a
   tall form (default inputs + preview) stays usable on short screens. Keep the delete dialog unchanged.

5. **Compact hints.** Trim each `.field__hint--block` string to one concise line (keep the meaning; drop
   the second sentence where it's redundant) and tighten the style — smaller/muted/tight line-height. Keep
   the Command-template **"Detected: {a} {b}"** readout. Default inputs / Substitution preview section
   hints stay (compact).

6. **Fix stale copy.** The empty-state (changed by T-0075 to "Use the form above…", `~:` in the Library
   section) no longer matches — point it at the button, e.g. "Use **New item** to create your first
   reusable command template." Grep for any other "above/form"/"rail" wording and fix.

7. **Rebuild + commit the SPA bundle** (`./web/build.sh` → `web/assets/dist/app.{js,css}`,
   `web/dist/index.html`).

## Out of scope

- No behavior / data / API change: CRUD, `/api/baseline/preview`, placeholder detection, defaults, the
  delete dialog's behavior — all unchanged. Backend (`internal/baseline`, `internal/panel/baseline.go`)
  untouched.
- No new fields, no field reordering beyond what the modal layout needs, no change to the Library table.
- Don't convert the hint copy to tooltips or hide it; it stays visible, just compact.

## Done means

- The Baseline page shows the Library by default; **New item** and **Edit** open the form in a modal; **✕ /
  Esc / Discard / successful save** close it; nothing keeps the form permanently open.
- Hints are present but visibly quieter/compact (one line each); the page reads neat, not noisy.
- All interactions verified: create, edit (row + icon), duplicate, delete (its own dialog still works),
  live preview with test inputs, default-input add/remove, search, `noBackend` + load-error banners. No
  horizontal scroll; modal scrolls internally on short screens. Focus moves into the modal on open and the
  page is usable with keyboard (Esc closes).
- `go build ./...` ok, `scripts/test.sh server` passes (baseline API unaffected), `check-app` clean on the
  Baseline page in light + dark; web bundle rebuilt + committed.

## Facts this brief relies on

- Current page (merged T-0075): `web/src/pages/BaselinePage.jsx` — `.stack` wrapper `:324`, editor
  `<section>` `:326` (`id="ed-title"` h2 `:329`), Library `<section>` `:611`. Field hints
  `field__hint--block` at `:360, :376, :400, :430, :446, :455, :515`. Existing delete `<dialog
  className="dialog">`: sync useEffect `:85-99`, markup `:740`. Openers: `startNewItem` `:182` (button
  `:302`), `selectItem` `:190`, `discardChanges` `:197`.
- Dialog CSS to reuse/extend: `web/assets/css/components.css:376-381` (`.dialog` = `min(34rem, …)`,
  `.dialog__head/body/foot`; `::backdrop` = `--scrim`). Hint style: `.field__hint` + `.field__hint--block`
  (added by T-0075) near `components.css:120`.
- Committed bundle: `web/assets/dist/app.{js,css}`, `web/dist/index.html` (`web/build.sh`).

## Notes for the doer

- Run `./scripts/wf start T-0077` before committing; commit only inside `touches` + append `log.md` ending
  in the `wf:` footer. Do NOT push/merge/branch-delete, edit `state:`, or touch decisions/generated/other
  tasks. A hook refusal means STOP and report.
- The design is operator-specified; implement it directly (no separate design-review handback needed).
  Reference UI patterns: `https://github.com/nextlevelbuilder/ui-ux-pro-max-skill`.
- Keep the diff tight: it's a re-home of the existing form into a modal + a copy/style trim, not a rewrite
  of the component's logic. UI language English (D-0003).
