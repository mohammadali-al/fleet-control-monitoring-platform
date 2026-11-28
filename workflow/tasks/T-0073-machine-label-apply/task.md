---
id: T-0073
title: machine-label-apply
state: ready
touches: [web/src/components/, web/src/pages/MachinesPage.jsx, web/src/pages/CommandsPage.jsx,
          web/src/pages/TerminalPage.jsx, web/src/pages/ManeuverPage.jsx, web/src/pages/WatchdogPage.jsx,
          web/src/hooks/, internal/panel/server_test.go]
tests: [server]
after: [T-0047]
shares: {}
model: {claude: "sonnet high"}
operator_check: "A small reusable machine-identity label (name · private-IP · public-IP) is used in the SPECIFIC small spots where a machine is identified/picked across the panel — so machines that share a username + public IP (behind NAT) are told apart by their private IP. It is applied surgically to those spots only (not a page redesign, not a copy of the sample mockup), reuses the real design-system classes, and degrades gracefully when a private IP isn't reported yet."
---

## Purpose of the chosen sample (why this exists)

`workflow/design/machine-label-samples/02-consistent-label.html` (the approved direction) demonstrates a
**consistent machine-identity label** rendered the same way everywhere a machine is referenced:
**`name · private-IP · public-IP`**. The problem it solves: **username is a weak identifier** — many machines
share the same username (e.g. `root`) and the same **public IP** behind one NAT, so you can't tell them
apart; the **private IP is the disambiguator**. The label makes machine identity unambiguous and consistent
across the UI.

**Scope discipline (read carefully):** this label is for **specific tiny areas only** — the small places
where a machine is named or picked. **This is NOT a copy of the sample** (don't reproduce its demo layout or
its multi-context showcase) and **NOT a page redesign**. Build one small reusable element and drop it into
the identified spots, changing as little else as possible.

## What to build (front-end only)

1. **A reusable component** `web/src/components/MachineLabel.jsx` (+ any tiny CSS, reusing existing tokens/
   classes — no new design language): renders a machine's identity as `name · priv · pub` with a couple of
   size variants (e.g. `full` and `compact`). Data: machine `name`/`id`, the **primary private IP**
   (`primary_private_ip` from T-0070 when present, else first `private_ips`, else omit the priv part), and
   `public_ip`. **Degrade gracefully** when a field is missing (never render empty `· ·`). Use real classes
   (`chip--mono`, muted text, etc.), not invented ones.
2. **Apply it surgically** in the small identity spots only — replace the existing ad-hoc machine-identity
   text with `<MachineLabel>` in:
   - the **Machines** table primary/identity cell and the details-drawer header (`MachinesPage.jsx`),
   - the **Command runner** target picker items (`CommandsPage.jsx`),
   - the **Terminal** tab/session label (`TerminalPage.jsx`),
   - the **Maneuver** agent-selection list items (`ManeuverPage.jsx`),
   - the **Watchdog** per-host identity cell (`WatchdogPage.jsx`).
   Keep each change minimal — only the identity element, nothing else about those pages/components.
3. Rebuild + commit the web bundle (`./web/build.sh`).

## Out of scope

- No backend change (consumes existing machine fields; `primary_private_ip` is populated by T-0070 — this
  task just uses it when present). No new pages, no layout/IA changes, no restyle beyond the label element,
  no reproducing the sample mockup. Don't touch non-identity parts of those pages.

## Done means

- One `MachineLabel` component used in the listed spots; machines with shared username+public-IP are
  distinguishable by private IP wherever they're shown/picked; the label reuses real design-system classes,
  degrades gracefully, and the rest of each page is unchanged.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` passes;
  web bundle rebuilt + committed. A quick manual pass of each touched spot in both themes.

## Facts this brief relies on

- Approved sample + its purpose: `workflow/design/machine-label-samples/02-consistent-label.html` +
  `README.md`. Machine fields available: `name`/`id`, `private_ips`, `public_ip` (+ `primary_private_ip`
  once T-0070 lands) — `internal/registry/registry.go`; rendered in `web/src/pages/MachinesPage.jsx`. The
  SPA pages that reference machines: Machines/Commands/Terminal/Maneuver/Watchdog under `web/src/pages/`.
  Design-system classes: `web/assets/css/{tokens,components}.css`.

## Notes for the doer

- **Surgical** — smallest diff that puts the identity label in those spots; do not redesign or copy the
  sample. Reuse existing classes. Rebuild + commit the bundle. Run `./scripts/wf start T-0073` before
  committing; commit inside touches + the log. Do NOT push/merge/branch-delete or edit decisions/generated/
  other tasks/`state:` (except via `wf start`).
