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
