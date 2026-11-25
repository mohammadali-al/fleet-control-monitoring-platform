## 2026-11-25: heartbeat hang detection built (Go 1.26.8), wiring of two call sites is a `.patch`

**What was built (inside `touches`).**
- `internal/watchdog/heartbeat.go`: `ValidateHeartbeat` is the one bounds check (write in [2 s, 15 s],
  threshold in [45 s, 300 s], threshold >= 3 x write). `Policy` gained `heartbeat_write` / `heartbeat_threshold`
  (`policy.go`: defaults 5 s / 90 s, `Validate` calls `ValidateHeartbeat`), so the parser on the server (the
  panel save goes through `ParseProfile`) and on the agent reject the same values, and a pushed policy applies
  live like every other key. `Heartbeat.Tick` writes `"<unix> <threshold>\n"` (fixed 17 bytes, in place, open
  per write so a deleted file is recreated) and tells the loop when the next beat is due.
  `LivenessScript` / `LivenessExecStart` are the single implementation of the freshness check (plain sh): unit
  not active -> start it (the old behaviour); no heartbeat file -> leave alone (no restart loop for a binary or
  role that does not write one); otherwise the reference time is the later of the last beat and the unit's
  `ActiveEnterTimestamp`, and older than the threshold carried in the file (90 s if unreadable) -> log "hung"
  and `systemctl restart`. Reading the threshold from the file is what keeps the one-shot tiny and live.
- `internal/watchdog/heartbeat_run.go`: `Watchdog.RunWithHeartbeat`: `Run` plus a beat on every wake-up, from
  the loop itself (a hang in sampling or the ladder stops the beats).
- `internal/agent/heartbeat.go`: `RunHeartbeat` / `StartHeartbeat` for the main agent, driven by the pushed
  policy it already stores under the data dir (`watchdog.HeartbeatSettings`, cached by policy.yaml mtime).
  The beat is tied to the agent's state: each beat first runs `Alive()` (takes the listener and worker locks;
  implemented for `Agent`, `DialingWorker`, `Monitor`, `DialingMonitor`) off-loop with a 2 s timeout and at
  most one probe in flight, so a deadlock withholds the beat instead of a free-running goroutine masking it.
- `deploy/systemd/`: `fleet-watchdog-liveness.service` now carries the freshness one-shot;
  new `fleet-agent-liveness.service` + `.timer` (`OnUnitActiveSec=45s`, fixed, not a policy key).
- `internal/onboard/ssh.go`: installs `fleet-agent-liveness.service/.timer` next to `fleet-agent.service`
  (same text as `deploy/`, from `watchdog.LivenessExecStart`), `daemon-reload`, and
  `systemctl enable --now fleet-agent-liveness.timer` after the agent is started. `Remove` now disables the
  timer first (best effort), or it would restart the agent being decommissioned.
- Panel: `policyTemplate` carries `heartbeat_write` / `heartbeat_threshold` with the bounds as a comment;
  `web/pages/watchdog.html` explains them and says the check is every 45 s (fixed).

**Tests** (all `-race`): `internal/watchdog` `TestValidateHeartbeat` (10/75 ok, 50/75 rejected, edges),
`TestPolicyHeartbeatFields` (through `ParseProfile`, incl. overrides), `TestHeartbeatTick...`,
`TestLivenessScriptFreshness` (runs the real script under sh with stand-in `systemctl`/`logger`: stale ->
"hung" + restart, fresh -> nothing, live threshold from the file, freshly restarted unit, missing file,
inactive -> start), `TestDeployUnitsMatchTheScript`, `TestWatchdogLoopBeatsAndHangStopsIt` (a sampler that
blocks stops the beats). `internal/agent`: policy-driven interval + threshold, a hung probe withholds the beat
without piling up goroutines and resumes, `StartHeartbeat` on a real worker over mTLS.
`internal/onboard`: units installed, timer enabled, `deploy/` copies equal the installed text, `TestRemove`
updated for the timer. `internal/panel`: template exposes both keys, bad bounds -> 400, 10 s/75 s -> 200.
`gofmt -l .` empty, `go vet ./...` clean, `scripts/build.sh` ok, `scripts/test.sh server` and `agent` pass.
The patched tree (below) also passed `go test -race ./cmd/... ./internal/...`.

**Outside `touches` (needs the supervisor).**
`workflow/tasks/T-0027-agent-heartbeat-liveness/out-of-touches-wiring.patch` (`git apply --check` clean;
with it applied the full suite passes under `-race`). Until it is applied nothing calls the new code, so no
service writes a heartbeat yet (and the files stay absent, which the one-shot treats as "do not judge"):
- `internal/agent/watchdog_wire.go`: `RunWatchdog` calls `w.RunWithHeartbeat(ctx)` instead of `w.Run(ctx)`.
- `cmd/fleet-agent/main.go`: `agent.StartHeartbeat(ctx, a, ac, dataDir, log)` before `a.Run(ctx)` (all roles).
- `deploy/systemd/fleet-watchdog-liveness.timer`: `OnUnitActiveSec=1min` -> `45s` (the brief fixes 45 s; the
  file is not in `touches`).
- `web/assets/js/live.js`: one row in the "Effective values" tab for the two heartbeat fields.

**Brief facts that did not match, and what I did.**
- The brief says onboarding installs the new timer "alongside the existing watchdog one", but `ssh.go`
  installs no watchdog units at all (only `fleet-agent.service`). I added only the agent liveness pair; I did
  not start installing `fleet-watchdog.service` (that would start a root watchdog on every machine, monitors
  included, a separate decision). The watchdog's liveness timer remains a `deploy/` file.
- The brief names the "main loop" of the agent; `Agent.Run` blocks in the listener, so there is no per-tick
  loop to hang a beat on. The probe above is the substitute (it catches a deadlock on the core locks, not a
  hang deep inside one command's goroutine).

**Notes.**
- With write <= 15 the 45 s floor already implies threshold >= 3 x write, so that rule never binds on its own
  today; it is still enforced, and a test fails if the bounds change so that it could.
- Worst-case time to restart a hung service is about threshold + 45 s (timer) + the timer's 5 s accuracy.
- Real restart-on-hang (systemd, `ActiveEnterTimestamp` parsing by the host's `date`) needs the supervisor's
  check on a real machine; the script was only exercised against stand-in `systemctl`/`logger`.

<!-- wf: commits=457b066 left=- tests=server:pass,agent:pass blocked=no -->
