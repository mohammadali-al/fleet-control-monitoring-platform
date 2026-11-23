# Hooks

Enforcement lives in git hooks under `.githooks/` (wired with `git config core.hooksPath .githooks`,
which `wf start` also ensures). Claude Code session hooks may be added later to mirror the same guards
inside a doer session.

**Active today:** `pre-commit` blocks raw bidi/invisible characters (R-02) and hand-edits to
`workflow/generated/**` (R-03).

**Planned** (tracked in `workflow/rules/enforced.md`): Touches enforcement (R-01), notes/state guard
(R-04), no-delete (R-05), real-data guard (R-06). Hardening these is an early follow-up task — the
tooling is deliberately thin at bootstrap (see AGENTS.md).
