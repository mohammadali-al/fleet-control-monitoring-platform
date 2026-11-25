# T-0032 agent-binaries-multiplatform: log

## 2026-11-25: multi-platform agent build, shared selection helper, SSH onboarding + installer support (go1.26.8)

**Built (inside `touches`).**

- **`scripts/build-agents.sh` (new) + `scripts/build.sh`:** Added `scripts/build-agents.sh` to cross-compile `./cmd/fleet-agent` (`CGO_ENABLED=0`, `-trimpath`, same commit-derived version `-ldflags` via `scripts/build.sh --ldflags`) for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64` into `bin/agents/fleet-agent-<os>-<arch>` (or a caller-supplied directory). Updated `scripts/build.sh` so a default run calls `scripts/build-agents.sh`, `agents` builds only the multi-platform agents, and `fleet-server`/`fleet-agent` single-target builds keep working. `bin/` is already gitignored (`.gitignore`).
- **`internal/config/config.go` + `internal/config/agent_binary_test.go` (new):** Added the shared helper `SelectAgentBinary` (plus `NormalizeAgentPlatform`, `AgentBinaryName`, and `ErrAgentBinaryUnavailable`) that prefers `<agent_binary_dir>/fleet-agent-<os>-<arch>` (non-empty regular file), falls back to `agent_binary_path` only when target OS/arch matches the host, and returns a clear error otherwise.
- **`internal/onboard/ssh.go` + `internal/onboard/ssh_test.go`:** Added `Provisioner.AgentBinaryDir` and optional `Target.OS`/`Target.Arch`. When target platform is not specified on `Target`, `Provision` probes `uname -sm` over SSH before modifying remote state or writing to the registry, resolves the binary via `config.SelectAgentBinary`, and fails with `ErrAgentBinaryMissing` before any install step runs if no matching binary is available. Added tests covering all four platforms, preference of `AgentBinaryDir` over `AgentBinaryPath`, and early clear errors before install.
- **`internal/enroll/install.go` + `internal/enroll/install_test.go`:** Added a non-empty binary guard (`[ -s "$WORK/fleet-agent" ]`) after fetching `/enroll/agent?os=$OS&arch=$ARCH` and added tests verifying `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64` binary selection plus clear HTTP 404 failure before key generation, CSR submission, or file installation when a target binary is missing.
- **`deploy/README.md`:** Documented `scripts/build.sh` / `scripts/build-agents.sh`, the `bin/agents/fleet-agent-<os>-<arch>` output layout, and `server.agent_binary_dir` vs `server.agent_binary_path`.

**Outside `touches` (`.patch` files saved under `workflow/tasks/T-0032-agent-binaries-multiplatform/`).**

- **`exec-cgroup-darwin.patch`:** `internal/exec/session.go` currently fails to compile for `GOOS=darwin` (`attrs.UseCgroupFD` / `attrs.CgroupFD` on `syscall.SysProcAttr` are Linux-only). The patch splits `Cgroup.attach` into `internal/exec/session_linux.go` and `internal/exec/session_other.go`.
- **`fleet-open-agent-binary-dir.patch`:** Wires `AgentBinaryDir: o.Config.AgentBinaryDir` into `onboard.Provisioner` in `internal/fleet/open.go`.
- **`enroll-server-select-binary.patch`:** Makes `enroll.BinaryDir` in `internal/enroll/server.go` delegate directly to `config.SelectAgentBinary` so both paths execute the exact same function.

**Verification.**

- *With the three `.patch` files temporarily applied:* `gofmt -l .` empty; `go vet ./...` clean; `./scripts/build.sh` built `bin/fleet-server`, `bin/fleet-agent`, and all four `bin/agents/fleet-agent-<os>-<arch>` targets; running `scripts/build-agents.sh` twice produced identical SHA-256 digests for all four binaries; `./scripts/test.sh server` passed across all packages.
- *On the clean branch (only `touches` committed; `git apply --check` clean on all three `.patch` files):* `gofmt -l .` empty; `go vet ./...` clean; `./scripts/build.sh fleet-server`, `./scripts/build.sh fleet-agent`, `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build`, and `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build` pass; `./scripts/test.sh server` passes. Cross-compiling `darwin/amd64` and `darwin/arm64` (and therefore running `scripts/build-agents.sh` / full `scripts/build.sh`) is blocked on `internal/exec/session.go` until `exec-cgroup-darwin.patch` is applied by the supervisor.

wf: T-0032 done
