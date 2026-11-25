---
id: T-0035
title: server-state-versioning-migrate
state: merged
touches: [internal/config/config.go, internal/migrate/migrate.go, internal/migrate/migrate_test.go,
          internal/registry/registry.go, cmd/fleet-server/main.go]
tests: [server]
after: [T-0006, T-0024]
shares: {}
model: {claude: "sonnet high"}
operator_check: "On a box carrying an older data dir, `fleet-server migrate` (and server startup) upgrades the on-disk schema in place after backing it up, never regenerates the CA, and a data dir NEWER than the binary refuses to start with a clear message."
---

## Why

Upgrading an already-deployed main server needs a safe, explicit path for the on-disk state (config +
registry + data dir), so a new binary never silently misreads or corrupts an old layout. This is the Go
half of the upgrade story; the installer (T-0036) just calls it.

## What to build

1. **Schema version** stamped on persistent state: a `schema_version` (integer) in `config.json` and a
   version marker file in the data dir (e.g. `<FLEET_DATA_DIR>/SCHEMA_VERSION`). Define the current
   version as a constant in the binary.
2. **`internal/migrate`**: an ordered, **idempotent** set of migration steps from version N→N+1 that
   transform the on-disk state (config fields, registry store format, data-dir layout). Running migrate
   when already current is a no-op. **Back up the data dir** (copy to `<dir>.bak-<version>-<ts>`) before
   applying any step.
3. **`fleet-server migrate` subcommand** (`cmd/fleet-server/main.go`): runs the migrations to current and
   reports what it did; safe to run repeatedly.
4. **Startup guard** (config load / server start): if the on-disk schema is **older** than the binary,
   either auto-migrate or refuse with a clear "run `fleet-server migrate`" message (pick one and
   document it); if the on-disk schema is **newer** than the binary, **refuse to start** with a clear
   message (don't downgrade).
5. **CA is sacred:** migrations must **never** touch or regenerate `<FLEET_DATA_DIR>/ca/` (the internal
   CA + private key). Preserving agent trust across upgrades depends on this — assert it in a test.

## Out of scope

- The installer / release pipeline / `--upgrade` orchestration (T-0036 — it calls this).
- Agent-side or channel protocol versioning (separate concern); any real migration content beyond a
  v1 baseline + the framework (seed version 1 = today's layout; add a trivial no-op/established step so
  the machinery is exercised).

## Done means

- A data dir/config stamped at an older version is migrated to current by `fleet-server migrate`
  (tested with a fixture), a backup is created first, and re-running is a no-op.
- A data dir stamped NEWER than the binary makes startup refuse with a clear error.
- A test asserts `ca/` is byte-for-byte untouched across a migration.
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server` passes with `-race` on
  `internal/migrate`, `internal/config`, `internal/registry`.

## Facts this brief relies on

- Data dir is `FLEET_DATA_DIR`, a folder holding `registry`, `ca/` (CA private key), `config/`:
  `workflow/decisions/D-0006-data-directory.md`; `workflow/decisions/D-0008-deployment-layout.md`.
- The registry store + config loader exist: `workflow/tasks/T-0006-machine-registry-onboarding/task.md`;
  `internal/registry/registry.go`; `internal/config/config.go`.

## Notes for the doer

- Keep migrations small, ordered, and pure; one registry/version helper, reused by the subcommand and
  the startup guard. If `cmd/fleet-server/main.go` needs more than a small addition, put the bulk in
  `internal/migrate` and keep `main.go` to dispatch. Do NOT push/merge/branch-delete or edit
  decisions/generated/other tasks/`state:` (except via `wf start`); save out-of-touches needs as a
  `.patch` and report.
