---
id: T-0044
title: architecture-gap-audit
state: ready
touches: [workflow/tasks/T-0044-architecture-gap-audit/log.md]
tests: []
after: []
shares: {}
model: {claude: "sonnet high"}
operator_check: "A gap-audit report exists OUTSIDE the repo at /home/bierellow/Research/Architecture/Architecture_Gap_Audit.md listing every behaviour the architecture doc specifies that is MISSING / PARTIAL / STUBBED in the current code — each with the architecture reference (quote + section) and the code evidence (file:line or 'none') and a gap type. Findings only, no fixes, no decisions, no recommendations. The repo log.md has only a terse count; no detailed gaps are committed to the (public) repo."
---

## Why

We just found a feature the architecture requires but the code only stubs (the panel's public-IP manual
refresh). There are likely others left un-built. Produce a **complete gap audit**: everything the
**architecture file** specifies that is **missing, partial, stubbed, or deviates** in the current project.
**Finding is the whole deliverable** — no fixes, no decisions, no recommendations, no new tasks.

## What to build

A read-only audit (no product code changes):

1. **Read the architecture (authoritative):**
   `/home/bierellow/Research/Architecture/Fleet_Control_Monitoring_Platform_Architecture.md` — it lives
   OUTSIDE this repo (kept private). `Fleet_Control_Monitoring_Platform_Architecture.v1.1.bak.md` in the
   same folder is an **older backup** — ignore it except to disambiguate. Go through it systematically:
   the acceptance-criteria / capability tables, and each chapter (channel, onboarding/enrollment, command
   exec, evidence capture, monitors, watchdog, backup, panel pages, deployment, reflector/public-IP, etc.).
2. **Compare against the current code** in this worktree (`internal/`, `cmd/`, `web/`, `deploy/`,
   `scripts/`). For each architecture-specified item classify it: **implemented** (skip), or a gap —
   **missing** (no code), **partial** (incomplete vs spec), **stub** (UI/endpoint present but inert), or
   **deviation** (built differently than specified).
3. **Write the report** to `/home/bierellow/Research/Architecture/Architecture_Gap_Audit.md` (OUTSIDE the
   repo — the architecture docs are deliberately excluded from the public repo, so the gap analysis stays
   with them). Structure it by architecture chapter; for **each gap** give:
   - the **architecture reference** (section / line + a short quote of the requirement),
   - the **code evidence**: `file:line` of what exists, or `none`,
   - the **gap type** (missing | partial | stub | deviation) and a one-line factual description.
   Start with a summary table (counts by type) and a one-line-per-gap index.
4. **Repo log only:** append `workflow/tasks/T-0044-architecture-gap-audit/log.md` with a **terse**
   summary — total gaps and the count per type, and the report's path — ending `wf: T-0044 done`. Do
   **not** put the detailed gap list in the repo (it is public).

## Out of scope

- No fixes, no code changes (except the log), no edits to the architecture file, no decisions /
  recommendations / prioritisation / new task briefs. Don't commit the report into the repo. Don't
  invent requirements not in the architecture; "specified" means actually written there.

## Done means

- `/home/bierellow/Research/Architecture/Architecture_Gap_Audit.md` exists with a structured, chapter-
  grouped gap list, each gap carrying arch-reference + code-evidence (`file:line`/`none`) + type, plus a
  counts summary. Every listed gap is real (verified against both the doc and the code).
- The repo has **no** changed files except `workflow/tasks/T-0044-architecture-gap-audit/log.md` (terse
  summary + counts + report path). Known examples that MUST appear if still true: public-IP manual refresh
  is a stub (`web/pages/machines.html:10`, agent `capture.RefreshPublicIP` exists but is never reported);
  token enrollment off by default (installer sets no `enroll_addr`).

## Facts this brief relies on

- Architecture doc path + that it is private/out-of-repo: `/home/bierellow/Research/Architecture/` (moved
  out of the repo earlier; the `.bak` is an older version). Reflector/public-IP manual refresh is arch
  §6.4 / the Machines-page spec.
- Current code roots: `internal/`, `cmd/`, `web/`, `deploy/`. Server-side channel dispatch is
  `internal/fleet/fleet.go`; panel pages are `web/pages/*.html`; deploy is `deploy/`.

## Notes for the doer

- Be thorough and **factual** — quote the architecture, cite `file:line`, state only what is or isn't
  there. This audit's value is accuracy and completeness, not judgement. You run inside a git worktree:
  read the code here, but WRITE the report to the absolute parent-folder path above (it is intentionally
  outside the repo). Run `./scripts/wf start T-0044` before committing the log; commit only the log inside
  touches. Do NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via
  `wf start`). A hook refusal means stop + report verbatim.
