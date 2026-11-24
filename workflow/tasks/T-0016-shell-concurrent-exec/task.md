---
id: T-0016
title: shell-concurrent-exec
state: ready
touches: [internal/exec/queue.go, internal/exec/executor.go, internal/exec/pty.go,
          internal/exec/executor_test.go]
tests: [server, agent]
after: [T-0007]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

T-0007 put interactive shells (Mode B) through the same per-machine one-shot FIFO, so an open shell
blocks every one-shot command until it closes. That's wrong UX. An interactive shell should run as its
own concurrent, long-lived session; the FIFO governs one-shot commands only (spec updated, §4.6).

## What to build

**Backend (Go), `internal/exec`.**

1. **Shells bypass the one-shot FIFO.** A PTY/shell session (Mode B) starts **immediately** as its own
   concurrent session and does **not** take a slot in the per-machine one-shot queue. One-shot commands
   continue to run **strictly one at a time among themselves**, regardless of whether a shell is open.
2. **Keep everything else intact:** session id + cgroup per session (so capture T-0008 still attaches
   per session), the dangerous-command gate (applies to shell input lines and one-shots alike), stream/
   timeout/terminate, and the lifecycle messages. Concurrent sessions are safe (manifest is
   concurrency-safe after T-0015).
3. Decide and document the policy for **multiple shells**: at least one open shell concurrent with the
   one-shot queue must work; keep it simple (e.g. allow N shells, or one shell — state which in a
   comment) and make the panel-visible state correct.

## Out of scope

- Maneuver / Load Controller concurrency (deferred).
- Panel UI changes (the sessions list already exists; wiring is T-0011) — just expose correct state.
- Capture, channel redesign.

## Done means

- A one-shot command **runs while an interactive shell is open** on the same worker agent (test: open a
  shell, submit a one-shot, it runs without waiting for the shell to close).
- One-shot commands still run **strictly serially among themselves** (two submitted together run in
  order, never overlapping) — unchanged by an open shell.
- Session id/cgroup, the gate, timeout and terminate still work for both shells and one-shots.
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server` and `scripts/test.sh agent` pass,
  including `-race` on `internal/exec`.

## Facts this brief relies on

- One-shot commands serial per worker via the FIFO; a shell runs as its own **concurrent** session and
  does not occupy the FIFO: `Fleet_Control_Monitoring_Platform_Architecture.md:203`, `:205`.
- Mode A one-shot vs Mode B shell, same session tagging: `Fleet_Control_Monitoring_Platform_Architecture.md:209`, `:210`.
- The executor/queue/PTY to change (T-0007): `workflow/tasks/T-0007-command-executor/task.md`.

## Notes for the doer

- Keep the change surgical: it's a scheduling change (shells out of the FIFO), not a rewrite of the
  executor. Preserve the gate and session/cgroup wiring exactly.
- Keep logic unit-testable without root (the queue/scheduling behavior needs no root).
