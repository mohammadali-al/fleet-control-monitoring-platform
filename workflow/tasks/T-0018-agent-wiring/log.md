# T-0018 agent-wiring — log

## Doer entry

**What was built.**
- `internal/agent/agent.go`: `agent.New/Run` binds the channel **Listener** (server_dial: the agent
  listens), `FromConfig` loads the pinned CA + client cert + key from the files onboarding wrote
  (no key material in code), `DefaultCgroupRoot` (root on cgroup v2 only).
- `internal/agent/worker.go`: the channel handler. `run`/`input`/`resize`/`terminate`/`confirm` go to the
  `internal/exec` Executor (one-shot + PTY, chunks/state/exit streamed back); a `run` that beats the
  first config waits (`ConfigWait`, default 5 s) and is answered with an error exit if none comes.
  Capture attaches per session through `OnSessionStart` (+ `CgroupPath()`) and stops in `OnSessionEnd`
  (records go to `<DataDir>/evidence/<machine>/`); capture is best-effort and never fails a command.
- `internal/agent/config_apply.go`: the generic applier for `channel.ConfigPayload` (gate secret,
  capture section), idempotent, acks with `config-ack`. The gate secret is fixed inside `exec.Gate`
  (and `gate.go` is outside touches), so the executor is built when the first config arrives and a
  *changed* secret starts a new executor for new runs (the old one stays reachable for its sessions).
  The live capture factory checks capability up front (root + `nft`) and otherwise reports capture
  as off, once, without starting the public-IP refresh (so unprivileged agents/tests make no
  network calls).
- `internal/channel/envelope.go`: `TypeConfigReq`, `TypeConfig`, `TypeConfigAck`, `ConfigPayload`
  (`gate_secret`, `capture`), `ConfigAckPayload`. `ConfigPayload` redacts the secret in `String()`
  and as a `slog.LogValuer`.
- `internal/fleet/fleet.go`: `Config.GateSecret` + `Config.Capture`; `agentHandler` answers an agent's
  `config-req` with the config message (worker machines only, nothing sent when no secret) and logs
  `config-ack`.
- `internal/config/config.go`: `AgentConfig.ListenAddr/DataDir/CgroupRoot` (+ `ListenAddrOrDefault`,
  `ResolveDataDir`), `ServerConfig.AgentCapture`, `CaptureConfig.IsZero`.
- `cmd/fleet-agent/main.go`: `run(ctx, args, stdout, stderr)`; `--role worker` loads config, inits
  logging, resolves the data dir, runs the agent, shuts down on SIGINT/SIGTERM (exit 0).
  `--role monitor` prints "the monitor role is not implemented yet" and exits 1 (TODO for after
  T-0010); `agent_dial` is rejected with a clear error (server_dial only).
- New test support `internal/fleet/fleettest/mtls.go`: `MTLS` runs real agents on 127.0.0.1 with a
  throwaway CA and gives the Manager the production dialer (`fleet.NewDialerFactory`), plus
  `DialerConfig` for protocol-level tests. `Loopback` is untouched.

**Config-push + gate secret flow.** The server owns the secret (`<data>/config/gate.key`). When an
agent's connection is established the agent sends `config-req`; the Manager answers with a `config`
envelope `{gate_secret, capture?}` over the mTLS channel; the agent builds `exec.NewGate` from it,
installs the executor, applies capture, and replies `config-ack {ok}`. It happens on every connection
(idempotent), so a restarted agent is configured again; the secret lives in agent memory only and is
never logged or written to disk. A request-driven push (rather than a server-side "on connect"
hook) needs no change to the dialer link and cannot be missed by a reconnect faster than the 1 s state
poll. Tokens minted by `Manager.Submit(Confirm)` / `ShellConfirm` are verified by the agent with the
pushed secret.

**Needs the supervisor: `internal/fleet/open.go` is outside `touches`.** The production `Open()` must
pass the secret and capture config to the Manager; that two-line change (plus a stale comment) is in
`workflow/tasks/T-0018-agent-wiring/open-go.patch` (`git apply` it; it applies cleanly and builds).
Until it is applied, a production server does not push (no secret in `fleet.Config`) and agents stay
unconfigured; everything is tested without it via `fleettest.MTLS`.

**Self-check.** `gofmt -l .` empty; `go vet ./...` clean; `scripts/build.sh` ok;
`scripts/test.sh server` and `scripts/test.sh agent` pass; `go test -race -count=5` on
`internal/agent` and `cmd/fleet-agent`, `-race` on `internal/fleet` pass.
- agent connects over real mTLS and is reported online; one-shot streams queued,running,exited with
  stdout+stderr; capture config pushed reaches the capture factory: `TestEndToEndOneShotAndGate`.
- dangerous command: refused by the server unconfirmed; with `Confirm` the server mints a token and
  the agent runs it using the pushed secret; a token from another secret is refused by the agent:
  `TestEndToEndOneShotAndGate`, `TestAgentRefusesTokenFromAnotherSecret`.
- protocol level over a raw main-server connection: config-req on connect, push, ack, dangerous
  command refused without a token, runs with one, wrong secret / wrong machine refused, run-before-
  config waits or errors, idempotent push, secret rotation, no-secret config rejected, secret not
  leaked by formatting/logging: `TestRawConnectionConfigPushAndGate`, `TestRunBeforeConfig`,
  `TestConfigPushIsIdempotentAndRotates`, `TestConfigPayloadDoesNotLeakSecret`.
- capture attach/stop per session with the real executor and a real (delegated) session cgroup, fake
  capturer: `TestCaptureAttachedPerSession` (ran here, unprivileged).
- **Root-gated, SKIPPED here (uid 1000):** `TestLiveCaptureProducesRecords` (real nftables/NFLOG:
  a UDP datagram over loopback from the session must produce records in the manifest). Run it with
  `sudo go test ./internal/agent/ -run TestLiveCaptureProducesRecords -v` on a real box; it was
  written but has not been executed.
- `cmd/fleet-agent`: `--role worker` starts and shuts down cleanly (`TestWorkerStartsAndShutsDownCleanly`);
  `--role monitor` reports not implemented (exit 1, no crash); no role -> exit 2; unonboarded worker ->
  clear `machine_id` error.

**Notes / deviations.**
- Agent data dir default: `data_dir`, else `FLEET_DATA_DIR`, else the working directory (onboarding's
  unit sets `WorkingDirectory` to `/var/lib/fleet-agent`); D-0006's "one level above the binary"
  would be `/usr/local/data` for the installed binary, which the service user cannot write.
- The onboarded systemd unit runs as a dedicated non-root `fleet-agent` user, so live capture (needs
  root/CAP_NET_ADMIN) and cgroup tracking will report "off" on a real worker until the unit/layout
  grants that (onboarding task). Commands, streaming and the gate work unprivileged.
- Capture attaches synchronously inside `OnSessionStart` (the executor calls it right after the
  command starts, before output pumping), so the first few packets of a command may predate the rule.

<!-- wf: commits=28e21e1 left=- tests=server:pass,agent:pass blocked=no -->
