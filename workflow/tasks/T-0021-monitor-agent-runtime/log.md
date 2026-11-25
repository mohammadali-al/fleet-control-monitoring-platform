# T-0021 log

## Doer entry

**Built.**
- `internal/agent/monitor.go`: the monitor runtime (`agent.Monitor`, `NewMonitor`,
  `MonitorFromConfig`). It listens for the main server on the mTLS channel (same `channel.Listener`
  and `LoadTLS` as the worker, server_dial), asks for its config with `config-req` on connect, applies
  a pushed `mon-config` to a `probe.Engine` (`monitor.DecodeConfig` -> `Engine.Apply`, idempotent) and
  acks with `config-ack`, and flushes probe results as `mon-data` through `monitor.Flusher` every
  2-3 s. While the channel is down `Listener.Send` fails with `ErrNotConnected`, so the Flusher keeps
  the batches (oldest dropped only past its 100k cap, counted) and sends them in order on the first
  flush after reconnect. Probing/flushing run for the whole life of the process; a config-less or
  server-less monitor just has nothing to probe / buffers. No shell, no executor, no capture, no root.
- `internal/fleet/fleet.go`: `Config.Monitor` / `Manager.SetMonitor` take a `MonitorSource`
  (`ConfigFor` = `monitor.Jobs.ConfigFor`, `Ingest` = `monitor.Ingestor`). On `config-req` from a
  monitor machine the manager pushes `mon-config` (workers still get the gate secret/capture config);
  `mon-data` from a monitor machine goes to the ingest under the machine id of the verified link (never
  a batch field); `mon-data` from a worker is ignored. `PushMonitorConfigs(ctx)` re-pushes to connected
  monitors after jobs/settings change (for T-0023). Online state needed no change (the existing poller
  already reports monitors online).
- `cmd/fleet-agent/main.go`: `--role monitor` runs the monitor runtime (stub removed); the worker path is
  unchanged. A small `runner` interface lets both roles share the start/run/shutdown code.
- Test support (new files): `internal/fleet/fleettest/mtls_monitor.go` (`MTLS.StartMonitor`, the
  monitor analogue of `StartAgent`; it records the port in the same table so `Factory` dials it).
- `internal/config/config.go` needed no change.

**Tests.**
- `internal/agent/monitor_test.go` (real mTLS channel, real manager, real `monitor.Jobs`+`Ingestor`+
  `MemorySink`, real HTTP and TCP probes against a loopback server): `TestMonitorEndToEnd` (connects,
  reported online, receives its 3 targets, results reach the sink under agent id + history key,
  re-push of a settings change is applied); `TestMonitorBacklogFlushesOnReconnect` (main server
  stopped -> agent sees the disconnect, keeps probing, backlog builds, sink does not grow; a new
  manager connects -> online again, backlog flushed incl. outage-time results, none dropped, per-target
  order kept); `TestMonitorConfigProtocol` (config-req on connect, ack of good config, rejection of a
  broken one leaves the applied config unchanged, worker `run` ignored).
- `internal/fleet/monitor_test.go`: config pushed to monitors only (bound at `New` and via `SetMonitor`),
  `mon-data` ingested only for monitors under the verified id, no source is harmless,
  `PushMonitorConfigs` reaches connected monitors only and pushes `[]` for an empty target list.
- `cmd/fleet-agent/main_test.go`: `--role monitor` starts and shuts down cleanly (exit 0, "shut down");
  unonboarded monitor -> `machine_id` error and no "not implemented"; worker test unchanged in behaviour
  (now a shared helper).
- ICMP: the ICMP target in the end-to-end tests uses a fake always-OK prober registered through
  `MonitorConfig.Probers`, so no raw/ping socket is needed here; the live ICMP path stays gated by
  `probe.ICMPAvailable()` as in T-0010 (its test skips off-root). Nothing skipped in this task's tests.
- `gofmt -l .` empty; `go vet ./...` clean; `scripts/build.sh` ok; `scripts/test.sh server` and
  `scripts/test.sh agent` pass; `go test -race` on `internal/agent`, `internal/fleet`, `cmd/fleet-agent`
  passes (monitor tests also run with `-count=4 -race`).

**Notes / deviations.**
- Production wiring is not part of this task's touches: `fleet.Open` (internal/fleet/open.go) and
  `cmd/fleet-server` do not yet build a `monitor.Jobs` or a real sink, so a stock server pushes no
  `mon-config` and drops `mon-data` until something calls `Manager.SetMonitor` (T-0023 owns the jobs
  API/panel and is the natural place; the real VictoriaMetrics sink is out of scope). No patch was
  saved because that wiring depends on the T-0023 jobs/panel design.
- The server does not ack `mon-data`; a batch that the server fails to write (sink error) is logged and
  lost, while a batch that cannot be sent at all stays buffered. Acceptable until the real sink exists.
- The monitor's applied config is held in memory only: after an agent restart it probes nothing until the
  server connects and pushes (it re-requests on every connect). Persisting the last config is a possible
  follow-up.
- Flush interval is clamped to 2-3 s by `monitor.Flusher`, so the end-to-end tests wait a few seconds.

<!-- wf: commits=cc9a4f7 left=- tests=server:pass,agent:pass blocked=no -->
