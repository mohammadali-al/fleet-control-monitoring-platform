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
| Monitor time-series store | Standalone `victoria-metrics` static binary (`v1.157.0`, no Docker) | Installed & verified by installer | Downloaded with SHA-256 verification |
| Monitor dashboards | Standalone `grafana` static binary (`v11.6.1`, no Docker) | Installed & verified by installer | Downloaded with SHA-256 verification |
| Proxy runtime (`--proxy=native`, default) | Standalone `caddy` + `authelia` binaries (no Docker) | Installed & verified by installer | Downloaded with checksum verification |
| Proxy runtime (`--proxy=docker`) | Docker Engine + Compose v2 (`docker compose`) | `docker info` + `docker compose version` | Yes (`docker.io` / `docker-ce` + compose plugin) |
| DNS / domain names | **1 DNS name** by default (`<panel-domain>`; interactive install asks and defaults to single-domain, or pass `--single-domain`), or 2 DNS names with `--two-domain` (`<panel-domain>` + `<auth-domain>`) | Validated by installer (rejects raw IPs) | No |
| Disk space | `>= 2 GiB` free on `/var/lib` (covers `/var/lib/fleet-server`, `/var/lib/fleet-victoriametrics`, `/var/lib/fleet-grafana`), `>= 1 GiB` on `/usr/local` (covers `fleet-server`, `caddy`, `authelia`, `victoria-metrics`, and `/usr/local/share/grafana` ~656 MiB extracted) | `df -Pm` | No |

> **One-DNS-name vs IP-only:** Single-domain mode (the default) removes the second `auth.<panel-domain>` subdomain by hosting Authelia under `https://<panel-domain>/authelia` (reserving `/authelia*` on the panel domain), which works with single-hostname setups such as Tailscale `*.ts.net`, DuckDNS, or single-SAN certs. Interactive installs ask whether to use one or two names and default to one; pass `--single-domain` or `--two-domain` to force a layout without prompting. It still requires **one real DNS name**: Authelia rejects raw-IP cookie domains, so IP-only deployments remain unsupported.

## Ports

| Port | Proto | Bind / Direction | Owner | Notes |
|---|---|---|---|---|
| `443` | TCP | Public / LAN (`0.0.0.0:443`) | Caddy (auth proxy) | HTTPS for panel + Authelia login portal (customizable via `--panel-port`) |
| `6789` | TCP | Loopback only (`127.0.0.1:6789`) | `fleet-server` | Panel HTTP; never expose off loopback (trusts `Remote-User`) |
| `8428` | TCP | Loopback only (`127.0.0.1:8428`) | `victoria-metrics` | Monitor time-series store HTTP ingest & query API (never expose off loopback; customizable via `--tsdb-port`; state dir `/var/lib/fleet-victoriametrics`, default retention `90d`) |
| `3000` | TCP | Loopback only (`127.0.0.1:3000`) | `grafana` | Embedded monitor dashboards served under `/grafana` via Caddy with Authelia SSO (`Remote-User`; never expose off loopback; customizable via `--grafana-port`; state dir `/var/lib/fleet-grafana`) |
| `9091` | TCP | Loopback only (`127.0.0.1:9091`) | Authelia | Forward-auth backend for Caddy |
| `7443` | TCP | Outbound to agents (and local if co-located) | `fleet-agent` (`server_dial`) | mTLS agent control channel (does not pass through the proxy) |
| `7444` | TCP | Inbound from agents (`0.0.0.0:7444`) | `fleet-server` (`agent_dial_addr`) | Agent-dial mTLS: agent-initiated connections for token-enrolled agents (on by default; overridable via `FLEET_AGENT_DIAL_PORT`) |
| `7445` | TCP | Inbound from enrolling agents (`0.0.0.0:7445`) | `fleet-server` (`enroll_addr`) | Token-enrollment HTTPS (server ← enrolling agents; bypasses the proxy; own pinned-pubkey TLS; on by default; overridable via `FLEET_ENROLL_PORT`) |
| `22` | TCP | Outbound from server / `sshd` on host | SSH | Used once per machine during SSH onboarding (`sshd` on port 22 is allowed) |

## Pinned Component Versions

Both `--proxy=docker` and `--proxy=native` pin identical Caddy and Authelia versions so upgrades are deterministic; VictoriaMetrics and Grafana are always installed as standalone native binaries (`--proxy` independent).

| Component | Pinned Version | Docker Image (`deploy/proxy/compose.yml`) | Native Release Asset & Checksum |
|---|---|---|---|
| Caddy | `2.10.0` | `caddy:2.10.0` | `https://github.com/caddyserver/caddy/releases/download/v2.10.0/caddy_2.10.0_linux_<arch>.tar.gz` (`caddy_2.10.0_checksums.txt`, SHA-512) |
| Authelia | `4.39.27` | `authelia/authelia:4.39.27` | `https://github.com/authelia/authelia/releases/download/v4.39.27/authelia-v4.39.27-linux-<arch>.tar.gz` (`checksums.sha256`, SHA-256) |
| VictoriaMetrics | `1.157.0` | — (native systemd unit `fleet-victoriametrics.service`) | `https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/v1.157.0/victoria-metrics-linux-<arch>-v1.157.0.tar.gz` (`victoria-metrics-linux-<arch>-v1.157.0_checksums.txt` + pinned SHA-256: `amd64=7b31192da6cf805b8b9244a904b022948e8022a067a822c30cb9f94600d0c387`, `arm64=e5368dec72d212dcb2b994345ab872245299000d37197f723123e63a50e573b5`) |
| Grafana | `11.6.1` | — (native systemd unit `fleet-grafana.service`) | `https://dl.grafana.com/oss/release/grafana-11.6.1.linux-<arch>.tar.gz` (`.sha256` sidecar + pinned SHA-256: `amd64=35a5efca8c535ce961217765faf8d6895c6d1c1587eaeec50d606a5cb218603b`, `arm64=6b45fc6c2e534aee02220c1e8ed792bf4bb44ceac2da5ba0cd66e9579496f73e`) |

## Package Managers Supported for Auto-Install

`install-server.sh` can install missing OS dependencies via:
- `apt-get` (Debian / Ubuntu)
- `dnf` / `yum` (RHEL / Rocky / Alma / Fedora)
- `zypper` (openSUSE / SLES)
- `pacman` (Arch Linux)

If the host uses an unsupported package manager, or lacks `systemd` / `cgroup v2` / minimum kernel, preflight exits with a clear error before modifying the system.

> **Uninstall note:** OS packages auto-installed by `install-server.sh` (`nftables`, `openssl`, `curl`, `tar`, `coreutils`, `iproute2`, `docker.io` / `docker-compose-plugin`) are treated as shared host dependencies and are **not** removed by `deploy/uninstall-server.sh`. Only Fleet-owned units, binaries, configs, and (only when `--purge-data` is passed) data directories are removed.
