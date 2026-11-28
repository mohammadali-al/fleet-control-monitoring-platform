import { useEffect, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { StatusAlert } from '../components/StatusAlert';
import { useBaseline } from '../hooks/useBaseline';
import { useMachines } from '../hooks/useMachines';
import { useManeuver } from '../hooks/useManeuver';
import { fmtTime } from '../lib/fmt';
import { usePageTitle } from '../lib/title';
import { useToast } from '../lib/toast';

function formatMMSS(sec) {
  const s = Math.max(0, Math.round(Number(sec) || 0));
  const m = Math.floor(s / 60);
  const rem = s % 60;
  return `${m}:${String(rem).padStart(2, '0')}`;
}

function parseDurationInput(raw) {
  const s = String(raw || '').trim();
  if (!s) return 0;
  if (/^\d+$/.test(s)) return parseInt(s, 10);
  const parts = s.split(':');
  if (parts.length === 2 && /^\d+$/.test(parts[0]) && /^\d{1,2}$/.test(parts[1])) {
    return parseInt(parts[0], 10) * 60 + parseInt(parts[1], 10);
  }
  return NaN;
}

function emptyDraft() {
  return {
    name: '',
    description: '',
    targetIP: '',
    targetOrigin: '',
    totalText: '05:00',
    patternType: 'all',
    maxCPUCores: '4.0',
    maxRAMGB: '6.0',
    timeline: [],
    agents: [],
  };
}

function draftFromManeuver(mv) {
  return {
    name: mv.name || '',
    description: mv.description || '',
    targetIP: mv.target?.ip || '',
    targetOrigin: mv.target?.origin || '',
    totalText: formatMMSS(mv.total_sec || 300),
    patternType: mv.pattern?.type || 'all',
    maxCPUCores: mv.pattern?.max_cpu_cores ? String(mv.pattern.max_cpu_cores) : '4.0',
    maxRAMGB: mv.pattern?.max_ram_gb ? String(mv.pattern.max_ram_gb) : '6.0',
    timeline: Array.isArray(mv.timeline)
      ? mv.timeline.map((ti) => ({
          id: ti.id,
          baseline_id: ti.baseline_id,
          name: ti.name || '',
          start_sec: Number(ti.start_sec) || 0,
          duration_sec: Number(ti.duration_sec) || 30,
          replicas: Number(ti.replicas) || 1,
          inputs: { ...(ti.inputs || {}) },
        }))
      : [],
    agents: Array.isArray(mv.agents) ? [...mv.agents] : [],
  };
}

function MachineOnlineChip({ state }) {
  if (state === 'online') {
    return (
      <span className="chip chip--ok">
        <svg className="icon" aria-hidden="true"><use href="#i-check" /></svg>
        Online
      </span>
    );
  }
  if (state === 'reconnecting') {
    return (
      <span className="chip chip--warn">
        <svg className="icon" aria-hidden="true"><use href="#i-refresh" /></svg>
        Reconnecting
      </span>
    );
  }
  return (
    <span className="chip chip--danger">
      <svg className="icon" aria-hidden="true"><use href="#i-x" /></svg>
      Offline
    </span>
  );
}

function RunStateChip({ state }) {
  switch (state) {
    case 'preparing':
      return <span className="chip chip--warn chip--live">Preparing (waiting ACKs)</span>;
    case 'committed':
      return <span className="chip chip--info chip--live">Committed (waiting T)</span>;
    case 'running':
      return <span className="chip chip--accent chip--live">Running</span>;
    case 'completed':
      return <span className="chip chip--ok">Completed</span>;
    case 'aborted':
      return <span className="chip chip--danger">Aborted</span>;
    case 'stopped':
      return <span className="chip chip--outline">Stopped</span>;
    default:
      return <span className="chip chip--neutral">{state || 'Idle'}</span>;
  }
}

function AgentAckChip({ state }) {
  switch (state) {
    case 'ready':
      return <span className="chip chip--ok">Ready ACK</span>;
    case 'running':
      return <span className="chip chip--accent chip--live">Running</span>;
    case 'scheduled':
      return <span className="chip chip--info">Scheduled</span>;
    case 'completed':
      return <span className="chip chip--ok">Completed</span>;
    case 'missed':
      return <span className="chip chip--danger">Missed T</span>;
    case 'rejected':
    case 'offline':
    case 'error':
      return <span className="chip chip--danger">{state}</span>;
    case 'stopped':
      return <span className="chip chip--outline">Stopped</span>;
    default:
      return <span className="chip chip--warn">Pending ACK</span>;
  }
}

export function ManeuverPage() {
  usePageTitle('Maneuver');
  const pushToast = useToast();
  const {
    maneuvers,
    runs,
    noBackend,
    error: loadError,
    loading,
    createManeuver,
    updateManeuver,
    duplicateManeuver,
    deleteManeuver,
    previewManeuver,
    startManeuver,
    stopManeuver,
  } = useManeuver();
  const { items: baselineItems } = useBaseline();
  const { machines } = useMachines();

  const [selectedId, setSelectedId] = useState(null); // null => new draft
  const [draft, setDraft] = useState(emptyDraft);
  const [selectedItemId, setSelectedItemId] = useState(null);
  const [baselineSearch, setBaselineSearch] = useState('');
  const [formStatus, setFormStatus] = useState(null);
  const [saving, setSaving] = useState(false);
  const [starting, setStarting] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [previewPlan, setPreviewPlan] = useState(null);
  const [previewError, setPreviewError] = useState('');
  const [gateConfirm, setGateConfirm] = useState(null);
  const [deleteTarget, setDeleteTarget] = useState(null);
  const [nowMs, setNowMs] = useState(() => Date.now());

  const previewSeqRef = useRef(0);
  const gateDialogRef = useRef(null);
  const deleteDialogRef = useRef(null);
  const initializedRef = useRef(false);

  const workers = useMemo(
    () => machines.filter((m) => m.role === 'worker' && !m.removed),
    [machines]
  );

  const baselineById = useMemo(() => {
    const map = {};
    for (const it of baselineItems) map[it.id] = it;
    return map;
  }, [baselineItems]);

  // Select first saved maneuver on initial load once available.
  useEffect(() => {
    if (initializedRef.current || loading) return;
    initializedRef.current = true;
    if (maneuvers.length > 0) {
      setSelectedId(maneuvers[0].id);
      setDraft(draftFromManeuver(maneuvers[0]));
      setSelectedItemId(maneuvers[0].timeline?.[0]?.id || null);
    }
  }, [loading, maneuvers]);

  // Sync dialogs.
  useEffect(() => {
    const dlg = gateDialogRef.current;
    if (!dlg) return;
    if (gateConfirm && !dlg.open) dlg.showModal();
    if (!gateConfirm && dlg.open) dlg.close();
  }, [gateConfirm]);

  useEffect(() => {
    const dlg = deleteDialogRef.current;
    if (!dlg) return;
    if (deleteTarget && !dlg.open) dlg.showModal();
    if (!deleteTarget && dlg.open) dlg.close();
  }, [deleteTarget]);

  const totalSec = useMemo(() => {
    const parsed = parseDurationInput(draft.totalText);
    return Number.isFinite(parsed) && parsed > 0 ? parsed : 300;
  }, [draft.totalText]);

  const activeRun = useMemo(
    () =>
      runs.find((r) => ['preparing', 'committed', 'running'].includes(r.state)) ||
      (selectedId ? runs.find((r) => r.maneuver_id === selectedId) : runs[0]) ||
      null,
    [runs, selectedId]
  );

  const isRunning = activeRun && ['preparing', 'committed', 'running'].includes(activeRun.state);

  // Tick playhead clock while a run is committed/running.
  useEffect(() => {
    if (!activeRun || !['committed', 'running'].includes(activeRun.state)) return undefined;
    const id = window.setInterval(() => setNowMs(Date.now()), 250);
    return () => window.clearInterval(id);
  }, [activeRun]);

  const playheadPct = useMemo(() => {
    if (!activeRun || !['committed', 'running', 'completed'].includes(activeRun.state)) return 0;
    if (activeRun.state === 'completed') return 100;
    const startMs = new Date(activeRun.start_at).getTime();
    if (Number.isNaN(startMs)) return 0;
    const durSec = activeRun.total_sec || totalSec;
    const elapsed = Math.max(0, (nowMs - startMs) / 1000);
    return Math.min(100, Math.max(0, (elapsed / durSec) * 100));
  }, [activeRun, nowMs, totalSec]);

  function buildPayload() {
    return {
      name: draft.name.trim(),
      description: draft.description.trim(),
      target: {
        ip: draft.targetIP.trim(),
        origin: draft.targetOrigin.trim(),
      },
      total_sec: parseDurationInput(draft.totalText),
      pattern: {
        type: draft.patternType || 'all',
        max_cpu_cores: Number(draft.maxCPUCores) || 0,
        max_ram_gb: Number(draft.maxRAMGB) || 0,
      },
      timeline: draft.timeline.map((ti) => ({
        id: ti.id,
        baseline_id: ti.baseline_id,
        name: ti.name,
        start_sec: Number(ti.start_sec) || 0,
        duration_sec: Number(ti.duration_sec) || 0,
        replicas: Number(ti.replicas) || 1,
        inputs: ti.inputs || {},
      })),
      agents: draft.agents,
    };
  }

  // Live materialization preview whenever target/timeline changes.
  useEffect(() => {
    if (noBackend || !draft.targetIP.trim() || draft.timeline.length === 0) {
      setPreviewPlan(null);
      setPreviewError('');
      return undefined;
    }
    const seq = ++previewSeqRef.current;
    const timer = window.setTimeout(async () => {
      const payload = buildPayload();
      if (!payload.name) payload.name = 'Preview';
      if (!(payload.total_sec > 0)) payload.total_sec = 300;
      const r = await previewManeuver(payload);
      if (previewSeqRef.current !== seq) return;
      if (r.ok) {
        setPreviewPlan(r.data);
        setPreviewError('');
      } else {
        setPreviewPlan(null);
        setPreviewError(r.data.error || `Preview failed (${r.status})`);
      }
    }, 150);
    return () => window.clearTimeout(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft.targetIP, draft.targetOrigin, draft.totalText, draft.timeline, noBackend]);

  function selectManeuver(id) {
    setFormStatus(null);
    if (!id) {
      setSelectedId(null);
      setDraft(emptyDraft());
      setSelectedItemId(null);
      return;
    }
    const mv = maneuvers.find((m) => m.id === id);
    if (!mv) return;
    setSelectedId(mv.id);
    setDraft(draftFromManeuver(mv));
    setSelectedItemId(mv.timeline?.[0]?.id || null);
  }

  function addBaselineToTimeline(bItem) {
    const nextNum = draft.timeline.length + 1;
    let id = `item-${nextNum}`;
    const existing = new Set(draft.timeline.map((t) => t.id));
    let n = nextNum;
    while (existing.has(id)) {
      n += 1;
      id = `item-${n}`;
    }
    const dur = Math.min(60, totalSec);
    const nextItem = {
      id,
      baseline_id: bItem.id,
      name: bItem.name,
      start_sec: 0,
      duration_sec: dur,
      replicas: 1,
      inputs: {},
    };
    setDraft((prev) => ({ ...prev, timeline: [...prev.timeline, nextItem] }));
    setSelectedItemId(id);
    setFormStatus(null);
  }

  function updateTimelineItem(itemId, patch) {
    setDraft((prev) => ({
      ...prev,
      timeline: prev.timeline.map((ti) => (ti.id === itemId ? { ...ti, ...patch } : ti)),
    }));
  }

  function removeTimelineItem(itemId) {
    setDraft((prev) => ({
      ...prev,
      timeline: prev.timeline.filter((ti) => ti.id !== itemId),
    }));
    if (selectedItemId === itemId) setSelectedItemId(null);
  }

  function toggleAgent(id) {
    setDraft((prev) => {
      const exists = prev.agents.includes(id);
      return {
        ...prev,
        agents: exists ? prev.agents.filter((a) => a !== id) : [...prev.agents, id],
      };
    });
  }

  async function saveDraft() {
    if (noBackend) return null;
    const payload = buildPayload();
    if (!payload.name) {
      setFormStatus({ kind: 'danger', msg: 'Maneuver name is required.' });
      return null;
    }
    if (!(payload.total_sec > 0)) {
      setFormStatus({ kind: 'danger', msg: 'Total time must be a positive duration (mm:ss or seconds).' });
      return null;
    }
    setSaving(true);
    setFormStatus({ kind: 'info', msg: selectedId ? 'Saving Maneuver...' : 'Creating Maneuver...' });
    const r = selectedId ? await updateManeuver(selectedId, payload) : await createManeuver(payload);
    setSaving(false);
    if (!r.ok) {
      setFormStatus({ kind: 'danger', msg: r.data.error || `Save failed (${r.status})` });
      return null;
    }
    const saved = r.data.maneuver;
    setSelectedId(saved.id);
    setDraft(draftFromManeuver(saved));
    setFormStatus({ kind: 'ok', msg: `Saved "${saved.name}".` });
    pushToast(`Saved Maneuver "${saved.name}".`);
    return saved;
  }

  async function handleSave(e) {
    if (e) e.preventDefault();
    await saveDraft();
  }

  async function handleDuplicate() {
    if (!selectedId || noBackend) return;
    const r = await duplicateManeuver(selectedId);
    if (!r.ok) {
      setFormStatus({ kind: 'danger', msg: r.data.error || `Duplicate failed (${r.status})` });
      return;
    }
    const copy = r.data.maneuver;
    setSelectedId(copy.id);
    setDraft(draftFromManeuver(copy));
    setFormStatus({ kind: 'ok', msg: `Duplicated as "${copy.name}".` });
    pushToast(`Duplicated as "${copy.name}".`);
  }

  async function confirmDelete(e) {
    e.preventDefault();
    if (!deleteTarget || noBackend) return;
    const r = await deleteManeuver(deleteTarget.id);
    if (!r.ok) {
      pushToast(r.data.error || `Delete failed (${r.status})`);
      setDeleteTarget(null);
      return;
    }
    const removed = deleteTarget;
    setDeleteTarget(null);
    selectManeuver(null);
    pushToast(`Deleted "${removed.name}".`);
  }

  async function handleStart(confirm = false) {
    if (noBackend) return;
    const saved = await saveDraft();
    if (!saved) return;

    setStarting(true);
    const r = await startManeuver(saved.id, confirm ? { confirm: true } : {});
    setStarting(false);

    if (r.status === 409 && r.data?.needs_confirmation) {
      setGateConfirm({
        id: saved.id,
        pattern: r.data.pattern,
        command: r.data.command,
        machines: r.data.machines || [],
      });
      return;
    }
    if (!r.ok) {
      setFormStatus({ kind: 'danger', msg: r.data.error || `Start failed (${r.status})` });
      return;
    }
    setGateConfirm(null);
    setFormStatus({ kind: 'ok', msg: `Synchronized start initiated (run ${r.data.run.run_id}).` });
    pushToast(`Started Maneuver "${saved.name}".`);
  }

  async function handleStop() {
    if (!activeRun || noBackend) return;
    setStopping(true);
    const r = await stopManeuver(activeRun.run_id);
    setStopping(false);
    if (!r.ok) {
      setFormStatus({ kind: 'danger', msg: r.data.error || `Stop failed (${r.status})` });
      return;
    }
    pushToast(`Stopped run ${activeRun.run_id}.`);
  }

  const filteredBaseline = useMemo(() => {
    const q = baselineSearch.trim().toLowerCase();
    if (!q) return baselineItems;
    return baselineItems.filter((it) =>
      [it.name, it.id, it.command_template, ...(it.tags || [])].join(' ').toLowerCase().includes(q)
    );
  }, [baselineItems, baselineSearch]);

  const selectedItem = useMemo(
    () => draft.timeline.find((ti) => ti.id === selectedItemId) || null,
    [draft.timeline, selectedItemId]
  );

  const selectedPreviewItem = useMemo(() => {
    if (!selectedItem || !previewPlan?.items) return null;
    return previewPlan.items.find((it) => it.item_id === selectedItem.id) || null;
  }, [selectedItem, previewPlan]);

  const selectedBaseline = selectedItem ? baselineById[selectedItem.baseline_id] || null : null;

  const editablePlaceholders = useMemo(() => {
    if (!selectedBaseline) return [];
    return (selectedBaseline.placeholders || []).filter((p) => p !== 'ip' && p !== 'origin');
  }, [selectedBaseline]);

  const totalReplicas = useMemo(
    () => draft.timeline.reduce((acc, ti) => acc + (Number(ti.replicas) || 1), 0),
    [draft.timeline]
  );

  const rulerTicks = useMemo(() => {
    const out = [];
    for (let i = 0; i < 10; i++) {
      out.push({ pct: i * 10, label: formatMMSS((totalSec * i) / 10) });
    }
    return out;
  }, [totalSec]);

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <div className="crumbs">Plan</div>
          <h1>Maneuver editor</h1>
          <p>
            Compose baseline items on a timeline targeting a single <code>(ip, origin)</code>, mirror onto selected
            worker agents, and run with synchronized start.
          </p>
        </div>
        <div className="page-head__actions">
          <button className="btn" type="button" onClick={() => selectManeuver(null)}>
            <svg className="icon" aria-hidden="true"><use href="#i-plus" /></svg>
            New
          </button>
          <button className="btn" type="button" disabled={noBackend || saving} onClick={handleSave}>
            <svg className="icon" aria-hidden="true"><use href="#i-save" /></svg>
            {saving ? 'Saving...' : 'Save Maneuver'}
          </button>
          {selectedId && (
            <button className="btn" type="button" disabled={noBackend || saving} onClick={handleDuplicate}>
              <svg className="icon" aria-hidden="true"><use href="#i-copy" /></svg>
              Duplicate
            </button>
          )}
          {selectedId && (
            <button
              className="btn btn--danger"
              type="button"
              disabled={noBackend || saving}
              onClick={() => setDeleteTarget(maneuvers.find((m) => m.id === selectedId) || null)}
            >
              <svg className="icon" aria-hidden="true"><use href="#i-trash" /></svg>
              Delete
            </button>
          )}
          {isRunning ? (
            <button className="btn btn--danger" type="button" disabled={stopping} onClick={handleStop}>
              <svg className="icon" aria-hidden="true"><use href="#i-stop" /></svg>
              {stopping ? 'Stopping...' : 'Stop'}
            </button>
          ) : (
            <button
              className="btn btn--primary"
              type="button"
              disabled={noBackend || starting || saving}
              onClick={() => handleStart(false)}
            >
              <svg className="icon" aria-hidden="true"><use href="#i-play" /></svg>
              {starting ? 'Starting...' : 'Start test'}
            </button>
          )}
        </div>
      </div>

      {noBackend && (
        <div className="alert alert--warn" role="note" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
          <div><strong>No backend configured.</strong> Connect a live fleet-server to save and execute Maneuvers.</div>
        </div>
      )}

      {loadError && (
        <div className="alert alert--danger" role="alert" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
          <div>{loadError}</div>
        </div>
      )}

      <div className="alert" role="note" style={{ marginBottom: 'var(--sp-4)' }}>
        <svg className="icon" aria-hidden="true"><use href="#i-info" /></svg>
        <div>
          <strong>All-in-one synchronized execution is live.</strong> The Load Controller (dynamic load shaping &amp; 1 s
          cgroup telemetry) is deferred (D-0012), so Linear and Spikes patterns remain disabled.
        </div>
      </div>

      <StatusAlert status={formStatus} />

      <div className="split" style={{ marginTop: formStatus ? 'var(--sp-4)' : 0 }}>
        {/* ================= Main column ================= */}
        <div className="stack">
          {/* Identity + single (ip, origin) target */}
          <form className="card" aria-labelledby="plan-title" onSubmit={handleSave}>
            <div className="card__head">
              <div>
                <h2 id="plan-title">Maneuver</h2>
                <p>Persisted plan with one target <code>(ip, origin)</code> and mirrored worker timeline.</p>
              </div>
              <span className="chip chip--outline">{selectedId ? selectedId : 'New draft'}</span>
            </div>
            <div className="card__body stack">
              <div className="form-grid">
                <div className="field">
                  <label htmlFor="m-lib">Saved Maneuvers</label>
                  <select
                    className="select"
                    id="m-lib"
                    value={selectedId || ''}
                    onChange={(e) => selectManeuver(e.target.value || null)}
                  >
                    <option value="">New Maneuver...</option>
                    {maneuvers.map((mv) => (
                      <option key={mv.id} value={mv.id}>
                        {mv.name} ({mv.target?.ip})
                      </option>
                    ))}
                  </select>
                </div>
                <div className="field">
                  <label htmlFor="m-name">Maneuver name</label>
                  <input
                    className="input"
                    id="m-name"
                    required
                    placeholder="Web tier probe"
                    value={draft.name}
                    onChange={(e) => setDraft((p) => ({ ...p, name: e.target.value }))}
                  />
                </div>
                <div className="field">
                  <label htmlFor="m-total">Total time</label>
                  <input
                    className="input input--mono"
                    id="m-total"
                    value={draft.totalText}
                    onChange={(e) => setDraft((p) => ({ ...p, totalText: e.target.value }))}
                    aria-describedby="m-total-h"
                  />
                  <span className="field__hint" id="m-total-h">mm:ss or seconds ({totalSec} s)</span>
                </div>
                <div className="field">
                  <label htmlFor="m-desc">Description (optional)</label>
                  <input
                    className="input"
                    id="m-desc"
                    placeholder="Target notes"
                    value={draft.description}
                    onChange={(e) => setDraft((p) => ({ ...p, description: e.target.value }))}
                  />
                </div>
              </div>
              <hr style={{ margin: 0 }} />
              <div className="form-grid">
                <div className="field">
                  <label htmlFor="m-ip">Target IP</label>
                  <input
                    className="input input--mono"
                    id="m-ip"
                    required
                    placeholder="198.51.100.10"
                    value={draft.targetIP}
                    onChange={(e) => setDraft((p) => ({ ...p, targetIP: e.target.value }))}
                  />
                </div>
                <div className="field">
                  <label htmlFor="m-origin">Origin / URL (optional)</label>
                  <input
                    className="input input--mono"
                    id="m-origin"
                    placeholder="https://shop.example.test"
                    value={draft.targetOrigin}
                    onChange={(e) => setDraft((p) => ({ ...p, targetOrigin: e.target.value }))}
                  />
                </div>
              </div>
              <p className="field__hint">
                One Maneuver targets exactly one <code>(ip, origin)</code>; several paths of the same origin are allowed
                via per-item inputs.
              </p>
            </div>
          </form>

          {/* Load pattern */}
          <section className="card" aria-labelledby="pat-title">
            <div className="card__head">
              <div>
                <h2 id="pat-title">Load pattern</h2>
                <p>Only All-in-one is enabled; Linear and Spikes require the deferred Load Controller.</p>
              </div>
            </div>
            <div className="card__body stack">
              <fieldset>
                <legend className="sr-only">Pattern</legend>
                <div className="pattern">
                  <label className="pattern__opt">
                    <input
                      type="radio"
                      name="pattern"
                      value="all"
                      checked={draft.patternType === 'all'}
                      onChange={() => setDraft((p) => ({ ...p, patternType: 'all' }))}
                    />
                    <span className="pattern__card">
                      <svg viewBox="0 0 80 32" preserveAspectRatio="none" aria-hidden="true">
                        <path d="M2 6h76" fill="none" stroke="currentColor" strokeWidth="2.5" />
                        <path d="M2 30h76" stroke="currentColor" strokeWidth="1" opacity=".4" />
                      </svg>
                      <span>
                        <b>All-in-one</b>
                        <small>Runs scheduled replicas directly from start to end. Default.</small>
                      </span>
                    </span>
                  </label>

                  <label className="pattern__opt" title="Needs the Load Controller — deferred (D-0012)">
                    <input
                      type="radio"
                      name="pattern"
                      value="linear"
                      disabled
                      aria-describedby="linear-reason"
                      style={{ cursor: 'not-allowed' }}
                    />
                    <span className="pattern__card" style={{ opacity: 0.55, filter: 'grayscale(1)', cursor: 'not-allowed' }}>
                      <svg viewBox="0 0 80 32" preserveAspectRatio="none" aria-hidden="true">
                        <path
                          d="M2 30h6v-6h6v-6h6v-6h6V6l0 24M28 30h6v-6h6v-6h6v-6h6V6l0 24M54 30h6v-6h6v-6h6v-6h6V6"
                          fill="none"
                          stroke="currentColor"
                          strokeWidth="2.5"
                          strokeLinejoin="round"
                        />
                      </svg>
                      <span>
                        <b>Linear (sawtooth)</b>
                        <small>Ramps 0 to max over a window.</small>
                        <small id="linear-reason" className="tag-reason">
                          <strong>Disabled:</strong> needs the Load Controller &mdash; deferred.
                        </small>
                      </span>
                    </span>
                  </label>

                  <label className="pattern__opt" title="Needs the Load Controller — deferred (D-0012)">
                    <input
                      type="radio"
                      name="pattern"
                      value="spikes"
                      disabled
                      aria-describedby="spike-reason"
                      style={{ cursor: 'not-allowed' }}
                    />
                    <span className="pattern__card" style={{ opacity: 0.55, filter: 'grayscale(1)', cursor: 'not-allowed' }}>
                      <svg viewBox="0 0 80 32" preserveAspectRatio="none" aria-hidden="true">
                        <path
                          d="M2 30h8V6h8v24h12V6h8v24h12V6h8v24h8"
                          fill="none"
                          stroke="currentColor"
                          strokeWidth="2.5"
                          strokeLinejoin="round"
                        />
                      </svg>
                      <span>
                        <b>Spikes</b>
                        <small>Max during the spike, zero in the gaps.</small>
                        <small id="spike-reason" className="tag-reason">
                          <strong>Disabled:</strong> needs the Load Controller &mdash; deferred.
                        </small>
                      </span>
                    </span>
                  </label>
                </div>
              </fieldset>

              <div className="form-grid">
                <div className="field">
                  <label htmlFor="p-cpu">Max CPU / agent (metadata)</label>
                  <input
                    className="input input--mono"
                    id="p-cpu"
                    value={draft.maxCPUCores}
                    onChange={(e) => setDraft((p) => ({ ...p, maxCPUCores: e.target.value }))}
                  />
                </div>
                <div className="field">
                  <label htmlFor="p-ram">Max RAM GB / agent (metadata)</label>
                  <input
                    className="input input--mono"
                    id="p-ram"
                    value={draft.maxRAMGB}
                    onChange={(e) => setDraft((p) => ({ ...p, maxRAMGB: e.target.value }))}
                  />
                </div>
              </div>
            </div>
          </section>

          {/* Timeline */}
          <section className="card" aria-labelledby="tl-title">
            <div className="card__head">
              <div>
                <h2 id="tl-title">Timeline</h2>
                <p>Add baseline items from the right rail; click a bar to configure offsets, replicas, and inputs.</p>
              </div>
              <span className="chip chip--outline">
                {draft.timeline.length} item{draft.timeline.length === 1 ? '' : 's'} &middot; {totalReplicas} replica{totalReplicas === 1 ? '' : 's'}/agent
              </span>
            </div>

            <div className="card__body--flush tl" role="group" aria-label={`Maneuver timeline, 0 to ${formatMMSS(totalSec)}`}>
              <div className="tl__scroll">
                <div className="tl__inner">
                  <div className="tl__row tl__ruler">
                    <div className="tl__label" style={{ background: 'var(--surface-2)' }}>
                      <span className="tl__axis">Time (mm:ss)</span>
                    </div>
                    <div className="tl__track">
                      {rulerTicks.map((tk) => (
                        <span key={tk.pct} className="tl__tick" style={{ left: `${tk.pct}%` }}>
                          {tk.label}
                        </span>
                      ))}
                    </div>
                  </div>

                  <div className="tl__row" style={{ '--lane-h': '3.0rem' }}>
                    <div className="tl__label">
                      <svg className="icon" aria-hidden="true"><use href="#i-activity" /></svg>
                      <span>All-in-one</span>
                    </div>
                    <div className="tl__track">
                      <div className="tl__ceiling">
                        <div
                          className="tl__spikes"
                          style={{ background: 'var(--series-2)', opacity: 0.3 }}
                          aria-label="All-in-one pattern active for full duration"
                        />
                      </div>
                      <div className="tl__playhead" style={{ left: `${playheadPct}%` }} />
                    </div>
                  </div>

                  {draft.timeline.length === 0 ? (
                    <div className="tl__row">
                      <div className="tl__label"><span className="faint">No items yet</span></div>
                      <div className="tl__track" style={{ display: 'flex', alignItems: 'center', paddingInline: 'var(--sp-3)' }}>
                        <span className="faint">Use + on a Baseline item in the right rail to place it on this timeline.</span>
                      </div>
                    </div>
                  ) : (
                    draft.timeline.map((ti, idx) => {
                      const leftPct = Math.min(100, Math.max(0, (ti.start_sec / totalSec) * 100));
                      const widthPct = Math.min(100 - leftPct, Math.max(2, (ti.duration_sec / totalSec) * 100));
                      const label = ti.name || baselineById[ti.baseline_id]?.name || ti.baseline_id;
                      const isSel = ti.id === selectedItemId;
                      return (
                        <div className="tl__row" key={ti.id}>
                          <div className="tl__label">
                            <svg className="icon" aria-hidden="true"><use href="#i-layers" /></svg>
                            <span title={label}>{label}</span>
                          </div>
                          <div className="tl__track">
                            <button
                              type="button"
                              className={`tl__bar${idx % 2 === 1 ? ' tl__bar--alt' : ''}`}
                              style={{
                                left: `${leftPct}%`,
                                width: `${widthPct}%`,
                                outline: isSel ? '2px solid var(--text)' : undefined,
                              }}
                              onClick={() => setSelectedItemId(ti.id)}
                            >
                              {formatMMSS(ti.start_sec)} &ndash; {formatMMSS(ti.start_sec + ti.duration_sec)} ({ti.replicas || 1}x)
                            </button>
                          </div>
                        </div>
                      );
                    })
                  )}
                </div>
              </div>
            </div>

            {selectedItem && (
              <div className="card__body stack" style={{ borderTop: '1px solid var(--border)', background: 'var(--surface-2)' }}>
                <div className="row row--between">
                  <strong>Edit placed item: {selectedItem.name || selectedItem.baseline_id}</strong>
                  <button
                    className="btn btn--sm btn--danger"
                    type="button"
                    onClick={() => removeTimelineItem(selectedItem.id)}
                  >
                    <svg className="icon icon--sm" aria-hidden="true"><use href="#i-trash" /></svg>
                    Remove
                  </button>
                </div>
                <div className="form-grid">
                  <div className="field">
                    <label htmlFor="ti-start">Start offset (s)</label>
                    <input
                      id="ti-start"
                      type="number"
                      min="0"
                      max={Math.max(0, totalSec - 1)}
                      className="input input--mono"
                      value={selectedItem.start_sec}
                      onChange={(e) => updateTimelineItem(selectedItem.id, { start_sec: Math.max(0, parseInt(e.target.value, 10) || 0) })}
                    />
                  </div>
                  <div className="field">
                    <label htmlFor="ti-dur">Duration (s)</label>
                    <input
                      id="ti-dur"
                      type="number"
                      min="1"
                      max={totalSec}
                      className="input input--mono"
                      value={selectedItem.duration_sec}
                      onChange={(e) => updateTimelineItem(selectedItem.id, { duration_sec: Math.max(1, parseInt(e.target.value, 10) || 1) })}
                    />
                  </div>
                  <div className="field">
                    <label htmlFor="ti-rep">Replicas / agent</label>
                    <input
                      id="ti-rep"
                      type="number"
                      min="1"
                      max="64"
                      className="input input--mono"
                      value={selectedItem.replicas}
                      onChange={(e) => updateTimelineItem(selectedItem.id, { replicas: Math.min(64, Math.max(1, parseInt(e.target.value, 10) || 1)) })}
                    />
                  </div>
                </div>
                {editablePlaceholders.length > 0 && (
                  <div className="form-grid">
                    {editablePlaceholders.map((ph) => (
                      <div className="field" key={ph}>
                        <label htmlFor={`ti-in-${ph}`}>
                          Placeholder <code className="mono">{`{${ph}}`}</code>
                        </label>
                        <input
                          id={`ti-in-${ph}`}
                          className="input input--mono"
                          placeholder={selectedBaseline?.defaults?.[ph] ? `default: ${selectedBaseline.defaults[ph]}` : `value for ${ph}`}
                          value={selectedItem.inputs?.[ph] || ''}
                          onChange={(e) =>
                            updateTimelineItem(selectedItem.id, {
                              inputs: { ...(selectedItem.inputs || {}), [ph]: e.target.value },
                            })
                          }
                        />
                      </div>
                    ))}
                  </div>
                )}
                {selectedPreviewItem && (
                  <div className="field">
                    <label>Materialized command</label>
                    <pre className="mono" style={{ margin: 0, padding: 'var(--sp-2)', background: 'var(--surface)', borderRadius: 'var(--radius)', overflowX: 'auto' }}>
                      {selectedPreviewItem.command}
                    </pre>
                  </div>
                )}
              </div>
            )}

            <div className="card__foot row row--between">
              <span>
                {isRunning
                  ? `Playhead running (${Math.round(playheadPct)}%).`
                  : `Playhead is idle. Total time ${formatMMSS(totalSec)}.`}
              </span>
              {previewError && <span style={{ color: 'var(--danger)' }}>{previewError}</span>}
            </div>
          </section>

          {/* Synchronized Execution + Live /ws/events Status */}
          <section className="card" aria-labelledby="run-title">
            <div className="card__head">
              <div>
                <h2 id="run-title">Synchronized run &amp; agent readiness</h2>
                <p>Server pushes plan + future start T, collects ready ACKs, and commits T only if all ACK ready.</p>
              </div>
              <RunStateChip state={activeRun?.state} />
            </div>
            <div className="card__body stack">
              {!activeRun ? (
                <p className="faint">No Maneuver run yet. Save a Maneuver and click Start test.</p>
              ) : (
                <>
                  <dl className="kv">
                    <dt>Run ID</dt><dd className="mono">{activeRun.run_id}</dd>
                    <dt>Start T</dt><dd className="mono">{fmtTime(activeRun.start_at)}</dd>
                    <dt>Target</dt><dd className="mono">{activeRun.target?.ip}{activeRun.target?.origin ? ` (${activeRun.target.origin})` : ''}</dd>
                    {activeRun.error && (<><dt>Reason</dt><dd style={{ color: 'var(--danger)' }}>{activeRun.error}</dd></>)}
                  </dl>
                  <div className="table-wrap">
                    <table className="table">
                      <thead>
                        <tr>
                          <th scope="col">Worker agent</th>
                          <th scope="col">Sync / Run state</th>
                          <th scope="col" className="num">Active replicas</th>
                          <th scope="col">Detail</th>
                        </tr>
                      </thead>
                      <tbody>
                        {Object.values(activeRun.agents || {})
                          .sort((a, b) => a.machine.localeCompare(b.machine))
                          .map((ag) => (
                            <tr key={ag.machine}>
                              <th scope="row" className="cell-primary mono">{ag.machine}</th>
                              <td><AgentAckChip state={ag.state} /></td>
                              <td className="num">{ag.active_replicas || 0}</td>
                              <td className="faint">{ag.reason || (ag.session_ids?.length ? ag.session_ids.join(', ') : '—')}</td>
                            </tr>
                          ))}
                      </tbody>
                    </table>
                  </div>
                </>
              )}
            </div>
          </section>
        </div>

        {/* ================= Right rail ================= */}
        <div className="stack">
          <section className="card" aria-labelledby="lc-title">
            <div className="card__head">
              <h2 id="lc-title">Execution</h2>
              <RunStateChip state={activeRun?.state} />
            </div>
            <div className="card__body stack">
              <dl className="kv">
                <dt>Pattern</dt><dd>All-in-one</dd>
                <dt>Load Controller</dt><dd className="faint">Deferred (D-0012)</dd>
                <dt>Worker agents</dt><dd>{draft.agents.length} selected</dd>
                <dt>Replicas / agent</dt><dd>{totalReplicas}</dd>
              </dl>
              <div className="row">
                <button
                  className="btn btn--sm btn--primary"
                  type="button"
                  disabled={noBackend || isRunning || starting || saving}
                  onClick={() => handleStart(false)}
                >
                  <svg className="icon icon--sm" aria-hidden="true"><use href="#i-play" /></svg>
                  Start
                </button>
                <button
                  className="btn btn--sm btn--danger"
                  type="button"
                  disabled={noBackend || !isRunning || stopping}
                  onClick={handleStop}
                >
                  <svg className="icon icon--sm" aria-hidden="true"><use href="#i-stop" /></svg>
                  Stop
                </button>
              </div>
            </div>
            <div className="card__foot">
              Start aligns across worker agents at T; any agent receiving the order after T reports &ldquo;missed&rdquo;.
            </div>
          </section>

          <section className="card" aria-labelledby="wa-title">
            <div className="card__head">
              <div>
                <h2 id="wa-title">Worker agents</h2>
                <p>The Maneuver is mirrored on each selected worker.</p>
              </div>
            </div>
            <div className="card__body row" style={{ borderBottom: '1px solid var(--border)', paddingBlock: 'var(--sp-2)' }}>
              <button
                className="btn btn--sm"
                type="button"
                onClick={() => setDraft((p) => ({ ...p, agents: workers.map((w) => w.id) }))}
              >
                Select all
              </button>
              <button
                className="btn btn--sm"
                type="button"
                onClick={() => setDraft((p) => ({ ...p, agents: workers.filter((w) => w.state === 'online').map((w) => w.id) }))}
              >
                Select online
              </button>
              <button
                className="btn btn--sm"
                type="button"
                onClick={() => setDraft((p) => ({ ...p, agents: [] }))}
              >
                Select none
              </button>
            </div>
            <ul className="list">
              {workers.length === 0 ? (
                <li className="list__item"><span className="faint">No worker agents registered.</span></li>
              ) : (
                workers.map((w) => {
                  const checked = draft.agents.includes(w.id);
                  const liveAg = activeRun?.agents?.[w.id];
                  return (
                    <li className="list__item" key={w.id}>
                      <label className="check">
                        <input
                          type="checkbox"
                          checked={checked}
                          onChange={() => toggleAgent(w.id)}
                          aria-label={w.id}
                        />
                      </label>
                      <div className="list__main">
                        <b>{w.id}</b>
                        <small>{w.private_ips?.[0] || 'no ip'}</small>
                      </div>
                      {liveAg ? <AgentAckChip state={liveAg.state} /> : <MachineOnlineChip state={w.state} />}
                    </li>
                  );
                })
              )}
            </ul>
          </section>

          <section className="card" aria-labelledby="bi-title">
            <div className="card__head">
              <div>
                <h2 id="bi-title">Baseline items</h2>
                <p>Global library. Click + to place on timeline.</p>
              </div>
            </div>
            <div className="card__body" style={{ paddingBottom: 'var(--sp-2)' }}>
              <div className="input-affix">
                <svg className="icon" aria-hidden="true"><use href="#i-search" /></svg>
                <input
                  className="input"
                  type="search"
                  placeholder="Search items"
                  aria-label="Search baseline items"
                  value={baselineSearch}
                  onChange={(e) => setBaselineSearch(e.target.value)}
                />
              </div>
            </div>
            <ul className="list">
              {filteredBaseline.length === 0 ? (
                <li className="list__item"><span className="faint">No baseline items found.</span></li>
              ) : (
                filteredBaseline.map((it) => (
                  <li className="list__item" key={it.id}>
                    <div className="list__main">
                      <b>{it.name}</b>
                      <small className="mono">{it.command_template}</small>
                    </div>
                    <button
                      className="icon-btn icon-btn--sm icon-btn--bordered"
                      type="button"
                      aria-label={`Add ${it.name} to timeline`}
                      onClick={() => addBaselineToTimeline(it)}
                    >
                      <svg className="icon" aria-hidden="true"><use href="#i-plus" /></svg>
                    </button>
                  </li>
                ))
              )}
            </ul>
            <div className="card__foot">
              <Link to="/baseline">Manage the library</Link>
            </div>
          </section>
        </div>
      </div>

      {/* Dangerous-command confirmation dialog */}
      <dialog
        ref={gateDialogRef}
        className="dialog"
        aria-labelledby="gate-title"
        onCancel={(e) => {
          e.preventDefault();
          setGateConfirm(null);
        }}
      >
        <div className="dialog__box stack">
          <h2 id="gate-title">Confirm dangerous command</h2>
          <p>
            A timeline item matched gate pattern <code className="mono">{gateConfirm?.pattern}</code> on{' '}
            <strong>{gateConfirm?.machines?.join(', ')}</strong>:
          </p>
          <pre className="mono" style={{ margin: 0, padding: 'var(--sp-2)', background: 'var(--surface-2)', borderRadius: 'var(--radius)', overflowX: 'auto' }}>
            {gateConfirm?.command}
          </pre>
          <div className="row" style={{ justifyContent: 'flex-end' }}>
            <button className="btn" type="button" onClick={() => setGateConfirm(null)}>Cancel</button>
            <button className="btn btn--danger" type="button" onClick={() => handleStart(true)}>
              Confirm &amp; Start
            </button>
          </div>
        </div>
      </dialog>

      {/* Delete confirmation dialog */}
      <dialog
        ref={deleteDialogRef}
        className="dialog"
        aria-labelledby="del-mv-title"
        onCancel={(e) => {
          e.preventDefault();
          setDeleteTarget(null);
        }}
      >
        <form className="dialog__box stack" onSubmit={confirmDelete}>
          <h2 id="del-mv-title">Delete Maneuver</h2>
          <p>
            Remove <strong>{deleteTarget?.name}</strong> (<span className="mono">{deleteTarget?.id}</span>)?
          </p>
          <div className="row" style={{ justifyContent: 'flex-end' }}>
            <button className="btn" type="button" onClick={() => setDeleteTarget(null)}>Cancel</button>
            <button className="btn btn--danger" type="submit">Delete</button>
          </div>
        </form>
      </dialog>
    </>
  );
}
