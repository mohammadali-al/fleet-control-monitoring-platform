# D-0010 — Panel skin: round-5 design system, dark default, client-side theme

**Status:** accepted · **Date:** 2026-11-27

## Decision
The web panel adopts the **round-5 design system** from
`workflow/design/panel-hacker-samples/round-5/` (fonts **Space Mono + Red Hat Mono**, carbon-ink
box-outlines + offset-shadow frames, the warm-paper light / void-black + signal-red dark palettes):

- **Default theme is DARK.**
- **Theme is client-side only** — toggled in the browser and persisted in `localStorage`, applied before
  first paint (no flash), with **no server-side theme state** and no backend involvement.
- **Background is a swappable layer**, default = the **grey/muted Matrix-rain** (`round-5/01`); other
  sample backgrounds stay switchable.
- Fonts are **self-hosted** (not fetched from Google Fonts) so the panel works offline / makes no
  external calls.

## Consequences
- This is a **front-end-only** change (templates + assets under `web/`); no change to panel Go handlers,
  routes, or any backend. Existing live behaviour (SSE/WS updates, xterm terminals, forms, Grafana
  embeds) must keep working. Implemented by [[T-0040]] (panel-skin-refactor).
