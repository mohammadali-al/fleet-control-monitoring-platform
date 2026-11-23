# Guidance (style & conventions — no enforcement)

- **Prose in plain English** in notes, briefs and logs. The operator panel's own UI words follow
  `project.yml:ui_language`.
- **Go:** standard layout (`cmd/`, `internal/`, `pkg/` only if truly shared externally). `gofmt` +
  `go vet` clean. Errors wrapped with context. `log/slog` for logging. Context-first function
  signatures. No panics in library code.
- **Small, named things.** New behavior goes in new files/packages where it can, so shared files
  change little (helps overlap merges, workflow §K).
- **Commits** are small and scoped to the task; message says what and why, not how.
- **Decisions** are one-per-file ADRs; when something visible must give way, that is its own question
  to the operator.

## Doer toolchain

- **Claude-only for now.** Briefs set `model: {claude: ...}`. Codex may be used later; the
  `model:` front matter can gain a `codex:` slot then, without any other change.
