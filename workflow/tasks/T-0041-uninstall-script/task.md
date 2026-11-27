---
id: T-0041
title: uninstall-server-script
state: merged
touches: [deploy/uninstall-server.sh, deploy/README.md, deploy/REQUIREMENTS.md, .github/workflows/release.yml]
tests: []
after: [T-0036, T-0038, T-0039]
shares: {}
model: {claude: "sonnet high"}
operator_check: "`deploy/uninstall-server.sh` cleanly removes everything install-server.sh created (fleet + proxy systemd units, binaries, /etc/fleet-server, /opt/fleet/proxy, and — only on explicit confirmation — the data dirs incl. the CA). It REFUSES to run without confirmation, has --dry-run, never deletes an empty/underived path, touches only known fleet paths, and leaves the box clean. Verified on a disposable box."
---

## Why

There is an installer but no uninstaller. Operators need a safe way to fully remove a Fleet main-server
deployment. This is **destructive** (it can delete the internal CA + registry), so correctness and
safety matter more than anything.

## What to build

**`deploy/uninstall-server.sh`** — a POSIX/bash script (`set -euo pipefail`, `shellcheck`-clean) that
reverses `install-server.sh`, mirroring exactly the paths/units it creates (read install-server.sh +
D-0008 so the lists stay in sync):

1. **Stop + disable + remove units:** `fleet-server.service`, `fleet-proxy.service`, native
   `fleet-caddy.service` / `fleet-authelia.service`, the liveness timers/services, and any watchdog
   units — `systemctl disable --now`, then delete the unit files, then `daemon-reload`. For `--proxy=docker`,
   `docker compose -f /opt/fleet/proxy/compose.yml down` (remove containers; volumes only with data purge).
2. **Remove binaries + config:** `/usr/local/bin/fleet-server`, `/usr/local/share/fleet/`,
   `/etc/fleet-server/` (config.json, fleet-server.env, install-answers.env), the fleet-agent wrapper,
   `/opt/fleet/proxy/` (Caddyfile, authelia config, compose).
3. **Data dirs — GATED:** `/var/lib/fleet-server/` (registry, **`ca/` = CA private key**, backups) and
   `/var/lib/fleet-proxy/` (proxy secrets/user db/certs) are deleted **only with explicit opt-in**
   (`--purge-data`, or an interactive "type the panel hostname to confirm data deletion"); default
   **keeps** them and says so loudly (deleting the CA invalidates every agent cert). A `--keep-data` is
   the explicit opposite.
4. **Flags:** `--dry-run` (print every action, delete nothing — the default recommended first run),
   `--yes` (non-interactive, still requires `--purge-data` to touch data), `--proxy-dir` /
   `--data-dir` / `--proxy-state-dir` overrides matching the installer. Reads
   `/etc/fleet-server/install-answers.env` to discover what was installed (proxy mode, dirs).
5. **Docs:** `deploy/README.md` (an Uninstall section + the `curl | sudo bash` form) and
   `deploy/REQUIREMENTS.md` if relevant; add `uninstall-server.sh` to the **release workflow**
   (`.github/workflows/release.yml`) as a staged asset + in SHA256SUMS, like install/upgrade.

## SAFETY (non-negotiable — this is the point of the task)

- **Never `rm -rf` an empty or underived variable.** Guard every removal: refuse if the target var is
  empty, is `/`, `/usr`, `/var`, `/etc`, `$HOME`, or not under the expected prefix. Prefer removing a
  fixed explicit list over globbing.
- **Confirm before destroying.** No data deletion (and ideally no removal at all) without an explicit
  `--yes`/typed confirmation; print a clear summary of what will be removed first.
- **Idempotent + tolerant:** missing units/files are not errors (it may run on a partial install).
- Do **not** uninstall shared OS packages (Docker, system `openssl`, etc.) — they may be used elsewhere;
  only remove what Fleet installed. (Optionally mention them in the final summary.)

## Out of scope

- Uninstalling agents from remote machines (that's Detach/`fleet-agent` on each box); removing OS
  packages; touching anything outside the Fleet-owned paths.

## Done means

- `uninstall-server.sh --dry-run` lists every unit/path it would remove and deletes nothing.
- A real run removes all units + binaries + config + proxy dir and (only with `--purge-data`) the data
  dirs, leaving the box clean; a second run is a no-op (idempotent); without confirmation it refuses.
- Every destructive step is guarded against empty/unsafe paths. `shellcheck` + `bash -n` clean; docs +
  release workflow updated. **Verify on a disposable box** (install → uninstall → confirm clean), like T-0038.

## Facts this brief relies on

- The exact paths/units the installer creates: `deploy/install-server.sh`;
  `workflow/decisions/D-0008-deployment-layout.md`; `deploy/README.md`.
- Native vs docker proxy + proxy home `/opt/fleet/proxy` + state `/var/lib/fleet-proxy`:
  `workflow/decisions/D-0009-default-proxy-native.md`; `workflow/tasks/T-0036-github-release-and-installer/task.md`.
- Real-box verification pattern (redacted report, creds out-of-band): `workflow/tasks/T-0038-deploy-verification/task.md`.

## Notes for the doer

- Keep the removal lists in lockstep with `install-server.sh` — if the installer path set changes, this
  must too. If a test box is used, never commit its address/creds; keep any report redacted. Run
  `./scripts/wf start T-0041` before committing; commit inside touches (+ log). Do NOT push/merge/
  branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`). A hook refusal
  means stop + report.
