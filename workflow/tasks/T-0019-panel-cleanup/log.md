# T-0019 panel-cleanup - log

## Doer entry

- Deleted the dead stamp pipeline: `scripts/stamp-web.py`, all ten `web/_src/pages/*.html`, and eleven pre-rendered
  root pages (`web/{index,machines,maneuver,commands,terminal,projects,baseline,monitor,watchdog,evidence,backup}.html`).
- **Kept `web/styleguide.html` (deviation from the brief, needs a supervisor decision).** The brief lists it among the
  pages to delete, but it is not stamp output (no GENERATED banner, hand-written reviewer aid, T-0004 review-1 said
  keep it) and it is a live route: `internal/panel/server.go:69` `GET /styleguide.html` serves it from `web.FS`, and
  `internal/panel/server_test.go:51` requires a 200. Deleting it needs a change to `server.go`/`server_test.go`, which
  are outside touches and the brief's "no backend change beyond embed.go". If it should go, the supervisor/next task
  must drop that route and test line, then `git rm web/styleguide.html` and remove it from the embed.
- `web/embed.go`: `//go:embed *.html assets` became `//go:embed styleguide.html assets` (assets + the one served
  static page); doc comments updated. Without the root `*.html` the old pattern would not compile.
- `web/assets/js/app.js`: the three `.focus()` calls in the row-menu code (first menu item on open, trigger button on
  Escape, ArrowUp/Down between items) now pass `{ preventScroll: true }`, so opening a below-the-fold menu does not
  scroll the page and trip the capture scroll listener that closes it. `node --check` passes. Not exercised in a real
  browser (none driven here); the change is the fix recorded in T-0011's log.
- Verified by running `./bin/fleet-server` (FLEET_LISTEN_ADDR=127.0.0.1:16789, scratch data dir): `/`, `/index.html`,
  and the ten page URLs plus `/styleguide.html` all 200 text/html (templates); `/nope.html`, `/embed.go`,
  `/_src/shell.html`, `/pages/machines.html`, `/assets/`, `/fragments/nope` all 404; `/assets/js/app.js`,
  `/assets/css/components.css`, `/assets/js/live.js` 200. Server stopped and scratch dir removed.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/build.sh` ok, `scripts/test.sh server` all packages ok.
- Remaining references (outside touches, left alone): comment in `web/templates.go:8` ("pre-rendered *.html pages
  embedded in FS"), and `workflow/design/panel.md` / `panel-wiring.md` describing the stamp script. `git grep` shows no
  code reference to the deleted files.

<!-- wf: commits=38933fa left=- tests=server:pass blocked=no -->
