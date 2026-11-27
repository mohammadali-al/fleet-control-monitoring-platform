---
id: T-0057
title: onboard-agent-dial-config
state: ready
touches: [internal/onboard/ssh.go, cmd/fleet-server/main.go, cmd/fleet-server/enroll.go, internal/onboard/ssh_test.go]
tests: [server]
after: [T-0043]
shares: {}
model: {claude: "sonnet high"}
operator_check: "When a machine is onboarded over SSH in agent_dial mode, its rendered agent config now includes server_addr (the main server's reachable host:port for the agent_dial listener) and server_id, so the agent can actually dial home and verify the server. server_dial onboarding is unchanged (those fields stay empty — the server dials the agent). A machine onboarded as agent_dial connects successfully end to end."
---

## Why

SSH onboarding renders the agent config without `server_addr`/`server_id` (`internal/onboard/ssh.go`
`renderAgentConfig` sets only Role/MachineID/ConnectMode/CA/Cert/Key). But an `agent_dial` agent needs the
server's address to dial and its identity to verify it — `internal/agent/agentdial.go` `validate()` errors
`"agent_dial needs the main server's address (server_addr)"` when it's empty. So today an SSH-onboarded
`agent_dial` machine **cannot connect** (audit GAP-03, remainder). T-0043 already put `agent_dial_addr` +
`enroll_host` in the **server** config; this task writes the matching values into the **agent** config at
onboarding. (Default `server_dial` mode doesn't need them — the server dials the agent.)

## What to build

1. **Thread the server's agent_dial endpoint + id into onboarding.** Add fields to the `Provisioner`
   (`internal/onboard/ssh.go:162`) — e.g. `ServerAddr string` (the host:port agents dial for the
   `agent_dial` listener) and `ServerID string` — alongside the existing config-derived fields
   (`AgentBinaryDir`/`AgentBinaryPath`). Populate them where the `Provisioner` is constructed from
   `ServerConfig` (in `cmd/fleet-server`): `ServerAddr` = `<EnrollHost>:<port of AgentDialAddr>`, `ServerID`
   = `ServerConfig.ServerIDOrDefault()`. If `agent_dial` isn't configured on the server (no `agent_dial_addr`),
   leave `ServerAddr` empty.
2. **Populate the agent config for agent_dial only.** In `renderAgentConfig` (`internal/onboard/ssh.go:694`),
   when `m.ConnectMode == config.ConnectModeAgentDial`, set `ServerAddr` + `ServerID` on the rendered
   `config.AgentConfig` (fields already exist: `server_addr`, `server_id`). For `server_dial`, leave them
   empty (unchanged behaviour). If a machine is `agent_dial` but the server has no agent_dial endpoint
   configured, fail onboarding with a clear error rather than writing an unusable config.

## Out of scope

- No change to the `server_dial` path, the control-channel protocol, enrollment, or the agent runtime; no
  panel/front-end change. Don't change how the server's own `agent_dial_addr`/`enroll_host` are set (that's
  T-0043). Don't add new config fields to `ServerConfig` — reuse `EnrollHost`, `AgentDialAddr`, `ServerID`.

## Done means

- An `agent_dial` SSH onboarding writes `server_addr` (`<EnrollHost>:<agent_dial port>`) + `server_id` into
  the agent config; a `server_dial` onboarding leaves them empty (unchanged). An `agent_dial` machine
  onboarded this way connects end to end; `agent_dial` onboarding with no server endpoint configured fails
  with a clear error.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok, `scripts/test.sh server` passes with a
  test asserting `renderAgentConfig` emits `server_addr`/`server_id` for agent_dial and omits them for
  server_dial.

## Facts this brief relies on

- `renderAgentConfig` omits the fields: `internal/onboard/ssh.go:694-704`; `Provisioner`:
  `internal/onboard/ssh.go:162`. Agent needs them: `internal/agent/agentdial.go:40-44`. Fields exist on
  `AgentConfig`: `internal/config/config.go` (`server_addr` ~L15, `server_id` ~L18, `ConnectModeAgentDial`
  L29). Server has the source values after T-0043: `ServerConfig.EnrollHost`, `AgentDialAddr`,
  `ServerIDOrDefault()` (`internal/config/config.go:124-166`). Where the Provisioner is built from
  ServerConfig: `cmd/fleet-server` (main.go / enroll.go).

## Notes for the doer

- Derive the agent_dial port from `AgentDialAddr` (host:port); combine with `EnrollHost` for `server_addr`.
  Keep `server_dial` output byte-identical to today. If wiring the Provisioner's new fields requires a touch
  just outside these files, make the minimal change and note it (or save a `.patch`). Run `./scripts/wf
  start T-0057` before committing; commit inside touches + the log. Do NOT push/merge/branch-delete or edit
  decisions/generated/other tasks/`state:` (except via `wf start`).
