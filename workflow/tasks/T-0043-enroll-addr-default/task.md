---
id: T-0043
title: enroll-addr-default
state: ready
touches: [deploy/install-server.sh, deploy/README.md, deploy/REQUIREMENTS.md]
tests: []
after: [T-0036]
shares: {}
model: {claude: "sonnet high"}
operator_check: "A fresh install writes config.json with server.enroll_addr (\":7445\") and server.enroll_host (the panel domain), so fleet-server starts the token-enrollment listener and the panel's 'Add agent (token)' button is enabled out of the box. No new required flag; the port is overridable like the others. Existing installs are not clobbered."
---

## Why

Token enrollment is off on every fresh deploy: the installer's generated `config.json` never sets
`server.enroll_addr`/`server.enroll_host`, so `fleet-server` doesn't open the enrollment listener and the
panel **disables** the "Add agent (token)" button (`web/assets/js/live.js:353` — "Token enrollment is not
enabled on this server (set server.enroll_addr)"). Make token enrollment **on by default** so the button
works without manual config. No install flag needed (per operator): just a sensible default port + the
required host.

## What to build

In `deploy/install-server.sh` only (plus docs):

1. **Config template.** In `render_server_config_example()` (~`:923`), add two fields to the `server`
   block: `"enroll_addr": ":${FLEET_ENROLL_PORT}"` and `"enroll_host": "${FLEET_PANEL_DOMAIN}"`.
   `enroll_host` is **required** whenever `enroll_addr` is set (`internal/config/config.go:112`;
   `cmd/fleet-server/enroll.go:67` errors otherwise) — use the already-collected panel domain.
2. **Port variable.** Add `FLEET_ENROLL_PORT` with default **`7445`** (mirror the `FLEET_AGENT_PORT`
   pattern: env/`--answers` overridable at `:85`/`:232`/`:318`; include it in the existing custom-ports
   prompt block alongside the agent port at ~`:881`). Save it in `install-answers.env` (the
   `FLEET_* =` echo block ~`:910`). **No new required CLI flag.**
3. **Preserve existing installs.** The installer already only writes `config.json` when absent and
   otherwise just refreshes `config.json.example` (`:1406-1413`) — keep that. So existing deployments are
   not flipped by an upgrade; note that in docs (operators enable it by adding the two fields +
   restart, or re-running fresh).
4. **Docs.** `deploy/REQUIREMENTS.md`: add **`7445/tcp`** to the ports table (server ← enrolling agents;
   token-enrollment HTTPS; **bypasses the proxy**, its own pinned-pubkey TLS). `deploy/README.md`: note
   token enrollment is on by default (`enroll_addr :7445`, `enroll_host <panel-domain>`), that the port
   must be reachable from machines you enroll (like the agent channel port), and the override env var.

## Out of scope

- **No firewall/nftables rule additions** — the installer does not manage accept rules today (it only
  checks nft kernel support + port availability); reachability stays the operator's concern, same as the
  agent channel port. No Go changes, no panel changes, no new interactive domain/flag prompt. Don't change
  the enrollment protocol or TLS — only emit the config fields.

## Done means

- A fresh install's `config.json` (and `config.json.example`) contains `server.enroll_addr: ":7445"` and
  `server.enroll_host: "<panel-domain>"`; `fleet-server` starts the enrollment listener; the panel's
  "Add agent (token)" button is enabled.
- `FLEET_ENROLL_PORT` defaults to 7445, is overridable via env/`--answers`/the custom-ports prompt, and is
  saved to `install-answers.env`. Existing `config.json` is preserved.
- `shellcheck` clean on `install-server.sh`; `bash -n` clean; REQUIREMENTS ports table + README updated.

## Facts this brief relies on

- `enroll_addr` is the token-enrollment HTTPS listener (e.g. `:7445`), separate from the panel on purpose;
  `enroll_host` is required when it is set: `internal/config/config.go:104-113`; `cmd/fleet-server/enroll.go:67`.
- Config template + write-if-absent: `deploy/install-server.sh:923` (`render_server_config_example`),
  `:1406-1413`. Port var pattern: `FLEET_AGENT_PORT` at `:85,:232,:318,:881,:910`.
- Panel disables the enroll button when the server reports enrollment off: `web/assets/js/live.js:353`.

## Notes for the doer

- Keep the config valid JSON; don't reorder unrelated fields. Panel domain is already validated (rejects
  raw IPs) — reuse it for `enroll_host`. Run `./scripts/wf start T-0043` before committing; commit inside
  touches + the log. Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:`
  (except via `wf start`). A hook refusal means stop + report verbatim.
