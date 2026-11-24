# T-0005 log

## 2026-11-24 doer — BLOCKED, no code written

The brief's embed plan cannot work as written. `internal/panel/embed.go` is to `//go:embed` the repo's
top-level `web/` directory, but `go:embed` only accepts paths inside the package directory (no `..`,
no symlinks). Verified: a `//go:embed ../../web` in `internal/panel/embed.go` fails
`go vet` with `internal/panel/embed.go:5:12: pattern ../../web: invalid pattern syntax`.

Options for the supervisor (none taken by me, as each leaves the brief's `touches` or the "do not
modify web/" instruction):
1. Add a new file `web/embed.go` (`package web`, `//go:embed *.html assets`, exporting an `embed.FS`);
   `internal/panel` imports it. New file, nothing existing under `web/` is modified, but it is under
   `web/` and needs to be added to `touches`.
2. Copy/sync `web/` into `internal/panel/` at build time (needs `scripts/build.sh` in `touches`,
   duplicates content, and the tests need the copy present).
3. Serve from disk (`os.DirFS`) instead of embedding; contradicts the brief's embed bullet and the
   single-binary goal.

Nothing implemented, no temp files left.

<!-- wf: commits=- left=- tests=server:not-run blocked=go:embed cannot reach ../../web from internal/panel; brief needs revising -->

## 2026-11-24 doer — done (after brief revision: web/embed.go in touches)

Built per the revised brief:
- `web/embed.go` (`package web`, `//go:embed *.html assets`, `var FS embed.FS`) — the only `web/` change.
- `internal/panel/server.go`: `Handler(fs.FS)` (GET/HEAD only; `/` -> index.html; non-regular files incl.
  directories -> 404, so no listing; Content-Type from file extension), `URL`, `Listen`, `Serve(ctx, ln, h, log)`
  with graceful shutdown (5s) on ctx cancel.
- `internal/config/config.go`: `ServerConfig.ListenAddr` (json `listen_addr`, default `:6789`, env
  `FLEET_LISTEN_ADDR`) and `DataDir` (json `data_dir`, env `FLEET_DATA_DIR`); env beats file.
  `ResolveDataDir(exePath)`: configured/env value, else `<dir of binary>/../data`; made absolute and created 0o750.
  Nothing is written into it.
- `cmd/fleet-server/main.go`: resolves + logs data dir, listens, logs the panel URL, serves until
  SIGINT/SIGTERM (`signal.NotifyContext`), then shuts down gracefully.
- Tests: `internal/panel/server_test.go` (/, /index.html, /machines.html, css/js content types, 404s incl.
  directories and `..`, HEAD/POST, URL, shutdown on cancel); config tests for defaults/env/file and DataDir.

Checks: `gofmt -l .` empty; `go vet ./...` clean; `scripts/test.sh server` passes; `scripts/build.sh` ok.
Live run of `./bin/fleet-server` (FLEET_DATA_DIR in a scratch dir): `/` 200 text/html, `/machines.html` 200
text/html, `/assets/css/tokens.css` 200 text/css, `/assets/js/app.js` 200 text/javascript, `/nope` 404,
`/assets/` 404; SIGINT logged "shutting down" and the process exited. Default data dir verified with a copy of
the binary at `<scratch>/inst/bin`: created `<scratch>/inst/data` (drwxr-x---). Scratch dirs removed.

Note for the supervisor: running `./bin/fleet-server` from this repo with no override defaults DataDir to
`<repo>/data` (one level above `bin/`), which is inside the repo tree and not in `.gitignore`
(`.gitignore` is not in touches). Suggest adding `/data/` to `.gitignore` or setting `FLEET_DATA_DIR` for local dev.

<!-- wf: commits=3efd7e3 left=- tests=server:pass blocked=no -->
