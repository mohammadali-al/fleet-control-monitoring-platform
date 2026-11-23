# AGENTS.md — Fleet Control & Monitoring Platform

This repository is built with a **supervisor–doer workflow** (see `~/Desktop/workflow-proposal.md`
for the full rationale). One page; read it, then the rules that bind you.

## Roles

- **Supervisor** (one session): writes samples and briefs, reviews, merges, bumps/tags, keeps the
  decisions log and the board. Talks to the operator. Never does a task's coding on a doer branch.
- **Doer** (one session, one task, one branch): does exactly one task, inside its declared `touches`.
  **A refusal from a hook means stop** — report it, don't work around it.

## Where things are

| Path | What |
|---|---|
| `Fleet_Control_Monitoring_Platform_Architecture.md` | The master design (source of truth). |
| `launchkill-sim/` | Load Controller tuning harness (architecture §10). |
| `workflow/rules/enforced.md` | The rules that bind a doer. Read this + this file, nothing else unless the brief points to it. |
| `workflow/rules/review.md` | What the supervisor checks at review. |
| `workflow/rules/guidance.md` | Style, wording, conventions (no enforcement). |
| `workflow/rules/why.md` | One line of reason per rule id. |
| `workflow/tasks/T-XXXX-*/` | `task.md` (brief), `log.md` (doer, append-only), `review-N.md`, `audit.md`. |
| `workflow/design/` | One note per feature; `samples/` for UI samples. |
| `workflow/decisions/` | One decision per file (ADR style). |
| `workflow/generated/` | `STATUS.md`, `CHECKS.md` — written by `wf`, **never by hand**. |
| `workflow/QUESTIONS.md` | Open questions to the operator, one line each. |
| `workflow/project.yml` | The only file that names the stack. |
| `scripts/wf` | The one workflow command. |

## The doer's five steps

1. **`wf start T-x`** — creates/checks out the branch, wires the hooks, prints the brief's
   dependencies and what to read. If it stops, you stop.
2. **Read** `task.md` and `workflow/rules/enforced.md`.
3. **Build** only what the brief says, only inside `touches` (new files are free).
4. **Self-check** — build, the task's tests (`scripts/test.sh <group>`), and (for UI) `check-app`.
5. **Log + commit** — append a `log.md` entry ending in the `wf:` footer, commit **inside touches**.

## Status of the tooling (be honest)

The `wf` command and `.githooks/` are **freshly written for this project and still thin** — not yet
copied-and-matured from a prior repo. What is enforced today vs. planned is listed in
`workflow/rules/enforced.md`. Hardening them is an early follow-up task. Until then the supervisor
reviews by hand against `workflow/rules/review.md`.
