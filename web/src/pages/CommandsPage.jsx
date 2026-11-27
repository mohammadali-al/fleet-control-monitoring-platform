import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { useMachines } from '../hooks/useMachines';
import { api } from '../lib/api';
import { useEvents } from '../lib/events';
import { fmtDur, fmtTime } from '../lib/fmt';
import { usePageTitle } from '../lib/title';
import { useToast } from '../lib/toast';
import { isRoutePorted, routeHref } from '../routes';

const MAX_OUTPUT_SEGMENTS = 400;

function b64bytes(s) {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function StateChip({ state }) {
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

function ExitChip({ exit }) {
  const x = exit || { reason: 'error', exit_code: -1 };
  const code = x.exit_code;
  switch (x.reason) {
    case 'exited':
      return code === 0 ? (
        <span className="chip chip--ok">
          <svg className="icon icon--sm" aria-hidden="true"><use href="#i-check" /></svg>
          Exited 0
        </span>
      ) : (
        <span className="chip chip--danger">
          <svg className="icon icon--sm" aria-hidden="true"><use href="#i-x" /></svg>
          {`Exited ${code}`}
        </span>
      );
    case 'timeout':
      return (
        <span className="chip chip--warn">
          <svg className="icon icon--sm" aria-hidden="true"><use href="#i-clock" /></svg>
          Timed out
        </span>
      );
    case 'terminated':
      return (
        <span className="chip chip--warn">
          <svg className="icon icon--sm" aria-hidden="true"><use href="#i-stop" /></svg>
          Terminated
        </span>
      );
    case 'refused':
      return (
        <span className="chip chip--danger">
          <svg className="icon icon--sm" aria-hidden="true"><use href="#i-x" /></svg>
          Refused (no confirmation)
        </span>
      );
    case 'cancelled':
      return <span className="chip chip--outline">Cancelled</span>;
    case 'lost':
      return (
        <span className="chip chip--danger">
          <svg className="icon icon--sm" aria-hidden="true"><use href="#i-x" /></svg>
          Connection lost
        </span>
      );
    default:
      return (
        <span className="chip chip--danger">
          <svg className="icon icon--sm" aria-hidden="true"><use href="#i-x" /></svg>
          Error
        </span>
      );
  }
}

function RunChip({ run }) {
  if (run.state === 'queued') {
    return (
      <span className="chip chip--warn">
        <svg className="icon icon--sm" aria-hidden="true"><use href="#i-clock" /></svg>
        {`Queued${run.position ? ` #${run.position}` : ''}`}
      </span>
    );
  }
  if (run.state === 'running') {
    return <span className="chip chip--accent chip--live">Running</span>;
  }
  return <ExitChip exit={run.exit} />;
}

function stepClassName(stepName, state, badExit) {
  const order = ['queued', 'running', 'exited'];
  const current = state === 'finished' ? 'exited' : state;
  const at = order.indexOf(current);
  const idx = order.indexOf(stepName);
  if (idx < at) return 'step step--done';
  if (idx === at) {
    if (current === 'exited') return badExit ? 'step step--bad' : 'step step--done';
    return 'step step--now';
  }
  return 'step';
}

function RunCard({ run, onCancel }) {
  const bodyRef = useRef(null);
  const stickBottomRef = useRef(true);

  const handleScroll = () => {
    const el = bodyRef.current;
    if (!el) return;
    stickBottomRef.current = el.scrollTop + el.clientHeight >= el.scrollHeight - 24;
  };

  useEffect(() => {
    const el = bodyRef.current;
    if (!el || !stickBottomRef.current) return;
    el.scrollTop = el.scrollHeight;
  }, [run.segments]);

  const exit = run.exit || { reason: 'error', exit_code: -1 };
  const badExit = !(exit.reason === 'exited' && exit.exit_code === 0);

  let metaText = '';
  if (run.state === 'queued') {
    metaText = run.position ? `position ${run.position} in the queue` : 'waiting for the machine';
  } else if (run.state === 'running') {
    metaText = `started ${fmtTime(run.started_at)}`;
  } else {
    metaText = `${fmtDur(exit.duration_ms)}${exit.error ? ` · ${exit.error}` : ''}`;
  }

  return (
    <article className="run" data-run data-session={run.session}>
      <div className="run__head">
        <b className="mono" data-run-machine>{run.machine}</b>
        <span data-run-chip>
          <RunChip run={run} />
        </span>
      </div>
      <div className="steps" aria-label="Run lifecycle">
        <span className={stepClassName('queued', run.state, badExit)} data-step="queued">Queued</span>
        <span className="steps__sep" />
        <span className={stepClassName('running', run.state, badExit)} data-step="running">Running</span>
        <span className="steps__sep" />
        <span className={stepClassName('exited', run.state, badExit)} data-step="exited">Exited</span>
      </div>
      <div className="term-ph" role="log" data-run-out aria-label={`Output of ${run.machine}`}>
        <div className="term-ph__body" ref={bodyRef} onScroll={handleScroll}>
          {run.segments.map((seg) => (
            <span
              key={seg.id}
              className={seg.stream === 'stderr' ? 'out-err' : seg.stream === 'note' ? 'term-dim' : undefined}
            >
              {seg.text}
            </span>
          ))}
        </div>
      </div>
      <div className="row row--between">
        <span className="muted" data-run-meta>{metaText}</span>
        {run.state === 'queued' || run.state === 'running' ? (
          <button
            className={`btn btn--sm${run.state === 'running' ? ' btn--danger' : ''}`}
            type="button"
            data-run-cancel
            onClick={() => onCancel(run.session)}
          >
            {run.state === 'queued' ? 'Cancel' : 'Terminate'}
          </button>
        ) : (
          <button className="btn btn--sm btn--danger" type="button" data-run-cancel hidden>
            Terminate
          </button>
        )}
      </div>
    </article>
  );
}

export function CommandsPage() {
  usePageTitle('Command runner');
  const pushToast = useToast();
  const { machines, noBackend, refresh: refreshMachines } = useMachines();

  const workers = useMemo(
    () => machines.filter((m) => m.role === 'worker' && !m.removed),
    [machines]
  );

  // Target selection state: default all online workers on first load, preserve user selection across refreshes.
  const [selectedIds, setSelectedIds] = useState(() => new Set());
  const initializedTargetsRef = useRef(false);

  useEffect(() => {
    if (workers.length === 0) return;
    const onlineSet = new Set(workers.filter((w) => w.state === 'online').map((w) => w.id));
    if (!initializedTargetsRef.current) {
      initializedTargetsRef.current = true;
      setSelectedIds(onlineSet);
      return;
    }
    setSelectedIds((prev) => {
      const next = new Set();
      prev.forEach((id) => {
        if (onlineSet.has(id)) next.add(id);
      });
      return next;
    });
  }, [workers]);

  // Form state
  const [command, setCommand] = useState('');
  const [timeoutSec, setTimeoutSec] = useState('300');
  const [submitting, setSubmitting] = useState(false);
  const [formStatus, setFormStatus] = useState(null); // { kind: 'info'|'ok'|'danger', msg: string }
  const cmdInputRef = useRef(null);

  // Dangerous-command confirmation gate dialog
  const [pendingGate, setPendingGate] = useState(null); // { body, pattern, command, machines }
  const [gatePhrase, setGatePhrase] = useState('');
  const gateDialogRef = useRef(null);
  const gatePhraseRef = useRef(null);

  useEffect(() => {
    const dlg = gateDialogRef.current;
    if (!dlg) return;
    if (pendingGate && !dlg.open) {
      dlg.showModal();
      window.setTimeout(() => gatePhraseRef.current?.focus(), 0);
    } else if (!pendingGate && dlg.open) {
      dlg.close();
    }
  }, [pendingGate]);

  // Live runs (ordered newest-first) and history
  const [runs, setRuns] = useState([]); // [{ session, machine, command, state, position, started_at, exit, segments }]
  const [history, setHistory] = useState([]);
  const [historyError, setHistoryError] = useState('');
  const [historyLoaded, setHistoryLoaded] = useState(false);
  const [historyFilter, setHistoryFilter] = useState('');

  const decodersRef = useRef(new Map());
  const nextSegIdRef = useRef(1);
  const knownSessionsRef = useRef(new Set());
  const historyDebounceRef = useRef(0);
  const machinesDebounceRef = useRef(0);

  const loadOutput = useCallback(async (session) => {
    const res = await api('GET', `/api/commands/${encodeURIComponent(session)}`);
    if (!res.ok) return;
    const output = Array.isArray(res.data.output) ? res.data.output : [];
    const loadedSegs = output.map((seg) => ({
      id: nextSegIdRef.current++,
      stream: seg.stream,
      text: seg.text,
    }));
    setRuns((prev) =>
      prev.map((r) => (r.session === session ? { ...r, segments: loadedSegs.slice(-MAX_OUTPUT_SEGMENTS) } : r))
    );
  }, []);

  const refreshHistory = useCallback(async () => {
    const res = await api('GET', '/api/commands?mode=oneshot');
    setHistoryLoaded(true);
    if (!res.ok) {
      setHistoryError(res.data.error || 'History is unavailable.');
      return;
    }
    setHistoryError('');
    const list = Array.isArray(res.data.commands) ? res.data.commands : [];
    setHistory(list);

    const sessionsToLoad = [];
    for (const c of list.slice().reverse()) {
      if (!knownSessionsRef.current.has(c.session) && c.state !== 'finished') {
        knownSessionsRef.current.add(c.session);
        sessionsToLoad.push(c.session);
      }
    }

    setRuns((prev) => {
      const bySession = new Map(prev.map((r) => [r.session, r]));
      const next = [...prev];

      for (const c of list.slice().reverse()) {
        const existing = bySession.get(c.session);
        if (!existing && c.state === 'finished') continue;
        if (existing) {
          const updated = {
            ...existing,
            state: c.state,
            position: c.position,
            started_at: c.started_at || existing.started_at,
            exit: c.exit || existing.exit,
          };
          const idx = next.findIndex((r) => r.session === c.session);
          if (idx !== -1) next[idx] = updated;
          bySession.set(c.session, updated);
        } else {
          const created = {
            session: c.session,
            machine: c.machine,
            command: c.command,
            state: c.state,
            position: c.position,
            started_at: c.started_at,
            exit: c.exit,
            segments: [],
          };
          next.unshift(created);
          bySession.set(c.session, created);
        }
      }
      return next;
    });

    sessionsToLoad.forEach((sid) => {
      loadOutput(sid);
    });
  }, [loadOutput]);

  const refreshHistorySoon = useCallback(() => {
    if (historyDebounceRef.current) window.clearTimeout(historyDebounceRef.current);
    historyDebounceRef.current = window.setTimeout(() => {
      historyDebounceRef.current = 0;
      refreshHistory();
    }, 120);
  }, [refreshHistory]);

  const refreshMachinesSoon = useCallback(() => {
    if (machinesDebounceRef.current) window.clearTimeout(machinesDebounceRef.current);
    machinesDebounceRef.current = window.setTimeout(() => {
      machinesDebounceRef.current = 0;
      refreshMachines();
    }, 200);
  }, [refreshMachines]);

  useEffect(() => {
    refreshHistory();
    return () => {
      if (historyDebounceRef.current) window.clearTimeout(historyDebounceRef.current);
      if (machinesDebounceRef.current) window.clearTimeout(machinesDebounceRef.current);
    };
  }, [refreshHistory]);

  // Subscribe to /ws/events for live command lifecycle & output chunks.
  useEvents(
    (m) => {
      if (!m) return;
      if (m.type === 'chunk') {
        const key = `${m.session}:${m.stream}`;
        let dec = decodersRef.current.get(key);
        if (!dec) {
          dec = new TextDecoder('utf-8');
          decodersRef.current.set(key, dec);
        }
        let text = '';
        try {
          text = dec.decode(b64bytes(m.data || ''), { stream: true });
        } catch {
          return;
        }
        if (!text) return;
        setRuns((prev) =>
          prev.map((r) => {
            if (r.session !== m.session) return r;
            const nextSegs = [...r.segments, { id: nextSegIdRef.current++, stream: m.stream, text }];
            return {
              ...r,
              segments: nextSegs.length > MAX_OUTPUT_SEGMENTS ? nextSegs.slice(-MAX_OUTPUT_SEGMENTS) : nextSegs,
            };
          })
        );
        return;
      }
      if (m.type === 'command' && m.mode === 'oneshot') {
        if (m.event === 'queued' && !knownSessionsRef.current.has(m.session)) {
          knownSessionsRef.current.add(m.session);
          setRuns((prev) => {
            if (prev.some((r) => r.session === m.session)) return prev;
            return [
              {
                session: m.session,
                machine: m.on,
                command: m.command,
                state: 'queued',
                position: 0,
                started_at: null,
                exit: null,
                segments: [],
              },
              ...prev,
            ];
          });
          loadOutput(m.session);
        } else if (m.event === 'running') {
          setRuns((prev) =>
            prev.map((r) => (r.session === m.session ? { ...r, state: 'running', started_at: m.started_at } : r))
          );
        } else if (m.event === 'exited') {
          setRuns((prev) =>
            prev.map((r) =>
              r.session === m.session ? { ...r, state: 'finished', exit: m.exit || r.exit } : r
            )
          );
        }
        refreshHistorySoon();
        refreshMachinesSoon();
      }
    },
    () => {
      refreshHistory();
      refreshMachines();
    }
  );

  const toggleTarget = (id, checked) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  };

  const selectAllOnline = () => {
    setSelectedIds(new Set(workers.filter((w) => w.state === 'online').map((w) => w.id)));
  };

  const clearSelection = () => {
    setSelectedIds(new Set());
  };

  const submitCommand = async (body, confirm) => {
    const payload = { ...body, confirm: Boolean(confirm) };
    setSubmitting(true);
    const r = await api('POST', '/api/commands', payload);
    setSubmitting(false);

    if (r.status === 409 && r.data.needs_confirmation) {
      setGatePhrase('');
      setPendingGate({
        body,
        pattern: r.data.pattern,
        command: r.data.command,
        machines: Array.isArray(r.data.machines) ? r.data.machines : payload.machines,
      });
      setFormStatus(null);
      return;
    }

    if (!r.ok && !r.data.results) {
      setFormStatus({ kind: 'danger', msg: r.data.error || `Failed (${r.status})` });
      return;
    }

    const errors = [];
    const results = Array.isArray(r.data.results) ? r.data.results : [];
    const newCards = [];
    for (const res of results) {
      if (res.error) {
        errors.push(`${res.machine}: ${res.error}`);
        continue;
      }
      newCards.push({
        session: res.session,
        machine: res.machine,
        command: payload.command,
        state: 'queued',
        position: 0,
        started_at: null,
        exit: null,
        segments: [],
      });
    }
    if (newCards.length > 0) {
      newCards.forEach((c) => knownSessionsRef.current.add(c.session));
      setRuns((prev) => {
        const known = new Set(prev.map((x) => x.session));
        const additions = newCards.filter((c) => !known.has(c.session)).reverse();
        return [...additions, ...prev];
      });
    }
    setFormStatus(errors.length ? { kind: 'danger', msg: errors.join(' | ') } : null);
    refreshHistorySoon();
  };

  const handleFormSubmit = (e) => {
    e.preventDefault();
    setFormStatus(null);
    const targetList = workers.filter((w) => w.state === 'online' && selectedIds.has(w.id)).map((w) => w.id);
    submitCommand(
      {
        machines: targetList,
        command,
        timeout_sec: parseInt(timeoutSec, 10) || 0,
      },
      false
    );
  };

  const handleGateSubmit = (e) => {
    e.preventDefault();
    if (gatePhrase.trim() !== 'I understand' || !pendingGate) return;
    const { body } = pendingGate;
    setPendingGate(null);
    setCommand(body.command);
    submitCommand(body, true);
  };

  const handleCancelRun = async (session) => {
    const res = await api('POST', `/api/commands/${encodeURIComponent(session)}/cancel`, {});
    if (!res.ok) pushToast(res.data.error || 'Could not cancel.');
  };

  const handleRerun = (cmdEntry) => {
    setCommand(cmdEntry.command);
    cmdInputRef.current?.focus();
    const target = workers.find((w) => w.id === cmdEntry.machine && w.state === 'online');
    if (target) {
      setSelectedIds(new Set([target.id]));
    }
  };

  const selectedCount = useMemo(
    () => workers.filter((w) => w.state === 'online' && selectedIds.has(w.id)).length,
    [workers, selectedIds]
  );

  const activeRuns = useMemo(() => history.filter((c) => c.state !== 'finished'), [history]);
  const runningCount = useMemo(() => activeRuns.filter((c) => c.state === 'running').length, [activeRuns]);
  const newestHistory = history[0] || null;

  const filteredHistory = useMemo(() => {
    const q = historyFilter.trim().toLowerCase();
    if (!q) return history;
    return history.filter((c) => `${c.command} ${c.machine} ${c.session}`.toLowerCase().includes(q));
  }, [history, historyFilter]);

  const termHref = routeHref('terminal');
  const machinesHref = routeHref('machines');
  const evidenceHref = routeHref('evidence');

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Command runner</h1>
          <p>
            Mode A: run a one-shot command on one or more worker agents and watch the output live. Manual commands run
            strictly one at a time per worker agent; extra runs queue.
          </p>
        </div>
        <div className="page-head__actions">
          {isRoutePorted('terminal') ? (
            <Link className="btn" to={termHref}>
              <svg className="icon" aria-hidden="true"><use href="#i-terminal" /></svg>
              Interactive shell (Mode B)
            </Link>
          ) : (
            <a className="btn" href={termHref}>
              <svg className="icon" aria-hidden="true"><use href="#i-terminal" /></svg>
              Interactive shell (Mode B)
            </a>
          )}
        </div>
      </div>

      {noBackend && (
        <div className="alert alert--warn" role="status" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
          <div>The panel is running without a backend, so commands cannot be dispatched.</div>
        </div>
      )}

      <div className="grid">
        {/* New run */}
        <section className="card col-4" aria-labelledby="run-title">
          <div className="card__head">
            <h2 id="run-title">New run</h2>
          </div>
          <form className="card__body stack" id="run-form" data-live-run onSubmit={handleFormSubmit}>
            <fieldset className="field">
              <legend>Target worker agents</legend>
              <div className="row" style={{ marginBottom: 'var(--sp-1)' }}>
                <button className="btn btn--sm" type="button" data-select="online" onClick={selectAllOnline}>
                  Select all online
                </button>
                <button className="btn btn--sm" type="button" data-select="none" onClick={clearSelection}>
                  Clear
                </button>
              </div>
              <ul className="list list--boxed" id="targets">
                {workers.length === 0 ? (
                  <li className="list__item">
                    <div className="list__main">
                      <small>
                        No worker agents are registered. Onboard one on the{' '}
                        {isRoutePorted('machines') ? (
                          <Link to={machinesHref}>Machines</Link>
                        ) : (
                          <a href={machinesHref}>Machines</a>
                        )}{' '}
                        page.
                      </small>
                    </div>
                  </li>
                ) : (
                  workers.map((w) => {
                    const online = w.state === 'online';
                    const checked = online && selectedIds.has(w.id);
                    const sub = !online
                      ? `unavailable while ${w.state}`
                      : w.running || w.queued
                        ? `${w.running} running, ${w.queued} queued`
                        : 'idle';
                    return (
                      <li key={w.id} className="list__item">
                        <label className="check">
                          <input
                            type="checkbox"
                            value={w.id}
                            disabled={!online}
                            checked={checked}
                            aria-label={online ? w.id : `${w.id} (${w.state})`}
                            onChange={(e) => toggleTarget(w.id, e.target.checked)}
                          />
                        </label>
                        <div className="list__main">
                          <b className="mono">{w.id}</b>
                          <small>{sub}</small>
                        </div>
                        <StateChip state={w.state} />
                      </li>
                    );
                  })
                )}
              </ul>
              <span className="field__hint">Monitor agents never appear here: they have no command role.</span>
            </fieldset>

            <div className="field">
              <label htmlFor="cmd">Command</label>
              <textarea
                ref={cmdInputRef}
                className="textarea input--mono"
                id="cmd"
                name="command"
                rows={3}
                spellCheck={false}
                placeholder="uname -a && uptime"
                required
                value={command}
                onChange={(e) => setCommand(e.target.value)}
              />
              <span className="field__hint">
                Runs as <code>root</code> on each selected worker agent.
              </span>
            </div>

            <div className="field">
              <label htmlFor="to">Timeout (seconds)</label>
              <input
                className="input input--mono"
                id="to"
                name="timeout_sec"
                inputMode="numeric"
                aria-describedby="to-h"
                value={timeoutSec}
                onChange={(e) => setTimeoutSec(e.target.value)}
              />
              <span className="field__hint" id="to-h">
                Default 300 s. A hung command ends at the timeout and the queue advances.
              </span>
            </div>

            <div className="alert">
              <svg className="icon" aria-hidden="true"><use href="#i-info" /></svg>
              <div>
                Traffic produced by this command is captured as <strong>evidence</strong> under its session id.
              </div>
            </div>

            {formStatus && formStatus.msg ? (
              <div
                className={`alert${formStatus.kind === 'info' ? '' : ` alert--${formStatus.kind}`}`}
                data-form-status
                role="status"
                aria-live="polite"
              >
                <svg className="icon" aria-hidden="true">
                  <use href={`#${formStatus.kind === 'ok' ? 'i-check' : formStatus.kind === 'danger' ? 'i-alert' : 'i-info'}`} />
                </svg>
                <div>{formStatus.msg}</div>
              </div>
            ) : (
              <div className="alert alert--danger" data-form-status role="status" aria-live="polite" hidden />
            )}

            <div className="row">
              <button
                className="btn btn--primary"
                type="submit"
                data-run-submit
                disabled={submitting || selectedCount === 0}
              >
                <svg className="icon" aria-hidden="true"><use href="#i-play" /></svg>
                <span data-run-label>
                  {`Run on ${selectedCount} worker agent${selectedCount === 1 ? '' : 's'}`}
                </span>
              </button>
            </div>
          </form>
        </section>

        {/* Live view & History */}
        <div className="col-8 stack">
          <section className="card" aria-labelledby="live-title">
            <div className="card__head">
              <div>
                <h2 id="live-title">Live runs</h2>
                <p className="mono" data-live-command>
                  {newestHistory ? newestHistory.command : 'Nothing running'}
                </p>
              </div>
              <span
                className={`chip chip--${activeRuns.length ? 'accent chip--live' : 'outline'}`}
                data-live-chip
              >
                {activeRuns.length
                  ? `${runningCount} running, ${activeRuns.length - runningCount} queued`
                  : 'Idle'}
              </span>
            </div>
            <div className="card__body stack">
              <div className="runs" id="runs" aria-label="Per worker agent" aria-live="off">
                {runs.map((r) => (
                  <RunCard key={r.session} run={r} onCancel={handleCancelRun} />
                ))}
              </div>
              <p className="empty" id="runs-empty" hidden={runs.length > 0}>
                Submitted commands appear here, one card per worker agent, with their output streamed live.
              </p>
            </div>
            <div className="card__foot">
              Output is streamed from the agent as it is produced. Evidence (packets, drops) arrives with the evidence
              pipeline; open the session in{' '}
              {isRoutePorted('evidence') ? (
                <Link to={evidenceHref}>Evidence</Link>
              ) : (
                <a href={evidenceHref}>Evidence</a>
              )}{' '}
              for the records.
            </div>
          </section>

          <section className="card" aria-labelledby="hist-title">
            <div className="card__head">
              <h2 id="hist-title">History</h2>
              <div className="input-affix" style={{ minWidth: '14rem' }}>
                <svg className="icon" aria-hidden="true"><use href="#i-search" /></svg>
                <input
                  className="input"
                  type="search"
                  data-history-filter
                  placeholder="Filter commands"
                  aria-label="Filter command history"
                  value={historyFilter}
                  onChange={(e) => setHistoryFilter(e.target.value)}
                />
              </div>
            </div>
            <div className="table-wrap">
              <table className="table">
                <caption className="sr-only">Manual command history, including the serial queue</caption>
                <thead>
                  <tr>
                    <th scope="col">Session</th>
                    <th scope="col">Command</th>
                    <th scope="col">Machine</th>
                    <th scope="col">Queued</th>
                    <th scope="col" className="num">Duration</th>
                    <th scope="col">Result</th>
                    <th scope="col"><span className="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody id="history-rows">
                  {!historyLoaded ? (
                    <tr>
                      <td colSpan={7} className="empty">Loading history...</td>
                    </tr>
                  ) : historyError ? (
                    <tr>
                      <td colSpan={7} className="empty">{historyError}</td>
                    </tr>
                  ) : filteredHistory.length === 0 ? (
                    <tr>
                      <td colSpan={7} className="empty">
                        {history.length ? 'No commands match the filter.' : 'No commands yet.'}
                      </td>
                    </tr>
                  ) : (
                    filteredHistory.map((c) => (
                      <tr key={c.session}>
                        <td className="mono">{c.session}</td>
                        <td className="mono truncate">{c.command}</td>
                        <td className="nowrap mono">{c.machine}</td>
                        <td className="nowrap">{fmtTime(c.queued_at)}</td>
                        <td className="num">
                          {c.state === 'finished' && c.exit ? fmtDur(c.exit.duration_ms) : c.state}
                        </td>
                        <td>
                          {c.state === 'queued' ? (
                            <span className="chip chip--warn">
                              <svg className="icon icon--sm" aria-hidden="true"><use href="#i-clock" /></svg>
                              {`Queued${c.position ? ` #${c.position}` : ''}`}
                            </span>
                          ) : c.state === 'running' ? (
                            <span className="chip chip--accent chip--live">Running</span>
                          ) : (
                            <ExitChip exit={c.exit} />
                          )}
                        </td>
                        <td className="actions">
                          <button
                            className="btn btn--sm"
                            type="button"
                            onClick={() => handleRerun(c)}
                          >
                            <svg className="icon icon--sm" aria-hidden="true"><use href="#i-refresh" /></svg>
                            Re-run
                          </button>
                        </td>
                      </tr>
                    ))
                  )}
                </tbody>
              </table>
            </div>
            <div className="card__foot">
              Refusals are logged and shown here, like every other action. History is kept in memory by the main server
              and is lost on restart.
            </div>
          </section>
        </div>
      </div>

      {/* Dangerous-command gate */}
      <dialog
        ref={gateDialogRef}
        className="dialog"
        id="dlg-gate"
        aria-labelledby="dlg-gate-title"
        onClose={() => setPendingGate(null)}
      >
        <form id="gate-form" onSubmit={handleGateSubmit}>
          <div className="dialog__head">
            <h2 id="dlg-gate-title">Confirm a dangerous command</h2>
            <p>This command matches the server-side dangerous-pattern list.</p>
          </div>
          <div className="dialog__body">
            <div className="alert alert--danger">
              <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
              <div>
                <strong>Matched pattern:</strong> <code data-gate-pattern>{pendingGate?.pattern || ''}</code>
                <br />
                <strong>Command:</strong>{' '}
                <code data-gate-command>
                  {pendingGate ? `${pendingGate.command}  (on ${pendingGate.machines.join(', ')})` : ''}
                </code>
                <br />
                The attempt is logged.
              </div>
            </div>
            <div className="field">
              <label htmlFor="gate-phrase">
                Type <code>I understand</code> to continue
              </label>
              <input
                ref={gatePhraseRef}
                className="input input--mono"
                id="gate-phrase"
                autoComplete="off"
                value={gatePhrase}
                onChange={(e) => setGatePhrase(e.target.value)}
              />
              <span className="field__hint">
                The agent refuses the command unless it carries a confirmation token minted by the main server,
                whatever the browser does.
              </span>
            </div>
          </div>
          <div className="dialog__foot">
            <button
              className="btn"
              type="button"
              data-dialog-close
              onClick={() => setPendingGate(null)}
            >
              Cancel
            </button>
            <button
              className="btn btn--danger"
              type="submit"
              data-gate-confirm
              disabled={gatePhrase.trim() !== 'I understand'}
            >
              Run anyway
            </button>
          </div>
        </form>
      </dialog>
    </>
  );
}
