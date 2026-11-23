# D-0003 — Operator panel UI language

**Status:** tentative · **Date:** 2026-11-23

## Context
The operator authors Persian pentest reports elsewhere, but this platform's panel is a separate
**internal operator tool** used by a single operator. The architecture document is written in English.

## Decision
- **Tentative: English** for the operator panel's own words (`project.yml:ui_language: en`).
- Revisit when the panel is actually designed; the operator may override (tracked in `QUESTIONS.md`).

## Consequences
- `guidance.md` wording rules key off `ui_language`. If this flips to Persian, UI strings become
  Persian while prose/notes stay English.
