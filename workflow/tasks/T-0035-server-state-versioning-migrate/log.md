# T-0035 server-state-versioning-migrate: log

## Entry 1 (doer)

- `internal/config/config.go`: added `CurrentSchemaVersion` (`2`), `SchemaVersionFile` (`SCHEMA_VERSION`), `ErrNewerSchema`, `Config.SchemaVersion` (`schema_version,omitempty`), `ReadSchemaVersion`, and version-check on `Load` so configs with `schema_version > CurrentSchemaVersion` (even with unknown future fields) fail loudly.
- `internal/registry/registry.go`: added `CurrentFormatVersion` (`1`) and `InspectFileVersion` to inspect `<dataDir>/registry/machines.json` without modifying `store.go`.
- `internal/migrate/migrate.go`: added ordered, idempotent `N -> N+1` migration engine (`SeedVersion = 1` for today's layout, trivial `1 -> 2` step establishing `config/` and `registry/` directories and verifying registry loadability). Backs up `<dataDir>` to `<dir>.bak-<version>-<ts>` before applying steps, stamps `<dataDir>/SCHEMA_VERSION` and `config.json` `schema_version` atomically, is a no-op when already current, never touches or regenerates `<dataDir>/ca/`, and refuses newer `SCHEMA_VERSION`, `config.json`, or `machines.json` versions with `ErrNewerSchema`.
- Startup guard choice: **auto-migrate on startup** (`migrate.EnsureCurrent` in `cmd/fleet-server/main.go`) — initializes brand-new empty data dirs at `CurrentSchemaVersion` without a backup, auto-migrates older populated/v1-stamped state in place after backing up, and refuses to start if on-disk state is newer than the binary (or if `config.json` update fails on permissions, directing the operator to run `fleet-server migrate`).
- `cmd/fleet-server/main.go`: added `fleet-server migrate [--config PATH] [--data-dir PATH]` subcommand and wired `migrate.EnsureCurrent` before `fleet.Open` on server startup.
- Verification (`go version go1.26.8 linux/amd64`): `gofmt -l .` empty, `go vet ./...` clean, `go test -race ./internal/migrate ./internal/config ./internal/registry ./cmd/fleet-server` passed (covering v1-stamped & unversioned migration with backup, idempotent re-run, byte-for-byte untouched `ca/`, newer-version refusal across `SCHEMA_VERSION`/`config.json`/`machines.json`/CLI, and ordered multi-step recovery), and `./scripts/test.sh server` passed.

wf: T-0035 done
