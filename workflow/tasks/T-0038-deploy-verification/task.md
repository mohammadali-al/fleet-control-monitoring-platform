---
id: T-0038
title: deploy-verification
state: ready
touches: [deploy/install-server.sh, deploy/upgrade-server.sh, deploy/proxy/compose.yml,
          deploy/proxy/fleet-proxy.service, deploy/REQUIREMENTS.md, deploy/verification-report.md]
tests: []
after: [T-0036]
shares: {}
model: {claude: "sonnet high"}
operator_check: "`shellcheck` is clean on the deploy scripts (any findings fixed), AND a real end-to-end `install-server.sh` then `upgrade-server.sh` run on the operator's test box succeeded (server + proxy up, panel reachable over HTTPS; upgrade applied, migrate ran, rollback path verified). Results are written to deploy/verification-report.md, which contains NO secrets, credentials, or full host addresses."
---

## Why

T-0036 shipped `install-server.sh` / `upgrade-server.sh` but they were only syntax-checked (`bash -n`)
and dry-run staged — never **lint-clean** or **actually run on a machine**. Deployment is only real once
the scripts pass `shellcheck` and a true end-to-end install + upgrade works on a box.

## What to build / do

1. **shellcheck pass.** Install `shellcheck` (locally, not as a repo dep), run it on
   `deploy/install-server.sh`, `deploy/upgrade-server.sh`, and any other deploy shell. **Fix** the real
   findings (quoting, unset vars, `[ ]` vs `[[ ]]`, masked return codes, etc.) inside the touched scripts.
   Keep behavior identical — this is hardening, not a redesign.
2. **Real end-to-end on the operator's test box** (details below — NOT in this repo):
   - **Install:** run `install-server.sh` on a clean box (try the **default `--proxy=docker`** path;
     if Docker install isn't desired, also exercise `--proxy=native`). Verify: preflight passes,
     deps install, secrets + CA generated, `fleet-server` is up on loopback `:6789`, the proxy is up,
     and the panel is reachable over **HTTPS** through Caddy with the Authelia login.
   - **Upgrade:** simulate an older deployment, then run `upgrade-server.sh` — verify the
     backup → swap → `fleet-server migrate` → restart → health-check flow, that the **CA and live
     config/secrets are preserved**, and that the **rollback** path triggers on an induced failure.
   - Capture what worked / what broke; fix script bugs found (inside touches) and re-run until green.
3. **Report:** write `deploy/verification-report.md` — shellcheck result (clean / fixed), the install +
   upgrade + rollback outcomes, OS/versions used, and anything still flaky. **Redact everything
   sensitive**: no passwords, secrets, tokens, or full host IP/hostnames (say "the test box"); use
   placeholders.

## Test box (provided out-of-band — do NOT write it into any committed file)

- The operator provides a **throwaway Linux test box** reachable over SSH as **root**; its IP and
  password are supplied by the supervisor **at run time only** and must never be committed to this
  (public) repo, printed in the report, or logged. Treat them as secrets.

## Out of scope

- Redesigning the installer/upgrader (T-0036); application/Go changes; the release pipeline.

## Done means

- `shellcheck` is clean on the deploy scripts (findings fixed, behavior unchanged).
- A real `install-server.sh` run leaves the panel reachable over HTTPS behind the proxy on the test box;
  a real `upgrade-server.sh` run upgrades it (migrate runs, CA + config preserved) and its rollback path
  is verified.
- `deploy/verification-report.md` records the outcomes with **no secrets / credentials / full host
  addresses**.

## Facts this brief relies on

- The installer/upgrader + proxy layout: `workflow/tasks/T-0036-github-release-and-installer/task.md`;
  `deploy/install-server.sh`; `deploy/upgrade-server.sh`; `deploy/REQUIREMENTS.md`.
- `fleet-server migrate` (called by the upgrade): `workflow/tasks/T-0035-server-state-versioning-migrate/task.md`.
- Panel on loopback `:6789` behind Caddy(HTTPS)+Authelia: `workflow/decisions/D-0004-deployment.md`;
  `workflow/decisions/D-0008-deployment-layout.md`.

## Notes for the doer

- This task **touches real infrastructure over SSH as root** — be careful and idempotent; prefer a
  disposable box; tear down / note any residue. Never echo the password or write it anywhere. Run
  `./scripts/wf start T-0038` before committing; commit only the script fixes + the redacted report +
  the `log.md` entry. Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:`
  (except via `wf start`). If a real script bug is outside touches, save a `.patch` and report it.
