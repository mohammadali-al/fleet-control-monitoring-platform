---
id: T-0059
title: machine-details-view
state: merged
touches: [web/src/pages/MachinesPage.jsx, web/src/hooks/, internal/panel/api.go, internal/panel/machines.go,
          internal/panel/machines_test.go]
tests: [server]
after: [T-0047]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The Machines row 'Details' action opens a real per-machine details view (drawer/panel) showing the machine's full registry state — role, connect_mode, online/stage, public IP + freshness, disk state, cert/enrollment info, last-seen — instead of a stub toast. Data comes from the existing registry via a details endpoint or the existing machine payload; updates live."
---

## Why

The Machines row **Details** action is a stub (`web/pages/fragments.html:30` historically; now a stub in the
React `MachinesPage`). Operators have no way to see a machine's full state (audit GAP-33; §12.2#1 lists
"registry, online state, role, connect_mode, public IP, disk state"). Build a real details view on the
React Machines page.

## What to build

1. **Details view (React)** — in `web/src/pages/MachinesPage.jsx`, replace the `Details` stub with a real
   per-machine **drawer/side-panel** (or route-less modal) showing the machine's full state: id, role,
   `connect_mode`, online/reconnecting + watchdog stage, public IP + `public_ip_at`, disk state + meters,
   cert fingerprint / enrollment status, last-seen, addresses. Updates live via the existing `/ws/events`
   machine stream. Follow the existing page's hook/live pattern (`web/src/hooks/useMachines.js`,
   `web/src/lib/events.jsx`).
2. **Data source.** Prefer the machine object the list already has; if some fields aren't in the list
   payload, add a `GET /api/machines/{id}` details endpoint (`internal/panel/api.go` +
   `internal/panel/machines.go`) returning the full `registry.Machine` view (behind the panel auth like
   other machine routes). Don't expose secrets (no private keys).
3. Rebuild + commit the web bundle (`./web/build.sh`).

## Out of scope

- No new machine actions beyond viewing; no change to onboarding/enrollment/registry schema; no other
  pages. Keep it read-only (actions like re-provision/remove already exist in the row menu).

## Done means

- The Machines `Details` action opens a working details view with the machine's full registry state, live;
  no stub toast remains. If an endpoint was added, it returns the machine view (404 for unknown) behind auth
  and leaks no secrets.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` passes
  (endpoint test if added; markup/hook test for the details view); bundle rebuilt + committed.

## Facts this brief relies on

- Audit: GAP-33 (the `Details` stub). React Machines page + live hook: `web/src/pages/MachinesPage.jsx`,
  `web/src/hooks/useMachines.js`, `web/src/lib/events.jsx` (T-0047). Machine routes + registry view:
  `internal/panel/api.go:49-52`, `internal/registry/registry.go` (`Machine` incl. `PublicIP`/`PublicIPAt`,
  `DiskState` after T-0053). Public-IP refresh + disk already render on this page (T-0045/T-0053).

## Notes for the doer

- Reuse the fields the list already carries where possible; only add an endpoint if needed. Rebuild + commit
  the bundle. Run `./scripts/wf start T-0059` before committing; commit inside touches + the log. Do NOT
  push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
