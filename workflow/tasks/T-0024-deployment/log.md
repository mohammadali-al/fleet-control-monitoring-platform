# T-0024 deployment: log

## Entry 1 (doer)

Ops/config and docs only; no Go changes.

- `deploy/proxy/compose.yml` + `deploy/proxy/Caddyfile` (new): Caddy (host network, HTTPS on :443, cert files
  from `state/certs`) does forward-auth to Authelia and reverse-proxies to `127.0.0.1:6789`, copying
  `Remote-User/Groups/Name/Email`. Authelia is published on loopback only. `deploy/proxy/.gitignore` ignores
  `state/` (certs, secrets, users, db). Caddy was chosen as the TLS/forward-auth front because Authelia is not
  itself a reverse proxy; D-0004 forbids a webserver for the panel, not a proxy in front of it.
- `deploy/authelia/configuration.yml` (Authelia 4.39): file users, `two_factor` for `fleet.internal`, default
  deny, TOTP, sqlite storage, filesystem notifier (no SMTP; TOTP enrolment code is read from
  `state/authelia/notification.txt`), secrets via `*_FILE` env. Cookie domain must be a DNS name (Authelia
  rejects IPs), so the reference uses `fleet.internal` / `auth.fleet.internal` (README: hosts/DNS).
  `users.yml.example` carries a placeholder hash only.
- `deploy/install-server.sh` + `deploy/systemd/fleet-server.service`: installs `fleet-server`, the
  `fleet-agent` payload at `/usr/local/share/fleet/fleet-agent`, the unit; creates user `fleet`, data dir
  `/var/lib/fleet-server`, `/etc/fleet-server/config.json` (only if absent) with `server.agent_binary_path`;
  starts and waits for `<data>/ca/ca.crt` + `ca.key`. Unit sets `FLEET_DATA_DIR` and
  `FLEET_LISTEN_ADDR=127.0.0.1:6789` (loopback, because the panel trusts `Remote-User`), optional env file,
  hardening. `DESTDIR` staging skips user/systemctl (used for testing and future .deb).
- `deploy/README.md`: build, install, proxy state (secrets, self-signed cert, swapping a real cert, first user
  hash), bring up, TOTP enrolment, onboarding the first agent, operations notes, the open packaging choice
  (tarball vs .deb vs container; tarball + systemd used here) and Pomerium as an alternative (untested here).
- Facts checked against code: flags are only `--config`/`--version`; env `FLEET_LISTEN_ADDR`, `FLEET_DATA_DIR`
  (internal/config); `server.agent_binary_path` is config-only (JSON); CA created on first run under
  `<data>/ca` (internal/fleet/open.go); operator name read from `Remote-User` first (internal/panel/api.go);
  panel WebSocket origin check needs Host preserved (Caddy does).
- Validation: `authelia config validate` (4.39 image) passes with the shipped config; `caddy validate` and
  `caddy fmt` clean; `docker compose config -q` ok; YAML parses; `bash -n` and shellcheck clean on the script;
  `systemd-analyze verify` only complains the binary is not yet installed. Script: DESTDIR run twice
  (second run changes nothing), missing-binary and non-root refusals tested. Live: ran the real compose stack
  with a built `fleet-server` on 127.0.0.1:6789: unauthenticated request -> 302 to the portal; after
  sign-in the upstream receives `Remote-User: alice` and a client-sent `Remote-User: mallory` is replaced;
  Host preserved. Live run used one_factor in a scratch copy only (cannot drive TOTP headless); the shipped
  config is `two_factor`. Real fleet-server run with the unit's env showed the CA created and `/` returning 200.
  Not done: a real `systemctl` install (needs root on a real host), TOTP login by hand, real onboarding.
- `scripts/test.sh server` passes (no Go touched). Docker images authelia/authelia:4.39 and caddy:2 were
  pulled locally for validation; compose stack was brought down; nothing is left running.
- Open for the operator: packaging choice (README table); hostnames/cert for the real site.

<!-- wf: commits=f98786b left=- tests=server:pass blocked=no -->
