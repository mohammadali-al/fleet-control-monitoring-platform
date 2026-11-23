# Fleet Control & Monitoring Platform

Design-phase repository for a fleet orchestration, monitoring and load-generation platform:
a **main server** controlling **worker agents** (command execution + evidence capture + the
Load Controller) and **monitor agents** (headless probers), over an mTLS WebSocket control plane.

- **Master design:** [`Fleet_Control_Monitoring_Platform_Architecture.md`](Fleet_Control_Monitoring_Platform_Architecture.md)
- **How work happens here:** [`AGENTS.md`](AGENTS.md) (supervisor–doer workflow)
- **Board:** [`workflow/generated/STATUS.md`](workflow/generated/STATUS.md)
- **Load Controller simulator:** [`launchkill-sim/`](launchkill-sim/)

Status: design approved, scaffolding in place, first implementation task (T-0001) ready.
