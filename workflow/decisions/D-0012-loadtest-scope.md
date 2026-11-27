# D-0012 — Load-test feature scope: build the stack, defer the Load Controller

**Status:** accepted (2026-11-28)

## Context

The gap audit (`Architecture_Gap_Audit.md`) found the whole load-testing stack unbuilt: Projects, Baseline
items, Maneuvers, load patterns, Maneuver synchronized-start + worker continuation, and the §10 **Load
Controller** (`reserve_pid` engine, 1 s cgroup telemetry). The Load Controller is large and needs
real-machine calibration (OD-5), so the operator chose to defer it while building everything around it.

## Decision

- **Build now:** Projects, Baseline items, Maneuver editor, Maneuver store + `(ip, origin)` scope + agent
  selection, Maneuver **synchronized start** (future `T`, ready ACKs, `missed`), worker Maneuver
  continuation + wall-clock rejoin, and Maneuver-replica cgroup capture attribution.
- **Defer:** the §10 **Load Controller** (`reserve_pid` Smith-predictor engine, over-launch-and-cull v1
  hooks, 1 s per-item cgroup CPU/RAM telemetry — GAP-28/29/30).
- **Load patterns:** only **All-in-one** is selectable. **Linear (sawtooth)** and **Spikes** are made
  **unselectable in the panel** (disabled, with a "needs the Load Controller — deferred" note), because
  they require the controller to shape load. When the Load Controller is built, both are re-enabled.
- All of this is built on the **React SPA** (D-0011): feature pages are real React routes under
  `web/src/pages/` registered in `web/src/routes.js`, not the legacy server-rendered pages.

## Consequences

- The stub pages (Projects/Baseline/Maneuver) are built for real as React features; this **supersedes**
  the "port the stub" scope of the T-0048/T-0049 SPA port tasks — those are re-scoped when the feature
  tasks are briefed.
- Linear/Spikes being disabled is a temporary UI gate keyed to the Load Controller's absence, not a
  permanent removal.
