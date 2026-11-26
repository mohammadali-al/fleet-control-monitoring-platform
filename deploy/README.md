# Deploying the Fleet main server

Reference deployment for the Fleet main server: `fleet-server` as a systemd service bound to loopback (`127.0.0.1:6789`) behind a TLS + 2FA auth proxy (Caddy + Authelia). The panel keeps no accounts of its own (D-0004); it trusts the `Remote-User` header forwarded by the proxy.

```
operator browser --HTTPS--> Caddy :443 --forward auth--> Authelia 127.0.0.1:9091
                               |  (password + TOTP; sets Remote-User)
                               +--HTTP, loopback only--> fleet-server 127.0.0.1:6789

fleet-server --mTLS (internal CA), server_dial--> fleet-agent <machine IP>:7443   (not via the proxy)
fleet-server --SSH, onboarding only-----------> new machine :22
```

## Layout (deployed host)

| Component | Location | Notes |
|---|---|---|
| `fleet-server` binary & unit | `/usr/local/bin/fleet-server`, `/etc/systemd/system/fleet-server.service` | Managed by systemd (`fleet` user) |
| Server config & env | `/etc/fleet-server/config.json`, `fleet-server.env`, `install-answers.env` | Live files never overwritten on upgrade (`*.example` updated alongside) |
| Server data dir (`FLEET_DATA_DIR`) | `/var/lib/fleet-server` | Registry, `ca/` (**CA private key**), `SCHEMA_VERSION`, backups |
| `fleet-agent` binaries | `/usr/local/share/fleet/agents/fleet-agent-<os>-<arch>` & `/usr/local/share/fleet/fleet-agent` | Multi-platform (`server.agent_binary_dir`) + host fallback |
| Proxy home | `/opt/fleet/proxy/` | `compose.yml`, `Caddyfile`, `authelia/configuration.yml` (+ `*.example`) |
| Proxy mutable state | `/var/lib/fleet-proxy/` | `certs/`, `secrets/`, `authelia/` (`users.yml`, `db.sqlite3`), `caddy/` |
| Proxy systemd unit(s) | `/etc/systemd/system/fleet-proxy.service` | Native `fleet-caddy` + `fleet-authelia` (`--proxy=native`, default) or Docker Compose (`--proxy=docker`) |
| Upgrade helper | `/usr/local/lib/fleet/upgrade-server.sh` | Invoked by `install-server.sh --upgrade` or directly |

## Prerequisites

See [`deploy/REQUIREMENTS.md`](REQUIREMENTS.md) for the full host matrix, ports, and pinned Caddy (`2.10.0`) / Authelia (`4.39.27`) versions.

- Linux host with `systemd`, root access, unified **cgroup v2**, **nftables**, `openssl`, and at least 2 GiB free on `/var/lib`.
- No Docker needed for the default `--proxy=native` mode (standalone Caddy + Authelia binaries). Use `--proxy=docker` for the Compose container stack, which then needs Docker + Compose v2.
- DNS names (e.g. `fleet.internal` and `auth.fleet.internal`, or public domains) pointing to the main server. Authelia requires a DNS-style cookie domain; an IP address alone will not work.

## 1. Install from GitHub Release (recommended)

For a private repository (or to avoid GitHub API rate limits), export a scoped token first:

```bash
export GH_TOKEN="<github-token-with-repo-read>"
```

Run the bootstrap installer (checks prerequisites, offers to install missing OS packages, downloads + SHA256-verifies binaries, prompts for domains/admin/TLS, generates secrets + CA, and starts `fleet-server` + `fleet-proxy`):

```bash
curl -fsSL -H "Authorization: Bearer $GH_TOKEN" \
  https://raw.githubusercontent.com/mohammadali-al/fleet-control-monitoring-platform/main/deploy/install-server.sh \
  | sudo -E bash
```

Options:
- `--check`: run preflight checks only.
- `--proxy=native|docker`: native Caddy+Authelia binaries (default) or the Docker Compose proxy stack (`--proxy=docker`).
- `--yes` / `--no-install`: auto-approve or skip OS package installation.
- `--answers FILE` / `--non-interactive`: supply answers from an env file without prompting. Non-secret answers are saved to `/etc/fleet-server/install-answers.env` for future upgrades.
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
2. Browse to `https://<panel-domain>/` — you are redirected to `https://<auth-domain>/`.
3. Sign in with the admin user and password configured during installation, and register a TOTP device.
4. Read the one-time verification code from:

   ```bash
   sudo cat /var/lib/fleet-proxy/authelia/notification.txt
   ```

5. Scan the QR code with an authenticator app and enter the 6-digit code. Subsequent logins use password + TOTP.

## 4. Onboard the first agent

In the panel open **Machines** -> **Onboard machine** (or use token enrollment). Enter the target machine's IP, an SSH user (root or passwordless `sudo -n`), and its role (`worker` or `monitor`). `fleet-server` pushes the matching `fleet-agent-<os>-<arch>` binary, signs a leaf cert from `/var/lib/fleet-server/ca/`, and connects over mTLS (`TCP 7443` in default `server_dial` mode).

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
3. Stops `fleet-server`, backs up `/var/lib/fleet-server` + binaries + `config.json` under `/var/backups/fleet-server/`.
4. Swaps binaries, updates `*.example` templates without clobbering live files, and runs `fleet-server migrate`.
5. Restarts `fleet-server` and verifies health (`http://127.0.0.1:6789/` + CA intact); **automatically rolls back** binary + config + data backup on any failure.
6. Upgrades the proxy (`docker compose pull && up -d` on pinned tags, or native binary upgrade) while preserving `/var/lib/fleet-proxy/`.

## Operations notes

- **Logs:** `journalctl -u fleet-server -f`, `journalctl -u fleet-proxy -f` (or `docker compose -f /opt/fleet/proxy/compose.yml logs -f`).
- **Brute-force protection:** Authelia bans a user for 10 minutes after 5 failed logins in 2 minutes (`regulation` in `/opt/fleet/proxy/authelia/configuration.yml`).
- **Additional users:** Edit `/var/lib/fleet-proxy/authelia/users.yml` and generate argon2id hashes with `docker run --rm authelia/authelia:4.39.27 authelia crypto hash generate argon2 --password '...'` (or `/usr/local/bin/authelia` in native mode).
- **Keep `fleet-server` on loopback:** The panel trusts `Remote-User` from the proxy; never expose `:6789` or `:9091` externally.

## Alternative: Pomerium instead of Authelia

Any forward-auth or identity-aware proxy that terminates TLS, enforces 2FA, and forwards the authenticated user header works. `fleet-server` checks `Remote-User`, `X-Forwarded-User`, `X-Auth-Request-User`, and `X-Pomerium-Claim-Email` (`internal/panel/api.go`).
