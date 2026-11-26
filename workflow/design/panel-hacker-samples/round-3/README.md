# Fleet Panel — Hacker Restyle Samples, Round 3 (T-0034)

A matched **light + dark** pair built as **one unified design system** from the round-2 winners (`round-2/sample-2.html` and `round-2/sample-5.html`). Both files render the exact same representative Fleet panel slice (`Machines` view: sidebar nav, top bar, 4 summary tiles, 6-row mock registry with live client-side filtering, expandable panels, row menus, and embedded `Terminal` session panel) with identical layout, spacing, components, typography, and border geometry — differing only in their theme color tokens.

## Shared Design System

- **Typography (from round-2 sample-2)**:
  - **Space Mono** (`400`, `700`) for display headings, section titles, navigation group labels, table headers, and oversized stat counters.
  - **Red Hat Mono** (`400`, `500`, `600`, `700`) for body copy, dense registry rows, form controls, chips, and terminal output, with `tabular-nums`.
- **Borders & Box-Outline Framing (from round-2 sample-2)**:
  - Crisp `1.5px` / `2px` box outlines (`--border-strong`) with `2px` radius (`0px` on chips).
  - Heavy `2px`–`4px` structural rules (`--rule`) on page headers, card headers, and stat tile top bars (accent-highlighted on *Needs attention* and *Disk guard*).
  - Hard offset block-shadow frames (`3px 3px 0` on cards, dropdown menus, and mobile drawer; `2px 2px 0` on buttons, brand mark, active nav item, bordered icon buttons, terminal frame, and toasts) with tactile depression (`translate(2px, 2px)`) on button press.

## Themes

- **[light.html](light.html) — Warm Paper & Carbon Ink** *(round-2 sample-2 palette)*:
  - Archival paper canvas (`#f3efe6` with subtle `20px` dot grid), warm cream surfaces (`#fbf9f3`, `#ede8dc`), `#181a18` carbon-ink rules and offset block shadows, `#d9480f` international orange primary accent, `#1d4ed8` blueprint cobalt links/info, and an embedded obsidian hardware scope (`#121512`) for the `Terminal` session.
- **[dark.html](dark.html) — Void Black, Bone-White & Signal Red** *(round-2 sample-5 palette)*:
  - Void black canvas (`#09090b` with subtle `20px` dot grid), dark charcoal surfaces (`#101014`, `#17171c` with subtle halftone card-header dot matrix), `#f4f4f0` bone-white typography and major rules, `#858594` graphite-bone box outlines paired with `#383844` offset block-shadow frames, `#ff2a3c` Signal Red reserved for primary triggers, active terminal cursor, and critical alerts/stats, and a `#050507` void terminal panel.
