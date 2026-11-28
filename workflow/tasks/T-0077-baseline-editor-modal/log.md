## 2026-11-28 — T-0077: baseline-editor-modal

### Changes
- `web/src/pages/BaselinePage.jsx`:
  - Removed the inline `.stack` editor section above the Library so the Library card is the full-width page content.
  - Moved the New/Edit form into a modal `<dialog className="dialog dialog--wide">` synced via `editorOpen` + `editorDialogRef` (`showModal()`/`close()`, focusing `nameInputRef` on open).
  - Structured the modal with `.dialog__head` (title `New item` / `Edit item`, `{id}` subtitle in edit mode, and a `Close editor` `✕` button), `.dialog__body` (Name, Description, Command template with Detected readout, Warm-up, Tags, Default inputs, live Substitution preview, and `StatusAlert`), and `.dialog__foot` (`Duplicate` and `Delete` in edit mode, `Discard`, and `Create item` / `Save changes`).
  - Wired open/close: `New item` (`startNewItem`) and row click / `Edit` icon (`selectItem`) open the modal; `✕` (`closeEditor`), `Esc` (`onCancel`), `Discard` (`discardChanges`), and successful `handleSave` close it; closing while `saving` is blocked. Deleting the currently open `selectedId` in `confirmDelete` closes the modal and resets the draft.
  - Gated the preview `useEffect` on `editorOpen` so preview requests only fire while the editor modal is open.
  - Trimmed all seven `.field__hint--block` strings to one concise line each and updated the empty-state copy from "Use the form above..." to "Use New item to create your first reusable command template."
- `web/assets/css/components.css`:
  - Tightened `.field__hint--block` (`font-size: 0.6875rem; line-height: 1.25; color: var(--text-muted); max-width: 68ch`).
  - Added `.dialog--wide` (`width: min(60rem, calc(100vw - 2rem))`), flex header spacing on `.dialog--wide .dialog__head`, internal vertical scrolling on `.dialog--wide .dialog__body` (`max-height: calc(100vh - 11rem); overflow: auto`), and `min-width: 0` on its children.
- Rebuilt the SPA bundle via `./web/build.sh` (`web/assets/dist/app.js`, `web/assets/dist/app.css`).

### Checks
- `./web/build.sh`: passed (`assets/dist/app.js` 606.8kb, `assets/dist/app.css` 40.3kb).
- `go build ./...`: passed.
- `./scripts/test.sh server`: passed (all packages including `internal/baseline` and `internal/panel`).
- `check-app`: not installed / not present in this tree, so no automated browser smoke run was executed.

<!-- wf: commits=7d17542 left=- tests=server:pass blocked=no -->
