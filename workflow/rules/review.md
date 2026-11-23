# Review rules — what the supervisor checks

- `task.md` unchanged; `log.md` only grew.
- Commits match the log footer; nothing outside `touches`.
- Trial merge in a detached worktree is clean.
- `scripts/build.sh` reproduces identical binaries (when `build.must_be_reproducible`).
- Tests: the whole suite on every trial merge **(current policy)**. (Alternative: whole suite every
  N merges and before every deploy — change this line to switch; `wf merge` reads it.)
- For UI work: a `check-app` smoke run with the brief's steps.
- The brief's "Done means" holds; the facts still check out (`wf audit`).
- Wording, naming and design-rule conformance (the review catches what the checks cannot).
