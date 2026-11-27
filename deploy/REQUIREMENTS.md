# Main Server Host Requirements

`deploy/install-server.sh` runs a preflight check (`--check`) before installing or upgrading the Fleet main server. Missing OS-level packages can be installed automatically via the host package manager (gated by interactive `y/N` consent or `--yes`; skip with `--no-install`).

## System Requirements

| Requirement | Minimum / Expected | Checked by preflight | Auto-installed if missing |
|---|---|---|---|
| Privilege | `root` (or `sudo`) | `id -u == 0` | No |
| OS & Arch | Linux (`amd64` / `x86_64`, `arm64` / `aarch64`) | `uname -s`, `uname -m` | No |
| Kernel | Linux `5.4+` (`5.15+` recommended) | `uname -r` | No |
| Init system | `systemd` (running PID 1) | `/run/systemd/system` + `systemctl` | No |
| cgroup v2 | Unified cgroup v2 mounted at `/sys/fs/cgroup` | `/sys/fs/cgroup/cgroup.controllers` | No |
| nftables | `nft` CLI + kernel `nf_tables` support | `command -v nft` + kernel check | Yes (`nftables`) |
| OpenSSL | `openssl` CLI | `command -v openssl` | Yes (`openssl`) |
| Fetch / Verify tools | `curl`, `tar`, `sha256sum`, `ss` | `command -v` | Yes (`curl`, `tar`, `coreutils`, `iproute2`) |
| Proxy runtime (`--proxy=native`, default) | Standalone `caddy` + `authelia` binaries (no Docker) | Installed & verified by installer | Downloaded with checksum verification |
| Proxy runtime (`--proxy=docker`) | Docker Engine + Compose v2 (`docker compose`) | `docker info` + `docker compose version` | Yes (`docker.io` / `docker-ce` + compose plugin) |
| DNS / domain names | 2 DNS names by default (`<panel-domain>` + `<auth-domain>`), or **1 DNS name** with `--single-domain` (`<panel-domain>`) | Validated by installer (rejects raw IPs) | No |
| Disk space | `>= 2 GiB` free on `/var/lib`, `>= 500 MiB` on `/usr/local` & `/opt` | `df -Pm` | No |

> **One-DNS-name vs IP-only:** `--single-domain` removes the second `auth.<panel-domain>` subdomain by hosting Authelia under `https://<panel-domain>/authelia` (reserving `/authelia*` on the panel domain), which works with single-hostname setups such as Tailscale `*.ts.net`, DuckDNS, or single-SAN certs. It still requires **one real DNS name**: Authelia rejects raw-IP cookie domains, so IP-only deployments remain unsupported.

## Ports

| Port | Proto | Bind / Direction | Owner | Notes |
|---|---|---|---|---|
| `443` | TCP | Public / LAN (`0.0.0.0:443`) | Caddy (auth proxy) | HTTPS for panel + Authelia login portal (customizable via `--panel-port`) |
| `6789` | TCP | Loopback only (`127.0.0.1:6789`) | `fleet-server` | Panel HTTP; never expose off loopback (trusts `Remote-User`) |
| `9091` | TCP | Loopback only (`127.0.0.1:9091`) | Authelia | Forward-auth backend for Caddy |
| `7443` | TCP | Outbound to agents (and local if co-located) | `fleet-agent` (`server_dial`) | mTLS agent control channel (does not pass through the proxy) |
| `22` | TCP | Outbound from server / `sshd` on host | SSH | Used once per machine during SSH onboarding (`sshd` on port 22 is allowed) |

Optional listeners (disabled by default, configured in `/etc/fleet-server/config.json`):
- `7444/tcp`: `server.agent_dial_addr` (agent-initiated mTLS connections)
- `7445/tcp`: `server.enroll_addr` (token enrollment HTTPS listener)

## Pinned Proxy Versions

Both `--proxy=docker` and `--proxy=native` pin identical Caddy and Authelia versions so upgrades are deterministic.

| Component | Pinned Version | Docker Image (`deploy/proxy/compose.yml`) | Native Release Asset & Checksum |
|---|---|---|---|
| Caddy | `2.10.0` | `caddy:2.10.0` | `https://github.com/caddyserver/caddy/releases/download/v2.10.0/caddy_2.10.0_linux_<arch>.tar.gz` (`caddy_2.10.0_checksums.txt`, SHA-512) |
| Authelia | `4.39.27` | `authelia/authelia:4.39.27` | `https://github.com/authelia/authelia/releases/download/v4.39.27/authelia-v4.39.27-linux-<arch>.tar.gz` (`checksums.sha256`, SHA-256) |

## Package Managers Supported for Auto-Install

`install-server.sh` can install missing OS dependencies via:
- `apt-get` (Debian / Ubuntu)
- `dnf` / `yum` (RHEL / Rocky / Alma / Fedora)
- `zypper` (openSUSE / SLES)
- `pacman` (Arch Linux)

If the host uses an unsupported package manager, or lacks `systemd` / `cgroup v2` / minimum kernel, preflight exits with a clear error before modifying the system.
