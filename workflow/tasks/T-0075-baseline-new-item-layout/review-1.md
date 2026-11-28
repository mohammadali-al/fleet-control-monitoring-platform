# T-0075 design for review (phase 1, no code written yet)

Author: doer. Scope: `web/src/pages/BaselinePage.jsx` plus at most 2-3 small utility classes in
`web/assets/css/components.css`. Layout and copy only; all state, handlers, `/api/baseline/preview` untouched.

## Design approach (patterns taken from ui-ux-pro-max-skill)

The reference repo's form guidance, applied here:
- Visible label on every field, plus **persistent helper text under the control** (not placeholder-only).
- Cap control width to the expected content (do not stretch inputs edge-to-edge); keep the hint at a readable
  line length (~60-70ch).
- Group related fields; reflow to one column on narrow widths, no truncation or horizontal scroll.
- Reuse the existing semantic tokens/components (`.card`, `.field`, `.field__hint`, `.form-grid`, `.stack`).

## 1. Layout sketch

Replace `<div className="split">` (BaselinePage.jsx:324) with `<div className="stack">`. Move the editor
`<section>` first, the Library `<section>` second. Drop `sticky-rail` from the editor card (:453). Nothing else
in either card changes except the items below.

```
[ page head: Baseline items ............................ (+ New item) ]
[ alerts: noBackend / loadError ]

+-- card: New item / Edit item ------------------------------------------+   full width
| Create a reusable command template                                     |
|                                                                        |
| Name (required)                 Description                            |   .form-grid 2 cols
| [HTTP GET flood        ]        [Optional short note ............. ]   |   (collapses to 1 col < 700px)
|  hint (1-2 lines)                hint (1-2 lines)                      |
|                                                                        |
| Command template (required)                                            |   full row, textarea max-width ~60rem
| [ hey -z {duration}s -q {rate} {url}                                 ] |
|  hint + "Detected: {duration} {rate} {url}"                            |
|                                                                        |
| Warm-up (startup lag, s)        Tags                                   |   .form-grid 2 cols
| [0     ]  (input capped ~10rem) [http, l7 ...................]        |
|  hint                            hint                                  |
|                                                                        |
| Default inputs   (section hint always visible)                         |
| {duration} default   {rate} default   {url} default                    |   .form-grid auto-fit, 3 across
| [..............]     [............]   [...........]                    |   at wide widths, fewer when narrow
|                                                                        |
| Substitution preview  (section hint always visible)                    |
| Test {duration}  Test {rate}  Test {url}                               |   same grid
| +-- codeblock: rendered command -------------------------------------+ |
| [Create item] [Duplicate] [Clear/Cancel/...]  (buttons unchanged)      |
+------------------------------------------------------------------------+

+-- card: Library (search, table, footer unchanged) --------------------+   full width, below
```

Small CSS additions (in `components.css`, reusing tokens; no new colors):
- `.form-grid--2 { grid-template-columns: repeat(2, minmax(0, 1fr)); }` with a one-column collapse under
  ~700px (replaces the three inline `gridTemplateColumns: 'repeat(2,minmax(0,1fr))'` styles and fixes their
  lack of a narrow-width fallback).
- `.field__hint` in the editor gets readable line length: `.field__hint { max-width: 65ch; }` scoped
  via a modifier `.field__hint--block` (so other pages' hints are not affected), and colour
  `--text-muted` instead of `--text-faint` for contrast.
- `.input--narrow { max-width: 10rem; }` for the Warm-up numeric input only.

The two-column pairs (Name | Description, Warm-up | Tags) keep each field in a column wide enough
(~25-35rem at desktop) that the one-to-two-line hint wraps without crowding. Default-input/test-value grids
use the existing auto-fit `.form-grid` (3-4 across on wide screens).

## 2. Exact per-field copy

(Voice: second person-free, plain sentences, matches page header. `{x}` shown in `token` spans in the UI.)

| Field | Label (unchanged unless noted) | Hint (new) |
|---|---|---|
| Name | `Name (required)` | `What you will see when picking this preset for a Maneuver. Make it recognizable, e.g. "HTTP GET flood".` |
| Command template | `Command template (required)` | `The exact command run on each target. Write {name} wherever a value changes; each placeholder is filled in per Maneuver, or from the Default inputs below. To print a literal brace, type {{ or }}.` followed by the existing `Detected: {a} {b}` readout (kept; shown only when placeholders are detected). |
| Description | `Description` | `Optional note for operators. Shown under the name in the Library and when choosing this item for a Maneuver.` |
| Warm-up | `Warm-up (startup lag, s)` | `Seconds the tool needs to ramp up before it does real work, so Maneuver timing and the Load Controller can allow for it. Use 0 if there is no lag.` |
| Tags | `Tags` | `Optional labels for filtering and grouping, separated by commas (for example: http, l7).` |
| Default inputs (section) | `Default inputs` | `Values used for a placeholder when a Maneuver does not set its own. Leave a field blank to require a value when the item is placed.` Shown always: when the template has no placeholders yet, the hint reads `Placeholders from your command template appear here once you add them (for example {url}).` (one extra conditional line; no state change.) |
| Default input row | `{key} default` (+ `(unused)`) | unchanged label; placeholder text `Leave blank if filled per Maneuver` unchanged |
| Substitution preview (section) | `Substitution preview` | `Renders the final command using the test values below, so you can check the template before saving. Nothing is run and nothing is saved.` Shown always. |
| Test row | `Test {key}` | unchanged; placeholder logic unchanged |

Other copy changes:
- Empty-state (BaselinePage.jsx:365): from `No baseline items yet. Use the form on the right to create your first reusable command template.` to `No baseline items yet. Use the form above to create your first reusable command template.`
- Page header blurb (:292-296): scanned, contains no "right"/"rail" wording; left unchanged.
- Card head subtitle for the new-item state (`Create a reusable command template`): unchanged.
- Grep for other "right"/"rail" strings in BaselinePage.jsx during implementation; fix any found.

## 3. Questions / flags for the supervisor

1. Default inputs / Substitution preview sections: I propose showing their title + hint always (today the
   Default inputs block is hidden until the template has placeholders), so the explanation is visible to a
   first-time user. That adds one conditional hint line and keeps all handlers/state untouched. If you prefer
   zero structural change, I will keep the block hidden when empty and put the explanation in the empty state
   of the Library footer only. Default plan: always-visible hints.
2. I will add the three small CSS utilities above (`.form-grid--2`, `.field__hint--block`, `.input--narrow`)
   in `components.css` (inside `touches`). Brief allowed "one or two"; this is three tiny ones. If you want
   it at two, I will drop `.input--narrow` and rely on the 2-col grid width.
3. No separate "Cancel"/"Reset" button changes; actions row untouched.

## 4. Verification plan after approval

`go build ./...`, `scripts/test.sh server`, `./web/build.sh`, `check-app` on Baseline (light + dark), manual
pass: create, edit (row + icon), duplicate, delete dialog, preview with test inputs, add/remove default
inputs, search, noBackend and load-error banners, plus narrow-width check (no horizontal scroll).
