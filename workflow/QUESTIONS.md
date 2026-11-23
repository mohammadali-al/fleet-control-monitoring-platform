# Open questions to the operator

One line each, with the date asked. Removed when answered; the answer goes into a design note or a
decision.

- 2026-11-23 — Operator panel **UI language**: English is tentative (D-0003). Keep English, or Persian?
- 2026-11-23 — Doer **toolchain**: the brief front matter has a `codex` model slot. Is Codex also in
  use, or Claude-only? (Affects `model:` in briefs.)
- 2026-11-23 — Any **Go toolchain constraint** on the build/target machines (all Ubuntu)? Assuming a
  recent Go (1.23+) and CGO off for the agent unless a capture library needs it.
