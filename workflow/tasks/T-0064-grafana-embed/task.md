---
id: T-0064
title: grafana-embed
state: merged
touches: [deploy/install-server.sh, deploy/proxy/Caddyfile, deploy/authelia/configuration.yml,
          deploy/grafana/, deploy/REQUIREMENTS.md, deploy/README.md,
          web/src/pages/MonitorPage.jsx, internal/panel/server.go]
tests: []
after: [T-0055]
shares: {}
model: {claude: "sonnet high"}
operator_check: "A fresh install provisions Grafana as a pinned native binary (like caddy/authelia/victoria-metrics), reverse-proxied by Caddy under the panel domain with Authelia SSO (no second login — the operator's panel session authenticates Grafana via the auth proxy), a VictoriaMetrics datasource + default dashboards pre-provisioned, and the Monitor page embeds the Grafana dashboard(s). --proxy=docker also works."
---

## Why

The architecture calls for **Grafana embedded in the panel, SSO-shared** (§3 L108/L136, §12.1 L556), but no
Grafana exists (audit GAP-01); the Monitor page shows a placeholder. Deploy Grafana the same native-binary
way as the rest of the stack (D-0009) and embed it with Authelia SSO, backed by the VictoriaMetrics store
(T-0055).

## What to build

1. **Install Grafana (native).** In `deploy/install-server.sh`, install Grafana as a **pinned static
   binary** (pinned version + SHA256 + checksum verification + systemd unit + loopback port, e.g.
   `127.0.0.1:3000`, state dir) — mirror the VictoriaMetrics/caddy/authelia install pattern. Config +
   provisioning files live under `deploy/grafana/` (and are rendered/installed): a **VictoriaMetrics
   datasource** (Prometheus-compatible, `http://127.0.0.1:8428`) and **default dashboards** for the fleet
   monitor metrics (`fleet_probe_*`).
2. **SSO via the auth proxy.** Configure Grafana for **auth-proxy header auth** (trust `Remote-User` from
   Authelia) so the operator's existing panel session authenticates Grafana — no second login. In
   `deploy/proxy/Caddyfile`: a `handle /grafana*` (or a subpath) that `forward_auth` → Authelia then
   `reverse_proxy` → Grafana (serve Grafana under that subpath; set `GF_SERVER_ROOT_URL`/
   `serve_from_sub_path`). Update `deploy/authelia/configuration.yml` if an access rule is needed. Keep it
   working for both `--proxy=native` and `--proxy=docker`.
3. **Embed in the panel.** `web/src/pages/MonitorPage.jsx`: embed the Grafana dashboard(s) (iframe under the
   same origin/subpath) as a tab/section, replacing the placeholder. Reserve the `/grafana*` path in
   `internal/panel/server.go` if needed (fleet-server must not serve it). Rebuild + commit the web bundle.
4. **Docs.** `deploy/REQUIREMENTS.md` (Grafana port + pinned version/checksum + disk) and `deploy/README.md`
   (Grafana is provisioned + embedded, SSO-shared).

## Out of scope

- No change to the VictoriaMetrics store/ingest (T-0055) or the in-panel React charts; no custom
  dashboard-authoring UI (ship a sensible default set); no History/semantic-zoom work (that's T-0065).

## Done means

- A fresh `--proxy=native` install runs Grafana (pinned binary, systemd), Caddy proxies it under the panel
  domain with Authelia SSO (no second login), the VictoriaMetrics datasource + default dashboards are
  provisioned, and the Monitor page embeds the dashboard. `--proxy=docker` path also provisions Grafana.
- `shellcheck`/`bash -n` clean on `install-server.sh`; `go build ./...` ok (no Node); `gofmt`/`go vet`
  clean; web bundle rebuilt + committed; REQUIREMENTS/README updated.

## Facts this brief relies on

- Audit: GAP-01 §3 L108/L136, §12.1 L556 (`web/pages/monitor.html:74` placeholder). Native pinned-binary +
  installer pattern: `deploy/install-server.sh` (caddy/authelia, and VictoriaMetrics from T-0055),
  `workflow/decisions/D-0009-default-proxy-native.md`. Proxy + forward-auth: `deploy/proxy/Caddyfile`,
  `deploy/authelia/configuration.yml`. VictoriaMetrics datasource: `http://127.0.0.1:8428` (T-0055). SPA
  Monitor page: `web/src/pages/MonitorPage.jsx` (T-0058).

## Notes for the doer

- Pin Grafana (version + SHA256) and verify like the other binaries; keep the native + docker paths in sync.
  Test the installer parts with shellcheck/bash -n (no live box here — supervisor runs the real-box check).
  Rebuild + commit the bundle. Run `./scripts/wf start T-0064` before committing; commit inside touches +
  the log. Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via
  `wf start`).
