import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { StatusAlert } from '../components/StatusAlert';
import { useMachines } from '../hooks/useMachines';
import { api } from '../lib/api';
import { useEvents } from '../lib/events';
import { decodePayloadHead, fmtAgo, fmtBytes, fmtDateTime, fmtTime, fmtTimeMs } from '../lib/fmt';
import { usePageTitle } from '../lib/title';

const KIND_LABELS = {
  oneshot: 'One-shot',
  pty: 'Shell',
};

function SyncChip({ row }) {
  const s = row.sync || {};
  if (s.state === 'pulling') {
    return <span className="chip chip--accent chip--live">Pulling</span>;
  }
  if (s.state === 'failed') {
    return (
      <span className="chip chip--danger" title={s.error || ''}>
        <svg className="icon icon--sm" aria-hidden="true">
          <use href="#i-x" />
        </svg>
        Failed
      </span>
    );
  }
  if (s.state === 'done') {
    if (s.rejected) {
      return (
        <span className="chip chip--danger">
          <svg className="icon icon--sm" aria-hidden="true">
            <use href="#i-x" />
          </svg>
          {`${s.rejected} rejected (bad SHA-256)`}
        </span>
      );
    }
    return (
      <span className="chip chip--ok">
        <svg className="icon icon--sm" aria-hidden="true">
          <use href="#i-check" />
        </svg>
        {s.fetched ? `Synced, ${s.fetched} fetched` : 'Up to date'}
      </span>
    );
  }
  if (row.state !== 'online') {
    return (
      <span className="chip chip--danger">
        <svg className="icon icon--sm" aria-hidden="true">
          <use href="#i-x" />
        </svg>
        Agent offline
      </span>
    );
  }
  return <span className="chip chip--outline">Not pulled yet</span>;
}

export function EvidencePage() {
  usePageTitle('Evidence');

  const { machines, noBackend: machinesNoBackend } = useMachines();
  const workers = useMemo(
    () => machines.filter((m) => m.role === 'worker' && !m.removed),
    [machines]
  );

  const [archiveNoBackend, setArchiveNoBackend] = useState(false);
  const noBackend = machinesNoBackend || archiveNoBackend;
  const [status, setStatus] = useState(null);
  const [activeTab, setActiveTab] = useState('sessions'); // 'sessions' | 'sync' | 'export'
  const tabRefs = useRef({});

  // Sessions browser + filters
  const [filterQ, setFilterQ] = useState('');
  const [filterMachine, setFilterMachine] = useState('');
  const [filterKind, setFilterKind] = useState('');
  const [sessionsLoaded, setSessionsLoaded] = useState(false);
  const [sessionsError, setSessionsError] = useState('');
  const [sessions, setSessions] = useState([]);

  // Selected session packet records
  const [selectedSession, setSelectedSession] = useState(null); // session object
  const [recordsLoading, setRecordsLoading] = useState(false);
  const [recordsError, setRecordsError] = useState('');
  const [records, setRecords] = useState([]);
  const [recordsMore, setRecordsMore] = useState(false);

  // Sync & disk state
  const [syncLoaded, setSyncLoaded] = useState(false);
  const [syncError, setSyncError] = useState('');
  const [syncWorkers, setSyncWorkers] = useState([]);
  const [diskInfo, setDiskInfo] = useState(null);
  const [pullingAll, setPullingAll] = useState(false);
  const [pollingSync, setPollingSync] = useState(false);

  // Manifest
  const [manifestMachine, setManifestMachine] = useState('');
  const [manifestError, setManifestError] = useState('');
  const [manifestSegments, setManifestSegments] = useState([]);
  const [manifestLoaded, setManifestLoaded] = useState(false);

  // Export form + export history (browser session)
  const [exportScope, setExportScope] = useState('session'); // 'session' | 'machine' | 'range'
  const [exportSession, setExportSession] = useState('');
  const [exportMachine, setExportMachine] = useState('');
  const [exportFrom, setExportFrom] = useState('');
  const [exportTo, setExportTo] = useState('');
  const [exportFormat, setExportFormat] = useState('pcapng');
  const [exportsList, setExportsList] = useState([]);

  const filterDebounceRef = useRef(0);
  const eventDebounceRef = useRef(0);
  const wasPullingRef = useRef(false);

  // Keep manifestMachine defaulted to the first registered worker.
  useEffect(() => {
    if (workers.length === 0) {
      setManifestMachine('');
      return;
    }
    if (!manifestMachine || !workers.some((w) => w.id === manifestMachine)) {
      setManifestMachine(workers[0].id);
    }
  }, [workers, manifestMachine]);

  // Keep exportSession aligned with available sessions.
  useEffect(() => {
    if (sessions.length === 0) {
      setExportSession('');
      return;
    }
    const keys = new Set(sessions.map((s) => `${s.machine}/${s.session}`));
    if (!exportSession || !keys.has(exportSession)) {
      setExportSession(`${sessions[0].machine}/${sessions[0].session}`);
    }
  }, [sessions, exportSession]);

  const loadSessions = useCallback(async (qVal = filterQ, mVal = filterMachine, kVal = filterKind) => {
    const qs = new URLSearchParams();
    if (qVal.trim()) qs.set('q', qVal.trim());
    if (mVal) qs.set('machine', mVal);
    if (kVal) qs.set('kind', kVal);

    const r = await api('GET', `/api/evidence/sessions?${qs.toString()}`);
    setSessionsLoaded(true);
    if (r.status === 503) {
      setArchiveNoBackend(true);
      setSessionsError('');
      setSessions([]);
      return;
    }
    if (!r.ok) {
      setSessionsError(r.data.error || 'The archive could not be read.');
      return;
    }
    setArchiveNoBackend(false);
    setSessionsError('');
    setSessions(Array.isArray(r.data.sessions) ? r.data.sessions : []);
  }, [filterQ, filterMachine, filterKind]);

  const loadManifest = useCallback(async (machineId) => {
    if (!machineId) {
      setManifestSegments([]);
      setManifestError('');
      setManifestLoaded(false);
      return;
    }
    const r = await api('GET', `/api/evidence/manifest/${encodeURIComponent(machineId)}`);
    setManifestLoaded(true);
    if (r.status === 503) {
      setArchiveNoBackend(true);
      setManifestSegments([]);
      setManifestError('');
      return;
    }
    if (!r.ok) {
      setManifestSegments([]);
      setManifestError(r.data.error || 'Unavailable.');
      return;
    }
    setManifestError('');
    setManifestSegments(Array.isArray(r.data.segments) ? r.data.segments : []);
  }, []);

  const loadSync = useCallback(async () => {
    const r = await api('GET', '/api/evidence/sync');
    setSyncLoaded(true);
    if (r.status === 503) {
      setArchiveNoBackend(true);
      setSyncError('');
      setSyncWorkers([]);
      setDiskInfo(null);
      setPollingSync(false);
      return;
    }
    if (!r.ok) {
      setSyncError(r.data.error || 'Sync status is unavailable.');
      setPollingSync(false);
      return;
    }
    setArchiveNoBackend(false);
    setSyncError('');
    const list = Array.isArray(r.data.workers) ? r.data.workers : [];
    setSyncWorkers(list);
    setDiskInfo(r.data.disk || null);

    const busy = list.some((w) => w.sync && w.sync.state === 'pulling');
    setPollingSync(busy);
    if (busy) {
      loadSessions();
    } else if (wasPullingRef.current) {
      loadSessions();
      if (manifestMachine) loadManifest(manifestMachine);
    }
    wasPullingRef.current = busy;
  }, [loadSessions, loadManifest, manifestMachine]);

  // Initial fetch
  useEffect(() => {
    loadSessions('', '', '');
    loadSync();
    return () => {
      if (filterDebounceRef.current) window.clearTimeout(filterDebounceRef.current);
      if (eventDebounceRef.current) window.clearTimeout(eventDebounceRef.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Reload sessions when filters change (debounced for search input)
  const scheduleSessionsLoad = useCallback(
    (qVal, mVal, kVal, delayMs) => {
      if (filterDebounceRef.current) window.clearTimeout(filterDebounceRef.current);
      filterDebounceRef.current = window.setTimeout(() => {
        filterDebounceRef.current = 0;
        loadSessions(qVal, mVal, kVal);
      }, delayMs);
    },
    [loadSessions]
  );

  // Poll /api/evidence/sync every 2s while a pull is active
  useEffect(() => {
    if (!pollingSync) return undefined;
    const iv = window.setInterval(() => {
      loadSync();
    }, 2000);
    return () => window.clearInterval(iv);
  }, [pollingSync, loadSync]);

  // Reload manifest when selected manifest machine changes
  useEffect(() => {
    if (manifestMachine) {
      loadManifest(manifestMachine);
    }
  }, [manifestMachine, loadManifest]);

  // Keep sessions & sync responsive to /ws/events (agent connect/drop or finished commands)
  const refreshLiveSoon = useCallback(() => {
    if (eventDebounceRef.current) window.clearTimeout(eventDebounceRef.current);
    eventDebounceRef.current = window.setTimeout(() => {
      eventDebounceRef.current = 0;
      loadSync();
      loadSessions();
    }, 250);
  }, [loadSync, loadSessions]);

  useEvents(
    (msg) => {
      if (!msg) return;
      if (msg.type === 'machine' || (msg.type === 'command' && msg.event === 'exited')) {
        refreshLiveSoon();
      }
    },
    () => {
      loadSync();
      loadSessions();
    }
  );

  const openSession = async (s) => {
    setSelectedSession(s);
    setRecordsLoading(true);
    setRecordsError('');
    setRecords([]);
    setRecordsMore(false);

    const url = `/api/evidence/sessions/${encodeURIComponent(s.machine)}/${encodeURIComponent(s.session)}/records`;
    const r = await api('GET', url);
    setRecordsLoading(false);
    if (!r.ok) {
      setRecordsError(r.data.error || 'Records could not be read.');
      return;
    }
    setRecords(Array.isArray(r.data.records) ? r.data.records : []);
    setRecordsMore(Boolean(r.data.more));
  };

  const handlePull = async (machine) => {
    if (!machine) setPullingAll(true);
    const r = await api('POST', '/api/evidence/pull', machine ? { machine } : {});
    if (!machine) setPullingAll(false);

    const res = Array.isArray(r.data.results) ? r.data.results : [];
    if (!r.ok) {
      setStatus({
        kind: 'danger',
        msg: r.data.error || (res[0] && res[0].error) || `Pull failed (${r.status})`,
      });
    } else {
      const failed = res.filter((x) => !x.started);
      setStatus({
        kind: failed.length ? 'info' : 'ok',
        msg: res.length
          ? `Pull started on ${res.length - failed.length} worker agent(s).${
              failed.length
                ? ` Not started: ${failed.map((x) => `${x.machine} (${x.error})`).join('; ')}`
                : ''
            }`
          : 'No worker agent is online to pull from.',
      });
    }
    loadSync();
    setPollingSync(true);
  };

  const handleExportSubmit = (e) => {
    e.preventDefault();
    const qs = new URLSearchParams();
    qs.set('format', exportFormat);
    let label = 'archive';

    if (exportScope === 'session') {
      if (!exportSession) {
        setStatus({ kind: 'danger', msg: 'There is no session to export yet.' });
        return;
      }
      const slash = exportSession.indexOf('/');
      const mach = exportSession.slice(0, slash);
      const sess = exportSession.slice(slash + 1);
      qs.set('machine', mach);
      qs.set('session', sess);
      label = sess;
    } else {
      if (exportMachine) {
        qs.set('machine', exportMachine);
        label = exportMachine;
      }
      if (exportScope === 'range') {
        if (!exportFrom && !exportTo) {
          setStatus({ kind: 'danger', msg: 'Pick a start or an end time for a time-range export.' });
          return;
        }
        if (exportFrom) qs.set('from', new Date(exportFrom).toISOString());
        if (exportTo) qs.set('to', new Date(exportTo).toISOString());
        label += ` ${exportFrom || ''} to ${exportTo || 'now'}`;
      }
    }

    const url = `/api/evidence/export?${qs.toString()}`;
    setExportsList((prev) => [
      {
        id: `${Date.now()}-${prev.length}`,
        filename: `${label}.${exportFormat}`,
        requestedAt: new Date().toISOString(),
        url,
      },
      ...prev,
    ]);
    setStatus({ kind: 'ok', msg: `Export started: your browser will download the ${exportFormat} file.` });

    const a = document.createElement('a');
    a.href = url;
    a.setAttribute('download', '');
    a.style.display = 'none';
    document.body.appendChild(a);
    a.click();
    a.remove();
  };

  const selectTab = (tab) => {
    setActiveTab(tab);
    if (tab === 'sync') {
      loadSync();
      if (manifestMachine) loadManifest(manifestMachine);
    }
  };

  const handleTabKeyDown = (e) => {
    const order = ['sessions', 'sync', 'export'];
    const idx = order.indexOf(activeTab);
    let next = null;
    if (e.key === 'ArrowRight') next = order[(idx + 1) % order.length];
    else if (e.key === 'ArrowLeft') next = order[(idx + order.length - 1) % order.length];
    else if (e.key === 'Home') next = order[0];
    else if (e.key === 'End') next = order[order.length - 1];
    if (next) {
      e.preventDefault();
      selectTab(next);
      tabRefs.current[next]?.focus();
    }
  };

  const detailSrcs = useMemo(() => {
    const set = new Set();
    records.forEach((p) => {
      if (p.src_ip_used) set.add(p.src_ip_used);
    });
    return Array.from(set).join(', ') || 'not recorded';
  }, [records]);

  const diskPct = diskInfo?.used_percent || 0;
  const diskState = diskInfo?.state || '';
  const diskClass = diskState === 'paused' ? ' meter--danger' : diskState === 'warn' ? ' meter--warn' : '';
  const diskLabel = diskInfo
    ? `Archive disk ${diskPct.toFixed(0)}% used${
        diskState === 'paused' ? ' - intake paused' : diskState === 'warn' ? ' - warning' : ''
      }`
    : 'Checking...';

  const selectedKey = selectedSession ? `${selectedSession.machine}/${selectedSession.session}` : '';

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Evidence</h1>
          <p>
            Evidence logs recorded by worker agents: what each agent sent while running panel commands. Evidence stays on
            the agent until you pull it; it is separate from monitor data.
          </p>
        </div>
        <div className="page-head__actions">
          <button
            className="btn"
            type="button"
            id="ev-pull-all"
            disabled={noBackend || pullingAll}
            onClick={() => handlePull('')}
          >
            <svg className="icon" aria-hidden="true">
              <use href="#i-download" />
            </svg>
            Pull from all online
          </button>
        </div>
      </div>

      {noBackend && (
        <div className="alert alert--warn" role="status" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true">
            <use href="#i-alert" />
          </svg>
          <div>The panel is running without a backend, so the evidence archive is unavailable.</div>
        </div>
      )}

      <StatusAlert status={status} id="ev-status" style={{ marginBottom: 'var(--sp-4)' }} />

      <div className="card">
        <div className="tabs" role="tablist" aria-label="Evidence views">
          <button
            ref={(el) => {
              tabRefs.current.sessions = el;
            }}
            className="tab"
            role="tab"
            id="etab-s"
            aria-controls="ep-s"
            aria-selected={String(activeTab === 'sessions')}
            tabIndex={activeTab === 'sessions' ? 0 : -1}
            type="button"
            onClick={() => selectTab('sessions')}
            onKeyDown={handleTabKeyDown}
          >
            <svg className="icon icon--sm" aria-hidden="true">
              <use href="#i-evidence" />
            </svg>
            Sessions
          </button>
          <button
            ref={(el) => {
              tabRefs.current.sync = el;
            }}
            className="tab"
            role="tab"
            id="etab-y"
            aria-controls="ep-y"
            aria-selected={String(activeTab === 'sync')}
            tabIndex={activeTab === 'sync' ? 0 : -1}
            type="button"
            onClick={() => selectTab('sync')}
            onKeyDown={handleTabKeyDown}
          >
            <svg className="icon icon--sm" aria-hidden="true">
              <use href="#i-refresh" />
            </svg>
            Sync (pull)
          </button>
          <button
            ref={(el) => {
              tabRefs.current.export = el;
            }}
            className="tab"
            role="tab"
            id="etab-x"
            aria-controls="ep-x"
            aria-selected={String(activeTab === 'export')}
            tabIndex={activeTab === 'export' ? 0 : -1}
            type="button"
            onClick={() => selectTab('export')}
            onKeyDown={handleTabKeyDown}
          >
            <svg className="icon icon--sm" aria-hidden="true">
              <use href="#i-download" />
            </svg>
            Export
          </button>
        </div>

        {/* SESSIONS */}
        <div
          className="card__body tabpanel"
          role="tabpanel"
          id="ep-s"
          aria-labelledby="etab-s"
          hidden={activeTab !== 'sessions'}
        >
          <div className="stack">
            <div className="row">
              <div className="input-affix" style={{ flex: '1 1 16rem', maxWidth: '24rem' }}>
                <svg className="icon" aria-hidden="true">
                  <use href="#i-search" />
                </svg>
                <input
                  className="input"
                  id="e-q"
                  type="search"
                  placeholder="Search sessions"
                  aria-label="Search sessions"
                  value={filterQ}
                  onChange={(e) => {
                    const next = e.target.value;
                    setFilterQ(next);
                    scheduleSessionsLoad(next, filterMachine, filterKind, 250);
                  }}
                />
              </div>
              <label className="sr-only" htmlFor="e-m">
                Machine
              </label>
              <select
                className="select"
                id="e-m"
                style={{ width: 'auto' }}
                value={filterMachine}
                onChange={(e) => {
                  const next = e.target.value;
                  setFilterMachine(next);
                  scheduleSessionsLoad(filterQ, next, filterKind, 0);
                }}
              >
                <option value="">All worker agents</option>
                {workers.map((w) => (
                  <option key={w.id} value={w.id}>
                    {w.id}
                  </option>
                ))}
              </select>
              <label className="sr-only" htmlFor="e-k">
                Kind
              </label>
              <select
                className="select"
                id="e-k"
                style={{ width: 'auto' }}
                value={filterKind}
                onChange={(e) => {
                  const next = e.target.value;
                  setFilterKind(next);
                  scheduleSessionsLoad(filterQ, filterMachine, next, 0);
                }}
              >
                <option value="">All kinds</option>
                <option value="oneshot">One-shot</option>
                <option value="pty">Shell</option>
              </select>
              <span className="muted" id="ev-count" aria-live="polite">
                {sessionsLoaded && !noBackend && !sessionsError
                  ? `${sessions.length} ${sessions.length === 1 ? 'session' : 'sessions'}`
                  : ''}
              </span>
            </div>

            <div className="card">
              <div className="table-wrap">
                <table className="table table--sticky-first">
                  <caption className="sr-only">Evidence sessions in the archive</caption>
                  <thead>
                    <tr>
                      <th scope="col">Session</th>
                      <th scope="col">Worker agent</th>
                      <th scope="col">Kind</th>
                      <th scope="col">Command</th>
                      <th scope="col">Started</th>
                      <th scope="col" className="num">
                        Packets
                      </th>
                      <th scope="col" className="num">
                        Dropped
                      </th>
                      <th scope="col">Archive</th>
                    </tr>
                  </thead>
                  <tbody id="ev-sessions">
                    {noBackend ? (
                      <tr>
                        <td colSpan={8} className="faint">
                          No backend is configured on this server.
                        </td>
                      </tr>
                    ) : !sessionsLoaded ? (
                      <tr>
                        <td colSpan={8} className="faint">
                          Loading the archive...
                        </td>
                      </tr>
                    ) : sessionsError ? (
                      <tr>
                        <td colSpan={8} className="faint">
                          {sessionsError}
                        </td>
                      </tr>
                    ) : sessions.length === 0 ? (
                      <tr>
                        <td colSpan={8} className="faint">
                          No evidence in the archive yet. Pull from a worker agent on the Sync tab.
                        </td>
                      </tr>
                    ) : (
                      sessions.map((s) => {
                        const key = `${s.machine}/${s.session}`;
                        const isSel = key === selectedKey;
                        return (
                          <tr
                            key={key}
                            tabIndex={0}
                            style={{ cursor: 'pointer' }}
                            aria-selected={isSel ? 'true' : undefined}
                            onClick={() => openSession(s)}
                            onKeyDown={(e) => {
                              if (e.key === 'Enter' || e.key === ' ') {
                                e.preventDefault();
                                openSession(s);
                              }
                            }}
                          >
                            <th scope="row" className="mono">
                              {s.session}
                            </th>
                            <td>{s.machine}</td>
                            <td className="nowrap">
                              {KIND_LABELS[s.kind] || (s.kind ? s.kind : 'Unknown')}
                            </td>
                            <td className="mono truncate">{s.command || ''}</td>
                            <td className="nowrap">{fmtDateTime(s.started)}</td>
                            <td className="num">{(s.packets ?? 0).toLocaleString()}</td>
                            <td className="num">{String(s.dropped ?? 0)}</td>
                            <td>
                              {s.online ? (
                                <span className="chip chip--ok">
                                  <svg className="icon icon--sm" aria-hidden="true">
                                    <use href="#i-check" />
                                  </svg>
                                  {`${s.segments} ${s.segments === 1 ? 'segment' : 'segments'}`}
                                </span>
                              ) : (
                                <span className="chip chip--danger">
                                  <svg className="icon icon--sm" aria-hidden="true">
                                    <use href="#i-x" />
                                  </svg>
                                  Agent offline
                                </span>
                              )}
                            </td>
                          </tr>
                        );
                      })
                    )}
                  </tbody>
                </table>
              </div>
              <div className="card__foot">
                Sessions are read from the archive on the main server, so a session shows here once its segments were
                pulled (or pushed by an idle agent). Packets is the exact record count of this session across its
                archived segments. Command and kind come from the command history, which is kept in memory only.
              </div>
            </div>

            <section className="card" aria-labelledby="rec-title" id="ev-detail" hidden={!selectedSession}>
              <div className="card__head">
                <div>
                  <h2 id="rec-title">
                    Session <span className="mono" id="ev-detail-id">{selectedSession?.session || ''}</span>: packet
                    records
                  </h2>
                  <p id="ev-detail-sub">
                    {selectedSession
                      ? `On ${selectedSession.machine}. Each row is one outbound packet.`
                      : 'Each row is one outbound packet.'}
                  </p>
                </div>
                <div className="row">
                  <span className="chip chip--ok" id="ev-detail-verified">
                    <svg className="icon icon--sm" aria-hidden="true">
                      <use href="#i-check" />
                    </svg>
                    All segments verified (SHA-256)
                  </span>
                  <span className="chip chip--outline" id="ev-detail-dropped">
                    {selectedSession ? `${selectedSession.dropped ?? 0} dropped` : ''}
                  </span>
                </div>
              </div>

              <div className="card__body">
                <dl
                  className="kv"
                  style={{ gridTemplateColumns: 'repeat(auto-fit,minmax(14rem,1fr))', gap: 'var(--sp-3) var(--sp-5)' }}
                >
                  <div>
                    <dt>Command</dt>
                    <dd className="mono" style={{ textAlign: 'left' }} id="ev-detail-cmd">
                      {selectedSession ? selectedSession.command || '(not in the command history)' : ''}
                    </dd>
                  </div>
                  <div>
                    <dt>Source address used</dt>
                    <dd className="mono" style={{ textAlign: 'left' }} id="ev-detail-src">
                      {recordsLoading ? '' : detailSrcs}
                    </dd>
                  </div>
                </dl>
              </div>

              <div className="table-wrap">
                <table className="table">
                  <caption className="sr-only">Packet records of the selected session</caption>
                  <thead>
                    <tr>
                      <th scope="col">Timestamp</th>
                      <th scope="col">Proto</th>
                      <th scope="col">Source</th>
                      <th scope="col">Destination</th>
                      <th scope="col">Flags</th>
                      <th scope="col" className="num">
                        Seq / Ack
                      </th>
                      <th scope="col">Payload</th>
                      <th scope="col">Payload SHA-256</th>
                    </tr>
                  </thead>
                  <tbody id="ev-records">
                    {recordsLoading ? (
                      <tr>
                        <td colSpan={8} className="faint">
                          Loading records...
                        </td>
                      </tr>
                    ) : recordsError ? (
                      <tr>
                        <td colSpan={8} className="faint">
                          {recordsError}
                        </td>
                      </tr>
                    ) : records.length === 0 ? (
                      <tr>
                        <td colSpan={8} className="faint">
                          This session has no packet records.
                        </td>
                      </tr>
                    ) : (
                      records.map((p, idx) => {
                        const hash = p.payload_sha256 || '';
                        const head = decodePayloadHead(p.payload_head);
                        return (
                          <tr key={`${p.time}-${p.src}-${p.dst}-${idx}`}>
                            <td className="mono nowrap">{fmtTimeMs(p.time)}</td>
                            <td>{p.protocol}</td>
                            <td className="mono">{p.src}</td>
                            <td className="mono">{p.dst}</td>
                            <td className="mono">{p.flags || ''}</td>
                            <td className="num mono">
                              {p.protocol === 'TCP' ? `${p.seq ?? 0} / ${p.ack ?? 0}` : '-'}
                            </td>
                            <td>
                              <span
                                className={`chip chip--${
                                  p.payload_status === 'trimmed' ? 'warn' : 'outline'
                                }`}
                              >
                                {`${p.payload_status} · ${p.payload_bytes} B`}
                              </span>
                              {head && (
                                <code className="payload-head" data-payload-head title={head.full}>
                                  {head.full}
                                </code>
                              )}
                            </td>
                            <td className="hash" title={hash || undefined}>
                              {hash ? `${hash.slice(0, 8)}...${hash.slice(-4)}` : ''}
                            </td>
                          </tr>
                        );
                      })
                    )}
                  </tbody>
                </table>
              </div>

              <div className="card__foot">
                <span id="ev-detail-more">
                  {recordsMore
                    ? `Showing the first ${records.length} records; export the session for all of them. `
                    : ''}
                </span>
                <strong>Honest scope.</strong> Capture is outbound-only: it proves what the agent sent, including its
                own half of the TCP handshake, not that the target replied. Target responsiveness is monitor data.
              </div>
            </section>
          </div>
        </div>

        {/* SYNC */}
        <div
          className="card__body tabpanel"
          role="tabpanel"
          id="ep-y"
          aria-labelledby="etab-y"
          hidden={activeTab !== 'sync'}
        >
          <div className="stack">
            <div className="alert">
              <svg className="icon" aria-hidden="true">
                <use href="#i-info" />
              </svg>
              <div>
                <strong>Pull</strong> compares the manifest on the main server with the agent&apos;s, downloads only
                missing or changed segments and checks each one&apos;s SHA-256. An interrupted transfer resumes inside
                the segment. Idle agents may also push pending segments on their own.
              </div>
            </div>

            <section className="card" aria-labelledby="sync-title">
              <div className="card__head">
                <h2 id="sync-title">Per worker agent</h2>
                <span className="muted">Segments rotate every 5 minutes or 50 MB</span>
              </div>
              <div className="table-wrap">
                <table className="table table--sticky-first">
                  <caption className="sr-only">Evidence sync status per worker agent</caption>
                  <thead>
                    <tr>
                      <th scope="col">Worker agent</th>
                      <th scope="col">Archived on main</th>
                      <th scope="col">Last sync</th>
                      <th scope="col">Status</th>
                      <th scope="col">Finished</th>
                      <th scope="col">
                        <span className="sr-only">Actions</span>
                      </th>
                    </tr>
                  </thead>
                  <tbody id="ev-sync-rows">
                    {noBackend ? (
                      <tr>
                        <td colSpan={6} className="faint">
                          No backend is configured on this server.
                        </td>
                      </tr>
                    ) : !syncLoaded ? (
                      <tr>
                        <td colSpan={6} className="faint">
                          Loading...
                        </td>
                      </tr>
                    ) : syncError ? (
                      <tr>
                        <td colSpan={6} className="faint">
                          {syncError}
                        </td>
                      </tr>
                    ) : syncWorkers.length === 0 ? (
                      <tr>
                        <td colSpan={6} className="faint">
                          No worker agents are registered.
                        </td>
                      </tr>
                    ) : (
                      syncWorkers.map((w) => {
                        const s = w.sync || {};
                        const lastSyncText =
                          s.state === 'done' || s.state === 'failed'
                            ? `${s.segments} on agent, ${s.fetched} fetched${
                                s.resumed ? ` (${s.resumed} resumed)` : ''
                              }, ${s.skipped} already archived, ${fmtBytes(s.bytes)}`
                            : s.state === 'pulling'
                              ? 'in progress'
                              : '-';
                        const disabled = w.state !== 'online' || s.state === 'pulling';
                        return (
                          <tr key={w.machine}>
                            <th scope="row" className="cell-primary">
                              {w.machine}
                            </th>
                            <td className="mono">{`${w.archived_segments} segments, ${fmtBytes(w.archived_bytes)}`}</td>
                            <td className={s.state === 'idle' ? 'faint' : undefined}>{lastSyncText}</td>
                            <td>
                              <SyncChip row={w} />
                              {s.error && s.state === 'failed' && <div className="cell-sub">{s.error}</div>}
                            </td>
                            <td className="nowrap">
                              {s.finished
                                ? `${fmtAgo(s.finished)}${s.trigger === 'push' ? ' (agent push)' : ''}`
                                : '-'}
                            </td>
                            <td className="actions">
                              <button
                                className={`btn btn--sm${w.state === 'online' ? ' btn--primary' : ''}`}
                                type="button"
                                aria-disabled={disabled ? 'true' : undefined}
                                disabled={disabled}
                                onClick={() => handlePull(w.machine)}
                              >
                                <svg className="icon icon--sm" aria-hidden="true">
                                  <use href="#i-download" />
                                </svg>
                                Pull
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
                An offline agent&apos;s evidence is safe on that agent; pull it once it returns. The last-sync figures
                are kept in memory and reset when the server restarts.
              </div>
            </section>

            <div className="grid">
              <section className="card col-6" aria-labelledby="arch-title">
                <div className="card__head">
                  <div>
                    <h2 id="arch-title">Main-server archive disk</h2>
                    <p>Unlimited retention; the guard never deletes.</p>
                  </div>
                </div>
                <div className="card__body stack" id="ev-disk">
                  <div className={`meter${diskClass}`}>
                    <div className="meter__bar">
                      <div className="meter__fill" style={{ width: `${Math.min(100, diskPct)}%` }} />
                    </div>
                    <div className="meter__text">
                      <span>{diskLabel}</span>
                      <span>{diskInfo ? `${diskPct.toFixed(0)}%` : ''}</span>
                    </div>
                  </div>
                  <p className="field__hint">80% raises a warning here; 90% pauses intake of new segments.</p>
                </div>
              </section>

              <section className="card col-6" aria-labelledby="man-title">
                <div className="card__head">
                  <div>
                    <h2 id="man-title">Manifest</h2>
                    <p>Archived segments, each with its SHA-256.</p>
                  </div>
                  <div>
                    <label className="sr-only" htmlFor="ev-man-m">
                      Machine
                    </label>
                    <select
                      className="select"
                      id="ev-man-m"
                      style={{ width: 'auto' }}
                      value={manifestMachine}
                      onChange={(e) => setManifestMachine(e.target.value)}
                    >
                      {workers.length === 0 ? (
                        <option value="">No worker agents</option>
                      ) : (
                        workers.map((w) => (
                          <option key={w.id} value={w.id}>
                            {w.id}
                          </option>
                        ))
                      )}
                    </select>
                  </div>
                </div>
                <div className="table-wrap">
                  <table className="table">
                    <caption className="sr-only">Segment manifest of the selected machine</caption>
                    <thead>
                      <tr>
                        <th scope="col">Segment</th>
                        <th scope="col">Range</th>
                        <th scope="col" className="num">
                          Size
                        </th>
                        <th scope="col" className="num">
                          Records
                        </th>
                        <th scope="col">SHA-256</th>
                        <th scope="col">Check</th>
                      </tr>
                    </thead>
                    <tbody id="ev-manifest">
                      {!manifestMachine ? (
                        <tr>
                          <td colSpan={6} className="faint">
                            Choose a worker agent.
                          </td>
                        </tr>
                      ) : manifestError ? (
                        <tr>
                          <td colSpan={6} className="faint">
                            {manifestError}
                          </td>
                        </tr>
                      ) : manifestLoaded && manifestSegments.length === 0 ? (
                        <tr>
                          <td colSpan={6} className="faint">
                            No segments archived for this machine yet.
                          </td>
                        </tr>
                      ) : (
                        manifestSegments
                          .slice(-30)
                          .reverse()
                          .map((g) => (
                            <tr key={g.file}>
                              <td className="mono">{g.file}</td>
                              <td className="nowrap">{`${fmtTime(g.start)}-${fmtTime(g.end)}`}</td>
                              <td className="num">{fmtBytes(g.bytes)}</td>
                              <td className="num">{(g.records ?? 0).toLocaleString()}</td>
                              <td className="hash" title={g.sha256 || undefined}>
                                {g.sha256 ? `${g.sha256.slice(0, 6)}...${g.sha256.slice(-4)}` : ''}
                              </td>
                              <td>
                                <span className="chip chip--ok">
                                  <svg className="icon icon--sm" aria-hidden="true">
                                    <use href="#i-check" />
                                  </svg>
                                  Verified
                                </span>
                              </td>
                            </tr>
                          ))
                      )}
                    </tbody>
                  </table>
                </div>
              </section>
            </div>
          </div>
        </div>

        {/* EXPORT */}
        <div
          className="card__body tabpanel"
          role="tabpanel"
          id="ep-x"
          aria-labelledby="etab-x"
          hidden={activeTab !== 'export'}
        >
          <div className="grid">
            <form className="col-6 stack" id="ev-export-form" onSubmit={handleExportSubmit}>
              <fieldset className="field">
                <legend>Scope</legend>
                <div className="seg" role="radiogroup" aria-label="Export scope">
                  {[
                    ['session', 'Session'],
                    ['machine', 'Worker agent'],
                    ['range', 'Time range'],
                  ].map(([val, lbl]) => (
                    <label key={val}>
                      <input
                        type="radio"
                        name="scope"
                        value={val}
                        checked={exportScope === val}
                        onChange={() => setExportScope(val)}
                      />
                      <span>{lbl}</span>
                    </label>
                  ))}
                </div>
              </fieldset>

              <div className="field" data-scope="session" hidden={exportScope !== 'session'}>
                <label htmlFor="x-sess">Session</label>
                <select
                  className="select"
                  id="x-sess"
                  value={exportSession}
                  onChange={(e) => setExportSession(e.target.value)}
                >
                  {sessions.length === 0 ? (
                    <option value="">No sessions in the archive yet</option>
                  ) : (
                    sessions.map((s) => (
                      <option key={`${s.machine}/${s.session}`} value={`${s.machine}/${s.session}`}>
                        {`${s.session} · ${s.machine}${s.command ? ` · ${s.command}` : ''}`}
                      </option>
                    ))
                  )}
                </select>
              </div>

              <div
                className="field"
                data-scope="machine"
                hidden={exportScope !== 'machine' && exportScope !== 'range'}
              >
                <label htmlFor="x-mach">Worker agent</label>
                <select
                  className="select"
                  id="x-mach"
                  value={exportMachine}
                  onChange={(e) => setExportMachine(e.target.value)}
                >
                  <option value="">All worker agents</option>
                  {workers.map((w) => (
                    <option key={w.id} value={w.id}>
                      {w.id}
                    </option>
                  ))}
                </select>
              </div>

              <div
                className="form-grid"
                style={{ gridTemplateColumns: 'repeat(2,minmax(0,1fr))' }}
                data-scope="range"
                hidden={exportScope !== 'range'}
              >
                <div className="field">
                  <label htmlFor="x-from">From</label>
                  <input
                    className="input input--mono"
                    id="x-from"
                    type="datetime-local"
                    value={exportFrom}
                    onChange={(e) => setExportFrom(e.target.value)}
                  />
                </div>
                <div className="field">
                  <label htmlFor="x-to">To</label>
                  <input
                    className="input input--mono"
                    id="x-to"
                    type="datetime-local"
                    value={exportTo}
                    onChange={(e) => setExportTo(e.target.value)}
                  />
                </div>
              </div>

              <fieldset className="field">
                <legend>Format</legend>
                <div className="seg" role="radiogroup" aria-label="Export format">
                  {[
                    ['pcapng', 'pcapng'],
                    ['csv', 'CSV'],
                    ['json', 'JSON'],
                  ].map(([val, lbl]) => (
                    <label key={val}>
                      <input
                        type="radio"
                        name="fmt"
                        value={val}
                        checked={exportFormat === val}
                        onChange={() => setExportFormat(val)}
                      />
                      <span>{lbl}</span>
                    </label>
                  ))}
                </div>
                <span className="field__hint">
                  pcapng is regenerated from the packet records; trimmed payloads are reconstructed as far as stored, and
                  the record&apos;s SHA-256 vouches for the original.
                </span>
              </fieldset>

              <div className="row">
                <button className="btn btn--primary" type="submit" disabled={noBackend}>
                  <svg className="icon" aria-hidden="true">
                    <use href="#i-download" />
                  </svg>
                  Export
                </button>
              </div>
            </form>

            <section className="card col-6" aria-labelledby="rx-title">
              <div className="card__head">
                <h2 id="rx-title">Exports from this page</h2>
              </div>
              <ul className="list" id="ev-exports" data-has={exportsList.length > 0 ? '1' : undefined}>
                {exportsList.length === 0 ? (
                  <li className="list__item">
                    <div className="list__main">
                      <small>
                        Nothing exported yet. An export is generated from the archive when you ask and downloads
                        straight to your browser; the server keeps no copy.
                      </small>
                    </div>
                  </li>
                ) : (
                  exportsList.map((ex) => (
                    <li key={ex.id} className="list__item">
                      <div className="list__main">
                        <b className="mono">{ex.filename}</b>
                        <small>{`requested ${fmtDateTime(ex.requestedAt)}`}</small>
                      </div>
                      <a className="btn btn--sm" href={ex.url} download="">
                        <svg className="icon icon--sm" aria-hidden="true">
                          <use href="#i-download" />
                        </svg>
                        Download again
                      </a>
                    </li>
                  ))
                )}
              </ul>
            </section>
          </div>
        </div>
      </div>
    </>
  );
}
