---
id: T-0001
title: project-skeleton
state: ready
touches: [go.mod, go.sum, Makefile, VERSION, .gitignore,
          cmd/fleet-server/main.go, cmd/fleet-agent/main.go,
          internal/version/version.go, internal/version/version_test.go,
          internal/config/config.go, internal/config/config_test.go,
          internal/logging/logging.go,
          scripts/build.sh, scripts/test.sh]
tests: [server, agent]
after: []
shares: {}
model: {claude: "sonnet high"}
operator_check: "Backend only, no UI. Verify: `make build` yields bin/fleet-server and bin/fleet-agent; `./bin/fleet-agent --version` prints a version; `./bin/fleet-agent --role bogus` exits non-zero."
---

## Why

This is the foundation every later task stacks on: the Go module, the two binaries, and the build +
test harness. Its layout and conventions (config, logging, graceful shutdown, test groups) set the
shape of the whole codebase, so getting the skeleton clean matters more than it being large. It is
**scaffolding only** — no control channel, no capture, no probes, no panel.

## What to build

**Backend (Go).**

1. **Module & layout.** `go.mod` with module path from D-0001, Go 1.23+. Packages:
   - `cmd/fleet-server/main.go` — the main-server binary entrypoint.
   - `cmd/fleet-agent/main.go` — the agent binary entrypoint.
   - `internal/version` — `Version`, `Commit`, `BuildTime` package vars (set via `-ldflags` at build),
     and a `String()` that formats them.
   - `internal/config` — a `Load(path string)` that reads a config file (TOML or YAML — doer's choice,
     record it in the file) into typed structs. A `ServerConfig` and an `AgentConfig`. `AgentConfig`
     carries `Role` (`worker` | `monitor`) and a `Validate()` that rejects anything else. Missing file
     → sane defaults (do not crash).
   - `internal/logging` — `log/slog` setup: level and format (text|json) from config/env.

2. **`fleet-server` behavior.** Flags: `--config <path>`, `--version`. On `--version`, print
   `internal/version.String()` and exit 0. Otherwise: load config, init logging, log a structured
   "starting" line, block until `SIGINT`/`SIGTERM`, log "shutting down", exit 0.

3. **`fleet-agent` behavior.** Same as server, plus: `--role worker|monitor` flag that overrides the
   config role. The effective role must validate (`AgentConfig.Validate()`); an invalid or missing
   role prints a clear error and exits non-zero. Log the resolved role at startup.

4. **Build harness.**
   - `scripts/build.sh` — builds both binaries into `bin/` with `-trimpath` and version `-ldflags`;
     reproducible (`project.yml:build.must_be_reproducible`). Takes an optional single-binary arg.
   - `scripts/test.sh` — `go test ./...` by default; with a group arg (`server`|`agent`|`e2e`) runs
     only that group's packages. Define the mapping at the top of the script (e.g. `server` →
     `./cmd/fleet-server/... ./internal/...` as appropriate; `e2e` may be empty for now and must exit 0).
   - `Makefile` — `build`, `test`, `run-server`, `run-agent`, `fmt`, `vet` targets.
   - `VERSION` file containing `0.0.0`.
   - `.gitignore` — add `/bin/`, `*.out`, coverage files.

5. **Minimal tests** (so the harness is real): `internal/version` (`String()` is non-empty and
   includes the version), `internal/config` (defaults load; a sample config round-trips;
   `AgentConfig.Validate()` accepts `worker`/`monitor` and rejects `bogus`).

## Out of scope

- Any networking, WebSocket, mTLS, or CA code (later tasks).
- Capture, probes, watchdog, Load Controller, panel, storage — none of it.
- systemd unit files, provisioning, Docker, CI config.
- A web/front-end anything.

## Done means

- `make build` produces `bin/fleet-server` and `bin/fleet-agent`.
- `./bin/fleet-server --version` and `./bin/fleet-agent --version` each print a version and exit 0.
- `./bin/fleet-server` and `./bin/fleet-agent --role worker` start, log a structured startup line,
  and exit 0 on `SIGINT`.
- `./bin/fleet-agent --role bogus` (and an agent with no role) exits non-zero with a clear message.
- `scripts/test.sh` passes; `scripts/test.sh server` and `scripts/test.sh agent` each pass.
- `scripts/build.sh` run twice yields byte-identical binaries.
- `gofmt -l .` is empty and `go vet ./...` is clean.

## Facts this brief relies on

- The agent is **one Go binary with two role profiles (worker / monitor)**:
  `Fleet_Control_Monitoring_Platform_Architecture.md:34`, `:49`, `:228`.
- The agent software is named **`fleet-agent`** and is a role-profiled daemon:
  `Fleet_Control_Monitoring_Platform_Architecture.md:137`.
- Connectivity / role / policy are **config-driven** (per-machine config flag; live-reloaded policy):
  `Fleet_Control_Monitoring_Platform_Architecture.md:152`, `:241`.
- Runtime is **systemd services** and the agent **requires root** (not exercised here, but the binary
  must not assume a TTY or a non-root dev shell): `Fleet_Control_Monitoring_Platform_Architecture.md:243`.
- Stack baseline (Go, module path, `cmd/` layout): `workflow/decisions/D-0001-stack.md`.
- The main-server binary name `fleet-server` is this repo's convention from D-0001 (the architecture
  calls it "Fleet Server (Go)", `:135`), not a verbatim string in the architecture.

## Notes for the doer

- Keep `main.go` thin: parse flags, call into `internal/*`, wire signals. Put logic in `internal/`.
- Config format (TOML vs YAML) is your call — pick one, add a tiny sample under the config package's
  test data, and note the choice in a comment. No new config library unless stdlib/`encoding` can't do it.
- `e2e` group is intentionally empty now; `scripts/test.sh e2e` must still exit 0 so the harness is
  shaped for later.
- No business logic — resist adding channel/capture stubs. Later tasks own those and declare them.
