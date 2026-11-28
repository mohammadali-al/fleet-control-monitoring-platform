---
id: T-0078
title: agent-resource-telemetry
state: ready            # draft | ready | running | blocked | review | changes | merged | closed
touches: [internal/channel/envelope.go, internal/channel/envelope_test.go,
          internal/agent/resource.go, internal/agent/worker.go, internal/agent/resource_test.go,
          internal/fleet/fleet.go, internal/fleet/resource_test.go,
          web/src/pages/MachinesPage.jsx, web/assets/dist/, web/dist/index.html]
tests: [agent, server]
after: [T-0053, T-0059]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The Machines page now shows each online worker agent's live CPU and memory usage (alongside the existing disk bar) — a small gauge/readout per machine that updates as the agent reports, and fuller detail in the machine details drawer. It survives nothing persistent (live only), degrades gracefully to 'unreported' for agents that don't send it, and adds no measurable load."
---

## Why

The panel surfaces per-machine **disk** usage live, but not **CPU or memory** — operators asked to see live
resource usage of the agent machines. The agent already measures CPU/mem, but only inside the **watchdog**,
which runs as a *separate process* (`fleet-watchdog.service`) purely to drive its escalation policy; those
readings are never sent to the server. This task adds a lightweight **machine-level resource telemetry
stream** from the worker agent to the server and a small live CPU/mem widget on the Machines page — built by
mirroring the existing **disk-state** telemetry end to end.

## What to build

### Backend — mirror the disk-state pipeline

1. **Envelope type + payload** (`internal/channel/envelope.go`). Add `TypeResourceState = "resource-state"`
   (agent → server) next to `TypeDiskState` (`:218`), and a `ResourceStatePayload` modeled on
   `DiskStatePayload` (`:222`): `CPUPercent float64`, `MemPercent float64`, `LoadPerCPU float64`,
   `EgressMbps float64` (omitempty), `At time.Time`. Keep it additive/backward-tolerant (older peers ignore
   unknown types, as the file notes). Cover encode/decode in `envelope_test.go`.

2. **Worker reporter** (`internal/agent/resource.go` new + wiring in `worker.go`). Mirror the disk monitor
   (`internal/agent/diskstate.go`): a `startResourceMonitor()` ticker (reuse / parallel the
   `diskReportInterval` cadence — a modest interval, ~10–15s) plus a `reportResourceOnConnect()`, both
   started where `startDiskMonitor` / `reportDiskOnConnect` are started in the worker. Sample **machine-level
   only** by reusing `watchdog.NewSystemSampler(...)` and taking `sample.Machine` (CPUPercent / MemoryPercent
   / LoadPerCPU / EgressMbps) — hold one sampler instance in the worker so CPU% deltas work between ticks
   (CPU% is a delta since the previous read). Send `TypeResourceState`. This is the worker's OWN lightweight
   sampling; it does NOT touch or depend on the watchdog process. On sample error, debug-log and skip (like
   disk).

3. **Server record** (`internal/fleet/fleet.go`). Add a `Resource *ResourceState` field to `MachineInfo`
   (`:162`, next to the disk fields) and a `recordResourceState(n, env)` dispatched from the envelope switch
   (`:608`, beside `TypeDiskState`), mirroring `recordDiskState` (`:1923`): validate ranges (0–100 for
   percents; non-negative), stamp `At` if zero, store the latest in memory, and `publishMachine` so the panel
   updates live. Keep it **in-memory / live-only** (not persisted to the registry — it's a fast-changing
   gauge, unlike disk's guard state). Tests in `internal/fleet/resource_test.go`: a valid payload updates
   MachineInfo + publishes; an out-of-range payload is rejected; `MachineInfo` carries it through to the
   panel JSON.

### Frontend — live CPU/mem widget (design-first)

4. **Machines page widget** (`web/src/pages/MachinesPage.jsx`). Add a compact live CPU + memory readout per
   machine, consistent with the existing disk bar, and fuller detail in the machine details drawer (next to
   the disk `DiskSection`). It arrives on `machine.resource` via `/api/machines` + `/ws/events` machine
   frames (adding the `MachineInfo` field auto-propagates — confirm). Show "unreported" when absent (older
   agents / monitor agents). This part is **design-first**: hand back a short design (where the gauge sits in
   the row vs drawer, its visual form — reuse the disk bar/meter pattern + theme tokens) in `review-1.md` for
   supervisor review, then implement on approval. Pull meter/gauge patterns from the reference repo.

5. **Rebuild + commit the SPA bundle** (`./web/build.sh`).

## Out of scope

- No change to the watchdog, its sampling, or its escalation policy; no second sampling process.
- No historical storage / time-series / charts of CPU-mem (live gauge only — a history view is a separate
  follow-up, cf. the monitor time-series work). No per-process or per-session breakdown.
- No new config/flag for the cadence unless trivial (a const like `diskReportInterval` is fine).
- Don't touch the Monitor or Watchdog pages.

## Done means

- An online worker agent reports CPU% / mem% / load every ~10–15s and on connect; the Machines page shows a
  live CPU+mem readout per machine and fuller detail in the drawer; absent data reads "unreported".
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok; `scripts/test.sh agent` and
  `scripts/test.sh server` pass (new envelope/agent/fleet tests included); `check-app` clean on the Machines
  page in light + dark; bundle rebuilt + committed.
- A `review-1.md`-approved widget design preceded the frontend code.

## Facts this brief relies on

- Disk telemetry to mirror: payload `internal/channel/envelope.go:218,222` (`TypeDiskState`,
  `DiskStatePayload`); worker reporter `internal/agent/diskstate.go:80` (`startDiskMonitor` ticker),
  `:99` (`reportDiskOnConnect`), `:113` (`sampleAndReportDisk`); server `internal/fleet/fleet.go:608`
  (dispatch), `:1923` (`recordDiskState`); `MachineInfo` `internal/fleet/fleet.go:162` (disk fields
  `:180-181`).
- CPU/mem source: `internal/watchdog/metrics.go:18` (`MachineMetrics{CPUPercent, MemoryPercent, LoadPerCPU,
  DiskPercent, EgressMbps}`), sampled by `watchdog.NewSystemSampler` (`internal/agent/watchdog_wire.go:73`).
  The watchdog is a SEPARATE process (`watchdog_wire.go:26`), so the worker must sample for itself — it does
  not read the watchdog's samples.
- `MachineInfo` is serialized to the panel via `/api/machines` and `/ws/events` machine frames, so a new
  field propagates without panel-server changes (confirm during build).
- Machines page disk rendering to sit beside: `web/src/pages/MachinesPage.jsx` (the drawer `DiskSection` ~
  `:140`, the row disk bar/`DISK` column). Committed bundle: `web/assets/dist/app.{js,css}`,
  `web/dist/index.html` (`web/build.sh`).

## Notes for the doer

- Run `./scripts/wf start T-0078` before committing; commit only inside `touches` (new files free) + append
  `log.md` with the `wf:` footer. Do NOT push/merge/branch-delete, edit `state:`, or touch
  decisions/generated/other tasks. A hook refusal means STOP and report.
- Backend is a faithful mirror of disk-state — keep it that shape. Frontend is design-first: review-1 → my
  approval → code. Reference UI patterns: `https://github.com/nextlevelbuilder/ui-ux-pro-max-skill`.
- Sample machine-level only; hold one `SystemSampler` so CPU deltas work; keep the cadence modest so it adds
  no measurable load. UI language English (D-0003).
