# T-0057 onboard-agent-dial-config: log

## 2026-11-28: SSH onboarding server_addr + server_id for agent_dial (GAP-03)

**Built (inside `touches`).**

- **`internal/onboard/ssh.go`:**
  - Added `ServerAddr` and `ServerID` fields to `Provisioner` alongside `AgentBinaryDir` and `AgentBinaryPath`.
  - Added `ErrAgentDialNotConfigured` and `Provisioner.preflightAgentDial(Target)` (using shared `findMachine(Target)` helper) so onboarding an `agent_dial` machine (or re-provisioning an existing `agent_dial` machine with empty `Target.ConnectMode`) when `Provisioner.ServerAddr` is empty fails before dialing SSH, creating a registry entry, or writing remote files.
  - Updated `stepWriteConfig` and `renderAgentConfig(m, l, serverAddr, serverID)`: when `m.ConnectMode == config.ConnectModeAgentDial`, requires non-empty `serverAddr` (returning `ErrAgentDialNotConfigured` if empty) and populates `AgentConfig.ServerAddr` and `AgentConfig.ServerID` (defaulting empty `serverID` to `config.DefaultServerID`). For `server_dial` (and empty `ConnectMode`), leaves `ServerAddr` and `ServerID` empty so the rendered JSON is byte-identical to before.
- **`internal/onboard/ssh_test.go`:**
  - Added `TestRenderAgentConfigAgentDialVsServerDial` verifying `renderAgentConfig` emits `server_addr` and `server_id` for `agent_dial`, defaults empty `server_id` to `config.DefaultServerID`, returns `ErrAgentDialNotConfigured` when `server_addr` is empty, and omits `server_addr`/`server_id` with byte-identical output for `server_dial` (and empty `connect_mode`).
  - Added `TestProvisionAgentDialRequiresEndpointAndConnects` verifying preflight failure before SSH/registry changes when `ServerAddr` is unset (both initial provision and reprovision keeping existing `agent_dial` mode), and end-to-end connection of an SSH-onboarded `agent_dial` config via `agent.DialFromConfig` against a `channel.Accept` listener.
  - Updated `TestReprovisionUpdatesRoleAndMode` to set `ServerAddr`/`ServerID` on the test `Provisioner` and assert both are written to `/etc/fleet-agent/config.json`.

**Outside `touches` (`.patch` saved under `workflow/tasks/T-0057-onboard-agent-dial-config/`).**

- **`fleet-open-agent-dial-config.patch`:** In this codebase `onboard.Provisioner` is constructed from `config.ServerConfig` inside `fleet.Open` (`internal/fleet/open.go`, called by `cmd/fleet-server/main.go`), not directly in `cmd/fleet-server`. `fleet.Manager` does not export `Provisioner`, so wiring `ServerAddr` (`<EnrollHost>:<port parsed from AgentDialAddr>`, or empty when `AgentDialAddr` or `EnrollHost` is unset) and `ServerID` (`ServerConfig.ServerIDOrDefault()`) requires editing `internal/fleet/open.go`. The minimal patch is saved here and `git apply --check` passes cleanly; `cmd/fleet-server/main.go` and `cmd/fleet-server/enroll.go` required no changes.

**Verification.**

- *With `fleet-open-agent-dial-config.patch` temporarily applied:* `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok; `internal/onboard`, `cmd/fleet-server`, and standalone `go test -count=1 ./internal/fleet/...` passed (an initial full `./scripts/test.sh server` run hit an unrelated timing flake in `internal/fleet` `TestDiskStateDispatch` which passed standalone).
- *On the clean branch (only `touches` + task folder; `internal/fleet/open.go` reverted; `git apply --check` clean on the patch):* `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok, `./scripts/test.sh server` passed.

wf: T-0057 done
