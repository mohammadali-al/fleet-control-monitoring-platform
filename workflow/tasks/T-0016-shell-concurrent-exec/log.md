# T-0016 log

## Doer entry

Interactive shells (Mode B) now run as their own concurrent sessions, outside the per-machine one-shot
FIFO. One-shot commands stay strictly serial among themselves, whether or not a shell is open.

What changed (a scheduling change on the main-server `Queue`; the agent `Executor` already ran every
`run` in its own goroutine, so only a doc comment changed there; `pty.go` needed no change):

- `internal/exec/queue.go`: `Submit` puts PTY sessions in a separate `shells` list, one-shots in the FIFO.
  `Run` now dispatches both: every new shell starts immediately in its own goroutine; the FIFO head starts
  only when no one-shot is in flight (`busy`). `Handle`/`Terminate`/`Input`/`Resize`/`Confirm`/`Cancel`/
  `ConnectionLost` look a session up through `active(id)` (running one-shot or open shell), so state,
  chunks, exit and the control messages route per session id. `Snapshot` lists running one-shot, open
  shells, queued one-shots, then finished. `finish` now emits `exited` before releasing the executing
  goroutine, so the next one-shot cannot start before the previous one's exit is reported.
- Multi-shell policy (documented on `Queue`): several shells may be open at once per machine, each an
  independent session, capped at `MaxShells = 8`; the 9th `Submit` returns `ErrTooManyShells`. One-shots
  are unaffected by the cap.
- Session id + cgroup per session, the gate (one-shot at `run`, shell input lines in the agent line
  filter), timeout and terminate are unchanged (executor.go/pty.go logic not touched).
- `internal/exec/executor_test.go`: replaced `TestQueueSerialisesPTYAndOneShot` (it asserted the old
  behaviour) with tests: one-shot runs while a shell is open; shell starts while a one-shot runs;
  one-shots strictly serial with a shell open; terminate/gate for both kinds independently; per-session
  cgroups under concurrency; two concurrent shells; queue scheduling with a silent agent (no root);
  shell cap.

Self-check: `gofmt -l .` empty; `go vet ./...` clean; `go test -race ./internal/exec/` pass (also
`-count=5` on the shell/queue/PTY tests); `scripts/build.sh`, `scripts/test.sh server`, `scripts/test.sh
agent` pass.

<!-- wf: commits=4fd11e7 left=- tests=server:pass,agent:pass blocked=no -->
