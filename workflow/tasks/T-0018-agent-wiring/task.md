---
id: T-0018
title: agent-wiring
state: ready
touches: [cmd/fleet-agent/main.go, internal/agent/agent.go, internal/agent/worker.go,
          internal/agent/config_apply.go, internal/agent/agent_test.go,
          internal/channel/envelope.go, internal/fleet/fleet.go, internal/config/config.go]
tests: [server, agent]
after: [T-0017]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Onboard a real worker from the panel: it shows ONLINE; a one-shot command runs on it with live output; evidence capture produces records for the session; a dangerous command requires the gate. (Supervisor can validate end-to-end on a real machine.)"
---

## Why

The big unlock: make `cmd/fleet-agent` actually **run** in the worker profile so a provisioned machine
connects back and becomes usable. Today the agent is a skeleton — the server side is live (T-0011) but
nothing on a worker connects, so machines stay offline. This wires the agent runtime from the pieces
already built (channel, executor, capture) and delivers the gate secret by **config-push over the
channel** (operator decision).

## What to build

**Backend (Go).** Worker role only (monitor role is deferred until monitors, T-0010).

1. **Agent runtime** (`internal/agent/`): on start, the worker agent runs the **channel Listener**
   (default `server_dial`: the agent listens, the main server dials it) using its provisioned client
   cert + pinned CA from its config/data dir. On an accepted connection it serves the control protocol.
2. **Executor wiring** (`worker.go`): handle `run`/`chunk`/`exit`/`resize`/`terminate`/`confirm` (and
   `input`/`state`) envelopes by driving `internal/exec` — one-shot + PTY, the serial queue for
   one-shots, streaming output back. (Shells concurrent with the queue once T-0016 lands; don't conflict
   with it — keep the executor's queue usage as-is.)
3. **Capture attach**: for each session, attach `internal/capture` via the executor's `OnSessionStart`
   hook + `CgroupPath()` (capture is fixed by T-0017). Records go to local segments under `DataDir`.
4. **Config-push + gate secret** (`config_apply.go` + server side in `internal/fleet`): after a machine
   connects, the **main server pushes a config message over the channel** carrying the **gate secret**
   (and the per-node capture config). Add the message type to `internal/channel/envelope.go`. The agent
   **applies** it and uses the secret in the dangerous-command gate. This establishes the config-push
   pattern that watchdog policy (§11.6) and monitor config (§8.1) will reuse later.
5. **`cmd/fleet-agent/main.go`**: start the worker runtime for `role=worker` (load config, init logging,
   run the agent, graceful shutdown). `role=monitor` stays a clear TODO/stub (wired after T-0010).

## Out of scope

- Monitor-role runtime / probes (after T-0010) and watchdog runtime (T-0012).
- `agent_dial` (the main server has no agent-initiated listener yet) — `server_dial` only.
- A general config-push for watchdog/monitor policy — just the gate secret + capture config now; keep the
  message/ applier generic enough to extend.

## Done means

- In-process end-to-end via `internal/fleet/fleettest` (server + agent over the real mTLS loopback): the
  agent **connects and is reported online**; a one-shot command runs and streams `queued→running→exited`;
  **capture produces records** for the session; a dangerous command is **refused without**, and **runs
  with**, a gate token minted by the server and verified by the agent using the **pushed** secret.
- `cmd/fleet-agent --role worker` starts the runtime and shuts down cleanly; `--role monitor` reports it's
  not yet implemented (no crash).
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server` and `scripts/test.sh agent` pass,
  with `-race` on `internal/agent` and `internal/fleet`.
- (Real end-to-end against a physical worker is validated by the supervisor separately.)

## Facts this brief relies on

- Default `server_dial`: the **agent listens**, the main server dials: `Fleet_Control_Monitoring_Platform_Architecture.md:152`, `:584`.
- Onboarding writes the pinned CA + client cert + `connect_mode` to the agent: `Fleet_Control_Monitoring_Platform_Architecture.md:195`.
- Config is **pushed over the control channel** (precedent: watchdog policy §11.6, monitor timeouts §8.1):
  `Fleet_Control_Monitoring_Platform_Architecture.md:541`, `:345`.
- Built on: channel (`workflow/tasks/T-0003-control-channel/task.md`), executor+gate
  (`workflow/tasks/T-0007-command-executor/task.md`), capture+fix
  (`workflow/tasks/T-0008-evidence-capture/task.md`, `workflow/tasks/T-0017-capture-live-hang-fix/task.md`),
  server-side manager (`internal/fleet`, `workflow/tasks/T-0011-panel-wiring/task.md`).

## Notes for the doer

- Reuse `internal/fleet/fleettest` for the in-process server+agent harness; don't invent a parallel one.
- The agent holds **no key material in code** — it reads its cert/CA/config from its data dir (D-0006).
- Keep the config-apply path generic (a typed config message with a `gate_secret` and `capture` section)
  so later policy/monitor config reuse it.
- Capture needs root on a real machine; in tests, gate capture behind the capability check (skip off-root)
  but still exercise the executor/stream/gate end-to-end.
