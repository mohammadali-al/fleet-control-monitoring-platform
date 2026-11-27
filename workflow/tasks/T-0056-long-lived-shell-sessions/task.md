---
id: T-0056
title: long-lived-shell-sessions
state: merged
touches: [internal/panel/ws.go, internal/fleet/fleet.go, internal/panel/api.go,
          web/src/pages/TerminalPage.jsx, internal/panel/ws_test.go]
tests: [server]
after: [T-0048]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Closing or navigating away from a terminal (browser WS disconnect) no longer kills the agent PTY — the shell keeps running as a long-lived session. The operator can reattach to a running session from the active-sessions list and resume it (with recent scrollback). A session ends only on explicit Terminate or an idle-timeout reaper, never on mere browser disconnect. Multiple one-shot/shell behaviour otherwise unchanged."
---

## Why

Architecture §4.6/§4.7 says an interactive shell (Mode B) is its "own concurrent, long-lived session" with
explicit per-session terminate. Today the panel kills the agent PTY the moment the browser WebSocket closes
(`internal/panel/ws.go:296-299` — `// However this handler ends, the shell ends with it.` → `s.be.Cancel(session)`),
so a refresh, navigation, or network blip destroys the shell (audit GAP-09). Decouple the PTY's lifetime
from the browser connection: keep it alive on disconnect, allow reattach, and end it only explicitly or by
idle timeout.

## What to build

**Backend:**
1. **Don't cancel on browser disconnect.** In `wsShell` (`internal/panel/ws.go:263`), remove the
   kill-on-handler-exit (`defer … Cancel`). When the browser WS closes, the agent PTY session must stay
   open. The server must **keep draining** the session's output while detached (so the agent doesn't block
   on backpressure) into a **bounded per-session scrollback ring buffer**.
2. **Session registry + reattach.** Track live shell sessions in the fleet manager (`internal/fleet/fleet.go`,
   around `OpenShell`): a new `/ws/shell` connection with `?session=<id>` **attaches** to that existing
   session (replays the recent scrollback buffer, then streams live + forwards input/resize) instead of
   always opening a new one; without `?session` it opens a new one as today. Guard reattach by operator/
   machine ownership.
3. **Explicit terminate + idle reaper.** Explicit Terminate (the existing `/api/commands/{id}/cancel`
   → `Cancel`) ends the session and its buffer. Add an **idle-timeout reaper** (a detached session with no
   attachment for N minutes — make N a small constant/config) so sessions can't orphan forever; log every
   reap. Terminating also tells the agent to end the PTY.

**Front-end (React, on the merged Terminal port):**
4. `web/src/pages/TerminalPage.jsx`: closing a terminal **tab** or leaving the route must **detach, not
   terminate** (just close the WS; don't cancel). The active-PTY list (already `GET /api/commands?mode=pty&active=1`,
   `TerminalPage.jsx:421`) gets a **Reattach** action that opens `/ws/shell?session=<id>` and resumes
   (showing the replayed scrollback). Keep an explicit **Terminate** control (calls the cancel endpoint).
   Rebuild + commit the web bundle (`./web/build.sh`).

## Out of scope

- No change to one-shot (Mode A) command behaviour, the exec queue semantics, evidence capture, or the
  dangerous-line gate. No multi-viewer simultaneous attach required (single active attach per session is
  fine; a second attach may take over or be refused — pick one and document it). No persistence of shells
  across a fleet-server restart (in-memory session registry is acceptable; note it).

## Done means

- Browser WS disconnect (close/refresh/navigate) leaves the agent PTY running; reattach from the active
  list resumes it with recent scrollback; a session ends only on explicit Terminate or the idle reaper; the
  agent is told to end the PTY on terminate. No orphaned/blocked sessions (the server drains output while
  detached).
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` passes
  with tests for: disconnect-does-not-cancel, reattach replays buffer + streams, idle-reaper ends a
  detached session, explicit terminate ends it. Web bundle rebuilt + committed.

## Facts this brief relies on

- Kill-on-close + OpenShell: `internal/panel/ws.go:289` (`OpenShell`), `:296-299` (`defer … Cancel`).
  Session/shell plumbing: `internal/fleet/fleet.go:870-891` (`ShellRequest`, `OpenShell`); the event bus
  relays PTY chunks (`internal/panel/ws.go:76`). Cancel endpoint: `internal/panel/api.go:432`
  (`/api/commands/{id}/cancel`). Active-PTY list consumed by the React terminal: `web/src/pages/TerminalPage.jsx:421`.
  Spec: §4.6/§4.7 (`Architecture_Gap_Audit.md`, GAP-09).

## Notes for the doer

- The subtle part is **draining output while detached** (ring buffer) so the agent never blocks; size the
  buffer sensibly (recent scrollback, not full history). Keep reattach authorization tight (same operator/
  machine). Rebuild + commit the web bundle. Run `./scripts/wf start T-0056` before committing; commit
  inside touches + the log. Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:`
  (except via `wf start`).
