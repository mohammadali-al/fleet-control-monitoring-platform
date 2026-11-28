# T-0069 panel-favicon: log

## Doer entry

- **Favicon assets (`web/assets/`):**
  - Created `web/assets/favicon.svg` (32x32 viewBox) with a `#ff2a3c` rounded-square background (`rx="6"`, matching `--accent` / `--danger` in `web/assets/css/tokens.css`) and the centered white (`#ffffff`, `--on-accent`) `i-fleet` cube paths (`M12 3l8 4.5v9L12 21l-8-4.5v-9z` and `M12 12l8-4.5M12 12v9M12 12L4 7.5`) with `stroke-width="2"`, `stroke-linecap="round"`, and `stroke-linejoin="round"`.
  - Generated crisp supersampled PNG fallbacks: `web/assets/favicon-32x32.png` (32x32) and `web/assets/apple-touch-icon.png` (180x180). No external requests.
- **SPA `<head>`, legacy shell, and bundle (`web/build.mjs`, `web/_src/shell.html`, `web/dist/index.html`):**
  - Added `<link rel="icon" type="image/svg+xml" href="/assets/favicon.svg">`, `<link rel="icon" type="image/png" sizes="32x32" href="/assets/favicon-32x32.png">`, and `<link rel="apple-touch-icon" sizes="180x180" href="/assets/apple-touch-icon.png">` to the `<head>` template in `web/build.mjs`.
  - Added the matching `assets/favicon.svg`, `assets/favicon-32x32.png`, and `assets/apple-touch-icon.png` `<link>` tags to `web/_src/shell.html`.
  - Rebuilt the SPA bundle with `./web/build.sh`, regenerating `web/dist/index.html`.
- **Root `/favicon.ico` & tests (`internal/panel/server.go`, `internal/panel/server_test.go`):**
  - Registered `GET /favicon.ico` in `internal/panel/server.go` to redirect (`302 Found`) to `/assets/favicon.svg` so direct `/favicon.ico` requests neither 404 nor fall through to the SPA HTML catch-all.
  - Updated `internal/panel/server_test.go` (`TestServesPanel`, `TestSPAShellAndCatchAll`, `TestLegacyPagesRender`) to verify `/assets/favicon.svg` (`200`, `image/svg+xml`), `/assets/favicon-32x32.png` (`200`, `image/png`), `/assets/apple-touch-icon.png` (`200`, `image/png`), `/favicon.ico` (`302 -> /assets/favicon.svg` and `200`, `image/svg+xml` on follow), and favicon `<link>` references in the SPA and legacy HTML shells.
- **Verification:**
  - `gofmt -l .` empty.
  - `go vet ./...` clean.
  - `go build ./...` succeeded with `node` removed from `PATH`.
  - `go test -v ./internal/panel/...` passed; `./scripts/test.sh server` passed (all `cmd/fleet-server/...` and `internal/...` packages ok; note `internal/fleet` exhibited an unrelated intermittent timing flake in `TestPublicIPDispatchAndRefresh`/`TestDiskStateDispatch` on earlier runs before passing both standalone and in `./scripts/test.sh server`).

wf: T-0069 done
