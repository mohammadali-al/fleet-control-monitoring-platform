# D-0002 — Data rules

**Status:** accepted · **Date:** 2026-11-23

## Context
The platform handles data that is sensitive or irreplaceable: captured **evidence** (used as
contractual proof), **monitor data**, the **internal CA private keys** (losing them forces
re-enrollment of every machine — architecture §4.4, §7.5), **agent SSH credentials**, and
**backup-server credentials**.

## Decision
- **Doers never touch real/sensitive data.** Off-limits paths are listed in
  `workflow/project.yml:real_data` and will be guarded by a pre-commit hook (R-06). None exist yet;
  the list is in place so the guard is ready.
- **Doers use fixtures only** — synthetic configs, test certs generated in-test, fake agents.
- **The supervisor touches real data only after a backup** (architecture §7.5 defines the backup).
- **Secrets never get committed** — CA keys, SSH creds, tokens live outside the repo (or in ignored
  paths) and are provisioned at install time.

## Consequences
- Tests that need certs/keys generate throwaway ones at runtime.
- The e2e/dev harness (a later task) runs against fake agents and self-signed test CAs, never real
  machines or real evidence.
