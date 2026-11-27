---
id: T-0053
title: agent-telemetry-completeness
state: ready
touches: [internal/channel/envelope.go, internal/agent/, internal/capture/, internal/monitor/,
          internal/fleet/fleet.go, internal/registry/registry.go, internal/panel/api.go,
          web/src/pages/MachinesPage.jsx, web/src/hooks/]
tests: [channel, capture, fleet, registry, panel, agent, monitor]
after: [T-0045, T-0047]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Worker agents report local disk usage/80%-90% guard state to the server, so the Machines page shows real disk state (not 'later task'); the public-IP cache also refreshes on an OS network-change event; monitor-profile agents also run the public-IP reflector; and command-runner session history survives a server restart. The React Machines page renders the live disk state. No architecture-doc behaviour beyond these telemetry paths."
---

## Why

Several agent→server telemetry paths are stubbed/partial (audit GAP-08/13/16/17): disk state is never
reported to the panel, the public-IP cache has no network-change trigger, monitor agents lack the reflector,
and command history is memory-only. Complete them. Front-end work is the **React** Machines page
(`web/src/pages/MachinesPage.jsx`) per the SPA (D-0011) — no legacy page structure.

## What to build

1. **GAP-17 — disk telemetry to panel.** The worker disk guard logs/pauses locally
   (`internal/capture/segments.go:181-203`) but never reports. Add a channel telemetry message (mirror the
   public-IP reporting pattern from T-0045 in `internal/channel/envelope.go` + the `internal/fleet/fleet.go`
   dispatch) carrying disk usage + 80%/90% state; store on `registry.Machine.DiskState`
   (`internal/registry/registry.go:59-61`); render it on the React Machines page (the page already has disk
   meter cells — feed them real data via the live `/ws/events` machine updates).
2. **GAP-16 — public-IP network-change trigger.** `internal/capture/srcip.go:240-260` refreshes on boot +
   15 min + manual only. Add an OS network-change trigger (netlink/route-change on Linux) that calls the
   existing refresh; keep it best-effort (note+skip if unavailable), don't regress the other triggers.
3. **GAP-13 — monitor-agent reflector.** The public-IP reflector is worker-only
   (`internal/capture/srcip.go`); monitor-profile agents (`internal/agent/monitor.go`) don't run it (§5
   lists it for monitors too). Instantiate the reflector/cache on the monitor profile and report its IP the
   same way as workers.
4. **GAP-08 — persistent command history.** Command-runner history is in-memory, capped and lost on restart
   (`internal/fleet/fleet.go:166,932-934`, `maxSessions = 500`). Persist session history to disk under the
   data dir and reload on start so the re-run history survives restarts (keep the in-memory fast path).

## Out of scope

- No Load Controller / Maneuver work; no new UI pages (only wire real data into the existing React Machines
  page); don't change the capture algorithm or evidence sync. Monitor data/history store is a separate task.

## Done means

- Disk state + public-IP are reported over the channel and shown live on the React Machines page; the
  public-IP cache also refreshes on network change (best-effort); monitor agents run the reflector; command
  history persists across a server restart.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok, `scripts/test.sh` passes for the touched
  groups, with tests for the disk telemetry round-trip (agent→registry), the reflector on the monitor
  profile, and command-history persistence/reload. Rebuild the web bundle (`./web/build.sh`) if JSX changed.

## Facts this brief relies on

- Audit evidence: GAP-17 `internal/capture/segments.go:181-203`, `internal/registry/registry.go:59-61`;
  GAP-16 `internal/capture/srcip.go:240-260`; GAP-13 `internal/agent/monitor.go`, `internal/capture/srcip.go`;
  GAP-08 `internal/fleet/fleet.go:166,932-934`. Telemetry/report pattern to mirror: T-0045 (public-IP), the
  envelope + `internal/fleet/fleet.go` dispatch switch. SPA Machines page + live hook: `web/src/pages/MachinesPage.jsx`,
  `web/src/hooks/useMachines.js`, `web/src/lib/events.jsx` (T-0047).

## Notes for the doer

- Reuse the T-0045 public-IP reporting/dispatch pattern for disk telemetry — keep the channel
  backward-tolerant (older peers ignore unknown types). Rebuild + commit the web bundle if you touch JSX.
  Run `./scripts/wf start T-0053` before committing; commit inside touches + the log. Do NOT
  push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
