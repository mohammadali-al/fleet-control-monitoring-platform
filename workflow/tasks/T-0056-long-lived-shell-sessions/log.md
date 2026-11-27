# T-0056 long-lived-shell-sessions: log

## Doer entry

- **Disconnect & scrollback ring buffer (`internal/fleet/fleet.go`, `internal/panel/ws.go`):**
  - Removed the kill-on-handler-exit `defer ... Cancel` from `wsShell` (`internal/panel/ws.go`) and replaced it with `att.Detach()`, so closing the browser WebSocket or navigating away detaches without terminating the agent PTY.
  - Added an in-memory live shell registry (`Manager.shells`) and bounded per-session circular byte ring buffer (`ringBuf`, default `defaultShellScrollbackBytes = 128 << 10`, configurable via `Config.ShellScrollbackBytes`). `onExec` continuously drains `ModePTY` `EventChunk` bytes into `ringBuf` regardless of whether a browser is attached, and sends non-blocking updates to any active viewer channel (dropping a lagging viewer without blocking the agent read loop). Live shell sessions are in-memory only and are not persisted across a fleet-server restart.
- **Session attach, ownership & second-attach policy (`internal/fleet/fleet.go`, `internal/panel/api.go`, `internal/panel/ws.go`):**
  - Added `Manager.AttachShell(AttachRequest)` (and exposed it on `panel.Backend` in `internal/panel/api.go`, with `ErrForbidden` mapped to HTTP 403 in `statusFor`).
  - `/ws/shell?session=<id>` attaches to an existing live session, replays the buffered `Scrollback` in 16 KiB binary frames before streaming live events, and forwards input/resize/confirm. Without `?session`, `/ws/shell` calls `OpenShell` and attaches as before.
  - Guarded reattach by operator and machine ownership (`normalizeOperator(req.User) == normalizeOperator(s.user)` and, if `req.Machine` is given, `req.Machine == s.machine`).
  - **Second-attach policy:** single active attach per session where a **second attach takes over** — `AttachShell` closes the previous attachment's `Evicted` channel so the displaced WebSocket receives `{"type":"error","error":"session attached elsewhere"}` and closes cleanly without detaching or cancelling the new attachment.
- **Explicit terminate & idle reaper (`internal/fleet/fleet.go`):**
  - Explicit terminate (`POST /api/commands/{id}/cancel` -> `Manager.Cancel` -> `Queue.Cancel` -> `TypeTerminate`) continues to ask the agent to end the PTY; on `EventExited`, `handleShellExec` clears the session's scrollback ring buffer and removes it from `Manager.shells`.
  - Added an idle-timeout reaper goroutine in `Manager.Start` (`reapIdleShells`, default `defaultShellIdleTimeout = 15 * time.Minute`, configurable via `Config.ShellIdleTimeout`) that finds detached sessions idle >= timeout, logs `"reaping idle shell session"` (with machine, session, operator, and idle duration), and calls `Manager.Cancel` to end the session and its agent PTY.
- **Front-end (`web/src/pages/TerminalPage.jsx` + rebuilt `web/assets/dist/app.js`):**
  - Closing a terminal tab (`Detach` / `Close tab`) or leaving the `/terminal` route now only closes the WebSocket via React unmount without calling `/api/commands/{id}/cancel`.
  - Added a **Reattach** button on each entry in the active **Sessions** list (`GET /api/commands?mode=pty&active=1`) and on detached tabs, opening `/ws/shell?session=<id>` and displaying replayed scrollback + live I/O. Kept explicit **Terminate** controls in both the tab toolbar and the Sessions list calling `POST /api/commands/{id}/cancel`.
  - Rebuilt the SPA bundle with `./web/build.sh` (`assets/dist/app.js` 495.4kb, `assets/dist/app.css` 39.8kb).
- **Tests & verification:**
  - Added `internal/panel/ws_test.go`: `TestWSShellDisconnectDoesNotCancel`, `TestWSShellReattachReplaysBufferAndStreams`, `TestWSShellReattachOwnershipAndErrors`, `TestWSShellSecondAttachTakesOver`, `TestWSShellExplicitTerminateEndsSession`, and `TestWSShellIdleReaperEndsDetachedSession`.
  - Updated `TestWSShell` in `internal/panel/api_test.go` to verify closing the WebSocket leaves the session active and explicit cancel ends it.
  - Added `TestShellScrollbackRingBufferBounded` in `internal/fleet/fleet_test.go` to verify bounded ring-buffer eviction while detached.
  - Verified: `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok with `node` removed from `PATH`, `./scripts/test.sh server` passed, and `go test -race ./internal/panel ./internal/fleet` passed.

wf: T-0056 done
