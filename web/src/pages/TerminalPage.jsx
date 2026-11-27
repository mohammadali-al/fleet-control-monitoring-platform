import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useMachines } from '../hooks/useMachines';
import { api } from '../lib/api';
import { useEvents } from '../lib/events';
import { fmtTime } from '../lib/fmt';
import { useTheme } from '../lib/theme';
import { usePageTitle } from '../lib/title';
import { useToast } from '../lib/toast';
import { ensureXterm } from '../lib/xterm';
import { isRoutePorted, routeHref } from '../routes';

function themeFromCss() {
  const cs = getComputedStyle(document.documentElement);
  const v = (name, fallback) => (cs.getPropertyValue(name) || '').trim() || fallback;
  const fg = v('--term-fg', '#d6deeb');
  return {
    background: v('--term-bg', '#0a0f1a'),
    foreground: fg,
    cursor: fg,
  };
}

function TermChip({ state }) {
  if (state === 'queued') {
    return (
      <span className="chip chip--warn">
        <svg className="icon icon--sm" aria-hidden="true"><use href="#i-clock" /></svg>
        Waiting for the machine
      </span>
    );
  }
  if (state === 'running') {
    return (
      <span className="chip chip--ok">
        <svg className="icon icon--sm" aria-hidden="true"><use href="#i-check" /></svg>
        Connected
      </span>
    );
  }
  if (state === 'detached') {
    return <span className="chip chip--outline">Detached</span>;
  }
  if (state === 'ended') {
    return <span className="chip chip--outline">Ended</span>;
  }
  return (
    <span className="chip chip--info">
      <svg className="icon icon--sm" aria-hidden="true"><use href="#i-info" /></svg>
      Opening...
    </span>
  );
}

function TerminalTabPanel({
  session,
  active,
  focusTick,
  theme,
  xtermApi,
  onUpdate,
  onRemove,
  onTerminate,
  onReattach,
  onRefusedLine,
  onError,
  onSessionsChanged,
}) {
  const mountRef = useRef(null);
  const termRef = useRef(null);
  const fitRef = useRef(null);
  const wsRef = useRef(null);
  const stateRef = useRef(session.state);
  stateRef.current = session.state;

  const callbacksRef = useRef({
    onUpdate,
    onRefusedLine,
    onError,
    onSessionsChanged,
  });
  callbacksRef.current = {
    onUpdate,
    onRefusedLine,
    onError,
    onSessionsChanged,
  };

  const pushToast = useToast();
  const [sizeText, setSizeText] = useState('-- x --');

  // Mount vendored xterm.js, wire /ws/shell, and tear down cleanly on close or route change.
  useEffect(() => {
    const mountEl = mountRef.current;
    const XTerm = xtermApi?.Terminal;
    const FitAddonCtor = xtermApi?.FitAddon;
    if (!mountEl || !XTerm) return undefined;

    let disposed = false;
    const enc = new TextEncoder();
    const fontFamily =
      getComputedStyle(document.documentElement).getPropertyValue('--font-mono') || 'monospace';

    const term = new XTerm({
      cursorBlink: true,
      fontFamily,
      fontSize: 13,
      theme: themeFromCss(),
      scrollback: 5000,
    });
    const fit = FitAddonCtor ? new FitAddonCtor() : null;
    if (fit) term.loadAddon(fit);

    termRef.current = term;
    fitRef.current = fit;

    term.open(mountEl);
    if (fit) {
      try {
        fit.fit();
      } catch {
        // panel may still be settling
      }
    }
    setSizeText(`${term.cols} x ${term.rows}`);

    const proto = window.location.protocol === 'https:' ? 'wss://' : 'ws://';
    const url = session.attachSessionId
      ? `${proto}${window.location.host}/ws/shell?session=${encodeURIComponent(session.attachSessionId)}${
          session.machine ? `&machine=${encodeURIComponent(session.machine)}` : ''
        }&cols=${term.cols}&rows=${term.rows}`
      : `${proto}${window.location.host}/ws/shell?machine=${encodeURIComponent(
          session.machine
        )}&cols=${term.cols}&rows=${term.rows}`;
    const ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';
    wsRef.current = ws;

    const sendConfirm = (line) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'confirm', line }));
      }
      term.focus();
    };

    const focusTerm = () => term.focus();

    ws.onmessage = (e) => {
      if (disposed) return;
      if (typeof e.data !== 'string') {
        term.write(new Uint8Array(e.data));
        return;
      }
      let m;
      try {
        m = JSON.parse(e.data);
      } catch {
        return;
      }
      switch (m.type) {
        case 'session':
          callbacksRef.current.onUpdate(session.key, {
            sessionId: m.session,
            machine: m.machine || session.machine,
          });
          callbacksRef.current.onSessionsChanged();
          break;
        case 'state':
          stateRef.current = m.state;
          callbacksRef.current.onUpdate(session.key, { state: m.state });
          if (m.state === 'queued') {
            term.write('\x1b[2m[fleet] waiting for the machine to accept the shell.\x1b[0m\r\n');
          } else if (m.state === 'running') {
            if (fit) {
              try {
                fit.fit();
              } catch {
                // ignore if hidden
              }
            }
            if (ws.readyState === WebSocket.OPEN) {
              ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
            }
            term.focus();
          }
          callbacksRef.current.onSessionsChanged();
          break;
        case 'refused':
          if (m.line) {
            callbacksRef.current.onRefusedLine({
              machine: session.machine,
              line: m.line,
              sendConfirm,
              focusTerm,
            });
          } else {
            pushToast(`The agent refused a line (${m.reason || 'dangerous'}). Retype it to try again.`);
          }
          break;
        case 'exit':
          stateRef.current = 'ended';
          callbacksRef.current.onUpdate(session.key, { state: 'ended' });
          term.write(
            `\r\n\x1b[2m[fleet] session ended (${m.reason || 'exited'}${
              m.code != null ? `, code ${m.code}` : ''
            })\x1b[0m\r\n`
          );
          break;
        case 'error':
          term.write(`\r\n\x1b[31m[fleet] ${m.error || 'error'}\x1b[0m\r\n`);
          callbacksRef.current.onError(m.error || 'The shell could not be opened.');
          break;
      }
    };

    ws.onclose = () => {
      if (disposed) return;
      if (stateRef.current !== 'ended') {
        stateRef.current = 'detached';
        callbacksRef.current.onUpdate(session.key, { state: 'detached' });
        term.write('\r\n\x1b[2m[fleet] detached (shell remains active on server)\x1b[0m\r\n');
      }
      callbacksRef.current.onSessionsChanged();
    };

    const dataSub = term.onData((d) => {
      if (ws.readyState === WebSocket.OPEN && stateRef.current === 'running') {
        ws.send(enc.encode(d));
      }
    });

    const resizeSub = term.onResize((sz) => {
      setSizeText(`${sz.cols} x ${sz.rows}`);
      if (ws.readyState === WebSocket.OPEN && stateRef.current === 'running') {
        ws.send(JSON.stringify({ type: 'resize', cols: sz.cols, rows: sz.rows }));
      }
    });

    let resizeTimer = 0;
    const handleWindowResize = () => {
      if (resizeTimer) window.clearTimeout(resizeTimer);
      resizeTimer = window.setTimeout(() => {
        resizeTimer = 0;
        if (disposed || !fit || !mountEl.offsetParent) return;
        try {
          fit.fit();
        } catch {
          // ignore hidden panel
        }
      }, 150);
    };
    window.addEventListener('resize', handleWindowResize);

    return () => {
      disposed = true;
      if (resizeTimer) window.clearTimeout(resizeTimer);
      window.removeEventListener('resize', handleWindowResize);
      dataSub.dispose();
      resizeSub.dispose();
      try {
        ws.close();
      } catch {
        // already closed
      }
      term.dispose();
      termRef.current = null;
      fitRef.current = null;
      wsRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [session.key, session.machine, session.attachSessionId, xtermApi]);

  // Refit (and optionally focus) when switching tabs.
  useEffect(() => {
    if (!active) return;
    if (fitRef.current) {
      try {
        fitRef.current.fit();
      } catch {
        // ignore if layout not ready
      }
    }
    if (focusTick > 0) {
      termRef.current?.focus();
    }
  }, [active, focusTick]);

  // Sync xterm theme when the SPA shell toggles dark/light mode.
  useEffect(() => {
    const raf = window.requestAnimationFrame(() => {
      if (termRef.current) {
        termRef.current.options.theme = themeFromCss();
      }
    });
    return () => window.cancelAnimationFrame(raf);
  }, [theme]);

  const handleCopy = () => {
    const t = termRef.current?.getSelection() || '';
    if (!t) {
      pushToast('Select some terminal text first.');
      return;
    }
    if (navigator.clipboard) {
      navigator.clipboard.writeText(t).then(
        () => pushToast('Copied.'),
        () => pushToast('Copy is blocked by the browser here; use Ctrl-Shift-C.')
      );
    } else {
      pushToast('Copy is blocked by the browser here; use Ctrl-Shift-C.');
    }
  };

  const handleDetachOrClose = () => {
    onRemove(session.key);
  };

  const titleText = session.sessionId
    ? `${session.machine} · session ${session.sessionId}`
    : `${session.machine} · opening`;

  const isLive = session.state === 'opening' || session.state === 'queued' || session.state === 'running';

  return (
    <div
      id={`pnl-${session.key}`}
      className="tabpanel"
      role="tabpanel"
      aria-labelledby={`tab-${session.key}`}
      data-term-panel
      style={{ paddingTop: 0 }}
      hidden={!active}
    >
      <div className="row row--between" style={{ marginBottom: 'var(--sp-3)' }}>
        <div className="row">
          <span data-term-chip>
            <TermChip state={session.state} />
          </span>
          <span className="chip chip--outline chip--mono" data-term-size>
            {sizeText}
          </span>
        </div>
        <div className="row">
          <button className="btn btn--sm" type="button" data-term-copy onClick={handleCopy}>
            <svg className="icon icon--sm" aria-hidden="true"><use href="#i-copy" /></svg>
            Copy
          </button>
          {session.state === 'detached' && session.sessionId && (
            <button
              className="btn btn--sm"
              type="button"
              data-term-reattach
              onClick={() => onReattach({ session: session.sessionId, machine: session.machine }, session.key)}
            >
              <svg className="icon icon--sm" aria-hidden="true"><use href="#i-refresh" /></svg>
              Reattach
            </button>
          )}
          {session.state !== 'ended' && session.sessionId && (
            <button
              className="btn btn--sm btn--danger"
              type="button"
              data-term-terminate
              onClick={() => onTerminate(session.sessionId)}
            >
              <svg className="icon icon--sm" aria-hidden="true"><use href="#i-stop" /></svg>
              Terminate
            </button>
          )}
          <button
            className="btn btn--sm"
            type="button"
            data-term-stop
            onClick={handleDetachOrClose}
          >
            <svg className="icon icon--sm" aria-hidden="true"><use href="#i-x" /></svg>
            {isLive ? 'Detach' : 'Close tab'}
          </button>
        </div>
      </div>
      <div className="term-ph term-ph--tall">
        <div className="term-ph__bar">
          <span data-term-title>{titleText}</span>
          <span className="term-ph__rec">EVIDENCE RECORDING</span>
        </div>
        <div ref={mountRef} data-term-mount />
      </div>
    </div>
  );
}

export function TerminalPage() {
  usePageTitle('Terminal');
  const { theme } = useTheme();
  const pushToast = useToast();
  const [searchParams, setSearchParams] = useSearchParams();
  const { machines, noBackend, loading: machinesLoading, refresh: refreshMachines } = useMachines();

  const [xtermApi, setXtermApi] = useState(null);
  const [formError, setFormError] = useState('');

  useEffect(() => {
    let cancelled = false;
    ensureXterm()
      .then((apiObj) => {
        if (!cancelled) setXtermApi(apiObj);
      })
      .catch(() => {
        if (!cancelled) setFormError('xterm.js did not load; terminals are unavailable.');
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const workers = useMemo(
    () => machines.filter((m) => m.role === 'worker' && !m.removed),
    [machines]
  );

  // Host selector state
  const [selectedMachine, setSelectedMachine] = useState('');

  useEffect(() => {
    if (workers.length === 0) {
      setSelectedMachine('');
      return;
    }
    const current = workers.find((w) => w.id === selectedMachine && w.state === 'online');
    if (current) return;
    const firstOnline = workers.find((w) => w.state === 'online');
    setSelectedMachine(firstOnline ? firstOnline.id : '');
  }, [workers, selectedMachine]);

  // Open client terminal tabs
  const [tabs, setTabs] = useState([]); // [{ key, machine, sessionId, state }]
  const [activeKey, setActiveKey] = useState(null);
  const [focusTick, setFocusTick] = useState(0);
  const seqRef = useRef(0);
  const tabRefs = useRef(new Map());
  const tabsRef = useRef(tabs);
  tabsRef.current = tabs;
  const activeKeyRef = useRef(activeKey);
  activeKeyRef.current = activeKey;

  // Active PTY sessions list (server-wide)
  const [activeSessions, setActiveSessions] = useState([]);
  const sessionsDebounceRef = useRef(0);

  const refreshSessions = useCallback(async () => {
    const res = await api('GET', '/api/commands?mode=pty&active=1');
    setActiveSessions(res.ok && Array.isArray(res.data.commands) ? res.data.commands : []);
  }, []);

  const refreshSessionsSoon = useCallback(() => {
    if (sessionsDebounceRef.current) window.clearTimeout(sessionsDebounceRef.current);
    sessionsDebounceRef.current = window.setTimeout(() => {
      sessionsDebounceRef.current = 0;
      refreshSessions();
    }, 150);
  }, [refreshSessions]);

  useEffect(() => {
    refreshSessions();
    return () => {
      if (sessionsDebounceRef.current) window.clearTimeout(sessionsDebounceRef.current);
    };
  }, [refreshSessions]);

  useEvents(
    (m) => {
      if (!m) return;
      if (m.type === 'command' && m.mode === 'pty') {
        refreshSessionsSoon();
      }
    },
    () => {
      refreshMachines();
      refreshSessions();
    }
  );

  // Dangerous-line confirmation dialog (#dlg-tgate)
  const [tgate, setTgate] = useState(null); // { machine, line, sendConfirm, focusTerm }
  const [tgatePhrase, setTgatePhrase] = useState('');
  const tgateDialogRef = useRef(null);
  const tgatePhraseRef = useRef(null);

  useEffect(() => {
    const dlg = tgateDialogRef.current;
    if (!dlg) return;
    if (tgate && !dlg.open) {
      dlg.showModal();
      window.setTimeout(() => tgatePhraseRef.current?.focus(), 0);
    } else if (!tgate && dlg.open) {
      dlg.close();
    }
  }, [tgate]);

  const openShell = useCallback(
    (machine) => {
      if (!xtermApi) return;
      setFormError('');
      const key = `k${++seqRef.current}`;
      setTabs((prev) => [...prev, { key, machine, sessionId: '', attachSessionId: '', state: 'opening' }]);
      setActiveKey(key);
      setFocusTick(0);
    },
    [xtermApi]
  );

  const reattachShell = useCallback(
    (sess, replaceKey = null) => {
      if (!xtermApi || !sess?.session) return;
      setFormError('');
      if (!replaceKey) {
        const existing = tabsRef.current.find(
          (t) =>
            t.sessionId === sess.session &&
            (t.state === 'opening' || t.state === 'queued' || t.state === 'running')
        );
        if (existing) {
          setActiveKey(existing.key);
          setFocusTick((n) => n + 1);
          return;
        }
      }
      const key = `k${++seqRef.current}`;
      const nextTab = {
        key,
        machine: sess.machine || '',
        sessionId: sess.session,
        attachSessionId: sess.session,
        state: 'opening',
      };
      setTabs((prev) => [
        ...prev.filter((t) => t.key !== replaceKey && !(t.sessionId === sess.session && t.state === 'detached')),
        nextTab,
      ]);
      setActiveKey(key);
      setFocusTick(0);
    },
    [xtermApi]
  );

  // Support deep-link /terminal?machine=<id> from MachinesPage
  const consumedMachineParamRef = useRef('');
  useEffect(() => {
    const wanted = searchParams.get('machine');
    if (!wanted) {
      consumedMachineParamRef.current = '';
      return;
    }
    if (machinesLoading || !xtermApi || consumedMachineParamRef.current === wanted) return;
    consumedMachineParamRef.current = wanted;
    const target = workers.find((w) => w.id === wanted && w.state === 'online');
    if (target) {
      setSelectedMachine(wanted);
      openShell(wanted);
    }
    setSearchParams({}, { replace: true });
  }, [searchParams, machinesLoading, xtermApi, workers, openShell, setSearchParams]);

  const updateTab = useCallback((key, patch) => {
    setTabs((prev) => prev.map((t) => (t.key === key ? { ...t, ...patch } : t)));
  }, []);

  const removeTab = useCallback(
    (key) => {
      const next = tabsRef.current.filter((t) => t.key !== key);
      setTabs(next);
      if (activeKeyRef.current === key) {
        setActiveKey(next.length ? next[next.length - 1].key : null);
        if (next.length) setFocusTick((n) => n + 1);
      }
      refreshSessions();
    },
    [refreshSessions]
  );

  const selectTab = (key, focus = true) => {
    setActiveKey(key);
    if (focus) setFocusTick((n) => n + 1);
  };

  const handleTabKeyDown = (e, idx) => {
    if (tabs.length < 2) return;
    let nextIdx = null;
    if (e.key === 'ArrowRight') nextIdx = (idx + 1) % tabs.length;
    if (e.key === 'ArrowLeft') nextIdx = (tabs.length + idx - 1) % tabs.length;
    if (nextIdx !== null) {
      e.preventDefault();
      const target = tabs[nextIdx];
      selectTab(target.key, true);
      tabRefs.current.get(target.key)?.focus();
    }
  };

  const handleFormSubmit = (e) => {
    e.preventDefault();
    if (!xtermApi) {
      setFormError('xterm.js did not load; terminals are unavailable.');
      return;
    }
    if (!selectedMachine) {
      setFormError('Pick an online worker agent.');
      return;
    }
    openShell(selectedMachine);
  };

  const handleRefusedLine = useCallback((payload) => {
    setTgatePhrase('');
    setTgate(payload);
  }, []);

  const handleTgateSubmit = (e) => {
    e.preventDefault();
    if (tgatePhrase.trim() !== 'I understand' || !tgate) return;
    const pending = tgate;
    setTgate(null);
    pending.sendConfirm(pending.line);
  };

  const handleTgateCancel = () => {
    const pending = tgate;
    setTgate(null);
    pending?.focusTerm?.();
  };

  const handleTerminateSession = async (session) => {
    const r = await api('POST', `/api/commands/${encodeURIComponent(session)}/cancel`, {});
    if (!r.ok) pushToast(r.data.error || 'Could not terminate.');
    refreshSessions();
  };

  const commandsHref = routeHref('commands');
  const evidenceHref = routeHref('evidence');

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Terminal</h1>
          <p>
            Mode B: interactive shells on worker agents. Shells are opt-in and run as their own concurrent long-lived
            session; everything they send is captured as evidence under the session id.
          </p>
        </div>
        <div className="page-head__actions">
          {isRoutePorted('commands') ? (
            <Link className="btn" to={commandsHref}>
              <svg className="icon" aria-hidden="true"><use href="#i-bolt" /></svg>
              One-shot runner (Mode A)
            </Link>
          ) : (
            <a className="btn" href={commandsHref}>
              <svg className="icon" aria-hidden="true"><use href="#i-bolt" /></svg>
              One-shot runner (Mode A)
            </a>
          )}
        </div>
      </div>

      {noBackend && (
        <div className="alert alert--warn" role="status" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
          <div>The panel is running without a backend, so shells cannot be opened.</div>
        </div>
      )}

      <div className="split">
        {/* Terminal sessions */}
        <section className="card" aria-label="Open terminal sessions">
          <div className="tabs" role="tablist" aria-label="Open shells" id="term-tabs">
            {tabs.map((t, idx) => {
              const selected = t.key === activeKey;
              return (
                <button
                  key={t.key}
                  ref={(el) => {
                    if (el) tabRefs.current.set(t.key, el);
                    else tabRefs.current.delete(t.key);
                  }}
                  id={`tab-${t.key}`}
                  type="button"
                  className="tab"
                  role="tab"
                  aria-selected={String(selected)}
                  aria-controls={`pnl-${t.key}`}
                  tabIndex={selected ? 0 : -1}
                  onClick={() => selectTab(t.key, true)}
                  onKeyDown={(e) => handleTabKeyDown(e, idx)}
                >
                  <svg className="icon icon--sm" aria-hidden="true"><use href="#i-terminal" /></svg>
                  {`${t.machine} `}
                  <span className="faint mono">{t.sessionId || '...'}</span>
                </button>
              );
            })}
          </div>
          <div className="card__body" id="term-panels">
            {tabs.map((t) => (
              <TerminalTabPanel
                key={t.key}
                session={t}
                active={t.key === activeKey}
                focusTick={t.key === activeKey ? focusTick : 0}
                theme={theme}
                xtermApi={xtermApi}
                onUpdate={updateTab}
                onRemove={removeTab}
                onTerminate={handleTerminateSession}
                onReattach={reattachShell}
                onRefusedLine={handleRefusedLine}
                onError={setFormError}
                onSessionsChanged={refreshSessions}
              />
            ))}
            <p className="empty" id="term-empty" hidden={tabs.length > 0}>
              No shell is open. Pick a worker agent on the right and choose <strong>Open shell</strong>.
            </p>
          </div>
          <div className="card__foot">
            The agent enforces the dangerous-command gate on every line, including pasted ones: a refused line is
            discarded and you are asked to confirm it here.
          </div>
        </section>

        {/* Rail */}
        <div className="stack">
          <section className="card" aria-labelledby="ns-title">
            <div className="card__head">
              <h2 id="ns-title">New shell</h2>
            </div>
            <form className="card__body stack" id="shell-form" data-live-shell onSubmit={handleFormSubmit}>
              <div className="field">
                <label htmlFor="ns-host">Worker agent</label>
                <select
                  className="select"
                  id="ns-host"
                  name="machine"
                  value={selectedMachine}
                  onChange={(e) => setSelectedMachine(e.target.value)}
                >
                  {workers.length === 0 ? (
                    <option value="" disabled>
                      No worker agents
                    </option>
                  ) : (
                    <>
                      {!selectedMachine && (
                        <option value="" disabled>
                          No online worker agent
                        </option>
                      )}
                      {workers.map((w) => (
                        <option key={w.id} value={w.id} disabled={w.state !== 'online'}>
                          {w.state !== 'online' ? `${w.id} (${w.state})` : w.id}
                        </option>
                      ))}
                    </>
                  )}
                </select>
                <span className="field__hint">Monitor agents are not listed: they have no shell.</span>
              </div>

              {formError ? (
                <div className="alert alert--danger" data-form-status role="status" aria-live="polite">
                  <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
                  <div>{formError}</div>
                </div>
              ) : (
                <div className="alert alert--danger" data-form-status role="status" aria-live="polite" hidden />
              )}

              <button className="btn btn--primary" type="submit">
                <svg className="icon" aria-hidden="true"><use href="#i-terminal" /></svg>
                Open shell
              </button>
            </form>
          </section>

          <section className="card" aria-labelledby="ss-title">
            <div className="card__head">
              <div>
                <h2 id="ss-title">Sessions</h2>
                <p>Every open or waiting shell; reattach or terminate.</p>
              </div>
              <span className="muted" data-session-count>{`${activeSessions.length} open`}</span>
            </div>
            <ul className="list" id="sessions">
              {activeSessions.length === 0 ? (
                <li className="list__item">
                  <div className="list__main">
                    <small>No shells.</small>
                  </div>
                </li>
              ) : (
                activeSessions.map((c) => (
                  <li key={c.session} className="list__item">
                    <div className="list__main">
                      <b>
                        {`${c.machine} `}
                        <span className="mono faint">{c.session}</span>
                      </b>
                      <small>
                        {`${c.state === 'queued' ? 'waiting' : `open since ${fmtTime(c.started_at)}`} · ${c.user || 'operator'}`}
                      </small>
                    </div>
                    <div className="row">
                      <button
                        className="btn btn--sm"
                        type="button"
                        aria-label={`Reattach to session ${c.session} on ${c.machine}`}
                        onClick={() => reattachShell(c)}
                      >
                        Reattach
                      </button>
                      <button
                        className="btn btn--sm btn--danger"
                        type="button"
                        aria-label={`Terminate session ${c.session} on ${c.machine}`}
                        onClick={() => handleTerminateSession(c.session)}
                      >
                        Terminate
                      </button>
                    </div>
                  </li>
                ))
              )}
            </ul>
            <div className="card__foot">
              Closed shells stay in{' '}
              {isRoutePorted('evidence') ? (
                <Link to={evidenceHref}>Evidence</Link>
              ) : (
                <a href={evidenceHref}>Evidence</a>
              )}
              .
            </div>
          </section>
        </div>
      </div>

      {/* Dangerous-line gate in Mode B */}
      <dialog
        ref={tgateDialogRef}
        className="dialog"
        id="dlg-tgate"
        aria-labelledby="dlg-tgate-title"
        onClose={handleTgateCancel}
      >
        <form id="tgate-form" onSubmit={handleTgateSubmit}>
          <div className="dialog__head">
            <h2 id="dlg-tgate-title">Confirm a dangerous command</h2>
            <p>The agent refused this line. It matches the dangerous-pattern list.</p>
          </div>
          <div className="dialog__body">
            <div className="alert alert--danger">
              <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
              <div>
                <strong>Line:</strong> <code data-tgate-line>{tgate?.line || ''}</code>
                <br />
                <strong>Machine:</strong> <span className="mono" data-tgate-machine>{tgate?.machine || ''}</span>
                <br />
                The attempt is logged.
              </div>
            </div>
            <div className="field">
              <label htmlFor="tgate-phrase">
                Type <code>I understand</code> to continue
              </label>
              <input
                ref={tgatePhraseRef}
                className="input input--mono"
                id="tgate-phrase"
                autoComplete="off"
                value={tgatePhrase}
                onChange={(e) => setTgatePhrase(e.target.value)}
              />
            </div>
          </div>
          <div className="dialog__foot">
            <button
              className="btn"
              type="button"
              data-dialog-close
              onClick={handleTgateCancel}
            >
              Cancel
            </button>
            <button
              className="btn btn--danger"
              type="submit"
              data-tgate-confirm
              disabled={tgatePhrase.trim() !== 'I understand'}
            >
              Run anyway
            </button>
          </div>
        </form>
      </dialog>
    </>
  );
}
