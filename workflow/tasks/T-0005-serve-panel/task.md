---
id: T-0005
title: serve-panel
state: ready
touches: [cmd/fleet-server/main.go, internal/config/config.go, internal/config/config_test.go,
          internal/panel/server.go, internal/panel/server_test.go, internal/panel/embed.go]
tests: [server]
after: [T-0001, T-0004]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Run ./bin/fleet-server, open http://localhost:6789 in a browser: the panel loads and every page is navigable (light/dark). Unknown paths return 404."
---

## Why

So the operator can run and check the app locally without deploying: `fleet-server` serves the static
panel on `:6789`. This is the local dev loop — one binary, open a browser. No real main server, no
auth proxy needed locally.

## What to build

**Backend (Go).**

1. **Embed the panel.** `internal/panel/embed.go` embeds the repo's `web/` directory with `//go:embed`
   (the static front-end from T-0004). Do **not** modify `web/` content.
2. **HTTP server** (`internal/panel/server.go`): `net/http` serving the embedded `web/`:
   - `/` → `web/index.html`; `/assets/...` and every `web/*.html` served with correct content types.
   - Unknown path → 404. No directory listing.
   - Clean startup/shutdown tied to context (graceful on SIGINT/SIGTERM, reusing T-0001's signal wiring).
3. **Config** (`internal/config/config.go`): add to `ServerConfig`:
   - `ListenAddr` — default **`:6789`** (D-0004), overridable by config/env.
   - `DataDir` — the base data directory (D-0006): resolve from config, env `FLEET_DATA_DIR`, else the
     default (a `data/` dir one level above the binary). **Create it on first run**; just resolve +
     ensure it exists and log it (no files written into it yet).
4. **Wire** `cmd/fleet-server/main.go` to start the panel server on `ListenAddr` and log the URL.

## Out of scope

- Authentication/authorization (the panel sits behind Authelia/Pomerium; locally there is none). At
  most, read and pass through a forwarded-auth header for later display — do not build auth.
- Any backend API, live data, WebSocket, or converting pages to Go `html/template` (later tasks). Serve
  the static files **as-is**.
- Writing anything into `DataDir` beyond ensuring it exists.

## Done means

- `make build` then `./bin/fleet-server` logs the panel URL and serves **http://localhost:6789**.
- `GET /` returns the panel; `GET /machines.html` and `GET /assets/css/tokens.css` return 200 with
  correct content types; an unknown path returns 404.
- `ListenAddr` honored from config/env; `DataDir` resolved (default a `data/` dir above the binary),
  created if missing, and logged.
- Graceful shutdown on SIGINT; `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes
  (an `httptest` server test covers `/`, an asset, and a 404).

## Facts this brief relies on

- Panel served by `fleet-server` (Go `net/http`), **listens on `:6789`**, behind the auth proxy:
  `workflow/decisions/D-0004-deployment-basics.md`; `Fleet_Control_Monitoring_Platform_Architecture.md:549`.
- The static front-end exists under `web/` (merged T-0004): `workflow/tasks/T-0004-panel-frontend/task.md`.
- Base data directory, env `FLEET_DATA_DIR`, default a folder above the binary:
  `workflow/decisions/D-0006-data-directory.md`.
- Config loading + graceful shutdown already exist: `workflow/tasks/T-0001-project-skeleton/task.md`.

## Notes for the doer

- Stdlib only (`embed`, `net/http`); no web framework, no new module.
- Serve the committed `web/*.html` directly — they already work standalone. Keep relative asset paths.
- Resolve `DataDir` to an absolute path; create with `0o750`. Do not put it inside the repo.
