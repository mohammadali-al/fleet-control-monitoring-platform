---
id: T-0074
title: agent-update-stuck-timeout
state: merged
touches: [internal/fleet/update.go, internal/fleet/fleet.go, internal/fleet/update_test.go,
          web/src/pages/MachinesPage.jsx, web/assets/dist/, web/dist/index.html]
tests: [server]
after: [T-0068]
shares: {}
model: {claude: "sonnet high"}
operator_check: "A stuck agent update no longer hangs forever in 'Update requested...'. If an agent doesn't report update progress within a bounded window, the panel marks that machine's update as Failed (timed out) and re-enables the Update button so the operator can retry. Normal (healthy) updates still complete and show 'Updated to <ver>' as before."
---

## Why

An operator triggered a self-update on one worker and the machine is stuck showing **"Update requested…"**
indefinitely. Root cause: the push-update state machine has **no server-side timeout**. `UpdateAgent`
sets the in-memory state to `requested` and then relies *entirely* on the agent to report back
`downloading → applying → ok`/`failed` (`recordAgentUpdateResult`). If the agent never replies — e.g. an
**older agent build that predates self-update** and so silently ignores `TypeAgentUpdate` (exactly the
`airesi01` case: its version is "Unreported", meaning it also never sent `TypeAgentVersion`) — the state
sits at `requested` forever.

That is doubly bad because the panel treats `requested|downloading|applying` as "in progress" and
**disables the Update button** for that machine (and the bulk "Update outdated"), so the operator cannot
even retry. The update is both stuck and un-retryable.

This task adds a bounded timeout so a non-progressing update resolves to `failed` (timed out), and makes
the panel treat a stale in-progress state as no-longer-in-progress so the controls re-enable.

> Operational note (for the operator, not this task): the update state is an **in-memory map** and is not
> persisted, so restarting `fleet-server` already clears a stuck state today. This task is the durable fix
> so a restart isn't needed and the operator sees *why* it stopped.

## What to build

### Backend (`internal/fleet`)

1. **Record the dispatch time** — `AgentUpdateState` already has `UpdatedAt` (`update.go:29`); it is set
   to "now" when `requested` is written (`update.go:180`) and refreshed on every agent report
   (`recordAgentUpdateResult`, `update.go:278`). Use that as the "last progress" timestamp. Add a
   timeout constant (suggest `agentUpdateStallTimeout`, default **5 minutes**) — comfortably longer than
   the agent's own `ReconnectTimeout` (60s) + `GraceWindow` (120s) + a channel download, so it only fires
   on genuine non-progress, never on a healthy slow update or a legitimate restart gap.

2. **Sweep stale in-progress updates.** In the existing poll loop (the `m.poll()` ticker on
   `PollInterval`, `fleet.go:393` / the `poll()` body), scan `m.updates` under `updMu`: for any state whose
   `Status` is `requested`, `downloading`, or `applying` and whose `UpdatedAt` is older than
   `agentUpdateStallTimeout`, rewrite it to `Status: failed`, `Error: "timed out: agent did not report
   update progress"`, `UpdatedAt: now`, keeping `RequestID`/`TargetVersion`. Call `m.publishMachine(id)`
   for each so the panel updates live. Keep the scan cheap (collect ids under the lock, publish after
   releasing it — mirror the `reapIdleShells` pattern at `fleet.go:1264`).

3. **Don't clobber a real late report.** The staleness check must compare `UpdatedAt`, and
   `recordAgentUpdateResult` must keep refreshing `UpdatedAt` on each progress message (it already writes a
   fresh state) — so an agent that *is* progressing slowly keeps resetting the clock and never times out.
   A `failed`/`ok` state is terminal: never swept.

### Frontend (`web/src/pages/MachinesPage.jsx`)

4. **Treat a stale in-progress state as not-in-progress.** `isAgentUpdateInProgress(upd)`
   (`MachinesPage.jsx:196`) currently returns true for any `requested|downloading|applying`. Make it also
   require that `upd.updated_at` is recent (within a client-side staleness window — suggest ~6 min, i.e.
   slightly longer than the backend timeout so the backend normally wins the race and flips it to
   `failed` first). When the state is stale, the function returns false → the per-row / drawer / menu
   "Update agent" buttons (`:526`, `:2046`, `:2163`) and the bulk "Update outdated" re-enable, and
   `formatUpdateStatus` still shows the last known line. This is a safety net in case a server-side flip is
   delayed; the backend timeout (step 2) is the primary mechanism.

5. **Rebuild the committed SPA bundle** (`./web/build.sh`) and commit the regenerated
   `web/assets/dist/app.{js,css}` + `web/dist/index.html` along with the source change.

## Out of scope

- No change to the agent-side update protocol, chunk transfer, rollback, or `recordAgentUpdateResult`'s
  stale-request-id guard beyond keeping `UpdatedAt` fresh.
- **Not** fixing the already-deployed old agent on `airesi01` — that agent simply predates self-update and
  version reporting; getting it current is an operational re-onboard/upgrade, not a code change here.
- No new config surface/flag unless trivial; a const default is fine (mention in the log if you add one).
- No redesign of the Machines page or the update UI beyond the gating/staleness tweak.

## Done means

- Triggering an update on an agent that never reports results in the machine flipping from
  "Update requested…" to a **Failed (timed out)** line within ~5 min, and the Update button becoming
  clickable again (both via the backend flip and, as a fallback, the client staleness check).
- A healthy update still shows `downloading → applying → Updated to <ver>` and is never prematurely timed
  out (covered by a test that advances the clock while reports keep arriving).
- New/updated tests in `internal/fleet/update_test.go`: (a) a `requested` state older than the timeout is
  swept to `failed` by `poll()`; (b) a state kept fresh by progress reports is **not** swept; (c) a
  terminal `ok`/`failed` state is left untouched.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok, `scripts/test.sh server` passes; web
  bundle rebuilt + committed; `check-app` clean for the Machines page.

## Facts this brief relies on

- `UpdateAgent` sets state `requested` with no deadline: `internal/fleet/update.go:180-185`; it returns
  early as `noop` when already on target (`update.go:139`) so the sweep never sees a no-op.
- `AgentUpdateState` (with `Status` + `UpdatedAt`): `internal/fleet/update.go:29-35`. In-memory map
  `m.updates` guarded by `updMu`, created empty, **not persisted**: `internal/fleet/fleet.go:255-256,323`.
- Agent reports advance state in `recordAgentUpdateResult` (writes a fresh state incl. `UpdatedAt`):
  `internal/fleet/update.go:246-306`. Valid agent statuses: `downloading|applying|ok|failed`
  (`update.go:252-257`).
- Poll ticker to hang the sweep on: `internal/fleet/fleet.go:393` (`m.poll()` on `m.cfg.PollInterval`).
  Reaper pattern to mirror (collect-under-lock, act-after-unlock): `internal/fleet/fleet.go:1264`
  `reapIdleShells`.
- Agent that doesn't implement `TypeAgentUpdate` sends nothing back; the handler that *would* reply is
  `internal/agent/update.go:314` `handleUpdate` (absent in older builds). Version reporting is
  `TypeAgentVersion` → `recordAgentVersion` (`internal/fleet/fleet.go:603,1952`); "Unreported" = never
  sent it.
- UI gating: `isAgentUpdateInProgress` `web/src/pages/MachinesPage.jsx:196`; `formatUpdateStatus` `:200`;
  disabled Update buttons at `:526-530`, row `:2046`, menu `:2160-2164`; bulk "Update outdated" `:1448`.
  The update state arrives on the client as `machine.agent_update` (`:221`, `:488`) with `updated_at`.
- Committed bundle artifacts to regenerate: `web/assets/dist/app.{js,css}`, `web/dist/index.html`
  (`web/build.sh`).

## Notes for the doer

- Run `./scripts/wf start T-0074` before committing; commit only inside `touches` + append `log.md` ending
  in the `wf:` footer. Do NOT push/merge/branch-delete, edit `state:`, or touch decisions/generated/other
  tasks.
- Keep the backend timeout generous and documented inline (one comment explaining why 5 min is safe vs the
  agent's 60s reconnect / 120s grace). Prefer a single const over a config flag.
- The frontend change is a tiny predicate tweak — don't restructure the page. Make sure the staleness
  window is read from `agent_update.updated_at` (server sends RFC3339); reuse any existing age helper on
  the page (there's `fmtAgo`) for parsing rather than hand-rolling.
