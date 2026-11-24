---
id: T-0015
title: evidence-capture-fixes
state: ready
touches: [internal/capture/records.go, internal/capture/records_test.go,
          internal/capture/segments.go, internal/capture/manifest.go, internal/capture/segments_test.go]
tests: [agent]
after: [T-0008]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

T-0009 surfaced two correctness gaps in the merged T-0008 evidence-capture. Fix them before the
evidence is relied on.

## What to build

**Backend (Go), `internal/capture`.**

1. **Store the full payload for `full` records.** Today `BuildRecord` writes `payload_head` only when a
   payload is **trimmed**, so a `full` (≤ trim-threshold) packet keeps only its SHA-256 + length and
   loses its bytes. Fix: a **`full` record retains its complete payload in `payload_head`**; a
   `trimmed` record keeps the first ≤4 KB as today. (Spec clarified: architecture §6.3 `payload_head`,
   row 8.) The export path already reads `payload_head` whenever present, so no export change is needed —
   but add/adjust tests so a `full` record's `payload_head` round-trips and an exported pcapng for a full
   packet carries the real payload.
2. **Make the per-machine manifest concurrency-safe.** T-0008 creates one `SegmentWriter` per session
   over the same machine directory + `manifest.json`; concurrent sessions would race on that manifest.
   Fix: serialize manifest updates (a per-machine lock, or a single manifest owner), so N concurrent
   capture sessions on one worker agent update the manifest without races or lost entries.

## Out of scope

- The live nftables/NFLOG capture path, source-IP, disk guard (unchanged) — only records + segments +
  manifest.
- Sync/export (T-0009) — do not change; it already consumes `payload_head` when present.

## Done means

- A `full` record's `payload_head` holds the complete payload; a trimmed one holds first ≤4 KB
  (unit-tested, incl. a boundary at exactly the threshold).
- Two (or more) concurrent `SegmentWriter`s on one machine directory update `manifest.json` with **no
  race and no lost segment entries** — covered by a `-race` test with concurrent sessions.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh agent` passes (and `-race` on
  `internal/capture`).

## Facts this brief relies on

- Record content: full payload (≤ threshold); above 4 KB → first 4 KB + count + SHA-256:
  `Fleet_Control_Monitoring_Platform_Architecture.md:55`.
- `payload_head` holds the full payload when `full`, first ≤4 KB when `trimmed` (clarified):
  `Fleet_Control_Monitoring_Platform_Architecture.md:278`.
- The gaps were found during T-0009: `workflow/tasks/T-0009-evidence-sync-export/log.md`.
- The capture package to fix: `workflow/tasks/T-0008-evidence-capture/task.md`.

## Notes for the doer

- Keep logic unit-testable without root (synthetic packets); do not touch the root-gated live path.
- Don't change the record's field names/JSON — only *populate* `payload_head` for full records.
