# Enforced rules

Each rule names the hook/check that enforces it. **A refusal means stop.** `[planned]` = agreed but
not yet wired (the tooling is young; see AGENTS.md).

- **R-01 Stay inside Touches.** A commit may change only files listed in the task's `touches`, plus
  the free-by-default set (new files, the task's own folder, tests in the task's group, fixtures,
  built bundles). `[hook: pre-commit/touches — planned]`
- **R-02 No raw bidi/invisible characters** in any committed text. Write them as escapes.
  `[hook: pre-commit/bidi — active]`
- **R-03 Generated files are off-limits by hand.** `workflow/generated/**` is written only by `wf`.
  `[hook: pre-commit/generated — active]`
- **R-04 Doers do not edit state or notes.** `state:` front matter, other tasks' folders, the
  decisions log, and `QUESTIONS.md` are the supervisor's. `[hook: pre-commit/notes — planned]`
- **R-05 No deletes of tracked files** on a doer branch unless the brief says so.
  `[hook: pre-commit/no-delete — planned]`
- **R-06 Real/sensitive data is off-limits** to doers (D-0002): paths in `project.yml:real_data`,
  the internal CA private keys, agent SSH credentials, backup-server credentials.
  `[hook: pre-commit/real-data — planned]`
- **R-07 Nothing left running.** The doer leaves no background process, server, or temp dir behind
  except those named `left:` in its log footer (which `wf merge` cleans). `[review check]`
- **R-08 The brief's facts hold.** Every `file:line` fact in the brief must still be true at review.
  `[wf audit — planned]`
