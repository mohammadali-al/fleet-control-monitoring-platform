import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { MachineLabel } from '../components/MachineLabel';
import { Link } from 'react-router-dom';
import { useMachineDetails } from '../hooks/useMachineDetails';
import { useMachines } from '../hooks/useMachines';
import { api } from '../lib/api';
import { useEvents } from '../lib/events';
import { diskView, fmtAgo, fmtBytes, fmtDateTime } from '../lib/fmt';
import { useToast } from '../lib/toast';
import { usePageTitle } from '../lib/title';
import { isRoutePorted, routeHref } from '../routes';

function RoleChip({ role }) {
  return role === 'worker' ? (
    <span className="chip chip--accent">Worker agent</span>
  ) : (
    <span className="chip chip--info">Monitor agent</span>
  );
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

function StatusAlert({ status }) {
  if (!status || !status.msg) return null;
  const cls = status.kind === 'info' ? 'alert' : `alert alert--${status.kind}`;
  const iconId = status.kind === 'ok' ? 'i-check' : status.kind === 'danger' ? 'i-alert' : 'i-info';
  return (
    <div className={cls} data-form-status role="status" aria-live="polite">
      <svg className="icon" aria-hidden="true"><use href={`#${iconId}`} /></svg>
      <div>{status.msg}</div>
    </div>
  );
}

function specText(sp) {
  if (!sp) return '';
  const parts = [];
  if (sp.os) parts.push(sp.os + (sp.arch ? `/${sp.arch}` : ''));
  if (sp.os_version) parts.push(sp.os_version);
  if (sp.cpus) parts.push(`${sp.cpus} CPU`);
  if (sp.mem_bytes) parts.push(`${fmtBytes(sp.mem_bytes)} RAM`);
  if (sp.hostname) parts.push(sp.hostname);
  return parts.join(', ');
}

function formatExpiry(expiresAt) {
  if (!expiresAt) return '';
  const left = Math.round((new Date(expiresAt).getTime() - Date.now()) / 1000);
  if (left <= 0) return 'This code has expired. Generate a new one.';
  const mins = Math.floor(left / 60);
  const secs = String(left % 60).padStart(2, '0');
  return `Expires in ${mins}:${secs}`;
}

const WATCHDOG_STAGE_LABELS = {
  none: 'None (normal)',
  throttle: 'Throttle',
  kill_session: 'Kill session',
  restart: 'Service restart',
  reboot: 'Reboot',
};

const WATCHDOG_COND_CHIPS = {
  normal: ['chip--ok', 'Normal', 'i-check'],
  breaching: ['chip--warn', 'Breaching', 'i-alert'],
  throttled: ['chip--warn', 'Throttled', 'i-alert'],
  'alert-only': ['chip--danger', 'Alert-only', 'i-alert'],
  'not-reporting': ['chip--outline', 'Not reporting', 'i-x'],
};

const DETAIL_DL_STYLE = {
  display: 'grid',
  gridTemplateColumns: 'minmax(8.5rem, auto) minmax(0, 1fr)',
  gap: 'var(--sp-2) var(--sp-3)',
  fontSize: 'var(--fs-sm)',
  alignItems: 'baseline',
};

const DETAIL_DT_STYLE = {
  color: 'var(--text-muted)',
};

const DETAIL_DD_STYLE = {
  minWidth: 0,
  fontWeight: 600,
  overflowWrap: 'anywhere',
  textAlign: 'right',
};

function DetailDiskSection({ disk }) {
  const rawDisk = diskView(disk);
  const diskStatus = disk?.status || '';
  const view = rawDisk
    ? {
        ...rawDisk,
        className:
          diskStatus === 'critical'
            ? 'danger'
            : diskStatus === 'warn'
              ? 'warn'
              : rawDisk.className,
        word:
          diskStatus === 'critical'
            ? 'intake paused'
            : diskStatus === 'warn'
              ? 'warning'
              : rawDisk.word,
      }
    : null;

  const statusChip =
    diskStatus === 'critical' ? (
      <span className="chip chip--danger">Intake paused</span>
    ) : diskStatus === 'warn' ? (
      <span className="chip chip--warn">Warning</span>
    ) : diskStatus === 'ok' ? (
      <span className="chip chip--ok">OK</span>
    ) : (
      <span className="chip chip--outline">Unknown</span>
    );

  return (
    <section className="card" aria-label="Disk state">
      <div className="card__head">
        <h3>Disk</h3>
        {statusChip}
      </div>
      <div className="card__body stack" style={{ gap: 'var(--sp-3)' }}>
        {view ? (
          <div className={`meter${view.className ? ` meter--${view.className}` : ''}`}>
            <div className="meter__bar">
              <div className="meter__fill" style={{ width: `${view.pct}%` }} />
            </div>
            <div className="meter__text">
              <span>{view.used} / {view.total}</span>
              <span>{view.pct}%{view.word ? ` · ${view.word}` : ''}</span>
            </div>
          </div>
        ) : (
          <div className="muted" style={{ fontSize: 'var(--fs-sm)' }}>No disk capacity reported yet.</div>
        )}
        <dl style={DETAIL_DL_STYLE}>
          <dt style={DETAIL_DT_STYLE}>Free / Total</dt>
          <dd className="mono" style={DETAIL_DD_STYLE}>
            {disk?.total_bytes ? `${fmtBytes(disk.free_bytes)} / ${fmtBytes(disk.total_bytes)}` : 'Unknown'}
          </dd>
          <dt style={DETAIL_DT_STYLE}>Disk updated</dt>
          <dd style={DETAIL_DD_STYLE}>
            {disk?.updated_at ? `${fmtAgo(disk.updated_at)} (${fmtDateTime(disk.updated_at)})` : 'Never'}
          </dd>
        </dl>
      </div>
    </section>
  );
}

function sameStringList(a, b) {
  const xs = Array.isArray(a) ? a : [];
  const ys = Array.isArray(b) ? b : [];
  if (xs.length !== ys.length) return false;
  for (let i = 0; i < xs.length; i += 1) {
    if (xs[i] !== ys[i]) return false;
  }
  return true;
}

function isAgentOutdated(machine, serverVersion) {
  if (!machine || machine.removed || !serverVersion) return false;
  if (machine.agent_version) return machine.agent_version !== serverVersion;
  return machine.state === 'online';
}

function isAgentUpdateInProgress(upd) {
  return Boolean(upd && (upd.status === 'requested' || upd.status === 'downloading' || upd.status === 'applying'));
}

function formatUpdateStatus(upd) {
  if (!upd || !upd.status) return '';
  switch (upd.status) {
    case 'requested':
      return 'Update requested...';
    case 'downloading':
      return 'Downloading update...';
    case 'applying':
      return 'Applying & restarting...';
    case 'ok':
      return upd.target_version ? `Updated to ${upd.target_version}` : 'Updated';
    case 'failed':
      return upd.error ? `Update failed: ${upd.error}` : 'Update failed';
    default:
      return upd.status;
  }
}

function VersionCell({ machine, serverVersion }) {
  const outdated = isAgentOutdated(machine, serverVersion);
  const ver = machine?.agent_version;
  const upd = machine?.agent_update;
  const updText = formatUpdateStatus(upd);
  return (
    <div>
      <div className="row" style={{ gap: 'var(--sp-1)', flexWrap: 'wrap' }}>
        <span className={ver ? 'mono' : 'faint'}>
          {ver || (machine?.state === 'online' ? 'Unreported' : 'Unknown')}
        </span>
        {outdated && (
          <span
            className="chip chip--warn"
            title={serverVersion ? `Server version: ${serverVersion}` : undefined}
          >
            Outdated
          </span>
        )}
      </div>
      {updText && (
        <div
          className="cell-sub"
          style={upd?.status === 'failed' ? { color: 'var(--danger)' } : undefined}
          title={upd?.error || undefined}
        >
          {updText}
        </div>
      )}
    </div>
  );
}

function MachineDetailsDrawer({
  machineId,
  machine,
  watchdog,
  serverVersion,
  loading,
  error,
  notFound,
  closeBtnRef,
  onClose,
  onGroupsSaved,
  onUpdateAgent,
  updating,
}) {
  const [groupDraft, setGroupDraft] = useState([]);
  const [groupInput, setGroupInput] = useState('');
  const [groupDirty, setGroupDirty] = useState(false);
  const [groupBusy, setGroupBusy] = useState(false);
  const [groupStatus, setGroupStatus] = useState(null);
  const [policyGroups, setPolicyGroups] = useState([]);

  const machineGroupsKey = Array.isArray(machine?.groups) ? machine.groups.join('\0') : '';

  useEffect(() => {
    setGroupDraft(Array.isArray(machine?.groups) ? machine.groups : []);
    setGroupInput('');
    setGroupDirty(false);
    setGroupStatus(null);
  }, [machineId]);

  useEffect(() => {
    if (!groupDirty) {
      setGroupDraft(Array.isArray(machine?.groups) ? machine.groups : []);
    }
  }, [machineGroupsKey, groupDirty]);

  useEffect(() => {
    if (!machineId) return undefined;
    let active = true;
    api('GET', '/api/watchdog/policy').then((r) => {
      if (!active || !r.ok) return;
      setPolicyGroups(Array.isArray(r.data.groups) ? r.data.groups : []);
    });
    return () => {
      active = false;
    };
  }, [machineId]);

  if (!machineId) return null;

  const privateIPs = machine?.private_ips || [];
  const primaryPrivateIP = machine?.primary_private_ip || '';
  const wdCond = watchdog ? WATCHDOG_COND_CHIPS[watchdog.condition] || WATCHDOG_COND_CHIPS['not-reporting'] : null;
  const wdStageLabel = watchdog ? WATCHDOG_STAGE_LABELS[watchdog.stage] || watchdog.stage || 'None' : '';
  const wdHref = routeHref('watchdog');
  const savedGroups = Array.isArray(machine?.groups) ? machine.groups : [];
  const groupsChanged = !sameStringList(groupDraft, savedGroups);
  const availablePolicyGroups = policyGroups.filter((g) => !groupDraft.includes(g));

  const addGroupName = (raw) => {
    const name = raw.trim();
    if (!name) return;
    if (groupDraft.includes(name)) {
      setGroupInput('');
      return;
    }
    setGroupDraft((prev) => [...prev, name]);
    setGroupDirty(true);
    setGroupStatus(null);
    setGroupInput('');
  };

  const removeGroupName = (name) => {
    setGroupDraft((prev) => prev.filter((g) => g !== name));
    setGroupDirty(true);
    setGroupStatus(null);
  };

  const resetGroupsDraft = () => {
    setGroupDraft(savedGroups);
    setGroupInput('');
    setGroupDirty(false);
    setGroupStatus(null);
  };

  const saveGroups = async (e) => {
    e.preventDefault();
    if (!machine) return;
    const pending = groupInput.trim();
    const nextGroups = pending && !groupDraft.includes(pending) ? [...groupDraft, pending] : groupDraft;
    setGroupBusy(true);
    setGroupStatus({ kind: 'info', msg: 'Saving watchdog groups...' });
    const r = await api('POST', `/api/machines/${encodeURIComponent(machine.id)}/groups`, { groups: nextGroups });
    setGroupBusy(false);
    if (!r.ok) {
      setGroupStatus({ kind: 'danger', msg: r.data.error || `Could not save groups (${r.status})` });
      return;
    }
    const persisted = Array.isArray(r.data.machine?.groups) ? r.data.machine.groups : [];
    setGroupDraft(persisted);
    setGroupInput('');
    setGroupDirty(false);
    setGroupStatus({ kind: 'ok', msg: persisted.length ? `Assigned groups: ${persisted.join(', ')}.` : 'Cleared watchdog groups.' });
    if (onGroupsSaved) onGroupsSaved(persisted);
  };

  return (
    <div
      data-machine-details-backdrop
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
      style={{
        position: 'fixed',
        inset: 0,
        zIndex: 75,
        background: 'var(--scrim)',
        display: 'flex',
        justifyContent: 'flex-end',
      }}
    >
      <aside
        role="dialog"
        aria-modal="true"
        aria-labelledby="machine-details-title"
        data-machine-details={machineId}
        style={{
          width: 'min(34rem, 100vw)',
          height: '100%',
          overflowY: 'auto',
          background: 'var(--surface)',
          color: 'var(--text)',
          borderLeft: '2px solid var(--border-strong)',
          boxShadow: 'var(--ink-shadow)',
          display: 'flex',
          flexDirection: 'column',
        }}
      >
        <div
          style={{
            position: 'sticky',
            top: 0,
            zIndex: 2,
            display: 'flex',
            alignItems: 'flex-start',
            justifyContent: 'space-between',
            gap: 'var(--sp-3)',
            padding: 'var(--sp-4)',
            borderBottom: '1.5px solid var(--rule)',
            background: 'var(--surface-2)',
          }}
        >
          <div className="stack" style={{ gap: 'var(--sp-2)', minWidth: 0 }}>
            <div>
              <div className="crumbs">Machine details</div>
              <h2 id="machine-details-title" className="mono" style={{ fontSize: 'var(--fs-lg)', textTransform: 'none', overflowWrap: 'anywhere' }}>
                {machine ? <MachineLabel machine={machine} /> : machineId}
              </h2>
            </div>
            {machine && (
              <div className="row" style={{ gap: 'var(--sp-2)' }}>
                <RoleChip role={machine.role} />
                <StateChip state={machine.state} />
                {machine.stopped && <span className="chip chip--warn">Stopped</span>}
                {machine.removed && <span className="chip chip--outline">Decommissioned</span>}
              </div>
            )}
          </div>
          <button
            ref={closeBtnRef}
            className="icon-btn icon-btn--bordered"
            type="button"
            aria-label="Close machine details"
            title="Close"
            onClick={onClose}
          >
            <svg className="icon" aria-hidden="true"><use href="#i-x" /></svg>
          </button>
        </div>

        <div className="stack" style={{ padding: 'var(--sp-4)', gap: 'var(--sp-4)', flex: 1 }}>
          {notFound && (
            <div className="alert alert--warn" role="status">
              <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
              <div>{error || 'This machine is no longer in the registry.'}</div>
            </div>
          )}

          {error && !notFound && (
            <div className="alert alert--danger" role="status">
              <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
              <div>{error}</div>
            </div>
          )}

          {machine?.note && (
            <div className="alert" role="status">
              <svg className="icon" aria-hidden="true"><use href="#i-info" /></svg>
              <div>{machine.note}</div>
            </div>
          )}

          {!machine ? (
            <div className="muted" style={{ fontSize: 'var(--fs-sm)' }}>
              {loading ? 'Loading machine details...' : 'Machine details unavailable.'}
            </div>
          ) : (
            <>
              <section className="card" aria-label="Identity and connection">
                <div className="card__head">
                  <h3>Identity &amp; connection</h3>
                  <span className="chip chip--outline chip--mono">{machine.connect_mode}</span>
                </div>
                <div className="card__body">
                  <dl style={DETAIL_DL_STYLE}>
                    <dt style={DETAIL_DT_STYLE}>Machine ID</dt>
                    <dd className="mono" style={DETAIL_DD_STYLE}>{machine.id}</dd>
                    <dt style={DETAIL_DT_STYLE}>Role</dt>
                    <dd className="mono" style={DETAIL_DD_STYLE}>{machine.role}</dd>
                    <dt style={DETAIL_DT_STYLE}>State</dt>
                    <dd className="mono" style={DETAIL_DD_STYLE}>{machine.state}</dd>
                    <dt style={DETAIL_DT_STYLE}>Connect mode</dt>
                    <dd className="mono" style={DETAIL_DD_STYLE}>{machine.connect_mode}</dd>
                    <dt style={DETAIL_DT_STYLE}>Agent version</dt>
                    <dd style={DETAIL_DD_STYLE}>
                      <span className={machine.agent_version ? 'mono' : 'faint'}>
                        {machine.agent_version || (machine.state === 'online' ? 'Unreported' : 'Unknown')}
                      </span>
                      {isAgentOutdated(machine, serverVersion) && (
                        <>
                          {' '}
                          <span className="chip chip--warn">Outdated</span>
                        </>
                      )}
                    </dd>
                    <dt style={DETAIL_DT_STYLE}>Server version</dt>
                    <dd className="mono" style={DETAIL_DD_STYLE}>{serverVersion || 'Unknown'}</dd>
                    {machine.agent_update && (
                      <>
                        <dt style={DETAIL_DT_STYLE}>Update status</dt>
                        <dd
                          style={{
                            ...DETAIL_DD_STYLE,
                            color: machine.agent_update.status === 'failed' ? 'var(--danger)' : undefined,
                          }}
                        >
                          {formatUpdateStatus(machine.agent_update)}
                        </dd>
                      </>
                    )}
                    <dt style={DETAIL_DT_STYLE}>Last seen</dt>
                    <dd style={DETAIL_DD_STYLE}>
                      {machine.last_seen ? `${fmtAgo(machine.last_seen)} (${fmtDateTime(machine.last_seen)})` : 'Never'}
                    </dd>
                    {machine.role === 'worker' && (
                      <>
                        <dt style={DETAIL_DT_STYLE}>Command queue</dt>
                        <dd className="mono" style={DETAIL_DD_STYLE}>
                          {machine.running || 0} running · {machine.queued || 0} queued
                        </dd>
                      </>
                    )}
                  </dl>
                  {!machine.removed && (
                    <div className="row row--between" style={{ marginTop: 'var(--sp-3)', gap: 'var(--sp-2)' }}>
                      <span className="muted" style={{ fontSize: 'var(--fs-xs)' }}>
                        {machine.state !== 'online'
                          ? `Agent must be online to update (${machine.state}).`
                          : isAgentOutdated(machine, serverVersion)
                            ? `Update available (${serverVersion}).`
                            : 'Agent matches the server version.'}
                      </span>
                      <button
                        className="btn btn--sm btn--primary"
                        type="button"
                        disabled={
                          updating ||
                          machine.state !== 'online' ||
                          !isAgentOutdated(machine, serverVersion) ||
                          isAgentUpdateInProgress(machine.agent_update)
                        }
                        onClick={() => onUpdateAgent && onUpdateAgent(machine.id)}
                      >
                        <svg className="icon icon--sm" aria-hidden="true"><use href="#i-upload" /></svg>
                        {updating || isAgentUpdateInProgress(machine.agent_update) ? 'Updating...' : 'Update agent'}
                      </button>
                    </div>
                  )}
                </div>
              </section>

              <section className="card" aria-label="Watchdog">
                <div className="card__head">
                  <h3>Watchdog</h3>
                  {wdCond && (
                    <span className={`chip ${wdCond[0]}`}>
                      <svg className="icon icon--sm" aria-hidden="true"><use href={`#${wdCond[2]}`} /></svg>
                      {wdCond[1]}
                    </span>
                  )}
                </div>
                <div className="card__body stack" style={{ gap: 'var(--sp-3)' }}>
                  {machine.role !== 'worker' ? (
                    <div className="muted" style={{ fontSize: 'var(--fs-sm)' }}>
                      Not applicable (monitor agents do not run the watchdog).
                    </div>
                  ) : machine.removed ? (
                    <div className="muted" style={{ fontSize: 'var(--fs-sm)' }}>
                      Not applicable (decommissioned).
                    </div>
                  ) : watchdog ? (
                    <>
                      <dl style={DETAIL_DL_STYLE}>
                        <dt style={DETAIL_DT_STYLE}>Watchdog stage</dt>
                        <dd style={DETAIL_DD_STYLE}>
                          <span className="ladder" aria-hidden="true">
                            {[1, 2, 3, 4].map((i) => {
                              const cls =
                                i <= (watchdog.level || 0)
                                  ? watchdog.condition === 'alert-only' || i >= 3
                                    ? 'on hi'
                                    : 'on'
                                  : '';
                              return <i key={i} className={cls || undefined} />;
                            })}
                          </span>{' '}
                          <span>{wdStageLabel}</span>
                        </dd>
                        <dt style={DETAIL_DT_STYLE}>Restarts today</dt>
                        <dd className="mono" style={DETAIL_DD_STYLE}>
                          {watchdog.restarts_today} / {watchdog.restart_cap || 0}
                        </dd>
                        <dt style={DETAIL_DT_STYLE}>Reboots today</dt>
                        <dd className="mono" style={DETAIL_DD_STYLE}>
                          {watchdog.reboots_today} / {watchdog.reboot_cap || 0}
                        </dd>
                        <dt style={DETAIL_DT_STYLE}>Policy</dt>
                        <dd style={DETAIL_DD_STYLE}>
                          {watchdog.policy_acked
                            ? watchdog.policy_ok
                              ? `v${watchdog.policy_in_force} active`
                              : `v${watchdog.policy_in_force} error: ${watchdog.policy_error || 'rejected'}`
                            : 'Not acknowledged'}
                        </dd>
                        {Array.isArray(watchdog.breaches) && watchdog.breaches.length > 0 && (
                          <>
                            <dt style={DETAIL_DT_STYLE}>Active breaches</dt>
                            <dd className="mono" style={DETAIL_DD_STYLE}>{watchdog.breaches.join(', ')}</dd>
                          </>
                        )}
                      </dl>
                      <div>
                        {isRoutePorted('watchdog') ? (
                          <Link className="btn btn--sm" to={wdHref}>Open Watchdog</Link>
                        ) : (
                          <a className="btn btn--sm" href={wdHref}>Open Watchdog</a>
                        )}
                      </div>
                    </>
                  ) : (
                    <div className="muted" style={{ fontSize: 'var(--fs-sm)' }}>
                      {loading ? 'Loading watchdog state...' : 'Watchdog state unavailable.'}
                    </div>
                  )}

                  <form
                    data-machine-groups-form
                    className="stack"
                    style={{ gap: 'var(--sp-2)', paddingTop: 'var(--sp-3)', borderTop: '1px solid var(--rule)' }}
                    onSubmit={saveGroups}
                  >
                    <div className="row row--between">
                      <strong style={{ fontSize: 'var(--fs-sm)' }}>Watchdog groups</strong>
                      <span className="muted" style={{ fontSize: 'var(--fs-xs)' }}>
                        Precedence: host &gt; group &gt; default
                      </span>
                    </div>
                    <div className="row" style={{ gap: 'var(--sp-1)', flexWrap: 'wrap' }} data-machine-groups-list>
                      {groupDraft.length > 0 ? (
                        groupDraft.map((g) => (
                          <span key={g} className="chip chip--outline chip--mono">
                            {g}
                            {!machine.removed && (
                              <button
                                type="button"
                                aria-label={`Remove group ${g}`}
                                title={`Remove ${g}`}
                                onClick={() => removeGroupName(g)}
                                style={{
                                  background: 'none',
                                  border: 0,
                                  padding: '0 0 0 0.25rem',
                                  color: 'inherit',
                                  cursor: 'pointer',
                                  font: 'inherit',
                                  lineHeight: 1,
                                }}
                              >
                                ×
                              </button>
                            )}
                          </span>
                        ))
                      ) : (
                        <span className="muted" style={{ fontSize: 'var(--fs-sm)' }}>
                          No groups assigned (default policy).
                        </span>
                      )}
                    </div>
                    {!machine.removed && (
                      <>
                        <div className="row" style={{ gap: 'var(--sp-2)' }}>
                          <input
                            className="input input--mono"
                            id="machine-group-input"
                            type="text"
                            placeholder="Add group (e.g. edge, gpu)"
                            aria-label="Watchdog group name"
                            value={groupInput}
                            onChange={(e) => setGroupInput(e.target.value)}
                            onKeyDown={(e) => {
                              if (e.key === 'Enter') {
                                e.preventDefault();
                                addGroupName(groupInput);
                              }
                            }}
                            style={{ flex: 1, minWidth: 0 }}
                          />
                          <button
                            className="btn btn--sm"
                            type="button"
                            disabled={!groupInput.trim() || groupBusy}
                            onClick={() => addGroupName(groupInput)}
                          >
                            Add
                          </button>
                        </div>
                        {availablePolicyGroups.length > 0 && (
                          <div className="row" style={{ gap: 'var(--sp-1)', flexWrap: 'wrap' }}>
                            <span className="muted" style={{ fontSize: 'var(--fs-xs)' }}>Policy groups:</span>
                            {availablePolicyGroups.map((g) => (
                              <button
                                key={g}
                                className="btn btn--sm"
                                type="button"
                                onClick={() => addGroupName(g)}
                              >
                                + {g}
                              </button>
                            ))}
                          </div>
                        )}
                        <StatusAlert status={groupStatus} />
                        <div className="row" style={{ gap: 'var(--sp-2)' }}>
                          <button
                            className="btn btn--sm btn--primary"
                            type="submit"
                            data-save-groups
                            disabled={groupBusy || (!groupsChanged && !groupInput.trim())}
                          >
                            {groupBusy ? 'Saving...' : 'Save groups'}
                          </button>
                          {groupsChanged && (
                            <button
                              className="btn btn--sm"
                              type="button"
                              disabled={groupBusy}
                              onClick={resetGroupsDraft}
                            >
                              Reset
                            </button>
                          )}
                        </div>
                      </>
                    )}
                  </form>
                </div>
              </section>

              <section className="card" aria-label="Addresses">
                <div className="card__head">
                  <h3>Addresses</h3>
                </div>
                <div className="card__body">
                  <dl style={DETAIL_DL_STYLE}>
                    <dt style={DETAIL_DT_STYLE}>Primary private IP</dt>
                    <dd className="mono" style={DETAIL_DD_STYLE}>
                      {primaryPrivateIP || 'Not reported yet'}
                    </dd>
                    <dt style={DETAIL_DT_STYLE}>Private IPs</dt>
                    <dd className="mono" style={DETAIL_DD_STYLE}>
                      {privateIPs.length > 0 ? privateIPs.join(', ') : 'None'}
                    </dd>
                    <dt style={DETAIL_DT_STYLE}>Public IP</dt>
                    <dd className="mono" style={DETAIL_DD_STYLE}>
                      {machine.public_ip || 'Not checked yet'}
                    </dd>
                    <dt style={DETAIL_DT_STYLE}>Public IP checked</dt>
                    <dd style={DETAIL_DD_STYLE}>
                      {machine.public_ip_at
                        ? `${fmtAgo(machine.public_ip_at)} (${fmtDateTime(machine.public_ip_at)})`
                        : 'Never'}
                    </dd>
                  </dl>
                </div>
              </section>

              <DetailDiskSection disk={machine.disk} />

              <section className="card" aria-label="Certificates and enrollment">
                <div className="card__head">
                  <h3>Enrollment &amp; certificates</h3>
                </div>
                <div className="card__body">
                  <dl style={DETAIL_DL_STYLE}>
                    <dt style={DETAIL_DT_STYLE}>Enrollment status</dt>
                    <dd style={DETAIL_DD_STYLE}>
                      {machine.removed
                        ? `Decommissioned${machine.removed_at ? ` (${fmtDateTime(machine.removed_at)})` : ''}`
                        : machine.provisioned
                          ? machine.enrolled_via === 'token'
                            ? machine.stopped
                              ? 'Enrolled via token (stopped)'
                              : 'Enrolled via token'
                            : 'Provisioned via SSH'
                          : 'Not provisioned'}
                    </dd>
                    <dt style={DETAIL_DT_STYLE}>Cert fingerprint</dt>
                    <dd className="mono" style={DETAIL_DD_STYLE}>
                      {machine.cert_fingerprint || 'None'}
                    </dd>
                    {machine.revoked_fingerprint && (
                      <>
                        <dt style={DETAIL_DT_STYLE}>Revoked cert</dt>
                        <dd className="mono" style={DETAIL_DD_STYLE}>{machine.revoked_fingerprint}</dd>
                      </>
                    )}
                    {machine.ssh_host_key && (
                      <>
                        <dt style={DETAIL_DT_STYLE}>SSH host key</dt>
                        <dd className="mono" style={DETAIL_DD_STYLE}>{machine.ssh_host_key}</dd>
                      </>
                    )}
                    <dt style={DETAIL_DT_STYLE}>Registered</dt>
                    <dd style={DETAIL_DD_STYLE}>
                      {machine.created_at ? fmtDateTime(machine.created_at) : 'Unknown'}
                    </dd>
                    <dt style={DETAIL_DT_STYLE}>Record updated</dt>
                    <dd style={DETAIL_DD_STYLE}>
                      {machine.updated_at ? `${fmtAgo(machine.updated_at)} (${fmtDateTime(machine.updated_at)})` : 'Unknown'}
                    </dd>
                  </dl>
                </div>
              </section>

              {machine.specs && (
                <section className="card" aria-label="Host specs">
                  <div className="card__head">
                    <h3>Host specs</h3>
                  </div>
                  <div className="card__body">
                    <dl style={DETAIL_DL_STYLE}>
                      {machine.specs.hostname && (
                        <>
                          <dt style={DETAIL_DT_STYLE}>Hostname</dt>
                          <dd className="mono" style={DETAIL_DD_STYLE}>{machine.specs.hostname}</dd>
                        </>
                      )}
                      {machine.specs.os && (
                        <>
                          <dt style={DETAIL_DT_STYLE}>OS / Arch</dt>
                          <dd className="mono" style={DETAIL_DD_STYLE}>
                            {machine.specs.os}{machine.specs.arch ? `/${machine.specs.arch}` : ''}
                          </dd>
                        </>
                      )}
                      {machine.specs.os_version && (
                        <>
                          <dt style={DETAIL_DT_STYLE}>OS version</dt>
                          <dd style={DETAIL_DD_STYLE}>{machine.specs.os_version}</dd>
                        </>
                      )}
                      {machine.specs.kernel && (
                        <>
                          <dt style={DETAIL_DT_STYLE}>Kernel</dt>
                          <dd className="mono" style={DETAIL_DD_STYLE}>{machine.specs.kernel}</dd>
                        </>
                      )}
                      {machine.specs.cpus > 0 && (
                        <>
                          <dt style={DETAIL_DT_STYLE}>CPUs</dt>
                          <dd className="mono" style={DETAIL_DD_STYLE}>{machine.specs.cpus}</dd>
                        </>
                      )}
                      {machine.specs.mem_bytes > 0 && (
                        <>
                          <dt style={DETAIL_DT_STYLE}>Memory</dt>
                          <dd className="mono" style={DETAIL_DD_STYLE}>{fmtBytes(machine.specs.mem_bytes)}</dd>
                        </>
                      )}
                    </dl>
                  </div>
                </section>
              )}
            </>
          )}
        </div>

        <div
          className="card__foot row row--between"
          style={{ position: 'sticky', bottom: 0, zIndex: 2 }}
        >
          <span>Live updates via /ws/events</span>
          <button className="btn btn--sm" type="button" onClick={onClose}>
            Close
          </button>
        </div>
      </aside>
    </div>
  );
}

export function MachinesPage() {
  usePageTitle('Machines');
  const pushToast = useToast();
  const { machines, summary, serverVersion, noBackend, error: loadError, refresh, setRefreshPaused } = useMachines();

  // Panels
  const [onboardOpen, setOnboardOpen] = useState(false);
  const [enrollOpen, setEnrollOpen] = useState(false);
  const onboardIpRef = useRef(null);
  const enrollRoleRef = useRef(null);

  // Filters
  const [filterQ, setFilterQ] = useState('');
  const [filterRole, setFilterRole] = useState('all');
  const [filterState, setFilterState] = useState('all');

  // Public IP refresh state (per-row and bulk)
  const [refreshingIds, setRefreshingIds] = useState(() => new Set());
  const [bulkRefreshing, setBulkRefreshing] = useState(false);

  // Agent self-update state (per-row and bulk)
  const [updatingIds, setUpdatingIds] = useState(() => new Set());
  const [bulkUpdating, setBulkUpdating] = useState(false);

  // Onboard form state
  const [onboardForm, setOnboardForm] = useState({
    host: '',
    port: '22',
    user: '',
    auth: 'password',
    password: '',
    private_key: '',
    role: 'worker',
    connect_mode: 'server_dial',
  });
  const [onboardBusy, setOnboardBusy] = useState(false);
  const [onboardStatus, setOnboardStatus] = useState(null);

  // Token enrollment state
  const [enrollEnabled, setEnrollEnabled] = useState(null); // null until checked
  const [enrolledMachines, setEnrolledMachines] = useState([]);
  const [enrollRole, setEnrollRole] = useState('worker');
  const [enrollBusy, setEnrollBusy] = useState(false);
  const [enrollStatus, setEnrollStatus] = useState(null);
  const [enrollResult, setEnrollResult] = useState(null);
  const [enrollExpiryText, setEnrollExpiryText] = useState('');
  // Outcome of the issued token: null (still pending) | { state: 'used', machineId } | { state: 'expired' }
  const [enrollOutcome, setEnrollOutcome] = useState(null);
  const enrollBusyRef = useRef(false);

  // Row menu state
  const [openMenu, setOpenMenu] = useState(null); // { id, top, left }
  const menuPopRef = useRef(null);

  // Re-provision / Remove dialog
  const [credAction, setCredAction] = useState(null); // { kind: 'reprovision'|'remove', machine: string }
  const [credForm, setCredForm] = useState({ user: '', password: '', private_key: '', port: '22', skip_remote: false });
  const [credBusy, setCredBusy] = useState(false);
  const [credStatus, setCredStatus] = useState(null);
  const credDialogRef = useRef(null);

  // Token-enrolled Stop / Detach dialog
  const [enrollAct, setEnrollAct] = useState(null); // { kind: 'stop'|'detach', machine: string }
  const [enrollActBusy, setEnrollActBusy] = useState(false);
  const [enrollActStatus, setEnrollActStatus] = useState(null);
  const enrollActDialogRef = useRef(null);

  // Per-machine details drawer
  const [detailsId, setDetailsId] = useState(null);
  const detailsCloseBtnRef = useRef(null);
  const detailsRestoreFocusRef = useRef(null);
  const selectedMachineFromList = useMemo(
    () => (detailsId ? machines.find((m) => m.id === detailsId) || null : null),
    [machines, detailsId]
  );
  const {
    machine: detailsMachine,
    watchdog: detailsWatchdog,
    serverVersion: detailsServerVersion,
    loading: detailsLoading,
    error: detailsError,
    notFound: detailsNotFound,
    refresh: refreshDetails,
  } = useMachineDetails(detailsId, selectedMachineFromList);

  // Pause table refreshes while a row menu is open so the menu never closes under the cursor.
  useEffect(() => {
    setRefreshPaused(Boolean(openMenu));
  }, [openMenu, setRefreshPaused]);

  // Enrolled machines polling + live updates.
  const refreshEnrolled = useCallback(async () => {
    if (enrollBusyRef.current) return;
    const r = await api('GET', '/api/enroll/machines');
    if (!r.ok) {
      setEnrollEnabled(false);
      return;
    }
    setEnrollEnabled(true);
    setEnrolledMachines(Array.isArray(r.data.machines) ? r.data.machines : []);
  }, []);

  const enrollDebounceRef = useRef(0);
  const refreshEnrolledSoon = useCallback(() => {
    if (enrollDebounceRef.current) window.clearTimeout(enrollDebounceRef.current);
    enrollDebounceRef.current = window.setTimeout(() => {
      enrollDebounceRef.current = 0;
      refreshEnrolled();
    }, 300);
  }, [refreshEnrolled]);

  useEffect(() => {
    refreshEnrolled();
    const iv = window.setInterval(refreshEnrolled, 15000);
    return () => {
      window.clearInterval(iv);
      if (enrollDebounceRef.current) window.clearTimeout(enrollDebounceRef.current);
    };
  }, [refreshEnrolled]);

  // Watch the issued token (T-0071): GET /api/enroll/tokens/{code} says pending|used|expired
  // and, once used, which machine it enrolled.
  const enrollCode = enrollResult?.code || '';
  const checkEnrollToken = useCallback(async () => {
    if (!enrollCode) return;
    const r = await api('GET', `/api/enroll/tokens/${encodeURIComponent(enrollCode)}`);
    if (!r.ok) return;
    if (r.data.state === 'used') {
      setEnrollOutcome((cur) =>
        cur && cur.state === 'used' ? cur : { state: 'used', machineId: r.data.machine_id }
      );
      refreshEnrolled();
    } else if (r.data.state === 'expired') {
      setEnrollOutcome((cur) => cur || { state: 'expired' });
    }
  }, [enrollCode, refreshEnrolled]);

  useEffect(() => {
    if (!enrollCode || enrollOutcome) return undefined;
    checkEnrollToken();
    const iv = window.setInterval(checkEnrollToken, 3000);
    return () => window.clearInterval(iv);
  }, [enrollCode, enrollOutcome, checkEnrollToken]);

  useEvents(
    (msg) => {
      if (msg && msg.type === 'machine') {
        refreshEnrolledSoon();
        checkEnrollToken();
      }
    },
    refreshEnrolled
  );

  // Token expiry countdown
  useEffect(() => {
    if (!enrollResult?.expires_at) {
      setEnrollExpiryText('');
      return undefined;
    }
    const update = () => setEnrollExpiryText(formatExpiry(enrollResult.expires_at));
    update();
    const iv = window.setInterval(update, 1000);
    return () => window.clearInterval(iv);
  }, [enrollResult]);

  // Row menu outside-click, Escape, and scroll handling
  useEffect(() => {
    if (!openMenu) return undefined;
    const onDocClick = (e) => {
      if (e.target.closest('[data-menu-btn]') || e.target.closest('.menu__pop')) return;
      setOpenMenu(null);
    };
    const onKeyDown = (e) => {
      if (e.key === 'Escape') {
        setOpenMenu(null);
      } else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        const pop = menuPopRef.current;
        if (!pop) return;
        const items = Array.from(pop.querySelectorAll('[role="menuitem"]'));
        if (!items.length) return;
        e.preventDefault();
        const idx = items.indexOf(document.activeElement);
        const delta = e.key === 'ArrowDown' ? 1 : items.length - 1;
        items[(Math.max(0, idx) + delta) % items.length].focus({ preventScroll: true });
      }
    };
    const onScroll = () => setOpenMenu(null);

    document.addEventListener('click', onDocClick);
    document.addEventListener('keydown', onKeyDown);
    window.addEventListener('scroll', onScroll, true);
    const first = menuPopRef.current?.querySelector('[role="menuitem"]');
    if (first) first.focus({ preventScroll: true });

    return () => {
      document.removeEventListener('click', onDocClick);
      document.removeEventListener('keydown', onKeyDown);
      window.removeEventListener('scroll', onScroll, true);
    };
  }, [openMenu]);

  // Dialog open/close sync
  useEffect(() => {
    const dlg = credDialogRef.current;
    if (!dlg) return;
    if (credAction && !dlg.open) dlg.showModal();
    if (!credAction && dlg.open) dlg.close();
  }, [credAction]);

  useEffect(() => {
    const dlg = enrollActDialogRef.current;
    if (!dlg) return;
    if (enrollAct && !dlg.open) dlg.showModal();
    if (!enrollAct && dlg.open) dlg.close();
  }, [enrollAct]);

  // Details drawer focus & Escape handling
  useEffect(() => {
    if (!detailsId) return undefined;
    detailsCloseBtnRef.current?.focus({ preventScroll: true });
    const onKeyDown = (e) => {
      if (e.key === 'Escape' && !credAction && !enrollAct) {
        setDetailsId(null);
      }
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [detailsId, credAction, enrollAct]);

  function openDetails(id, triggerEl) {
    detailsRestoreFocusRef.current = triggerEl || document.activeElement;
    setOpenMenu(null);
    setDetailsId(id);
  }

  function closeDetails() {
    setDetailsId(null);
    const el = detailsRestoreFocusRef.current;
    detailsRestoreFocusRef.current = null;
    if (el && typeof el.focus === 'function' && el.isConnected) {
      window.setTimeout(() => el.focus({ preventScroll: true }), 0);
    }
  }

  const filteredMachines = useMemo(() => {
    const q = filterQ.trim().toLowerCase();
    return machines.filter((m) => {
      if (filterRole !== 'all' && m.role !== filterRole) return false;
      if (filterState !== 'all' && m.state !== filterState) return false;
      if (!q) return true;
      const hay = [
        m.id,
        m.role,
        m.state,
        m.connect_mode,
        m.public_ip || '',
        m.primary_private_ip || '',
        m.note || '',
        ...(m.private_ips || []),
        ...(m.groups || []),
      ]
        .join(' ')
        .toLowerCase();
      return hay.includes(q);
    });
  }, [machines, filterQ, filterRole, filterState]);

  function toggleOnboard() {
    setOnboardOpen((was) => {
      const next = !was;
      if (next) window.setTimeout(() => onboardIpRef.current?.focus(), 0);
      return next;
    });
  }

  function toggleEnroll() {
    setEnrollOpen((was) => {
      const next = !was;
      if (next) window.setTimeout(() => enrollRoleRef.current?.focus(), 0);
      return next;
    });
  }

  async function refreshPublicIPOne(id) {
    setRefreshingIds((prev) => new Set(prev).add(id));
    const r = await api('POST', `/api/machines/${encodeURIComponent(id)}/refresh-public-ip`, {});
    setRefreshingIds((prev) => {
      const next = new Set(prev);
      next.delete(id);
      return next;
    });
    if (!r.ok) {
      pushToast(r.data.error || `Public IP refresh failed for ${id} (${r.status}).`);
      return;
    }
    pushToast(`Public IP refresh requested for ${id}.`);
    refresh();
  }

  async function refreshPublicIPsAll() {
    const online = machines.filter((m) => !m.removed && m.state === 'online');
    if (online.length === 0) {
      pushToast('No online machines to refresh.');
      return;
    }
    setBulkRefreshing(true);
    const results = await Promise.all(
      online.map(async (m) => ({
        id: m.id,
        res: await api('POST', `/api/machines/${encodeURIComponent(m.id)}/refresh-public-ip`, {}),
      }))
    );
    setBulkRefreshing(false);
    const failed = results.filter((x) => !x.res.ok);
    if (failed.length === 0) {
      pushToast(`Public IP refresh requested for ${online.length} online machine${online.length === 1 ? '' : 's'}.`);
    } else if (failed.length === results.length) {
      pushToast(failed[0].res.data.error || `Public IP refresh failed (${failed[0].res.status}).`);
    } else {
      pushToast(
        `Refreshed ${results.length - failed.length} of ${results.length} machines; ${failed.length} failed (${failed[0].id}).`
      );
    }
    refresh();
  }

  async function updateMachineOne(id) {
    setUpdatingIds((prev) => new Set(prev).add(id));
    const r = await api('POST', `/api/machines/${encodeURIComponent(id)}/update`, {});
    setUpdatingIds((prev) => {
      const next = new Set(prev);
      next.delete(id);
      return next;
    });
    if (!r.ok) {
      pushToast(r.data.error || `Agent update failed for ${id} (${r.status}).`);
      return;
    }
    if (r.data.status === 'noop') {
      pushToast(`${id} is already on version ${r.data.target_version}.`);
    } else {
      pushToast(`Update to ${r.data.target_version} started for ${id}.`);
    }
    refresh();
    if (detailsId === id) refreshDetails();
  }

  async function updateOutdatedAll() {
    const targets = machines.filter(
      (m) => !m.removed && m.state === 'online' && isAgentOutdated(m, serverVersion)
    );
    if (targets.length === 0) {
      pushToast('No online outdated agents to update.');
      return;
    }
    setBulkUpdating(true);
    const r = await api('POST', '/api/machines/update-outdated', {});
    setBulkUpdating(false);
    if (!r.ok) {
      pushToast(r.data.error || `Bulk agent update failed (${r.status}).`);
      return;
    }
    const dispatched = r.data.dispatched || 0;
    const failed = r.data.failed || 0;
    if (dispatched === 0 && failed === 0) {
      pushToast('All online agents are already up to date.');
    } else if (failed === 0) {
      pushToast(`Started agent update on ${dispatched} outdated machine${dispatched === 1 ? '' : 's'}.`);
    } else {
      pushToast(`Started update on ${dispatched} machine(s); ${failed} failed to dispatch.`);
    }
    refresh();
    if (detailsId) refreshDetails();
  }

  async function handleOnboardSubmit(e) {
    e.preventDefault();
    const useKey = onboardForm.auth === 'key';
    const body = {
      host: onboardForm.host.trim(),
      port: parseInt(onboardForm.port, 10) || 0,
      user: onboardForm.user.trim(),
      password: useKey ? '' : onboardForm.password,
      private_key: useKey ? onboardForm.private_key : '',
      role: onboardForm.role,
      connect_mode: onboardForm.connect_mode,
    };
    setOnboardBusy(true);
    setOnboardStatus({
      kind: 'info',
      msg: `Provisioning ${body.host}. This installs the agent over SSH and can take a minute.`,
    });
    const r = await api('POST', '/api/machines', body);
    setOnboardBusy(false);
    if (!r.ok) {
      setOnboardStatus({ kind: 'danger', msg: r.data.error || `Provisioning failed (${r.status})` });
      return;
    }
    const m = r.data.machine;
    setOnboardStatus({
      kind: 'ok',
      msg: `Provisioned ${m.id}${r.data.created ? ' (new machine)' : ' (existing machine)'}. It shows as online once its agent connects.`,
    });
    setOnboardForm((prev) => ({ ...prev, password: '', private_key: '' }));
    refresh();
  }

  async function handleEnrollSubmit(e) {
    e.preventDefault();
    setEnrollBusy(true);
    setEnrollStatus({ kind: 'info', msg: 'Generating...' });
    const r = await api('POST', '/api/enroll/tokens', { role: enrollRole });
    setEnrollBusy(false);
    setEnrollOutcome(null);
    if (!r.ok) {
      setEnrollResult(null);
      setEnrollStatus({
        kind: 'danger',
        msg: r.data.error || `Could not generate a token (${r.status})`,
      });
      return;
    }
    setEnrollStatus({
      kind: 'ok',
      msg: 'Token ready. It works once, for one machine, within 10 minutes.',
    });
    setEnrollResult(r.data);
  }

  // "Add another": retire the finished token and return to the generate form.
  function resetEnroll() {
    setEnrollResult(null);
    setEnrollOutcome(null);
    setEnrollStatus(null);
    window.setTimeout(() => enrollRoleRef.current?.focus(), 0);
  }

  function doneEnroll() {
    resetEnroll();
    setEnrollOpen(false);
  }

  function copyEnrollCmd(text) {
    if (!text) return;
    if (navigator.clipboard) {
      navigator.clipboard.writeText(text).then(
        () => pushToast('Copied.'),
        () => pushToast('Copy is blocked by the browser here; select the command and copy it.')
      );
    } else {
      pushToast('Copy is blocked by the browser here; select the command and copy it.');
    }
  }

  function openCredDialog(kind, machine) {
    setOpenMenu(null);
    setCredForm({ user: '', password: '', private_key: '', port: '22', skip_remote: false });
    setCredStatus(null);
    setCredAction({ kind, machine });
  }

  async function handleCredSubmit(e) {
    e.preventDefault();
    if (!credAction) return;
    const body = {
      user: credForm.user.trim(),
      password: credForm.password,
      private_key: credForm.private_key,
      port: parseInt(credForm.port, 10) || 0,
    };
    if (credAction.kind === 'remove') {
      body.skip_remote = credForm.skip_remote;
    }
    setCredBusy(true);
    setCredStatus({ kind: 'info', msg: 'Working...' });
    const url = `/api/machines/${encodeURIComponent(credAction.machine)}/${credAction.kind}`;
    const r = await api('POST', url, body);
    setCredBusy(false);
    if (!r.ok) {
      setCredStatus({ kind: 'danger', msg: r.data.error || `Failed (${r.status})` });
      return;
    }
    const doneMsg =
      credAction.kind === 'remove'
        ? `${credAction.machine} was removed.`
        : `${credAction.machine} was re-provisioned.`;
    setCredAction(null);
    pushToast(doneMsg);
    refresh();
  }

  function openEnrollActDialog(kind, machine) {
    setEnrollActStatus(null);
    setEnrollAct({ kind, machine });
  }

  async function handleEnrollActSubmit(e) {
    e.preventDefault();
    if (!enrollAct) return;
    setEnrollActBusy(true);
    enrollBusyRef.current = true;
    setEnrollActStatus({ kind: 'info', msg: 'Working...' });
    const url = `/api/enroll/machines/${encodeURIComponent(enrollAct.machine)}/${enrollAct.kind}`;
    const r = await api('POST', url, {});
    setEnrollActBusy(false);
    enrollBusyRef.current = false;
    if (!r.ok) {
      setEnrollActStatus({ kind: 'danger', msg: r.data.error || `Failed (${r.status})` });
      return;
    }
    const doneMsg =
      enrollAct.kind === 'stop'
        ? `${enrollAct.machine} was stopped.`
        : `${enrollAct.machine} was detached and its certificate revoked.`;
    setEnrollAct(null);
    pushToast(doneMsg);
    refreshEnrolled();
    refresh();
  }

  function toggleRowMenu(id, btnEl) {
    if (openMenu && openMenu.id === id) {
      setOpenMenu(null);
      return;
    }
    const rect = btnEl.getBoundingClientRect();
    const popW = 176;
    const popH = 120;
    setOpenMenu({
      id,
      top: Math.min(rect.bottom + 4, window.innerHeight - popH - 8),
      left: Math.max(8, rect.right - popW),
    });
  }

  const workersNote = summary.workers
    ? summary.workers_online === summary.workers
      ? 'All connected'
      : `${summary.workers - summary.workers_online} not connected`
    : 'None registered';

  const monitorsNote = summary.monitors
    ? summary.monitors_online === summary.monitors
      ? 'All connected'
      : `${summary.monitors - summary.monitors_online} not connected`
    : 'None registered';

  const needsAttention = summary.needs_attention || [];
  const diskWarn = summary.disk_warn || [];
  const diskCritical = summary.disk_critical || [];
  const diskAlertParts = [];
  if (diskWarn.length) diskAlertParts.push(`warning: ${diskWarn.join(', ')}`);
  if (diskCritical.length) diskAlertParts.push(`intake paused: ${diskCritical.join(', ')}`);
  const hasDiskReports = machines.some((m) => !m.removed && (m.disk?.total_bytes > 0 || Boolean(m.disk?.status)));
  const outdatedOnlineCount = machines.filter(
    (m) => !m.removed && m.state === 'online' && isAgentOutdated(m, serverVersion)
  ).length;

  const activeMenuMachine = openMenu ? machines.find((m) => m.id === openMenu.id) : null;

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Machines</h1>
          <p>
            Registry of worker agents and monitor agents: online state, role, connection mode, addresses and disk.
            Live: the state updates as agents connect and drop.
          </p>
        </div>
        <div className="page-head__actions">
          <button
            className="btn"
            type="button"
            disabled={bulkUpdating || noBackend}
            title={serverVersion ? `Update online agents not on ${serverVersion}` : 'Update outdated agents'}
            onClick={updateOutdatedAll}
          >
            <svg className="icon" aria-hidden="true"><use href="#i-upload" /></svg>
            {bulkUpdating
              ? 'Updating agents...'
              : outdatedOnlineCount > 0
                ? `Update outdated (${outdatedOnlineCount})`
                : 'Update outdated'}
          </button>
          <button
            className="btn"
            type="button"
            disabled={bulkRefreshing || noBackend}
            onClick={refreshPublicIPsAll}
          >
            <svg className="icon" aria-hidden="true"><use href="#i-refresh" /></svg>
            {bulkRefreshing ? 'Refreshing public IPs...' : 'Refresh public IPs'}
          </button>
          <button
            className="btn"
            type="button"
            disabled={enrollEnabled === false}
            title={
              enrollEnabled === false
                ? 'Token enrollment is not enabled on this server (set server.enroll_addr).'
                : undefined
            }
            aria-expanded={enrollOpen}
            aria-controls="enroll"
            onClick={toggleEnroll}
          >
            <svg className="icon" aria-hidden="true"><use href="#i-lock" /></svg>
            Add agent (token)
          </button>
          <button
            className="btn btn--primary"
            type="button"
            aria-expanded={onboardOpen}
            aria-controls="onboard"
            onClick={toggleOnboard}
          >
            <svg className="icon" aria-hidden="true"><use href="#i-plus" /></svg>
            Onboard machine
          </button>
        </div>
      </div>

      {noBackend && (
        <div className="alert alert--warn" role="status" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
          <div>The panel is running without a backend, so the registry is empty and nothing can be provisioned.</div>
        </div>
      )}

      {loadError && (
        <div className="alert alert--danger" role="status" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
          <div>{loadError}</div>
        </div>
      )}

      {/* Onboarding form (SSH): IP + SSH user + password/key + role */}
      <section className="card panel" id="onboard" hidden={!onboardOpen} aria-labelledby="onboard-title" style={{ marginBottom: 'var(--sp-4)' }}>
        <div className="card__head">
          <div>
            <h2 id="onboard-title">Onboard a machine</h2>
            <p>
              Provisioning installs <code>fleet-agent</code>, creates its client certificate and writes the connect mode.
            </p>
          </div>
        </div>
        <form className="card__body stack" id="onboard-form" onSubmit={handleOnboardSubmit}>
          <div className="form-grid">
            <div className="field">
              <label htmlFor="f-ip">Machine IP address</label>
              <input
                ref={onboardIpRef}
                className="input input--mono"
                id="f-ip"
                name="host"
                inputMode="decimal"
                placeholder="10.20.0.30"
                required
                value={onboardForm.host}
                onChange={(e) => setOnboardForm((p) => ({ ...p, host: e.target.value }))}
              />
              <span className="field__hint">Reachable from the main server.</span>
            </div>

            <div className="field">
              <label htmlFor="f-port">SSH port</label>
              <input
                className="input input--mono"
                id="f-port"
                name="port"
                inputMode="numeric"
                value={onboardForm.port}
                onChange={(e) => setOnboardForm((p) => ({ ...p, port: e.target.value }))}
              />
            </div>

            <div className="field">
              <label htmlFor="f-user">SSH username</label>
              <input
                className="input"
                id="f-user"
                name="user"
                autoComplete="off"
                placeholder="provision"
                required
                value={onboardForm.user}
                onChange={(e) => setOnboardForm((p) => ({ ...p, user: e.target.value }))}
              />
            </div>

            <fieldset className="field">
              <legend>Authentication</legend>
              <div className="seg" role="radiogroup" aria-label="Authentication method">
                <label>
                  <input
                    type="radio"
                    name="auth"
                    value="password"
                    checked={onboardForm.auth === 'password'}
                    onChange={() => setOnboardForm((p) => ({ ...p, auth: 'password' }))}
                  />
                  <span>Password</span>
                </label>
                <label>
                  <input
                    type="radio"
                    name="auth"
                    value="key"
                    checked={onboardForm.auth === 'key'}
                    onChange={() => setOnboardForm((p) => ({ ...p, auth: 'key' }))}
                  />
                  <span>Private key</span>
                </label>
              </div>
            </fieldset>

            <div className="field" hidden={onboardForm.auth === 'key'}>
              <label htmlFor="f-secret">SSH password</label>
              <input
                className="input"
                id="f-secret"
                name="password"
                type="password"
                autoComplete="new-password"
                value={onboardForm.password}
                onChange={(e) => setOnboardForm((p) => ({ ...p, password: e.target.value }))}
              />
              <span className="field__hint">Used once for provisioning, then discarded.</span>
            </div>

            <div className="field field--wide" hidden={onboardForm.auth !== 'key'}>
              <label htmlFor="f-key">SSH private key (PEM)</label>
              <textarea
                className="textarea input--mono"
                id="f-key"
                name="private_key"
                rows={4}
                spellCheck={false}
                autoComplete="off"
                value={onboardForm.private_key}
                onChange={(e) => setOnboardForm((p) => ({ ...p, private_key: e.target.value }))}
              />
              <span className="field__hint">Used once for provisioning, then discarded. Never stored or logged.</span>
            </div>

            <div className="field">
              <label htmlFor="f-role">Role</label>
              <select
                className="select"
                id="f-role"
                name="role"
                value={onboardForm.role}
                onChange={(e) => setOnboardForm((p) => ({ ...p, role: e.target.value }))}
              >
                <option value="worker">Worker agent</option>
                <option value="monitor">Monitor agent</option>
              </select>
              <span className="field__hint">Monitor agents never run commands or shells.</span>
            </div>

            <div className="field">
              <label htmlFor="f-mode">Connect mode</label>
              <select
                className="select"
                id="f-mode"
                name="connect_mode"
                value={onboardForm.connect_mode}
                onChange={(e) => setOnboardForm((p) => ({ ...p, connect_mode: e.target.value }))}
              >
                <option value="server_dial">server_dial (default)</option>
                <option value="agent_dial">agent_dial</option>
              </select>
              <span className="field__hint"><code>agent_dial</code> is for agents on another network.</span>
            </div>
          </div>

          <StatusAlert status={onboardStatus} />

          <div className="row">
            <button className="btn btn--primary" type="submit" disabled={onboardBusy}>
              Start provisioning
            </button>
            <button className="btn" type="button" onClick={() => setOnboardOpen(false)}>
              Cancel
            </button>
          </div>
        </form>
      </section>

      {/* Token enrollment (T-0026): SSH-free, the machine dials out */}
      <section className="card panel" id="enroll" hidden={!enrollOpen} aria-labelledby="enroll-title" style={{ marginBottom: 'var(--sp-4)' }}>
        <div className="card__head">
          <div>
            <h2 id="enroll-title">Add an agent with a token</h2>
            <p>
              No SSH needed. Generate a one-time code, then run the one-liner as root on the machine.
              The machine creates its own key, gets a certificate, installs a permanent service and dials out to this server.
            </p>
          </div>
        </div>

        <form className="card__body stack" id="enroll-form" data-enroll-form onSubmit={handleEnrollSubmit}>
          <div className="form-grid">
            <div className="field">
              <label htmlFor="e-role">Role</label>
              <select
                ref={enrollRoleRef}
                className="select"
                id="e-role"
                name="role"
                value={enrollRole}
                onChange={(e) => setEnrollRole(e.target.value)}
              >
                <option value="worker">Worker agent (Linux)</option>
                <option value="monitor">Monitor agent (Linux or macOS)</option>
              </select>
              <span className="field__hint">
                macOS machines can only be monitor agents: evidence capture and commands are Linux-only.
              </span>
            </div>
          </div>

          <StatusAlert status={enrollStatus} />

          <div className="row">
            <button className="btn btn--primary" type="submit" disabled={enrollBusy}>
              Generate token
            </button>
            <button className="btn" type="button" onClick={() => setEnrollOpen(false)}>
              Close
            </button>
          </div>
        </form>

        {enrollOutcome?.state === 'used' && (() => {
          const em = enrolledMachines.find((x) => x.id === enrollOutcome.machineId);
          const ips = em ? [em.public_ip, ...(em.private_ips || [])].filter(Boolean) : [];
          return (
            <div className="card__body stack" data-enroll-success role="status" aria-live="polite">
              <div>
                <span className="chip chip--ok">Enrolled</span>{' '}
                <strong>{enrollOutcome.machineId}</strong> joined the fleet.
              </div>
              <div className="stack" style={{ gap: 'var(--sp-1)' }}>
                <div><span className="muted">Machine id:</span> {enrollOutcome.machineId}</div>
                <div><span className="muted">Role:</span> {em ? em.role : enrollRole}</div>
                {em?.specs && <div><span className="muted">Specs:</span> {specText(em.specs)}</div>}
                {ips.length > 0 && <div><span className="muted">IP:</span> {ips.join(', ')}</div>}
              </div>
              <p className="muted">The one-time code has been used and is no longer valid.</p>
              <div className="row">
                <button className="btn btn--primary" type="button" data-enroll-done onClick={doneEnroll}>Done</button>
                <button className="btn" type="button" data-enroll-another onClick={resetEnroll}>Add another</button>
              </div>
            </div>
          );
        })()}

        {enrollOutcome?.state === 'expired' && (
          <div className="card__body stack" data-enroll-expired role="status" aria-live="polite">
            <div><span className="chip chip--warn">Expired</span> The code expired before any machine used it.</div>
            <div className="row">
              <button className="btn btn--primary" type="button" data-enroll-another onClick={resetEnroll}>Generate a new token</button>
              <button className="btn" type="button" onClick={doneEnroll}>Close</button>
            </div>
          </div>
        )}

        <div className="card__body stack" data-enroll-result hidden={!enrollResult || Boolean(enrollOutcome)}>
          <div>
            <div className="muted">Code (valid for 10 minutes, one machine, one use)</div>
            <div className="enroll-code" data-enroll-code>{enrollResult?.code || ''}</div>
            <div className="muted" data-enroll-expiry aria-live="polite">{enrollExpiryText}</div>
          </div>

          <div className="stack" style={{ gap: 'var(--sp-2)' }} data-enroll-os="linux">
            <div className="row">
              <strong>Linux</strong>
              <button
                className="btn btn--sm"
                type="button"
                data-enroll-copy="linux"
                onClick={() => copyEnrollCmd(enrollResult?.linux)}
              >
                <svg className="icon" aria-hidden="true"><use href="#i-copy" /></svg>
                Copy
              </button>
            </div>
            <pre className="codeblock" data-enroll-cmd="linux" tabIndex={0}>{enrollResult?.linux || ''}</pre>
          </div>

          <div className="stack" style={{ gap: 'var(--sp-2)' }} data-enroll-os="macos" hidden={!enrollResult?.macos}>
            <div className="row">
              <strong>macOS</strong> <span className="muted">(monitor agents only)</span>
              <button
                className="btn btn--sm"
                type="button"
                data-enroll-copy="macos"
                onClick={() => copyEnrollCmd(enrollResult?.macos)}
              >
                <svg className="icon" aria-hidden="true"><use href="#i-copy" /></svg>
                Copy
              </button>
            </div>
            <pre className="codeblock" data-enroll-cmd="macos" tabIndex={0}>{enrollResult?.macos || ''}</pre>
          </div>

          <p className="muted">
            The command pins this server&apos;s TLS key, so it cannot be intercepted on the way. It installs a service
            that starts at boot and restarts itself.{' '}
            <a
              href={`/api/enroll/script?role=${encodeURIComponent(enrollRole)}`}
              data-enroll-script
              target="_blank"
              rel="noopener noreferrer"
            >
              Show script
            </a>{' '}
            to read exactly what it does before running it.
          </p>
        </div>
      </section>

      {/* Summary tiles */}
      <section className="grid" id="machines-tiles" aria-label="Fleet summary" style={{ marginBottom: 'var(--sp-4)' }}>
        <div className="card stat col-3 col-3--md-6">
          <div className="stat__label">Worker agents</div>
          <div className="stat__value">
            {summary.workers_online} <small>/ {summary.workers} online</small>
          </div>
          <div className="stat__note">{workersNote}</div>
        </div>

        <div className="card stat col-3 col-3--md-6">
          <div className="stat__label">Monitor agents</div>
          <div className="stat__value">
            {summary.monitors_online} <small>/ {summary.monitors} online</small>
          </div>
          <div className="stat__note">{monitorsNote}</div>
        </div>

        <div className="card stat col-3 col-3--md-6">
          <div className="stat__label">Needs attention</div>
          <div className="stat__value">{needsAttention.length}</div>
          <div className="stat__note">{needsAttention.length ? needsAttention.join(', ') : 'Nothing to do'}</div>
        </div>

        <div className="card stat col-3 col-3--md-6">
          <div className="stat__label">Disk guard</div>
          <div className="stat__value">{diskWarn.length + diskCritical.length}</div>
          <div className="stat__note">
            {diskAlertParts.length ? diskAlertParts.join('; ') : hasDiskReports ? 'All reported disks OK' : 'No disk reports yet'}
          </div>
        </div>
      </section>

      {/* Registry */}
      <section className="card" aria-labelledby="reg-title">
        <div className="card__head">
          <h2 id="reg-title">Registry</h2>
          <span className="muted" data-filter-count aria-live="polite">
            Showing {filteredMachines.length} of {machines.length} machines
          </span>
        </div>

        <div className="table-toolbar" role="search">
          <div className="input-affix">
            <svg className="icon" aria-hidden="true"><use href="#i-search" /></svg>
            <input
              className="input"
              type="search"
              data-filter-q
              placeholder="Filter by ID or IP"
              aria-label="Filter machines by ID or IP"
              value={filterQ}
              onChange={(e) => setFilterQ(e.target.value)}
            />
          </div>

          <div className="seg" role="radiogroup" aria-label="Filter by role">
            {[
              ['all', 'All'],
              ['worker', 'Worker agents'],
              ['monitor', 'Monitor agents'],
            ].map(([val, lbl]) => (
              <label key={val}>
                <input
                  type="radio"
                  name="role"
                  value={val}
                  checked={filterRole === val}
                  onChange={() => setFilterRole(val)}
                />
                <span>{lbl}</span>
              </label>
            ))}
          </div>

          <select
            className="select"
            data-filter-state
            aria-label="Filter by state"
            style={{ width: 'auto' }}
            value={filterState}
            onChange={(e) => setFilterState(e.target.value)}
          >
            <option value="all">Any state</option>
            <option value="online">Online</option>
            <option value="reconnecting">Reconnecting</option>
            <option value="offline">Offline</option>
          </select>
        </div>

        <div className="table-wrap">
          <table className="table table--sticky-first" data-live-table>
            <caption className="sr-only">Registered machines</caption>
            <thead>
              <tr>
                <th scope="col">Machine</th>
                <th scope="col">Role</th>
                <th scope="col">State</th>
                <th scope="col">Version</th>
                <th scope="col">Connect mode</th>
                <th scope="col">Addresses</th>
                <th scope="col">Disk</th>
                <th scope="col">Last seen</th>
                <th scope="col"><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody id="machines-rows">
              {machines.length === 0 ? (
                <tr data-empty-row>
                  <td colSpan={9} className="empty">
                    No machines are registered yet. Use <strong>Onboard machine</strong> to add the first one.
                  </td>
                </tr>
              ) : (
                filteredMachines.map((m) => {
                  const rawDisk = diskView(m.disk);
                  const diskStatus = m.disk?.status || '';
                  const disk = rawDisk
                    ? {
                        ...rawDisk,
                        className:
                          diskStatus === 'critical'
                            ? 'danger'
                            : diskStatus === 'warn'
                              ? 'warn'
                              : rawDisk.className,
                        word:
                          diskStatus === 'critical'
                            ? 'intake paused'
                            : diskStatus === 'warn'
                              ? 'warning'
                              : rawDisk.word,
                      }
                    : null;
                  const privateShown = m.primary_private_ip
                    ? [m.primary_private_ip]
                    : m.private_ips || [];
                  const termSearch = `?machine=${encodeURIComponent(m.id)}`;
                  const termHref = routeHref('terminal', termSearch);
                  const rowRefreshing = refreshingIds.has(m.id);

                  return (
                    <tr
                      key={m.id}
                      data-id={m.id}
                      data-role={m.role}
                      data-state={m.state}
                      data-removed={m.removed ? '' : undefined}
                      aria-selected={detailsId === m.id ? 'true' : undefined}
                    >
                      <th scope="row" className="cell-primary mono">
                        <button
                          type="button"
                          onClick={(e) => openDetails(m.id, e.currentTarget)}
                          title={`View details for ${m.id}`}
                          style={{
                            background: 'none',
                            border: 0,
                            padding: 0,
                            font: 'inherit',
                            color: 'inherit',
                            cursor: 'pointer',
                            textAlign: 'left',
                          }}
                        >
                          <MachineLabel machine={m} variant="compact" />
                        </button>
                        {m.removed && (
                          <>
                            {' '}
                            <span className="chip chip--outline">Decommissioned</span>
                          </>
                        )}
                        {Array.isArray(m.groups) && m.groups.length > 0 && (
                          <div className="row" style={{ gap: 'var(--sp-1)', marginTop: 'var(--sp-1)', flexWrap: 'wrap' }}>
                            {m.groups.map((g) => (
                              <span key={g} className="chip chip--outline chip--mono">
                                {g}
                              </span>
                            ))}
                          </div>
                        )}
                        {m.note && <div className="cell-sub">{m.note}</div>}
                      </th>
                      <td><RoleChip role={m.role} /></td>
                      <td><StateChip state={m.state} /></td>
                      <td><VersionCell machine={m} serverVersion={serverVersion} /></td>
                      <td className="mono">{m.connect_mode}</td>
                      <td>
                        {privateShown.length > 0 ? (
                          <div>
                            Private <span className="mono">{privateShown.join(', ')}</span>
                            {m.public_ip && (
                              <>
                                {' · Public '}
                                <span className="mono">{m.public_ip}</span>
                                {m.public_ip_at ? ` · ${fmtAgo(m.public_ip_at)}` : ''}
                              </>
                            )}
                          </div>
                        ) : (
                          <div className="faint">No address</div>
                        )}
                        {(privateShown.length === 0 || !m.public_ip) && (
                          <div className="cell-sub">
                            {m.public_ip ? (
                              <>
                                Public <span className="mono">{m.public_ip}</span>
                                {m.public_ip_at ? ` · ${fmtAgo(m.public_ip_at)}` : ''}
                              </>
                            ) : (
                              'Public IP not checked yet'
                            )}
                          </div>
                        )}
                      </td>
                      <td>
                        {disk ? (
                          <div
                            className={`meter${disk.className ? ` meter--${disk.className}` : ''}`}
                            title={m.disk?.updated_at ? `Updated ${fmtAgo(m.disk.updated_at)}` : undefined}
                          >
                            <div className="meter__bar">
                              <div className="meter__fill" style={{ width: `${disk.pct}%` }} />
                            </div>
                            <div className="meter__text">
                              <span>{disk.used} / {disk.total}</span>
                              <span>{disk.pct}%{disk.word ? ` · ${disk.word}` : ''}</span>
                            </div>
                          </div>
                        ) : diskStatus ? (
                          <span className={diskStatus === 'critical' ? 'chip chip--danger' : diskStatus === 'warn' ? 'chip chip--warn' : 'chip chip--ok'}>
                            {diskStatus === 'critical' ? 'intake paused' : diskStatus === 'warn' ? 'warning' : 'ok'}
                          </span>
                        ) : (
                          <span className="faint">Unknown</span>
                        )}
                      </td>
                      <td className="nowrap">{m.last_seen ? fmtAgo(m.last_seen) : 'never'}</td>
                      <td className="actions">
                        {!m.removed && (
                          <>
                            {m.state === 'online' && isAgentOutdated(m, serverVersion) && (
                              <button
                                className="icon-btn icon-btn--sm"
                                type="button"
                                aria-label={`Update agent on ${m.id}`}
                                title={`Update agent to ${serverVersion}`}
                                disabled={updatingIds.has(m.id) || isAgentUpdateInProgress(m.agent_update)}
                                onClick={() => updateMachineOne(m.id)}
                              >
                                <svg className="icon" aria-hidden="true"><use href="#i-upload" /></svg>
                              </button>
                            )}
                            <button
                              className="icon-btn icon-btn--sm"
                              type="button"
                              aria-label={`Refresh public IP of ${m.id}`}
                              title="Refresh public IP"
                              disabled={rowRefreshing}
                              onClick={() => refreshPublicIPOne(m.id)}
                            >
                              <svg className="icon" aria-hidden="true"><use href="#i-refresh" /></svg>
                            </button>
                            {m.role === 'worker' && m.state === 'online' ? (
                              isRoutePorted('terminal') ? (
                                <Link
                                  className="icon-btn icon-btn--sm"
                                  to={termHref}
                                  aria-label={`Open terminal on ${m.id}`}
                                  title="Open terminal"
                                >
                                  <svg className="icon" aria-hidden="true"><use href="#i-terminal" /></svg>
                                </Link>
                              ) : (
                                <a
                                  className="icon-btn icon-btn--sm"
                                  href={termHref}
                                  aria-label={`Open terminal on ${m.id}`}
                                  title="Open terminal"
                                >
                                  <svg className="icon" aria-hidden="true"><use href="#i-terminal" /></svg>
                                </a>
                              )
                            ) : m.role === 'monitor' ? (
                              <span
                                className="icon-btn icon-btn--sm"
                                role="link"
                                aria-disabled="true"
                                aria-label="Terminal unavailable: monitor agents have no shell"
                                title="Monitor agents have no shell"
                              >
                                <svg className="icon" aria-hidden="true"><use href="#i-terminal" /></svg>
                              </span>
                            ) : (
                              <span
                                className="icon-btn icon-btn--sm"
                                role="link"
                                aria-disabled="true"
                                aria-label={`Terminal unavailable: ${m.id} is ${m.state}`}
                                title={`Terminal unavailable: machine is ${m.state}`}
                              >
                                <svg className="icon" aria-hidden="true"><use href="#i-terminal" /></svg>
                              </span>
                            )}
                          </>
                        )}
                        <div className="menu">
                          <button
                            className="icon-btn icon-btn--sm"
                            type="button"
                            aria-label={`More actions for ${m.id}`}
                            title="More"
                            aria-haspopup="menu"
                            aria-expanded={openMenu?.id === m.id}
                            data-menu-btn
                            onClick={(e) => toggleRowMenu(m.id, e.currentTarget)}
                          >
                            <svg className="icon" aria-hidden="true"><use href="#i-more" /></svg>
                          </button>
                        </div>
                      </td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>

        <p className="empty" data-filter-empty hidden={machines.length === 0 || filteredMachines.length > 0}>
          No machines match the current filter.
        </p>

        <div className="card__foot">
          Live registry. State is derived from the control channel; <em>Reconnecting</em> means the agent was connected
          since the server started and dropped. Public IP is cached from the last reflector check. Disk guard: 80% raises
          a warning, 90% pauses capture intake (nothing is deleted). Disk figures update live from agent telemetry.
        </div>
      </section>

      {/* Fixed-position row action menu (outside overflow-x table wrapper) */}
      {openMenu && activeMenuMachine && (
        <div
          ref={menuPopRef}
          className="menu__pop"
          role="menu"
          style={{ top: `${openMenu.top}px`, left: `${openMenu.left}px` }}
        >
          <button
            className="menu__item"
            role="menuitem"
            type="button"
            onClick={() => openDetails(activeMenuMachine.id)}
          >
            Details &amp; groups
          </button>
          {!activeMenuMachine.removed && (
            <button
              className="menu__item"
              role="menuitem"
              type="button"
              disabled={
                activeMenuMachine.state !== 'online' ||
                !isAgentOutdated(activeMenuMachine, serverVersion) ||
                updatingIds.has(activeMenuMachine.id) ||
                isAgentUpdateInProgress(activeMenuMachine.agent_update)
              }
              onClick={() => {
                const id = activeMenuMachine.id;
                setOpenMenu(null);
                updateMachineOne(id);
              }}
            >
              Update agent
            </button>
          )}
          <button
            className="menu__item"
            role="menuitem"
            type="button"
            onClick={() => openCredDialog('reprovision', activeMenuMachine.id)}
          >
            Re-provision
          </button>
          {!activeMenuMachine.removed && (
            <button
              className="menu__item menu__item--danger"
              role="menuitem"
              type="button"
              onClick={() => openCredDialog('remove', activeMenuMachine.id)}
            >
              Remove
            </button>
          )}
        </div>
      )}

      {/* Token-enrolled agents */}
      <section
        className="card"
        id="enrolled"
        aria-labelledby="enrolled-title"
        style={{ marginTop: 'var(--sp-4)' }}
        hidden={enrollEnabled !== true}
      >
        <div className="card__head">
          <h2 id="enrolled-title">Token-enrolled agents</h2>
          <span className="muted" data-enrolled-count aria-live="polite">
            {enrolledMachines.length} {enrolledMachines.length === 1 ? 'agent' : 'agents'}
          </span>
        </div>

        <div className="table-wrap">
          <table className="table" data-enrolled-table>
            <caption className="sr-only">Agents enrolled with a token</caption>
            <thead>
              <tr>
                <th scope="col">Machine</th>
                <th scope="col">Role</th>
                <th scope="col">State</th>
                <th scope="col">Specs</th>
                <th scope="col"><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody id="enrolled-rows">
              {enrolledMachines.map((m) => (
                <tr key={m.id} data-id={m.id}>
                  <th scope="row">{m.id}</th>
                  <td>{m.role === 'monitor' ? 'Monitor' : 'Worker'}</td>
                  <td>
                    {m.stopped ? (
                      <span className="chip chip--warn">
                        <svg className="icon icon--sm" aria-hidden="true"><use href="#i-pause" /></svg>
                        Stopped
                      </span>
                    ) : m.online ? (
                      <span className="chip chip--ok chip--live">
                        <svg className="icon icon--sm" aria-hidden="true"><use href="#i-check" /></svg>
                        Online
                      </span>
                    ) : (
                      <span className="chip chip--outline">
                        <svg className="icon icon--sm" aria-hidden="true"><use href="#i-clock" /></svg>
                        Offline
                      </span>
                    )}
                  </td>
                  <td className="muted">{specText(m.specs)}</td>
                  <td>
                    <button
                      className="btn btn--sm"
                      type="button"
                      data-enroll-act="stop"
                      data-machine={m.id}
                      onClick={() => openEnrollActDialog('stop', m.id)}
                    >
                      Stop
                    </button>{' '}
                    <button
                      className="btn btn--sm btn--danger"
                      type="button"
                      data-enroll-act="detach"
                      data-machine={m.id}
                      onClick={() => openEnrollActDialog('detach', m.id)}
                    >
                      Detach
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        <p className="empty" data-enrolled-empty hidden={enrolledMachines.length !== 0}>
          No agents were enrolled with a token yet.
        </p>

        <div className="card__foot">
          <strong>Stop</strong> disables the service on the machine: it stays down through a reboot until someone runs{' '}
          <code>sudo fleet-agent start</code> there. <strong>Detach</strong> revokes the machine&apos;s certificate and
          forgets it; adding it again needs a fresh token.
        </div>
      </section>

      {/* Stop / Detach confirmation dialog */}
      <dialog
        ref={enrollActDialogRef}
        className="dialog"
        id="dlg-enroll-act"
        aria-labelledby="dlg-enroll-act-title"
        onClose={() => setEnrollAct(null)}
      >
        <form id="enroll-act-form" onSubmit={handleEnrollActSubmit}>
          <div className="dialog__head">
            <h2 id="dlg-enroll-act-title">
              {enrollAct ? `${enrollAct.kind === 'stop' ? 'Stop' : 'Detach'} ${enrollAct.machine}` : 'Stop agent'}
            </h2>
            <p>
              {enrollAct?.kind === 'stop'
                ? 'This disables the agent service on the machine: it stays down through a reboot. Someone must run "sudo fleet-agent start" on the machine to bring it back. The machine stays in the registry.'
                : "This revokes the machine's certificate and takes it out of the registry. The agent can no longer connect. To add the machine again you need a fresh token."}
            </p>
          </div>
          <div className="dialog__body">
            <StatusAlert status={enrollActStatus} />
          </div>
          <div className="dialog__foot">
            <button className="btn" type="button" onClick={() => setEnrollAct(null)}>
              Cancel
            </button>
            <button
              className={`btn ${enrollAct?.kind === 'stop' ? 'btn--primary' : 'btn--danger'}`}
              type="submit"
              disabled={enrollActBusy}
            >
              {enrollAct?.kind === 'stop' ? 'Stop agent' : 'Detach machine'}
            </button>
          </div>
        </form>
      </dialog>

      {/* Credentials for re-provision / remove (never stored) */}
      <dialog
        ref={credDialogRef}
        className="dialog"
        id="dlg-cred"
        aria-labelledby="dlg-cred-title"
        onClose={() => setCredAction(null)}
      >
        <form id="cred-form" onSubmit={handleCredSubmit}>
          <div className="dialog__head">
            <h2 id="dlg-cred-title">
              {credAction
                ? `${credAction.kind === 'remove' ? 'Remove' : 'Re-provision'} ${credAction.machine}`
                : 'Re-provision machine'}
            </h2>
            <p>
              {credAction?.kind === 'remove'
                ? 'Removing stops the agent over SSH, decommissions the machine and records its client certificate for revocation. The record stays in the registry.'
                : 'Re-provisioning is idempotent: same machine ID, same identity; a certificate is issued only if missing. Credentials are used once and never stored.'}
            </p>
          </div>
          <div className="dialog__body">
            <div className="field">
              <label htmlFor="c-user">SSH username</label>
              <input
                className="input"
                id="c-user"
                name="user"
                autoComplete="off"
                required={!credForm.skip_remote}
                value={credForm.user}
                onChange={(e) => setCredForm((p) => ({ ...p, user: e.target.value }))}
              />
            </div>
            <div className="field">
              <label htmlFor="c-secret">SSH password</label>
              <input
                className="input"
                id="c-secret"
                name="password"
                type="password"
                autoComplete="new-password"
                value={credForm.password}
                onChange={(e) => setCredForm((p) => ({ ...p, password: e.target.value }))}
              />
            </div>
            <div className="field">
              <label htmlFor="c-key">or SSH private key (PEM)</label>
              <textarea
                className="textarea input--mono"
                id="c-key"
                name="private_key"
                rows={3}
                spellCheck={false}
                autoComplete="off"
                value={credForm.private_key}
                onChange={(e) => setCredForm((p) => ({ ...p, private_key: e.target.value }))}
              />
            </div>
            <div className="field">
              <label htmlFor="c-port">SSH port</label>
              <input
                className="input input--mono"
                id="c-port"
                name="port"
                inputMode="numeric"
                value={credForm.port}
                onChange={(e) => setCredForm((p) => ({ ...p, port: e.target.value }))}
              />
            </div>
            <label className="check" hidden={credAction?.kind !== 'remove'}>
              <input
                type="checkbox"
                name="skip_remote"
                checked={credForm.skip_remote}
                onChange={(e) => setCredForm((p) => ({ ...p, skip_remote: e.target.checked }))}
              />
              <span>
                Machine is unreachable: decommission in the registry only (skip stopping the agent over SSH)
              </span>
            </label>
            <StatusAlert status={credStatus} />
          </div>
          <div className="dialog__foot">
            <button className="btn" type="button" onClick={() => setCredAction(null)}>
              Cancel
            </button>
            <button
              className={`btn ${credAction?.kind === 'remove' ? 'btn--danger' : 'btn--primary'}`}
              type="submit"
              disabled={credBusy}
            >
              {credAction?.kind === 'remove' ? 'Remove machine' : 'Re-provision'}
            </button>
          </div>
        </form>
      </dialog>

      <MachineDetailsDrawer
        machineId={detailsId}
        machine={detailsMachine}
        watchdog={detailsWatchdog}
        serverVersion={detailsServerVersion || serverVersion}
        loading={detailsLoading}
        error={detailsError}
        notFound={detailsNotFound}
        closeBtnRef={detailsCloseBtnRef}
        onClose={closeDetails}
        updating={Boolean(detailsId && updatingIds.has(detailsId))}
        onUpdateAgent={(id) => updateMachineOne(id)}
        onGroupsSaved={(groups) => {
          pushToast(
            groups.length
              ? `Updated watchdog groups for ${detailsId}: ${groups.join(', ')}.`
              : `Cleared watchdog groups for ${detailsId}.`
          );
          refresh();
          refreshDetails();
        }}
      />
    </>
  );
}
