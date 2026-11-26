# Deploy Verification Report (`T-0038`)

Real end-to-end verification of `deploy/install-server.sh` and `deploy/upgrade-server.sh` on a disposable Linux test box, plus `shellcheck` hardening.

## Environment

- **Target host:** the test box (disposable Ubuntu VM; accessed over SSH as a privileged user)
- **OS:** Ubuntu 26.04.1 LTS (Resolute Raccoon)
- **Kernel / Init:** Linux `7.0.0-34-generic` (`x86_64`), `systemd 259`, `cgroup2fs`
- **Binaries verified:**
  - `fleet-server` & `fleet-agent` built from repo (`0.0.0`, commit `32ed296f634b`, Go `1.26.8`), plus a simulated older `0.1.0-old` `fleet-server` build for upgrade testing
  - Caddy `v2.10.0`
  - Authelia `v4.39.27`
  - `docker.io` `29.1.3-0ubuntu4.1` and `docker-compose-v2` `2.40.3+ds1-0ubuntu1` (auto-installed during `--proxy=docker` preflight)
- **Lint tooling:** `shellcheck` `0.10.0` (run locally, not added as a repo dependency)

## 1. `shellcheck` Hardening

Ran `shellcheck` on `deploy/install-server.sh` and `deploy/upgrade-server.sh`.

Initial findings fixed (without changing behavior):
- `deploy/install-server.sh`:
  - `SC2034`: removed unused `REPO_ROOT` assignment.
  - `SC2016`: replaced `'\$argon2id\$[^[:space:]]+'` with `'[$]argon2id[$][^[:space:]]+'` so literal `$` is matched without triggering single-quote expansion warning.
- `deploy/upgrade-server.sh`:
  - `SC2034`: removed unused answer variables (`FLEET_INSTALL_VERSION`, `FLEET_PANEL_DOMAIN`, `FLEET_AUTH_DOMAIN`, `FLEET_PANEL_PORT`, `FLEET_AUTH_PORT`) that `upgrade-server.sh` does not consume.
  - `SC2015`: replaced `A && B || C` chains at the `systemd` prerequisite check and agent backup copy with explicit `if ...; then ... fi` blocks.

Result after all edits: `shellcheck deploy/install-server.sh deploy/upgrade-server.sh` and `bash -n` both exit `0` with zero warnings.

## 2. Real End-to-End Install (`deploy/install-server.sh`)

### `--proxy=docker` (default path)

1. **`--check --proxy=docker`:** Preflight verified root privilege, `Linux/x86_64`, kernel `>= 5.4`, active `systemd`, `cgroup2fs`, free disk space on `/var/lib`, and correctly exited non-zero reporting `docker_needed=1`.
2. **`--proxy=docker --yes --non-interactive --bin-dir <dir>`:**
   - Preflight passed and auto-installed `docker.io` and `docker-compose-v2` via `apt-get`, enabled/started `docker.service`, and verified `nftables`, `Docker + Compose`, and free TCP ports (`443`, `6789`, `7443`, `9091`, `22` sshd allowed).
   - Installed `fleet-server`, `fleet-agent`, multi-arch agent binaries, `upgrade-server.sh`, `config.json`, `install-answers.env`, proxy configs, proxy secrets (`jwt`, `session`, `storage`), and self-signed SAN TLS cert.
   - **Blocked at image pull:** `docker run --rm authelia/authelia:4.39.27 ...` failed because `https://registry-1.docker.io/v2/...` returned `HTTP/2 403 Forbidden` from the test box's network (while `ghcr.io` and `github.com` returned `HTTP/2 200`).
   - **Script fix:** Removed `2>/dev/null` stderr suppression around `authelia crypto hash generate` in `generate_authelia_users()` so the real error message is shown on failure instead of a generic error.

### `--proxy=native` (exercised end-to-end)

Because Docker Hub was blocked from the test box, exercised `--proxy=native` from a clean state (`--yes --non-interactive --bin-dir <dir> --panel-domain fleet.internal --auth-domain auth.fleet.internal --tls-mode internal`):

- **Bugs found & fixed during native verification:**
  1. **Authelia checksum asset name (`install-server.sh`, `upgrade-server.sh`, `REQUIREMENTS.md`):** Upstream Authelia `v4.39.27` publishes `checksums.sha256`, not `<tarball>.sha256` (which returned `404`). Updated both scripts to download `checksums.sha256` and verify via `sha256sum -c --ignore-missing checksums.sha256 | grep -q "^${auth_tgz}: OK$"`, and updated `deploy/REQUIREMENTS.md`.
  2. **Authelia binary extraction (`install-server.sh`, `upgrade-server.sh`):** The release tarball also contains `authelia.service`, `authelia.sysusers.conf`, etc., so `find ... -name 'authelia*'` could select a systemd unit file instead of the binary. Changed to `-name 'authelia'`.
  3. **Native `fleet-proxy.service` restart deadlock (`install-server.sh`, `upgrade-server.sh`):** Running an idempotent re-run of `install-server.sh --proxy=native` caused `systemctl restart fleet-authelia.service fleet-caddy.service fleet-proxy.service` to fail with `Job ... canceled` due to `fleet-proxy.service` combining `Requires=` with `ExecStop=/bin/systemctl stop ...`. Fixed by adding `PartOf=fleet-proxy.service` to `fleet-authelia.service` and `fleet-caddy.service`, dropping the manual `ExecStop`, and restarting `fleet-proxy.service` alone.
- **Verification after fixes:**
  - Preflight passed; Caddy `v2.10.0` (SHA-512 verified) and Authelia `v4.39.27` (SHA-256 verified) installed to `/usr/local/bin/`.
  - System users `fleet` and `fleet-proxy` created; proxy secrets, self-signed TLS cert (`SANs: fleet.internal, auth.fleet.internal`), `users.yml`, and internal CA (`/var/lib/fleet-server/ca/{ca.crt,ca.key}`) generated.
  - All 4 systemd units active: `fleet-server.service`, `fleet-authelia.service`, `fleet-caddy.service`, `fleet-proxy.service`.
  - Sockets bound as designed: `127.0.0.1:6789` (`fleet-server`), `127.0.0.1:9091` (`authelia`), `*:443` (`caddy`).
  - HTTP/HTTPS checks:
    - `http://127.0.0.1:6789/` -> `200`
    - `https://fleet.internal/` (via Caddy TLS + `forward_auth`) -> `HTTP/2 302` redirecting to `https://auth.fleet.internal/?rd=https%3A%2F%2Ffleet.internal%2F&rm=GET`
    - `https://auth.fleet.internal/` -> `200`
    - `POST https://auth.fleet.internal/api/firstfactor` with admin credentials -> `200` (`{"status":"OK"}`) with session cookie set.
  - Idempotent re-run of `install-server.sh --proxy=native` and `systemctl stop`/`start fleet-proxy.service` succeeded without clobbering config, CA, certs, or `users.yml`.

## 3. Upgrade & Rollback (`deploy/upgrade-server.sh`)

### Simulated Older Deployment Upgrade (`v1` -> `v2`)

Simulated an older installation on the test box:
- Installed `0.1.0-old` `fleet-server` binary
- Set `SCHEMA_VERSION` to `1` and `"schema_version": 1` in `/etc/fleet-server/config.json`
- Added operator customizations (`"level": "warn"` in `config.json`, marker comment in `/opt/fleet/proxy/Caddyfile`) and recorded SHA-256 checksums of CA, secrets, certs, `users.yml`, and `Caddyfile`.

Results:
- `install-server.sh --upgrade --check --bin-dir <dir> --non-interactive` initially ran a full upgrade instead of `--check` because `--check` was not forwarded in `UPGRADE_ARGS`. Fixed in `deploy/install-server.sh` and re-verified (`check complete: 0.0.0 -> 0.0.0` without mutating state).
- `upgrade-server.sh --check` reported `0.1.0-old -> 0.0.0`.
- Live `upgrade-server.sh --bin-dir <dir> --non-interactive` executed cleanly:
  1. Stopped `fleet-server.service` and backed up `/var/lib/fleet-server`, binaries, and `config.json` under `/var/backups/fleet-server/upgrade-<ts>/`.
  2. Swapped `fleet-server` (`0.1.0-old` -> `0.0.0`) and `fleet-agent` binaries and refreshed `config.json.example`.
  3. Ran `fleet-server migrate`: backed up `/var/lib/fleet-server` -> `/var/lib/fleet-server.bak-1-<ts>` and migrated `v1 -> v2` (`SCHEMA_VERSION` = `2`, `config.json` `schema_version` = `2`, custom `"level": "warn"` preserved).
  4. Verified CA byte-for-byte unchanged, restarted `fleet-server.service`, and passed health check on `http://127.0.0.1:6789/`.
  5. Preserved all 9 recorded SHA-256 checksums (`/var/lib/fleet-server/ca/{ca.key,ca.crt}`, `/var/lib/fleet-proxy/secrets/{jwt,session,storage}`, `/var/lib/fleet-proxy/certs/{tls.crt,tls.key}`, `/var/lib/fleet-proxy/authelia/users.yml`, `/opt/fleet/proxy/Caddyfile`), and kept HTTPS panel auth redirect working.

### Induced Rollback Verification

- **Induced `migrate` failure:** Replaced `--bin-dir` `fleet-server` with a failing stub that wrote a corrupt marker file into `/var/lib/fleet-server` and exited `42` on `migrate`. `upgrade-server.sh` triggered `rollback()`, restored the previous binary and data directory (moving the failed data dir to `/var/lib/fleet-server.failed-<ts>`), and restarted `fleet-server.service`.
  - **Bug found & fixed:** Immediately after `upgrade-server.sh` returned from `rollback()`, `curl http://127.0.0.1:6789/` transiently failed because `systemctl start fleet-server.service` returns before Go binds the socket. Updated `rollback()` in `deploy/upgrade-server.sh` to use atomic `install -D -m 0755` for `fleet-server` and poll `http://127.0.0.1:${FLEET_SERVER_PORT}/` for up to 10s for `200` before exiting.
- **Re-verified both rollback paths** after the fix:
  1. Induced `migrate` failure: rolled back cleanly, corrupt marker gone, `http://127.0.0.1:6789/` returned `200` immediately upon exit, all 9 SHA-256 checksums `: OK`.
  2. Induced health-check failure (`migrate` exited `0`, server binary exited `1` on start): timed out waiting on `:6789`, triggered `rollback()`, restored `0.0.0` binary, `http://127.0.0.1:6789/` returned `200` immediately upon exit, all 9 SHA-256 checksums `: OK`.

## 4. Cleanup & Residue

Cleaned up the test box after verification:
- Stopped and disabled `fleet-proxy`, `fleet-caddy`, `fleet-authelia`, and `fleet-server` units; removed all `/etc/systemd/system/fleet-*.service` files and reloaded `systemd`.
- Removed `/usr/local/bin/{fleet-server,caddy,authelia}`, `/usr/local/share/fleet`, `/usr/local/lib/fleet`, `/etc/fleet-server`, `/opt/fleet`, `/var/lib/fleet-server*`, `/var/lib/fleet-proxy`, `/var/backups/fleet-server`, and temporary staging directories.
- Deleted system users `fleet` and `fleet-proxy`.
- Purged `docker.io` and `docker-compose-v2` (and ran `apt-get autoremove`) and removed `/var/lib/docker` and `/var/lib/containerd`.
- Removed the ephemeral SSH public key added for the session.
- **Post-cleanup check:** Only `sshd` (`22`) and `systemd-resolve` (`53`) remain listening; no Fleet directories or files remain.
- **Noted side effect:** `apt-get autoremove` during Docker package cleanup also removed an already-superseded kernel package (`7.0.0-30-generic`) alongside Docker's unused dependencies; the running kernel (`7.0.0-34-generic`) is unaffected.

## 5. Known Limitations / Flakiness

- `--proxy=docker` container pull/startup could not be exercised live on the test box because `registry-1.docker.io` returned `HTTP/2 403 Forbidden` from the test box's network. Docker preflight, package installation, daemon startup, Compose v2 check, and `--proxy=native` full proxy/HTTPS/Authelia flow were all verified live.
- Full 2FA TOTP enrollment in a browser was not automated; first-factor authentication through Caddy -> Authelia (`POST /api/firstfactor`) and `forward_auth` redirect were verified via `curl`.
