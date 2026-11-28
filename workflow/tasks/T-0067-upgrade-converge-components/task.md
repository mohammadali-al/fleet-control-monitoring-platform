---
id: T-0067
title: upgrade-converge-components
state: merged
touches: [deploy/upgrade-server.sh, deploy/install-server.sh, deploy/README.md, deploy/REQUIREMENTS.md]
tests: []
after: [T-0055, T-0064]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Upgrading an already-installed server (upgrade-server.sh / install-server.sh --upgrade) now CONVERGES newly-added bundled components and proxy routes, not just the fleet-server binary: it installs/verifies VictoriaMetrics + Grafana (pinned binaries, systemd units, state dirs) if missing, and re-renders the Caddy + Authelia config from the saved install-answers.env so new routes (e.g. /grafana) work. Operator data/CA/secrets and live config.json are preserved; the flow still backs up + rolls back. After an upgrade, Grafana no longer 404s."
---

## Why

`upgrade-server.sh` only swaps the `fleet-server` binary (+ migrate + restart) and upgrades the *existing*
proxy; it does **not** provision components added in later versions. A box upgraded after T-0055/T-0064 gets
the new binary (which serves the Monitor Grafana embed tab) but **not** the Grafana service or the Caddy
`/grafana` route + Authelia rule — so the embed 404s. The upgrade must **converge** the install: add
newly-bundled services and refresh proxy/auth configs idempotently.

## What to build

1. **Converge bundled components on upgrade.** In `deploy/upgrade-server.sh` (and keep
   `install-server.sh --upgrade` delegation working), after the binary swap + migrate, **idempotently
   install/verify the native bundled services that this version ships** — VictoriaMetrics (`:8428`) and
   Grafana (`:3000`): if the pinned binary / systemd unit / state dir / provisioning is missing or on an
   old pinned version, install/update it (pinned version + SHA256, like install does), enable + start the
   unit. Skip under `--proxy=docker` where those are containers (or handle the compose equivalent).
2. **Refresh proxy + auth config for new routes.** Re-render the Caddy `Caddyfile` and Authelia
   `configuration.yml` from the saved `install-answers.env` (domain mode, ports, proxy mode) so newly-added
   routes (e.g. `handle /grafana*` + the Authelia access rule, the Grafana provisioning) are present.
   **Back up the existing proxy configs first** and reload/restart the proxy; keep the existing
   backup→restart→health→rollback behavior. Do **not** clobber the internal CA, `/var/lib/fleet-proxy`
   secrets/user-db/certs, or the operator's live `config.json` (those stay; proxy *templates* are
   regenerated as the install flow already treats them).
3. **Share, don't fork, the provisioning logic.** Reuse `install-server.sh`'s component-install +
   config-render functions rather than duplicating pinned versions/checksums in two places — factor shared
   helpers so install and upgrade stay in lockstep (both scripts are fetched standalone via `curl`, so keep
   them self-contained: either a sourced shared snippet staged in the release, or upgrade invoking the
   installer's idempotent reconcile path). State the approach in the log.
4. **Docs.** `deploy/README.md` (upgrades now provision new components + refresh proxy routes) and
   `deploy/REQUIREMENTS.md` if needed.

## Out of scope

- No change to the data-dir/CA/secret handling, the migrate step, or `config.json` preservation; no new
  components beyond what the current version already bundles (VictoriaMetrics, Grafana); no live-box run by
  the doer (supervisor/operator tests on a real box).

## Done means

- `upgrade-server.sh` on an existing install **adds VictoriaMetrics + Grafana if missing** (pinned binary +
  unit + state dir + provisioning) and **re-renders Caddy + Authelia** from `install-answers.env` so
  `/grafana` works, preserving data/CA/secrets/`config.json`, with backup + rollback intact. Idempotent (a
  second upgrade is a no-op for already-current components). `--proxy=docker` handled or cleanly skipped.
- Pinned versions/checksums are **not duplicated** divergently between install and upgrade. `shellcheck`
  clean on both scripts; `bash -n` clean; docs updated.

## Facts this brief relies on

- `upgrade-server.sh` swaps binary + migrate + restart + proxy upgrade, reads `install-answers.env`, but
  installs no new components / re-renders no proxy routes: `deploy/upgrade-server.sh` (header lines 2-11,
  `--skip-proxy`, `FLEET_PROXY_MODE`/dirs, binary swap ~L410). `install-server.sh --upgrade` delegates to
  it: `deploy/install-server.sh:10,125,176`. Component install + config render live in
  `install-server.sh` (`install_victoriametrics_binary`/`render_*` from T-0055, `install_grafana_binary`/
  `render_caddyfile`/`render_authelia_config`/grafana provisioning from T-0064).

## Notes for the doer

- Keep both scripts `curl`-pipe-safe and self-contained; whatever sharing approach you pick, verify with
  `shellcheck` + `bash -n`. Preserve the existing upgrade safety (backup/health/rollback). Do NOT run the
  installer/upgrader against this host. Run `./scripts/wf start T-0067` before committing; commit inside
  touches + the log. Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:`
  (except via `wf start`).
