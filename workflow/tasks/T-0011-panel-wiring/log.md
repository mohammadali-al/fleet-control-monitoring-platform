## T-0011 panel-wiring (doer, claude sonnet high)

Not split: Machines, Command runner and Terminal went in as one unit (they share the manager, the bus and `live.js`).
Go 1.26.8. Design note with the full API/WS reference and limits: `workflow/design/panel-wiring.md`.

**Needs the supervisor (touches):** `cmd/fleet-server/main.go` is not in this brief's `touches`, but the binary cannot
serve live data without it (it must build the manager and pass it to the panel). A commit of it was refused by the
permission layer, so I did NOT commit it. The exact change is `workflow/tasks/T-0011-panel-wiring/main-go.patch`
(`git apply` it; 17 lines: `fleet.Open`, `panel.New{Backend}`, `mgr.Start/Close`, `Serve(handler)`). Without it
`fleet-server` still starts and serves every page via `panel.Handler(web.FS)` but with no backend (empty registry, a
visible warning, API 503). All the tests below exercise the real wiring through `panel.New`; the patched `main` was
built and run by hand once (all pages 200, `/api/machines` JSON) before I reverted it.

What was built
- **Templates** (`web/_src/shell.html` -> `{{define "layout"}}`, `web/pages/*.html`, `web/pages/fragments.html`,
  `web/templates.go`, `internal/panel/templates.go`): all ten pages render from `html/template`; active nav from `.Page`;
  Mock chip only on pages that are still mock. **Stamp workflow replaced, not preserved** (one shell, no second
  renderer). Page templates were generated once from the stamped output, then Machines/Commands/Terminal/Maneuver
  edited by hand. `scripts/stamp-web.py`, `web/_src/pages/*`, and the pre-rendered `web/*.html` are now dead and were
  left (outside touches / no deletes): a cleanup task should remove them. `stamp-web.py` no longer works.
- **internal/fleet** (new): per-machine `exec.Queue` + link; state online/reconnecting/offline; `lost` on link drop;
  event bus; confirmation tokens; bounded in-memory output/history; `fleet.Open` (registry, CA in `<data>/ca` created
  on first run and never replaced if half-present, gate secret `<data>/config/gate.key` 0600, provisioner, mTLS
  dialers). A peer whose cert CN differs from the registry ID never shows online. `fleettest` = in-process link for tests.
- **API** (`api.go`) and **WebSockets** (`ws.go`): see the design note. Dangerous one-shot -> 409 `needs_confirmation`
  with nothing queued; on confirm the manager mints a machine+command-bound token that the **agent** verifies. Shell:
  agent-side line refusal -> dialog -> token minted for that line. Monitor agents: 422 / not listed. Forwarded user
  header (`Remote-User` first) is the command's `user` and in the panel's audit logs; no auth logic.
- **Live pages** (`web/assets/js/live.js`, new): Machines (rows/tiles swapped from server-rendered fragments on WS
  events, filters, onboard form, re-provision/remove dialog), Command runner (streamed output cards, visible queue
  `Queued #n`, cancel/terminate, history, gate dialog), Terminal (xterm.js, tabs, sessions list, in-terminal gate
  dialog, `?machine=` auto-open). Stubs that are not backed keep `data-stub` (Refresh public IPs, Details).
- **Maneuver** (`web/pages/maneuver.html`): Spike disabled, greyed, with a visible reason and `aria-describedby`;
  all-in-one default; timeline ceiling and summary no longer say spikes (D-0005). Still a stub.
- **Vendored** xterm.js 6.0.0 + fit addon under `web/assets/vendor/xterm/` (MIT, LICENSE included; sourcemap comments
  stripped). `web/assets/css/live.css` adds `[hidden]{display:none!important}` (the `.alert` rule was overriding `hidden`).
- `internal/config`: `server.channel_port` (7443), `server.server_id` (`fleet-server`). `go.mod`/`go.sum` unchanged.

Done-means check
- Served as `html/template`, all 10 pages render in light and dark, three Operate pages live: PASS. Evidence: panel
  tests render all pages (doctype, shell, exactly one `aria-current`, no `{{`, Mock chip only on mock pages); a Chrome
  (CDP/Playwright, uncommitted script) run loaded every page in both themes with no console or page errors.
- Machines = real registry, onboarding provisions, remove/re-provision: PASS. `TestMachinesPageShowsRegistry`,
  `TestOnboardRemoveReprovision` (fake SSH remote via `Provisioner.Dial`; machine appears, comes online, password never
  in logs or remote command lines, re-provision idempotent, remove keeps the record); same flow driven from the browser form and row menu.
- Command runner live stream + serial queue + gate: PASS. `TestWSCommandStream` (queued,running,exited + chunks over
  `/ws/events`), `TestQueueVisibleAndCancel`, `TestCommandSubmitHistoryAndGate`; fleet tests `TestSerialQueue` (strict
  FIFO), `TestManagerOverRealChannel` (real mTLS + real executor + same-secret gate); browser run saw output arrive
  mid-command, `Queued #1`, the gate dialog, and the confirmed command run.
- Terminal interactive shell, monitors not selectable: PASS. `TestWSShell` (PTY round trip, resize, refusal, confirm,
  close ends session), `TestWSShellRefusesMonitorAndOffline`, `TestWSEventsDoNotLeakShellOutput`; browser typed into xterm.js.
- `gofmt -l .` empty; `go vet ./...` clean; `scripts/test.sh server` passes; `-race -count=6` on panel and fleet passes;
  `scripts/build.sh` ok (bin/ removed).

Known limits / follow-ups (also in the design note)
- `cmd/fleet-agent` is still scaffolding: a real machine shows offline until the agent runs the Listener + Executor.
  The agent also needs the gate secret (`<data>/config/gate.key`); config push is a later task.
- `agent_dial` machines show offline with a note (no main-server listener exists yet).
- History/output are in memory, lost on restart. A shell shares the machine's FIFO with one-shots. A reload ends its shells.
- Pre-existing `app.js` quirk (T-0004): a row-menu popup that lands below the fold scrolls the page and the capture
  scroll listener closes it. Fix: `first.focus({preventScroll:true})`. Not touched (outside touches).
- Confirmation tokens live 10 min (not 2) so a confirmed command can wait in the queue.
- Nothing left running; temp work (browser scripts, screenshots, vendored-package extraction) stayed in the session scratchpad.

<!-- wf: commits=137eed4 left=- tests=server:pass blocked=main.go-outside-touches(patch in task folder, not committed) -->
