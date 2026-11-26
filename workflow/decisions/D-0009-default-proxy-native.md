# D-0009 — Default auth-proxy mode is `native` (not Docker)

**Status:** accepted · **Date:** 2026-11-26 · **Supersedes** the T-0036 default.

## Context
The installer supports two proxy runtimes behind the panel: `--proxy=docker` (Caddy + Authelia as
Compose containers) and `--proxy=native` (Caddy + Authelia as standalone binaries + systemd units).
T-0036 defaulted to Docker. T-0038's real-box verification showed the Docker path can be blocked by
registry access (Docker Hub returned HTTP 403 for image pulls on that network), while the **native**
path installed and ran end-to-end with no external registry dependency.

## Decision
- **`native` is the default proxy mode.** `--proxy=docker` remains available for hosts that prefer the
  container stack. Both pin identical Caddy/Authelia versions.
- Rationale: no Docker daemon or image pulls required, so it works behind registry blocks/offline and
  has fewer moving parts; it is the path verified on real hardware (T-0038).

## Consequences
- Fresh installs and upgrades default to native unless `--proxy=docker` is passed (or an existing
  install's saved `FLEET_PROXY_MODE` says docker). Docker + Compose are only required/installed under
  `--proxy=docker`. If the Docker path is wanted offline, ship the images as `docker save` tarballs on
  the GitHub Release and `docker load` them (not committed to git).
