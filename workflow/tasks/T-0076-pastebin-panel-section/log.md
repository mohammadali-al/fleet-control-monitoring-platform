# T-0076 log

## Doer entry

**Built.** The LAN pastebin as a Pastebin section of the panel, Go backend plus React page.

- `internal/pastebin/store.go` (new, self-contained): `Store{text, version}` under a mutex, atomic
  persistence (temp + fsync + rename, `rename` field as in `internal/baseline/store.go`) to
  `<dataDir>/pastebin/paste.txt`; `Get`, `Set(text, client)` (5 MB cap -> `ErrTooLarge`; a failed save
  leaves the pad unchanged and broadcasts nothing), and an in-process hub: `Subscribe()` returns a
  channel that first delivers the current state, then every change; a subscriber that lags past its
  buffer is closed (the client reconnects and reloads). `config.PastebinDirName` added.
- `internal/panel/pastebin.go` (new): lazy per-server store (same dataDir resolution as
  `baselineStore()`), `GET /api/pastebin` -> `{text, version}`, `POST /api/pastebin` `{text, client}` ->
  `{version}` (400 over 5 MB or missing text; own body limit because the shared `decode` caps at 64 KB),
  `GET /ws/pastebin` (hello snapshot, then every change; close-on-lag; a 25 s ping so proxies do not
  drop an idle socket). Registered in `routesAPI` and `routesWS`. The fleet event bus is untouched.
- Front end: `web/src/hooks/usePastebin.js` (GET/POST via `api.js`, `/ws/pastebin` with backoff
  reconnect like `useMonitor.js`; 150 ms debounced save, random client id to drop the own echo, remote
  frames ignored while a local edit is pending or in flight, caret/selection restored after applying a
  remote update, unsaved edit flushed on unmount) and `web/src/pages/PastebinPage.jsx` (`.card` with a
  `.textarea.input--mono` pad, status chip in the card head: connecting / live / saving / save failed /
  reconnecting; no-backend alert). Route `pastebin` added right after `backup` in the Administer
  group with the existing `i-copy` icon, no `legacyPath`. Bundle rebuilt (`web/assets/dist/app.js`;
  `app.css` and `dist/index.html` came out byte-identical, so they have no diff).

**Checks.** `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok, `scripts/test.sh server`
passes (new: store round trip across reopen, size cap, failed-save keeps pad, snapshot + broadcast,
lagging subscriber dropped; panel: 503 without backend, POST/GET + disk file + 400 cap + 415,
restart persistence, two websocket clients both receive the snapshot and the broadcast with the
`client` tag). `-race` on the new tests passes.

`scripts/check-app` / `check-app` does not exist in this tree (same as T-0028's log), so I ran an
equivalent ad-hoc check: the built `fleet-server` on a temp data dir, headless Chrome driven over CDP
with two tabs on `/pastebin`. Result: sidebar shows `Backup` then `Pastebin` (`/pastebin`, SPA route);
status chip "Live"; typing in tab A appeared in tab B within ~1 s; with tab B focused and its caret at
3, a remote edit left the caret at 3; no console errors or warnings in either tab; screenshots in
light and dark theme render with the panel's cards, chips and tokens. Server, Chrome and the temp
profile/data were stopped/removed (the scratchpad copy of the server binary and data dir remain in the
session scratchpad, outside the repo).

**Notes for the supervisor.**

- The pad version restarts at 0 on every server start (as in the Flask original); it is only an
  ordering hint, clients reload the text on reconnect.
- One live pad per server process; survives restart via `paste.txt` (covered by a test).
- Not hand-checked: dragging a very large (multi-MB) paste through a real browser.

<!-- wf: commits=88a8cef left=- tests=server:pass blocked=no -->
