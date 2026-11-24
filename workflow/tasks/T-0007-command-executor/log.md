## T-0007 command-executor (doer, claude sonnet high)

Built package `internal/exec`. `github.com/creack/pty` v1.1.24 downloaded fine (go.mod/go.sum updated, `go mod tidy` clean).

- Channel (`internal/channel/envelope.go`): only added type constants `run`, `state`, `chunk`, `input`, `exit`,
  `resize`, `terminate`, `confirm` (envelope ID = session_id). `state` (queued/running/refused) and `input` (PTY bytes)
  are two extras beyond the brief's list, needed for the lifecycle and PTY input. Payload structs live in
  `exec/messages.go`; the channel itself is unchanged.
- `session.go`: `Session` (id, command, user, host, mode, start/end, exit code, cgroup) holding metadata once;
  `Info()` snapshot; `CgroupPath()` for capture (T-0008). Per-session cgroup v2 at `<CgroupRoot>/<session_id>`;
  the command is placed in it at fork via `SysProcAttr.CgroupFD` (CLONE_INTO_CGROUP), so no unattributed window.
  `Kill()` uses `cgroup.kill`; removed after the session unless `KeepCgroup`. Session ids are restricted to
  `[A-Za-z0-9][A-Za-z0-9._-]{0,63}` (they become directory names). If `CgroupRoot` is set and creation fails the
  command fails (never runs untracked); `CgroupRoot==""` disables cgroups (dev/unit tests).
- `executor.go` (agent): `New(Config)`, `Handle` = the channel handler (never blocks on a command). Mode A runs
  `/bin/sh -c cmd` in its own process group + cgroup; stdout/stderr are pumped as 16 KiB `chunk`s (seq per
  session); per-command timeout (default 300 s, `TimeoutSec` overrides); terminate and timeout send SIGTERM, then
  SIGKILL of the whole tree (cgroup.kill, else process group) after a 3 s grace; after the command exits the tree is
  killed so the session ends with its command. Emits `running` (with the cgroup path), then `exit{code, duration_ms,
  reason}` strictly after all chunks. Signal deaths report 128+sig. Hooks `OnSessionStart/OnSessionEnd` for capture.
- `pty.go` (Mode B): `/bin/sh` (or `Command`) on a PTY via creack/pty, in the session cgroup, `pty` chunks out,
  `input` envelopes in, `resize`, terminate (SIGHUP then SIGKILL), optional timeout (none by default).
- `queue.go` (main server): per-machine strict FIFO `Queue` (Submit/Run/Handle/Cancel/Terminate/Input/Resize/
  Confirm/Snapshot/ConnectionLost, events queued/running/refused/chunk/exited). The next `run` is sent only after the
  previous session's `exit` arrived. PTY sessions go through the same FIFO, so a machine holds one manual session at
  a time (a PTY shell blocks later commands until it ends). Maneuver concurrency is not here.
- Gate (`gate.go`, enforced in the agent): server-configurable regex list (`DefaultPatterns`, `SetPatterns`) matched on
  the whitespace-normalised command. Confirmation token = `v1.<expiry>.<hex HMAC-SHA256(secret, machine NUL command
  NUL expiry)>`; minted by the server (`Gate.Mint`) after the operator confirms, verified by the agent (`Authorize`)
  with the shared secret: bound to one machine and one command, expires (default 2 min), single-use. A browser-only
  "ok", a token from another key, machine or command, an expired or replayed token are all refused. One-shot: checked
  in `run` before anything executes; the token rides in `run.confirm_token`. PTY: `lineFilter` tracks the typed line
  (backspace, Ctrl-U/C/W, bracketed paste) as input arrives; on Enter for a matching line without a valid token the
  Enter is withheld and Ctrl-U is sent so the shell discards the line, the terminal gets a `[fleet] refused` note and
  a `state refused` goes to the server. The token comes either inline (`input.confirm_token`) or earlier via a
  `confirm` envelope (stored per session, single-use). Every refusal is logged (slog WARN with machine, session,
  user, command, reason).
- Known limits (documented in code): the PTY gate cannot see edits made with history/cursor escape sequences; by
  default such a line is refused on Enter (`AllowOpaquePTY` relaxes this for TUIs). Aliases/scripts/subshells are
  out of reach of any text gate. The gate pattern list and secret are objects here; a later task wires config push.
  If the channel drops mid-command the agent's exit cannot arrive; the caller invokes `Queue.ConnectionLost()`.
- Tests: `gate_test.go` (patterns, token forgery/expiry/replay/binding, line filter incl. split lines, backspace,
  history keys, paste), `executor_test.go` (stream + exit code + duration, large output, timeout incl. children,
  default 300 s, terminate and SIGKILL escalation, session metadata/hooks, cgroup placement and cleanup, escaped
  children killed via cgroup, cgroup failure fails the command, strict FIFO order, cancel, connection lost, one-shot
  and PTY gate refusal/token/confirm-envelope flows incl. log assertions, PTY round trip + resize + terminate +
  shell exit, PTY serialised with one-shots, and one test over the real mTLS channel). Gate tests use a `DANGER`
  pattern so no destructive command ever runs.
- cgroup tests: they need a writable cgroup v2 dir. They use `/proc/self/cgroup`'s own (systemd-delegated) subtree,
  which works as an unprivileged user here, and skip clearly otherwise; they ran and passed (not skipped) in this
  sandbox, but as uid 1000, not root. Running as root under `/sys/fs/cgroup/fleet` is not exercised.
- Self-check: `gofmt -l .` empty; `go vet ./...` clean; `go test -race -count=4 ./internal/exec` passes;
  `scripts/test.sh server` and `scripts/test.sh agent` pass; `scripts/build.sh` ok (bin/ removed). Temp cgroups and
  processes cleaned up (checked).

<!-- wf: commits=f0df59a left=- tests=server:pass,agent:pass blocked=no -->
