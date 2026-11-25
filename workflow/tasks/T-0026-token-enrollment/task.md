---
id: T-0026
title: token-enrollment
state: ready
touches: [internal/enroll/enroll.go, internal/enroll/install.go, internal/enroll/enroll_test.go,
          internal/pki/ca.go, internal/registry/registry.go, internal/panel/machines.go,
          internal/panel/machines_test.go, web/pages/machines.html, web/assets/js/live.js,
          cmd/fleet-server/main.go, internal/config/config.go]
tests: [server]
after: [T-0006, T-0025]
shares: {}
model: {claude: "sonnet high"}
operator_check: "In Machines, 'Add agent (token)' gives a short code + install one-liner; running it on a Linux/macOS box enrolls it as a permanent agent that appears with its specs; Stop keeps it down across reboot; Detach revokes it."
---

## Why

An SSH-free, firewall-friendly onboarding for **both worker and monitor** machines (complements the SSH
path, T-0006): the operator generates a token; the user runs one `curl … | sudo bash` as root; the
machine dials **out** (`agent_dial`, T-0025) and becomes a permanent agent. Needed when SSH isn't
available or the agent is behind NAT/firewall.

## What to build

**Backend (Go) + Machines-page UI.**

1. **Token** (`internal/enroll`): panel "Add agent (token)" (role worker|monitor) mints a **one-time,
   single-use, ~10-minute-TTL** enrollment token + a **short human-typable code** (6–8 chars) + the
   install one-liner (Linux + macOS). The token binds to the **first** machine that redeems it and then
   can't be reused for another.
2. **Enrollment endpoint** (`cmd/fleet-server` + `internal/enroll`): a **token-gated HTTP(S) endpoint
   that bypasses the operator auth proxy** (the token is the auth). It serves the pinned **CA cert** and
   the **agent binary** over TLS; the agent generates its **own keypair + CSR** and exchanges
   **CSR + token** for a **signed client cert** (`internal/pki` signs; the private key never leaves the
   machine). The server records the machine (role, id, cert fingerprint, reported **specs**) in the
   registry.
3. **Installer** (`internal/enroll/install.go`): the bootstrap script installs a **permanent service** —
   **systemd on Linux**, **launchd on macOS** — enabled at boot, `Restart=always`/`KeepAlive`, role +
   `connect_mode=agent_dial` baked in. **macOS is monitor-only** (refuse a macOS *worker* — capture is
   Linux-only); Linux allows both. Expose a **"show script"** link so the installer can be read first.
4. **Lifecycle in the panel:** the machine shows with specs and a reusable entry. **Stop** = `disable`
   the unit (stays down through reboot; the machine's local `fleet-agent start` re-enables it). **Detach**
   = **revoke the client cert** + forget the machine (re-adding needs a fresh token).

## Out of scope

- Changing SSH onboarding (T-0006); the `agent_dial` channel itself (T-0025); a public relay for a fully
  firewalled server.

## Done means

- Minting a token returns a single-use, 10-min code + a valid Linux and macOS one-liner; a second
  redemption of a used token is refused; an expired token is refused.
- Against an in-process enrollment server + fake install target: the CSR→cert exchange issues a cert
  bound to the machine (private key never sent), the registry gets the machine + specs, and a macOS
  **worker** enrollment is refused.
- **Stop** disables the unit (verified in the rendered unit/commands); **Detach** revokes the cert so the
  agent can't reconnect.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes; the panel still serves all pages.

## Facts this brief relies on

- Internal CA signs per-machine client certs; mTLS: `Fleet_Control_Monitoring_Platform_Architecture.md:181`;
  `workflow/tasks/T-0002-internal-ca/task.md`.
- Onboarding writes the agent + unit + identity (the SSH path to complement): `Fleet_Control_Monitoring_Platform_Architecture.md:195`;
  `workflow/tasks/T-0006-machine-registry-onboarding/task.md`.
- `agent_dial` outbound channel (prerequisite): `workflow/tasks/T-0025-agent-dial-channel/task.md`.
- Agent runs as root; capture is Linux-only: `Fleet_Control_Monitoring_Platform_Architecture.md:249`;
  `workflow/tasks/T-0020-onboarding-agent-privileges/task.md`.

## Notes for the doer

- **Broad — split if needed:** enrollment backend (token + CSR + registry) first, then the Machines-page
  UI + installer. Commit the first half and report the split if it won't land clean in one.
- The enrollment endpoint must verify its own server TLS so `curl|bash` can't be MITM'd; never log the
  token or any password; the token is the only auth on that endpoint.
- If `cmd/fleet-server/main.go` needs more than a small addition, save a `.patch` and report it.
