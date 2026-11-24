---
id: T-0010
title: monitors
state: merged
touches: [internal/probe/probe.go, internal/probe/http.go, internal/probe/tcp.go, internal/probe/icmp.go,
          internal/probe/dns.go, internal/probe/probe_test.go, internal/monitor/jobs.go,
          internal/monitor/ingest.go, internal/monitor/jobs_test.go, internal/config/config.go,
          internal/channel/envelope.go, go.mod, go.sum]
tests: [server, agent]
after: [T-0003]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

The monitor-agent half of the system: continuously probe target availability/response and stream
**monitor data** back, with stateful monitor jobs on the server. Independent of the worker evidence
chain, so it can proceed in parallel with that work.

## What to build

**Backend (Go).**

1. **Probe engine** (`internal/probe/`), monitor-agent side: a pluggable `Prober` per type —
   **HTTP(S)** (status, latency, TLS info), **TCP connect** (connect time, success), **ICMP** (RTT,
   loss), **DNS** (query latency, answer). Probe type chosen per target. **Frequency** 300 ms–several
   seconds (global default + per-target override, pushed from the server); **timeouts pushed from the
   server**, with agent-local fallbacks only if no config ever arrived. Monitor agents **never stop** —
   buffer results locally during outages.
2. **Monitor-data flow** (`internal/monitor/ingest.go`): results flush **every 2–3 s** over the channel;
   server ingests into a time-series store (Prometheus/VictoriaMetrics) behind a small interface (a
   simple local sink is fine for tests). Monitor data is a **separate path** from worker evidence logs.
3. **Monitor jobs** (`internal/monitor/jobs.go`), server side: a **stateful** job object (not bound to a
   test's duration). `ip` **required**; `origin` **optional** (`scheme://host:port`); **no duplicate job
   per `(ip, origin)`**; multiple paths of the same origin allowed; a unique `(ip, origin)` history.
   Assign 2–3 targets per monitor agent at creation. Persist jobs under `DataDir` (D-0006).

## Out of scope

- Panel UI / charts / command-overlay / semantic-zoom (front-end exists as a stub; wiring is T-0011).
- Standing up a real Prometheus/VictoriaMetrics deployment — code to the ingest interface; a test sink
  suffices here.
- Worker evidence capture (separate subsystem).

## Done means

- Each prober returns a correct result/latency against a local test target (httptest for HTTP, a local
  listener for TCP, loopback for ICMP where permitted — skip clearly if not, a test DNS resolver for DNS).
- Frequency + timeout are taken from server-pushed config, not agent constants; a buffered backlog
  flushes on reconnect.
- Jobs enforce `(ip, origin)` uniqueness and the required/optional field rules; persistence round-trips.
- `gofmt -l .` empty, `go vet ./...` clean; `scripts/test.sh server` and `scripts/test.sh agent` pass.

## Facts this brief relies on

- Probe types, frequency, server-pushed timeouts: `Fleet_Control_Monitoring_Platform_Architecture.md:342`, `:343`, `:344`.
- Monitor agents dedicated/headless, 2–3 targets, never stop: `Fleet_Control_Monitoring_Platform_Architecture.md:341`, `:340`.
- Stateful jobs; `(ip, origin)` uniqueness; ip required / origin optional: `Fleet_Control_Monitoring_Platform_Architecture.md:349`, `:351`.
- Flush every 2–3 s to a time-series store; separate from evidence: `Fleet_Control_Monitoring_Platform_Architecture.md:362`, `:363`, `:66`.
- Channel exists (T-0003): `workflow/tasks/T-0003-control-channel/task.md`. Data dir: `workflow/decisions/D-0006-data-directory.md`.

## Notes for the doer

- ICMP needs raw sockets/root; gate that prober's live test behind a capability check and skip clearly.
- Keep the time-series sink behind an interface so the real store is swapped in later without a rewrite.
- May add a small ICMP library and/or a TS client (`go.mod`/`go.sum`) — if offline, stop and report blocked.
