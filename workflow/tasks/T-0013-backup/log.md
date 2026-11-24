## T-0013 backup - doer log

**Built** (`internal/backup/{backup,diff,restore}.go`, constants in `internal/config/config.go`; `go.mod`/`go.sum`
unchanged - `golang.org/x/crypto/ssh` was already a dependency).

- **Target.** `Service.AddServer(ctx, Server{Host,Port,User}, password)`: connects over SSH (trust-on-first-use
  host key, pinned afterwards), creates the constant path, installs a freshly generated ed25519 key in the backup
  user's `authorized_keys`, verifies a key login, then stores only the key (`<data>/config/backup-credential`, 0600).
  If a key cannot be installed the password is kept in that same 0600 file as a recorded fallback (`auth: password`).
  The password buffer is zeroed, never logged, never in a remote command, and `config/` is not in the backup set.
  The result also carries the first data diff and the suggested schedule.
- **Constant path** (owned by the main server, no setting for it): `fleet-control-backup/` relative to the SSH login
  dir: `data/` (mirrors the base data dir), `history/` (earlier versions), `meta/index.json` (path, size, SHA-256).
- **Backup set** (under DataDir, D-0006): `archive/`, `monitor/`, `projects/` (Maneuver/project metadata; new
  `config.ProjectsDirName`), `registry/` (so a restore keeps machine identities - an addition beyond the brief's
  list, flag if unwanted), `ca/` (CA keys). `archive/*/.partial`, `*.tmp`, `*.fleet-part` are skipped.
- **Replication.** Additive only: archive segment digests come from the evidence archive manifests
  (`evidence.ArchiveManifest`), everything else is hashed; each upload streams to `<file>.fleet-part`, the backup
  runs `sha256sum` and the file is committed only if it equals the expected digest (else exit 3, the main-side
  temp is the only thing ever `rm`'d). A changed file's previous version is copied to
  `history/<path>.<sha256-of-old>` before the atomic replace; files gone from main stay on the backup.
  Sync is a plain exec-over-SSH protocol (POSIX sh, sha256sum, cat, cp, mv, mkdir, find, cut, dirname on the
  backup) - no sftp, no agent software.
- **Diff + schedule + now.** `Diff` (main set vs remote index: Missing, Changed, BackupOnly, Pending bytes),
  `SuggestSchedule()` = 02:30 daily, `SetSchedule`, `Run(ctx)` loop, `BackupNow` (TryLock: returns
  `ErrBackupRunning` instead of stacking).
- **Restore.** `Restore` (stored credential) and `RestoreWithPassword` (rebuilt main with no `config/`): lists the
  constant path, verifies every file against the backup's own `sha256sum`, atomic rename; paths are confined to the
  set dirs; a differing local file is kept and reported as a conflict unless `Overwrite`; CA files get 0600/0700.
- **Reminder.** `Reminder(now)`: at most once per local calendar day (date persisted), kinds `no-server` /
  `stale` (no OK backup in 24 h); only takes the state mutex, so it never waits on a running backup.

**Tests** (`internal/backup/backup_test.go`): an in-process SSH server that runs the real POSIX commands in a
temp home (throwaway creds). Covers: add records + pins host key + creates the constant path + first diff +
suggestion; password not stored when key install works, fallback when keys are refused; additive backup with
byte-identical result, second run sends nothing, no `rm` beyond the upload temp; changed file -> history kept,
deleted-on-main file stays on backup; pre-existing backup file preserved; transfer corruption (byte flipped in
flight) detected and not committed; source differing from its manifest digest reported; diff; schedule +
persistence + the nightly loop actually running a backup; `BackupNow` not stacking; reminder once/day, persisted,
non-blocking during a backup, stale after 36 h; round trip backup -> wipe -> import reproduces every file
(incl. CA key, mode 0600) and is idempotent; conflicts; restore on a fresh server; host-key pinning; input
validation and password zeroing; path-safety of restore. `go test -race` clean.

**Self-check:** `gofmt -l .` empty; `go vet ./...` clean; `scripts/build.sh` ok (bin/ removed afterwards);
`scripts/test.sh server` pass.

**Known limitations / follow-ups:** (1) `Diff` trusts the remote `meta/index.json` (written by main after each
verified upload); a file removed on the backup by hand is noticed only when its content is probed on the next run
if the index was lost - a deep re-verify of the backup is not built. (2) Frequently-changing files (e.g. growing
monitor logs) get one `history/` version per changed night; unlimited retention matches the "never delete" rule
but a retention/compaction policy is a later decision. (3) Panel wiring (Backup page, surfacing `Reminder`,
starting `Run` from the server) is a later task.

<!-- wf: commits=2c3a8af left=- tests=server:pass blocked=no -->
