---
id: T-0007
title: command-executor
state: ready
touches: [internal/exec/session.go, internal/exec/executor.go, internal/exec/pty.go,
          internal/exec/gate.go, internal/exec/executor_test.go, internal/exec/gate_test.go,
          internal/channel/envelope.go, go.mod, go.sum]
tests: [server, agent]
after: [T-0003]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

The core worker-agent capability: run operator commands one at a time, stream their output back, and
enforce the dangerous-command gate agent-side. It also creates the `session_id` + tracked cgroup that
the evidence capture (T-0008) binds to.

## What to build

**Backend (Go).** Worker-agent executor + the server-side serial queue, over the channel (T-0003).

1. **Session** (`internal/exec/session.go`): a `Session` with `session_id`, command text, user, host,
   start/end, exit code, and a **tracked cgroup v2** (own path) the command runs in. Metadata is held
   **once** per session (not per packet). Expose the cgroup path so capture (T-0008) can attach.
2. **Executor** (`internal/exec/executor.go`), worker side:
   - **Mode A one-shot**: run a command (as root) inside the session cgroup; stream stdout/stderr back
     over the channel in chunks; enforce a per-command **timeout (default 300 s)**; support terminate.
   - Emit lifecycle `queued → running → exited(code, duration)`.
3. **PTY** (`internal/exec/pty.go`), **Mode B**: interactive shell over a PTY (`creack/pty`),
   resize-aware, streamed both ways over the channel; same session tagging; per-session terminate.
4. **Serial queue** (server side, in `executor.go` or a small queue type): a **per-machine FIFO**;
   manual commands run **strictly one at a time**; surface queued/running/finished. (Maneuver
   concurrency is NOT here — out of scope, owned by the Load Controller.)
5. **Dangerous-command gate** (`internal/exec/gate.go`): a **server-side configurable pattern list**;
   a matched command requires a confirmation token in the command message, **enforced in the agent** —
   including PTY input lines, checked before execution. Refusals are logged and surfaced. Browser checks
   are UX only and are not relied on.
6. Add the needed message types to `internal/channel/envelope.go` (run, chunk, exit, resize, terminate,
   confirm) — small additions, no redesign of the channel.

## Out of scope

- Evidence **capture** of the traffic (T-0008) — this task only creates the session + cgroup and runs
  the command; capture attaches separately.
- Maneuver / Load Controller concurrency (deferred, D-0005).
- Panel UI / HTTP wiring (later). Exercise everything through tests and the channel.

## Done means

- A one-shot command runs inside its session cgroup, streams output, honors the timeout and terminate,
  and reports the correct exit code/duration.
- The per-machine queue runs two submitted commands **strictly in order**, never concurrently.
- A PTY shell round-trips input/output and handles resize; terminate ends it.
- The gate: a command matching the pattern list is **refused** without a confirmation token (including a
  PTY input line) and **runs** with one; refusals are logged. A browser-only "ok" cannot bypass it.
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server` and `scripts/test.sh agent` pass.

## Facts this brief relies on

- Manual commands run **strictly serially per worker agent** via a per-machine FIFO queue; Maneuver
  replicas are separate: `Fleet_Control_Monitoring_Platform_Architecture.md:203`, `:204`.
- Mode A one-shot (`session_id`, streamed, `queued→running→exited`) and Mode B PTY (xterm ⇄ WS ⇄ agent
  PTY, resize-aware, same session tagging): `Fleet_Control_Monitoring_Platform_Architecture.md:209`, `:210`.
- Dangerous-command gate: server-side pattern list, **enforced in the agent** incl. PTY input lines,
  refusals logged: `Fleet_Control_Monitoring_Platform_Architecture.md:214`, `:216`.
- Every execution runs inside a tracked cgroup owned by a `session_id`; metadata stored once per
  session: `Fleet_Control_Monitoring_Platform_Architecture.md:256`, `:61`.
- The channel exists (T-0003): `workflow/tasks/T-0003-control-channel/task.md`.

## Notes for the doer

- Adds `github.com/creack/pty` (updates `go.mod`/`go.sum`) — **needs a module download**; if offline,
  stop and report blocked.
- cgroup v2 operations need root; guard tests that need real cgroups behind a capability check and skip
  with a clear message when not root, but keep queue/gate/PTY logic unit-testable without root.
- Keep the gate list and confirmation-token format simple and documented; a later task wires the panel.
