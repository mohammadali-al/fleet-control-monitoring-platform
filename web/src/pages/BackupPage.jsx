import { useCallback, useEffect, useRef, useState } from 'react';
import { StatusAlert } from '../components/StatusAlert';
import { api } from '../lib/api';
import { fmtBytes, fmtDateTime } from '../lib/fmt';
import { usePageTitle } from '../lib/title';
import { useToast } from '../lib/toast';

function formatScheduleTime(sc) {
  if (!sc) return '02:30';
  const hh = String(sc.hour ?? 0).padStart(2, '0');
  const mm = String(sc.minute ?? 0).padStart(2, '0');
  return `${hh}:${mm}`;
}

export function BackupPage() {
  usePageTitle('Backup');
  const toast = useToast();

  const [status, setStatus] = useState(null);
  const [noBackend, setNoBackend] = useState(false);
  const [noServerMsg, setNoServerMsg] = useState('Loading...');
  const [configured, setConfigured] = useState(false);
  const [server, setServer] = useState(null);
  const [lastBackup, setLastBackup] = useState(null);
  const [job, setJob] = useState(null);
  const [reminder, setReminder] = useState(null);

  const [schedule, setSchedule] = useState(null);
  const [suggested, setSuggested] = useState(null);
  const [schedEnabled, setSchedEnabled] = useState(false);
  const [schedTime, setSchedTime] = useState('02:30');
  const [schedSaving, setSchedSaving] = useState(false);

  const [diff, setDiff] = useState(null);
  const [diffLoading, setDiffLoading] = useState(false);
  const [diffError, setDiffError] = useState('');
  const [reach, setReach] = useState(null); // null | 'ok' | 'error'

  const [addForm, setAddForm] = useState({ host: '', port: '', user: '', password: '' });
  const [addingServer, setAddingServer] = useState(false);

  const [importOpen, setImportOpen] = useState(false);
  const [importConfirm, setImportConfirm] = useState(false);
  const [importOverwrite, setImportOverwrite] = useState(false);
  const [importError, setImportError] = useState(null);
  const [importing, setImporting] = useState(false);

  const mountedRef = useRef(true);
  const schedTouchedRef = useRef(false);
  const lastJobStateRef = useRef(null);
  const pollTimerRef = useRef(0);
  const haveServerRef = useRef(false);
  const importDialogRef = useRef(null);

  const stopPoll = useCallback(() => {
    if (pollTimerRef.current) {
      window.clearInterval(pollTimerRef.current);
      pollTimerRef.current = 0;
    }
  }, []);

  const loadDiff = useCallback(async () => {
    if (!haveServerRef.current) return;
    setDiffLoading(true);
    setDiffError('');
    const r = await api('GET', '/api/backup/diff');
    if (!mountedRef.current) return;
    setDiffLoading(false);
    if (!r.ok) {
      setDiff(null);
      setDiffError(r.data.error || 'The backup server could not be reached.');
      setReach('error');
      return;
    }
    setDiff(r.data);
    setDiffError('');
    setReach('ok');
  }, []);

  const loadRef = useRef(null);

  const startPoll = useCallback(() => {
    if (pollTimerRef.current) return;
    pollTimerRef.current = window.setInterval(() => {
      if (loadRef.current) loadRef.current();
    }, 2000);
  }, []);

  const load = useCallback(async () => {
    const r = await api('GET', '/api/backup');
    if (!mountedRef.current) return { ok: false, configured: false };
    if (!r.ok) {
      haveServerRef.current = false;
      setNoBackend(r.status === 503);
      setConfigured(false);
      setServer(null);
      setLastBackup(null);
      setJob(null);
      setNoServerMsg(r.data.error || 'The backup service is unavailable.');
      stopPoll();
      return { ok: false, configured: false };
    }

    const d = r.data;
    const hasServer = Boolean(d.configured);
    haveServerRef.current = hasServer;
    setNoBackend(false);
    setConfigured(hasServer);
    setServer(hasServer ? d.server || null : null);
    setLastBackup(d.last_backup || null);
    setJob(d.job || null);
    setReminder(d.reminder || null);
    setNoServerMsg(
      'No backup server is configured yet. Add one on the right: the archive exists in one place only until you do.'
    );
    if (!hasServer) {
      setDiff(null);
      setDiffError('');
      setReach(null);
    }

    const sc = d.schedule || null;
    const sg = d.suggested || null;
    setSchedule(sc);
    setSuggested(sg);
    if (sc && !schedTouchedRef.current) {
      setSchedEnabled(Boolean(sc.enabled));
      // Nothing saved yet (a zero schedule, off): offer the suggested time instead of 00:00.
      const shown = !sc.enabled && !sc.hour && !sc.minute && sg ? sg : sc;
      setSchedTime(formatScheduleTime(shown));
    }

    const isRunning = Boolean(d.job && d.job.state === 'running');
    if (isRunning) {
      startPoll();
    } else {
      stopPoll();
    }

    if (lastJobStateRef.current === 'running' && d.job && d.job.state !== 'running') {
      const done = d.job.state === 'done';
      const prefix = d.job.kind === 'import' ? 'Import ' : 'Backup ';
      setStatus({
        kind: done ? 'ok' : 'danger',
        msg: prefix + (done ? `finished. ${d.job.summary || ''}` : `failed: ${d.job.error || ''}`),
      });
      loadDiff();
    }
    lastJobStateRef.current = d.job ? d.job.state : null;

    return { ok: true, configured: hasServer };
  }, [loadDiff, startPoll, stopPoll]);

  loadRef.current = load;

  useEffect(() => {
    mountedRef.current = true;
    load().then((res) => {
      if (mountedRef.current && res?.configured) {
        loadDiff();
      }
    });

    const bgTimer = window.setInterval(() => {
      if (!document.hidden && loadRef.current) loadRef.current();
    }, 30000);

    return () => {
      mountedRef.current = false;
      stopPoll();
      window.clearInterval(bgTimer);
    };
  }, [load, loadDiff, stopPoll]);

  useEffect(() => {
    const dlg = importDialogRef.current;
    if (!dlg) return;
    if (importOpen && !dlg.open) dlg.showModal();
    if (!importOpen && dlg.open) dlg.close();
  }, [importOpen]);

  const handleRunBackup = async () => {
    const r = await api('POST', '/api/backup/run', {});
    if (!mountedRef.current) return;
    if (!r.ok) {
      setStatus({ kind: 'danger', msg: r.data.error || 'The backup could not be started.' });
      return;
    }
    setStatus({
      kind: 'info',
      msg: 'Backup started. Files are copied additively and each one is verified on the backup server.',
    });
    lastJobStateRef.current = 'running';
    if (r.data.job) setJob(r.data.job);
    load();
    startPoll();
  };

  const handleDismissReminder = async () => {
    await api('POST', '/api/backup/reminder/dismiss', {});
    if (!mountedRef.current) return;
    setReminder(null);
  };

  const handleScheduleSubmit = async (e) => {
    e.preventDefault();
    const parts = (schedTime || '02:30').split(':');
    const hour = parseInt(parts[0], 10);
    const minute = parseInt(parts[1], 10);
    setSchedSaving(true);
    const r = await api('POST', '/api/backup/schedule', {
      enabled: schedEnabled,
      hour,
      minute,
    });
    if (!mountedRef.current) return;
    setSchedSaving(false);
    if (!r.ok) {
      setStatus({ kind: 'danger', msg: r.data.error || 'The schedule was not saved.' });
      return;
    }
    schedTouchedRef.current = false;
    const saved = r.data.schedule;
    setStatus({
      kind: 'ok',
      msg: saved?.enabled ? `Schedule saved: ${saved.text}.` : 'Schedule saved (off).',
    });
    load();
  };

  const handleAddServer = async (e) => {
    e.preventDefault();
    const host = addForm.host.trim();
    const port = parseInt(addForm.port, 10) || 0;
    const user = addForm.user.trim();
    const password = addForm.password;

    setAddingServer(true);
    setStatus({
      kind: 'info',
      msg: `Connecting to ${host} and installing a key. This can take a moment.`,
    });

    const r = await api('POST', '/api/backup/server', { host, port, user, password });
    if (!mountedRef.current) return;
    setAddingServer(false);
    setAddForm((prev) => ({ ...prev, password: '' }));

    if (!r.ok) {
      setStatus({
        kind: 'danger',
        msg: r.data.error || `The server could not be added (${r.status})`,
      });
      return;
    }

    const diffSummary = r.data.diff
      ? r.data.diff.in_sync
        ? 'It is in sync.'
        : `${fmtBytes(r.data.diff.pending_bytes)} is not on it yet.`
      : `The diff could not be computed: ${r.data.diff_error || ''}`;
    setStatus({ kind: 'ok', msg: `Backup server added. ${diffSummary}` });
    setAddForm({ host: '', port: '', user: '', password: '' });

    await load();
    if (mountedRef.current && r.data.diff) {
      setDiff(r.data.diff);
      setDiffError('');
      setReach('ok');
    }
  };

  const openImportDialog = () => {
    setImportConfirm(false);
    setImportOverwrite(false);
    setImportError(null);
    setImportOpen(true);
  };

  const closeImportDialog = () => {
    setImportOpen(false);
    setImportConfirm(false);
    setImportOverwrite(false);
    setImportError(null);
  };

  const handleImportSubmit = async (e) => {
    e.preventDefault();
    if (!importConfirm) {
      toast('Tick the box to confirm the import first.');
      return;
    }
    setImporting(true);
    setImportError(null);
    const r = await api('POST', '/api/backup/import', {
      confirm: true,
      overwrite: importOverwrite,
    });
    if (!mountedRef.current) return;
    setImporting(false);
    if (!r.ok) {
      const msg = r.data.error || 'The import could not be started.';
      setStatus({ kind: 'danger', msg });
      setImportError({ kind: 'danger', msg });
      return;
    }
    closeImportDialog();
    setStatus({ kind: 'info', msg: 'Import started.' });
    lastJobStateRef.current = 'running';
    if (r.data.job) setJob(r.data.job);
    load();
    startPoll();
  };

  const running = Boolean(job && job.state === 'running');

  const runRows = [];
  if (job) {
    runRows.push({
      key: 'job',
      started: job.started,
      what: job.kind === 'import' ? 'Import (panel)' : 'Back up now (panel)',
      files: job.uploaded,
      bytes: job.bytes,
      state: job.state,
      note: job.error || job.summary,
      conflicts: job.conflicts,
    });
  }
  if (lastBackup) {
    runRows.push({
      key: 'last',
      started: lastBackup.started,
      what: 'Last backup run',
      files: lastBackup.uploaded,
      bytes: lastBackup.bytes,
      state: lastBackup.ok ? 'done' : 'failed',
      note: lastBackup.ok ? '' : `${lastBackup.failed} file(s) failed`,
    });
  }

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Backup</h1>
          <p>
            A storage-only replica of the main server&apos;s archive. It is not a standby control
            plane. Replication is additive: nothing is ever deleted on the backup server.
          </p>
        </div>
        <div className="page-head__actions">
          <button
            className="btn"
            type="button"
            id="bk-import-open"
            disabled={noBackend}
            onClick={openImportDialog}
          >
            <svg className="icon" aria-hidden="true">
              <use href="#i-upload" />
            </svg>
            Import from backup
          </button>
          <button
            className="btn btn--primary"
            type="button"
            id="bk-run"
            disabled={running || !configured}
            onClick={handleRunBackup}
          >
            <svg className="icon" aria-hidden="true">
              <use href="#i-database" />
            </svg>
            Back up now
          </button>
        </div>
      </div>

      <StatusAlert status={status} id="bk-status" style={{ marginBottom: 'var(--sp-4)' }} />

      {/* Non-intrusive daily reminder: at most once a day, never mid-task */}
      <div
        className="alert alert--warn"
        id="bk-reminder"
        role="status"
        style={{ marginBottom: 'var(--sp-4)' }}
        hidden={!reminder}
      >
        <svg className="icon" aria-hidden="true">
          <use href="#i-clock" />
        </svg>
        <div style={{ flex: 1 }}>
          <strong>Backup reminder.</strong>{' '}
          <span id="bk-reminder-text">{reminder?.message || ''}</span> This reminder shows at most
          once a day.
        </div>
        <button
          className="btn btn--sm"
          type="button"
          id="bk-reminder-dismiss"
          onClick={handleDismissReminder}
        >
          Remind me tomorrow
        </button>
      </div>

      <div className="grid">
        <div className="col-8 stack">
          {/* Backup server */}
          <section className="card" aria-labelledby="srv-title">
            <div className="card__head">
              <div>
                <h2 id="srv-title">Backup server</h2>
                <p>The main server owns the connection and the destination path.</p>
              </div>
              <span id="bk-reach">
                {reach === 'ok' && (
                  <span className="chip chip--ok">
                    <svg className="icon icon--sm" aria-hidden="true">
                      <use href="#i-check" />
                    </svg>
                    Reachable
                  </span>
                )}
                {reach === 'error' && (
                  <span className="chip chip--danger">
                    <svg className="icon icon--sm" aria-hidden="true">
                      <use href="#i-x" />
                    </svg>
                    Not reachable
                  </span>
                )}
              </span>
            </div>
            <div className="card__body stack">
              <p className="faint" id="bk-noserver" hidden={configured}>
                {noServerMsg}
              </p>
              <dl className="kv" id="bk-server" hidden={!configured}>
                <dt>Address</dt>
                <dd className="mono" data-f="address">
                  {server ? `${server.host}${server.port && server.port !== 22 ? `:${server.port}` : ''}` : ''}
                </dd>
                <dt>User</dt>
                <dd className="mono" data-f="user">
                  {server?.user || ''}
                </dd>
                <dt>Destination path</dt>
                <dd className="mono" data-f="path">
                  {server ? `${server.remote_path}  (relative to the SSH login directory)` : ''}
                </dd>
                <dt>Authentication</dt>
                <dd data-f="auth">
                  {server
                    ? server.auth === 'key'
                      ? 'SSH key (the password was discarded)'
                      : 'Password kept in a 0600 file (a key could not be installed)'
                    : ''}
                </dd>
                <dt>Host key (pinned)</dt>
                <dd className="mono" data-f="fp">
                  {server ? server.host_key_fp || 'not recorded' : ''}
                </dd>
                <dt>Last backup</dt>
                <dd data-f="last">
                  {lastBackup
                    ? `${fmtDateTime(lastBackup.finished)}${
                        lastBackup.ok ? ', completed' : `, with ${lastBackup.failed} failed file(s)`
                      }`
                    : 'never'}
                </dd>
              </dl>
              <p className="field__hint">
                The destination path is fixed, set by the main server. You do not choose it, and
                the backup server never receives anything outside it. Transport is SSH; every
                uploaded file is verified by SHA-256 on the backup server.
              </p>
            </div>
          </section>

          {/* Diff */}
          <section className="card" aria-labelledby="diff-title">
            <div className="card__head">
              <div>
                <h2 id="diff-title">What is not on the backup yet</h2>
                <p>Difference between the main server and the backup server.</p>
              </div>
              <button
                className="btn btn--sm"
                type="button"
                id="bk-diff-btn"
                disabled={!configured || diffLoading}
                onClick={loadDiff}
              >
                <svg className="icon icon--sm" aria-hidden="true">
                  <use href="#i-refresh" />
                </svg>
                Recompute
              </button>
            </div>
            <div className="table-wrap">
              <table className="table">
                <caption className="sr-only">Backup diff by content type</caption>
                <thead>
                  <tr>
                    <th scope="col">Content</th>
                    <th scope="col" className="num">
                      New on main
                    </th>
                    <th scope="col" className="num">
                      Changed on main
                    </th>
                    <th scope="col" className="num">
                      To send
                    </th>
                    <th scope="col" className="num">
                      Only on backup
                    </th>
                    <th scope="col">Status</th>
                  </tr>
                </thead>
                <tbody id="bk-diff-rows">
                  {diffLoading ? (
                    <tr>
                      <td colSpan={6} className="faint">
                        Comparing with the backup server...
                      </td>
                    </tr>
                  ) : diffError ? (
                    <tr>
                      <td colSpan={6} className="faint">
                        {diffError}
                      </td>
                    </tr>
                  ) : diff && Array.isArray(diff.groups) ? (
                    diff.groups.map((g) => {
                      const pend = g.missing + g.changed;
                      return (
                        <tr key={g.dir || g.label}>
                          <th className="cell-primary">{g.label}</th>
                          <td className={`num${g.missing ? '' : ' faint'}`}>
                            {g.missing ? String(g.missing) : 'none'}
                          </td>
                          <td className={`num${g.changed ? '' : ' faint'}`}>
                            {g.changed ? String(g.changed) : 'none'}
                          </td>
                          <td className={`num${pend ? '' : ' faint'}`}>
                            {pend ? fmtBytes(g.bytes) : 'none'}
                          </td>
                          <td className={`num${g.backup_only ? '' : ' faint'}`}>
                            {g.backup_only ? String(g.backup_only) : 'none'}
                          </td>
                          <td>
                            {pend ? (
                              <span className="chip chip--warn">
                                <svg className="icon icon--sm" aria-hidden="true">
                                  <use href="#i-clock" />
                                </svg>
                                Behind
                              </span>
                            ) : (
                              <span className="chip chip--ok">
                                <svg className="icon icon--sm" aria-hidden="true">
                                  <use href="#i-check" />
                                </svg>
                                In sync
                              </span>
                            )}
                          </td>
                        </tr>
                      );
                    })
                  ) : (
                    <tr>
                      <td colSpan={6} className="faint">
                        Add a backup server to compare.
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
            <div className="card__foot" id="bk-diff-foot">
              {diff
                ? `Computed ${fmtDateTime(diff.computed)}: ${diff.present} file(s) identical on both, ${fmtBytes(
                    diff.pending_bytes
                  )} to send.${
                    diff.scan_errors
                      ? ` ${diff.scan_errors} file(s) on this server could not be read.`
                      : ''
                  } CA keys are part of the backup set: without them a restore would force every machine to be re-enrolled. Key material is never shown in this panel.`
                : 'CA keys are part of the backup set: without them a restore would force every machine to be re-enrolled. Key material is never shown in this panel. Counts are files.'}
            </div>
          </section>

          {/* Last run */}
          <section className="card" aria-labelledby="bh-title">
            <div className="card__head">
              <h2 id="bh-title">Last run</h2>
            </div>
            <div className="table-wrap">
              <table className="table">
                <caption className="sr-only">Most recent backup or import</caption>
                <thead>
                  <tr>
                    <th scope="col">Started</th>
                    <th scope="col">What</th>
                    <th scope="col" className="num">
                      Files
                    </th>
                    <th scope="col" className="num">
                      Bytes
                    </th>
                    <th scope="col">Result</th>
                  </tr>
                </thead>
                <tbody id="bk-runs">
                  {runRows.length === 0 ? (
                    <tr>
                      <td colSpan={5} className="faint">
                        No backup has run yet.
                      </td>
                    </tr>
                  ) : (
                    runRows.map((r) => (
                      <tr key={r.key}>
                        <td className="nowrap">{fmtDateTime(r.started)}</td>
                        <td>{r.what}</td>
                        <td className="num">{String(r.files)}</td>
                        <td className="num">{fmtBytes(r.bytes)}</td>
                        <td>
                          {r.state === 'running' ? (
                            <span className="chip chip--accent chip--live">Running</span>
                          ) : r.state === 'done' ? (
                            <span className="chip chip--ok">
                              <svg className="icon icon--sm" aria-hidden="true">
                                <use href="#i-check" />
                              </svg>
                              Completed
                            </span>
                          ) : (
                            <span className="chip chip--danger">
                              <svg className="icon icon--sm" aria-hidden="true">
                                <use href="#i-x" />
                              </svg>
                              Failed
                            </span>
                          )}
                          {r.note && <div className="cell-sub">{r.note}</div>}
                          {r.conflicts && r.conflicts.length > 0 && (
                            <div className="cell-sub">
                              Kept (differ locally): {r.conflicts.slice(0, 5).join(', ')}
                              {r.conflicts.length > 5
                                ? ` and ${r.conflicts.length - 5} more`
                                : ''}
                            </div>
                          )}
                        </td>
                      </tr>
                    ))
                  )}
                </tbody>
              </table>
            </div>
            <div className="card__foot">
              The server keeps the most recent backup run and the most recent run or import started
              from this panel; older history is in the server log.
            </div>
          </section>
        </div>

        <div className="col-4 stack">
          {/* Schedule */}
          <section className="card" aria-labelledby="sch-title">
            <div className="card__head">
              <h2 id="sch-title">Schedule</h2>
            </div>
            <div className="card__body stack">
              <div className="alert">
                <svg className="icon" aria-hidden="true">
                  <use href="#i-info" />
                </svg>
                <div id="bk-suggest">
                  {suggested ? (
                    <>
                      <strong>
                        Suggested: {String(suggested.text || '').replace('daily at', 'every night at')}
                        .{' '}
                      </strong>
                      {suggested.reason || ''}
                    </>
                  ) : (
                    <>
                      <strong>Suggested: every night at 02:30.</strong> Around the middle of the
                      night, when the archive is quiet.
                    </>
                  )}
                </div>
              </div>
              <form className="stack" id="bk-sched-form" onSubmit={handleScheduleSubmit}>
                <label className="switch">
                  <input
                    type="checkbox"
                    role="switch"
                    name="enabled"
                    checked={schedEnabled}
                    onChange={(e) => {
                      schedTouchedRef.current = true;
                      setSchedEnabled(e.target.checked);
                    }}
                  />
                  <span className="switch__track" />
                  <span>{schedEnabled ? 'On' : 'Off'}</span>
                </label>
                <div className="field">
                  <label htmlFor="sch-time">Time (server local time)</label>
                  <input
                    className="input input--mono"
                    id="sch-time"
                    name="time"
                    type="time"
                    value={schedTime}
                    onChange={(e) => {
                      schedTouchedRef.current = true;
                      setSchedTime(e.target.value);
                    }}
                  />
                </div>
                <div className="row">
                  <button
                    className="btn btn--primary"
                    type="submit"
                    disabled={schedSaving || noBackend}
                  >
                    Save schedule
                  </button>
                </div>
                <p className="field__hint" id="bk-sched-next">
                  {schedule
                    ? schedule.enabled && schedule.next
                      ? `Next run: ${fmtDateTime(schedule.next)}`
                      : schedule.enabled
                        ? ''
                        : 'The schedule is off: nothing runs unless you use Back up now.'
                    : ''}
                </p>
              </form>
            </div>
          </section>

          {/* Add / replace */}
          <section className="card" aria-labelledby="add-title">
            <div className="card__head">
              <div>
                <h2 id="add-title">Add or replace backup server</h2>
              </div>
            </div>
            <form className="card__body stack" id="bk-add-form" onSubmit={handleAddServer}>
              <div className="field">
                <label htmlFor="b-ip">IP address</label>
                <input
                  className="input input--mono"
                  id="b-ip"
                  name="host"
                  inputMode="decimal"
                  placeholder="10.20.0.50"
                  required
                  value={addForm.host}
                  onChange={(e) => setAddForm((prev) => ({ ...prev, host: e.target.value }))}
                />
              </div>
              <div className="field">
                <label htmlFor="b-port">SSH port</label>
                <input
                  className="input input--mono"
                  id="b-port"
                  name="port"
                  inputMode="numeric"
                  placeholder="22"
                  value={addForm.port}
                  onChange={(e) => setAddForm((prev) => ({ ...prev, port: e.target.value }))}
                />
              </div>
              <div className="field">
                <label htmlFor="b-user">Username</label>
                <input
                  className="input"
                  id="b-user"
                  name="user"
                  autoComplete="off"
                  required
                  value={addForm.user}
                  onChange={(e) => setAddForm((prev) => ({ ...prev, user: e.target.value }))}
                />
              </div>
              <div className="field">
                <label htmlFor="b-pass">Password</label>
                <input
                  className="input"
                  id="b-pass"
                  name="password"
                  type="password"
                  autoComplete="new-password"
                  required
                  value={addForm.password}
                  onChange={(e) => setAddForm((prev) => ({ ...prev, password: e.target.value }))}
                />
              </div>
              <div className="row">
                <button
                  className="btn btn--primary"
                  type="submit"
                  disabled={addingServer || noBackend}
                >
                  <svg className="icon" aria-hidden="true">
                    <use href="#i-plus" />
                  </svg>
                  Add server
                </button>
              </div>
              <p className="field__hint">
                The password is used once, to install a key; it is not stored unless the key
                cannot be installed. After adding a server the panel computes the diff and suggests
                a schedule.
              </p>
            </form>
          </section>
        </div>
      </div>

      {/* Import is destructive in spirit: confirm first */}
      <dialog
        ref={importDialogRef}
        className="dialog"
        id="dlg-import"
        aria-labelledby="dlg-import-title"
        onClose={closeImportDialog}
      >
        <form id="bk-import-form" onSubmit={handleImportSubmit}>
          <div className="dialog__head">
            <h2 id="dlg-import-title">Import from the backup server?</h2>
            <p>Reads data from the fixed backup path and restores it on this main server.</p>
          </div>
          <div className="dialog__body">
            <div className="alert alert--warn">
              <svg className="icon" aria-hidden="true">
                <use href="#i-alert" />
              </svg>
              <div>
                Use this to rebuild a main server. A local file that differs from the backup is
                kept and reported, unless you choose to overwrite; nothing is deleted on either
                side.
              </div>
            </div>
            <StatusAlert status={importError} />
            <label className="check">
              <input
                type="checkbox"
                name="confirm"
                checked={importConfirm}
                onChange={(e) => setImportConfirm(e.target.checked)}
              />{' '}
              I understand this restores data from the backup server.
            </label>
            <label className="check">
              <input
                type="checkbox"
                name="overwrite"
                checked={importOverwrite}
                onChange={(e) => setImportOverwrite(e.target.checked)}
              />{' '}
              Overwrite local files that differ (default: keep them).
            </label>
          </div>
          <div className="dialog__foot">
            <button className="btn" type="button" onClick={closeImportDialog}>
              Cancel
            </button>
            <button
              className="btn btn--primary"
              type="submit"
              id="bk-import-go"
              disabled={importing}
            >
              <svg className="icon" aria-hidden="true">
                <use href="#i-upload" />
              </svg>
              Import
            </button>
          </div>
        </form>
      </dialog>
    </>
  );
}
