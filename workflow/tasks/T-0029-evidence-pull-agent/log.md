# T-0029 evidence-pull-agent: log

## Entry 1: worker serves evidence sync

**What changed.**
- `internal/agent/evidence.go` (new): `worker.initEvidence` builds `evidence.NewAgent` over
  `DataDir/evidence/<machine>` (via `capture.EvidenceMachineDir`) with the worker's lazy channel sender
  and an `Idle` predicate; `handleEvidence` routes `ev-manifest-req` / `ev-range` / `ev-verify`;
  `startEvidence` runs the periodic idle push (`evidence.Agent.Run`); `offerEvidence` is the idle push at
  connection time.
- `internal/agent/worker.go`: `ev`/`evDone`/`live` fields; the evidence agent is created in `newWorker`
  (so both `agent.New` and `WorkerFromConfig`/`NewWithWatchdog` get it, and `cmd/fleet-agent` needed no
  change); `handle` routes the three `ev-*` types to it, ahead of the executor fan-out; `start` launches
  the push loop and `shutdown` waits for it; `onConnect` offers segments in a goroutine;
  `sessionStart`/`sessionEnd` keep a set of running sessions, which is "idle" = empty.
- `internal/agent/export_test.go` (new): `SetEvidencePushInterval` so tests can shorten the 1-minute push
  interval (`agent.Config` is in `agent.go`, outside touches, so there is no config field for it).
- `internal/agent/evidence_test.go` (new): real worker + `fleet.Manager` over mTLS (`fleettest.MTLS`),
  with an evidence archive bound via `SetObserve`; segments are written with `capture.SegmentWriter`
  (what the capture subsystem writes).

**Done means.**
- Server pull through the real agent runtime transfers the worker's segments with SHA-256 verify:
  `TestPullTransfersWorkerSegments` (160 x 4 KiB records, several segments, the first larger than one
  256 KiB chunk; archived files hash to the manifest digest and to the worker's files; a second pull
  skips all), `TestPullFetchesNewSegmentsOnly`, `TestPullFromWorkerWithNoEvidence` (empty manifest, no
  syncer timeout). Checked that these fail (20 s timeout) when the dispatch line is removed.
- An idle worker push delivers: `TestIdleWorkerPushDelivers` (no pull; archive fills, sync trigger
  `push`). A busy worker holds back until its command ends: `TestBusyWorkerDoesNotPush`.
- `gofmt -l .` empty; `go vet ./...` clean; `scripts/build.sh` ok; `scripts/test.sh server` and
  `scripts/test.sh agent` pass; `go test -race -count=5` on `internal/agent` and `cmd/fleet-agent` pass.

**Notes.**
- The sync endpoint reads the segment directory, not the capture subsystem, so it works when live capture
  is off (unprivileged agent): it serves existing segments, or an empty manifest.
- Idle = no running command session on the worker (set maintained by the executor's session hooks). A
  command between being accepted and its `running` announcement is not yet counted; the worst case is an
  offer the server can fetch harmlessly.
- Tests do not need cgroups or root (no command capture is exercised; live capture stays
  `TestLiveCaptureProducesRecords`, root-gated). The operator check (Evidence page, Pull against a real
  onboarded worker) is not run here.
- No patch needed; nothing outside `touches` changed.

<!-- wf: commits=PENDING left=- tests=server:pass,agent:pass blocked=no -->
