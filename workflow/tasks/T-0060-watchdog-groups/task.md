---
id: T-0060
title: watchdog-groups
state: ready
touches: [internal/registry/registry.go, internal/onboard/ssh.go, internal/watchdog/watchdog.go,
          internal/panel/api.go, internal/panel/watchdog.go, web/src/pages/WatchdogPage.jsx,
          web/src/pages/MachinesPage.jsx, internal/panel/watchdog_test.go]
tests: [server]
after: [T-0047, T-0050]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Machines can be assigned to one or more watchdog groups, and the watchdog's per-group policy overrides (overrides.groups) actually apply to those machines. Group assignment is settable from the panel (and stored in the registry); the watchdog resolves a machine's effective policy using its groups. No change to the policy YAML format."
---

## Why

Watchdog policy already parses per-group overrides (`internal/watchdog/policy.go:420-444`,
`internal/watchdog/watchdog.go:37` reads `Config.Groups`), but **nothing assigns machines to groups** — the
registry has no group field and there's no onboarding/panel control (audit GAP-31; §11.6
"per-host and per-group overrides"). Add machine group membership so group overrides take effect.

## What to build

1. **Registry field.** Add `Groups []string` (`json:"groups,omitempty"`) to `registry.Machine`
   (`internal/registry/registry.go`); persist/reload with the registry; validate group names.
2. **Policy resolution.** In `internal/watchdog/watchdog.go` (and the panel's effective-policy computation,
   `internal/panel/watchdog.go`), resolve a machine's effective policy using its groups + per-host overrides
   (precedence: host > group > default — confirm against the existing override logic).
3. **Assignment UI + API.** Add a panel way to set a machine's groups — a `POST /api/machines/{id}/groups`
   (or extend an existing machine update) in `internal/panel/api.go`, surfaced on the React Machines page
   (`web/src/pages/MachinesPage.jsx`, e.g. in the row menu or the details view) and reflected on the React
   Watchdog page's per-host rows (`web/src/pages/WatchdogPage.jsx`). Optionally allow setting groups at
   onboarding (`internal/onboard/ssh.go`).
4. Rebuild + commit the web bundle (`./web/build.sh`).

## Out of scope

- No change to the policy YAML schema or the `overrides.groups` parsing (it already works); no new watchdog
  actions; no monitor-visibility/persistence work (that's GAP-35/T-0061).

## Done means

- A machine can be put in/removed from groups via the panel; the assignment persists; the watchdog applies
  the matching `overrides.groups` policy to that machine; the panel shows a machine's groups + effective
  policy.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` passes
  with tests for group assignment persistence + group-override resolution; bundle rebuilt + committed.

## Facts this brief relies on

- Audit: GAP-31 — `internal/watchdog/policy.go:420-444`, `internal/watchdog/watchdog.go:37` (groups parsed/
  read), `internal/registry/registry.go:90-130` (no group field), `internal/onboard/ssh.go:788-805`.
  Watchdog panel + effective policy: `internal/panel/watchdog.go`. React Watchdog/Machines pages
  (T-0050/T-0047): `web/src/pages/{WatchdogPage,MachinesPage}.jsx`.

## Notes for the doer

- Confirm the override precedence against the existing policy code before wiring resolution. Rebuild +
  commit the bundle. Run `./scripts/wf start T-0060` before committing; commit inside touches + the log. Do
  NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
