---
id: T-0045
title: public-ip-refresh
state: merged
touches: [internal/channel/envelope.go, internal/capture/srcip.go, internal/capture/capture.go,
          internal/agent/, internal/fleet/fleet.go, internal/fleet/observe.go,
          internal/panel/api.go, internal/registry/registry.go,
          web/pages/machines.html, web/pages/fragments.html, web/assets/js/live.js]
tests: [channel, capture, fleet, registry, panel, agent]
after: [T-0023]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Each worker agent reports its reflector-derived public IP to the server over the control channel; the registry's PublicIP/PublicIPAt are populated and shown on the Machines page; and the panel's 'Refresh public IPs' button (page-level) + the per-row refresh button trigger an immediate reflector re-check on the agent that updates the panel live. The stub toast is gone. No stub-only buttons remain for this feature."
---

## Why

The architecture (§6.4 + Machines page) specifies the agent's public IP is resolved by reflector
consensus and **refreshed at boot, every 15 min, on network-change, and via a manual panel button**, with
the Machines page showing public IP with manual refresh. The agent side already exists (reflector cache +
`capture.Controller.RefreshPublicIP()` + the 15-min loop) and the registry already has the fields — but
nothing **reports** the IP to the server or **wires** the button, so the panel shows a stub toast
("needs the reflector telemetry … a later task"). Build the missing wiring end-to-end. (This was a task
left un-briefed, not a deferral the operator approved.)

## What to build

**Backend (Go):**
1. **Channel envelopes** (`internal/channel/envelope.go`): add `TypePublicIP` (agent → server: the
   current public IP + timestamp; add a `PublicIPPayload{IP string; At time.Time; Confirmed bool}`-style
   struct) and `TypePublicIPRefresh` (server → agent: trigger an immediate reflector refresh). Mirror the
   existing typed envelopes + payloads (e.g. `TypeServiceControl`/`...Result`, `TypeWatchdogReset`).
2. **Agent** (`internal/agent/`, new `publicip.go` + minimal wiring): report the public IP over the
   channel **on connect**, **when it changes** (and/or each 15-min refresh), using
   `capture` cache `Current()`/`Result()` (`internal/capture/srcip.go:206,213`). Handle an inbound
   `TypePublicIPRefresh` by calling the controller's `RefreshPublicIP()` (`internal/capture/capture.go:115`)
   / `cache.RefreshNow(ctx)` and then reporting the fresh result. Expose a getter on the capture
   `Controller` if the agent can't reach the cache result today.
3. **Server dispatch** (`internal/fleet/fleet.go`): add `case channel.TypePublicIP:` in the inbound
   switch (alongside `TypeMonData`/`TypeWatchdogEv`/`TypeServiceControlResult`, ~`:421-446`) → update the
   registry (`m.PublicIP`, `m.PublicIPAt` via `reg.Update`, fields at `internal/registry/registry.go:93-95`)
   and push the change to panel observers (same path the other telemetry uses, see
   `internal/fleet/observe.go`). Add a method to **send** `TypePublicIPRefresh` to a given agent (mirror
   the service-control / watchdog-reset send path in the same file).
4. **Panel endpoint** (`internal/panel/api.go` + a handler): `POST /api/machines/{id}/refresh-public-ip`
   (reuse the `s.live(...)` wrapper like the other machine routes at `:49-52`) → call the fleet
   send-refresh method for that machine (online only; clear error if offline). The page-level "Refresh
   public IPs" button may call it for each online machine, or add a bulk route — your call, keep it simple.

**Frontend:**
5. **Wire the buttons, remove the stub** (`web/pages/machines.html:10` page button;
   `web/pages/fragments.html:26` per-row button): drop the `data-stub="..."`, make them POST the refresh
   endpoint (via the existing live/app JS patterns). Show the public IP + its freshness on the row
   (fields now populated). Update `web/assets/js/live.js` so the registry/public-IP updates render live
   and the button triggers the request (don't touch the separate enroll-button logic at `live.js:353`).

## Out of scope

- Don't change the reflector algorithm, the capture source-IP logic, or how records store the cached IP
  at capture time (already per spec). No new agent config surface beyond what's needed. Don't touch the
  enroll button / token-enrollment. No new panel pages — restyle nothing; this is a wiring + one column.
- Network-change event detection is nice-to-have: boot + 15-min + manual-button are the required refresh
  triggers; if network-change signal isn't already available, note it and skip (don't build OS netlink
  plumbing here).

## Done means

- A worker agent reports its public IP on connect and on refresh; `fleet-server` stores it in the registry
  (`PublicIP`/`PublicIPAt`) and streams it to the panel; the Machines page shows it.
- The page-level **and** per-row refresh buttons trigger an immediate agent reflector re-check that
  updates the panel live; the stub toast is gone; offline machines give a clear "agent offline" response,
  not a crash.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok, and `scripts/test.sh` passes for the
  touched groups (channel, capture, fleet, registry, panel, agent) with new tests covering: envelope
  round-trip, the fleet `TypePublicIP` → registry update, and the refresh endpoint (online + offline).

## Facts this brief relies on

- Reflector cache + refresh API already exist: `internal/capture/srcip.go:192,206,213,221,233`
  (`NewPublicIPCache`/`Current`/`Result`/`RefreshNow`/`TriggerRefresh`);
  `internal/capture/capture.go:109,115` (`Start` loop "boot + every 15 min + on demand", `RefreshPublicIP`).
- Registry already has the fields: `internal/registry/registry.go:93-95` (`PublicIP`, `PublicIPAt`).
- Server inbound envelope dispatch + registry updates: `internal/fleet/fleet.go:421-446`; panel push:
  `internal/fleet/observe.go`. Panel machine routes: `internal/panel/api.go:49-52`.
- Typed-envelope pattern to mirror: `internal/channel/envelope.go` (`TypeServiceControl`/`...Result` at
  `:128-129`, `TypeWatchdogReset` at `:122`, `TypeMonData` at `:57`).
- The stub buttons to replace: `web/pages/machines.html:10` (page) and `web/pages/fragments.html:26`
  (per-row), both carrying `data-stub`. Architecture: §6.4 + Machines-page manual-refresh requirement.

## Notes for the doer

- Broad but mechanical — **split into logical commits** (channel+payload; agent report+handle; fleet
  dispatch+send; panel endpoint; front-end wiring) and report the split. Preserve every existing id/class/
  `data-*` the live JS + tests rely on. Keep the control-channel change backward-tolerant (an agent that
  never sends `TypePublicIP` must not break the server; an older server must ignore unknown types). If the
  server-side send path or observer push lives somewhere other than the cited files, extend there and note
  it (stay within the same packages / `touches` dirs; a change outside → save a `.patch` + report). Run
  `./scripts/wf start T-0045` before committing; commit inside touches + the log. Do NOT push/merge/
  branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`). Hook refusal =
  stop + report verbatim.
