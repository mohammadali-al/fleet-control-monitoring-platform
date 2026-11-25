---
id: T-0036
title: github-release-and-installer
state: ready
touches: [.github/workflows/release.yml, deploy/install-server.sh, deploy/upgrade-server.sh,
          deploy/REQUIREMENTS.md, deploy/README.md, README.md,
          deploy/proxy/compose.yml, deploy/proxy/fleet-proxy.service]
tests: []
after: [T-0024, T-0032, T-0035]
shares: {}
model: {claude: "sonnet high"}
operator_check: "From a GitHub release, `curl|bash` installs the main server interactively (prompts for domains/admin/TLS, generates secrets + CA, brings the proxy up) and `install-server.sh --upgrade` moves an already-deployed OLDER version to a new release (preflight → backup → swap → `fleet-server migrate` → restart → health check → rollback-on-fail) WITHOUT wiping local config/secrets or the CA. README documents install, first deploy, and update."
---

## Why

Deployment is currently weak: no requirements check, no install/update from GitHub, configs edited in a
repo clone. Make the **main server** installable and upgradable directly from GitHub releases, with an
interactive first install and a safe upgrade path. (Agents still get their files from the main server,
not GitHub.)

## What to build

**Packaging / CI / docs — broad; split into logical commits.**

1. **Release pipeline** (`.github/workflows/release.yml`): on a version tag, build `fleet-server` + the
   four `fleet-agent-<os>-<arch>` binaries (reuse `scripts/build.sh` / `build-agents.sh`), produce
   `SHA256SUMS`, and attach them to a **GitHub Release**. Pin the Go version.
2. **Preflight + requirements install** (`deploy/REQUIREMENTS.md` + the installer): verify root,
   systemd, Docker + Compose, `openssl`, **cgroup v2 + nftables**, free ports (443/6789/7443/9091/22),
   disk space, min kernel/OS. Beyond `--check`, the installer **installs the missing OS-level
   requirements** (Docker + Compose, `openssl`, …) via the host package manager — gated behind consent
   (interactive y/N or `--yes`), skippable with `--no-install`, and a clear error if it can't. Also
   offer a **native (no-Docker) proxy path**: run Caddy + Authelia as standalone binaries + systemd
   units instead of containers, for hosts that can't/won't run Docker (`--proxy=docker|native`). Document
   the pinned Caddy/Authelia versions (image tags and native binary versions) here.
3. **Bootstrap installer** (`deploy/install-server.sh`, runnable as `curl … | sudo bash`): downloads the
   release asset for the host, **verifies the checksum**, runs preflight, then **interactively prompts**
   for the values that can't be defaulted (panel + auth domains, admin user/email, TLS mode: internal CA
   vs public/ACME, any non-default ports/paths) and **auto-generates all secrets + the fleet CA**. It
   renders `config.json`, Caddyfile, and Authelia config from templates, installs the systemd units
   (`fleet-server` + the proxy), and brings the proxy up. A **non-interactive** mode (`--answers
   answers.env` / flags) must exist. **Save the answers (secrets excluded)** to
   `/etc/fleet-server/install-answers.env` so upgrades don't re-prompt.
4. **Upgrade** (`deploy/upgrade-server.sh`, or `install-server.sh --upgrade`): query the Releases API,
   compare versions, then **stop → back up the data dir → swap the binary → run `fleet-server migrate`
   (T-0035) → restart → health-check → rollback (restore binary + data backup) on failure**. **Preserve
   the CA and the operator's edited config/secrets** — ship templates as `*.example`, never clobber live
   files; reuse `install-answers.env`, prompting only for genuinely new keys. Proxy upgrade = `docker
   compose pull && up -d` on **pinned tags**, state preserved in `/var/lib/fleet-proxy/`.
5. **Proxy home** (`deploy/proxy/fleet-proxy.service` + `compose.yml`): run the proxy via systemd from
   `/opt/fleet/proxy/` with mutable state bind-mounted from `/var/lib/fleet-proxy/` (per D-0008 + the
   agreed proxy layout), not from a repo clone.
6. **GitHub-facing `README.md`** (repo root, short, operator-focused): quickstart (`curl|bash` + the
   **private-repo auth note**), First deploy (preflight → prompts → install → proxy up → DNS → login),
   Updating (the `--upgrade` flow; CA + config/secrets preserved), a requirements + ports table, and a
   one-line file-layout pointer to `deploy/README.md` / D-0008.

## Out of scope

- The migration logic itself (T-0035 — this calls `fleet-server migrate`); agent-side update rollout;
  changing the panel or agent code.

## Done means

- A tagged build produces a GitHub Release carrying `fleet-server` + 4 agent binaries + `SHA256SUMS`.
- `install-server.sh` does a clean interactive first install (and a non-interactive one via `--answers`)
  that passes preflight, generates secrets + CA, and leaves the panel reachable behind the proxy.
- `install-server.sh --upgrade` (or `upgrade-server.sh`) upgrades an older deployment via
  backup→swap→migrate→restart with rollback-on-fail, **preserving the CA and local config/secrets**.
- `shellcheck` is clean on the scripts; the release workflow is valid YAML; README covers install /
  first deploy / update. (No Go tests — this task is scripts/CI/docs.)

## Facts this brief relies on

- The reference deploy stack (Caddy + Authelia + install-server.sh + systemd) and the panel on
  loopback `:6789` behind the proxy: `workflow/tasks/T-0024-deployment/task.md`; `deploy/README.md`;
  `workflow/decisions/D-0004-deployment.md`; `workflow/decisions/D-0008-deployment-layout.md`.
- Multi-platform agent binaries + `scripts/build.sh`/`build-agents.sh`:
  `workflow/tasks/T-0032-agent-binaries-multiplatform/task.md`.
- `fleet-server migrate` + schema versioning (prerequisite): `workflow/tasks/T-0035-server-state-versioning-migrate/task.md`.

## Notes for the doer

- The repo is **Private** — `curl|bash` and the Releases API need auth (a `gh` token or scoped deploy
  token); document it, don't hardcode a secret. Keep scripts idempotent and re-runnable. Prefer POSIX
  sh / bash, `set -euo pipefail`, and `shellcheck`-clean. Do NOT push/merge/branch-delete or edit
  decisions/generated/other tasks/`state:` (except via `wf start`); save out-of-touches needs as a
  `.patch` and report. Broad task — commit in logical parts (pipeline, installer, upgrade, proxy,
  README) and report the split.
