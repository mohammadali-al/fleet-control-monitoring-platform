# T-0055 log

## Doer entry

**Store choice & rationale.**
- Chose **(a) VictoriaMetrics (`victoria-metrics-prod` v1.157.0 static binary)** over a custom embedded Go TSDB.
- Why: Architecture §8.4/§15 and D-0001 specify Prometheus/VictoriaMetrics. VictoriaMetrics ships as a single static Go binary with zero external dependencies and no Docker required, matching the native-proxy pattern (D-0009, standalone Caddy/Authelia binaries + systemd units). It natively provides HTTP JSON-line ingestion (`/api/v1/import`), raw sample export (`/api/v1/export`), and Prometheus range queries (`/api/v1/query_range`), directly serving both the server-side History read path now and the future Grafana embed (GAP-01) as a Prometheus datasource without reimplementing PromQL in Go.

**Built.**
- `internal/monitor/store.go`:
  - Metric constants (`MetricLatencyMs` = `fleet_probe_latency_ms`, `MetricOK` = `fleet_probe_ok`, `MetricStatus` = `fleet_probe_status_code`, `MetricLoss` = `fleet_probe_loss_ratio`) and `NormalizeMetric` (aliases `latency_ms`, `ok`, `status`, `loss` or validated metric names).
  - `HistoryQuery`, `HistoryPoint`, `HistorySeries`, `HistoryReader`, `HistoryStore` interfaces.
  - `MultiSink` (`NewMultiSink`) fanning `monitor.Sink.Write` to all non-nil sinks independently (so a TSDB error never blocks or drops live in-memory `fleet.Series` updates).
  - `VMStore` (`NewVMStore`) writing NDJSON batches to VictoriaMetrics `/api/v1/import` (tagged with `agent`, `target`, `type`, and `history_key` when present) and querying ranges via `/api/v1/export` (`Step <= 0`, raw samples) or `/api/v1/query_range` (`Step > 0`, downsampled matrix).
- `internal/config/config.go`:
  - Added `MonitorStoreConfig` (`addr`, `retention`, `RetentionOrDefault()` defaulting to `"90d"`), `ServerConfig.MonitorStore`, defaults (`DefaultMonitorStorePort = 8428`, `DefaultMonitorStoreAddr = "http://127.0.0.1:8428"`, `DefaultMonitorStoreRetention = "90d"`), and `FLEET_MONITOR_STORE_ADDR` env override (`EnvMonitorStoreAddr`). `Defaults()` remains zero-valued for backwards compatibility when no store is configured.
- `internal/fleet/fleet.go`, `internal/fleet/open.go`, `internal/fleet/observe.go`:
  - Added `Config.MonitorStore` (populated from `Options.Config.MonitorStore` in `fleet.Open`).
  - Added `Observe.Store` (`monitor.HistoryStore`) and `OpenObserveWithOptions` (`OpenObserve` preserved for existing callers; `Manager.EnableObserve` passes `m.cfg.MonitorStore`).
  - `Manager.SetObserve` wires `monitor.NewMultiSink(o.Series, o.Store)` into `monitor.NewIngestor` when both are non-nil, keeping the 10-minute / 4000-point `Series` (`internal/fleet/series.go:13-31`) and `/ws/monitor` live feed intact.
- `internal/panel/monitor.go`:
  - Added `GET /api/monitor/history` (`monHistory`) querying `o.Store.QueryRange` with `metric`, `from`/`start`, `to`/`end`, `step`, `agent`, `target`, `history_key`, and `type`. Returns 503 if `o.Store == nil`, 400 on invalid parameters, 502 on store query failure, and 200 JSON `{metric, from, to, step_sec, series}`.
- `deploy/install-server.sh` & `deploy/REQUIREMENTS.md`:
  - Pinned `VICTORIAMETRICS_VERSION="1.157.0"` with pinned release tarball SHA-256 digests (`amd64`: `7b31192da6cf805b8b9244a904b022948e8022a067a822c30cb9f94600d0c387`, `arm64`: `e5368dec72d212dcb2b994345ab872245299000d37197f723123e63a50e573b5`) and `_checksums.txt` verification, installed to `/usr/local/bin/victoria-metrics` regardless of `--proxy` mode.
  - Added `fleet-victoriametrics.service` systemd unit (`User=fleet-tsdb`, `-httpListenAddr=127.0.0.1:${FLEET_TSDB_PORT}`, `-storageDataPath=${FLEET_TSDB_DIR}`, `-retentionPeriod=${FLEET_TSDB_RETENTION}`), default port `8428`, state dir `/var/lib/fleet-victoriametrics`, CLI flags (`--tsdb-port`, `--tsdb-dir`, `--tsdb-retention`), preflight port check, `monitor_store` in `config.json.example`, `FLEET_MONITOR_STORE_ADDR` in `fleet-server.env` / `fleet-server.service`, and `/health` startup check.
  - Updated `deploy/REQUIREMENTS.md` with loopback port `8428`, `/var/lib/fleet-victoriametrics` disk note, and pinned VictoriaMetrics version/asset/checksums.

**Tests & verification.**
- `internal/monitor/store_test.go`: `TestIngestorToVMStoreRangeQueryAndRestart` (full `Ingestor` -> `MultiSink` -> `VMStore` -> disk-backed fake VictoriaMetrics -> simulated restart -> raw `/api/v1/export` and stepped `/api/v1/query_range` round-trips across `latency_ms`, `ok`, `status`, `loss`), `TestMultiSinkIsolatesStoreErrors`, `TestVMStoreValidation`.
- `internal/fleet/observe_store_test.go`: `TestEnableObserveWiresPersistentStoreAndPreservesLiveWindow` (verifies `mon-data` hits live feed + 10-min `Series` window unaffected while older 2h samples persist and are queried from `o.Store`).
- `internal/panel/monitor_history_test.go`: `TestMonitorHistoryEndpoint` (verifies 503 when unconfigured, `POST /api/monitor/jobs` -> `mon-data` ingest -> `/api/monitor/live` unaffected -> `/api/monitor/history` raw & stepped queries, 400 validation errors).
- `internal/config/config_test.go`: `TestMonitorStoreConfigAndEnvOverride`.
- `gofmt -l .` empty; `go vet ./...` clean; `go build ./...` ok; `./scripts/test.sh server` passed across all server/internal packages; `bash -n deploy/install-server.sh` clean (`shellcheck` not installed in environment; installer not run against host).

wf: T-0055 done
