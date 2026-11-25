# Deploying the Fleet main server

A reference deployment: `fleet-server` as a systemd service on the main server, with the panel
published over HTTPS behind an auth proxy that adds password + TOTP (2FA). The panel keeps no accounts
of its own (D-0004, architecture decision 20); it trusts the `Remote-User` header the proxy forwards.

```
operator browser --HTTPS--> Caddy :443 --forward auth--> Authelia 127.0.0.1:9091
                               |  (password + TOTP; sets Remote-User)
                               +--HTTP, loopback only--> fleet-server 127.0.0.1:6789

fleet-server --mTLS (internal CA), server_dial--> fleet-agent <machine IP>:7443   (not via the proxy)
fleet-server --SSH, onboarding only-----------> new machine :22
```

What lives where (all paths are defaults of the files in this directory):

| Thing | Location |
|---|---|
| `fleet-server` binary and systemd unit | `/usr/local/bin/fleet-server`, `/etc/systemd/system/fleet-server.service` |
| Server config (JSON) | `/etc/fleet-server/config.json` (only `server.agent_binary_path` and log settings) |
| Optional env overrides | `/etc/fleet-server/fleet-server.env` (`FLEET_LISTEN_ADDR`, `FLEET_DATA_DIR`) |
| Data dir (`FLEET_DATA_DIR`) | `/var/lib/fleet-server`: registry, `ca/` (internal CA, **contains the CA private key**), `config/` |
| `fleet-agent` binary that onboarding pushes | `/usr/local/share/fleet/fleet-agent` (`server.agent_binary_path`) |
| Proxy stack | `deploy/proxy/` (Caddy + Authelia via `compose.yml`), Authelia config in `deploy/authelia/` |
| Proxy state (certs, secrets, users, sessions) | `deploy/proxy/state/` (git-ignored; never commit it) |

## Prerequisites

- Main server: Linux with systemd, root access, and Go (to build) or prebuilt `fleet-server` and
  `fleet-agent` binaries.
- Docker with the Compose plugin (for the proxy), and `openssl`.
- Network: operators reach the main server on TCP 443; the main server reaches each machine on TCP 7443
  (agent channel) and TCP 22 (SSH, for onboarding only). Do not expose `:6789` or `:9091` off the host.
- Two names for the panel that resolve to the main server in operators' browsers (the reference uses
  `fleet.internal` for the panel and `auth.fleet.internal` for the login portal). Authelia needs a
  DNS-style cookie domain, so an IP address alone does not work for the browser URL. Use internal DNS or
  an `/etc/hosts` line on each operator machine:

  ```
  <main server IP>  fleet.internal auth.fleet.internal
  ```

  To use other names, change them in `deploy/proxy/Caddyfile`, `deploy/authelia/configuration.yml`
  (`access_control`, `session.cookies`) and in the certificate below. Only the panel name appears in
  `access_control`; the portal name is covered automatically.

## 1. Build

```
scripts/build.sh          # writes bin/fleet-server and bin/fleet-agent
```

## 2. Install the main server

```
sudo deploy/install-server.sh
```

This is safe to re-run (it only replaces files that changed, never overwrites an existing
`/etc/fleet-server/config.json`, and restarts the service only when something changed). It:

1. creates the `fleet` system user and the data dir `/var/lib/fleet-server` (mode 0750);
2. installs `fleet-server`, the `fleet-agent` binary at `/usr/local/share/fleet/fleet-agent`, and the unit;
3. writes `/etc/fleet-server/config.json` with `server.agent_binary_path` set to that agent binary
   (without it, onboarding fails with "no agent binary path configured");
4. enables and starts the service. The unit sets `FLEET_DATA_DIR=/var/lib/fleet-server` and
   `FLEET_LISTEN_ADDR=127.0.0.1:6789`;
5. waits for the first run to create the internal CA in `/var/lib/fleet-server/ca/` (`ca.crt`, `ca.key`).
   An existing CA is never replaced; a half-present one makes the server refuse to start.

Options: `--bin-dir DIR` (binaries from somewhere other than `bin/`), `--no-start` (install only).
Check it: `systemctl status fleet-server` and `curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:6789/`
(expect `200`).

Keep the server bound to loopback. The panel trusts `Remote-User` from whoever connects, so only the
proxy may reach it. To change the listen address or data dir, put `FLEET_LISTEN_ADDR=...` /
`FLEET_DATA_DIR=...` in `/etc/fleet-server/fleet-server.env` and restart; a different data dir also needs
`ReadWritePaths=<dir>` in a unit drop-in (the unit uses `ProtectSystem=strict`).

**Back up `/var/lib/fleet-server/ca/`.** Losing the CA key means every agent must be re-onboarded.

## 3. Prepare the proxy state

All commands run in `deploy/proxy/`.

```
cd deploy/proxy
mkdir -p state/certs state/secrets state/authelia state/caddy
```

**Secrets** (three random strings, one per file):

```
for f in jwt session storage; do openssl rand -hex 32 > state/secrets/$f; done
chmod 600 state/secrets/*
```

**TLS certificate.** For the reference, a self-signed certificate covering both names:

```
openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
  -keyout state/certs/tls.key -out state/certs/tls.crt \
  -subj "/CN=fleet.internal" \
  -addext "subjectAltName=DNS:fleet.internal,DNS:auth.fleet.internal"
```

Browsers will warn about it until you trust `state/certs/tls.crt` on the operator machines. To use a real
certificate (internal CA or public), replace `state/certs/tls.crt` (full chain) and `state/certs/tls.key`
with yours, then `docker compose restart caddy`. Both names must be in its SANs. Nothing else changes.

**First user.** Copy the example, generate an argon2id hash and put it in the `password` field of the
copy (also change the user name, display name and email as needed):

```
cp ../authelia/users.yml.example state/authelia/users.yml
docker run --rm authelia/authelia:4.39 authelia crypto hash generate argon2 --password 'a long passphrase'
${EDITOR:-vi} state/authelia/users.yml
```

The user key (`alice` in the example) is what the panel records as the operator in its audit trail.
More users go in the same file (Authelia reloads it on change).

## 4. Bring up the proxy

```
docker compose config -q      # optional: validates compose.yml
docker compose up -d
docker compose ps             # authelia and caddy should be running
```

Caddy runs on the host network and listens on `:443`; Authelia listens on `127.0.0.1:9091` only. Plain
HTTP (`:80`) is not served.

## 5. Open the panel and enrol 2FA

1. Browse to `https://fleet.internal/`. Without a session you are redirected to
   `https://auth.fleet.internal/`.
2. Sign in with the user from step 3. The first time, choose to register a one-time-password device.
   Authelia asks for an identity-verification code; this deployment has no SMTP, so the code is written to
   a file: `cat deploy/proxy/state/authelia/notification.txt`.
3. Scan the QR code with an authenticator app and enter the 6-digit code. From then on every sign-in is
   password + TOTP, and the panel opens.

The `access_control` rule is `two_factor` for the panel and `default_policy: deny`; a user that has not
registered a TOTP device cannot reach the panel.

## 6. Onboard the first agent

In the panel open **Machines**, then **Onboard machine**. Enter the target machine's IP, an SSH user
(root, or a user with passwordless `sudo -n`) with a password or key, and the role (worker or monitor). The
credentials are used once and not stored. The main server then, over SSH, installs
`/usr/local/bin/fleet-agent` (the file from `server.agent_binary_path`), creates the agent's client
certificate from the internal CA, writes the pinned CA and the connect mode, and starts the service. The
machine shows **online** once the main server's mTLS channel to `<machine IP>:7443` is up (the default
`server_dial` mode), so allow TCP 7443 from the main server to the machine.

The agent channel does not go through the proxy: it is mTLS directly between the main server and each
agent, authenticated by the internal CA, so no panel login is involved.

## Operations notes

- **Upgrade:** `git pull`, `scripts/build.sh`, `sudo deploy/install-server.sh`. The agent binary at
  `/usr/local/share/fleet/fleet-agent` is replaced too; machines already onboarded keep their installed
  agent until you re-provision them from the panel.
- **Logs:** `journalctl -u fleet-server`; proxy: `docker compose logs` in `deploy/proxy/`.
- **Stop the proxy:** `docker compose down` (state in `deploy/proxy/state/` is kept).
- **Sign-in brute force:** Authelia bans a user for 10 minutes after 5 failures in 2 minutes
  (`regulation` in `deploy/authelia/configuration.yml`).

## Open decision: packaging

The reference uses the simplest path: a **tarball-style install** (built binaries copied by
`install-server.sh`, a systemd unit, a Compose file for the proxy). The packaging choice itself is
still open for the operator to make:

| Option | Fits when | Cost |
|---|---|---|
| Tarball + `install-server.sh` (this reference) | few hosts, operators comfortable with a shell | no package-manager upgrades or dependency tracking |
| `.deb` | Ubuntu everywhere (the agents are Ubuntu) and you want `apt`-style upgrades | a packaging task (control files, maintainer scripts doing what `install-server.sh` does); `DESTDIR` staging in the script is meant to make this easy |
| Container image | you already run a container platform | the server needs the data dir (CA key!) on a persistent volume and the `fleet-agent` binary inside the image; onboarding SSH and agent channel egress must be allowed |

## Alternative: Pomerium instead of Authelia

Authelia is the reference, not a requirement. Any forward-auth proxy that terminates TLS, enforces 2FA
and passes the signed-in user to `127.0.0.1:6789` works. Pomerium is the named alternative: it acts as the
proxy itself (route `https://fleet.internal` to `http://127.0.0.1:6789`) with an identity provider that
provides MFA. `fleet-server` reads the operator name from, in order, `Remote-User`, `X-Forwarded-User`,
`X-Auth-Request-User` and `X-Pomerium-Claim-Email` (`internal/panel/api.go`), so a proxy that forwards
the user in one of those headers needs no server change (this reference was only run with Authelia and
Caddy; a Pomerium setup is untested here). Keep the same rule: `fleet-server` must be reachable only from
the proxy.
