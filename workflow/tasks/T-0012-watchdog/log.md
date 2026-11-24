# T-0012 watchdog: log

## Entry 1: implementation (doer)

Built `internal/watchdog` (agent side), the policy-push envelopes and the systemd units. Toolchain: go1.26.8.

**Files**
- `metrics.go`, `metrics_linux.go`, `metrics_other.go`: `Sampler` interface + `SystemSampler` (no root, no node_exporter):
  `/proc/stat|meminfo|loadavg` for CPU/memory/load, cgroup v2 `cpu.stat`/`memory.current` for watched units
  (`system.slice/<unit>`) and per-session cgroups (agent `cgroup_root`), rtnetlink `RTM_GETLINK` (`IFLA_STATS64`)
  for egress Mbit/s, `statfs` for disk. Deltas against the previous sample; first sample reports 0 for rates.
- `ladder.go`: `Actuator` interface (Throttle/Unthrottle/KillSession/RestartService/Reboot), anti-flap
  `detector` (N consecutive samples at/above `trigger`, N = ceil(sustain / interval) = 4 for 20 s / 5 s; once
  active it clears only below `recover`), and `Engine` (detect, lift throttles, take at most one rung per
  sample). Ladder: throttle -> kill session -> service restart -> reboot, each toggleable; scoped to the offender
  (service breach -> that unit; machine CPU/load/memory breach -> the session, else watched service, holding at
  least `attribution_share` of it; else the machine). Throttle held `throttle.duration` (120 s) and lifted early on
  recovery. Rungs that do not apply (kill with no session; CPU quota for a memory breach; egress shaping without
  `egress_kbit`) are skipped in the same sample; `ErrNotApplicable` from the actuator does the same.
- `guards.go`: persisted `State` (`<DataDir>/watchdog/state.json`, atomic write): daily restart/reboot counters,
  last-action times (cooldowns 10/30/60 min, plus 1 min after a kill), ladder stage, latched alert-only. Boot
  window (5 min) is checked against boot time from `/proc/uptime`. Caps (3 restarts, 1 reboot) exhausted ->
  alert-only latched (measure and notify, no action) until the operator `ResetCounters` or local midnight
  (`auto_reset_midnight`). Restart/reboot counters are persisted BEFORE the action runs; if they cannot be
  saved the action is refused. A corrupt state file is moved aside and the guards start in alert-only (fail safe).
- `policy.go`, `yaml.go`: version-stamped YAML profile -> `Policy` (every key optional over the baked-in
  default), per-group then per-host overrides (`overrides: {groups: {...}, hosts: {...}}`, deep-merged),
  `Validate` (hysteresis `0 < recover <= trigger`, ranges, units, no watching the watchdog's own units, and the
  Load Controller rule below), last-known profile persisted to `<DataDir>/watchdog/policy.yaml`,
  `StorePolicy` for a process that holds the channel but not the loop.
- `watchdog.go`: `Watchdog` (`New`, `Step`, `Run`, `ApplyPolicy`, `HandleEnvelope`, `ResetCounters`, `Status`).
  A pushed policy is validated, persisted, applied live (detector progress kept). An invalid or older policy
  is rejected and the last-known keeps running; a bad file on disk is ignored (reported once). On start the
  last-known profile loads; if missing or invalid the baked-in default is used.
- `actuator.go`: `SystemActuator` (real): `systemctl set-property --runtime <unit> CPUQuota=N%` (transient),
  session cgroup `cpu.max`, `tc qdisc replace ... tbf` for egress, `cgroup.kill` (one write, whole cgroup; falls
  back to SIGKILL of `cgroup.procs`), `systemctl restart`, `systemctl reboot`. All through a `Runner` so the
  command lines are tested without root. Unit names are validated (no flag injection); session paths must sit
  under the session root.
- `internal/channel/envelope.go`: three small additions: `TypePolicy` (`PolicyPayload{Version, YAML}`),
  `TypePolicyAck` (`PolicyAckPayload{OK, Error, Version}`), `TypeWatchdogEv` (`WatchdogEventPayload`).
- `internal/config/config.go`: `WatchdogDirName`, and `AgentConfig.Watchdog` (`Groups`, `EgressInterface`).
- `deploy/systemd/fleet-watchdog.service` (`Restart=always`), `fleet-watchdog-liveness.timer`
  (`OnBootSec=1min`, `OnUnitActiveSec=1min`) and its one-shot `fleet-watchdog-liveness.service` (starts the
  watchdog if not active and logs it).

**Not fighting the Load Controller (D-0005):** the policy carries `load_controller_ceiling` (default 90/90) and
`Validate` rejects a machine CPU/memory `trigger` at or below it, or a `recover` below it, so a Maneuver sitting
at its ceiling can neither start nor hold a breach (default triggers are 97 CPU / 96 memory). Covered by a test
that feeds readings at the ceiling for 5 minutes.

**Decisions and things to know**
1. No YAML library is in `go.mod` and `go.mod` is not in `touches`, so `yaml.go` parses a small, documented YAML
   subset (block maps, scalar lists, flow maps/lists, comments, quoted strings) and rejects everything else
   (anchors, tags, multi-line scalars, lists of maps, duplicate keys, tabs). If the supervisor prefers
   `gopkg.in/yaml.v3` (v3.0.1 is in the module cache), only `parseYAML` needs replacing.
2. Disk breaches are alert-only (reported, no ladder step): throttling, killing or rebooting does not free disk.
3. Throttle uses `systemctl set-property --runtime` for services (a transient drop-in) rather than
   `systemd-run --scope`, which only wraps new processes; sessions use their cgroup's `cpu.max`.
4. When the offender is a session or unattributed, "service restart" restarts `agent_unit`
   (default `fleet-agent.service`), which also ends all sessions.
5. A reset keeps the cooldown timestamps (it clears counters, alert-only and the stage), so a reset cannot allow
   an immediate second restart or reboot.
6. Not wired (outside `touches`): the `fleet-agent watchdog` subcommand the service unit names, the agent's
   `policy` envelope hook (`Watchdog.HandleEnvelope`, or `StorePolicy` + `PolicyAck` when the watchdog loop runs
   in a separate process, as the units imply), request of the policy on connect, and forwarding `Notify` events as
   `watchdog-ev` (`EventEnvelope`). The server-side policy store and panel UI are later tasks.
7. The units cannot detect a hung (not exited) watchdog beyond `is-active`; that matches the brief
   (11.7: timer checks the unit is active).

**Tests** (`-race`, ran with `-count=5`): fed sustained breach climbs the ladder in order (nothing at 15 s,
throttle on the 4th sample, then kill, restart, reboot, then alert-only and silence); flap (3 high, 1 low) never
breaches; hysteresis band holds a breach; throttle lifted at expiry and early on recovery; 10 min cooldown after
throttle; counters, cooldowns and alert-only persist across a simulated reboot (new Watchdog on the same data
dir), midnight auto-reset, operator reset; boot window; forced alert-only; skip of inapplicable rungs; disk
alert-only; failed reboot still counted; corrupt counters -> alert-only; pushed policy through the real
envelope types applied live, 12 kinds of invalid/stale policy rejected with the last-known still running and
persisted; on-disk pickup; overrides layering; the run loop; YAML subset and rejections; real sampler on this
host (`/proc`, netlink, statfs) and on a fake cgroup tree; actuator command lines and cgroup file writes in
temp dirs. Root-gated: `TestSystemctlAvailableAsRoot` SKIPS off-root (printed `SKIPPED (root-gated)`), and runs
only `systemctl --version` as root; no test throttles, restarts or reboots anything.

**Self-check:** `gofmt -l .` empty; `go vet ./...` clean; `go test -race -count=5 ./internal/watchdog` ok;
`scripts/test.sh agent` pass; `scripts/build.sh` ok (bin/ removed afterwards).

<!-- wf: commits=PENDING left=- tests=agent:pass blocked=no -->
