# T-0004 log

## 2026-11-24 - Phase 1 (design + 2 proof pages)

- Cloned `nextlevelbuilder/ui-ux-pro-max-skill` (commit f2f7266) into the session scratchpad (outside the
  repo) and ran its search tool offline with generic queries: `--design-system` (density 8, variance 3,
  motion 2), `ux` (badge/chip, form errors, dark-mode contrast), `chart` (real-time monitoring), plus its
  quick-reference rules. Details of what was taken and where I deviated are in `workflow/design/panel.md` section 1.
- Wrote `workflow/design/panel.md` (status `exploring`): IA for all ten pages, visual system (grid, nav,
  light+dark tokens, type, spacing, components), vocabulary mapping, Go `html/template` port notes, open questions.
- Built `web/`: `machines.html`, `maneuver.html` (non-functional stub, D-0005), `styleguide.html` (reviewer aid,
  not a panel page), `index.html` (redirect), `assets/css/{tokens,base,components}.css`, `assets/js/app.js`.
  Static, mock data, no network calls, English UI, light/dark via `data-theme`, responsive, keyboard-navigable.
- Self-check: no build/test group applies (static files). Rendered both proof pages and the style guide in
  headless Chrome in light and dark at 1440 px, plus 820 px and 390 px; ran a scripted WCAG contrast check
  over all token pairs (all pass after darkening `--border-strong` in light); `node --check` on `app.js`.
- Known gap by design: sidebar links to the eight unbuilt pages 404 until phase 2.
- Stopped here for supervisor review. Phase 2 not started.

<!-- wf: commits=45239fe left=- tests=n/a blocked=no -->

## 2026-11-24 - Phase 2 (remaining eight pages)

- Built `projects.html`, `commands.html`, `terminal.html`, `baseline.html`, `monitor.html`, `watchdog.html`,
  `evidence.html`, `backup.html` on the approved shell and components. Static, mock data, English, light and dark,
  responsive, keyboard-navigable, no network calls.
- Review answers applied: Q5 Machines row menu (Details, Re-provision, Remove; stub); Q6 disk thresholds are 80% warning
  and 90% critical / intake paused (Machines meters and note fixed, same on Evidence archive and styleguide); Q7 dev-only
  `scripts/stamp-web.py` stamps all pages from `web/_src/shell.html` + `web/_src/pages/*.html` (`--check` mode available);
  committed output stays plain static HTML. Monitor-job create form puts Origin / URL first and most prominent, IP required
  below. Signature interactions mocked as stubs: monitor command overlay (hover and focus tooltips) and semantic-zoom
  history (Overview / Day / Hour), evidence pull/sync status, confirm-to-start without a monitor, dangerous-command gate.
- New shared components: tabs, row menu, dialog, switch, steps and escalation ladder, tooltip, overlay lane, zoom bar,
  code block, frame placeholder, run card (all in `components.css`, shown in `styleguide.html`). One new token
  `--term-err`. `app.js` gained tabs, menus, dialogs, tooltips, semantic-zoom levels, switch labels.
- Self-check: no build/test group applies. `stamp-web.py --check` clean. Scripted Chrome DevTools Protocol smoke test
  over all 11 pages: loads, no JS errors, no external URLs, theme toggle, menu (open, focus, Esc, item), dialog
  open/close, tabs (click and arrow key), zoom levels and label, tooltip on focus, switch label, stub toast, Machines
  filter: all pass. Rendered every page in light/dark at 1440 px and spot-checked 820 and 390 px. Contrast script re-run
  (tokens plus terminal colours): all pairs pass. No leftover processes; temp files only in the session scratchpad.
- Open items (also in design note section 9): watchdog threshold percentages are placeholder values; per-machine and
  per-target monitor views are visual only.

<!-- wf: commits=86d51e7 left=- tests=n/a blocked=no -->
