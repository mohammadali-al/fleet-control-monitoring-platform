# Operator panel — front-end design

**Status:** exploring · **Task:** T-0004 (phase 1) · **Decisions:** D-0001 (stack), D-0003 (English UI),
D-0004 (`:6789`), D-0005 (Maneuver execution deferred) · **Source of truth:** architecture §12

Phase 1 delivers this note plus two proof pages under `web/`: **Machines** and **Maneuver / Load
Controller** (a non-functional stub). `web/styleguide.html` is a reviewer aid showing every shared
component, including the chart and terminal placeholders. The other eight pages are built in phase 2
after review.

How to look at it: open `web/machines.html` or `web/maneuver.html` straight from the filesystem (no
server, no network). Toggle light/dark with the button at the top right.

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
| **Meter** | Bar plus numeric text; warn/danger variants carry a word ("high", "critical") not just colour. |
| **Alert** | info / warn / danger / ok, icon + text. |
| **Toast** | Live region; states why a stubbed action did nothing. |
| **Chart placeholder** | Dashed frame, grid, sample series (solid and dashed), legend with line-style swatches, centred "placeholder" label; later hosts an embedded Grafana frame. |
| **Terminal placeholder** | Always-dark box with title bar, prompt, blinking cursor; later hosts xterm.js. |
| **Timeline** | Ruler, load-ceiling lane, item lanes with focusable bars, sticky lane labels, idle playhead. A table fallback is the "Resource use" table; the editor itself stays a stub. |
| **Pattern chooser** | Three radio cards with a small ceiling-shape glyph. |
| **List / kv** | Checklist rows (worker agents, library) and key-value summary. |

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
  index.html            redirect to machines.html
  machines.html         proof page 1
  maneuver.html         proof page 2 (stub)
  styleguide.html       component reference (not a panel page)
  assets/css/tokens.css base.css components.css
  assets/js/app.js      theme, drawer, toasts, panel toggle, mock table filter
```

- **No build, no framework, no network.** Relative paths only, so it works from `file://` or any static
  server. A framework is not justified for ten server-rendered pages with islands of behaviour.
- The shell (SVG icon sprite, sidebar, top bar) is **duplicated in each page** between
  `<!-- tmpl:shell-open -->` and `<!-- /tmpl:shell-open -->`: external `fetch` includes do not work on
  `file://`. In Go it becomes `{{define "layout"}}` with `{{template "sidebar" .}}`; the page-specific
  `<main class="content">...</main>` becomes `{{define "content"}}`. The active nav item is
  `aria-current="page"` set from a `.Page` field.
- Mock rows and mock chips (`data-*`, "Mock data") are placeholders for `{{range}}` blocks.
- Target serving: `fleet-server` on **`:6789`** (D-0004) serves `web/` via `embed.FS`; `assets/` as static
  files. `app.js` has no network code; the later wiring task adds `fetch`/WebSocket where needed (xterm.js
  and Grafana frames are added to the Terminal and Monitor data pages then).
- `data-stub="..."` marks any action that is still a stub; a later task replaces it with the real handler.

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

## 8. Open design questions

- **Q1. Nav destinations in phase 1.** Links to the eight unbuilt pages are real hrefs and 404 until phase 2.
  Acceptable for review? (Alternative: ship phase-1 stubs for each.)
- **Q2. Fonts.** System stacks keep the panel fully offline. If a Fira look is wanted, self-host woff2 files
  under `web/assets/fonts/` (adds binary files to the repo). Decide before phase 2.
- **Q3. Accent colour.** Blue accent, green reserved for "ok" (deviates from the skill's green). Confirm.
- **Q4. Density.** 14 px body, 36 px controls. Is the operator on a large monitor (comfortable) or a laptop
  (maybe 13 px)?
- **Q5. Machines "More" menu.** Per-row details/actions (rename, change role, decommission, re-provision) are
  not in §12.2; shown as a stub kebab. Define the list or drop the kebab.
- **Q6. Disk thresholds.** 80 % high / 95 % critical are placeholders; no architecture value found.
- **Q7. Shared shell duplication.** Phase 2 will repeat the shell in eight more files. A tiny script to stamp
  it from one source (still no runtime dependency) could be added; or accept until the Go templates exist.

---

## 9. Phase 2 plan (after approval)

Build the eight remaining pages on the same shell and components; add only components the pages need
(tabs for Terminal sessions, a diff view for Backup, a form-section pattern for the policy editors);
re-run the contrast script when tokens change; keep `styleguide.html` in sync.
