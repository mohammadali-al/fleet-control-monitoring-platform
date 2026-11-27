# T-0042 Log

## 2026-11-27 — Single-domain default choice

- Ran `./scripts/wf start T-0042` to create and switch to `task/T-0042` with `.githooks` wired.
- `deploy/install-server.sh`:
  - Inside `collect_answers()`, added an interactive `/dev/tty` prompt when `FLEET_DOMAIN_MODE` is still empty after CLI flags, `--answers`, env, and any saved `install-answers.env`, and `NON_INTERACTIVE=0` with `has_tty`: asks `DNS layout: [1] one name (panel + Authelia under /authelia)  [2] two names (separate auth subdomain) [1]:`, accepting `1`/`single`/`s`/`one` (and empty Enter) as `single`, `2`/`two`/`t` as `two` (case-insensitive, trimmed), and warning + falling back to `single` on unrecognized input.
  - Changed the unset fallback `FLEET_DOMAIN_MODE="${FLEET_DOMAIN_MODE:-two}"` to `FLEET_DOMAIN_MODE="${FLEET_DOMAIN_MODE:-single}"` so non-interactive / no-TTY runs without an explicit mode default to `single`.
  - Preserved early `FLEET_DOMAIN_MODE` validation, explicit `--single-domain`/`--two-domain`/env/`--answers` precedence (skipping the prompt), the `FLEET_AUTH_DOMAIN` prompt in `two` mode, `is_ip_literal` IP guards, `install-answers.env` keys, and Caddyfile/Authelia templates. Updated header/usage comments so `--single-domain` is marked `(default)` instead of `--two-domain`.
- `deploy/README.md` & `deploy/REQUIREMENTS.md`: updated prerequisites, quickstart description, option descriptions, and first-login redirect wording to reflect that interactive installs ask and default to one domain, non-interactive/no-TTY defaults to single-domain, and `--single-domain` / `--two-domain` force either mode.
- Verification: `bash -n deploy/install-server.sh` clean; `shellcheck` (`0.10.0`, installed to `/tmp/sc-t0042` and removed afterward) clean on `deploy/install-server.sh`. Did not run the installer against this host (supervisor handles live-box verification).

wf: T-0042 done
