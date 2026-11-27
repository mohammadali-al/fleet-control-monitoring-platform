---
id: T-0058
title: monitor-page-react
state: merged
touches: [web/src/pages/MonitorPage.jsx, web/src/routes.js, web/src/hooks/,
          internal/monitor/jobs.go, internal/panel/monitor.go, internal/panel/monitor_test.go]
tests: [server]
after: [T-0047, T-0055]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The Monitor page is a real React route (live probe charts via /ws/monitor + /api/monitor/live), not a legacy fallback. Operators can create DNS probe targets (and pick probe type) — not just icmp/tcp/http (GAP-19). The command-overlay timeline shows per-interval packet counts on hover and no longer over-counts shared segments (GAP-22). Grafana embed + long-term History/semantic-zoom remain separate follow-ups."
---

## Why

Monitor is still a legacy-fallback route and two monitor functions are missing: DNS probe creation (GAP-19 —
the engine supports DNS but job creation only builds icmp/tcp/http and the UI has no probe-type selector)
and per-interval packet counts in the command overlay (GAP-22 — hover omits packet counts and sums whole
shared segments). Port Monitor to React (D-0011) and add both. **Grafana embed (GAP-01) and the long-term
History/semantic-zoom UI (GAP-23) are separate tasks** — not here.

## What to build

1. **Port Monitor to React** — `web/src/pages/MonitorPage.jsx` + set its `routes.js` entry
   `component: MonitorPage` (replace `null`): live probe series/charts fed by `/ws/monitor` + `GET
   /api/monitor/live`, job list/create, at parity with the current page (keep the existing live window;
   don't build the History tab here). Follow the T-0047 route-component contract.
2. **GAP-19 — DNS probe targets.** `internal/monitor/jobs.go:565-575` (`initialTargets`) only creates
   `icmp`/`tcp`/`http`; add `dns` target creation (the prober exists: `internal/probe/dns.go`,
   `internal/panel/monitor.go:267`). Add a **probe-type selector** to the React job-create form so operators
   can choose the probe type (incl. DNS) per target.
3. **GAP-22 — overlay packet counts.** `internal/panel/monitor.go:164-206`: include **per-interval packet
   counts** in the command-overlay hover data, and fix `archivedPackets` so it counts only the session's
   records in a shared segment (not the whole segment, which over-counts). Render the per-interval counts in
   the React overlay hover.
4. Rebuild + commit the web bundle (`./web/build.sh`).

## Out of scope

- No Grafana embed (GAP-01), no History/semantic-zoom UI (GAP-23), no confirm-to-start dialog (GAP-20); no
  change to the time-series store (T-0055) or the probe engine internals; no other pages.

## Done means

- `/monitor` is a React route with live charts + job create (incl. DNS probe type); DNS targets actually
  probe; the overlay hover shows per-interval packet counts and shared-segment counts are exact.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` passes
  with tests for DNS target creation and the per-interval/shared-segment count fix; bundle rebuilt + committed.

## Facts this brief relies on

- Audit: GAP-19 `internal/monitor/jobs.go:565-575`, `internal/probe/dns.go:20-24`, `internal/panel/monitor.go:267`;
  GAP-22 `internal/panel/monitor.go:164-206`, `web/assets/js/live.js:1592-1622` (legacy hover, parity ref).
  SPA route pattern: `web/src/routes.js`, `web/src/pages/MachinesPage.jsx`, `web/src/lib/events.jsx`
  (T-0047). Live monitor APIs + `/ws/monitor` and the history read `GET /api/monitor/history` (T-0055).

## Notes for the doer

- Copy the T-0047 route-component contract exactly; keep the live window behaviour. Rebuild + commit the
  bundle. Run `./scripts/wf start T-0058` before committing; commit inside touches + the log. Do NOT
  push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
