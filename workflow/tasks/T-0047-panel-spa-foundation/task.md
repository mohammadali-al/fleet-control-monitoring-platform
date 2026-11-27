---
id: T-0047
title: panel-spa-foundation
state: merged
touches: [web/, web/embed.go, internal/panel/server.go, internal/panel/templates.go,
          internal/panel/server_test.go, internal/panel/machines_test.go]
tests: [server]
after: [T-0040, T-0045]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The panel is a React single-page app: the left nav, topbar, theme toggle, and the Matrix-rain background live in a PERSISTENT shell that never unmounts, so navigating between routes does NOT reload the page and the rain keeps dropping (never resets). Routing is React Router (client-side, no .html URLs); deep links and refresh work; the mobile drawer is usable. The Machines route is fully ported incl. its live SSE/WS updates + public-IP refresh + token-enroll panel. All JSON/WS APIs unchanged. `go build ./...` works with no Node (built bundle embedded). A documented route pattern lets remaining pages be ported independently."
---

## Why

Today each route is a full server-rendered page, so navigation reloads everything and the background rain
resets. Convert the panel to a **React SPA** (per D-0011): a persistent shell (nav + theme + background
rain) with **client-side routing (React Router)**, so route changes swap only the content area — no
reload, and the rain keeps running. Drop `.html`-style routes. **This is the foundation**; it ports one
reference page (Machines) and establishes the pattern the remaining pages follow (T-0048+). Back-end JSON
and WS/SSE APIs are unchanged — this is a view-layer migration.

## What to build

1. **Build toolchain (new, pinned, no-Node-at-`go build`):** add a JS build (esbuild or Vite — pick one,
   pin exact versions, commit the lockfile) that compiles React + JSX + the existing design-system CSS
   into a static bundle under `web/assets/` (or `web/dist/`). **The built bundle is committed/embedded so
   `go build ./...` and CI need no Node**; provide a documented rebuild command (e.g. `scripts/web-build.sh`
   or a Make target). Vendor/pin React + React Router (self-hosted, no CDN at runtime — the panel must work
   offline, like the fonts).
2. **Persistent shell:** a React root that renders the sidebar nav, topbar (theme toggle), and the
   **background-rain `<canvas>` as a component that is mounted once at the shell level and never unmounts
   on navigation** (so the rain is continuous and never reset). Keep dark default + localStorage theme
   (no FOUC) and the round-5 design system (reuse `web/assets/css/*` tokens/components). The mobile nav
   drawer must be usable (same correctness as T-0046 — build it right here; see that task's root-cause).
3. **Routing:** React Router (client-side) with a route per nav item (`/machines`, `/watchdog`,
   `/monitor`, `/evidence`, `/terminal`, `/backup`, `/projects`, `/baseline`, `/maneuver`, `/commands`).
   Deep-linking and refresh must work. **Transition handling:** un-ported routes may fall back to a
   full-page load of the existing legacy page so the panel stays fully functional during the migration;
   ported routes are client-side. Establish a **route registry/pattern** (one component file + one
   registration per page) so later ports touch disjoint files and run in parallel.
4. **Serving (`internal/panel/server.go`, `templates.go`):** serve the SPA — the shell `index.html` + the
   bundle under `/assets/`, and a catch-all that returns the SPA shell for any non-API, non-asset app route
   (so client routes resolve on refresh/deep-link). Keep **all** `/api/*` and `/ws/events` handlers and the
   auth-proxy integration exactly as-is (the SPA sits behind Authelia like today). Update `web/embed.go` for
   the new asset paths. Do not change panel business logic.
5. **Port the Machines route end-to-end** as the reference: list + live updates via a React hook
   subscribing to `/ws/events`, the token-enroll panel, and the public-IP refresh buttons/endpoint
   (T-0045) — all working in React. This proves the live-data + action pattern for the remaining ports.

## Out of scope

- Porting the other 9 pages (separate parallel tasks T-0048+ after this merges) — but leave them reachable
  (legacy full-load fallback). No back-end/API/logic changes, no new features, no redesign (same look via
  the existing CSS), no changes to the control channel or Go business logic. Don't restyle/replace vendored
  xterm (its React wrapper is a later port task).

## Done means

- Navigating between **ported** routes does not reload the page; the shell (nav/theme/rain) stays mounted
  and the rain is continuous. Client routing works incl. deep-link + refresh; mobile drawer is usable;
  dark default + localStorage theme, no FOUC; fonts/React/Router all self-hosted (no runtime CDN).
- Machines route fully works (live SSE/WS, enroll panel, public-IP refresh). Un-ported routes still load.
- `go build ./...` builds with **no Node**; the rebuild command is documented. `gofmt -l .` empty,
  `go vet ./...` clean, `scripts/test.sh server` passes (tests updated for SPA serving: shell + bundle 200,
  catch-all returns shell for app routes, APIs unchanged).

## Facts this brief relies on

- Current panel is server-rendered html/template: `web/_src/shell.html` + `web/pages/*.html`, parsed in
  `internal/panel/templates.go`, served by `internal/panel/server.go` (`/assets/` static, page handlers);
  embedded via `web/embed.go`. Design system + rain: `web/assets/css/*`, `web/assets/js/{app,bg}.js`
  (T-0040). Live data: `/ws/events` + `/api/*` (e.g. `internal/panel/api.go:49-52`); public-IP refresh
  `POST /api/machines/{id}/refresh-public-ip` (T-0045). No JS build toolchain exists today (no package.json).

## Notes for the doer

- **Large — split into clear commits** (build setup; shell+rain+theme+router; serving+embed+tests;
  Machines port) and report the split. This is the pattern every later page copies — keep the route
  component contract small and documented. Preserve every API/WS contract and the auth-proxy behavior. If a
  back-end handler genuinely must change beyond serving the SPA, STOP and report. Run `./scripts/wf start
  T-0047` before committing; commit inside touches + the log. Do NOT push/merge/branch-delete or edit
  decisions/generated/other tasks/`state:` (except via `wf start`).
