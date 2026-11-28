# T-0075 log

## Entry 1 - design (phase 1) and implementation (phase 2)

Setup: fast-forwarded the branch to main (1b114e1) as sanctioned, then `wf start T-0075`.

Phase 1: design written to `review-1.md` (aa688a4); supervisor approved it with three notes, applied below.

Phase 2, what changed:
- `web/src/pages/BaselinePage.jsx`: `.split` replaced by `.stack`; editor card is first and full width,
  Library card second; `.sticky-rail` dropped. Name | Description and Warm-up | Tags sit in `.form-grid--2`
  pairs; Command template has its own row (textarea capped at 60rem); Default inputs and the test-value grid use
  the existing auto-fit `.form-grid`. Every field has a persistent `field__hint field__hint--block` with the
  approved copy (with `aria-describedby`). The Default inputs and Substitution preview titles plus hints are
  now always visible (Default inputs shows a "placeholders appear here" hint until the template has any).
  Empty-state copy now says "the form above". No state, handler, or API changes.
- Copy decisions per supervisor notes: Warm-up hint says "so Maneuver timing can allow for it" (no Load
  Controller claim); Description hint says "Shown under the name in the Library" only, because the Maneuver
  item picker shows name and command template, not the description (ManeuverPage.jsx ~1141-1146).
- `web/assets/css/components.css`: `.form-grid--2` (2 columns, 1 under 700px), `.field__hint--block`
  (65ch, muted colour), `.input--narrow` (10rem). All three are used.
- Rebuilt bundle with `./web/build.sh`: `web/assets/dist/app.{js,css}` changed; `web/dist/index.html` came out
  byte-identical, so nothing to commit there.

Self-check:
- `go build ./...` ok. `scripts/test.sh server`: one run printed FAIL (package not identified in the tail I
  saw); two reruns were fully green (rc=0, no FAIL lines). Backend untouched, so I treat the first as a
  flake, probably load from parallel worktrees, but flagging it.
- `check-app` does not exist in this tree (same as T-0026 and T-0028 logs). Substitute: ran the server from
  a scratch data dir and drove headless Chrome over CDP. Light and dark, 1440px: editor full width above the
  Library (both 1152px wide, same x), no right rail. 768px and 390px: no horizontal scroll, form collapses
  to one column. Zero console errors or exceptions in any run. Screenshots reviewed by eye in both themes.
- Interactions verified: create (typed values, placeholder detection, default inputs appear), live
  preview renders `ping -c 3 127.0.0.1`, row click opens "Edit item" with preview, duplicate (row icon),
  search filter, delete dialog opens and removes the item. Server, browser, and scratch data removed
  afterwards.
- Not exercised: the `noBackend` and load-error banners (code untouched, only the layout container
  around them).

<!-- wf: commits=1451ed9 left=- tests=server:pass blocked=no -->
