# T-0028 watchdog-reset-counters: log

## 2026-11-25: counter reset wired end to end (Go 1.26.8), one code commit

**Built.**

- `internal/channel/envelope.go`: `TypeWatchdogReset = "watchdog-reset"` (server -> agent, no payload).
- `internal/fleet/watchdog.go`: `Manager.ResetCounters(ctx, machine)` pushes it over the link. Same
  guards as `PushPolicy`: `ErrUnknown` for a non-worker, `ErrOffline` when not connected. A reset is
  not queued for a later connection (it is an immediate operator intent, unlike a policy).
- `internal/agent/watchdog_wire.go`: the main agent handles `watchdog-reset` next to `policy`.
  **Cross-process design:** the watchdog is its own process that keeps the counters in memory and owns
  `state.json`, so the main agent must not edit that file (the watchdog would write its memory back).
  It leaves a request marker `<DataDir>/watchdog/reset.request` instead (`watchdog.RequestReset`).
  `NewWatchdog` hooks `Guards.OnReset` so the reset is reported as a `counters-reset` watchdog-ev (the
  panel derives host state from that event).
- `internal/watchdog/guards.go`: `RequestReset`, `ResetRequested`, and `Guards.Roll` (called first in
  every `Engine.Observe`) takes a pending request (remove-as-test, so exactly once) and runs the
  existing `Guards.Reset` semantics: daily counters, alert-only latch, ladder stage and the corrupt
  flag cleared, **cooldowns kept**. `ladder.go`/`watchdog.go` are untouched.
- `internal/panel/watchdog.go`: `POST /api/watchdog/reset {machine?}`; no machine = every connected
  worker. 404 unknown/monitor, 409 offline, 400 bad body. Uses a small `counterResetter` interface on
  the backend (`observe.go`, outside touches, is unchanged).
- `web/pages/watchdog.html`: the header button is now `#wd-reset-all` (no `data-stub`), with a small
  inline script (confirm, POST, status line in `#wd-status`). It also handles the per-host
  "Reset counters" buttons, which `live.js` creates: a capture-phase delegated listener resets that
  host instead of the stub toast.

**Tests.** `internal/agent/watchdog_reset_test.go` (in-process `fleettest` mTLS agent + manager: a
reset pushed by the server clears the watchdog's counters + alert-only latch, keeps cooldown clocks,
persists to disk, consumed once, event reaches the server; raw envelope; unknown machine),
`internal/panel/watchdog_reset_test.go` (endpoint: one host, all, offline 409, unknown/monitor 404,
bad field 400, page no longer a stub), `internal/watchdog/guards_reset_test.go` (Roll applies a
request once). `gofmt -l .` empty, `go vet ./...` clean, `scripts/build.sh` ok,
`scripts/test.sh server` and `agent` pass; `go test -race` on watchdog, agent, fleet, panel, channel pass.

**Decisions / for the supervisor.**

- `internal/panel/observe_stream_test.go` (T-0023's test, same test group) asserted the stub text
  ("control-channel message") on the page; changed it to assert `id="wd-reset-all"`.
- `web/assets/js/live.js` is outside touches: its per-host buttons still get an inert `data-stub`
  attribute (the page script intercepts the click first, so it does not show). Optional cleanup patch:
  `workflow/tasks/T-0028-watchdog-reset-counters/live-js-drop-row-stub.patch` (`git apply` it).
- The reset takes effect at the watchdog's next sample (policy `sample_interval`, seconds), not
  instantly; the page says so. If no watchdog process runs, the request waits and applies when one
  starts. Nothing in `cmd/` needed changing. `scripts/check-app` does not exist in this tree, so no
  UI browser check was run; the page script was exercised only through the endpoint tests.

<!-- wf: commits=267f9a5 left=- tests=server:pass,agent:pass blocked=no -->
