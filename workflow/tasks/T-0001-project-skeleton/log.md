# T-0001 log (append-only)

## Entry 1 - doer (claude sonnet)

Built the project skeleton on branch `task/T-0001`: `go.mod` (module
`github.com/mohammadali-al/fleet-control-monitoring-platform`, Go 1.23), `cmd/fleet-server` and
`cmd/fleet-agent` (thin mains: flags, signals, call into `internal/`), `internal/version`
(ldflags-set vars + `String()`), `internal/config` (`Load`, `Defaults`, `ServerConfig`, `AgentConfig`
with `Validate()`), `internal/logging` (slog; level/format from config, overridden by
`FLEET_LOG_LEVEL` / `FLEET_LOG_FORMAT`), `scripts/build.sh` (trimpath, CGO off, `-buildid=`, build time
taken from the HEAD commit time or `SOURCE_DATE_EPOCH`, so two builds are byte-identical),
`scripts/test.sh` (groups `server`, `agent`, `e2e`; `e2e` is empty and exits 0), `Makefile`, `VERSION`
(`0.0.0`), `.gitignore` additions, and minimal tests for version and config.

Decisions: config format is JSON via stdlib `encoding/json` (neither TOML nor YAML is in the stdlib and
the brief says no new config library); unknown fields are rejected. Agent role has no default so a
missing role fails validation; invalid/missing role exits 2. A missing config file (or empty `--config`)
yields defaults. `go.sum` was not created because there are no dependencies (it is in `touches` but
unneeded).

Checks: gofmt clean, go vet clean, build twice gives identical sha256, `--version` exits 0 for both,
`--role worker` and server log a startup line and exit 0 on SIGINT, `--role bogus`/no role exit 2,
`test.sh` (all, server, agent, e2e) pass.

<!-- wf: commits=cf42b56 left=- tests=server:pass,agent:pass blocked=no -->
