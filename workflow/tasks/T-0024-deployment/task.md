---
id: T-0024
title: deployment
state: merged
touches: [deploy/README.md, deploy/authelia/configuration.yml, deploy/authelia/users.yml.example,
          deploy/proxy/compose.yml, deploy/install-server.sh, deploy/systemd/fleet-server.service]
tests: []
after: [T-0011]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Follow deploy/README.md to stand up the main server behind the auth proxy: https reaches the panel with 2FA; the agent↔server channel still works."
---

## Why

The main-server install and the **panel HTTPS via the auth proxy** were left open (D-0004). Produce a
reference deployment so the platform can actually be stood up: an **Authelia** reverse proxy terminating
TLS + 2FA in front of `fleet-server:6789`, a main-server install path (first-run CA, data dir, the
`fleet-agent` binary path onboarding needs), and docs.

## What to build

**Ops/config + docs (no app logic).**

1. **Auth proxy** (`deploy/proxy/compose.yml` + `deploy/authelia/`): Authelia (local users file + TOTP
   2FA) in front of `fleet-server`, terminating **HTTPS** and forwarding the authenticated user header
   to `:6789`. A self-signed/placeholder cert is fine for the reference; document swapping in a real one.
2. **Main-server install** (`deploy/install-server.sh` + `deploy/systemd/fleet-server.service`): install
   the `fleet-server` binary + unit, set `FLEET_DATA_DIR`, ensure first run creates the internal CA, and
   set `AgentBinaryPath` (the `fleet-agent` binary onboarding pushes). Idempotent.
3. **`deploy/README.md`**: step-by-step — build, install the server, bring up the proxy, add the first
   Authelia user, open the panel over HTTPS, onboard the first agent. Document the open packaging choice
   (tarball vs .deb vs container) and that Pomerium is an alternative to Authelia.

## Out of scope

- App code changes (the panel already trusts the forwarded header); the backup server; CI.

## Done means

- `deploy/` contains a working reference: the proxy config terminates HTTPS + 2FA and forwards to
  `:6789`; `install-server.sh` installs the server unit with the data dir + agent-binary path; the
  README walks the full stand-up.
- The compose/config files are valid (lint/parse where tooling exists); the README is accurate against
  the current binaries/flags.

## Facts this brief relies on

- Panel behind Authelia/Pomerium + 2FA; `fleet-server` trusts forwarded auth headers; panel on `:6789`:
  `Fleet_Control_Monitoring_Platform_Architecture.md:68`; `workflow/decisions/D-0004-deployment-basics.md`.
- First install generates the internal CA; onboarding needs the agent binary path:
  `Fleet_Control_Monitoring_Platform_Architecture.md:249`; `workflow/tasks/T-0006-machine-registry-onboarding/task.md`.

## Notes for the doer

- This is config + docs, not Go. Keep the reference minimal and correct; don't invent app features.
- If a packaging decision is genuinely needed, pick the simplest (tarball + systemd) for the reference
  and note the alternatives in the README for the operator to choose.
