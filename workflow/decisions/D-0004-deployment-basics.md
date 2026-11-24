# D-0004 — Deployment basics (panel port & serving)

**Status:** accepted · **Date:** 2026-11-24

## Context
Development is starting; we need a concrete port and serving model so tasks can wire the panel.

## Decision
- The operator panel is served by **`fleet-server` itself** (Go `net/http`) and **listens on `:6789`**.
- It is reached only through the **Authelia/Pomerium** reverse proxy (2FA); `fleet-server` trusts the
  proxy's forwarded auth headers and keeps **no local accounts** (architecture §12).
- **No separate webserver** (no nginx/Apache).

## Consequences
- In dev, hit `:6789` directly (the auth proxy is optional locally).
- The main server's **first-install/bootstrap** (packaging, bringing up the data stores, the install
  command) remains **open** — a later decision.
- **Panel HTTPS / auth proxy** (Authelia/Pomerium terminating TLS + 2FA, with a cert) is part of this
  deployment task, **scheduled for the deployment phase after T-0011 (panel-wiring)** — confirmed
  2026-11-24. Until then dev runs plain HTTP on localhost; the agent↔main-server control channel is
  already mTLS (T-0002/T-0003). (If HTTPS is needed off-localhost sooner, a small fleet-server-native-
  HTTPS task can be briefed instead.)
