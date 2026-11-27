---
id: T-0043
title: enroll-addr-default
state: merged
touches: [deploy/install-server.sh, deploy/README.md, deploy/REQUIREMENTS.md]
tests: []
after: [T-0036]
shares: {}
model: {claude: "sonnet high"}
operator_check: "A fresh install writes config.json with ALL THREE enrollment fields — server.enroll_addr (\":7445\"), server.enroll_host (the panel domain), AND server.agent_dial_addr (\":7444\") — so fleet-server starts the enrollment listener cleanly (NOT a crash-loop) and the panel's 'Add agent (token)' button is enabled out of the box. No new required flag; ports overridable like the others. Existing installs are not clobbered."
---

## Why

Token enrollment is off on every fresh deploy: the installer's generated `config.json` sets none of the
enrollment fields, so `fleet-server` doesn't open the enrollment listener and the panel **disables** the
"Add agent (token)" button (`web/assets/js/live.js:352-355` gates on `GET /api/enroll/machines` returning
OK). Make token enrollment **on by default** so the button works without manual config.

**Critical correctness fact (verified live on a real box 2026-11-27):** enabling enrollment requires
**three** fields together — setting only `enroll_addr` + `enroll_host` makes `fleet-server` **exit 1 in a
crash-loop** with `"token enrollment needs the agent_dial listener: set agent_dial_addr"`
(`cmd/fleet-server/enroll.go:69`; `cmd/fleet-server/main.go:120-123` returns 1 on `enableEnroll` error).
So the config MUST also set `agent_dial_addr`. With all three set, startup is clean and
`GET /api/enroll/machines` returns 200.

## What to build

In `deploy/install-server.sh` only (plus docs):

1. **Config template.** In `render_server_config_example()` (~`:936`), add **three** fields to the
   `server` block:
   - `"enroll_addr": ":${FLEET_ENROLL_PORT}"`
   - `"enroll_host": "${FLEET_PANEL_DOMAIN}"`  (required when enroll_addr is set — `internal/config/config.go:112`)
   - `"agent_dial_addr": ":${FLEET_AGENT_DIAL_PORT}"`  (**required** for enrollment — `cmd/fleet-server/enroll.go:69`)
   Keep valid JSON; don't reorder other fields.
2. **Port variables.** Add `FLEET_ENROLL_PORT` (default **`7445`**) and `FLEET_AGENT_DIAL_PORT` (default
   **`7444`**), both env/`--answers` overridable and prompted in the existing custom-ports block — mirror
   the `FLEET_AGENT_PORT` pattern exactly (init ~`:85`, answers-file case ~`:232`, `:-default` ~`:318`,
   custom-ports prompt ~`:881`, the `install-answers.env` echo block ~`:910`, and the preflight
   `check_port_state` list ~`:612`). **No new required CLI flag.**
3. **Preserve existing installs.** Keep the write-if-absent behavior (`:1406-1413`): existing
   `config.json` is not overwritten, only `config.json.example` is refreshed. Note in docs that existing
   deployments aren't auto-flipped (add the three fields + restart, or reinstall fresh).
4. **Docs.** `deploy/REQUIREMENTS.md` ports table: add **`7445/tcp`** (token-enrollment HTTPS, server ←
   enrolling agents, bypasses the proxy, own pinned-pubkey TLS) **and `7444/tcp`** (agent-dial mTLS,
   agent-initiated connections used by token-enrolled agents); both now **on by default**, overridable via
   `FLEET_ENROLL_PORT` / `FLEET_AGENT_DIAL_PORT`. Move them out of the "optional/disabled by default"
   list if present. `deploy/README.md`: note token enrollment is on by default (all three fields), that
   **both 7445 and 7444** must be reachable from machines you enroll, and the override env vars.

## Out of scope

- **No firewall/nftables rule additions** — the installer does not manage accept rules today (it only
  checks port availability); reachability stays the operator's concern. No Go changes, no panel changes,
  no new interactive domain/flag prompt, no enrollment-protocol/TLS changes.

## Done means

- A fresh install's `config.json` (and `config.json.example`) contains all three: `enroll_addr: ":7445"`,
  `enroll_host: "<panel-domain>"`, `agent_dial_addr: ":7444"`; the implied startup is **clean** (the
  missing-`agent_dial_addr` crash cannot happen); the panel's "Add agent (token)" button is enabled.
- `FLEET_ENROLL_PORT` (7445) and `FLEET_AGENT_DIAL_PORT` (7444) default correctly, are overridable via
  env/`--answers`/the custom-ports prompt, and are saved to `install-answers.env`. Existing `config.json`
  is preserved.
- `shellcheck` clean on `install-server.sh`; `bash -n` clean; a JSON dry-render of
  `render_server_config_example` parses and shows all three fields; REQUIREMENTS ports table + README updated.

## Facts this brief relies on

- Enrollment needs enroll_host **and** agent_dial_addr, else startup fails: `internal/config/config.go:104-113`;
  `cmd/fleet-server/enroll.go:65-71` (both guards) + `:99` (CA, already present); `cmd/fleet-server/main.go:120-123`
  (returns 1 on error). Verified live: three fields → `token enrollment listening :7445` + api 200.
- Config template + write-if-absent: `deploy/install-server.sh:936` (`render_server_config_example`),
  `:1406-1413`. Port var pattern: `FLEET_AGENT_PORT` at `:85,:232,:318,:612,:881,:910`.
- Panel disables the enroll button when `GET /api/enroll/machines` is not OK: `web/assets/js/live.js:352-355`.

## Notes for the doer

- Keep the config valid JSON; don't reorder unrelated fields. Reuse the already-validated panel domain for
  `enroll_host`. Run `./scripts/wf start T-0043` before committing; commit inside touches + the log. Do
  NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`). A
  hook refusal means stop + report verbatim.
