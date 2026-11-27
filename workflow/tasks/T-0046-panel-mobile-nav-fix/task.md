---
id: T-0046
title: panel-mobile-nav-fix
state: closed
canceled: "Superseded by T-0047 (panel-spa-foundation). The React SPA shell rebuilds the nav/drawer and fixes the mobile stacking-context bug as part of the rewrite, so a separate interim fix on the soon-to-be-replaced server-rendered shell is throwaway work. Canceled by operator 2026-11-28. Diagnosis kept below for T-0047 to reuse."
touches: [web/assets/css/base.css, web/_src/shell.html, web/assets/js/app.js, internal/panel/server_test.go]
tests: [server]
after: [T-0040]
shares: {}
model: {claude: "sonnet high"}
operator_check: "On a phone-width viewport, opening the nav drawer shows the menu AND every menu item is clickable/navigable; the scrim dims only the page content behind the drawer (not the drawer), tapping the scrim or a nav item closes the drawer, Esc closes it, and body scroll is locked while open. No regression at desktop width."
---

## Why

On mobile (`max-width: 959px`) opening the nav drawer greys the page and **no menu item can be
selected** (operator-reported, screenshots). Root cause: `.app` has `position: relative; z-index: 1`
(`web/assets/css/base.css:63`), creating a stacking context; the mobile drawer `.sidebar` is nested inside
`.app` with `z-index: 60` (`:178`), so that 60 is **trapped inside `.app`'s z-index‑1 band**. The scrim is
`body.nav-open::after { z-index: 50 }` (`:183`) at **body level**, so it paints above the whole `.app`
subtree — including the drawer — and intercepts all pointer events. The drawer is therefore un-clickable.

## What to build

A minimal, robust fix (current server-rendered panel — **no React, no new deps**):

1. Make the open drawer sit **above** the scrim and stay interactive. Pick the cleanest approach, e.g.:
   - give the scrim its own real element / layer that is **below** the drawer in the *same* stacking
     context, or lift the mobile `.sidebar` out of `.app`'s trapped context (e.g. the drawer and scrim as
     top-level fixed layers with `scrim < drawer` z-order), ensuring the drawer has `pointer-events:auto`
     and the scrim only covers the content.
   - Keep `.bg-canvas` (z-index 0, pointer-events none) and desktop layout unchanged.
2. Interactions: tapping a nav link navigates (and closes the drawer); tapping the scrim closes it;
   **Esc** closes it; focus moves into the drawer when opened and the toggle keeps `aria-expanded` in
   sync (some of this exists in `app.js:41-51` — extend, don't duplicate). Lock body scroll while open.
3. Verify at phone widths (≤599 and ≤959) in both themes; confirm desktop (≥960) is untouched.

## Out of scope

- No React / SPA / router work (that's T-0047), no new pages, no visual redesign beyond making the drawer
  usable, no backend/Go-logic change (only a cosmetic `server_test.go` assertion if one is needed).
- Note: T-0047 will rebuild the shell in React; this is the **interim live fix**. Keep it small.

## Done means

- At phone width the drawer opens, every nav item is tappable and navigates, the scrim dims only content,
  scrim/Esc/nav-item all close it, body scroll is locked while open, focus is handled, `aria-expanded`
  tracks state. Desktop unchanged.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes. Manual check at ≤599/≤959.

## Facts this brief relies on

- Stacking trap: `web/assets/css/base.css:63` (`.app z-index:1`), `:177-183` (mobile `.sidebar z-index:60`
  nested in `.app`; scrim `body.nav-open::after z-index:50`). Toggle JS: `web/assets/js/app.js:41-51`
  (`.topbar__menu`, `body.nav-open`). Drawer markup: `web/_src/shell.html:77` (`<aside class="sidebar">`),
  `:117` (menu button). `.bg-canvas`: `web/assets/css/bg.css:7-13` (z-index 0, pointer-events none).

## Notes for the doer

- Smallest change that makes the drawer reliably interactive above the scrim; don't refactor the shell.
  Run `./scripts/wf start T-0046` before committing; commit inside touches + the log. Do NOT
  push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
