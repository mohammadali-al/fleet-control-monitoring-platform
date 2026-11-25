## 2026-11-25: T-0030 onboarding-install-watchdog (Go 1.26.8)

**What changed.**
- `internal/onboard/ssh.go`:
  - `stepCreateDirs` creates `<StateDir>/watchdog` (`0750`, owned by `ServiceUser`).
  - `stepWriteConfig` seeds `<StateDir>/watchdog/policy.yaml` (`0640`, owned by `ServiceUser`) for `monitor` targets with a stripped headless policy (`version: 1`, `load_controller_ceiling: {cpu_percent: 0, memory_percent: 0}`, `steps: {throttle: false, kill_session: false, restart: true, reboot: true}`); `worker` targets receive no pre-seeded policy and continue using server-pushed policy / `DefaultPolicy`.
  - `stepInstallUnit` installs `fleet-watchdog.service` (`Restart=always`, `RestartSec=2`, `OOMScoreAdjust=-900`, `Nice=-5`, `User=root`, `Environment=FLEET_DATA_DIR=<StateDir>`; worker gets full `Description=Fleet watchdog (load-safety net)` while monitor gets `Description=Fleet watchdog (headless)` + `Environment=FLEET_WATCHDOG_MODE=headless`), plus `fleet-watchdog-liveness.service` and `fleet-watchdog-liveness.timer` (`45s` cadence, matching `deploy/systemd/`).
  - `stepEnableStart` enables and restarts `fleet-watchdog.service`, and enables `--now` both `fleet-agent-liveness.timer` and `fleet-watchdog-liveness.timer`.
  - `Remove` disables `--now` both liveness timers (`fleet-agent-liveness.timer`, `fleet-watchdog-liveness.timer`) and stops/disables `fleet-watchdog.service` (best-effort on `*RemoteError` for pre-T-0030 machines) before stopping/disabling `fleet-agent.service`.
- `internal/enroll/install.go`:
  - Added `RenderWatchdogSystemdUnit(role)`, `MonitorWatchdogPolicyYAML`, `RenderAgentLivenessService`, `RenderAgentLivenessTimer`, `RenderWatchdogLivenessService`, and `RenderWatchdogLivenessTimer`.
  - `RenderScript` Linux branch now installs `fleet-watchdog.service` (full for `worker`, headless/stripped + `/var/lib/fleet-agent/watchdog/policy.yaml` for `monitor`), `fleet-agent-liveness.{service,timer}`, and `fleet-watchdog-liveness.{service,timer}`, reloads systemd, enables/restarts `fleet-watchdog.service`, and enables `--now` both liveness timers.
  - Kept `StopCommand` unchanged so `cmd/fleet-server/enroll_test.go` and `internal/enroll/enroll_test.go` remain intact.

**macOS watchdog stance.**
- macOS agents are monitor-only (`CheckRoleOS` refuses `worker` on `darwin`) and run **no** `fleet-watchdog` launchd service. Watchdog host sampling (`/proc/stat`, `/proc/meminfo`, `/proc/loadavg`, netlink) and actuation (`systemctl`, cgroups, `tc`) are Linux-only (`internal/watchdog/metrics_other.go` returns `errUnsupported`), so running `fleet-agent watchdog` on macOS would fail every 5 s sample; launchd's `RunAtLoad` + `KeepAlive` on `com.fleet.agent` already restarts the agent if it exits.

**Checks.**
- `gofmt -l .`: empty
- `go vet ./...`: clean
- `./scripts/test.sh server`: pass (all packages)
- `go test -race ./internal/onboard/... ./internal/enroll/...`: pass (new tests in `ssh_test.go` and `install_test.go` assert worker=full vs monitor=headless/stripped units, `watchdog.ParseProfile` validity of the monitor policy, parity with `deploy/systemd/fleet-*-liveness.*`, Linux script execution for both roles, and `Remove` disabling timers + services).

wf: T-0030 done
