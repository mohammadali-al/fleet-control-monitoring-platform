---
id: T-0037
title: secret-scan-history
state: ready
touches: [security/secret-scan-report.md, security/gitleaks.toml]
tests: []
after: []
shares: {}
model: {claude: "sonnet high"}
operator_check: "A secret/sensitive-data scan of the FULL git history + working tree has been run; security/secret-scan-report.md lists every finding (secret, credential, private key, token, personal data) with file + commit, or states 'clean'. The doer does NOT rewrite history or rotate anything — it only reports + recommends."
---

## Why

The repo was made **public** on 2026-11-25. Public = the entire git history is exposed, so a single
secret ever committed (even if later deleted) is leaked. Before relying on it, scan the whole history and
working tree and produce a report. Believed clean (`data/`, `deploy/proxy/state/`, `settings.local.json`
gitignored; machine credentials and the operator's email were only ever in chat, never committed — see
D-0002), but **confirm**, don't assume.

## What to build

A **read-only audit + a written report** (this task changes nothing in the code):

1. **Run a secret scanner over the FULL history** (all commits, all branches, not just HEAD) and the
   working tree — e.g. `gitleaks detect --redact` and/or `trufflehog git file://.`. If the tool isn't
   installed, install it locally for the run (or use `docker run`); do not add it as a repo dependency.
   A tuned `security/gitleaks.toml` (allowlist for known-safe test fixtures) may be added.
2. **Targeted greps over history** for high-value patterns the generic rules may miss: `BEGIN .*PRIVATE
   KEY`, the fleet **CA key**, **Authelia** secrets (JWT/session/storage/HMAC), password literals
   (`password`/`passwd`/`Admin@`-style strings — the operator can supply the exact machine password
   out-of-band; never write it into this repo), API/deploy tokens, AWS keys, `.env` contents, the
   operator's **email**, and hard-coded **machine IPs / credentials**. Scan history with
   `git log -p -S<pattern>` / `git grep` across all commits.
3. **Confirm the gitignored-sensitive paths were never committed** in any past commit:
   `data/`, `deploy/proxy/state/`, `.claude/settings.local.json`, `.claude/worktrees/`,
   `*.bak.md`, the CA material, agent binaries.
4. **Write `security/secret-scan-report.md`**: tools + commands run, scope (commit range / branches),
   and a findings table (what, where = `path@commit`, severity) — or a clear "no secrets found". For any
   real finding, **recommend** remediation (rotate the secret, then purge history with `git filter-repo`
   / BFG, force-push, invalidate caches) but **DO NOT perform it** — history rewrite + rotation is the
   supervisor/operator's call.

## Out of scope

- **No history rewrite, no force-push, no secret rotation** — recommend only.
- No changes to application code, CI, or deploy config. No new runtime dependency.

## Done means

- The scanner + targeted greps have been run over the **full history** and working tree, and
  `security/secret-scan-report.md` exists with either an enumerated findings list (`path@commit` +
  severity + recommended fix) or an explicit "clean" with the scope/commands shown.
- The gitignored-sensitive paths are confirmed absent from all history.
- No repository behavior changed; nothing was rotated or rewritten.

## Facts this brief relies on

- Repo made public 2026-11-25; sensitive paths are gitignored (`.gitignore`): `data/`,
  `deploy/proxy/state/`, `.claude/settings.local.json`, `.claude/worktrees/`.
- Secrets must never be committed; CA private key lives under the (gitignored) data dir:
  `workflow/decisions/D-0002-data-rules.md`; `workflow/decisions/D-0006-data-directory.md`;
  `workflow/decisions/D-0008-deployment-layout.md`.

## Notes for the doer

- A linked worktree shares the main repo's full history, so `git log`/scanners see all commits — scan
  the whole range, not just your branch. Run `./scripts/wf start T-0037` before committing; commit only
  the report (+ optional `gitleaks.toml`) + the `log.md` entry. Do NOT push/merge/branch-delete, rewrite
  history, force-push, or edit decisions/generated/other tasks/`state:` (except via `wf start`). If you
  find a live secret, STOP after writing the report and flag it prominently — do not try to fix it.
