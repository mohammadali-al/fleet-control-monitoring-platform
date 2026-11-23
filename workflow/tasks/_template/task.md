---
id: T-XXXX
title: short-name
state: draft            # draft | ready | running | blocked | review | changes | merged | closed
touches: []             # files this task may change; new files are free
tests: []               # groups or files the doer runs (see project.yml:test.groups)
after: []               # task ids that must be merged first
shares: {}              # declared shared files, workflow §K
model: {claude: "sonnet high"}
operator_check: ""      # one line; what the operator verifies (empty if not operator-visible)
---

## Why
<!-- the reason this task exists, in one short paragraph -->

## What to build
<!-- Backend / Frontend subsections, or state why one side is untouched -->

## Out of scope
<!-- what this task must NOT do -->

## Done means
<!-- the checklist that makes this task acceptable -->

## Facts this brief relies on
<!-- every claim about existing code/design, each with file:line -->

## Notes for the doer
<!-- conventions, gotchas, pointers -->
