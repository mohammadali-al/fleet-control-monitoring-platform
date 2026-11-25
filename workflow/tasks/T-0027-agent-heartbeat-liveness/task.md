---
id: T-0027
title: agent-heartbeat-liveness
state: merged
touches: [internal/watchdog/heartbeat.go, internal/watchdog/heartbeat_test.go,
          internal/watchdog/policy.go, internal/agent/heartbeat.go,
          internal/onboard/ssh.go, deploy/systemd/fleet-watchdog-liveness.service,
          deploy/systemd/fleet-agent-liveness.service, deploy/systemd/fleet-agent-liveness.timer,
          internal/panel/watchdog.go, web/pages/watchdog.html]
tests: [server, agent]
after: [T-0012, T-0022]
shares: {}
model: {claude: "sonnet high"}
operator_check: "On a real worker: a hung watchdog or agent (stuck, not exited) is restarted within ~threshold+check; the Watchdog page exposes heartbeat interval + staleness threshold with validation."
---

## Why

The existing liveness timer (§11.7) only runs `systemctl is-active` — it catches a **crashed/stopped**
unit but **not a hung one** (deadlocked yet still "active"). Add **heartbeat-based** hang detection for
both the watchdog and the main agent.

## What to build

**Backend (Go) + systemd units.**

1. **Heartbeat write:** each service writes a timestamp to a heartbeat file every **`write`** seconds from
   inside its main loop — the watchdog (`internal/watchdog/heartbeat.go`) and the **main agent**
   (`internal/agent/heartbeat.go`). A healthy loop keeps the file fresh; a hung loop lets it go stale.
2. **Freshness check:** the liveness one-shot (run by the systemd timer) checks the heartbeat is newer
   than **`threshold`**; if stale → restart the unit. Add a **`fleet-agent-liveness`** timer+service for
   the main agent (mirroring the watchdog's), and make the watchdog's liveness one-shot do the
   freshness check (not just `is-active`). **The systemd timer cadence stays fixed at 45 s**
   (`OnUnitActiveSec`).
3. **Tunable via watchdog policy** (`internal/watchdog/policy.go`): `heartbeat_write` and
   `heartbeat_threshold` are fields of the pushed policy (applied live). **Bounds, validated on save/push
   (reject otherwise):** `write` ∈ [2,15] s; `threshold` ∈ [45,300] s; and `threshold ≥ 3 × write` and
   `threshold ≥ 45` (the fixed check). Expose both fields in the Watchdog policy editor
   (`internal/panel/watchdog.go` + `web/pages/watchdog.html`); the 45 s check cadence is **not** exposed.
4. **Onboarding units:** `internal/onboard/ssh.go` installs the new main-agent liveness timer alongside
   the existing watchdog one.

## Out of scope

- The watchdog ladder/guards (T-0012); changing the fixed timer cadence; non-systemd (launchd) liveness
  (token-enrollment/macOS can follow the same pattern later).

## Done means

- A stale heartbeat (simulated) makes the freshness check report "hung → restart"; a fresh one doesn't;
  the policy bounds + the `threshold ≥ 3×write` rule reject bad values and accept good ones (e.g.
  10/75 ok, 50/75 rejected).
- The main agent and watchdog both write heartbeats; the new `fleet-agent-liveness` unit/timer exists and
  the onboarding installs it.
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server`+`agent` pass with `-race` on touched
  packages. (Real restart-on-hang is validated by the supervisor on a real machine.)

## Facts this brief relies on

- Liveness is `Restart=always` + an independent ~1-minute systemd timer that verifies the unit is active
  (and the gap: it won't catch a hung process): `Fleet_Control_Monitoring_Platform_Architecture.md:71`.
- Watchdog policy is pushed over the channel + applied live: `Fleet_Control_Monitoring_Platform_Architecture.md:541`;
  `workflow/tasks/T-0012-watchdog/task.md`; `workflow/tasks/T-0022-watchdog-agent-wiring/task.md`.
- The onboarding that writes the units: `workflow/tasks/T-0020-onboarding-agent-privileges/task.md`.

## Notes for the doer

- Heartbeat writes must be cheap (a timestamp file) so they add no real load. Keep the freshness check a
  tiny one-shot. Validate bounds in one place reused by the policy parser and the panel editor.
