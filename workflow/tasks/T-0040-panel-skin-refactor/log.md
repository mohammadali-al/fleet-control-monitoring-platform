## 2026-11-27: T-0040 panel-skin-refactor

**What changed (split into 2 logical commits).**
- **Commit (a) — base layout, theme, self-hosted fonts, swappable background, design system CSS:**
  - `web/assets/fonts/*.woff2` + `web/assets/css/fonts.css`: self-hosted `Space Mono` (400, 700, latin + latin-ext) and `Red Hat Mono` (variable 400..700, latin + latin-ext); zero external font requests.
  - `web/assets/css/tokens.css`, `base.css`, `components.css`, `live.css`: Round-5 design system (Void Black `#09090b` + Bone-White `#f4f4f0` + Signal Red `#ff2a3c` dark palette as default; Warm Paper `#f3efe6` + Carbon Ink `#181a18` + Signal Orange `#d9480f` light palette; `Space Mono` uppercase headings/nav/stats + `Red Hat Mono` body/tables/code; carbon-ink/graphite-bone outlines + offset block-shadow frames; covers every component including nav, stat tiles, tables, tabs, dialogs, forms, xterm terminal chrome, watchdog ladder/policy editor, evidence, backup, maneuver timeline, and monitor charts/overlays).
  - `web/assets/css/bg.css` + `web/assets/js/bg.js`: swappable background layer keyed by `<html data-bg="matrix">` (`#bg-canvas`, `pointer-events: none`, `aria-hidden="true"`, DPR capped at `<= 2`, `prefers-reduced-motion` static frame + theme redraw, pauses `requestAnimationFrame` on `document.hidden` via `visibilitychange`).
  - `web/_src/shell.html` + `web/assets/js/app.js`: `<html data-theme="dark" data-bg="matrix">` default dark, inline `<head>` script reading `localStorage.getItem('fleet-theme')` in `try/catch` before first paint (fallback `'dark'`, no flash), client-side-only `.theme-toggle` persistence (`'fleet-theme'`), keeping all functional hooks intact.
- **Commit (b) — page-by-page template skinning & tests:**
  - `web/pages/machines.html`: replaced inline enroll-code style with `.enroll-code` and stacked OS command blocks.
  - `web/pages/projects.html`: replaced inline `background:var(--accent-soft)` with CSS `a.list__item[aria-current="true"]` styling.
  - `web/pages/commands.html`: styled `#targets` with `.list--boxed` and target quick-select buttons with `.btn--sm`.
  - `web/pages/terminal.html`: added `.term-ph__rec` (`EVIDENCE RECORDING`) header badge and moved mount padding/height into `live.css` for `.term-ph`.
  - `web/pages/maneuver.html`: updated worker agent selection buttons to `.btn--sm` (preserving disabled Spikes pattern and `TestManeuverStub` hooks).
  - `web/pages/monitor.html`: removed stale `border-style:solid` override on `.chart-ph`.
  - `web/styleguide.html`: updated with `data-theme="dark"`, `data-bg="matrix"`, inline no-FOUC script, self-hosted `fonts.css`, `bg.css`, `bg.js`, `#bg-canvas`, and `.term-ph__rec`.
  - `web/pages/baseline.html`, `watchdog.html`, `evidence.html`, `backup.html`, `fragments.html`: verified all components inherit the Round-5 skin via the shared layout and stylesheets with all live hooks preserved.
  - `internal/panel/server_test.go`: added assertions in `TestServesPanel` and `TestAllPagesRender` for self-hosted fonts, `bg.css`, `bg.js`, `data-theme="dark"`, `data-bg="matrix"`, `localStorage.getItem('fleet-theme')`, `#bg-canvas`, and absence of external Google Fonts URLs.

**Checks.**
- `gofmt -l .`: empty
- `go vet ./...`: clean
- `node --check web/assets/js/app.js web/assets/js/bg.js web/assets/js/live.js`: pass
- `google-chrome --headless --dump-dom` on `web/styleguide.html`: verified `data-theme="dark"`, `data-bg="matrix"`, `#bg-canvas` sized, `aria-pressed="true"` on `.theme-toggle`.
- `./scripts/test.sh server`: pass (all packages)
- No backend Go files modified (`git diff main -- '*.go'` touches only `internal/panel/server_test.go`).

wf: T-0040 done
