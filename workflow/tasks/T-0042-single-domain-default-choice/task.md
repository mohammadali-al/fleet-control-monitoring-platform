---
id: T-0042
title: single-domain-default-choice
state: ready
touches: [deploy/install-server.sh, deploy/README.md, deploy/REQUIREMENTS.md]
tests: []
after: [T-0039]
shares: {}
model: {claude: "sonnet high"}
operator_check: "A plain interactive install now ASKS 'one DNS name or two?' and DEFAULTS to single-domain (just press Enter) — panel on https://<domain>/ with Authelia under /authelia; choosing two still prompts for the separate auth subdomain. `--single-domain`/`--two-domain` flags and `--answers`/env still override the prompt and are never asked. Non-interactive with no mode set now defaults to single. The two-domain path still works unchanged when chosen."
---

## Why

Single-domain (one DNS name, Authelia under `/authelia`) is the simpler, more common setup and is now the
recommended default, but today the installer silently defaults to the **two**-subdomain layout
(`deploy/install-server.sh:807` → `FLEET_DOMAIN_MODE="${FLEET_DOMAIN_MODE:-two}"`) and never asks, so a
plain interactive install prompts for a second auth domain the operator usually doesn't want. Make
single-domain the **interactive default choice**: ask which layout, defaulting to single, and flip the
non-interactive fallback to single too. **Installer + docs only; no proxy/Authelia template changes** —
the single-domain rendering already exists from T-0039.

## What to build

In `deploy/install-server.sh`, inside `collect_answers` (where domain prompting happens, ~lines 807–827):

1. **Interactive mode prompt.** When `FLEET_DOMAIN_MODE` is still empty after flags/`--answers`/env (i.e.
   the operator passed neither `--single-domain` nor `--two-domain` nor set it via answers) **and** a TTY
   is available **and** this is not `--non-interactive`: ask a clear single question, e.g.
   `"DNS layout: [1] one name (panel + Authelia under /authelia)  [2] two names (separate auth subdomain)"`
   defaulting to **1 / single** (pressing Enter picks single). Map the answer to `single`/`two`. Keep it
   consistent with the existing `prompt_val` style/`/dev/tty` usage; accept `1`/`single`/`s` and
   `2`/`two`/`t` (be tolerant), re-ask or fall back to single on empty.
2. **Default fallback → single.** Change `FLEET_DOMAIN_MODE="${FLEET_DOMAIN_MODE:-two}"` so the fallback
   when unset (non-interactive, or TTY-less) is **`single`**, not `two`. The explicit-flag / answers /
   env paths (`--single-domain`, `--two-domain`, `FLEET_DOMAIN_MODE=...`) must still win and must **not**
   trigger the prompt (respect an already-set value — mirror the early validation at ~line 301).
3. **Only the single path continues to skip the auth-domain prompt; two still asks** for
   `FLEET_AUTH_DOMAIN` exactly as now. No change to the rendered Caddyfile/Authelia templates, ports, or
   `install-answers.env` keys — the mode is still saved as before (`FLEET_DOMAIN_MODE=...`).
4. **Docs.** Update `deploy/README.md` and `deploy/REQUIREMENTS.md` so the quickstart reflects that an
   interactive install now **asks** and **defaults to one domain** (and how to force either:
   `--single-domain` / `--two-domain`). Fix any wording that still calls two-domain "the default".

## Out of scope

- No change to the single-domain Caddy/Authelia rendering or `/authelia` path (done in T-0039); no change
  to `upgrade-server.sh` (it doesn't prompt for domains); no new flags; no IP-only support (still
  unsupported — keep the `is_ip_literal` guards). Don't touch Go, the panel, or proxy binaries.

## Done means

- Interactive install with no domain flag **prompts** for one-vs-two and **defaults to single** on empty
  input; choosing two prompts for the auth subdomain as before.
- `--single-domain` and `--two-domain` (and `FLEET_DOMAIN_MODE` via env/`--answers`) **skip the prompt**
  and are honored; `--non-interactive`/no-TTY with nothing set resolves to **single** (no hang, no `die`).
- `install-answers.env` records the resolved mode; the two-domain flow is otherwise unchanged.
- `shellcheck` clean on `install-server.sh`; `bash -n` clean; README/REQUIREMENTS updated. (A real-box
  check as in T-0038 is nice-to-have, not required — the supervisor can live-test.)

## Facts this brief relies on

- Default is hard-wired to two with no prompt: `deploy/install-server.sh:807`
  (`FLEET_DOMAIN_MODE="${FLEET_DOMAIN_MODE:-two}"`); mode validated early at `:301-304`; flags parsed at
  `:163-164`; answers/env at `:222`, `:75`.
- Domain prompting + auth-domain-skipped-in-single lives at `deploy/install-server.sh:813-827`; the
  single-domain Caddy/Authelia rendering begins at `:936`. `prompt_val`/`has_tty`/`/dev/tty` at
  `:331-351`; non-interactive guard via `has_tty || die`.
- Single-domain mode, its `/authelia` layout and one-DNS-name requirement were added by T-0039:
  `workflow/tasks/T-0039-single-domain-auth-path/task.md`; `deploy/proxy/Caddyfile`,
  `deploy/authelia/configuration.yml`.

## Notes for the doer

- Keep the prompt short and the mapping forgiving; never prompt when the mode was already supplied. Don't
  reorder unrelated prompts. Run `./scripts/wf start T-0042` before committing; commit inside touches +
  the log. Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via
  `wf start`). A hook refusal means stop + report verbatim.
