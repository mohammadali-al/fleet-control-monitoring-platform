---
id: T-0006
title: machine-registry-onboarding
state: merged
touches: [internal/registry/registry.go, internal/registry/registry_test.go,
          internal/registry/store.go, internal/onboard/ssh.go, internal/onboard/ssh_test.go,
          internal/config/config.go, go.mod, go.sum]
tests: [server]
after: [T-0002, T-0003]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

The main server must know its machines and be able to bring a new one online from the panel form:
IP + SSH creds + role → provisioned agent. This builds the registry model (persisted) and the SSH
onboarding flow. Live online-state comes from the channel (T-0003); this task owns identity,
persistence, and provisioning.

## What to build

**Backend (Go), main-server side.**

1. **Registry model** (`internal/registry/registry.go`): a `Machine` — `id`, `role`
   (`worker`/`monitor`), addresses (private IP(s), cached public IP + time), `connect_mode`
   (`server_dial` default / `agent_dial`), cert fingerprint, disk state, derived online state, created/
   updated times. CRUD over an in-memory map guarded for concurrency.
2. **Persistence** (`internal/registry/store.go`): load/save the registry under
   **`DataDir/registry/`** (D-0006) as JSON (atomic write: temp + rename). No DB dependency unless
   justified. Never write inside the repo/install.
3. **SSH onboarding** (`internal/onboard/ssh.go`) using `golang.org/x/crypto/ssh`, implementing the
   §4.5 flow against a machine (IP + SSH user + password **or** key + role):
   - connect over SSH; create a dedicated system user; install the `fleet-agent` binary and the systemd
     units; generate the machine ID and its client cert via `internal/pki` (T-0002); write the pinned
     CA and the `connect_mode` flag into the agent config; enable + start the service.
   - **Idempotent**: re-provisioning the same machine reuses its ID/identity, issuing a new cert only if
     missing. Provide `Remove` (decommission: stop/disable, mark removed, revoke intent).
   - The `fleet-agent` binary to install comes from a **configured path** (packaging is deferred — take
     the path from config; if absent, return a clear error).

## Out of scope

- The agent-side acceptance of provisioning (the agent binary already exists from T-0001; wiring its
  real install-time behavior beyond unit files is minimal here).
- Live connection management / online detection (that is the channel, T-0003) — expose a hook to mark
  online/offline, don't re-implement it.
- Panel UI / HTTP API wiring (a later task); this is a library + provisioning engine exercised by tests.
- Public-IP reflector (T-0008/§6.4) and disk telemetry sources — store the fields, don't populate them here.

## Done means

- Registry round-trips to disk under `DataDir/registry/` (add → save → load → same machines); atomic
  write leaves no partial file on simulated failure.
- `Validate` rejects a bad role or a duplicate ID; `connect_mode` defaults to `server_dial`.
- The onboarding flow is covered by a **test against a local in-process SSH server** (x/crypto/ssh test
  server or a mock exec transcript): it runs the documented steps in order, is idempotent on a second
  run, and surfaces a clear error when the agent-binary path is missing.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes.

## Facts this brief relies on

- Onboarding inputs and steps (SSH, install binary + units, system user, machine ID + client cert,
  pinned CA, `connect_mode`), idempotent re-provision:
  `Fleet_Control_Monitoring_Platform_Architecture.md:194`, `:195`.
- Per-machine client certs via the internal CA (T-0002): `workflow/tasks/T-0002-internal-ca/task.md`;
  `Fleet_Control_Monitoring_Platform_Architecture.md:181`.
- Default `connect_mode` `server_dial`: `Fleet_Control_Monitoring_Platform_Architecture.md:152`.
- Data under the base data dir: `workflow/decisions/D-0006-data-directory.md`.
- Provisioning uses `golang.org/x/crypto/ssh`: `workflow/decisions/D-0001-stack.md`.

## Notes for the doer

- Adds `golang.org/x/crypto/ssh` (updates `go.mod`/`go.sum`) — **needs a module download**; if offline,
  stop and report blocked.
- Tests generate throwaway SSH keys/certs at runtime (D-0002); never touch real machines or real creds.
- Keep provisioning steps as small, individually testable functions so the real install can be adjusted
  later without a rewrite.
