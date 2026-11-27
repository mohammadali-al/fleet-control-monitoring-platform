import { useEffect, useMemo, useRef, useState } from 'react';
import { StatusAlert } from '../components/StatusAlert';
import { useBaseline } from '../hooks/useBaseline';
import { usePageTitle } from '../lib/title';
import { useToast } from '../lib/toast';

const PLACEHOLDER_RE = /\{\{|\}\}|\{([a-zA-Z_][a-zA-Z0-9_-]*)\}/g;

function extractPlaceholdersClient(tmpl) {
  if (!tmpl) return [];
  const seen = new Set();
  const out = [];
  let m;
  PLACEHOLDER_RE.lastIndex = 0;
  while ((m = PLACEHOLDER_RE.exec(tmpl)) !== null) {
    if (m[1] && !seen.has(m[1])) {
      seen.add(m[1]);
      out.push(m[1]);
    }
  }
  return out;
}

function emptyDraft() {
  return {
    name: '',
    command_template: '',
    description: '',
    warmup_sec: '',
    tags: '',
    defaults: {},
  };
}

function draftFromItem(item) {
  return {
    name: item.name || '',
    command_template: item.command_template || '',
    description: item.description || '',
    warmup_sec: item.warmup_sec ? String(item.warmup_sec) : '',
    tags: Array.isArray(item.tags) ? item.tags.join(', ') : '',
    defaults: { ...(item.defaults || {}) },
  };
}

function formatDefaultsSummary(defaults) {
  if (!defaults) return null;
  const entries = Object.entries(defaults).filter(([, v]) => v !== '');
  if (entries.length === 0) return null;
  return entries.map(([k, v]) => `${k}=${v}`).join(' · ');
}

export function BaselinePage() {
  usePageTitle('Baseline items');
  const pushToast = useToast();
  const {
    items,
    noBackend,
    error: loadError,
    loading,
    createItem,
    updateItem,
    duplicateItem,
    deleteItem,
    previewTemplate,
  } = useBaseline();

  const [searchQ, setSearchQ] = useState('');
  const [selectedId, setSelectedId] = useState(null); // null => create mode
  const [draft, setDraft] = useState(emptyDraft);
  const [previewInputs, setPreviewInputs] = useState({});
  const [previewState, setPreviewState] = useState({
    loading: false,
    command: '',
    error: '',
    unresolved: [],
  });

  const [saving, setSaving] = useState(false);
  const [formStatus, setFormStatus] = useState(null);
  const [deleteTarget, setDeleteTarget] = useState(null);
  const [deleting, setDeleting] = useState(false);

  const nameInputRef = useRef(null);
  const deleteDialogRef = useRef(null);
  const previewSeqRef = useRef(0);

  const selectedItem = useMemo(
    () => (selectedId ? items.find((it) => it.id === selectedId) || null : null),
    [items, selectedId]
  );

  // Sync delete confirmation <dialog>.
  useEffect(() => {
    const dlg = deleteDialogRef.current;
    if (!dlg) return;
    if (deleteTarget && !dlg.open) dlg.showModal();
    if (!deleteTarget && dlg.open) dlg.close();
  }, [deleteTarget]);

  const filteredItems = useMemo(() => {
    const q = searchQ.trim().toLowerCase();
    if (!q) return items;
    return items.filter((it) => {
      const hay = [
        it.id,
        it.name,
        it.description || '',
        it.command_template,
        ...(it.placeholders || []),
        ...(it.tags || []),
        ...Object.entries(it.defaults || {}).flat(),
      ]
        .join(' ')
        .toLowerCase();
      return hay.includes(q);
    });
  }, [items, searchQ]);

  const detectedPlaceholders = useMemo(
    () => extractPlaceholdersClient(draft.command_template),
    [draft.command_template]
  );

  const extraDefaultKeys = useMemo(
    () =>
      Object.keys(draft.defaults || {}).filter(
        (k) => !detectedPlaceholders.includes(k) && draft.defaults[k] !== ''
      ),
    [draft.defaults, detectedPlaceholders]
  );

  const allDefaultKeys = useMemo(
    () => [...detectedPlaceholders, ...extraDefaultKeys],
    [detectedPlaceholders, extraDefaultKeys]
  );

  // Live substitution preview via POST /api/baseline/preview.
  useEffect(() => {
    const tmpl = draft.command_template;
    if (noBackend || !tmpl.trim()) {
      setPreviewState({ loading: false, command: '', error: '', unresolved: [] });
      return undefined;
    }
    const seq = ++previewSeqRef.current;
    const timer = window.setTimeout(async () => {
      setPreviewState((prev) => ({ ...prev, loading: true }));
      const cleanDefaults = {};
      for (const [k, v] of Object.entries(draft.defaults || {})) {
        if (v !== '') cleanDefaults[k] = v;
      }
      const cleanInputs = {};
      for (const [k, v] of Object.entries(previewInputs || {})) {
        if (v !== '') cleanInputs[k] = v;
      }
      const r = await previewTemplate({
        command_template: tmpl,
        defaults: cleanDefaults,
        inputs: cleanInputs,
      });
      if (previewSeqRef.current !== seq) return;
      if (r.ok) {
        setPreviewState({
          loading: false,
          command: r.data.command || '',
          error: '',
          unresolved: [],
        });
      } else {
        setPreviewState({
          loading: false,
          command: '',
          error: r.data.error || `Preview failed (${r.status})`,
          unresolved: Array.isArray(r.data.unresolved) ? r.data.unresolved : [],
        });
      }
    }, 120);

    return () => window.clearTimeout(timer);
  }, [draft.command_template, draft.defaults, previewInputs, noBackend, previewTemplate]);

  function startNewItem() {
    setSelectedId(null);
    setDraft(emptyDraft());
    setPreviewInputs({});
    setFormStatus(null);
    window.setTimeout(() => nameInputRef.current?.focus(), 0);
  }

  function selectItem(item) {
    setSelectedId(item.id);
    setDraft(draftFromItem(item));
    setPreviewInputs({});
    setFormStatus(null);
  }

  function discardChanges() {
    setFormStatus(null);
    if (selectedItem) {
      setDraft(draftFromItem(selectedItem));
    } else {
      setDraft(emptyDraft());
    }
    setPreviewInputs({});
  }

  async function handleSave(e) {
    e.preventDefault();
    if (noBackend) return;

    const cleanDefaults = {};
    for (const [k, v] of Object.entries(draft.defaults || {})) {
      if (v !== '') cleanDefaults[k] = v;
    }
    const warmup = draft.warmup_sec.trim() === '' ? 0 : Number(draft.warmup_sec);
    if (Number.isNaN(warmup) || warmup < 0) {
      setFormStatus({ kind: 'danger', msg: 'Warm-up must be a non-negative number of seconds.' });
      return;
    }
    const tags = draft.tags
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean);

    const payload = {
      name: draft.name.trim(),
      description: draft.description.trim(),
      command_template: draft.command_template.trim(),
      defaults: cleanDefaults,
      warmup_sec: warmup,
      tags,
    };

    setSaving(true);
    setFormStatus({ kind: 'info', msg: selectedId ? 'Saving changes...' : 'Creating item...' });
    const r = selectedId ? await updateItem(selectedId, payload) : await createItem(payload);
    setSaving(false);

    if (!r.ok) {
      setFormStatus({ kind: 'danger', msg: r.data.error || `Save failed (${r.status})` });
      return;
    }
    const saved = r.data.item;
    setSelectedId(saved.id);
    setDraft(draftFromItem(saved));
    setFormStatus({
      kind: 'ok',
      msg: selectedId ? `Saved "${saved.name}".` : `Created "${saved.name}".`,
    });
    pushToast(selectedId ? `Updated ${saved.name}.` : `Created ${saved.name}.`);
  }

  async function handleDuplicate(item) {
    if (!item || noBackend) return;
    const r = await duplicateItem(item.id);
    if (!r.ok) {
      pushToast(r.data.error || `Duplicate failed (${r.status})`);
      return;
    }
    const copy = r.data.item;
    setSelectedId(copy.id);
    setDraft(draftFromItem(copy));
    setPreviewInputs({});
    setFormStatus({ kind: 'ok', msg: `Duplicated as "${copy.name}".` });
    pushToast(`Duplicated "${item.name}".`);
  }

  async function confirmDelete(e) {
    e.preventDefault();
    if (!deleteTarget || noBackend) return;
    setDeleting(true);
    const r = await deleteItem(deleteTarget.id);
    setDeleting(false);
    if (!r.ok) {
      pushToast(r.data.error || `Delete failed (${r.status})`);
      setDeleteTarget(null);
      return;
    }
    const removed = deleteTarget;
    setDeleteTarget(null);
    if (selectedId === removed.id) {
      startNewItem();
    }
    pushToast(`Deleted "${removed.name}".`);
  }

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Baseline items</h1>
          <p>
            The global library of reusable command presets. Every project can place these items into a Maneuver;
            target fields are filled when an item is placed.
          </p>
        </div>
        <div className="page-head__actions">
          <button
            className="btn btn--primary"
            type="button"
            disabled={noBackend}
            onClick={startNewItem}
          >
            <svg className="icon" aria-hidden="true"><use href="#i-plus" /></svg>
            New item
          </button>
        </div>
      </div>

      {noBackend && (
        <div className="alert alert--warn" role="status" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
          <div>The panel is running without a backend, so baseline items cannot be loaded or saved.</div>
        </div>
      )}

      {loadError && (
        <div className="alert alert--danger" role="status" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
          <div>{loadError}</div>
        </div>
      )}

      <div className="split">
        {/* ============ Library ============ */}
        <section className="card" aria-labelledby="lib-title">
          <div className="card__head">
            <h2 id="lib-title">Library</h2>
            <span className="muted">
              {loading ? 'Loading...' : `${items.length} ${items.length === 1 ? 'item' : 'items'}`}
            </span>
          </div>

          <div className="table-toolbar" role="search">
            <div className="input-affix">
              <svg className="icon" aria-hidden="true"><use href="#i-search" /></svg>
              <input
                className="input"
                type="search"
                placeholder="Search by name, command, or placeholder"
                aria-label="Search baseline items"
                value={searchQ}
                onChange={(e) => setSearchQ(e.target.value)}
              />
            </div>
          </div>

          <div className="table-wrap">
            <table className="table table--sticky-first">
              <caption className="sr-only">Baseline item library</caption>
              <thead>
                <tr>
                  <th scope="col">Name</th>
                  <th scope="col">Command template</th>
                  <th scope="col">Defaults</th>
                  <th scope="col">Placeholders</th>
                  <th scope="col"><span className="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                {filteredItems.length === 0 ? (
                  <tr>
                    <td colSpan={5} className="muted">
                      {items.length === 0
                        ? 'No baseline items yet. Use the form on the right to create your first reusable command template.'
                        : 'No baseline items match that search.'}
                    </td>
                  </tr>
                ) : (
                  filteredItems.map((it) => {
                    const isSelected = selectedId === it.id;
                    const defSummary = formatDefaultsSummary(it.defaults);
                    const placeholders = Array.isArray(it.placeholders) ? it.placeholders : [];
                    return (
                      <tr
                        key={it.id}
                        aria-selected={isSelected ? 'true' : undefined}
                        onClick={() => selectItem(it)}
                        style={{ cursor: 'pointer' }}
                      >
                        <th scope="row" className="cell-primary">
                          <div>{it.name}</div>
                          {it.description && (
                            <div className="muted" style={{ fontWeight: 400, fontSize: 'var(--fs-xs)' }}>
                              {it.description}
                            </div>
                          )}
                        </th>
                        <td className="mono truncate" title={it.command_template}>{it.command_template}</td>
                        <td className="nowrap">
                          {defSummary ? (
                            <span className="mono">{defSummary}</span>
                          ) : (
                            <span className="faint">none set</span>
                          )}
                        </td>
                        <td>
                          {placeholders.length > 0 ? (
                            <span className="row" style={{ gap: '0.25rem', flexWrap: 'wrap' }}>
                              {placeholders.map((p) => (
                                <span key={p} className="token">{`{${p}}`}</span>
                              ))}
                            </span>
                          ) : (
                            <span className="faint">none</span>
                          )}
                        </td>
                        <td className="actions" onClick={(e) => e.stopPropagation()}>
                          <button
                            className="icon-btn icon-btn--sm"
                            type="button"
                            aria-label={`Edit ${it.name}`}
                            title="Edit"
                            onClick={() => selectItem(it)}
                          >
                            <svg className="icon" aria-hidden="true"><use href="#i-edit" /></svg>
                          </button>
                          <button
                            className="icon-btn icon-btn--sm"
                            type="button"
                            aria-label={`Duplicate ${it.name}`}
                            title="Duplicate"
                            disabled={noBackend}
                            onClick={() => handleDuplicate(it)}
                          >
                            <svg className="icon" aria-hidden="true"><use href="#i-copy" /></svg>
                          </button>
                          <button
                            className="icon-btn icon-btn--sm"
                            type="button"
                            aria-label={`Delete ${it.name}`}
                            title="Delete"
                            disabled={noBackend}
                            onClick={() => setDeleteTarget(it)}
                          >
                            <svg className="icon" aria-hidden="true"><use href="#i-trash" /></svg>
                          </button>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>

          <div className="card__foot">
            Global library shared across all projects. Items survive server restart.
          </div>
        </section>

        {/* ============ Create / Edit form ============ */}
        <section className="card sticky-rail" aria-labelledby="ed-title">
          <div className="card__head">
            <div>
              <h2 id="ed-title">{selectedId ? 'Edit item' : 'New item'}</h2>
              <p>
                {selectedItem ? (
                  <>
                    {selectedItem.name}{' '}
                    <span className="mono faint">({selectedItem.id})</span>
                  </>
                ) : (
                  'Create a reusable command template'
                )}
              </p>
            </div>
          </div>

          <form className="card__body stack" onSubmit={handleSave}>
            <div className="field">
              <label htmlFor="b-name">
                Name <span className="faint">(required)</span>
              </label>
              <input
                ref={nameInputRef}
                className="input"
                id="b-name"
                name="name"
                required
                placeholder="HTTP GET flood"
                value={draft.name}
                onChange={(e) => setDraft((p) => ({ ...p, name: e.target.value }))}
              />
            </div>

            <div className="field">
              <label htmlFor="b-tpl">
                Command template <span className="faint">(required)</span>
              </label>
              <textarea
                className="textarea input--mono"
                id="b-tpl"
                name="command_template"
                rows={3}
                required
                spellCheck={false}
                placeholder="hey -z {duration}s -q {rate} {url}"
                aria-describedby="b-tpl-h"
                value={draft.command_template}
                onChange={(e) => setDraft((p) => ({ ...p, command_template: e.target.value }))}
              />
              <span className="field__hint" id="b-tpl-h">
                Placeholders use <span className="token">{'{name}'}</span> (escape literal braces with{' '}
                <span className="token">{'{{'}</span> / <span className="token">{'}}'}</span>).{' '}
                {detectedPlaceholders.length > 0 && (
                  <>
                    Detected:{' '}
                    {detectedPlaceholders.map((p) => (
                      <span key={p} className="token" style={{ marginRight: '0.25rem' }}>
                        {`{${p}}`}
                      </span>
                    ))}
                  </>
                )}
              </span>
            </div>

            <div className="field">
              <label htmlFor="b-desc">Description</label>
              <input
                className="input"
                id="b-desc"
                name="description"
                placeholder="Optional short note for operators"
                value={draft.description}
                onChange={(e) => setDraft((p) => ({ ...p, description: e.target.value }))}
              />
            </div>

            <div className="form-grid" style={{ gridTemplateColumns: 'repeat(2,minmax(0,1fr))' }}>
              <div className="field">
                <label htmlFor="b-warm">Warm-up (startup lag, s)</label>
                <input
                  className="input input--mono"
                  id="b-warm"
                  name="warmup_sec"
                  inputMode="decimal"
                  placeholder="0"
                  value={draft.warmup_sec}
                  onChange={(e) => setDraft((p) => ({ ...p, warmup_sec: e.target.value }))}
                />
              </div>
              <div className="field">
                <label htmlFor="b-tags">Tags</label>
                <input
                  className="input"
                  id="b-tags"
                  name="tags"
                  placeholder="http, l7"
                  value={draft.tags}
                  onChange={(e) => setDraft((p) => ({ ...p, tags: e.target.value }))}
                />
              </div>
            </div>

            {allDefaultKeys.length > 0 && (
              <div className="stack" style={{ gap: 'var(--sp-2)' }}>
                <div className="label">Default inputs</div>
                <div className="form-grid" style={{ gridTemplateColumns: 'repeat(2,minmax(0,1fr))' }}>
                  {allDefaultKeys.map((key) => {
                    const isExtra = !detectedPlaceholders.includes(key);
                    return (
                      <div className="field" key={`def-${key}`}>
                        <label htmlFor={`b-def-${key}`}>
                          <span className="mono">{`{${key}}`}</span> default
                          {isExtra && <span className="faint"> (unused)</span>}
                        </label>
                        <div className="row" style={{ gap: 'var(--sp-1)' }}>
                          <input
                            className="input input--mono"
                            id={`b-def-${key}`}
                            placeholder="Leave blank if filled per Maneuver"
                            value={draft.defaults[key] || ''}
                            onChange={(e) => {
                              const val = e.target.value;
                              setDraft((p) => ({
                                ...p,
                                defaults: { ...p.defaults, [key]: val },
                              }));
                            }}
                          />
                          {isExtra && (
                            <button
                              className="icon-btn icon-btn--sm"
                              type="button"
                              title={`Remove default ${key}`}
                              aria-label={`Remove default ${key}`}
                              onClick={() => {
                                setDraft((p) => {
                                  const next = { ...p.defaults };
                                  delete next[key];
                                  return { ...p, defaults: next };
                                });
                              }}
                            >
                              <svg className="icon" aria-hidden="true"><use href="#i-x" /></svg>
                            </button>
                          )}
                        </div>
                      </div>
                    );
                  })}
                </div>
              </div>
            )}

            {/* Live substitution preview */}
            <div className="stack" style={{ gap: 'var(--sp-2)' }}>
              <div className="row" style={{ justifyContent: 'space-between' }}>
                <span className="label">Substitution preview</span>
                {previewState.loading && <span className="muted">Rendering...</span>}
              </div>

              {detectedPlaceholders.length > 0 && (
                <div className="form-grid" style={{ gridTemplateColumns: 'repeat(2,minmax(0,1fr))' }}>
                  {detectedPlaceholders.map((key) => (
                    <div className="field" key={`prev-${key}`}>
                      <label htmlFor={`b-prev-${key}`}>
                        Test <span className="mono">{`{${key}}`}</span>
                      </label>
                      <input
                        className="input input--mono"
                        id={`b-prev-${key}`}
                        placeholder={draft.defaults[key] ? `default: ${draft.defaults[key]}` : 'supply value to test'}
                        value={previewInputs[key] || ''}
                        onChange={(e) => {
                          const val = e.target.value;
                          setPreviewInputs((p) => ({ ...p, [key]: val }));
                        }}
                      />
                    </div>
                  ))}
                </div>
              )}

              {previewState.command && (
                <pre className="codeblock" aria-label="Rendered command preview">
                  {previewState.command}
                </pre>
              )}

              {previewState.error && (
                <div
                  className={previewState.unresolved.length > 0 ? 'alert alert--warn' : 'alert alert--danger'}
                  role="status"
                  aria-label="Preview validation message"
                >
                  <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
                  <div>{previewState.error}</div>
                </div>
              )}
            </div>

            <StatusAlert status={formStatus} />

            <div className="row" style={{ flexWrap: 'wrap' }}>
              <button
                className="btn btn--primary"
                type="submit"
                disabled={saving || noBackend}
              >
                <svg className="icon" aria-hidden="true"><use href="#i-save" /></svg>
                {selectedId ? 'Save changes' : 'Create item'}
              </button>

              {selectedItem && (
                <button
                  className="btn"
                  type="button"
                  disabled={saving || noBackend}
                  onClick={() => handleDuplicate(selectedItem)}
                >
                  <svg className="icon" aria-hidden="true"><use href="#i-copy" /></svg>
                  Duplicate
                </button>
              )}

              <button
                className="btn"
                type="button"
                disabled={saving}
                onClick={discardChanges}
              >
                Discard
              </button>

              {selectedItem && (
                <button
                  className="btn btn--danger"
                  type="button"
                  disabled={saving || noBackend}
                  onClick={() => setDeleteTarget(selectedItem)}
                >
                  <svg className="icon" aria-hidden="true"><use href="#i-trash" /></svg>
                  Delete
                </button>
              )}
            </div>
          </form>
        </section>
      </div>

      {/* Delete confirmation dialog */}
      <dialog
        ref={deleteDialogRef}
        className="dialog"
        aria-labelledby="del-title"
        onCancel={(e) => {
          e.preventDefault();
          if (!deleting) setDeleteTarget(null);
        }}
      >
        <form className="dialog__box stack" onSubmit={confirmDelete}>
          <h2 id="del-title">Delete baseline item</h2>
          <p>
            Remove <strong>{deleteTarget?.name}</strong> (<span className="mono">{deleteTarget?.id}</span>) from
            the global library?
          </p>
          <div className="row" style={{ justifyContent: 'flex-end' }}>
            <button
              className="btn"
              type="button"
              disabled={deleting}
              onClick={() => setDeleteTarget(null)}
            >
              Cancel
            </button>
            <button className="btn btn--danger" type="submit" disabled={deleting}>
              {deleting ? 'Deleting...' : 'Delete'}
            </button>
          </div>
        </form>
      </dialog>
    </>
  );
}
