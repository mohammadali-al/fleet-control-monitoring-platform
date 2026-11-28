---
id: T-0070
title: agent-private-ip-report
state: merged
touches: [internal/channel/envelope.go, internal/agent/, internal/capture/, internal/fleet/fleet.go,
          internal/registry/registry.go, web/src/pages/MachinesPage.jsx, internal/panel/machines_test.go]
tests: [server, agent]
after: [T-0045, T-0053, T-0068]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Each agent reports its PRIMARY private IP — the address of the interface on the default route (the source IP used for outbound/internet traffic) — to the server, and the Machines page shows BOTH private and public IP (e.g. 'Private 10.0.0.5 · Public 46.34.170.235 · 12h ago') instead of 'No address'. Works regardless of how the machine was onboarded (SSH or token-enrolled). Updates live."
---

## Why

The Machines row shows "No address" for the private IP: `registry.Machine.PrivateIPs` is only populated
from **onboarding-provided** dial addresses, so a machine enrolled without a known LAN IP (e.g. token
enrollment) has none. The agent should report its own **primary private IP** (the default-route
interface's source IP) so both private and public are always shown.

## What to build

1. **Agent determines + reports its primary private IP.** The primary private IP = the source IP of the
   interface on the **default route** (what an outbound connection would use). Reuse the existing route/
   source-IP logic in `internal/capture/` (the source-IP resolver / route NIC lookup) if it exposes a
   "default route source IP"; otherwise add a small helper. Report it over the channel (mirror the
   public-IP/disk telemetry — add it to that report payload or a small net-info report) on connect and when
   it changes.
2. **Server stores it.** Add a `PrimaryPrivateIP string` field (`json:"primary_private_ip,omitempty"`) to
   `registry.Machine` (keep the existing `PrivateIPs` onboarding list as-is), updated by the fleet dispatch
   (`internal/fleet/fleet.go`, like `recordPublicIP`/`recordDiskState`). Validate it's a real IP.
3. **Panel shows both.** `web/src/pages/MachinesPage.jsx`: in the row (and details), show **both** the
   private IP (prefer `primary_private_ip`, else the first `private_ips`) **and** the public IP — e.g.
   `Private <ip> · Public <ip> · <ago>` — replacing the "No address" when a private IP is known. Updates
   live via `/ws/events`. Rebuild + commit the web bundle (`./web/build.sh`).

## Out of scope

- No change to onboarding's `PrivateIPs` capture or the server_dial dial logic; no per-interface listing UI
  (just the primary); no IPv6-vs-IPv4 preference policy beyond "the default-route source IP" (note which if
  the OS gives both). Reuse the telemetry/report pattern — don't invent a new transport.

## Done means

- Agents report their primary private IP; `registry.Machine.PrimaryPrivateIP` is populated; the Machines
  row + details show both private and public IP (no "No address" when a private IP is known), live.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` + the
  agent group pass, with tests for the private-IP report → registry and the panel rendering both. Web
  bundle rebuilt + committed.

## Facts this brief relies on

- `registry.Machine.PrivateIPs` is onboarding dial addresses only (`internal/registry/registry.go:121-122`),
  empty for some enrollments → "No address". Row/details render: `web/src/pages/MachinesPage.jsx:226,602-620`
  (`private_ips`, `public_ip`). Telemetry-report pattern to mirror: public-IP (T-0045) + disk (T-0053) in
  `internal/fleet/fleet.go` dispatch + `internal/channel/envelope.go`. Route/source-IP logic:
  `internal/capture/srcip.go` (source-IP resolver / route NIC). Runs after T-0068 (shares envelope/fleet/
  registry/MachinesPage).

## Notes for the doer

- Keep the channel backward-tolerant. Rebuild + commit the bundle. Run `./scripts/wf start T-0070` before
  committing; commit inside touches + the log. Do NOT push/merge/branch-delete or edit decisions/generated/
  other tasks/`state:` (except via `wf start`).
