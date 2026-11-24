---
id: T-0019
title: panel-cleanup
state: merged
touches: [web/embed.go, web/assets/js/app.js, scripts/stamp-web.py, web/machines.html, web/maneuver.html,
          web/commands.html, web/terminal.html, web/projects.html, web/baseline.html, web/monitor.html,
          web/watchdog.html, web/evidence.html, web/backup.html, web/styleguide.html, web/index.html,
          web/_src/pages/machines.html, web/_src/pages/maneuver.html, web/_src/pages/commands.html,
          web/_src/pages/terminal.html, web/_src/pages/projects.html, web/_src/pages/baseline.html,
          web/_src/pages/monitor.html, web/_src/pages/watchdog.html, web/_src/pages/evidence.html,
          web/_src/pages/backup.html]
tests: [server]
after: [T-0011]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Run ./bin/fleet-server and open http://localhost:6789: all pages still render (light/dark). The row 'More' menu no longer scrolls the page/closes itself."
---

## Why

T-0011 replaced the stamp workflow with Go `html/template`, leaving dead files that the wiring doer
couldn't delete: `scripts/stamp-web.py`, `web/_src/pages/*`, and the pre-rendered `web/*.html`. Remove
them cleanly (keep the build green) and fix a small UI bug from T-0004.

## What to build

**Front-end / build cleanup.** **Deleting files IS authorized by this brief** (overrides the no-delete
rule for the files in `touches`).

1. **Delete the dead stamp pipeline and pre-rendered pages:** `scripts/stamp-web.py`, every
   `web/_src/pages/*.html`, and the pre-rendered root pages `web/*.html` (machines, maneuver, commands,
   terminal, projects, baseline, monitor, watchdog, evidence, backup, styleguide, index). **Keep**
   `web/_src/shell.html` (now the template layout), `web/pages/*`, `web/templates.go`, `web/assets/**`.
2. **Fix `web/embed.go`** so it no longer embeds the deleted root `*.html` and the build stays green —
   embed only what is still served (assets, and whatever the live panel needs). Confirm the panel still
   serves every page via the templates after the deletions.
3. **Fix the `app.js` row-menu scroll bug** (from T-0004): a row-menu popup that lands below the fold
   scrolls the page, and the scroll listener then closes it. Apply `focus({preventScroll:true})` (or the
   equivalent) so opening a menu doesn't scroll/self-close.

## Out of scope

- Any new pages or features; any backend change beyond `web/embed.go`.
- Touching `web/pages/*` templates or `web/_src/shell.html` content (they stay).

## Done means

- `scripts/stamp-web.py`, `web/_src/pages/*`, and the pre-rendered `web/*.html` are **gone**; `git grep`
  shows nothing still references them.
- `scripts/build.sh` builds; `./bin/fleet-server` serves **every** page (templates), light/dark, 404s intact.
- The row-menu opens without scrolling the page or closing itself.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes.

## Facts this brief relies on

- T-0011 replaced the stamp workflow with `html/template`; these files are now dead (its log lists them):
  `workflow/tasks/T-0011-panel-wiring/log.md`; `workflow/design/panel-wiring.md`.
- The `app.js` row-menu scroll quirk + the `focus({preventScroll:true})` fix: `workflow/tasks/T-0011-panel-wiring/log.md`.
- The live template system to preserve: `web/templates.go`, `web/pages/`, `web/_src/shell.html`.

## Notes for the doer

- If the environment **blocks file deletion**, do everything else (embed.go + app.js + verify), list the
  exact files to delete in your report, and the **supervisor will apply the deletions** — do not work
  around a refusal.
- Verify by actually building and serving, not just by tests.
