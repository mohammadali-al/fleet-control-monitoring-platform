# D-0005 — Defer the Load Controller

**Status:** accepted · **Date:** 2026-11-24

## Context
The Load Controller (architecture chapter 10) is the heaviest subsystem to build and review (Opus /
max effort, long iterations). The operator assumes, for now, that the sophisticated controller is
needed mainly for **spike** load patterns.

## Decision
- **Postpone the Load Controller backend** (chapter 10) until the operator explicitly schedules it.
- The panel still gets a **Maneuver / Load-Controller page as a non-functional UI stub** (layout and
  controls only, no execution).
- Simpler patterns (all-in-one, linear) also depend on the execution engine, so **Maneuver execution
  as a whole waits** until then; only the UI is built now.

## Consequences
- Near-term implementation focuses on the control channel, commands, evidence, monitors, and the panel.
- The `launchkill-sim` work stays as the reference for when the controller is picked up.
