---
id: T-0020
title: onboarding-agent-privileges
state: merged
touches: [internal/onboard/ssh.go, internal/onboard/ssh_test.go]
tests: [server]
after: [T-0006, T-0018]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Onboard a real worker: after provisioning, evidence capture works (the agent no longer logs 'capture is off'); a command produces session records. (Supervisor can validate on a real machine.)"
---

## Why

T-0018 found the onboarded agent runs as a **non-root** user (the systemd unit T-0006 writes uses a
dedicated user), so commands/streaming/gate work but **evidence capture, PTY-as-root, and cgroup
tracking are off** on a real worker. The architecture requires the agent to run as **root** (§5). Fix
the onboarding unit so a provisioned worker agent actually has the privileges capture needs, and lands
its data where expected.

## What to build

**Backend (Go), `internal/onboard`** — the systemd unit(s) written during provisioning.

1. **Run the worker-agent service as root.** The `fleet-agent.service` (worker profile) must run with
   the privileges capture/PTY/cgroup/watchdog need — i.e. **as root** (architecture §5). Drop the
   non-root `User=` for the worker agent (or keep the dedicated user only for data-dir ownership, not
   for running the service). The monitor-agent service (headless watchdog, no capture) may also run as
   root for simplicity — document the choice.
2. **Set the data dir consistently** so the agent writes evidence/registry where intended: set it
   explicitly in the unit (e.g. `Environment=FLEET_DATA_DIR=/var/lib/fleet-agent` with
   `StateDirectory=fleet-agent`, or the project's chosen path) so it no longer depends on the binary's
   location (D-0006's "one level above the binary" resolves to an unwritable path under the service).
3. Keep everything else about the unit (Restart=always, the liveness timer, enable+start) intact.

## Out of scope

- The capture/watchdog code itself (works given privileges) — this is only the unit/privileges.
- Hardening beyond what capture needs; `agent_dial`; monitor-role runtime.

## Done means

- The generated worker `fleet-agent.service` runs the agent **as root** (no non-root `User=` that
  blocks capture), with the data dir set explicitly in the unit.
- The onboarding tests assert the unit's privilege + data-dir settings; existing onboarding tests still
  pass (the in-process SSH server checks the written unit content).
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes.
- (Real validation: on a provisioned worker, the agent's capture factory no longer reports "capture is
  off" and a command yields session records — the supervisor confirms on a real machine.)

## Facts this brief relies on

- **Agent requires root** (capture, PTY, watchdog, cgroup): `Fleet_Control_Monitoring_Platform_Architecture.md:249`.
- Onboarding installs the systemd units + a dedicated user: `Fleet_Control_Monitoring_Platform_Architecture.md:195`.
- The non-root gap was found in T-0018: `workflow/tasks/T-0018-agent-wiring/log.md`.
- The onboarding code that writes the unit (T-0006): `workflow/tasks/T-0006-machine-registry-onboarding/task.md`.

## Notes for the doer

- Keep the change to the unit template + its tests; don't touch the agent runtime or capture.
- Tests run against the in-process SSH server (D-0002), asserting the installed unit's content; no real machine.
