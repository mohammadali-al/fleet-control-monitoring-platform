# T-0064 log

## Doer entry

**Built.**
- `deploy/install-server.sh`:
  - Pinned Grafana OSS `11.6.1` (`GRAFANA_VERSION="11.6.1"`) with pinned release tarball SHA-256 digests (`amd64`: `35a5efca8c535ce961217765faf8d6895c6d1c1587eaeec50d606a5cb218603b`, `arm64`: `6b45fc6c2e534aee02220c1e8ed792bf4bb44ceac2da5ba0cd66e9579496f73e`) plus upstream `.sha256` verification (`install_grafana_binary`), extracted to `/usr/local/share/grafana` with `/usr/local/bin/grafana` symlink in both `--proxy=native` and `--proxy=docker`.
  - Added `--grafana-port` (default `3000`, `FLEET_GRAFANA_PORT`) and `--grafana-dir` (default `/var/lib/fleet-grafana`, `FLEET_GRAFANA_DIR`), preflight port check and `/usr/local` `>= 1024 MiB` disk check, `fleet-grafana` system user, generated `secret_key`, `/etc/fleet-grafana/grafana.ini` (+ `.example`), provisioning, `fleet-grafana.service` (`GF_SERVER_ROOT_URL`, `GF_SERVER_SERVE_FROM_SUB_PATH=true`, `GF_AUTH_PROXY_ENABLED=true`, `GF_SECURITY_SECRET_KEY__FILE`), `/grafana/api/health` readiness check, and `fleet-grafana.service` ordering before `fleet-proxy`/`fleet-caddy`.
  - Updated `render_caddyfile` (both single-domain and two-domain modes) to proxy `handle /grafana*` through Authelia `forward_auth` (`Remote-User`, `Remote-Groups`, `Remote-Name`, `Remote-Email`) to `127.0.0.1:${FLEET_GRAFANA_PORT}`, and updated `render_authelia_config` with an explicit `^/grafana(/.*)?$` `two_factor` rule.
- `deploy/grafana/`:
  - `grafana.ini`: loopback `127.0.0.1:3000`, subpath `/grafana/`, `[auth.proxy]` trusting `Remote-User` from `127.0.0.1, ::1` with `auto_sign_up = true` and `disable_login_form = true`, `[security]` `allow_embedding = true`, and `default_home_dashboard_path` pointing to `fleet-overview.json`.
  - `provisioning/datasources/victoriametrics.yaml`: Prometheus-compatible VictoriaMetrics datasource (`http://127.0.0.1:8428`, uid `fleet-victoriametrics`).
  - `provisioning/dashboards/fleet.yaml` and `dashboards/{fleet-overview,fleet-agents}.json`: default dashboards for `fleet_probe_latency_ms`, `fleet_probe_ok`, `fleet_probe_loss_ratio`, and `fleet_probe_status_code`.
- `deploy/proxy/Caddyfile` & `deploy/authelia/configuration.yml`:
  - Added `handle /grafana*` with `forward_auth` -> Authelia and `reverse_proxy 127.0.0.1:3000` (plus updated single-domain reference comment); added `^/grafana(/.*)?$` `two_factor` rule in Authelia config.
- `internal/panel/server.go`:
  - Reserved `/grafana` and `/grafana/` (`http.NotFound`) so `fleet-server` never serves them via the SPA catch-all.
- `web/src/pages/MonitorPage.jsx` & rebuilt `web/assets/dist/app.js`:
  - Added a `Grafana` tab on the Monitor page embedding `/grafana/d/<uid>/<slug>?orgId=1&theme=<dark|light>&kiosk` in a same-origin iframe with dashboard selector (`Fleet Monitor — Overview` / `Per-Agent Breakdown`), reload button, and "Open in Grafana" link; rebuilt and committed the web bundle (`./web/build.sh`).
- `deploy/REQUIREMENTS.md` & `deploy/README.md`:
  - Documented Grafana port `3000` (loopback), `/var/lib/fleet-grafana`, `/etc/fleet-grafana`, `/usr/local` `>= 1 GiB` disk requirement, pinned `11.6.1` asset/checksums, `/grafana*` reserved subpath, and SSO-shared embedding.

**Verification.**
- `bash -n deploy/install-server.sh`: clean.
- `shellcheck deploy/install-server.sh` (v0.10.0 in `/tmp/sc`, removed after): clean.
- Dashboard JSON parse check (`fleet-overview.json`, `fleet-agents.json`): valid JSON.
- `./web/build.sh`: rebuilt `web/assets/dist/app.js` (497.9kb) and `web/assets/dist/app.css` (39.8kb).
- `gofmt -l .`: empty.
- `go build ./...` (with `node` stripped from `PATH`): clean.
- `go vet ./...`: clean.
- `./scripts/test.sh server`: all server & internal Go packages passed.
- Installer not run against this host (real-box check deferred to supervisor).

wf: T-0064 done
