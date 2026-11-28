---
id: T-0072
title: machine-label-samples
state: merged
touches: [workflow/design/machine-label-samples/]
tests: []
after: []
shares: {}
model: {claude: "sonnet high"}
operator_check: "workflow/design/machine-label-samples/ contains self-contained static HTML mock samples (not wired into the product) illustrating EACH machine-identity suggestion, using the panel's visual style and realistic mock data — including the collision case (several machines with the SAME username + SAME public IP but DIFFERENT private IPs). The operator can open the files in a browser and pick a direction. A README explains each sample."
---

## Why

Deciding how to identify machines in the panel (username is ambiguous: many machines share `root` + one
public IP behind NAT; the private IP disambiguates). Produce **mock samples only** — static HTML mockups,
no product wiring — so the operator can see each idea and choose. (This is design exploration like the
earlier `workflow/design/panel-hacker-samples/`; nothing is wired into the real panel.)

## What to build

Self-contained static **HTML mock samples** under `workflow/design/machine-label-samples/`, styled to match
the panel (inline the needed tokens/CSS so each file opens standalone; dark default, the round-5 look), with
**realistic mock data** that includes the **collision scenario**: ≥3 machines with the same username (`root`)
and the same public IP (`46.34.170.235`) but different private IPs (`10.0.0.5`, `10.0.0.9`, `10.0.0.21`).
One sample per suggestion (separate files or clearly separated sections):

1. **ID-as-primary-label:** a machines list where the **machine id/name** is the primary identifier and the
   username is a demoted attribute — side by side with a "username-primary (today)" version to show the
   ambiguity it fixes.
2. **Consistent `name · priv · pub` label everywhere:** the same machine-identity label shown across the
   contexts it would appear in — a **table row**, a **dropdown/target picker** (command runner), a
   **details header**, a **Maneuver agent-selection list**, a **terminal tab**, and a **compact chip** —
   demonstrating one consistent format.
3. **Reusable MachineLabel variants:** the label in several **format/density variants** (full / two-line /
   compact / chip / id-only-with-tooltip) so the operator can pick the preferred rendering.

Add a short `README.md` describing each sample and the trade-offs.

## Out of scope

- NO product code (no `web/src/`, no Go, no bundle) — mock HTML only under `workflow/design/`. No new
  component wired into the panel; no backend. This is a decision aid, not an implementation. (The real
  implementation would be a later task depending on T-0070's private-IP reporting.)

## Done means

- `workflow/design/machine-label-samples/` has standalone HTML mock(s) covering all three suggestions, each
  opening in a browser without external fetches, using the panel look and the collision mock data; a README
  explains them. No files changed outside `workflow/design/machine-label-samples/`.

## Facts this brief relies on

- Prior design-exploration pattern (samples under `workflow/design/`, operator picks): the earlier
  `workflow/design/panel-hacker-samples/` round files + their READMEs. The panel look/tokens to echo:
  `web/assets/css/tokens.css` / the round-5 design system (inline what you need so samples are standalone).
  The identity problem: machines can share username + public IP, differ by private IP (needs T-0070's
  primary-private-IP to populate in the real product — mock it here).

## Notes for the doer

- Mocks only — fast, static, standalone HTML; make them look like the real panel and show the collision case
  clearly. Run `./scripts/wf start T-0072` before committing; commit inside touches + the log. Do NOT
  push/merge/branch-delete or edit decisions/generated/other tasks/`state:`/product code (except via
  `wf start`).
