# Review 1 — T-0004 panel-frontend (Phase 1)

**Verdict: APPROVED — proceed to Phase 2.**

## What I checked
- Read `workflow/design/panel.md` (IA for all 10 pages, visual system, vocabulary mapping, Go
  `html/template` port, a11y).
- Rendered both proof pages in headless Chrome (light) and reviewed them visually.
- Cross-checked against the architecture: vocabulary (main server / worker agent / monitor agent /
  Maneuver / baseline item / `connect_mode` / evidence log / monitor data), the Maneuver model
  (patterns as **ceilings**, cores/GB, mirrored worker agents, aligned start), and D-0005 (the
  Maneuver page is a non-functional, clearly-labelled stub).

Quality is high and the method (UI/UX skill applied, then deliberate, documented deviations) is sound.
No changes required before continuing.

## Answers to the open questions
- **Q1 (nav 404 until Phase 2):** Acceptable — Phase 2 builds all eight remaining pages, so links
  resolve. No Phase-1 placeholders needed.
- **Q2 (fonts):** Keep **system font stacks**. Offline is a hard requirement; don't add web fonts.
- **Q3 (accent):** **Confirmed** — blue accent, green reserved for ok/online. Good call.
- **Q4 (density):** Keep **14 px body / 36 px controls** (desktop ops console). No change.
- **Q5 (Machines "More" kebab):** **Define it** as a small stub menu: **Details · Re-provision ·
  Remove** (re-provision = idempotent re-enroll, Remove = decommission + cert revoke, architecture
  §4.5 / §14). No rename.
- **Q6 (disk thresholds):** Use the architecture's real values — the disk guard is **80 % warn /
  90 % pause-intake** (§6.5, §7.3). Set **high = 80 %, critical = 90 %** (not 95 %), and fix the
  existing `machines.html` meters to match.
- **Q7 (shared shell):** Add a **tiny stamp script** (dev-only, no runtime dependency) that generates
  the pages from one shell source, so the shell can't drift across 10 files. Committed output stays
  plain static HTML that works from `file://`.

## Phase-2 notes (in addition to the brief)
- **Monitor data page:** the monitor-**job** create form must put **origin above IP** as the more
  prominent field (architecture §8.2: ip required, origin optional but shown first). (The Maneuver
  target block keeping IP-first is fine; this applies to the monitor-job form specifically.)
- **Mock the signature interactions** as stubs where they define the page: the monitor timeline's
  **command-overlay hover** and **semantic-zoom history** (§8.4), and the evidence **pull/sync status**.
- **Keep `styleguide.html`** — it's a useful reviewer aid; keep it in sync as components are added.
- Keep everything **static/mock, no backend**, English, and re-run the contrast script when tokens change.

Proceed with Phase 2: build the eight remaining pages on the same shell/components.
