# T-0022 log

## Doer entry (claude, sonnet high)

**Built.**
- `cmd/fleet-agent/main.go`: new `watchdog` subcommand (`fleet-agent watchdog --config ...`). It loads the
  config, resolves `DataDir`, and runs `agent.RunWatchdog` until SIGTERM/cancel (exit 0, logs "shut down").
  It never opens the channel. The worker role now builds its agent with `agent.WorkerFromConfig`.
- `internal/agent/watchdog_wire.go` (new):
  - Watchdog process side: `NewWatchdog` / `RunWatchdog` build `watchdog.New` over `DataDir` with the
    system sampler + `SystemActuator` (session root = `cgroup_root`, egress iface and groups from the
    `watchdog` config section). Its `Notify` is an `EventWriter` that appends each event as a JSON line
    to `<DataDir>/watchdog/events.jsonl` (rotated to `.1` beyond 1 MiB).
  - Main-agent side: a `policy` envelope is parsed, checked to resolve for this host
    (`Profile.Effective(machine, groups)`), stored with `watchdog.StorePolicy(dataDir, raw, -1)` and
    answered with `watchdog.PolicyAck` (`policy-ack`, ack id = envelope id, version = stored last-known).
    An invalid, host-unresolvable or stale profile is refused with a reason and nothing on disk changes;
    the running loop notices the file and applies it live (T-0012 `reloadPolicy`).
  - Event forward: `eventForwarder` tails `events.jsonl`, sends each complete line as `watchdog-ev`
    (`watchdog.EventEnvelope` shape), persists the forwarded offset in `events.offset` (restart neither
    repeats nor loses events), stops at the first send error and keeps the backlog until the server is
    back, follows a writer-side rotation, skips unreadable lines and waits for an unfinished line.
    A connect to the server triggers an immediate pass; otherwise it polls every 2 s.
  - `NewWithWatchdog` / `WatchdogAgent`: the worker `Agent` plus that wiring (see Notes).
- `internal/fleet/fleet.go` + new `internal/fleet/watchdog.go`: `Config.Watchdog` / `Manager.SetWatchdog`
  take a `WatchdogSource{PolicyFor, OnEvent}`. On a worker's `config-req` the manager pushes
  `policy` (after config; independent of the gate secret). `PushPolicies` (all connected workers) and
  `PushPolicy(machine)` re-push after an edit. `policy-ack` is recorded (`WatchdogStatus`: pushed
  version, ok/error, version in force); `watchdog-ev` is stored per machine under the link's verified
  id (last 200, deduplicated because forwarding is at-least-once, `WatchdogEvents`) and handed to
  `OnEvent`. Monitor agents are not sent policy and their policy-ack/watchdog-ev are ignored.
- `internal/fleet/fleettest/mtls_watchdog.go` (new): `MTLS.StartWatchdogAgent`, the production worker
  over real mTLS, like `StartAgent`.

**Tests.**
- `internal/agent/watchdog_wire_test.go` (real mTLS agent + real `fleet.Manager` + the watchdog loop on
  the same data dir): policy pushed on connect is stored + acked and the loop applies it live, an edit
  re-pushed and applied; invalid (garbage, bad value, no version, out-of-range, stale) policies are
  refused with the previous version still in force, stored file byte-identical, loop keeps last-known;
  a profile that parses but conflicts only for this host's group+host overrides is refused before
  storing; events raised while the server is away are forwarded on connect, later events follow in
  order, cursor catches up; ack echoes the envelope id; no policy source pushes nothing.
- `internal/agent/watchdog_forward_internal_test.go`: forwarder offline backlog, resume after restart,
  stop-at-first-failure, newline wait + garbage skip, rotation.
- `internal/fleet/watchdog_test.go`: push to workers only on connect/edit (bound at New and by
  SetWatchdog), ack and refusal recorded, events stored under verified id + dedup + bounded, harmless
  without a source.
- `cmd/fleet-agent/watchdog_test.go`: `watchdog --config` starts under the stored policy (version 7),
  opens no listener, exits 0 on cancel; bad flag -> 2, bad config -> 1.
- `gofmt -l .` empty; `go vet ./...` clean; `scripts/test.sh server` and `scripts/test.sh agent` pass;
  `go test -race -count=1` on `internal/agent`, `internal/fleet/...`, `cmd/fleet-agent`,
  `internal/watchdog` passes.

**Notes / deviations.**
- `internal/agent/agent.go` and `worker.go` are not in `touches`, and `agent.New` hard-wires
  `worker.handle` as the listener's handler with no hook for the `policy` type. So the wiring is a
  parallel constructor, `NewWithWatchdog` (same validation/defaults as `New`, handler wrapped to
  intercept `policy`, `OnConnect` also kicks the event forwarder); `main.go` uses it via
  `WorkerFromConfig`. Plain `agent.New`/`FromConfig` are unchanged and do not handle `policy`.
  Cleaner follow-up (outside touches): fold the wiring into `New` via an optional `Config.Watchdog`
  field and delete `NewWithWatchdog`/`WorkerFromConfig`/`StartWatchdogAgent`.
- No server-side policy store exists yet, so a stock server pushes nothing until something sets
  `Config.Watchdog.PolicyFor` / `SetWatchdog` (T-0023 Watchdog page owns the profile editing and the
  call to `PushPolicies`); `fleet.Open` and `cmd/fleet-server` are outside touches and unchanged.
- `internal/config/config.go` needed no change (`watchdog.groups` / `egress_interface` from T-0012 are
  used as is).
- `deploy/systemd/fleet-watchdog.service` still carries a comment saying the subcommand is "wired by a
  later task"; it is now wired. The file is outside touches; the comment can be dropped later.
- Events recorded while no main agent runs wait in `events.jsonl` (bounded by rotation: at most about
  2 MiB are kept); the tail of a file rotated and fully replaced across an agent restart can be missed.
- The `policy` envelope is handled inline on the read loop (a small file write), like config-push.

<!-- wf: commits=ef4c738 left=- tests=server:pass,agent:pass blocked=no -->
