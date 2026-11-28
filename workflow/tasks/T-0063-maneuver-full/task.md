---
id: T-0063
title: maneuver-full
state: merged
touches: [internal/maneuver/, internal/channel/envelope.go, internal/fleet/, internal/agent/,
          internal/capture/, internal/exec/, internal/config/config.go,
          internal/panel/api.go, internal/panel/maneuver.go,
          web/src/pages/ManeuverPage.jsx, web/src/routes.js, web/src/hooks/, internal/panel/maneuver_test.go]
tests: [server, agent]
after: [T-0047, T-0054]
shares: {}
model: {claude: "sonnet high"}
operator_check: "Operators can build a Maneuver (a timeline of baseline items targeting exactly one (ip, origin), mirrored on selected worker agents) in a real React editor, and RUN it: the server pushes the plan + a future start T, collects readiness ACKs, commits T, and agents start in sync (an agent that gets the order after T reports 'missed'). Worker agents continue the remaining plan autonomously during a server outage and rejoin at the current wall-clock timeline position on reconnect/reboot. Each Maneuver replica runs in a tracked cgroup owned by a session_id (evidence capture attributed). Load patterns: All-in-one only; Linear + Spikes are disabled in the panel (need the deferred Load Controller)."
---

## Why

The whole Maneuver stack is unbuilt (audit GAP-26/27/12/04/14): the editor is a stub, there's no store/
(ip,origin) validation/agent-selection, no synchronized-start protocol, no worker continuation across
outages, and no replica cgroup capture. Build it end to end (operator chose "everything in one push"),
on the React SPA (D-0011), reusing the Baseline library + substitution engine (T-0054). **Load Controller
stays deferred (D-0012): only the All-in-one pattern is selectable; Linear + Spikes are disabled.**

## What to build

**Backend — planning (`internal/maneuver/`, `internal/panel/maneuver.go`, `internal/config/config.go`):**
1. A persisted **Maneuver store**: a Maneuver = name, exactly one **`(ip, origin)`** target (validate it),
   a **timeline** of placed baseline-item instances (each resolves a `internal/baseline` item via its
   substitution engine), the set of **selected worker agents** it mirrors onto, and a **load pattern**.
   CRUD API behind `internal/panel/api.go` (`s.live`). Patterns: **All-in-one** only; keep the schema
   ready for Linear/Spikes but reject/disable them (Load Controller deferred, D-0012).

**Backend — execution (`internal/channel/envelope.go`, `internal/fleet/`, `internal/agent/`, `internal/exec/`, `internal/capture/`):**
2. **Synchronized start (GAP-12, §4.9):** new typed envelopes — server→agent push of the full plan + a
   **future start timestamp `T`**; each selected agent **ACKs "ready"**; server **commits `T` only after all
   ACKs**; an agent that receives the order **after `T` reports "missed"** and does not start late. Mirror
   the existing typed-envelope pattern.
3. **Worker continuation + rejoin (GAP-04, §4.3):** a worker agent **continues the remaining Maneuver plan
   on its own** during a main-server outage, and on reconnect (or after its own reboot mid-Maneuver)
   **rejoins at the current wall-clock position** of the timeline (persist enough plan state agent-side).
4. **Replica capture attribution (GAP-14, §6.1):** each Maneuver replica execution runs inside a **tracked
   cgroup owned by a `session_id`** (extend the exec/capture session model — `internal/exec/session.go`,
   `internal/capture/`) so its outbound traffic is captured and attributed like one-shot/PTY sessions.

**Front-end (React, per T-0047 contract):**
5. `web/src/pages/ManeuverPage.jsx` + register `component: ManeuverPage` in `web/src/routes.js`: the real
   editor — place baseline items on an interactive timeline, set the single `(ip, origin)`, select worker
   agents, pick the pattern (**All-in-one enabled; Linear + Spikes shown but disabled** with a "needs the
   Load Controller — deferred" note), and **Start/Stop** the Maneuver (showing readiness ACKs / missed /
   running state live over `/ws/events`). Rebuild + commit the web bundle (`./web/build.sh`).

## Out of scope

- **Load Controller engine** (reserve_pid, load shaping, 1s cgroup telemetry — GAP-28/29/30, deferred
  D-0012): do not build; Linear/Spikes stay disabled. **Projects/tests** (GAP-24) is a separate task
  (T-0066) — a Maneuver here can be run directly for now; the project/test wrapper + single-active-test
  lock come later. No report generation (T-0062).

## Done means

- A Maneuver can be created/edited (timeline + one (ip,origin) + agent selection + All-in-one pattern),
  persisted, and **run** with synchronized start (plan+T, ready ACKs, commit, missed-if-late); workers
  continue through a simulated server outage and rejoin at the wall-clock position; replicas run in tracked
  cgroups with attributed capture; Linear/Spikes are disabled in the UI.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` + the
  agent group pass, with tests for: sync-start handshake (ready/commit/missed), outage continuation +
  wall-clock rejoin, and replica session/cgroup attribution. Web bundle rebuilt + committed.

## Facts this brief relies on

- Audit: GAP-26 §9.3 L402-408, GAP-27 §9.4 L412-418 (`web/pages/maneuver.html:53` Spikes disabled), GAP-12
  §4.9 L223-226, GAP-04 §4.3 L171-172, GAP-14 §6.1 L257. Baseline library + substitution to reuse:
  `internal/baseline/` (T-0054). Typed-envelope pattern + dispatch: `internal/channel/envelope.go`,
  `internal/fleet/fleet.go`. Session/cgroup attribution: `internal/exec/session.go`, `internal/capture/`.
  SPA route contract: `web/src/routes.js`, `web/src/pages/MachinesPage.jsx` (T-0047). Scope decision: D-0012.

## Notes for the doer

- **Very large — split into clear commits** (store+API; sync-start envelopes+handshake; worker continuation+
  rejoin; replica capture; React editor+run) and report the split. Keep the channel backward-tolerant. If a
  change genuinely needs a file outside `touches`, save a `.patch` + report. Rebuild + commit the bundle.
  Run `./scripts/wf start T-0063` before committing; commit inside touches + the log. Do NOT push/merge/
  branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
