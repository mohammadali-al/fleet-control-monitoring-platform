# T-0039 Log

## 2026-11-27 — Single-domain auth path (`--single-domain`)

- Ran `./scripts/wf start T-0039` to create and switch to `task/T-0039` with `.githooks` wired.
- `deploy/install-server.sh`:
  - Added `--single-domain` and `--two-domain` CLI flags (`FLEET_DOMAIN_MODE=single|two`, defaulting to `two`), persisted in `install-answers.env` and restored across re-runs without overwriting CLI/`--answers` overrides.
  - In `--single-domain` mode, prompts for one DNS domain (`FLEET_PANEL_DOMAIN`, rejecting raw IPv4/IPv6 addresses in both modes), leaves `FLEET_AUTH_DOMAIN` empty, generates single-SAN internal TLS certs (`DNS:<panel-domain>`), and reports `https://<panel-domain>/authelia` as the auth URL.
  - Updated `render_caddyfile()` for single-domain mode: single hostname and `tls` block, `handle /authelia*` -> `reverse_proxy 127.0.0.1:${FLEET_AUTH_PORT}` (no `forward_auth`, prefix preserved), and `handle` -> `forward_auth 127.0.0.1:${FLEET_AUTH_PORT}` with `uri /authelia/api/authz/forward-auth` (`copy_headers Remote-User Remote-Groups Remote-Name Remote-Email`) -> `reverse_proxy 127.0.0.1:${FLEET_SERVER_PORT}`.
  - Updated `render_authelia_config()` for both `--proxy=native` (`tcp://127.0.0.1:${FLEET_AUTH_PORT}/authelia`) and `--proxy=docker` (`tcp://:${FLEET_AUTH_PORT}/authelia` for Compose port publishing), with `session.cookies[].domain: <panel-domain>`, `authelia_url: https://<panel-domain>/authelia`, and `default_redirection_url: https://<panel-domain>`. Default two-subdomain rendering remains unchanged.
- `deploy/proxy/Caddyfile`, `deploy/authelia/configuration.yml`, `deploy/proxy/compose.yml`: kept the two-subdomain layout as the active default and documented the single-domain `/authelia` configuration and `/authelia*` reserved path.
- `deploy/README.md` & `deploy/REQUIREMENTS.md`: documented `--single-domain` / `--two-domain`, the one-DNS-name requirement, IP-only being unsupported, `/authelia*` as the single reserved panel path, and that `upgrade-server.sh` preserves live proxy configs without automatic domain-mode migration.
- Verification: `shellcheck` (`0.10.0` in `/tmp/t0039-tools`, removed afterward) clean on `deploy/install-server.sh`; `bash -n deploy/install-server.sh` clean; `yaml.safe_load` valid on `deploy/authelia/configuration.yml`, `deploy/proxy/compose.yml`, and staged outputs; `DESTDIR` staging verified all 4 combinations (`single`/`two` x `native`/`docker`), IP-only rejection, and idempotent `FLEET_DOMAIN_MODE=single` re-run; confirmed no `/authelia` routes in `internal/panel/`. No live test box was available, so end-to-end browser/HTTP login against running Caddy+Authelia was not executed here.

wf: T-0039 done
