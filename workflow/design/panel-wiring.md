# Operator panel — Go templates and live wiring (Operate pages)

**Status:** built (T-0011, awaiting review) · **Task:** T-0011 · **Builds on:** `panel.md` (front-end), T-0005 (serving),
T-0006 (registry, onboarding), T-0007 (executor, queue, gate), T-0003 (channel) · **Decisions:** D-0001, D-0004, D-0005, D-0006

`panel.md` section 6 described the port to Go `html/template`; this note records what was actually done, what is live,
and what is still a stub.

## 1. Templates (supersedes the stamp workflow)

The **stamp workflow is replaced, not preserved.** Keeping it would have meant two renderers (stamped static pages for
the mock pages, templates for the live ones) and two copies of the shell.

```
web/_src/shell.html        the layout: {{define "layout"}} ... {{template "content" .}} ...; active nav from .Page;
                           the "Mock data" chip only when .Mock; {{block "head"}} / {{block "scripts"}} hooks
web/pages/<page>.html      {{define "title"}} and {{define "content"}} (+ optional "head" / "scripts"), one per page
web/pages/fragments.html   named fragments the browser re-fetches (state chip, machine rows and tiles, target list, host options)
web/templates.go           //go:embed _src/shell.html pages  ->  web.Templates
internal/panel/templates.go   parses layout + fragments + one page per set; renders to a buffer (a template error is a clean 500)
```

Page templates were generated once from the stamped output (macros expanded to markup), then Machines, Command runner,
Terminal and Maneuver were edited by hand. Routes keep the old URLs (`/machines.html`, ...); `/` serves Machines.
CSS, `app.js` and the icon sprite are unchanged; `assets/css/live.css` (new) adds only a `[hidden]` rule that wins over
component display rules, and `assets/js/live.js` (new) is the only new script.

**Left in place, now dead:** `scripts/stamp-web.py`, `web/_src/pages/*.html` and the pre-rendered `web/*.html` pages
(still embedded in `web.FS`, never served except `styleguide.html`). `stamp-web.py` no longer works against the Go
layout. A follow-up should delete them (they were outside this task's touches, and deleting is not allowed here).

## 2. What is live

| Page | Live | Still stub (`data-stub`) |
|---|---|---|
| Machines | registry table + tiles from the registry; state from the channel (online / reconnecting / offline); onboard form; row menu Re-provision and Remove (credentials dialog); filters; Open terminal link | Refresh public IPs (needs agent telemetry), Details, disk figures (shown when the registry has them) |
| Command runner | worker picker (online only, monitors absent), live streamed output per worker, visible serial queue (`Queued #n`), Terminate / Cancel, history, confirmation gate | Evidence columns |
| Terminal | xterm.js over `/ws/shell` to the agent PTY; monitors and offline machines not selectable; sessions list with terminate; in-terminal gate | - |
| Maneuver | unchanged stub; Spike is disabled with a reason, all-in-one is the default (D-0005) | everything (deferred) |
| Projects, Baseline, Monitor, Watchdog, Evidence, Backup | unchanged mock pages (Mock chip) | later tasks |

xterm.js 6.0.0 and its fit addon are vendored under `web/assets/vendor/xterm/` (MIT, licence file included; the page
must work offline, so no CDN).

## 3. Backend

```
browser --HTTP/JSON, fragments, WS--> internal/panel --Backend--> internal/fleet.Manager
                                                                    |  per machine: Queue (T-0007) + Link
                                                                    v
                                              channel.Dialer --mTLS--> agent (Executor + Gate)
```

* **`internal/fleet`** (new) owns one `exec.Queue` and one link per eligible machine (provisioned, not removed,
  `server_dial`). It derives the state shown in the registry (a 1 s poll of the links; `reconnecting` = was up since
  the server started, now down), ends a running command as `lost` when its link drops, fans events out on a bus
  (slow subscribers are dropped, never block the channel read loop), keeps bounded in-memory output and history, and
  mints confirmation tokens. `fleet.Open` builds the production instance: registry from `<data>/registry`, the CA from
  `<data>/ca` (created on first run; a half-present CA is an error, never replaced), the gate secret in
  `<data>/config/gate.key` (0600, created on first run), the provisioner, and mTLS dialers to `<machine IP>:<channel_port>`.
  A peer whose certificate CN differs from the registry ID never counts as online.
* **`internal/fleet/fleettest`** is test support: an in-process Executor behind a `fleet.Link`.
* **`internal/panel`** (`api.go`, `ws.go`): the JSON API and WebSockets below; no auth (the forwarded user header,
  `Remote-User` first, names the operator in the audit trail and as the command's `user`; absent = `operator`).
* **`internal/config`**: `server.channel_port` (default 7443) and `server.server_id` (default `fleet-server`).

### JSON API

| Method and path | Purpose |
|---|---|
| `GET /api/machines` | registry with live state + summary |
| `POST /api/machines` | onboard `{host, port, user, password or private_key, role, connect_mode}` -> 201 |
| `POST /api/machines/{id}/reprovision` | idempotent re-provision (credentials) |
| `POST /api/machines/{id}/remove` | decommission (`skip_remote` for an unreachable machine) |
| `GET /api/commands?mode=&active=` | the serial queue and history (queued, running, finished), newest first |
| `POST /api/commands` | `{machines[], command, timeout_sec, confirm}`; 202 with a result per machine; **409 `needs_confirmation`** (nothing queued) for a dangerous command without `confirm` |
| `GET /api/commands/{id}` | one entry with its captured output |
| `POST /api/commands/{id}/cancel` | cancel a queued command or terminate a running one or a shell |
| `GET /fragments/{name}` | server-rendered pieces (`machines-rows`, `machines-tiles`, `command-targets`, `shell-hosts`) |

State-changing calls must be `application/json`; unknown fields are rejected; bodies are capped at 64 KiB. SSH
passwords and keys are used for the one request and never stored or logged. Errors are `{"error": "..."}` with 400
(bad input), 404 (unknown), 409 (offline / needs confirmation), 422 (monitor agent: no shell), 503 (no backend or no agent
binary configured), 502 (provisioning failed).

### WebSockets

* `/ws/events`: `{"type":"machine","machine":{...}}`, `{"type":"command","event":"queued|running|refused|exited",...}`,
  `{"type":"chunk","session","stream","data":<base64>}` for one-shot output. Shell output is **never** sent here.
* `/ws/shell?machine=&cols=&rows=`: binary frames are keystrokes in / terminal output out; text frames are JSON control
  (`resize`, `confirm` in; `session`, `state`, `refused`, `exit`, `error` out). Closing the socket terminates the shell.
  Origin must match the host (library default).

### The confirmation gate (enforced agent-side)

The panel only collects the operator's confirmation. One-shot: `POST /api/commands` without `confirm` returns 409 for a
matching command; with `confirm` the manager mints an HMAC token bound to that machine and that exact command and
sends it in `run.confirm_token`; the **agent** verifies it (single use, expires; a browser-only "ok" never works).
Shell: the agent discards a refused line and reports it; the dialog shows the line (tracked server-side on a best-effort
basis); confirming mints a token for that line and re-sends it with the token. A wrong guess fails on the agent.
The token lives 10 minutes (not 2) because a confirmed command may wait in the queue behind a long one.

## 4. Limits and follow-ups

* **No real agent yet:** `cmd/fleet-agent` is still scaffolding, so against a real machine the panel shows it offline.
  The wire path is covered by tests over real mTLS with the real Executor (`internal/fleet/open_test.go`) and over an
  in-process link (panel tests).
* **Gate secret distribution:** the agent needs the same secret as `<data>/config/gate.key`; config push is a later task.
* **`agent_dial` machines** show offline with a note: the main-server listener for agent-initiated connections does not exist.
* **History is in memory** (queue keeps 100 finished per machine) and lost on restart; evidence is the durable record.
* A shell shares the machine's strict FIFO with one-shot commands, so it waits while a command runs (T-0007 design).
* A browser reload ends its shells (the socket closes).
* `app.js` (T-0004): opening a row menu whose popup lands below the fold can scroll the page, and its capture-phase
  scroll listener then closes the menu. Not caused by this task; fix with `focus({preventScroll: true})`.
* Dead files listed in section 1 should be deleted by a cleanup task.
