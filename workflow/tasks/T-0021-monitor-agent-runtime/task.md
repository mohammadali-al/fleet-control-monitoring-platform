---
id: T-0021
title: monitor-agent-runtime
state: ready
touches: [cmd/fleet-agent/main.go, internal/agent/monitor.go, internal/agent/monitor_test.go,
          internal/fleet/fleet.go, internal/config/config.go]
tests: [server, agent]
after: [T-0010, T-0018]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Onboard a monitor agent with targets: it shows online and monitor data starts flowing (visible once the Monitor page is wired, T-0023)."
---

## Why

T-0010 built the probe engine + monitor jobs + ingest as a library, and T-0018 left `--role monitor`
as a stub. Wire the monitor-agent runtime: run the probe loop, flush monitor data over the channel,
and receive pushed monitor config (targets/frequency/timeouts). This makes monitor agents actually
work end-to-end.

## What to build

**Backend (Go).**

1. **Monitor runtime** (`internal/agent/monitor.go`): on `--role monitor`, the agent connects over the
   channel (like the worker runtime) and runs `internal/probe` for its assigned targets; it flushes
   results every 2–3 s over the channel (`mon-data`), buffering locally during outages (monitor agents
   **never stop**). It applies pushed monitor config (`mon-config`: targets + frequency + timeouts).
2. **Server side** (`internal/fleet/fleet.go`): when a **monitor** machine connects, push its
   `mon-config` (from `internal/monitor` jobs → `ConfigFor(agent)`), and ingest its `mon-data` via the
   `internal/monitor` `Ingestor` into the sink.
3. **`cmd/fleet-agent/main.go`**: `--role monitor` now runs the monitor runtime (remove the
   not-implemented stub); keep `--role worker` as-is.

## Out of scope

- Worker-role changes (T-0018); the panel Monitor page (T-0023); a real Prometheus/VictoriaMetrics
  sink (use the existing `Sink` interface).

## Done means

- In-process via `internal/fleet/fleettest`: a monitor agent connects, is reported online, receives its
  pushed config, and its probe results arrive at the server ingest/sink; a buffered backlog flushes on
  reconnect.
- `cmd/fleet-agent --role monitor` runs + shuts down cleanly; `--role worker` unchanged.
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server`+`agent` pass with `-race` on
  `internal/agent` and `internal/fleet`.

## Facts this brief relies on

- Monitor probe loop + flush every 2–3 s: `Fleet_Control_Monitoring_Platform_Architecture.md:340`, `:363`.
- Monitor profile enables the probe loop, disables the executor: `Fleet_Control_Monitoring_Platform_Architecture.md:197`.
- Probe engine + jobs + ingest (`ConfigFor`, `Ingestor`, `Sink`) and `mon-config`/`mon-data` types:
  `workflow/tasks/T-0010-monitors/task.md`.
- Agent runtime + config-push pattern to reuse: `workflow/tasks/T-0018-agent-wiring/task.md`.

## Notes for the doer

- Reuse the config-push pattern + `fleettest` harness from T-0018; don't fork a parallel runtime.
- Monitor agents need no root (probes are unprivileged, except raw ICMP — gate that as T-0010 does).
