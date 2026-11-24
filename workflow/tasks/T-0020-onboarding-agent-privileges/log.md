# T-0020 log

## Entry 1 — doer (sonnet)

**Built.** `renderUnit` in `internal/onboard/ssh.go` now writes a unit that runs the agent as root
(`User=root`, `Group=root`) so capture, PTY-as-root, cgroup tracking and the watchdog get the
privileges they need (architecture §5). The data dir is set explicitly with
`Environment=FLEET_DATA_DIR=<Layout.StateDir>` (default `/var/lib/fleet-agent`, also the
`WorkingDirectory`), so evidence/registry no longer depend on the binary's location (D-0006).
Restart=always, RestartSec, After/Wants network-online, enable + restart are unchanged.

**Choices.**
- Same unit for worker and monitor profiles (the code has one unit); the monitor runs as root too, for
  simplicity.
- The dedicated `fleet-agent` user is kept only to own the config/state directories and config file
  (`stepEnsureUser`/`stepCreateDirs` untouched); it no longer runs the service. Root can read them.
- `StateDirectory=` not used: it is relative to `/var/lib` and would conflict with a non-default
  `Layout.StateDir`; `stepCreateDirs` already creates the directory. `FLEET_DATA_DIR` follows the Layout.
- The unit has no liveness timer in the current code; nothing to preserve there.

**Tests.** `ssh_test.go` onboarding test now asserts `User=root`, `Group=root`,
`Environment=FLEET_DATA_DIR=/var/lib/fleet-agent`, `Restart=always`, the ExecStart line, and that the
unit does not run as `fleet-agent`. `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server`
passes. Real-machine validation (capture no longer "off", a command yields session records) is left to
the supervisor.

<!-- wf: commits=cd42291 left=- tests=server:pass blocked=no -->
