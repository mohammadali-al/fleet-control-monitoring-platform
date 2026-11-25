---
id: T-0029
title: evidence-pull-agent
state: merged
touches: [internal/agent/evidence.go, internal/agent/evidence_test.go, internal/agent/worker.go,
          cmd/fleet-agent/main.go]
tests: [server, agent]
after: [T-0009, T-0018]
shares: {}
model: {claude: "sonnet high"}
operator_check: "On the Evidence page, Pull against a real onboarded worker actually transfers its segments (no more syncer timeout)."
---

## Why

T-0009 built evidence sync and T-0023 wired the server/panel **pull**, but the **agent runtime never
serves it** — `evidence.NewAgent` isn't run by `internal/agent`/`cmd/fleet-agent`, so a pull against a
real worker ends in the syncer timeout. Wire the agent side.

## What to build

**Backend (Go), worker agent.**

1. **Evidence sync agent** (`internal/agent/evidence.go`): on the worker runtime, run
   `evidence.NewAgent` over the segments the capture subsystem writes under `DataDir/evidence/<machine>/`,
   and serve the sync protocol (`ev-manifest`/`ev-range`/`ev-chunk`/`ev-verify`) over the channel.
2. **Dispatch** (`internal/agent/worker.go`): route the `ev-*` envelopes to the evidence agent (alongside
   the existing run/capture handling). Keep idle-push behavior as T-0009 defines (push when idle).
3. Ensure `cmd/fleet-agent` worker role starts it (it already has the data dir + channel).

## Out of scope

- The evidence sync/export logic (T-0009, unchanged); monitor role; the panel (already wired, T-0023).

## Done means

- In-process `fleettest` (server + worker over mTLS): a server **pull** transfers the worker's segments
  with SHA-256 verify (the same guarantees T-0009 tested in-process, now through the real agent runtime);
  an idle worker push also delivers.
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server`+`agent` pass with `-race` on `internal/agent`.

## Facts this brief relies on

- Evidence sync protocol (pull + idle push, manifest/range/chunk/verify) and `evidence.NewAgent`:
  `workflow/tasks/T-0009-evidence-sync-export/task.md`.
- The worker runtime + capture attach (where segments come from) to extend: `workflow/tasks/T-0018-agent-wiring/task.md`;
  `workflow/tasks/T-0008-evidence-capture/task.md`.
- The gap was flagged in: `workflow/tasks/T-0023-panel-live-wiring/log.md`.

## Notes for the doer

- Reuse the T-0018 worker runtime + `fleettest`; if `agent.New`/`worker.go` needs a hook you can't add
  inside touches, save a `.patch` and report it (as T-0022 did for the watchdog handler).
