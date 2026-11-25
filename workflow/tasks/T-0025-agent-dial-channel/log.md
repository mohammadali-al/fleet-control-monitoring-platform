# T-0025 agent-dial-channel: log

## 2026-11-25: agent_dial direction built (Go 1.26.8), one commit + one `.patch` for the agent binary

**Built.** The agent dials OUT to a server-side mTLS WS listener; `server_dial` code is untouched.

- **`internal/channel/agentdial.go` (new).** `AgentDialer` (agent side: same Envelope, same hello/hello-ack
  handshake with the roles reversed -- agent sends hello, server answers hello-ack, each id checked against
  the peer's verified certificate CN; capped exponential backoff + jitter, handshake resets it; ping
  keepalive) and `Acceptor` (server side: `ServeTLS` with `RequireAndVerifyClientCert`, identity from the
  verified chain only, optional `Admit` before the upgrade (HTTP 403), `Attach` after the handshake). An
  accepted connection is a `*Conn` with `Send`. `AgentDialTLSConfig` pins the CA and matches the server by
  its id (the cert's CN/DNS SAN), not by IP, so the server can sit behind any name/NAT.
- **`internal/fleet/agentdial.go` (new) + `fleet.go`.** `AgentListener` implements the Link for each
  agent_dial machine (`inboundLink`: `Send` = the attached conn, `Online` = a conn is attached, `Run` just
  waits) so Manager code (config-push, queue, monitor/evidence/watchdog handling, state poll) is the same
  for both directions. `Config.AgentDial` makes `wanted()` link agent_dial machines (key `"agent_dial"`
  instead of an IP; a mode/IP change restarts the node) and `Start` serves the listener. A newer
  connection from the same agent replaces an older one. Machines the manager has no link for get 403.
  With no listener configured the old behavior is kept (agent_dial machines offline, with a note).
- **`internal/config`.** `server.agent_dial_addr` (listener, **off unless set**), `agent.server_addr`,
  `agent.server_id`, `DefaultAgentDialPort` (7444, the value to put in `agent_dial_addr`), `Dials()`.
- **`cmd/fleet-server/main.go`.** Small: `mgr.EnableAgentDial(addr, serverID)` when the address is set.
  `Open()` (open.go) is outside touches, so `EnableAgentDial` takes the CA from `Config.Provisioner.CA`.
- **`internal/agent/agentdial.go` (new).** `DialingWorker` (the same `worker` + watchdog wiring as
  `WatchdogAgent`) and `DialingMonitor` (same engine/flusher), `DialFromConfig`, `LoadDialTLS`.
- **`internal/fleet/fleettest/agentdial.go` (new).** `ListenAgents`, `DialTLS`, `StartDialingAgent`,
  `StartDialingMonitor`.

**Done means.**
- agent_dial online + one-shot + dangerous-confirm (agent verifies with the PUSHED secret) + capture
  config-push + monitor config/data/config-update: `TestWorkerRoundTripBothDirections`,
  `TestMonitorRoundTripBothDirections` run the identical assertions for `server_dial` and `agent_dial`: pass.
- Dialer reconnects by itself after the server restarts: `TestAgentDialReconnectsAfterServerRestart`
  (manager + listener on the same address) and `TestAgentDialerReconnectsAfterServerRestart` (channel): pass.
- Different-CA agent rejected: `TestAgentDialRejectsForeignCAAndUnknownMachine` (also an unregistered id),
  `TestAgentDialForeignCAAgentRejected`, `TestAgentDialRejectsForeignServer` (reverse direction): pass.
- `server_dial` unchanged: all existing tests pass untouched (`TestMachineStates` still sees the agent_dial
  note when no listener is configured); `TestMixedDirections` runs both in one manager.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/build.sh` ok, `scripts/test.sh server` and `agent`
  pass; `go test -race -count=1 ./internal/channel ./internal/fleet/... ./internal/config` pass (the new
  fleet tests also ran `-race -count=3`).

**Outside `touches` (needs the supervisor).**
`workflow/tasks/T-0025-agent-dial-channel/cmd-fleet-agent-agent-dial.patch`: `cmd/fleet-agent/main.go` must
dispatch `ac.Dials()` to `agent.DialFromConfig` (a ~10-line `switch` change), plus a new
`cmd/fleet-agent/agentdial_test.go` (worker and monitor from a config file dial a real `channel.Acceptor`
and shut down cleanly; agent_dial without `server_addr` is an error). Checked: `git apply --check` is
clean and, with it applied, `go test -race ./cmd/fleet-agent` passes. Until it is applied the shipped
`fleet-agent` binary still refuses `connect_mode: agent_dial` (the `*FromConfig` guards are unchanged).

**Decisions / notes.**
- The listener is opt-in (`agent_dial_addr` empty = off) so `server_dial`-only installs open no extra port.
- Under agent_dial the server cert is the same server-id cert (`IssuePeer(serverID, nil, ...)`); agents
  verify it by server id (DNS SAN), so no IP SAN or fixed server address is needed.
- Monitor logic is duplicated (`DialingMonitor.handle/onConnect` mirror `Monitor`) because `monitor.go` is
  bound to `*channel.Listener` and outside touches; a follow-up could put both behind one small peer
  interface (`Send` + `Peer`) and delete the copy.
- Onboarding still writes no `server_addr`; that is T-0026 territory (token enrollment), as out of scope.
- No heartbeat beyond the existing ping/pong; liveness semantics are T-0027's.

<!-- wf: commits=c877fdf left=- tests=server:pass,agent:pass blocked=no -->
