# Operator panel — front-end design

**Status:** built (T-0004 phase 2 done, awaiting review) · **Task:** T-0004 · **Decisions:** D-0001 (stack),
D-0003 (English UI), D-0004 (`:6789`), D-0005 (Maneuver execution deferred) · **Source of truth:** architecture §12

Phase 1 (design + two proof pages) was approved in `review-1.md`. Phase 2 built the other eight pages on the
same shell and components. All ten pages are static, mock-data, English, light and dark, and work from `file://`.
`web/styleguide.html` is a reviewer aid showing every shared component; it is not a panel page.

How to look at it: open any page under `web/` straight from the filesystem (no server, no network), starting at
`web/index.html` (redirects to Machines). Toggle light/dark with the button at the top right.

---

## 1. Method (UI/UX reference skill)

The reference skill is `nextlevelbuilder/ui-ux-pro-max-skill` (cloned at commit `f2f7266`). Its
search tool was run offline against generic queries only (no project data). What was taken from it:

| Skill step | Query / source | What it decided here |
|---|---|---|
| Design system (`--design-system`, dials `density 8, variance 3, motion 2`) | "internal ops console fleet monitoring dashboard dark mode" | Style **Minimalism & Swiss** (light + dark both supported; "enterprise apps, dashboards, professional tools"). Slate neutral base, high contrast, grid-based, sharp hierarchy, subtle 150-250 ms transitions. Dense spacing scale for a dashboard. |
| Typography | system suggestion Fira Sans / Fira Code | **Kept the intent, changed the delivery:** system font stacks (`Fira Code` is in the mono stack and is used when installed). A Google Fonts import would break the "works offline" requirement. See open question Q2. |
| `ux` domain | "badge chip label wraps", "form label error validation", "dark mode contrast" | Chips never wrap (`nowrap`); visible labels on every field, errors under the field with `aria-invalid`; text >= 4.5:1, UI borders >= 3:1, focus ring >= 2 px. |
| `chart` domain | "real-time dashboard monitoring" | Live monitor charts: current value + status text beside the chart, series distinguished by dash pattern as well as colour, pause/resume control, a data-table fallback, 60 s buffered window. Applied to the chart placeholder and legend. |
| Quick reference | `nav-label-icon`, `nav-state-active`, `nav-hierarchy`, `empty-nav-state`, `color-not-only`, `color-dark-mode`, `reduced-motion`, `touch-target-size` | Icon + label nav with grouped sections and a clear current-page marker; unavailable actions explain why (e.g. no terminal on a monitor agent); status is icon + text, never colour alone; dark mode uses tonal variants, not inverted colours, contrast tested per theme; `prefers-reduced-motion` honoured; 44 px targets on coarse pointers. |
| Pre-delivery checklist | skill checklist | No emoji icons (inline SVG), pointer cursors, visible focus, responsive at 390 / 820 / 1440, contrast check run on every token pair (scripted, all pass). |

Deviations from skill output, on purpose: the skill's palette used **green as the accent**; here green is
reserved for "ok/online" so an action colour never reads as a health colour, so the accent is blue.
Body text is 14 px (skill default is 16) because this is a dense operator tool; nothing is below 12 px.
Default control height is 36 px on a mouse, 44 px on touch.

---

## 2. Principles

1. **Visible by default** (arch. §12.3). Every command, refusal, capture, watchdog action, Maneuver
   step, and sync appears in the panel. Stubbed controls are still on screen; clicking one raises a
   toast saying why nothing happened. Nothing is hidden or silent.
2. **One operator, dense, calm.** Single full-access role, no login screen (auth proxy sits in front).
   Information-dense tables, restrained colour, status always legible without colour.
3. **Honest about state.** A "Mock data" chip is in the top bar while the data is fake; "Stub" marks the
   Maneuver nav item; execution controls say so. Remove the chip when the real backend is wired.
4. **Ports cleanly.** Plain HTML + CSS variables + a few lines of vanilla JS. No framework, no build.

---

## 3. Information architecture

Primary navigation is a left sidebar (page list = arch. §12.2, same order, grouped into four labelled
sections). A global **active-test indicator** sits in the top bar (arch.: one active test system-wide).

| Group | Page | File | Contents (arch. §12.2) |
|---|---|---|---|
| Operate | **Machines** (proof) | `machines.html` | Summary tiles (worker agents, monitor agents, needs attention, disk warnings); filterable registry table (name + machine ID, role, state, `connect_mode`, private and public IP with last-checked time, disk meter, last seen); per-row refresh public IP, open terminal (disabled with a reason for monitor agents and offline machines), more; "Refresh public IPs" and "Onboard machine" (inline form: IP, SSH username, password or key, role, connect mode). |
| Operate | Projects & tests | `projects.html` | Project list; active-test banner (one system-wide); completed tests by date; test detail link to evidence and report. |
| Operate | Command runner | `commands.html` | Mode A one-shot: worker-agent picker, command box, confirmation gate for risky commands, live output streams (one per worker agent), history table, fan-out grid. Serial per-agent queue shown with queue position. |
| Operate | Terminal | `terminal.html` | Mode B: xterm.js placeholder per session, tabs, sessions list with terminate. Monitor agents are not selectable (no shell role). |
| Plan | Baseline items | `baseline.html` | Global preset library: table plus create/edit form (`name`, `command_template` required; `description`, `rate`, `duration`, `ip`, `domain`/`url`, `warmup` optional). |
| Plan | **Maneuver** (proof, stub) | `maneuver.html` | Maneuver identity and target `(ip, origin)`, total time, load-pattern chooser (all-in-one, linear, spikes) with parameters, timeline (ruler, load-ceiling lane, one lane per baseline item), worker-agent selection, library of baseline items, Load Controller card, per-item/live resource view (cores / GB). |
| Observe | Monitor data | `monitor.html` | Merged, per-machine, per-target charts; 60 s live window; command-overlay hover; semantic-zoom history; probe config editor. Chart placeholder or embedded Grafana frame. |
| Observe | Watchdog | `watchdog.html` | Policy editor, per-host stage and events, counter reset. |
| Observe | Evidence | `evidence.html` | Session browser, per-session packet records, pull button with sync status, exports. |
| Administer | Backup | `backup.html` | Add backup server, diff view, suggested nightly schedule, manual backup, import, daily reminder. |

**Signature interactions (all stubs, all keyboard-reachable):**
- *Monitor data*: command overlay (hover or focus a command marker, or hover the chart, to see the commands and packet
  counts for that interval; arch. 8.4) and semantic-zoom history (Overview / Day / Hour levels with merged labels).
  The monitor-job create form puts **Origin / URL first and most prominent**, IP required below it (arch. 8.2).
- *Evidence*: per-agent pull with live sync status (pulling, behind, synced, agent offline), segment manifest with
  SHA-256 verification state, export (pcapng / CSV / JSON).
- *Projects & tests*: confirm-to-start dialog when a test has no monitor (arch. 8.2).
- *Command runner / Terminal*: dangerous-command confirmation gate (dialog in Mode A, in-terminal prompt in Mode B;
  enforced agent-side, arch. 4.8), per-agent serial queue shown as queued / running / exited.
- *Machines*: row menu (stub): Details, Re-provision (idempotent re-enroll, same machine ID), Remove (decommission and
  revoke the client certificate).
- *Backup*: once-a-day non-intrusive reminder, diff view, suggested nightly schedule, back up now, import (confirm).
- *Watchdog*: policy as form or YAML, escalation ladder with per-step switches, guards, per-host stage and counters,
  events log including attempts to stop the watchdog.

**Page anatomy** (all pages): skip link -> sidebar (brand, nav, server footer) -> top bar (menu button
on narrow screens, active-test chip, mock-data chip, theme toggle) -> content (`page-head` with title,
one-line description, primary actions; then cards on a 12-column grid).

**Navigation rules:** nav is flat (two levels at most); the current page has `aria-current="page"` and
an accent bar; destructive actions never live in the nav; an unavailable destination or action says why.

---

## 4. Visual system

### 4.1 Layout grid
- App shell: fixed 15 rem sidebar + fluid main; top bar 3.5 rem; content max-width 90 rem, 1.5 rem padding.
- 12-column grid, 1 rem gap (`.grid`, `.col-N`); the Maneuver page uses a main column + 20 rem right
  rail (`.split`). Rails and columns stack below 1200 px.
- Breakpoints: **>= 1200** full layout; **960-1199** single column of cards, sidebar visible;
  **< 960** sidebar becomes an off-canvas drawer (menu button, Esc and outside-click close, `aria-expanded`);
  **< 600** tiles go full width. Wide tables scroll horizontally inside their card with the first column
  sticky; the timeline scrolls horizontally with sticky lane labels. No page-level horizontal scroll.

### 4.2 Colour tokens
All colour lives in `web/assets/css/tokens.css` as CSS variables; nothing else uses raw hex. Theme is
`<html data-theme="light|dark">`, set before first paint, persisted in `localStorage`, defaulting to the OS
preference; with JS disabled the OS preference still applies through `prefers-color-scheme`.

| Token | Light | Dark | Use |
|---|---|---|---|
| `--bg` | `#f4f6fa` | `#0b1220` | App background |
| `--surface` / `--surface-2` / `--surface-3` | `#fff` / `#eef1f7` / `#e3e8f1` | `#141d30` / `#1b263c` / `#243250` | Cards and inputs / table header, hover / pressed, bar tracks |
| `--border` / `--border-strong` | `#d3d9e4` / `#818da3` | `#2a3753` / `#6a7a98` | Dividers / control borders (>= 3:1) |
| `--text` / `--text-muted` / `--text-faint` | `#0f172a` / `#475569` / `#5b6a7f` | `#f1f5f9` / `#aab7cb` / `#8d9bb2` | Body / secondary / captions (all >= 4.8:1) |
| `--accent` (+ `-hover`, `-soft`, `--on-accent`) | `#2158d6` | `#6aa5ff` | Primary action, selected nav, selection |
| `--ok` / `--ok-soft` | `#15703a` / `#dcf5e4` | `#5fd38a` / `#12301f` | Online, healthy |
| `--warn` / `--warn-soft` | `#8f4a05` / `#fdecc8` | `#f2b84b` / `#382a0d` | Reconnecting, high disk |
| `--danger` / `--danger-soft` | `#b42318` / `#fde4e1` | `#ff8b82` / `#3d1a19` | Offline, critical, destructive |
| `--info` / `--info-soft` | `#1d5aa8` / `#dcebfb` | `#7db8ff` / `#14284a` | Informational, monitor-agent role |
| `--series-1..5` | blue, amber, teal, violet, pink | lighter tonal variants | Chart series (plus dash patterns) |

Every foreground/background pair was checked by script: text >= 4.8:1, status chips >= 5.3:1, accent
button >= 6.1:1, control borders >= 3.3:1 in both themes. The terminal placeholder is dark in both themes
(`--term-*`), as terminals are.

### 4.3 Typography
- Sans: `ui-sans-serif, system-ui, ...` (offline-safe). Mono (IPs, IDs, commands, `connect_mode`):
  `ui-monospace, "Fira Code", ...`.
- Scale: 12 (chips, table header, captions, the floor) / 13 (secondary) / **14 body and tables** / 16 card
  title / 20 section / 24 page title. Line height 1.5 body, 1.25 headings. Numbers in tables use tabular
  figures. Table headers are small caps-style uppercase labels.

### 4.4 Spacing, shape, motion
- 4 px base: 4 / 8 / 12 / 16 / 24 / 32 / 48 (`--sp-1..7`). Radius 4 / 8 / 12. Single soft shadow for cards,
  a larger one for drawers and toasts.
- Motion: 120-200 ms ease-out on colour/background/transform only; the toast is the only animation
  besides the terminal cursor. All of it is switched off under `prefers-reduced-motion`.

### 4.5 Core components (all in `web/assets/css/components.css`, shown in `web/styleguide.html`)
| Component | Notes |
|---|---|
| **Card** (`.card`, head/body/foot) | Basic container; a head with title + description + actions. |
| **Stat tile** | Label, big number, one-line note. |
| **Button** | primary / secondary / ghost / danger / small; icon button (with `aria-label`). Stubbed actions use `aria-disabled` plus `data-stub`, so they stay focusable and still explain themselves. |
| **Status chip** | Icon + text, six variants; never wraps. Used for state (online, reconnecting, offline), role (worker agent, monitor agent), and tags (Stub, Draft). |
| **Form** | Visible label on top, hint below, error below with `aria-invalid`; text input, select, segmented control (radio group), checkbox; mono variant for IP/command values; search input with icon. |
| **Table** | Dense (14 px), sticky header, optional sticky first column, row hover, `scope` attributes, caption for screen readers, toolbar (search + segmented + select + live result count), empty state. |
| **Meter** | Bar plus numeric text; warn/danger variants carry a word, not just colour. Disk thresholds are the architecture's: **80% warning, 90% critical (capture/archive intake paused)**, arch. 6.5 and 7.3. |
| **Alert** | info / warn / danger / ok, icon + text. |
| **Toast** | Live region; states why a stubbed action did nothing. |
| **Chart placeholder** | Dashed frame, grid, sample series (solid and dashed), legend with line-style swatches, centred "placeholder" label; later hosts an embedded Grafana frame. |
| **Terminal placeholder** | Always-dark box with title bar, prompt, blinking cursor; later hosts xterm.js. |
| **Timeline** | Ruler, load-ceiling lane, item lanes with focusable bars, sticky lane labels, idle playhead. A table fallback is the "Resource use" table; the editor itself stays a stub. |
| **Pattern chooser** | Three radio cards with a small ceiling-shape glyph. |
| **List / kv** | Checklist rows (worker agents, library) and key-value summary. |
| **Tabs** | `role=tablist`, roving tabindex, arrow/Home/End keys (Terminal sessions, Monitor data, Evidence, Watchdog policy). |
| **Row menu** | Button + `role=menu`, fixed-positioned so scrolling tables never clip it; Esc closes and returns focus. |
| **Dialog** | Native `<dialog>` (focus trap, Esc, backdrop for free); one question, then actions; destructive action in danger style. |
| **Switch** | Checkbox with `role=switch` plus a text state (On/Off). |
| **Steps / ladder** | Run lifecycle (queued, running, exited) and the 4-step watchdog escalation ladder; state is text plus shape, not only colour. |
| **Tooltip** | Opens on hover and on keyboard focus; used for the command overlay and zoom segments. |
| **Overlay lane / zoom bar** | Command markers aligned to the chart axis; merged-label segments for semantic zoom. |
| **Code block, frame placeholder, run card** | YAML/command preview; embedded-Grafana placeholder; per-worker-agent live output card. |

### 4.6 Accessibility
Skip link; one `h1` per page and sequential headings; landmarks (`aside`, `nav`, `main`, `header`);
2 px focus ring that is never removed; every icon-only control has an accessible name; status is
icon + text; `aria-live` on result counts and toasts; keyboard-reachable everything (the timeline bars are
buttons); reduced-motion; 44 px targets on touch.

---

## 5. Vocabulary mapping (architecture -> UI)

UI strings use the architecture's words and nothing else:

| Architecture term | Where and how it appears |
|---|---|
| **main server** | Sidebar footer ("Main server: fleet-server, panel :6789, auth via auth proxy"). Never "master" or "controller" for the box. |
| **worker agent** | Role chip "Worker agent" (accent blue); filter "Worker agents"; Maneuver agent picker; terminal and command targets. |
| **monitor agent** | Role chip "Monitor agent" (info blue); never offered a terminal or command (tooltip explains: "Monitor agents have no shell"). |
| **project** | Project select on the Maneuver page; Projects & tests page. |
| **test** | One run of a Maneuver in a project; shown by the active-test chip ("Active test: none"). |
| **Maneuver** | Capitalised in titles, nav and copy; the page is "Maneuver editor & Load Controller". |
| **baseline item** | Library list and the Baseline items page; fields use the architecture's names (`rate`, `duration`, `command_template`). |
| **Load Controller** | Card on the Maneuver page, labelled "Not running"; every control is a stub. |
| **evidence log** | Evidence page (session browser, per-session records, pull and sync status). |
| **monitor data** | Monitor data page and chart placeholders; the unit of the 60 s live window. |
| **`connect_mode`** | Mono text `server_dial` / `agent_dial` in the Machines table and onboarding form. |

---

## 6. Static front-end structure and Go `html/template` port

```
web/
  index.html               redirect to machines.html
  machines.html ... backup.html   the ten panel pages (GENERATED, see below)
  styleguide.html          component reference (hand-written, not a panel page)
  assets/css/tokens.css base.css components.css
  assets/js/app.js         theme, drawer, toasts, tabs, menus, dialogs, tooltips, mock filters
  _src/shell.html          the one layout: head, icon sprite, sidebar, top bar, <main>{{content}}</main>
  _src/pages/<page>.html   one fragment per page (first line: <!-- title: ... -->)
scripts/stamp-web.py       DEV-ONLY generator: shell + fragment -> web/<page>.html
```

- **No build at runtime, no framework, no network.** Relative paths only, so every page works from `file://` or any
  static server. A framework is not justified for ten server-rendered pages with islands of behaviour.
- **One shell, stamped.** The shell cannot be included at runtime on `file://`, so `scripts/stamp-web.py` (Python
  stdlib, dev-only) stamps every page from `_src/shell.html`; the committed `web/*.html` is plain static HTML with a
  "GENERATED" banner. `python3 scripts/stamp-web.py` regenerates; `--check` exits 1 if any page is out of date, so it
  can run in a check later. Fragments may use small macros (`{{i:name}}` icon, `{{state:online}}`, `{{role:worker}}`)
  that expand to the same markup shown in the styleguide. Edit `_src/`, never the generated pages.
- **Go port.** `shell.html` becomes `{{define "layout"}}`; each fragment becomes `{{define "content"}}`; the active nav
  item is `aria-current="page"` set from a `.Page` field; mock rows become `{{range}}` blocks. The stamp script is then
  retired. The `{{...}}` in `_src/` are stamp-script tokens, not Go template actions; none remain in the output.
- Target serving: `fleet-server` on **`:6789`** (D-0004) serves `web/` (without `_src/`) via `embed.FS`. `app.js` has no
  network code; later tasks add `fetch`/WebSocket, and mount xterm.js (Terminal) and Grafana frames (Monitor data).
- `data-stub="..."` marks any action that is still a stub; a later task replaces it with the real handler.
- Verified: every page loads with no JS errors and no external URLs; theme toggle, row menu, dialogs, tabs (mouse and
  arrow keys), semantic zoom, tooltips on focus, switches, and the Machines filter all behave (scripted check through
  Chrome DevTools Protocol).

---

## 7. Maneuver / Load Controller page (stub)

Layout only (D-0005): nothing runs, nothing saves. Top: a warn banner ("Layout preview - execution is
not available"). Main column: Maneuver (name, library, project, total time, target IP, origin/URL) ->
Load pattern (three cards + parameters `pattern_length`, `spike_duration`, window length, max CPU/RAM per
worker agent) -> Timeline -> Resource use (chart placeholder + per-item table showing "-" until a test
runs). Right rail: Load Controller (status, policy profile, ceiling per agent and aggregate, Start /
Stop / Edit profile), worker agents (all selected by default, offline ones flagged), baseline item library
(search, add). Every button is clickable and answers with a toast. Radio/checkbox controls do work
visually but drive no state.

---

## 8. Decisions taken during review (phase 1 questions)

- **Q1** Dead nav links were acceptable; phase 2 built all pages, so every link resolves.
- **Q2** System font stacks stay (offline wins); no web fonts.
- **Q3** Blue accent, green reserved for ok/online: confirmed.
- **Q4** 14 px body, 36 px controls: confirmed (desktop ops console).
- **Q5** Machines row menu: Details, Re-provision, Remove (stub). No rename.
- **Q6** Disk thresholds are the architecture's: 80% warning, 90% critical / intake paused.
- **Q7** Shell is stamped from one source by a dev-only script (section 6).

## 9. Open items for later tasks

- Threshold example values on the Watchdog page (CPU 95%, RAM 92%, recovery 85%) are placeholders: the architecture
  names the mechanism (sustained 20 s breach, hysteresis) but not the percentages. The operator should set them.
- Monitor "per machine" and "per target" chart views are radio buttons only; the data views come with the backend.
- The Maneuver page keeps its target block ip-first (as agreed); only the monitor-job form is origin-first.
- Reports (arch. 13) open from "Report" buttons on the Projects page; the report itself is not part of this front-end.
- When the Go templates exist, retire `scripts/stamp-web.py` and move `_src/` content into `internal/` templates.
