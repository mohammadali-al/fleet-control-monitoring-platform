# Fleet Control & Monitoring Platform

Fleet orchestration, monitoring, and load-generation platform: a **main server** (`fleet-server`) controlling **worker** and **monitor** agents (`fleet-agent`) over an mTLS WebSocket control plane, fronted by a TLS + 2FA auth proxy (Caddy + Authelia).

## Quickstart (Main Server Install)

This repository is **public** — no token or auth header is needed.

```bash
# 1. Optional: run preflight checks first
curl -fsSL https://raw.githubusercontent.com/mohammadali-al/fleet-control-monitoring-platform/main/deploy/install-server.sh \
  | sudo bash -s -- --check

# 2. Interactive install (native Caddy+Authelia proxy by default; add --proxy=docker for the container stack)
curl -fsSL https://raw.githubusercontent.com/mohammadali-al/fleet-control-monitoring-platform/main/deploy/install-server.sh \
  | sudo bash
```

> **Only if you hit GitHub API rate limits** (or run from a private fork): export a scoped token and
> pass it through — `export GH_TOKEN="<token>"`, add `-H "Authorization: Bearer $GH_TOKEN"` to the `curl`
> and use `sudo -E bash`. Never commit or embed tokens in scripts or `install-answers.env`.

Non-interactive installs accept `--answers answers.env --non-interactive` (or CLI flags like `--panel-domain`, `--tls-mode`, `--admin-user`, `--admin-password-file`). Non-secret answers are saved to `/etc/fleet-server/install-answers.env`.

## First Deploy Flow

1. **Preflight:** Verifies root, Linux `5.4+`, `systemd`, unified `cgroup v2`, `nftables`, `openssl`, Docker + Compose (only with `--proxy=docker`), free ports, and disk space. Offers to install missing OS packages (consent `y/N`, `--yes`, or `--no-install`).
2. **Prompts & Generation:** Prompts for panel/auth domains, TLS mode (`internal` self-signed SAN cert or `acme` Let's Encrypt), and initial admin user/password; auto-generates proxy secrets (`jwt`, `session`, `storage`), Argon2id user hash, and `fleet-server`'s internal mTLS CA.
3. **Services Up:** Installs `fleet-server` + `fleet-proxy` systemd units and verifies loopback health (`127.0.0.1:6789`) + proxy readiness.
4. **DNS & Login:** Point your panel and auth DNS names (or `/etc/hosts`) to the server IP, open `https://<panel-domain>/`, read the first-time TOTP verification code via `sudo cat /var/lib/fleet-proxy/authelia/notification.txt`, and enroll 2FA.

## Updating

```bash
sudo /usr/local/lib/fleet/upgrade-server.sh
# or:
sudo deploy/install-server.sh --upgrade
```

Upgrades query the GitHub Releases API, verify `SHA256SUMS`, stop `fleet-server`, **back up `/var/lib/fleet-server`**, swap binaries, run `fleet-server migrate`, restart, and health-check — **automatically rolling back** on failure. The internal CA (`/var/lib/fleet-server/ca/`), proxy state (`/var/lib/fleet-proxy/`), secrets, and operator-edited configs are **preserved** (templates ship as `*.example`).

## Requirements & Ports

| Requirement / Port | Value | Purpose |
|---|---|---|
| Host OS / Kernel | Linux `5.4+` (`5.15+` recommended), `amd64`/`arm64` | `systemd`, unified `cgroup v2`, `nftables`, `openssl`, `>= 2 GiB` disk |
| Proxy runtime | Native `caddy` + `authelia` binaries (`--proxy=native`, default) or Docker + Compose v2 (`--proxy=docker`) | Pinned Caddy `2.10.0` + Authelia `4.39.27` |
| `443/tcp` | Public / LAN | Caddy HTTPS panel + Authelia login portal |
| `6789/tcp` | `127.0.0.1` loopback | `fleet-server` panel HTTP (proxied only) |
| `9091/tcp` | `127.0.0.1` loopback | Authelia forward-auth endpoint |
| `7443/tcp` | Server -> Agents (`server_dial`) | mTLS agent control channel (bypasses proxy) |
| `22/tcp` | Server -> New agents | SSH onboarding (existing host `sshd` is allowed) |

File layout & operational details: see [`deploy/README.md`](deploy/README.md) (and [`deploy/REQUIREMENTS.md`](deploy/REQUIREMENTS.md); follows the D-0008 layout: `/opt/fleet/proxy/`, `/var/lib/fleet-proxy/`, `/etc/fleet-server/`, `/var/lib/fleet-server/`).

## Reference Docs

- Architecture: [`Fleet_Control_Monitoring_Platform_Architecture.md`](Fleet_Control_Monitoring_Platform_Architecture.md)
- Workflow: [`AGENTS.md`](AGENTS.md) · Board: [`workflow/generated/STATUS.md`](workflow/generated/STATUS.md)
- Local build: `./scripts/build.sh` (server + 4 multi-platform `fleet-agent-<os>-<arch>` binaries)
