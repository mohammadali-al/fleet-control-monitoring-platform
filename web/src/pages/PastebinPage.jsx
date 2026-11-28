import { usePastebin } from '../hooks/usePastebin';
import { usePageTitle } from '../lib/title';

const STATUS_VIEW = {
  connecting: { cls: 'chip chip--info', label: 'Connecting…' },
  live: { cls: 'chip chip--ok chip--live', label: 'Live' },
  saving: { cls: 'chip chip--warn chip--live', label: 'Saving…' },
  error: { cls: 'chip chip--danger', label: 'Save failed, retrying' },
  reconnecting: { cls: 'chip chip--danger', label: 'Reconnecting…' },
};

export function PastebinPage() {
  usePageTitle('Pastebin');
  const { text, setText, status, noBackend, padRef } = usePastebin();
  const view = STATUS_VIEW[status] || STATUS_VIEW.connecting;

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Pastebin</h1>
          <p>
            One shared scratch pad for every operator. It saves as you type and updates live in every
            open panel.
          </p>
        </div>
      </div>

      {noBackend && (
        <div className="alert alert--warn" role="status" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true">
            <use href="#i-alert" />
          </svg>
          <div>The panel is running without a backend, so the pastebin is unavailable.</div>
        </div>
      )}

      <section className="card" aria-labelledby="pb-title">
        <div className="card__head">
          <div>
            <h2 id="pb-title">Shared pad</h2>
            <p>Plain text, up to 5 MB.</p>
          </div>
          <span className={view.cls} id="pb-status" role="status" aria-live="polite">
            {view.label}
          </span>
        </div>
        <div className="card__body">
          <textarea
            ref={padRef}
            id="pb-pad"
            className="textarea input--mono"
            style={{ height: 'max(22rem, calc(100vh - 21rem))', resize: 'none' }}
            spellCheck={false}
            disabled={noBackend}
            aria-label="Shared pad"
            placeholder="Paste or type here. It syncs to every open panel automatically."
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
        </div>
      </section>
    </>
  );
}
