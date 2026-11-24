---
id: T-0013
title: backup
state: ready
touches: [internal/backup/backup.go, internal/backup/diff.go, internal/backup/restore.go,
          internal/backup/backup_test.go, internal/config/config.go, go.mod, go.sum]
tests: [server]
after: [T-0009]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

Protect the one-server archive: additive replication of the main server's data to a **storage-only**
backup server, with a manual backup, an import/restore, and a daily reminder. Includes the CA keys so a
restore doesn't force re-enrollment.

## What to build

**Backend (Go), main-server side.**

1. **Backup target** (`backup.go`): add a backup server by **username + password + IP**; the main server
   owns the connection over **SSH**. The destination is a **constant, unique path** provided by the main
   server (not operator-chosen).
2. **Replication** (`backup.go`): **additive only** — nothing is ever deleted on the backup. Reuse the
   segment **manifest + SHA-256** to transfer only new/changed files and **verify checksums**. Backup
   set = evidence **archive**, **monitor data**, **Maneuver/project metadata**, and the **internal CA
   keys** (all under `DataDir`, D-0006).
3. **Diff + schedule** (`diff.go`): compute the **data diff** between main and backup; **suggest a
   schedule around the middle of the night**. Expose a **manual "back up now"** trigger.
4. **Import/restore** (`restore.go`): restore data **from the constant path** on the backup server.
5. **Reminder:** a **non-intrusive, at-most-once-a-day** reminder surfaced to the panel; never interrupts.

## Out of scope

- A standby/failover main server — the backup is **storage only** (decision #37).
- Panel backup UI (wiring is a later task; the static Backup page exists).
- The backup server running any agent software — it is a plain SSH storage target.

## Done means

- Adding a backup server records it; a backup transfers the set to the constant path, **additively**
  (an existing backup file is never deleted/overwritten destructively) with **SHA-256 verification**.
- The diff reports what main has that backup lacks; a nightly schedule is suggested; "back up now" runs
  on demand.
- Import restores the set (including CA keys) from the constant path; a round-trip (backup → wipe a
  local copy → import) reproduces the data.
- The reminder fires at most once per day and is non-blocking.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes (against a local in-process
  SSH/stub target; throwaway creds per D-0002).

## Facts this brief relies on

- Storage-only backup; additive SSH replication to a constant unique path; diff + nightly suggestion;
  manual backup + import; CA keys included: `Fleet_Control_Monitoring_Platform_Architecture.md:85`, `:325`, `:329`, `:330`, `:331`.
- CA keys are part of the backup set (else re-enrollment): `Fleet_Control_Monitoring_Platform_Architecture.md:138`, `:139`.
- Reuses the evidence manifest + SHA-256 (T-0009): `workflow/tasks/T-0009-evidence-sync-export/task.md`.
- Data lives under the base data dir: `workflow/decisions/D-0006-data-directory.md`.

## Notes for the doer

- Never hold or log the backup-server password beyond what SSH needs; it is sensitive (D-0002).
- The constant path is defined and owned by the main server — document it; do not let the operator set it.
- Prefer the existing manifest/SHA-256 code over a new sync path.
