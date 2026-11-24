# T-0006 log

## 2026-11-24 doer — registry + SSH onboarding

Built (all inside `touches`):
- `internal/registry/registry.go`: `Machine` (id, role, private IPs, cached public IP + time, `connect_mode`,
  cert fingerprint, SSH host-key fingerprint, disk state, provisioned/removed/revoked-fingerprint, derived
  `Online` that is never persisted, created/updated). `Registry` is a mutex-guarded map: `Add` (defaults
  `connect_mode` to `server_dial`, rejects bad role/mode/id/IP and duplicate ID), `Get`/`List` (copies),
  `Update` (validates, ID immutable, bumps `UpdatedAt`), `Remove`, and `SetOnline` (the hook for the channel;
  does not touch `UpdatedAt`). `Validate([]Machine)` checks a whole list incl. duplicates.
- `internal/registry/store.go`: `NewStore(dataDir)` -> `<DataDir>/registry/machines.json` (JSON, version 1,
  sorted by ID, 0600). Atomic write: temp file in the same dir, fsync, rename, dir fsync; the temp file is
  removed on any failure. `Load` of a missing file gives an empty registry; corrupt/invalid/duplicate files
  are errors.
- `internal/onboard/ssh.go`: `Provisioner.Provision(ctx, Target)` over `golang.org/x/crypto/ssh`, password or
  key. Steps (small functions, `Result.Steps` records the order): ensure-user, create-dirs, install-binary,
  identity (client cert via `pki.IssuePeer`, both EKUs, IP SANs), write-ca, write-config, install-unit,
  enable-start. All remote work is plain commands (`getent`, `useradd`, `install`, `systemctl`) through a small
  `Remote` interface; file contents go over stdin, never on a command line. Non-root users get `sudo -n`.
  Idempotent: identity is looked up by ID or by IP; the existing remote cert is kept when it matches the
  registry fingerprint, is for this ID and has >30 days left, otherwise a new one is issued; `restart` makes a
  re-provision pick up new config/binary. The registry entry is saved before any remote change, so a failed
  run is retried against the same ID. `Remove` = `systemctl stop` + `disable`, then marks the machine removed
  and records `RevokedFingerprint` (the intent to revoke); `SkipRemote` decommissions registry-only. Re-enrolling
  a removed machine reuses the ID but always issues a fresh cert.
- The agent binary comes from `Provisioner.AgentBinaryPath` (fed from the new `server.agent_binary_path`); an
  unset/missing/non-regular path returns `ErrAgentBinaryMissing` with the path, before any registry or network
  action.
- `internal/config/config.go`: added `ConnectMode*` constants, `RegistryDirName`, `ServerConfig.AgentBinaryPath`,
  and optional `AgentConfig` fields (`machine_id`, `connect_mode`, `ca_file`, `cert_file`, `key_file`) so the
  agent's own loader (which rejects unknown fields) accepts the config onboarding writes; `connect_mode` is
  validated when set.
- `go.mod`/`go.sum`: `golang.org/x/crypto v0.41.0` (+ `x/sys v0.35.0`).

Decisions / notes for the supervisor:
- x/crypto v0.41.0 is the newest release whose `go` directive is 1.23.x; v0.42+ require go 1.24 and would force
  bumping the module's go version (a stack call, not mine). `go` went `1.23` -> `1.23.0` as a side effect of
  `go get`. Newer x/crypto has later SSH fixes; a go 1.24 bump + crypto upgrade is worth a follow-up.
- SSH host keys: pin via `Target.HostKeyFingerprint`, else the fingerprint stored for a known machine, else
  trust-on-first-use (recorded in the registry and pinned afterwards).
- The unit is intentionally minimal (User, Restart, WantedBy); hardening and any privileges the executor needs
  are left to tuning. `Target.Host` must be an IP (it becomes a cert SAN).
- Only one agent unit is installed (the brief says "units"); more can be added as another step.

Tests (`scripts/test.sh server`): `internal/registry/registry_test.go` (defaults, validation, duplicates,
update/remove/online, copies, concurrency, round trip incl. online not persisted, corrupt/invalid load, atomic
write with an injected rename failure on both existing and first-ever files, no leftover temp files) and
`internal/onboard/ssh_test.go`. The onboarding tests run the real x/crypto/ssh client against an in-process x/crypto/ssh
server on 127.0.0.1 with runtime-generated ed25519 host/user keys and throwaway credentials; the server records
every exec request and applies it to an in-memory fake machine (no real shell is ever run). Covered: steps and
commands in order, contents of binary/unit/CA/key/cert/config (cert verifies via `pki.DialTLSConfig` with the
machine IP, config loads through `config.Load`), no secrets on command lines, idempotent second run (same ID and
fingerprint, no `useradd`, no key rewrite, one registry entry), cert re-issue only if missing, role/mode change,
missing/empty/directory agent path (no connection, no registry entry), sudo for non-root, key auth, host-key
pinning, failed step then retry reusing the ID, Remove (stop/disable, marked, persisted, re-enroll gets a new
cert), unreachable machine, target validation.

Checks: `gofmt -l .` empty; `go vet ./...` clean; `scripts/test.sh server` passes (also `-race` on registry
and onboard, count=3); `scripts/build.sh` ok. Nothing left running; temp dirs are `t.TempDir()`.

<!-- wf: commits=0f9a35c left=- tests=server:pass blocked=no -->
