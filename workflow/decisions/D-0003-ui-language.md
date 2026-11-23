# D-0003 — Operator panel UI language

**Status:** accepted · **Date:** 2026-11-23

## Context
The operator authors Persian pentest reports elsewhere, but this platform's panel is a separate
**internal operator tool** used by a single operator. The architecture document is written in English.

## Decision
- **English** for the operator panel's own words (`project.yml:ui_language: en`). Confirmed by the
  operator 2026-11-23.

## Consequences
- `guidance.md` wording rules key off `ui_language`; all UI strings are English.
