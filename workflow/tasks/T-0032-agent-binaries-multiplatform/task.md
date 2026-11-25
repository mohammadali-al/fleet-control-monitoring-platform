---
id: T-0032
title: agent-binaries-multiplatform
state: merged
touches: [scripts/build.sh, scripts/build-agents.sh, internal/enroll/install.go,
          internal/enroll/install_test.go, internal/onboard/ssh.go, internal/onboard/ssh_test.go,
          internal/config/config.go, deploy/README.md]
tests: [server]
after: [T-0006, T-0026]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Enrolling/onboarding a linux-arm64 or macOS (intel/apple-silicon) machine serves the matching agent binary; a mismatched platform is a clear error, not a broken install."
---

## Why

Onboarding/enrollment serves a single-platform agent binary today (`agent_binary_path` = the server's own
platform). To onboard agents of different OS/arch we must build and serve the right binary per target.
Approved targets: **linux amd64, linux arm64, macOS amd64, macOS arm64**.

## What to build

**Build + serving (mostly ops, small Go).**

1. **Cross-compile** (`scripts/build-agents.sh`, called by/with `scripts/build.sh`): build `fleet-agent`
   for the four targets (CGO off) into an **`agent_binary_dir`** as `fleet-agent-<os>-<arch>`
   (e.g. `fleet-agent-linux-arm64`, `fleet-agent-darwin-arm64`). Reproducible, same ldflags as the main build.
2. **Serve the right one:** the **token installer** (`internal/enroll/install.go`) and **SSH onboarding**
   (`internal/onboard/ssh.go`) select the binary for the target's OS/arch from `agent_binary_dir`
   (config: prefer `agent_binary_dir`, keep `agent_binary_path` as a single-platform fallback). A target
   with no matching binary is a **clear error** before anything is installed.
3. **Docs** (`deploy/README.md`): how to build all agent binaries and where they live.

## Out of scope

- New runtime features; capture is Linux-only (a macOS worker is already refused, T-0026) so macOS
  binaries are for **monitor** agents.

## Done means

- `scripts/build-agents.sh` produces the four `fleet-agent-<os>-<arch>` binaries (verify with `go build`
  for each GOOS/GOARCH; CGO off); the main build still works.
- The installer/onboarding pick the binary matching the target OS/arch; a missing one errors clearly
  (tested); README documents the build.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes.

## Facts this brief relies on

- Enrollment serves per-OS/arch binaries from a dir; `agent_binary_path` is single-platform:
  `workflow/tasks/T-0026-token-enrollment/task.md` (gap in its log).
- SSH onboarding uses the agent binary path: `workflow/tasks/T-0006-machine-registry-onboarding/task.md`.
- macOS is monitor-only (capture Linux-only): `Fleet_Control_Monitoring_Platform_Architecture.md:261`;
  `workflow/decisions/D-0001-stack.md`.

## Notes for the doer

- Pure cross-compiles (CGO off) — no C toolchain needed. Don't commit the built binaries (gitignore
  `agent_binary_dir` output); the script produces them. Keep selection logic in one helper reused by both
  onboarding paths.
