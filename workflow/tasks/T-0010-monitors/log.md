# T-0010 monitors - log

## Doer entry

Built the probe engine, the monitor-data flow and the stateful monitor jobs. Nothing outside `touches`.

**Files**
- `internal/probe/probe.go`: `Prober` interface, `Target`, `Result`, server-pushed `Config`
  (global interval + per-type timeouts, per-target overrides), and the `Engine` (one loop per
  target, reconciles on every `Apply`, never exits except on ctx cancel; probe failures are results).
- `internal/probe/{http,tcp,icmp,dns}.go`: the four probers. HTTP: status, latency, TLS info
  (version, cipher, subject, issuer, NotAfter, SANs); OK = status < 400, redirects not followed.
  TCP: connect time. ICMP: `golang.org/x/net/icmp`, echo RTT + loss, unprivileged datagram socket
  first (Linux `ping_group_range`), raw socket as fallback. DNS: query latency + answers, optional
  explicit resolver `host:port`.
- `internal/monitor/ingest.go`: agent-side `Flusher` (Add = engine callback; flush every 2-3 s,
  interval clamped to that range; failed send keeps the backlog and retries next tick, oldest-first,
  bounded by a 100k-result cap that drops oldest and counts them), `Batch` wire shape, `Sink`
  interface + `MemorySink`, server-side `Ingestor` (labels each sample with the verified agent id
  and the `(ip, origin)` history key; ignores non-monitor envelopes), `ConfigEnvelope`/`DecodeConfig`.
- `internal/monitor/jobs.go`: stateful `Jobs` (ip required, origin optional `scheme://host:port`
  normalised with default port, one job per `(ip, origin)`, several paths per job via `AddPath`,
  history per `(ip, origin)` kept even across delete/recreate), target assignment to monitor agents
  (packs up to 3 per agent), `ConfigFor(agent)` builds the pushed `probe.Config`, server `Settings`
  (global interval, per-type timeouts, per-target interval override). Persists to
  `<DataDir>/monitor/jobs.json` (atomic write; memory rolled back if the save fails).
- `internal/channel/envelope.go`: two types only, `mon-config` (server -> agent) and `mon-data`
  (agent -> server).
- `internal/config/config.go`: `MonitorDirName = "monitor"`.
- `go.mod`: `golang.org/x/net` moved from indirect to direct (already in the module cache and
  go.sum; no fetch needed). The DNS test server uses `x/net/dns/dnsmessage`.

**Decisions**
- Agent-local fallbacks (HTTP 2 s, TCP 1.5 s, ICMP 1 s, DNS 2 s, interval 1 s) only fill a value the
  pushed config left out; the server `Settings` default to the same numbers but are server-owned.
- Agent identity on `mon-data` is the channel's verified peer passed to `Ingestor.Handle`, never a
  field in the payload.
- Delivery is at-most-once per flush: a send that "succeeds" on the websocket but is lost with the
  connection is not re-sent (no ack in this cut). The backlog during an outage is not lost.
- The local buffer is in memory (capped). D-0006 allows `monitor/` for local buffers; persisting the
  buffer across an agent restart is not done here.
- Probe targets in a job: ICMP to the ip; with an origin also TCP to the origin port and one HTTP
  probe per path. HTTP/TCP connect to the job's ip but keep the origin host for Host header and SNI.
- Assignment packs agents (fill one to 3 before using the next) so agents carry 2-3 targets.

**Not done (out of scope / wiring)**: panel, charts, main.go wiring of Flusher/Engine/Ingestor into
the agent and server processes, real Prometheus/VictoriaMetrics sink.

**Done means**
- Probers vs local targets: HTTP (httptest, plain and TLS), TCP (local listener), DNS (local UDP test
  resolver) all pass. ICMP loopback test ran and PASSED here (unprivileged ping socket available:
  `ping_group_range` is open); it is gated by `probe.ICMPAvailable()` and skips with a clear message
  where no ICMP socket can be opened. Not exercised as root / raw socket.
- Frequency and timeout come from the pushed config: engine tests show a 300 ms push gives ~300 ms
  gaps, a pushed 5 s override slows the loop, a pushed 100 ms HTTP timeout fails a 400 ms response
  (the 2 s fallback would succeed) and a pushed 3 s makes it succeed; removing a target stops it.
- Buffered backlog flushes on reconnect: `TestBacklogFlushesOnReconnectInOrder`.
- Jobs: ip required, origin optional, duplicate `(ip, origin)` rejected (also across spelling),
  persistence round-trips and survives reload (uniqueness, ids, history).
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/build.sh` ok, `scripts/test.sh server` and
  `scripts/test.sh agent` pass; `go test -race` on the two new packages passes (go1.26.8).

<!-- wf: commits=93dae27 left=- tests=server:pass,agent:pass blocked=no -->
