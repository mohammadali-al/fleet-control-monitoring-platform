# D-0011 — Panel moves from server-rendered templates to a React SPA

**Status:** accepted (2026-11-28)

## Context

The panel was server-rendered Go `html/template` (one full page per route). Operators want (1) no full-page
reload when switching routes, (2) a background Matrix-rain that keeps running across navigation instead of
resetting, and (3) clean client-side routing without `.html` URLs. A persistent shell with a client router
delivers all three.

## Decision

Rebuild the panel front-end as a **React single-page app with React Router**, client-side routing, and a
**persistent shell** (nav + theme toggle + background rain mounted once, never unmounted on navigation).
Drop `.html`-style routes. The back-end keeps its JSON (`/api/*`) and WS/SSE (`/ws/events`) contracts
unchanged — this is a **view-layer** migration only. `fleet-server` serves the SPA shell + a bundled
static JS/CSS asset set and a catch-all that returns the shell for app routes (deep-link/refresh safe).

## Consequences

- **New:** a pinned JS build toolchain (esbuild or Vite) enters the repo. The **built bundle is committed/
  embedded** so `go build ./...` and CI need no Node; a documented rebuild command regenerates it. React +
  React Router are self-hosted (no runtime CDN — the panel stays offline-capable, like the fonts).
- Migration is incremental: T-0047 builds the foundation + ports Machines as the reference; T-0048–T-0051
  port the remaining pages (un-ported routes fall back to a legacy full-load during the transition).
- This is a **deviation** from the architecture doc's server-rendered approach (noted in the T-0044 gap
  audit); recorded here as the governing decision.

## Alternatives considered

- **Tiny vanilla SPA router (no build step):** achieves the same three goals with ~100 lines and keeps the
  zero-build, offline-first setup. Rejected per operator preference for React + React Router.
- **Keep server-rendered + htmx-style partial swaps:** avoids a build step but not the chosen stack.
