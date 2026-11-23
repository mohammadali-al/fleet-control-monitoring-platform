# D-0001 — Technology stack

**Status:** accepted · **Date:** 2026-11-23

## Context
Greenfield build of the platform specified in `Fleet_Control_Monitoring_Platform_Architecture.md`.
The architecture already fixes most technology choices; this decision records them as the repo's
baseline and names the concrete repo conventions.

## Decision
- **Language:** Go (server + the one agent binary). Module path
  `github.com/mohammadali-al/fleet-control-monitoring-platform`. Target Go 1.23+.
- **Binaries / layout:** `cmd/fleet-server`, `cmd/fleet-agent`; shared code under `internal/`.
  (`fleet-server` is this repo's name for the "Fleet Server (Go)" of architecture §3; `fleet-agent`
  is from architecture §5 / L228.)
- **Control channel:** mTLS WebSocket via `coder/websocket` (architecture §4.1, L148).
- **PKI:** internal CA with Go `crypto/x509`, IP-SAN certs (architecture §4.4).
- **Capture:** cgroup v2 + eBPF (`cilium/ebpf`) / nftables `meta cgroup` (architecture §6.2).
- **Time-series:** Prometheus or VictoriaMetrics; **Grafana embedded** in the panel (architecture §8.4, §12).
- **Operator panel:** server-rendered (Go `html/template`) + `xterm.js` + embedded Grafana
  (architecture §3, §12). Front-end framework detail deferred to the panel task.
- **Auth:** Authelia/Pomerium + 2FA in front (architecture §12.1). **Runtime:** systemd.

## Consequences
- One repo, two binaries, shared `internal/` packages. CGO off by default (the agent enables it only
  if a capture library requires it).
- The panel's exact front-end approach is an open sub-decision, taken when that task is briefed.
