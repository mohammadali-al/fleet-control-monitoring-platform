# Deploying the Fleet main server

Reference deployment for the Fleet main server: `fleet-server` as a systemd service bound to loopback (`127.0.0.1:6789`) behind a TLS + 2FA auth proxy (Caddy + Authelia), paired with loopback VictoriaMetrics (`127.0.0.1:8428`) and Grafana (`127.0.0.1:3000`) served under `/grafana` with shared Authelia SSO. The panel keeps no accounts of its own (D-0004); it trusts the `Remote-User` header forwarded by the proxy.

```
operator browser --HTTPS--> Caddy :443 --forward auth--> Authelia 127.0.0.1:9091
                               |  (password + TOTP; sets Remote-User)
                               +--/grafana* (HTTP, loopback)--> Grafana 127.0.0.1:3000 --> VictoriaMetrics 127.0.0.1:8428
                               +--/*        (HTTP, loopback)--> fleet-server 127.0.0.1:6789

fleet-server --mTLS (internal CA), server_dial--> fleet-agent <machine IP>:7443   (not via the proxy)
fleet-agent  --mTLS (internal CA), agent_dial---> fleet-server :7444             (token-enrolled agents)
new machine  --HTTPS (pinned pubkey + token)----> fleet-server :7445             (token enrollment; bypasses proxy)
fleet-server --SSH, onboarding only-----------> new machine :22
```

## Layout (deployed host)

| Component | Location | Notes |
|---|---|---|
| `fleet-server` binary & unit | `/usr/local/bin/fleet-server`, `/etc/systemd/system/fleet-server.service` | Managed by systemd (`fleet` user) |
| Server config & env | `/etc/fleet-server/config.json`, `fleet-server.env`, `install-answers.env` | Live files never overwritten on upgrade (`*.example` updated alongside) |
| Server data dir (`FLEET_DATA_DIR`) | `/var/lib/fleet-server` | Registry, `ca/` (**CA private key**), `SCHEMA_VERSION`, backups |
| `fleet-agent` binaries | `/usr/local/share/fleet/agents/fleet-agent-<os>-<arch>` & `/usr/local/share/fleet/fleet-agent` | Multi-platform (`server.agent_binary_dir`) + host fallback |
| VictoriaMetrics binary, unit & data | `/usr/local/bin/victoria-metrics`, `/etc/systemd/system/fleet-victoriametrics.service`, `/var/lib/fleet-victoriametrics` | Loopback `127.0.0.1:8428` (`fleet-tsdb` user; `90d` default retention) |
| Grafana binary, unit, config & data | `/usr/local/share/grafana` (`/usr/local/bin/grafana`), `/etc/systemd/system/fleet-grafana.service`, `/etc/fleet-grafana/`, `/var/lib/fleet-grafana` | Loopback `127.0.0.1:3000` (`fleet-grafana` user); provisioned VictoriaMetrics datasource + default `fleet_probe_*` dashboards; embedded in **Monitor data → Grafana** with shared Authelia SSO (`Remote-User`) |
| Proxy home | `/opt/fleet/proxy/` | `compose.yml`, `Caddyfile`, `authelia/configuration.yml` (+ `*.example`) |
| Proxy mutable state | `/var/lib/fleet-proxy/` | `certs/`, `secrets/`, `authelia/` (`users.yml`, `db.sqlite3`), `caddy/` |
| Proxy systemd unit(s) | `/etc/systemd/system/fleet-proxy.service` | Native `fleet-caddy` + `fleet-authelia` (`--proxy=native`, default) or Docker Compose (`--proxy=docker`) |
| Upgrade & install helpers | `/usr/local/lib/fleet/upgrade-server.sh`, `install-server.sh` | Invoked by `install-server.sh --upgrade` or directly; `upgrade-server.sh` sources `install-server.sh` so pinned versions and component/proxy renderers stay in lockstep |

## Prerequisites

See [`deploy/REQUIREMENTS.md`](REQUIREMENTS.md) for the full host matrix, ports, and pinned Caddy (`2.10.0`), Authelia (`4.39.27`), VictoriaMetrics (`1.157.0`), and Grafana (`11.6.1`) versions.

- Linux host with `systemd`, root access, unified **cgroup v2**, **nftables**, `openssl`, and at least 2 GiB free on `/var/lib`.
- No Docker needed for the default `--proxy=native` mode (standalone Caddy + Authelia binaries). Use `--proxy=docker` for the Compose container stack, which then needs Docker + Compose v2.
- DNS name(s) pointing to the main server:
  - **Default (`--single-domain`, one DNS name):** **one** DNS name (`<panel-domain>`, e.g. `fleet.internal`, `*.ts.net`, or DuckDNS), serving Authelia under `https://<panel-domain>/authelia`.
  - **Two-subdomain (`--two-domain`):** `<panel-domain>` and `<auth-domain>` (e.g. `fleet.internal` and `auth.fleet.internal`).
  - **IP-only is unsupported:** Authelia requires a DNS-style cookie domain and rejects raw IP addresses in both modes.

## 1. Install from GitHub Release (recommended)

The repository is **public** — no token or auth header is needed. Run the bootstrap installer (checks prerequisites, offers to install missing OS packages, downloads + SHA256-verifies binaries, prompts for DNS layout — defaulting to one domain — plus domain/admin/TLS settings, generates secrets + CA, and starts `fleet-server` + `fleet-proxy`):

```bash
curl -fsSL https://raw.githubusercontent.com/mohammadali-al/fleet-control-monitoring-platform/main/deploy/install-server.sh \
  | sudo bash
```

> Only if you hit GitHub API rate limits (or run from a private fork): `export GH_TOKEN="<github-token-with-repo-read>"`, add `-H "Authorization: Bearer $GH_TOKEN"` to the `curl`, and use `sudo -E bash`.

Options:
- `--check`: run preflight checks only.
- `--proxy=native|docker`: native Caddy+Authelia binaries (default) or the Docker Compose proxy stack (`--proxy=docker`).
- `--single-domain` / `--two-domain`: skip the interactive DNS layout prompt (which asks and defaults to one domain) and force single-domain mode (`--single-domain`, also the `--non-interactive` / no-TTY default: **one** DNS name serving Authelia under `https://<panel-domain>/authelia`) or two-subdomain mode (`--two-domain`: separate `auth.<panel-domain>` subdomain). In single-domain mode, `/authelia*` is reserved for Authelia (and `/grafana*` is reserved in both modes for embedded Grafana); `fleet-server` never serves either path.
- `--yes` / `--no-install`: auto-approve or skip OS package installation.
- `--answers FILE` / `--non-interactive`: supply answers from an env file without prompting. Non-secret answers (including `FLEET_DOMAIN_MODE=single|two`) are saved to `/etc/fleet-server/install-answers.env` for future upgrades.
- `--tls-mode internal|acme`: generate an internal self-signed SAN cert (`internal`) or obtain public Let's Encrypt certs via Caddy (`acme`).
- `--bin-dir DIR` / `--no-start` / `DESTDIR=DIR`: install from a local `scripts/build.sh` build or stage into a packaging directory.

## 2. Install from a local build (development / offline)

```bash
./scripts/build.sh            # builds bin/fleet-server + bin/fleet-agent + bin/agents/fleet-agent-<os>-<arch>
sudo deploy/install-server.sh --bin-dir ./bin
```

`scripts/build.sh` runs `scripts/build-agents.sh` to cross-compile all four supported agent targets (`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`) into `bin/agents/`.

## 3. First login and 2FA enrollment

1. If using `--tls-mode internal`, trust `/var/lib/fleet-proxy/certs/tls.crt` in operator browsers (or replace `tls.crt` / `tls.key` with your CA-signed certificate and restart `fleet-proxy`).
2. Browse to `https://<panel-domain>/` — you are redirected to `https://<panel-domain>/authelia` (or `https://<auth-domain>/` in `--two-domain` mode).
3. Sign in with the admin user and password configured during installation, and register a TOTP device.
4. Read the one-time verification code from:

   ```bash
   sudo cat /var/lib/fleet-proxy/authelia/notification.txt
   ```

5. Scan the QR code with an authenticator app and enter the 6-digit code. Subsequent logins use password + TOTP.

## 4. Onboard the first agent

In the panel open **Machines** -> **Onboard machine** (SSH) or **Add agent (token)**.

- **Token enrollment (on by default):** Fresh installs write all three required fields (`server.enroll_addr: ":7445"`, `server.enroll_host: "<panel-domain>"`, and `server.agent_dial_addr: ":7444"`) into `/etc/fleet-server/config.json`, so **Add agent (token)** works out of the box. Make sure **both `7445/tcp`** (token-enrollment HTTPS with pinned-pubkey TLS, bypassing the proxy) **and `7444/tcp`** (agent-dial mTLS for token-enrolled agents) are reachable from machines you enroll (the installer checks local port availability only and does not add firewall accept rules). Override the default ports via `FLEET_ENROLL_PORT` (default `7445`) and `FLEET_AGENT_DIAL_PORT` (default `7444`) in the environment, `--answers` file, or the interactive custom-ports prompt.
- **SSH onboarding:** Enter the target machine's IP, an SSH user (root or passwordless `sudo -n`), and its role (`worker` or `monitor`). `fleet-server` pushes the matching `fleet-agent-<os>-<arch>` binary, signs a leaf cert from `/var/lib/fleet-server/ca/`, and connects over mTLS (`TCP 7443` in default `server_dial` mode).
- **Upgrades / existing installs:** Upgrades preserve live `/etc/fleet-server/config.json` (only `config.json.example` is refreshed), so existing deployments are not auto-flipped. To enable token enrollment on an existing host, add all three fields (`enroll_addr`, `enroll_host`, and `agent_dial_addr`) to `/etc/fleet-server/config.json` and restart `fleet-server` (`systemctl restart fleet-server`), or reinstall fresh.

**Back up `/var/lib/fleet-server/ca/`.** Losing the CA key means every agent must be re-onboarded.

## 5. Upgrading

```bash
sudo /usr/local/lib/fleet/upgrade-server.sh
# or:
sudo deploy/install-server.sh --upgrade
```

The upgrader:
1. Queries the GitHub Releases API (pass `--token` or `GH_TOKEN` if private) and compares versions.
2. Reuses `/etc/fleet-server/install-answers.env` without re-prompting (only asking for genuinely new keys).
3. Stops `fleet-server`, backs up `/var/lib/fleet-server` + binaries + `config.json` + existing proxy configs & systemd units under `/var/backups/fleet-server/`.
4. Swaps binaries, updates `*.example` templates without clobbering live `config.json` or `grafana.ini`, and runs `fleet-server migrate`.
5. Restarts `fleet-server` and verifies health (`http://127.0.0.1:6789/` + CA intact); **automatically rolls back** binary + config + data backup on any failure.
6. Converges newly-bundled native services (`fleet-victoriametrics` on `:8428` and `fleet-grafana` on `:3000` with provisioned VictoriaMetrics datasource + default dashboards) and `fleet-server.service` (`FLEET_MONITOR_STORE_ADDR`) idempotently using the pinned versions and SHA-256 checksums shared from `install-server.sh`.
7. Re-renders `/opt/fleet/proxy/Caddyfile` and `/opt/fleet/proxy/authelia/configuration.yml` (plus proxy systemd units) from saved `/etc/fleet-server/install-answers.env` after backing up existing configs, upgrades native Caddy/Authelia binaries or Docker Compose images, reloads/restarts `fleet-proxy`, and runs a health check with automatic rollback on failure — while preserving `/var/lib/fleet-proxy/` (secrets, TLS certs, `users.yml`, `db.sqlite3`), `/var/lib/fleet-server/ca/`, `/var/lib/fleet-victoriametrics/`, and `/var/lib/fleet-grafana/`.

> **Note on switching domain modes:** `upgrade-server.sh` re-renders `/opt/fleet/proxy/Caddyfile` and `/opt/fleet/proxy/authelia/configuration.yml` using the saved `FLEET_DOMAIN_MODE` in `/etc/fleet-server/install-answers.env` (preserving your existing single- or two-domain layout) and does not migrate an existing two-subdomain host to `--single-domain` (or vice versa). To switch layouts on an existing host, update `FLEET_DOMAIN_MODE` (and `FLEET_AUTH_DOMAIN`) in `install-answers.env` and re-run `install-server.sh --single-domain` / `--two-domain` (removing the old internal TLS cert if regenerating SANs for `--tls-mode internal`).

## 6. Uninstalling

Always preview with `--dry-run` first — nothing is deleted until you confirm:

```bash
curl -fsSL https://raw.githubusercontent.com/mohammadali-al/fleet-control-monitoring-platform/main/deploy/uninstall-server.sh \
  | sudo bash -s -- --dry-run
```

Run an interactive uninstall (stops/disables/removes Fleet systemd units, binaries, `/etc/fleet-server`, `/opt/fleet/proxy`, and the `fleet-agent` wrapper if present; **keeps** `/var/lib/fleet-server`, `/var/lib/fleet-proxy`, and `/var/backups/fleet-server` by default):

```bash
curl -fsSL https://raw.githubusercontent.com/mohammadali-al/fleet-control-monitoring-platform/main/deploy/uninstall-server.sh \
  | sudo bash
# or from a local repo clone:
sudo deploy/uninstall-server.sh
```

To also destroy persistent data (the internal CA private key in `/var/lib/fleet-server/ca/`, machine registry, upgrade backups, and Authelia users/secrets/TLS certs in `/var/lib/fleet-proxy/`), pass `--purge-data` explicitly:

```bash
sudo deploy/uninstall-server.sh --purge-data
```

> **Warning:** Deleting `/var/lib/fleet-server/ca/` destroys the internal CA private key and invalidates every enrolled agent certificate; all agents will need to be re-onboarded after a future reinstall.

Common options:
- `--dry-run`: print every unit/path that would be affected and delete nothing.
- `--yes` / `-y`: skip the interactive confirmation prompt for removing units, binaries, and configs (**still preserves data dirs** unless `--purge-data` is also passed).
- `--purge-data` / `--keep-data`: opt in to deleting persistent data dirs + backups (requires typing the panel hostname interactively unless `--yes` is also passed) or explicitly keep them (default).
- `--proxy native|docker`, `--data-dir DIR`, `--proxy-dir DIR`, `--proxy-state-dir DIR`: override values discovered from `/etc/fleet-server/install-answers.env`.
- Shared OS packages (`docker`, `openssl`, `nftables`, `curl`, etc.) and remote agents on other machines are **not** removed.

## Operations notes

- **Logs:** `journalctl -u fleet-server -f`, `journalctl -u fleet-grafana -f`, `journalctl -u fleet-victoriametrics -f`, `journalctl -u fleet-proxy -f` (or `docker compose -f /opt/fleet/proxy/compose.yml logs -f`).
- **Brute-force protection:** Authelia bans a user for 10 minutes after 5 failed logins in 2 minutes (`regulation` in `/opt/fleet/proxy/authelia/configuration.yml`).
- **Additional users:** Edit `/var/lib/fleet-proxy/authelia/users.yml` and generate argon2id hashes with `docker run --rm authelia/authelia:4.39.27 authelia crypto hash generate argon2 --password '...'` (or `/usr/local/bin/authelia` in native mode).
- **Keep `fleet-server`, `victoria-metrics`, and `grafana` on loopback:** The panel and Grafana trust `Remote-User` from the proxy; never expose `:6789`, `:3000`, `:8428`, or `:9091` externally.
- **Reserved paths:** `/grafana*` is reserved for Grafana (in both single-domain and two-domain modes, proxied with Authelia `forward_auth` and embedded in **Monitor data → Grafana**), and `/authelia*` is reserved in `--single-domain` mode for Authelia; `fleet-server` must never serve either prefix.

## Alternative: Pomerium instead of Authelia

Any forward-auth or identity-aware proxy that terminates TLS, enforces 2FA, and forwards the authenticated user header works. `fleet-server` checks `Remote-User`, `X-Forwarded-User`, `X-Auth-Request-User`, and `X-Pomerium-Claim-Email` (`internal/panel/api.go`).
