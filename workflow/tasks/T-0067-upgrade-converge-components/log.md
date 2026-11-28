# T-0067 Log — upgrade-converge-components

## Sharing approach

To keep pinned component versions, SHA-256 checksums, binary installers, and config/unit templates in a single source of truth without duplicating them or breaking standalone `curl | bash` execution or `.github/workflows/release.yml` asset staging, `deploy/install-server.sh` now wraps its top-level CLI parsing, `trap cleanup EXIT`, `--upgrade` delegation, and `main()` execution behind a `FLEET_INSTALL_LIB_ONLY=1` guard (mirroring `deploy/uninstall-server.sh`).

`deploy/upgrade-server.sh` removes its previously duplicated `CADDY_VERSION` / `AUTHELIA_VERSION` constants and sources `install-server.sh` in library mode (`FLEET_INSTALL_LIB_ONLY=1`), resolving it in order from:
1. `$STAGE_DIR/install-server.sh` (downloaded and SHA256-verified from the target GitHub release, or copied from `--bin-dir` if present)
2. `$UPGRADE_SCRIPT_DIR/install-server.sh` (local repo `deploy/` checkout)
3. `/usr/local/lib/fleet/install-server.sh` (installed alongside `upgrade-server.sh`)
4. Raw GitHub fallback (`deploy/install-server.sh` at `$RESOLVED_TAG` / `main`)

Both `install-server.sh` and `upgrade-server.sh` also keep `/usr/local/lib/fleet/install-server.sh` and `/usr/local/lib/fleet/upgrade-server.sh` refreshed when staged.

## Changes made

- `deploy/install-server.sh`:
  - Factored `parse_args()`, `handle_upgrade_delegation()`, `validate_install_modes()`, and `main()` behind `if [ "${FLEET_INSTALL_LIB_ONLY:-0}" != "1" ]`.
  - Updated `fetch_release_binaries()` and `main()` to also download/verify/stage `install-server.sh` into `/usr/local/lib/fleet/install-server.sh`.
  - Updated `install_native_proxy_binaries()` and `install_grafana_binary()` symlink check to set `CHANGED=1` when changes occur.
- `deploy/upgrade-server.sh`:
  - Extended `load_answers()` + `resolve_upgrade_answers()` to load domain mode, panel/auth domains, ports, retention, and TSDB/Grafana dirs from `/etc/fleet-server/install-answers.env` (inferring `FLEET_DOMAIN_MODE` for pre-T-0039 installs and persisting newly introduced defaults via `ensure_new_keys()`).
  - After binary swap + `config.json.example` refresh (via `render_server_config_example`) + `fleet-server migrate` + CA check + `fleet-server` restart, added idempotent convergence for:
    - VictoriaMetrics (`:8428`): `fleet-tsdb` user, `$FLEET_TSDB_DIR`, pinned binary via `install_victoriametrics_binary`, systemd unit via `render_victoriametrics_service`, enable/start/restart on change, `/health` verification.
    - Grafana (`:3000`): `fleet-grafana` user, `$FLEET_GRAFANA_DIR` + `secret_key` (preserved if present), pinned binary via `install_grafana_binary`, `/etc/fleet-grafana/grafana.ini.example` (preserving live `grafana.ini`), provisioning datasource + dashboard provider + dashboards via `install_grafana_dashboards`, systemd unit via `render_grafana_service`, enable/start/restart on change, `/grafana/api/health` verification.
    - `fleet-server.service` & `fleet-server.env`: ensures `FLEET_MONITOR_STORE_ADDR` and unit dependencies are present, restarting `fleet-server` only when changed.
    - Proxy (`--proxy=native` or `--proxy=docker`): backs up existing `Caddyfile`, `authelia/configuration.yml`, `compose.yml`, proxy systemd units, and native `caddy`/`authelia` binaries into `$BACKUP_DIR` before re-rendering from saved answers; updates `Caddyfile` + `authelia/configuration.yml` + proxy units idempotently via `put_file`; upgrades Docker Compose stack (restarting if proxy configs changed) or native binaries; validates systemd unit state + Authelia health + Caddy HTTPS response on `FLEET_PANEL_PORT` with full rollback on failure; preserves `/var/lib/fleet-server/ca/`, `/var/lib/fleet-proxy/` (secrets, certs, `users.yml`, `db.sqlite3`), `/var/lib/fleet-victoriametrics/`, `/var/lib/fleet-grafana/`, and live `config.json`.
- `deploy/README.md`, `deploy/REQUIREMENTS.md`: documented upgrade convergence of bundled components, proxy route re-rendering, and shared pinned-version sourcing from `install-server.sh`.

## Verification

- `bash -n deploy/install-server.sh deploy/upgrade-server.sh`: pass.
- `shellcheck deploy/upgrade-server.sh deploy/install-server.sh` (ShellCheck 0.10.0 installed temporarily to `/tmp` and cleaned up): pass (0 issues).
- Library-mode smoke test (`FLEET_INSTALL_LIB_ONLY=1`) and `--help` checks: pass (`LIB_SMOKE_OK`, `HELP_OK`).
- Idempotency check (reasoning): second run exits early if already on `$RESOLVED_TAG` without `--force`; with `--force` or `--bin-dir`, binary version checks skip downloads, `put_file` compares files with `cmp -s` so `CHANGED` stays `0`, existing `config.json` / `grafana.ini` / `secret_key` / `ca/` / `/var/lib/fleet-proxy` are untouched, and running services are not restarted unnecessarily.

wf: T-0067 done
