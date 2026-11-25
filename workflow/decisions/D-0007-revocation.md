# D-0007 — Agent revocation is registry-based (no CRL)

**Status:** accepted · **Date:** 2026-11-25

## Context
Detaching an agent removes it from the registry and records its revoked fingerprint. The server only
grants a session to a known, active machine, so a detached machine is refused at the app layer even if
its client cert is still cryptographically valid (the TLS handshake may succeed, but the Manager gives
it no link). The architecture (§14) phrases this as "cert revoke + panel removal".

## Decision
- **Registry-based revocation is sufficient.** Panel removal / detach is the effective revocation: a
  removed machine cannot obtain a session in either dial direction. **No CRL/OCSP** is implemented.
- Fits the trusted-operator / LAN model. A real CRL (reject revoked certs at the TLS handshake) is
  **deferred** and only worth it if the threat model changes.

## Consequences
- "Revoke" in the UI/flows means remove-from-registry; there is no separate cert-revocation list to
  distribute or check.
