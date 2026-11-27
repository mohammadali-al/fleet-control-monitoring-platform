---
id: T-0055
title: monitor-timeseries-store
state: ready
touches: [internal/fleet/series.go, internal/fleet/, internal/monitor/, internal/panel/monitor.go,
          internal/config/config.go, deploy/install-server.sh, deploy/REQUIREMENTS.md]
tests: [fleet, monitor, server]
after: []
shares: {}
model: {claude: "sonnet high"}
operator_check: "Monitor samples are persisted to a long-term time-series store (not just the 10-min in-memory window), and a server-side history query path can read past data back by range. The live window still works. Deploy docs cover the store. This is the backend foundation the Grafana embed + History UI build on; no panel UI change here."
---

## Why

Monitor data is retained only for a 10-minute, 4000-point in-memory ring (`internal/fleet/series.go:13-31`),
with no long-term storage (audit GAP-21; §8.4/§15 call for a Prometheus/VictoriaMetrics time-series store
via the server ingest path). Build the **storage backend + history read path**. This is the foundation for
the Grafana embed (GAP-01) and the History/semantic-zoom UI (GAP-23) — those are separate, later tasks.

## What to build

1. **Time-series store.** Add a persistent time-series backend fed by the existing monitor ingest path.
   Pick the approach and record it briefly in the log: either (a) run **VictoriaMetrics** (single static
   binary, installed/managed by the installer like caddy/authelia) as the store, or (b) an **embedded**
   local TSDB written from the ingest path — choose the one that fits the native-proxy, single-box deploy
   model (no Docker required; §15 names Prometheus/VictoriaMetrics). Write monitor samples to it from where
   they already land in `internal/fleet/` / `internal/monitor/`.
2. **History read path.** A server-side query API to read samples back by metric + time range (the data
   source for the later History UI). Keep the 10-min live in-memory window working for live charts.
3. **Config + deploy.** Add any needed `internal/config/config.go` settings (store address/retention) and,
   if a binary store is used, install/verify it in `deploy/install-server.sh` (pinned version, checksum,
   systemd unit, state dir, port) and document it in `deploy/REQUIREMENTS.md` (ports/disk).

## Out of scope

- No Grafana (GAP-01) and no History/semantic-zoom UI (GAP-23) here — this is storage + query only. No
  change to how live charts work today; no Maneuver/Load work.

## Done means

- Monitor samples persist to the store and survive restart; a range query reads them back; the live window
  is unaffected. If a binary store is used, the installer provisions it (pinned + checksum + unit) and docs
  cover it.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok, `scripts/test.sh` passes for the touched
  groups, with a test proving ingest→store→range-query round-trips. `shellcheck`/`bash -n` clean if the
  installer changed.

## Facts this brief relies on

- Audit evidence: GAP-21 `internal/fleet/series.go:13-31` (`DefaultSeriesRetention = 10 * time.Minute`,
  `maxSeriesPoints = 4000`); §8.4 L364, §15 L609 (`Architecture_Gap_Audit.md`). Native-proxy single-box
  deploy model + pinned-binary pattern: `workflow/decisions/D-0009-default-proxy-native.md`,
  `deploy/install-server.sh` (caddy/authelia install).

## Notes for the doer

- Favor the option that keeps the single-box, Docker-optional deploy simple; state the choice + why in the
  log. Run `./scripts/wf start T-0055` before committing; commit inside touches + the log. Do NOT
  push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
