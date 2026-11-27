---
id: T-0039
title: single-domain-auth-path
state: merged
touches: [deploy/install-server.sh, deploy/proxy/Caddyfile, deploy/authelia/configuration.yml,
          deploy/proxy/compose.yml, deploy/README.md, deploy/REQUIREMENTS.md]
tests: []
after: [T-0036, T-0038]
shares: {}
model: {claude: "sonnet high"}
operator_check: "With --single-domain, install uses ONE DNS name: the panel on https://<domain>/ and Authelia served under https://<domain>/authelia (no second auth subdomain). Login works end-to-end (forward-auth redirects to /authelia, firstfactor succeeds, session cookie set on <domain>), for both --proxy=native and --proxy=docker. The default two-subdomain layout still works unchanged."
---

## Why

Today the installer provisions **two** DNS names (panel + auth subdomain) because Authelia needs a
hostname for its session cookie. Many single-hostname setups (Tailscale `*.ts.net`, DuckDNS, no-wildcard
certs) only have one name. Authelia 4.39 supports **subpath hosting** via `server.address`, so we can
serve the auth portal under a path on the panel domain instead of a second subdomain — reducing 2 names
to 1. (It still needs 1 DNS name; Authelia rejects raw-IP cookie domains, so IP-only is out of scope.)

## What to build

Add a **`--single-domain`** installer mode that serves Authelia under **`/authelia`** on the panel
domain. Keep the current two-subdomain layout as the default (or a `--two-domain`), unchanged.

- **Caddy** (`deploy/proxy/Caddyfile` template): on the panel domain, `handle /authelia*` →
  `reverse_proxy` Authelia (NO `forward_auth`, do NOT strip the prefix); `handle` (everything else) →
  `forward_auth` Authelia `uri /authelia/api/authz/forward-auth` (copy `Remote-User/Groups/Name/Email`)
  → `reverse_proxy` fleet-server. One `tls` block, one hostname. NB: with subpath hosting the whole
  Authelia app (portal + API + forward-auth) lives under `/authelia`, so the forward-auth uri is
  `/authelia/api/authz/forward-auth`, not `/api/authz/forward-auth`.
- **Authelia** (`deploy/authelia/configuration.yml` template): `server.address:
  'tcp://127.0.0.1:9091/authelia'`; `session.cookies[]` with `domain: <panel-domain>`,
  `authelia_url: https://<panel-domain>/authelia`, `default_redirection_url: https://<panel-domain>`.
- **Installer** (`deploy/install-server.sh`): in single-domain mode prompt for ONE domain, render the
  above templates (both `--proxy=native` and `--proxy=docker`), and save the mode in
  `install-answers.env`. Default path is unchanged.
- **Reserved path:** with subpath hosting, the entire Authelia app is under `/authelia*`, so the panel
  must never serve `/authelia*` — that single prefix already covers `/authelia/api/authz/*`, so there is
  nothing else to reserve. `/authelia` chosen over `/auth` for distinctiveness. Document (and, if cheap,
  assert) it.
- **Docs** (`deploy/README.md`, `deploy/REQUIREMENTS.md`): the single-domain option, its one-name
  requirement, and that IP-only is still unsupported.

## Out of scope

- Removing the two-subdomain layout; changing fleet-server/panel routes; the IP-only case (unsupported
  by Authelia); the upgrade script's domain migration (note it, don't build it).

## Done means

- `--single-domain` renders a working one-hostname Caddyfile + Authelia config for BOTH proxy modes;
  login works end-to-end (forward-auth → `/authelia` portal → firstfactor → cookie on `<domain>`).
- The default two-subdomain install is unchanged and still works.
- `shellcheck` clean on `install-server.sh`; YAML valid; `bash -n` clean; README/REQUIREMENTS updated.
  (Prefer a real check on a test box as in T-0038 if available.)

## Facts this brief relies on

- fleet-server does not use `/authelia*` (nor `/auth*` / `/api/authz*`), so serving Authelia under
  `/authelia*` causes no route collision: `internal/panel/server.go`.
- Current installer generates the two-domain Caddy + Authelia layout: `deploy/install-server.sh`
  (domain/proxy rendering), `deploy/proxy/Caddyfile`, `deploy/authelia/configuration.yml`;
  `workflow/tasks/T-0036-github-release-and-installer/task.md`.
- Proxy runs native or docker; native is the default: `workflow/decisions/D-0009-default-proxy-native.md`;
  `workflow/decisions/D-0004-deployment.md`; `workflow/decisions/D-0008-deployment-layout.md`.
- Authelia 4.39 subpath via `server.address` (replaces the removed `server.path`); forward-auth at
  `/api/authz/forward-auth`. Ref: https://github.com/authelia/authelia .

## Notes for the doer

- Keep templates idempotent and both proxy modes in sync. Don't hardcode the domain; thread it through.
  Run `./scripts/wf start T-0039` before committing; commit inside touches + the log. Do NOT
  push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
  If a real test box is used, never commit its address/creds (see T-0038).
