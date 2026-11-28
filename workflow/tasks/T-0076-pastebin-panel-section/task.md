---
id: T-0076
title: pastebin-panel-section
state: ready            # draft | ready | running | blocked | review | changes | merged | closed
touches: [internal/pastebin/, internal/panel/pastebin.go, internal/panel/pastebin_test.go,
          internal/panel/ws.go, internal/panel/api.go, internal/config/config.go,
          web/src/pages/PastebinPage.jsx, web/src/hooks/usePastebin.js, web/src/routes.js,
          web/assets/dist/, web/dist/index.html]
tests: [server]
after: [T-0047]
shares: {}
model: {claude: "sonnet high"}
operator_check: "The panel has a new 'Pastebin' section in the sidebar under Administer, directly below Backup (a client-side SPA route, no top-level/standalone navigation). It's a single shared scratch pad: one big text area that persists on the server and syncs live across every open panel tab/device — type in one, it appears in the others within a moment. Survives server restart. Styled to match the rest of the panel in both light and dark themes."
---

## Why

The operator keeps a tiny standalone Flask app (`~/Desktop/pastebin/app.py`) they want folded into the
panel: a **LAN pastebin** — one shared textarea that auto-syncs across devices (Server-Sent-Events
broadcast + debounced POST, persisted to a `paste.txt`). Instead of running a second service, make it a
**Pastebin section in the panel**, listed in the sidebar under **Administer, right below Backup**, as a
normal client-side SPA route (no new top-level nav, no separate server).

**Keep the behavior/logic** of the original; **reimplement the backend in Go** to fit the panel (the panel
already provides the two things the Flask app hand-rolls: a websocket transport and an atomic file store),
and **restyle the front-end to match the current panel UI** (the original's bespoke CSS is replaced by the
panel's design system). The LAN-only IP guard is dropped — the panel's existing operator auth gates it.

## Scope decisions (settled — build exactly this)

- **One single global pad**, shared by all operators (mirrors the original; it's an Administer-level tool,
  like Backup). Not per-project, not per-user.
- **Live sync is kept** (that's the original's whole point): an edit in one open panel propagates to all
  other connected panel clients in near-real-time, with the editor's own echo suppressed and in-flight
  local edits not clobbered. Persist on change; reload on reconnect.

## What to build

### Backend — new self-contained package `internal/pastebin/`

1. **Store (`internal/pastebin/store.go`).** A `Store` holding `{text string, version uint64}` guarded by a
   mutex, persisted atomically (temp file + fsync + rename) to `<dataDir>/pastebin/paste.txt` — mirror the
   atomic-save pattern in `internal/baseline/store.go` (`Open(dataDir)`, `saveLocked`, the `rename` field).
   Add `PastebinDirName = "pastebin"` to `internal/config/config.go` (next to `BaselineDirName`,
   `config.go:41`). On `Open`, load any existing `paste.txt`. Methods: `Get() (text, version)`,
   `Set(text) version` (increments version + persists), and a small **in-process pub/sub hub** (a set of
   subscriber channels with `Subscribe()`/`Unsubscribe()` and a `broadcast`) — this is the Go equivalent of
   the Flask app's `subscribers`/`queue` fan-out. Keep it self-contained; do **not** route pastebin through
   the fleet event bus (`internal/fleet/bus.go`) — unrelated concern, keep the diff isolated.

2. **Panel wiring (`internal/panel/pastebin.go`, new).** Lazy per-server store construction mirroring
   `internal/panel/baseline.go:47-79` (`baselineStore()` + the per-server map + `dataDir` resolution).
   Register routes in `routesAPI` (`internal/panel/api.go:55`, add a `s.routesPastebin(mux)` call beside
   `s.routesBaseline(mux)` at `:71`):
   - `GET /api/pastebin` → `{text, version}` (current pad).
   - `POST /api/pastebin` → body `{text, client}`; sets text, bumps version, broadcasts, returns `{version}`.
     Enforce a size cap (the original uses 5 MB — reuse that).
   Register a websocket stream in `internal/panel/ws.go` (beside `GET /ws/events`, `ws.go:29`):
   - `GET /ws/pastebin` → on connect send the current `{text, version}`, then push every broadcast frame
     `{text, version, client}`; use the `coder/websocket` + `writeJSONFrame` pattern already in `wsEvents`
     (`ws.go:105`), including the keepalive/close-on-lag handling. Gate with the same backend-present /
     auth check the other sockets use (`s.be == nil` → 503).

3. **Tests (`internal/panel/pastebin_test.go`, + store coverage).** POST then GET returns the new text +
   bumped version; a second connection receives the update over `/ws/pastebin`; the store round-trips across
   a reopen (persistence); size cap rejected with 400.

### Frontend — match the panel UI

4. **Page (`web/src/pages/PastebinPage.jsx`).** A panel-styled page: a `.card` containing a full-height
   **textarea** (reuse `.textarea`/`.input--mono` styles) and a small **live-status indicator** in the card
   head (connecting / live / saving / reconnecting) — express it with the existing status/chip/`muted`
   patterns and theme tokens, not the original's custom colors. Keep the original's client logic:
   debounced auto-save on input (~150 ms), a random `client` id to ignore the server echo of your own write,
   preserve the caret/selection when applying a remote update, and don't overwrite while a local edit is in
   flight. `usePageTitle('Pastebin')`.

5. **Hook (`web/src/hooks/usePastebin.js`).** Load via `GET /api/pastebin`, save via `POST /api/pastebin`
   (use `web/src/lib/api.js`), and subscribe to `/ws/pastebin` for live updates — model the websocket
   connect/reconnect on an existing hook that opens a panel socket (e.g. `useMonitor.js` / the
   `web/src/lib/events.jsx` client). Expose `{ text, setText, status, noBackend }` to the page.

6. **Nav entry (`web/src/routes.js`).** Add a route object immediately **after** the `backup` entry
   (`routes.js:113-121`), in the `Administer` group: `{ id: 'pastebin', path: '/pastebin', group:
   'Administer', label: 'Pastebin', icon: <existing sprite icon>, component: PastebinPage }` and import
   `PastebinPage`. Reuse an existing sprite icon (e.g. `i-copy`, `i-edit`, or similar clipboard-ish glyph) —
   don't add a new icon unless none reads right. No `legacyPath` (brand-new, SPA-only).

7. **Rebuild + commit the SPA bundle** (`./web/build.sh`): regenerate `web/assets/dist/app.{js,css}` and
   `web/dist/index.html` and commit them with the source.

## Out of scope

- No port of the Python app itself, no second process, no `requirements.txt`/`venv`/`run.sh` — the source
  under `~/Desktop/pastebin/` is reference only.
- No LAN-only IP filtering (panel auth replaces it); no per-project or per-user pads; no history/versions
  beyond the single current text; no file-upload/attachment; no rich text.
- Don't touch the fleet event bus, other pages, or shared components beyond the small additions above.

## Done means

- Sidebar shows **Pastebin** under Administer directly below Backup; clicking it opens the SPA page (no full
  reload, no top-level nav change).
- Typing in the pad persists server-side and appears in a second open panel tab within a moment; caret isn't
  disturbed by remote updates; it survives a `fleet-server` restart.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok, `scripts/test.sh server` passes (new
  pastebin tests included); `check-app` clean on the new page in light + dark; web bundle rebuilt + committed.

## Facts this brief relies on

- Original app (reference only): `~/Desktop/pastebin/app.py` — Flask, `/paste` POST + `/events` SSE,
  `subscribers` fan-out, atomic `save()` to `paste.txt`, 5 MB cap, debounced client auto-save with a
  `client` id to drop own echo and caret preservation.
- Store pattern to mirror: `internal/baseline/store.go` (`Open(dataDir)` at :76, atomic `saveLocked` at
  :320, `rename` field). Dir-name consts: `internal/config/config.go:34-51` (add `PastebinDirName`).
- Panel route registration: `internal/panel/api.go:55` `routesAPI`, `:71` `s.routesBaseline(mux)`. Lazy
  per-server store: `internal/panel/baseline.go:47-79`.
- Websocket hub pattern: `internal/panel/ws.go:29` (`GET /ws/events`), `:105` `wsEvents` (coder/websocket
  Accept, `writeJSONFrame`, keepalive, close-on-lag). Sockets 503 when `s.be == nil`.
- Frontend: SPA routes + `NAV_GROUPS` (incl. `Administer`) in `web/src/routes.js:25`; `backup` entry
  `:113-121`. Page template + hooks: `web/src/pages/BackupPage.jsx`, `web/src/hooks/useMonitor.js`,
  `web/src/lib/api.js`, `web/src/lib/events.jsx`, `usePageTitle` (`web/src/lib/title.js`). Styling tokens:
  `web/assets/css/`. Committed bundle: `web/assets/dist/app.{js,css}`, `web/dist/index.html`
  (`web/build.sh`).

## Notes for the doer

- Run `./scripts/wf start T-0076` before committing; commit only inside `touches` (new files are free) +
  append `log.md` ending in the `wf:` footer. Do NOT push/merge/branch-delete, edit `state:`, or touch
  decisions/generated/other tasks.
- Reference UI/UX patterns: `https://github.com/nextlevelbuilder/ui-ux-pro-max-skill`. But the visual target
  is simply **"looks like the rest of this panel"** — reuse existing cards/inputs/status patterns and theme
  tokens; don't invent a new look. No separate design-review phase needed — match the existing pages.
- Keep the backend self-contained in `internal/pastebin/` so the surface area is small; the panel files only
  gain registration + a thin handler.
- UI language is English (D-0003).
