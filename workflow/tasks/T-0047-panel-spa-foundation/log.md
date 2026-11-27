# T-0047 panel-spa-foundation: log

## Entry 1 (doer)

- **Build setup (`296bd00`):**
  - Added pinned JS toolchain in `web/package.json` (`esbuild@0.28.2`, `react@19.3.1`, `react-dom@19.3.1`, `react-router-dom@7.18.7`) with `web/package-lock.json` committed, `web/.gitignore` ignoring `node_modules/`, `web/build.mjs`, `web/build.sh` (documented rebuild command: `./web/build.sh` or `npm --prefix web ci && npm --prefix web run build`), and `web/README.md`.
  - Self-hosts React, React Router, fonts (`../fonts/*` preserved as external in `web/assets/dist/app.css`), and vendored xterm (no runtime CDN).
- **Persistent shell, continuous Matrix rain, theme, mobile drawer fix, and router (`4263c9e`):**
  - `web/src/components/Shell.jsx` + `web/src/components/BgRain.jsx` + `web/src/components/Icons.jsx`: mounts `<BgRain />` (`#bg-canvas`), SVG sprite, sidebar nav, topbar theme toggle, and toast region once at the layout root around `<Outlet />` so route changes never unmount the canvas or reset the rain drops.
  - `web/src/lib/theme.jsx` + `web/dist/index.html`: dark default with `localStorage.getItem('fleet-theme')` blocking `<head>` script to avoid FOUC.
  - `web/assets/css/base.css` + `web/src/components/Shell.jsx`: fixed the T-0046 stacking-context trap by rendering `.nav-scrim` (`z-index: 50`) inside `.app` alongside `.sidebar` (`z-index: 60`, `pointer-events: auto`), locking body scroll (`body.nav-open`) at `<= 959px`, closing on scrim/Esc/nav-item, and managing focus/`aria-expanded`. Also scoped the legacy shell fallback scrim to `body.nav-open .app:not(.app--spa)::after`.
  - `web/src/routes.js` + `web/src/App.jsx`: documented route registry (`ROUTES`, `NAV_GROUPS`, `routeHref`, `isRoutePorted`) for parallel page ports (T-0048..T-0051); un-ported routes (`component: null`) link to legacy `/*.html` and redirect via `<LegacyFallback>` if entered via client URL.
  - `web/_src/shell.html`: pointed the legacy sidebar Machines link to `/machines`.
- **Go serving, embed, and tests (`3a92afe`):**
  - `web/embed.go`: embedded `dist/index.html` alongside `styleguide.html` and `assets` (which includes `assets/dist/app.{js,css}`).
  - `internal/panel/templates.go` + `internal/panel/server.go`: marked `machines` with `spa: true`; served `s.spa` at `/`, `/index.html`, `/machines`, and the non-API/non-asset/non-source catch-all `GET /` (preserving all `/api/*`, `/ws/*`, `/fragments/*`, and legacy `/*.html` routes; added 404 guards for unmatched `/api/`, `/ws/`, `/fragments/`, and internal template/source paths).
  - `internal/panel/server_test.go` + `internal/panel/machines_test.go`: updated for SPA shell, bundle serving, catch-all routes, and legacy fallback checks.
- **Machines route port (`3e947c0`):**
  - `web/src/hooks/useMachines.js` + `web/src/lib/events.jsx`: shared shell `/ws/events` WebSocket provider with reconnect backoff and `useMachines()` hook for live registry + summary updates.
  - `web/src/pages/MachinesPage.jsx`: ported the full Machines view (summary tiles, filter toolbar, sticky-first table with private/public IPs, disk meters, row action menu, SSH onboarding form, Re-provision/Remove dialogs, token-enrollment form/code/countdown/commands + enrolled agents table + Stop/Detach dialogs, and per-row + bulk `Refresh public IPs` wired to `POST /api/machines/{id}/refresh-public-ip`).
  - Note: on this base commit (`1c29923`), T-0045 is still `state: ready` (not merged yet), so `POST /api/machines/{id}/refresh-public-ip` is wired on the front end per spec and will resolve once T-0045 merges; `internal/panel/api.go` was left untouched inside `touches`.
- **Verification:**
  - `./web/build.sh` built `web/assets/dist/app.js` (314.0kb), `web/assets/dist/app.css` (39.6kb), and `web/dist/index.html` cleanly.
  - `go build ./...` passed with `node` removed from `PATH` (`node absent in PATH`).
  - `gofmt -l .` empty, `go vet ./...` clean, `./scripts/test.sh server` passed (`internal/panel` and `cmd/fleet-server` passed uncached after code edits; full `server` group green).

wf: T-0047 done
