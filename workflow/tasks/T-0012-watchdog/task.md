---
id: T-0012
title: watchdog
state: merged
touches: [internal/watchdog/watchdog.go, internal/watchdog/metrics.go, internal/watchdog/ladder.go,
          internal/watchdog/policy.go, internal/watchdog/guards.go, internal/watchdog/watchdog_test.go,
          internal/watchdog/policy_test.go, internal/config/config.go, internal/channel/envelope.go,
          deploy/systemd/fleet-watchdog.service, deploy/systemd/fleet-watchdog-liveness.timer]
tests: [agent]
after: [T-0003]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

The agent-side safety net: keep a machine from hitting max resource usage and freezing/becoming
inaccessible. It is a load-safety net, not a tamper-proof supervisor (trust model). It must **not fight
the Load Controller** — its thresholds sit above the Maneuver operating ceiling.

## What to build

**Backend (Go), agent side**, part of the agent binary (`fleet-watchdog.service`, `Restart=always`).

1. **Metrics** (`metrics.go`): self-contained sampling every **5 s** (configurable) from cgroup v2,
   `/proc`, netlink — no node_exporter dependency. Watch targets: configured systemd units (per-service
   thresholds) + machine-level thresholds (CPU, memory, load, disk, egress).
2. **Detection** (`ladder.go` or `metrics.go`): anti-flap — sustained breach for **20 s** (4 samples)
   before acting; **hysteresis** (trigger at breach, clear below recovery).
3. **Escalation ladder** (`ladder.go`), per host, scoped to the offending service where attributable,
   steps toggleable per policy: **1) throttle** (transient cgroup `CPUQuota` + optional `tc tbf`, held
   `throttle_duration` default 120 s) → **2) kill offending session** (its cgroup/process group in one
   action, never per-socket) → **3) service restart** (`systemctl restart`) → **4) reboot**
   (`systemctl reboot`).
4. **Guards** (`guards.go`): cooldowns (10/30/60 min after throttle/restart/reboot); **daily caps** (max
   3 restarts, 1 reboot) with counters **persisted to disk** (survive reboot); boot-stability window
   (5 min); **alert-only mode** when caps exhausted (keep measuring, notify, act no further; reset via
   panel or midnight).
5. **Policy** (`policy.go`): a version-stamped **YAML policy profile**, stored on the server, **pushed
   over the channel**, applied **live** without restart; per-host/group overrides; a minimal baked-in
   default; last-known profile keeps running if the server is unreachable.
6. **Units** (`deploy/systemd/`): `fleet-watchdog.service` (`Restart=always`) + an independent **1-minute
   liveness timer** that restarts the watchdog if its unit is not active.

## Out of scope

- The Load Controller itself (deferred, D-0005) — only ensure watchdog thresholds are configured to sit
  **above** its operating ceiling and never throttle intended Maneuver load.
- Panel watchdog UI (wiring is a later task; the static page exists).

## Done means

- A simulated sustained breach (fed metrics) triggers the ladder in order with the 20 s sustain +
  hysteresis; cooldowns and daily caps are enforced and **persist across a simulated reboot**; caps
  exhausted → alert-only.
- A pushed YAML policy is validated and applied **live** (no restart); an invalid policy is rejected and
  the last-known one keeps running.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh agent` passes. (Real `systemctl`/`tc`
  actions are gated behind root and skipped clearly when unavailable; the decision logic is unit-tested
  with a fake action executor.)

## Facts this brief relies on

- Purpose + trust model: `Fleet_Control_Monitoring_Platform_Architecture.md:505`, `:508`.
- Ladder steps: `Fleet_Control_Monitoring_Platform_Architecture.md:526`, `:527`, `:528`, `:529`.
- Guards (cooldowns, daily caps persisted): `Fleet_Control_Monitoring_Platform_Architecture.md:533`, `:534`.
- Liveness (`Restart=always` + 1-min timer): `Fleet_Control_Monitoring_Platform_Architecture.md:71`, `:506`.
- Must not fight the Load Controller (thresholds above its ceiling): `Fleet_Control_Monitoring_Platform_Architecture.md:70`.
- Channel for policy push (T-0003): `workflow/tasks/T-0003-control-channel/task.md`; data dir for counters: `workflow/decisions/D-0006-data-directory.md`.

## Notes for the doer

- Put every system action behind an `Actuator` interface so the ladder is testable without root; the
  real actuator calls `systemd-run`/`systemctl`/`tc`.
- Counters and last-known policy persist under `DataDir` (D-0006).
