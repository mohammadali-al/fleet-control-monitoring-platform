# D-0006 — Data directory & portability

**Status:** accepted · **Date:** 2026-11-24

## Context
The operator wants the app to stay portable/relocatable: persistent data must not be buried inside
the code or install tree, so the install can be moved, rebuilt or re-deployed without touching data,
and so backups (§7.5) target one place.

## Decision
- **All persistent runtime state lives under one configurable base data directory**, kept **outside**
  the code/install tree ("a folder above"). Nothing persistent is written inside the repo or the
  binary's install dir.
- Resolved from config, overridable by env **`FLEET_DATA_DIR`**; **default: a `data/` directory one
  level above the binary's install dir**.
- Layout under the base dir (created on first run):
  - `config/` — server/agent config not supplied elsewhere
  - `ca/` — internal CA key + cert and issued certs (§4.4; part of the backup set)
  - `evidence/<machine>/` — segment files + manifest (§7.1)
  - `archive/` — main-server evidence archive (§7.3)
  - `registry/` — machine registry (§4.5)
  - `monitor/` — any local monitor buffers (the time-series store has its own path)
  - `logs/`
- The backup server's constant unique path (§7.5) mirrors this base dir.

## Consequences
- Every task that persists state reads the base dir from config (never hard-codes a path, never writes
  inside the install).
- `project.yml:real_data` paths are the sensitive subtrees of this base dir (D-0002).
