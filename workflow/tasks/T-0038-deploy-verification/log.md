# T-0038 Log

## 2026-11-26 — Deploy verification and shellcheck hardening

- Ran `./scripts/wf start T-0038` to create/checkout `task/T-0038` and wire `.githooks`.
- Installed `shellcheck` `0.10.0` locally in `/tmp/t0038-tools` (removed before finishing) and resolved all findings in `deploy/install-server.sh` (`SC2034`, `SC2016`) and `deploy/upgrade-server.sh` (`SC2034`, `SC2015`).
- Built `fleet-server`, `fleet-agent`, and cross-compiled agents via `./scripts/build.sh` (plus a simulated `0.1.0-old` `fleet-server` in `/tmp/t0038-tools`) and verified end-to-end on the test box:
  - `--proxy=docker`: verified `--check` detection, `apt-get` auto-install of `docker.io` + `docker-compose-v2`, preflight, binary/config/secret/cert staging; image pull blocked by `registry-1.docker.io` returning `HTTP/2 403` from the test box's network. Fixed suppressed stderr in `generate_authelia_users()`.
  - `--proxy=native`: uncovered and fixed Authelia release checksum filename (`checksums.sha256` with `--ignore-missing` instead of `<tgz>.sha256`), exact `authelia` binary extraction (`-name 'authelia'`), native `fleet-proxy.service` restart job cancellation (`PartOf=fleet-proxy.service` + single-unit restart), and `--upgrade --check` argument forwarding. Verified full stack up (`fleet-server` on `127.0.0.1:6789`, `authelia` on `127.0.0.1:9091`, `caddy` on `*:443`), HTTPS `forward_auth` redirect, Authelia first-factor login API (`200`), and idempotent re-run.
  - `upgrade-server.sh`: simulated `v1` (`0.1.0-old`) -> `v2` (`0.0.0`) upgrade, verified backup, binary swap, `fleet-server migrate`, byte-for-byte CA + proxy secrets/certs/users/Caddyfile preservation, custom `config.json` preservation, and both induced `migrate`-failure and health-check-failure rollbacks (after adding post-rollback `:6789` health wait to `rollback()`).
- Cleaned up all Fleet artifacts, users, services, Docker packages, and temporary SSH key on the test box, and cleaned local `/tmp/t0038-tools` and `bin/`.
- Wrote redacted `deploy/verification-report.md` and verified `shellcheck`, `bash -n`, and absence of secrets/IPs/credentials.

wf: T-0038 done
