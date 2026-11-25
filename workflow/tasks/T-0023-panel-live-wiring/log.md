# T-0023 panel-live-wiring: log

## 2026-11-25: all four pages live (Go 1.26.8), two commits (Evidence + Backup, then Monitor data + Watchdog)

**Built.** The same approach as T-0011 (JSON API + WebSockets + Go templates + `live.js`). Nothing
was redesigned: the approved components are reused; `live.css` is untouched.

- **`internal/fleet/observe.go` (new), `series.go` (new), `policystore.go` (new), `fleet.go` (small edits).**
  `fleet.Observe` is the set of backends behind the four pages (archive, backup service, monitor jobs +
  `Series` sink, watchdog `PolicyStore`). `Manager.SetObserve` binds it: routes `ev-manifest`/`ev-chunk`
  to a per-node `evidence.Syncer`, calls `SetMonitor` (real `monitor.Jobs.ConfigFor` + an `Ingestor`
  writing to `Series`) and `SetWatchdog` (stored profile + an event feed), and starts the **backup `Run`
  loop** (ends with the manager's context, tracked in `wg`). `EnableObserve(dataDir)` = `OpenObserve` +
  `SetObserve`. Manager additions: `PullEvidence` (async, status per machine incl. agent idle pushes),
  `SyncStatuses`, `StartBackup`/`StartImport` (async job status), once-a-day `BackupReminder` held until
  dismissed, `SubscribeWatchdog`.
- **`internal/panel`**: `observe.go` (optional `observer` interface that `*fleet.Manager` satisfies; 503
  when there is no Observe), `evidence.go`, `backup.go`, `monitor.go`, `watchdog.go`; `api.go` registers
  the routes and maps `ErrBusy` / backup errors to 409; `ws.go` adds `/ws/monitor` and `/ws/watchdog`;
  `templates.go` flips the four pages from mock to live (no Mock chip).
- **Pages + `live.js`**: `evidence.html`, `backup.html`, `monitor.html`, `watchdog.html` rewritten to the
  live hooks; `initEvidence/initBackup/initMonitor/initWatchdog` added. `/ws/events` is now only opened by
  the Operate pages.

**What each page does now.**
- *Evidence*: session browser from the archive (filters; command and kind joined from the in-memory
  command history), per-session packet records (first 200), Pull per machine and "pull from all online"
  with live status, archive disk guard meter, manifest per machine, export pcapng/CSV/JSON (session,
  machine or time range) streamed as a download.
- *Backup*: server record, add/replace server (password never logged), diff per content type (CA reports
  counts only), back up now and import (confirm required, background job with polling), schedule +
  suggestion, last run, the once-a-day reminder with "remind me tomorrow".
- *Monitor data*: snapshot + `/ws/monitor` stream; merged / per machine / per target views, 60 s
  (2 / 5 min) window, target filter, pause, stats, accessible table, command overlay (hover shows the
  commands in that interval; packet count appears once the session's evidence is archived), probe-config
  editor (global frequency + timeouts, jobs create/delete, per-job frequency override) that pushes
  `mon-config`. Failed probes are ticks on the chart.
- *Watchdog*: summary tiles, per-host stage / policy-in-force / restarts and reboots today / last event,
  YAML policy editor (validate with `watchdog.ParseProfile`, version must rise, store, `PushPolicies`),
  read-only effective values, event history, `/ws/watchdog`.

**Verified.**
- `internal/panel/observe_test.go`: Evidence over a real `evidence.Agent` in-process (pull -> archived,
  second pull skips, sessions/filters, records + limit, manifest, three export formats incl. time range,
  idle push, errors 409/404/422); Backup over a POSIX-shell "remote" in a temp dir (reminder + dismiss,
  validation, unreachable host, add + diff, run job -> in sync, file on the backup, schedule, import
  confirm + job); no-backend 503s.
- `internal/panel/observe_stream_test.go`: Monitor (job create + `mon-config` push, settings, **WS test of
  `/ws/monitor`**, snapshot window, delete), command overlay with a real executor and archived packets,
  Watchdog (template valid, save/refuse/push only to online workers, version must rise, config-req
  pushes the stored profile, ack, **WS test of `/ws/watchdog`**, derived state, events), `deriveHost`
  unit test, all four pages serve live and the three deferred pages stay mock.
- `internal/fleet/series_test.go`: retention, window, out-of-order batches, bound, feed.
- Real build + serve: built `fleet-server` with the main.go patch applied, served on a temp data dir;
  every new API answered; the four pages loaded in headless Chrome and the page scripts filled them
  (DOM dump). A seeded in-process panel (agent evidence, backup, jobs, a 400 ms probe stream, watchdog
  events) was screenshotted: the live chart, tiles, host table with ladder, policy editor render
  correctly. (Throwaway harness, deleted before commit; the main.go change was reverted after.)
- `gofmt -l .` empty; `go vet ./...` clean; `scripts/build.sh` ok; `scripts/test.sh server` passes;
  `go test -race -count=3` on `internal/panel` and `internal/fleet` passes.

**Split.** None needed beyond two commits (`c5618cd` Evidence + Backup, `dfe2dff` Monitor + Watchdog),
as the brief suggested; it reviews as one unit.

**Out-of-touches patch (supervisor applies):** `main-go.patch` in this folder. One call in
`cmd/fleet-server/main.go` after `fleet.Open`: `mgr.EnableObserve(dataDir)`. Without it the four pages
render but their APIs answer 503 ("no ... backend configured").

**Notes / gaps (honest).**
- **Counter reset is not wired.** The agent's counters live in the watchdog process on the host and no
  control-channel message tells it to reset (`channel/envelope.go` has none; the agent has no handler;
  there is no `fleet-agent` subcommand). The two "Reset counters" buttons stay `data-stub` with that
  reason. Needs a follow-up: a `TypeWatchdogReset` envelope (channel), a handler in the main agent that
  runs `watchdog.Guards.Reset` (or signals the watchdog process), and a manager `ResetWatchdog`.
- **Evidence pull has no real agent peer yet.** `evidence.NewAgent` is not used by `internal/agent`
  or `cmd/fleet-agent`: nothing answers `ev-manifest-req` on a real agent, so against one a pull ends
  with the syncer's timeout error (shown on the page). The server half is wired and tested against an
  in-process `evidence.Agent`; the agent runtime needs `evidence.Agent.Handle` in its handler (and
  `Run` for idle push) in a follow-up.
- **Per-host watchdog stage and daily counters are derived** from the forwarded events (the agent does
  not report its guard state as such), so they start empty after a server restart; the page says so.
  Cooldown time left is not shown for the same reason.
- **Monitor history is in memory** (`fleet.Series`: 10 min, bounded). The History tab and the Grafana
  frame are labelled previews/placeholders: the long-term time-series store is a later task.
- Probe types and monitor-agent pickers of the mock "create job" form were dropped: `monitor.Jobs`
  chooses the targets (ICMP + TCP + HTTP per path) and balances the agents itself. The timeout override
  per job has no backend, so it is gone too.
- The policy store reuses the watchdog file format at `<DataDir>/watchdog/policy.yaml` on the main
  server. It is not in the backup set (`backup.SetDirs`), which is outside touches; worth deciding.
- Policy editor: YAML is the editor of record (overrides live there); the mock Form tab became a
  read-only "effective values" view rather than a second writer of the same YAML.
- Syncer routing for `ev-*` handles worker agents only; a monitor agent's `ev-*` is ignored.
- `server_test.go`'s `TestAllPagesRender` follows the `pages` table, so it needed no change.

<!-- wf: commits=c5618cd,dfe2dff left=- tests=server:pass blocked=no -->
