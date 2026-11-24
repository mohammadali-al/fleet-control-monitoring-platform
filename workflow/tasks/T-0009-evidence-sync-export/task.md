---
id: T-0009
title: evidence-sync-export
state: merged
touches: [internal/evidence/manifest.go, internal/evidence/sync.go, internal/evidence/archive.go,
          internal/evidence/export.go, internal/evidence/sync_test.go, internal/evidence/export_test.go,
          internal/channel/envelope.go, go.mod, go.sum]
tests: [server, agent]
after: [T-0008]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

Gets the captured evidence from each worker agent to the main server and out to the customer: the
checksum-diff **delta sync** (pull + idle push), the server **archive**, and **export** to pcapng /
CSV / JSON. Completes the evidence pipeline begun in T-0008.

## What to build

**Backend (Go), both sides**, over the channel (T-0003), on the segments + manifest from T-0008.

1. **Delta sync** (`internal/evidence/sync.go`):
   - **Pull** (main server, per machine): compare the server's manifest copy against the agent's →
     request only **missing/changed** segments → **verify each SHA-256** on arrival → update the server
     manifest. **Interrupted transfers resume mid-segment.**
   - **Idle push** (agent): when idle (no session, queue empty) proactively push pending segments, using
     the **same** protocol as pull.
   - Add the needed transfer message types to `internal/channel/envelope.go` (manifest request/response,
     segment range request, chunk, verify) — small additions.
2. **Archive** (`internal/evidence/archive.go`, main server): store pulled segments under
   **`DataDir/archive/<machine>/<session>/`** (D-0006), plaintext, unlimited retention, mirroring the
   segment structure. Server **disk guard**: 80 % warn / 90 % pause intake (§7.3).
3. **Export** (`internal/evidence/export.go`): from archived records —
   - **pcapng** (via `gopacket` pcapng writer): regenerate packets; trimmed payloads reconstructed as
     far as stored, the record's SHA-256 testifying to the original.
   - **CSV / JSON**: flat record export.
   - Scope selector: **per session, per machine, or per time range**.

## Out of scope

- Capture / record production (T-0008) — consume its segments + manifest.
- The backup **server** replication (§7.5 is a separate later task) — this is agent→main-server sync.
- Panel UI / HTTP endpoints (later wiring task); exercise via tests + the channel.

## Done means

- Pull transfers only missing/changed segments, verifies every SHA-256, and a transfer interrupted
  mid-segment **resumes** without re-downloading completed segments; a corrupted segment is rejected.
- Idle push delivers the same segments by the same protocol; a busy agent does not push.
- The server archive lays out segments per machine/session; the disk guard pauses intake at 90 %.
- Export produces a valid **pcapng** (opens in a standard reader), and CSV/JSON matching the records,
  for a chosen session / machine / time range.
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server` and `scripts/test.sh agent` pass.

## Facts this brief relies on

- Evidence stays local as segment files; panel **pull** does checksum-diff sync; agents **idle push**:
  `Fleet_Control_Monitoring_Platform_Architecture.md:62`, `:309`, `:310`, `:34`.
- Pull verifies SHA-256 and resumes interrupted transfers: `Fleet_Control_Monitoring_Platform_Architecture.md:309`.
- Export pcapng + CSV/JSON; archive retains metadata to regenerate both: `Fleet_Control_Monitoring_Platform_Architecture.md:319`, `:320`, `:57`.
- Segments + manifest produced by T-0008: `workflow/tasks/T-0008-evidence-capture/task.md`.
- Archive under the base data dir: `workflow/decisions/D-0006-data-directory.md`.
- pcapng via `gopacket`: `Fleet_Control_Monitoring_Platform_Architecture.md:604`.

## Notes for the doer

- Uses `github.com/google/gopacket` for the pcapng writer (may already be present from T-0008); if a
  module download is needed and the sandbox is offline, stop and report blocked.
- Tests run both ends in-process over a loopback channel with synthetic segments/records (D-0002); verify
  resume by cutting a transfer mid-segment and reconnecting.
- Keep pull and push on one protocol (the brief requires it); don't fork two code paths.
