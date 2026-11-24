# T-0015 evidence-capture-fixes - log

## Doer entry

**Fix 1 - `full` records keep their payload.** `internal/capture/records.go`: `buildFromDecoded` now
copies the complete payload into `payload_head` when `payload_status` is `full` (payload length <=
threshold). `trimmed` is unchanged (first `threshold` bytes, threshold capped at 4 KB). The head is a
copy, not an alias of the packet buffer (the live loop reuses its read buffer). Empty payloads leave
`payload_head` empty. Field names/JSON unchanged; `omitempty` still omits it for zero-length payloads.
Export (T-0009) was not touched; it already reads `payload_head` when present.

**Fix 2 - concurrency-safe manifest.** Root cause: each `SegmentWriter` calls `LoadManifest`, so every
session held its own `Manifest` value (own mutex, own in-memory entries) over the same
`manifest.json`; the last writer's save silently dropped the others' entries. Fix in
`internal/capture/manifest.go`: a process-wide per-machine-directory mutex (`dirLock`, keyed by the
absolute cleaned dir) serializes `Manifest.Add`, which now re-reads the on-disk manifest under that
lock, appends, and saves via the existing temp-file + rename. So N writers on one machine dir never
lose entries, and `Entries()`/`Verify()` on any writer's manifest reflect all sessions after its next
`Add`. Readers (`LoadManifest` in `internal/evidence`) are unchanged.

Related hazard found and fixed in `internal/capture/segments.go`: segment names were
`seg-<unixnano>-<per-writer seq>`, so two concurrent writers opening a segment in the same nanosecond
would pick the same file and `O_TRUNC` it (data loss). The sequence is now a package-level atomic
counter (name format unchanged) and the file is opened `O_EXCL`.

**Tests.**
- `records_test.go`: full record's `payload_head` equals the payload (updated existing test);
  `TestPayloadHeadAtThresholdBoundary` (0, 1, threshold-1, exactly threshold = full with full head;
  threshold+1 and far above = trimmed with first-threshold head; sha/length of full payload; JSON
  round-trip); `TestFullPayloadHeadIsACopy`.
- `manifest_concurrency_test.go` (new): `TestConcurrentSessionsShareManifest` (8 writers, 60 records
  each, tiny `MaxBytes` so ~480 segments roll; asserts every segment file is indexed, no duplicates,
  record total, `Verify`) and `TestConcurrentManifestAdd` (10x25 adds via independent Manifests).
  Confirmed they FAIL on the old code (60 of 480 entries; 25 of 250) and pass on the fix, under `-race`.
- `internal/evidence/export_fullpayload_test.go` (new, tests in the agent group): a packet built via
  `capture.BuildRecord`, archived, exported to pcapng; the exported TCP payload equals the real payload
  and the capture length equals the original length.

Root-gated live capture path, source-IP, disk guard: untouched and not exercised (no root needed here).

Self-check (go1.26.8): `gofmt -l .` empty, `go vet ./...` clean, `scripts/build.sh` ok,
`scripts/test.sh agent` pass, `go test -race -count=3 ./internal/capture/` pass.

<!-- wf: commits=1305098 left=- tests=agent:pass blocked=no -->
