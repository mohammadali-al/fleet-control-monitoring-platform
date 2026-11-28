---
id: T-0068
title: agent-self-update
state: merged
touches: [internal/channel/envelope.go, internal/agent/, internal/fleet/, internal/registry/registry.go,
          internal/enroll/server.go, internal/panel/api.go, internal/panel/machines.go,
          web/src/pages/MachinesPage.jsx, web/src/hooks/, internal/panel/machines_test.go]
tests: [server, agent]
after: [T-0026, T-0059]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Each agent reports its fleet-agent version, shown on the Machines page/details; outdated agents are flagged against the server's current version. The operator can trigger an update per-machine or fleet-wide; the agent downloads the matching fleet-agent-<os>-<arch> binary from the server (channel/binary endpoint), SHA256-verifies it, atomically replaces its binary, restarts its service, reconnects on the new version, and reports success/failure (with rollback if it can't come back). No manual re-provisioning needed to update an already-installed agent."
---

## Why

A main-server upgrade refreshes the agent binaries the server *serves to new onboardings* but does **not**
update already-installed agents — they keep their old version until manually re-provisioned (there's no
agent-version reporting and no update mechanism). Add **push-triggered agent self-update** over the existing
mTLS control channel so the fleet can be updated from the panel.

## What to build

**Report agent version:**
1. Agents report their `fleet-agent` version (`internal/version.Version`) to the server — add an
   `AgentVersion` field to `registry.Machine` (`internal/registry/registry.go`) and report it on connect
   (mirror the public-IP/disk telemetry pattern; a channel field or a small telemetry envelope). Surface it
   on the React Machines page/details (`web/src/pages/MachinesPage.jsx`, reuse the T-0059 details view),
   flagging agents whose version != the server's current.

**Update protocol + self-update:**
2. **Envelopes** (`internal/channel/envelope.go`): a server→agent `TypeAgentUpdate` (target version +
   checksum/where-to-fetch) and an agent→server `TypeAgentUpdateResult` (downloading/applying/ok/failed) —
   mirror the `TypeServiceControl`/`...Result` pattern; backward-tolerant.
3. **Server side** (`internal/fleet/`, `internal/enroll/server.go`): serve the matching
   `fleet-agent-<os>-<arch>` binary for the agent's platform (reuse the existing binary-serving mechanism,
   `enroll.BinaryDir`) + its SHA256; a `Manager` method to send `TypeAgentUpdate` to a machine (or all
   online worker agents).
4. **Agent side** (`internal/agent/`, new `update.go` + wiring): on `TypeAgentUpdate`, fetch the matching
   binary from the server, **SHA256-verify**, **atomically replace** its own binary, and **restart its
   service** (systemd) so it reconnects on the new version; report progress/result. **Safety:** verify
   before swap; keep the old binary for **rollback** if the new agent fails to reconnect within a timeout;
   coordinate with the watchdog (the update restart must not trip a watchdog escalation — use/annotate the
   watchdog-managed restart or a grace window). No-op if already on the target version.

**Panel:**
5. `internal/panel/api.go` + `machines.go`: `POST /api/machines/{id}/update` (and an "update all outdated"
   action); show version + an **Update** control on the Machines page/row/details. Rebuild + commit the web
   bundle (`./web/build.sh`).

## Out of scope

- No downgrade/pinning policy UI, no staged/canary rollout orchestration (update-all is fine), no
  monitor-agent-specific logic beyond the same mechanism, no change to onboarding/enrollment of *new*
  machines. Keep the channel backward-tolerant (an old agent that doesn't understand `TypeAgentUpdate`
  simply ignores it).

## Done means

- Agents report their version (shown + outdated-flagged on the panel); an operator-triggered update (per
  machine or fleet-wide) makes the agent download + SHA256-verify + atomically replace its binary + restart
  + reconnect on the new version and report success; a failed/unreachable update rolls back to the old
  binary; already-current agents are a no-op; the watchdog doesn't escalate on the update restart.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` + the
  agent group pass, with tests for: version reporting → registry, the update envelope round-trip, and the
  self-update apply/verify/rollback logic (with a fake binary + checksum). Web bundle rebuilt + committed.

## Facts this brief relies on

- No agent-version field today (`internal/registry/registry.go` has `OSVersion` host-spec only, no
  `AgentVersion`); agents don't report their version. Server already serves `fleet-agent-<os>-<arch>`:
  `internal/enroll/server.go:263-267` (`BinaryDir`), binaries under `ServerConfig.AgentBinaryDir`. Control
  pattern to mirror: `internal/channel/envelope.go:135-143` (`TypeServiceControl`/`...Result`,
  service actions); telemetry-report pattern: public-IP (T-0045) / disk (T-0053) in `internal/fleet/fleet.go`.
  Agent version: `internal/version.Version`. Watchdog restarts the agent service (coordinate): T-0027/T-0061.
  Machines details view to surface version: `web/src/pages/MachinesPage.jsx` (T-0059).

## Notes for the doer

- **Large — split into commits** (version report; envelopes; server send+serve; agent self-update+rollback;
  panel UI) and report the split. The risky part is the **atomic self-replace + restart + rollback** and
  **watchdog coordination** — be careful an in-progress update can't brick an agent (verify-before-swap,
  keep old binary, reconnect-or-rollback). Rebuild + commit the bundle. Run `./scripts/wf start T-0068`
  before committing; commit inside touches + the log. Do NOT push/merge/branch-delete or edit
  decisions/generated/other tasks/`state:` (except via `wf start`).
