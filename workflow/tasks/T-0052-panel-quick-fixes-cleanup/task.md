---
id: T-0052
title: backend-correctness-cleanup
state: merged
touches: [internal/fleet/open.go, internal/agent/evidence.go, internal/panel/evidence.go,
          deploy/systemd/fleet-watchdog-liveness.timer, internal/onboard/ssh.go, internal/watchdog/heartbeat.go,
          internal/panel/evidence_test.go]
tests: [server]
after: []
shares: {}
model: {claude: "sonnet high"}
operator_check: "Four backend correctness fixes land: (1) the main server's channel cert carries its IP in the SAN; (2) idle evidence-push respects the server FIFO queue, not just local sessions; (3) the watchdog liveness timer runs at 1min per spec; (4) the evidence API returns payload_head and stops over-counting shared-segment records. No behaviour regressions; existing tests pass."
---

## Why

Batch of cheap, back-end-only correctness gaps from the audit (the two front-end copy/stub items — GAP-10,
GAP-33 — are folded into the relevant React page work instead, to stay SPA-consistent). Pure server/agent
fixes, no front-end structure touched.

## What to build

1. **GAP-05 — server channel cert IP SAN.** `internal/fleet/open.go:136` issues the server's mTLS channel
   cert with `ca.IssuePeer(serverID, nil, …)` (nil IP SANs). Issue it with the server's IP in the SAN so
   clients validate "cert SAN matches the IP I connected to" (§4.4). Mirror how agent certs get IP SANs in
   `internal/onboard/ssh.go`. Keep backward-compatible with existing enrolled agents.
2. **GAP-18 — idle-push FIFO check.** `internal/agent/evidence.go:51-59` triggers idle evidence push on
   `len(w.live) == 0` only. Also require the server's FIFO queue to be empty before idle-pushing (§7.2
   "no session, queue empty"); thread the server-queue-empty signal through if not already available.
3. **GAP-32 — watchdog liveness cadence.** `deploy/systemd/fleet-watchdog-liveness.timer:12`
   (`OnUnitActiveSec=45s`), `internal/onboard/ssh.go:860,866`, and `internal/watchdog/heartbeat.go:30`
   (`HeartbeatCheckEvery = 45 * time.Second`) use 45 s. Change to **1 min** per §11.7 (keep the timer and
   the in-code check consistent).
4. **GAP-34 (backend) — evidence payload_head + shared-segment counts.** `internal/panel/evidence.go:108-146`:
   `evRecord` omits `payload_head` (add it to the record API response) and `evSessions` sums whole-segment
   `seg.Records` even when `seg.Shared` is true (count only the records belonging to the session, not an
   upper bound). The React Evidence page (ported later) will display `payload_head`.

## Out of scope

- No front-end structure changes (GAP-10 UI copy and GAP-33 Machines "Details" view are handled in the
  React page tasks). No new features; don't change the revocation model (D-0007) or enrollment.

## Done means

- Server channel cert includes the IP SAN; idle-push also gates on the empty server queue; liveness timer +
  in-code check both at 1 min; evidence record API includes `payload_head` and shared-segment record counts
  are exact (not upper-bound).
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok, `scripts/test.sh server` passes with new/
  updated tests for the cert SAN, the evidence count fix, and `payload_head` presence.

## Facts this brief relies on

- Audit evidence: GAP-05 `internal/fleet/open.go:136`; GAP-18 `internal/agent/evidence.go:51-59`;
  GAP-32 `deploy/systemd/fleet-watchdog-liveness.timer:12`, `internal/onboard/ssh.go:860,866`,
  `internal/watchdog/heartbeat.go:30`; GAP-34 `internal/panel/evidence.go:108-112,134-146`. Spec: §4.4,
  §7.2, §11.7, §6.3/§12.2#9 (`Architecture_Gap_Audit.md`).

## Notes for the doer

- Keep each fix isolated (separate commits is fine). The cert-SAN change must not break already-enrolled
  agents — verify the handshake still validates. Run `./scripts/wf start T-0052` before committing; commit
  inside touches + the log. Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:`
  (except via `wf start`).
