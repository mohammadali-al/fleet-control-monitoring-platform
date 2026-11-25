---
id: T-0030
title: onboarding-install-watchdog
state: merged
touches: [internal/onboard/ssh.go, internal/onboard/ssh_test.go,
          internal/enroll/install.go, internal/enroll/install_test.go]
tests: [server]
after: [T-0012, T-0020, T-0022, T-0026, T-0027]
shares: {}
model: {claude: "sonnet high"}
operator_check: "After onboarding (SSH or token) a worker and a monitor, the fleet-watchdog service + its liveness timer are installed, enabled and running on both."
---

## Why

T-0012 built the watchdog + its systemd units and T-0022 added the `fleet-agent watchdog` subcommand,
but **onboarding never installs the watchdog service/timer** — both the SSH path (`ssh.go`) and the token
installer (`internal/enroll/install.go`) install only `fleet-agent.service`. So the watchdog doesn't run
on onboarded machines. Install it.

## What to build

**Backend (Go), both onboarding paths.**

1. **SSH onboarding** (`internal/onboard/ssh.go`): also install + enable **`fleet-watchdog.service`**
   (runs `fleet-agent watchdog`) and its liveness timer (and the agent-liveness timer from T-0027),
   on **both worker and monitor** agents (the watchdog runs **full on workers, headless/stripped on
   monitors** — architecture §11.1). Keep `Restart=always`, enable-at-boot; `Remove` disables them too.
2. **Token installer** (`internal/enroll/install.go`): the systemd (Linux) / launchd (macOS) install sets
   up the watchdog service + liveness the same way. (On macOS the watchdog's Linux-only actions are
   inert; keep it headless there — or document that the macOS monitor runs no watchdog if that's simpler
   and note it.)

## Out of scope

- The watchdog logic/policy (T-0012/T-0022); heartbeat (T-0027, just install its timer); the Load-
  governance behavior.

## Done means

- Both onboarding paths install + enable the watchdog service and the liveness timer(s); the onboarding
  tests assert the written units (worker gets full, monitor gets the headless/stripped setup); `Remove`
  disables them.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes.

## Facts this brief relies on

- Watchdog runs on both roles (full on worker, headless/stripped on monitor), `fleet-watchdog.service`,
  `Restart=always`: `Fleet_Control_Monitoring_Platform_Architecture.md:506`.
- The units + subcommand exist: `workflow/tasks/T-0012-watchdog/task.md`; `workflow/tasks/T-0022-watchdog-agent-wiring/task.md`.
- The onboarding paths that currently install only the agent unit: `workflow/tasks/T-0020-onboarding-agent-privileges/task.md`;
  `workflow/tasks/T-0026-token-enrollment/task.md`. Liveness timers: `workflow/tasks/T-0027-agent-heartbeat-liveness/task.md`.

## Notes for the doer

- Reuse the existing unit-install helpers; tests run against the in-process SSH server / installer and
  assert the rendered units (no real machine). Decide and document the macOS watchdog stance.
