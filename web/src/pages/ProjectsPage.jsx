import { useEffect, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { StatusAlert } from '../components/StatusAlert';
import { useProjects } from '../hooks/useProjects';
import { fmtDateTime, fmtDur, fmtTime } from '../lib/fmt';
import { usePageTitle } from '../lib/title';
import { useToast } from '../lib/toast';

function formatTestDateKey(test) {
  const iso = test.ended_at || test.started_at || test.updated_at || test.created_at;
  if (!iso) return 'Unknown date';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return 'Unknown date';
  return d.toISOString().slice(0, 10);
}

function groupCompletedByDate(tests, filter) {
  const byDate = {};
  for (const t of tests) {
    if (t.status !== 'completed' && t.status !== 'failed') continue;
    if (filter === 'completed' && t.status !== 'completed') continue;
    if (filter === 'failed' && t.status !== 'failed') continue;
    const key = formatTestDateKey(t);
    if (!byDate[key]) byDate[key] = [];
    byDate[key].push(t);
  }
  return Object.keys(byDate)
    .sort((a, b) => b.localeCompare(a))
    .map((date) => ({
      date,
      tests: byDate[date].sort((a, b) => {
        const ta = new Date(a.ended_at || a.started_at || a.created_at).getTime();
        const tb = new Date(b.ended_at || b.started_at || b.created_at).getTime();
        return tb - ta;
      }),
    }));
}

function formatLastDate(tests) {
  if (!Array.isArray(tests) || tests.length === 0) return 'no tests';
  let latest = 0;
  for (const t of tests) {
    const ms = new Date(t.ended_at || t.started_at || t.updated_at || t.created_at).getTime();
    if (!Number.isNaN(ms) && ms > latest) latest = ms;
  }
  if (!latest) return `${tests.length} tests`;
  return `last ${new Date(latest).toISOString().slice(0, 10)}`;
}

function parseMetadataLines(raw) {
  const out = {};
  for (const line of String(raw || '').split('\n')) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const idx = trimmed.indexOf('=');
    if (idx <= 0) continue;
    const k = trimmed.slice(0, idx).trim();
    const v = trimmed.slice(idx + 1).trim();
    if (k && v) out[k] = v;
  }
  return out;
}

function formatMetadataLines(meta) {
  if (!meta) return '';
  return Object.entries(meta)
    .map(([k, v]) => `${k}=${v}`)
    .join('\n');
}

function TestStatusChip({ test }) {
  if (test.status === 'running') {
    return <span className="chip chip--accent chip--live">Running</span>;
  }
  if (test.status === 'planned') {
    return <span className="chip chip--outline">Planned</span>;
  }
  if (test.status === 'completed') {
    if (test.outcome === 'stopped') {
      return (
        <span className="chip chip--warn">
          <svg className="icon icon--sm" aria-hidden="true"><use href="#i-stop" /></svg>
          Stopped
        </span>
      );
    }
    return (
      <span className="chip chip--ok">
        <svg className="icon icon--sm" aria-hidden="true"><use href="#i-check" /></svg>
        Completed
      </span>
    );
  }
  return (
    <span className="chip chip--danger">
      <svg className="icon icon--sm" aria-hidden="true"><use href="#i-x" /></svg>
      {test.outcome === 'aborted' ? 'Aborted' : 'Failed'}
    </span>
  );
}

function MonitorChip({ monitor }) {
  if (!monitor || !monitor.job_id) {
    return <span className="chip chip--outline">Select on start</span>;
  }
  const label = monitor.mode === 'created' ? `Created ${monitor.job_id}` : `Attached ${monitor.job_id}`;
  return (
    <span className="chip chip--ok" title={monitor.history_key || monitor.job_id}>
      <svg className="icon icon--sm" aria-hidden="true"><use href="#i-activity" /></svg>
      {label}
    </span>
  );
}

export function ProjectsPage() {
  usePageTitle('Projects & tests');
  const pushToast = useToast();

  const {
    projects,
    active,
    maneuvers,
    runs,
    monitorJobs,
    monitorAgents,
    noBackend,
    error: loadError,
    loading,
    createProject,
    updateProject,
    deleteProject,
    createTest,
    deleteTest,
    startTest,
    stopTest,
  } = useProjects();

  const [selectedId, setSelectedId] = useState(null);
  const [searchQ, setSearchQ] = useState('');
  const [resultFilter, setResultFilter] = useState('all');
  const [pageStatus, setPageStatus] = useState(null);
  const [busyKey, setBusyKey] = useState('');

  // Project create/edit dialog
  const [projectDialog, setProjectDialog] = useState(null); // null | { mode: 'create'|'edit', id?, name, description, customer, extraMeta }
  const projectDialogRef = useRef(null);

  // Create test dialog
  const [testDialog, setTestDialog] = useState(null); // null | { name, description, maneuver_id, monitor_job_id }
  const testDialogRef = useRef(null);

  // Start test (attach/create monitor) dialog
  const [startDialog, setStartDialog] = useState(null); // null | { projectId, test, monitorChoice, gateConfirm? }
  const startDialogRef = useRef(null);

  // Delete project confirmation dialog
  const [deleteProjTarget, setDeleteProjTarget] = useState(null);
  const deleteProjDialogRef = useRef(null);

  const initializedRef = useRef(false);
  useEffect(() => {
    if (initializedRef.current || loading) return;
    if (projects.length > 0) {
      initializedRef.current = true;
      setSelectedId(active?.project_id || projects[0].id);
    }
  }, [loading, projects, active]);

  // Keep selectedId valid if projects list changes.
  useEffect(() => {
    if (projects.length === 0) {
      setSelectedId(null);
      return;
    }
    if (selectedId && !projects.some((p) => p.id === selectedId)) {
      setSelectedId(projects[0].id);
    }
  }, [projects, selectedId]);

  useEffect(() => {
    const sync = (ref, open) => {
      const dlg = ref.current;
      if (!dlg) return;
      if (open && !dlg.open) dlg.showModal();
      if (!open && dlg.open) dlg.close();
    };
    sync(projectDialogRef, Boolean(projectDialog));
    sync(testDialogRef, Boolean(testDialog));
    sync(startDialogRef, Boolean(startDialog));
    sync(deleteProjDialogRef, Boolean(deleteProjTarget));
  }, [projectDialog, testDialog, startDialog, deleteProjTarget]);

  const filteredProjects = useMemo(() => {
    const q = searchQ.trim().toLowerCase();
    if (!q) return projects;
    return projects.filter((p) => {
      const metaText = Object.entries(p.metadata || {})
        .flat()
        .join(' ');
      return `${p.name} ${p.description || ''} ${p.id} ${metaText}`.toLowerCase().includes(q);
    });
  }, [projects, searchQ]);

  const selectedProject = useMemo(
    () => projects.find((p) => p.id === selectedId) || null,
    [projects, selectedId]
  );

  const activeRun = useMemo(() => {
    if (!active?.maneuver_run_id) return null;
    return runs.find((r) => r.run_id === active.maneuver_run_id) || null;
  }, [active, runs]);

  const plannedAndRunningTests = useMemo(() => {
    if (!selectedProject) return [];
    return (selectedProject.tests || [])
      .filter((t) => t.status === 'planned' || t.status === 'running')
      .sort((a, b) => {
        if (a.status === 'running' && b.status !== 'running') return -1;
        if (b.status === 'running' && a.status !== 'running') return 1;
        return new Date(b.created_at).getTime() - new Date(a.created_at).getTime();
      });
  }, [selectedProject]);

  const completedGroups = useMemo(() => {
    if (!selectedProject) return [];
    return groupCompletedByDate(selectedProject.tests || [], resultFilter);
  }, [selectedProject, resultFilter]);

  const projectStats = useMemo(() => {
    const tests = selectedProject?.tests || [];
    let planned = 0;
    let running = 0;
    let completed = 0;
    let failed = 0;
    const monitors = new Set();
    for (const t of tests) {
      if (t.status === 'planned') planned++;
      else if (t.status === 'running') running++;
      else if (t.status === 'completed') completed++;
      else if (t.status === 'failed') failed++;
      if (t.monitor?.job_id) monitors.add(t.monitor.job_id);
    }
    return { total: tests.length, planned, running, completed, failed, monitorCount: monitors.size };
  }, [selectedProject]);

  function openCreateProject() {
    setProjectDialog({
      mode: 'create',
      name: '',
      description: '',
      customer: '',
      extraMeta: '',
      error: '',
    });
  }

  function openEditProject(proj) {
    const { customer = '', ...rest } = proj.metadata || {};
    setProjectDialog({
      mode: 'edit',
      id: proj.id,
      name: proj.name || '',
      description: proj.description || '',
      customer,
      extraMeta: formatMetadataLines(rest),
      error: '',
    });
  }

  async function handleSaveProject(e) {
    e.preventDefault();
    if (!projectDialog) return;
    const meta = parseMetadataLines(projectDialog.extraMeta);
    if (projectDialog.customer.trim()) {
      meta.customer = projectDialog.customer.trim();
    }
    const payload = {
      name: projectDialog.name.trim(),
      description: projectDialog.description.trim(),
      metadata: meta,
    };
    setBusyKey('save-project');
    const r =
      projectDialog.mode === 'create'
        ? await createProject(payload)
        : await updateProject(projectDialog.id, payload);
    setBusyKey('');
    if (!r.ok) {
      setProjectDialog((prev) => ({ ...prev, error: r.data.error || `Save failed (${r.status})` }));
      return;
    }
    const saved = r.data.project;
    setSelectedId(saved.id);
    setProjectDialog(null);
    pushToast(projectDialog.mode === 'create' ? `Created project "${saved.name}".` : `Updated "${saved.name}".`);
  }

  async function handleConfirmDeleteProject() {
    if (!deleteProjTarget) return;
    setBusyKey('delete-project');
    const r = await deleteProject(deleteProjTarget.id);
    setBusyKey('');
    if (!r.ok) {
      setPageStatus({ kind: 'danger', msg: r.data.error || `Delete failed (${r.status})` });
      setDeleteProjTarget(null);
      return;
    }
    pushToast(`Deleted project "${deleteProjTarget.name}".`);
    setDeleteProjTarget(null);
  }

  function openCreateTest() {
    if (!selectedProject) return;
    setTestDialog({
      name: '',
      description: '',
      maneuver_id: maneuvers[0]?.id || '',
      monitor_job_id: '',
      error: '',
    });
  }

  async function handleCreateTest(e) {
    e.preventDefault();
    if (!selectedProject || !testDialog) return;
    if (!testDialog.maneuver_id) {
      setTestDialog((prev) => ({ ...prev, error: 'Select a Maneuver for this test.' }));
      return;
    }
    setBusyKey('create-test');
    const r = await createTest(selectedProject.id, {
      name: testDialog.name.trim(),
      description: testDialog.description.trim(),
      maneuver_id: testDialog.maneuver_id,
      monitor_job_id: testDialog.monitor_job_id || undefined,
    });
    setBusyKey('');
    if (!r.ok) {
      setTestDialog((prev) => ({ ...prev, error: r.data.error || `Create failed (${r.status})` }));
      return;
    }
    const created = r.data.test;
    setTestDialog(null);
    setPageStatus({ kind: 'ok', msg: `Created planned test "${created.name}".` });
    pushToast(`Created test "${created.name}".`);
  }

  function openStartDialog(test) {
    if (!selectedProject) return;
    // Pre-select existing monitor job matching target (ip, origin) if present, else default to existing attached or create-new.
    let defaultChoice = test.monitor?.job_id || '';
    if (!defaultChoice && test.target?.ip) {
      const match = monitorJobs.find(
        (j) => j.ip === test.target.ip && (!test.target.origin || j.origin.startsWith(test.target.origin))
      );
      if (match) defaultChoice = match.id;
    }
    if (!defaultChoice) {
      defaultChoice = '__create__';
    }
    setStartDialog({
      projectId: selectedProject.id,
      test,
      monitorChoice: defaultChoice,
      error: '',
      gateConfirm: null,
    });
  }

  async function handleStartSubmit(e, confirmDangerous = false) {
    if (e) e.preventDefault();
    if (!startDialog) return;
    const { projectId, test, monitorChoice } = startDialog;
    const payload = { confirm: confirmDangerous };
    if (monitorChoice === '__create__') {
      payload.create_monitor = true;
    } else {
      payload.monitor_job_id = monitorChoice;
    }

    setBusyKey(`start:${test.id}`);
    const r = await startTest(projectId, test.id, payload);
    setBusyKey('');

    if (r.status === 409 && r.data?.needs_confirmation) {
      setStartDialog((prev) => ({
        ...prev,
        error: '',
        gateConfirm: r.data,
      }));
      return;
    }
    if (!r.ok) {
      const msg = r.data?.error || `Start refused (${r.status})`;
      setStartDialog((prev) => ({ ...prev, error: msg }));
      if (r.status === 409) {
        setPageStatus({ kind: 'warn', msg });
      }
      return;
    }

    setStartDialog(null);
    setPageStatus({
      kind: 'ok',
      msg: `Started "${test.name}" (Maneuver run ${r.data?.run?.run_id || ''}, monitor ${r.data?.test?.monitor?.job_id || ''}).`,
    });
    pushToast(`Started test "${test.name}".`);
  }

  async function handleStopTest(projectId, testId, testName) {
    setBusyKey(`stop:${testId}`);
    const r = await stopTest(projectId, testId, {});
    setBusyKey('');
    if (!r.ok) {
      setPageStatus({ kind: 'danger', msg: r.data?.error || `Stop failed (${r.status})` });
      return;
    }
    setPageStatus({ kind: 'ok', msg: `Stopped "${testName}" and released the active-test lock.` });
    pushToast(`Stopped test "${testName}".`);
  }

  async function handleDeleteTest(projectId, testId, testName) {
    setBusyKey(`del:${testId}`);
    const r = await deleteTest(projectId, testId);
    setBusyKey('');
    if (!r.ok) {
      setPageStatus({ kind: 'danger', msg: r.data?.error || `Delete failed (${r.status})` });
      return;
    }
    pushToast(`Deleted test "${testName}".`);
  }

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Projects &amp; tests</h1>
          <p>
            Group Maneuver tests, attached monitors, and evidence windows by engagement. Only one test may be active across the entire system at a time.
          </p>
        </div>
        <div className="page-head__actions">
          <button
            className="btn"
            type="button"
            disabled={noBackend}
            onClick={openCreateProject}
          >
            <svg className="icon" aria-hidden="true"><use href="#i-plus" /></svg>
            New project
          </button>
          <button
            className="btn btn--primary"
            type="button"
            disabled={noBackend || !selectedProject}
            onClick={openCreateTest}
          >
            <svg className="icon" aria-hidden="true"><use href="#i-play" /></svg>
            New test
          </button>
        </div>
      </div>

      {noBackend && (
        <StatusAlert
          status={{ kind: 'warn', msg: 'No backend is attached to the panel. Start fleet-server with a data directory to manage projects and tests.' }}
          style={{ marginBottom: 'var(--sp-4)' }}
        />
      )}

      {loadError && (
        <StatusAlert
          status={{ kind: 'danger', msg: loadError }}
          style={{ marginBottom: 'var(--sp-4)' }}
        />
      )}

      {pageStatus && (
        <StatusAlert
          status={pageStatus}
          style={{ marginBottom: 'var(--sp-4)' }}
        />
      )}

      {/* System-wide single active-test lock banner */}
      {active ? (
        <div className="alert alert--warn" role="status" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true"><use href="#i-lock" /></svg>
          <div style={{ flex: 1 }}>
            <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--sp-2)' }}>
              <strong>Active test lock held:</strong>
              <span>
                {active.project_name} / <strong>{active.test_name}</strong>
              </span>
              <span className="chip chip--accent chip--live">
                {activeRun ? activeRun.state : 'running'}
              </span>
              {active.monitor_job_id && (
                <span className="chip chip--ok mono">Monitor: {active.monitor_job_id}</span>
              )}
              {active.maneuver_run_id && (
                <span className="chip chip--outline mono">Run: {active.maneuver_run_id}</span>
              )}
            </div>
            <div className="muted" style={{ fontSize: 'var(--fs-xs)', marginTop: 'var(--sp-1)' }}>
              Concurrent test starts across all projects are blocked until this test completes or is stopped.
              Started {fmtTime(active.acquired_at)}
              {active.started_by ? ` by ${active.started_by}` : ''}.
            </div>
          </div>
          <div style={{ display: 'flex', gap: 'var(--sp-2)', alignItems: 'center', flexWrap: 'wrap' }}>
            {selectedId !== active.project_id && (
              <button
                className="btn btn--sm"
                type="button"
                onClick={() => setSelectedId(active.project_id)}
              >
                View project
              </button>
            )}
            <Link className="btn btn--sm" to="/maneuver">
              Maneuver run
            </Link>
            <button
              className="btn btn--sm btn--danger"
              type="button"
              disabled={busyKey === `stop:${active.test_id}`}
              onClick={() => handleStopTest(active.project_id, active.test_id, active.test_name)}
            >
              <svg className="icon icon--sm" aria-hidden="true"><use href="#i-stop" /></svg>
              {busyKey === `stop:${active.test_id}` ? 'Stopping...' : 'Stop test'}
            </button>
          </div>
        </div>
      ) : (
        <div className="alert" role="status" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true"><use href="#i-info" /></svg>
          <div>
            <strong>No test is running.</strong> The system-wide active-test lock is free. Create a test inside a project and attach or create a monitor when starting it.
          </div>
        </div>
      )}

      <div className="split split--rail-left">
        {/* Left rail: projects list */}
        <section className="card" aria-labelledby="projects-list-title">
          <div className="card__head">
            <h2 id="projects-list-title">Projects</h2>
            <span className="muted">{projects.length} {projects.length === 1 ? 'project' : 'projects'}</span>
          </div>
          <div className="card__body" style={{ paddingBottom: 'var(--sp-2)' }}>
            <div className="input-affix">
              <svg className="icon" aria-hidden="true"><use href="#i-search" /></svg>
              <input
                className="input"
                type="search"
                placeholder="Search projects"
                aria-label="Search projects"
                value={searchQ}
                onChange={(e) => setSearchQ(e.target.value)}
              />
            </div>
          </div>
          {filteredProjects.length === 0 ? (
            <div className="card__body">
              <p className="muted">
                {projects.length === 0 ? 'No projects created yet.' : 'No projects match your search.'}
              </p>
            </div>
          ) : (
            <ul className="list">
              {filteredProjects.map((p) => {
                const isSelected = p.id === selectedId;
                const holdsLock = active?.project_id === p.id;
                const customer = p.metadata?.customer;
                return (
                  <li key={p.id}>
                    <button
                      type="button"
                      className="list__item"
                      style={{ width: '100%', textAlign: 'left', background: 'none', border: 'none', cursor: 'pointer' }}
                      aria-current={isSelected ? 'true' : undefined}
                      onClick={() => setSelectedId(p.id)}
                    >
                      <div className="list__main">
                        <b>{p.name}</b>
                        <small>
                          {customer ? `${customer} · ` : ''}
                          {(p.tests || []).length} {(p.tests || []).length === 1 ? 'test' : 'tests'} · {formatLastDate(p.tests)}
                        </small>
                      </div>
                      {holdsLock ? (
                        <span className="chip chip--accent chip--live">Active</span>
                      ) : isSelected ? (
                        <span className="chip chip--accent">Selected</span>
                      ) : null}
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
        </section>

        {/* Right pane: project details, planned/running tests, completed by date */}
        <div className="stack">
          {!selectedProject ? (
            <section className="card">
              <div className="card__body">
                <h2>No project selected</h2>
                <p className="muted" style={{ marginTop: 'var(--sp-1)', marginBottom: 'var(--sp-3)' }}>
                  Create a project to group Maneuver runs, attached monitors, and historical test windows.
                </p>
                <button
                  className="btn btn--primary"
                  type="button"
                  disabled={noBackend}
                  onClick={openCreateProject}
                >
                  <svg className="icon" aria-hidden="true"><use href="#i-plus" /></svg>
                  Create project
                </button>
              </div>
            </section>
          ) : (
            <>
              <section className="card" aria-labelledby="proj-detail-title">
                <div className="card__head">
                  <div>
                    <h2 id="proj-detail-title">{selectedProject.name}</h2>
                    {selectedProject.description && <p>{selectedProject.description}</p>}
                  </div>
                  <div style={{ display: 'flex', gap: 'var(--sp-2)', flexWrap: 'wrap' }}>
                    <button
                      className="btn btn--sm"
                      type="button"
                      onClick={() => openEditProject(selectedProject)}
                    >
                      <svg className="icon icon--sm" aria-hidden="true"><use href="#i-edit" /></svg>
                      Edit
                    </button>
                    <button
                      className="btn btn--sm btn--danger"
                      type="button"
                      disabled={active?.project_id === selectedProject.id}
                      title={active?.project_id === selectedProject.id ? 'Cannot delete project while its test is active' : 'Delete project'}
                      onClick={() => setDeleteProjTarget(selectedProject)}
                    >
                      <svg className="icon icon--sm" aria-hidden="true"><use href="#i-trash" /></svg>
                      Delete
                    </button>
                  </div>
                </div>
                {selectedProject.metadata && Object.keys(selectedProject.metadata).length > 0 && (
                  <div className="card__body" style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--sp-2)' }}>
                    {Object.entries(selectedProject.metadata).map(([k, v]) => (
                      <span className="chip chip--outline" key={k}>
                        <span className="muted">{k}:</span> {v}
                      </span>
                    ))}
                  </div>
                )}
              </section>

              <section className="grid" aria-label="Project summary">
                <div className="card stat col-4">
                  <div className="stat__label">Tests</div>
                  <div className="stat__value">{projectStats.total}</div>
                  <div className="stat__note">
                    {projectStats.planned} planned · {projectStats.running} running
                  </div>
                </div>
                <div className="card stat col-4">
                  <div className="stat__label">Completed</div>
                  <div className="stat__value">{projectStats.completed}</div>
                  <div className="stat__note">{projectStats.failed} failed / aborted</div>
                </div>
                <div className="card stat col-4">
                  <div className="stat__label">Monitors used</div>
                  <div className="stat__value">{projectStats.monitorCount}</div>
                  <div className="stat__note">{monitorJobs.length} total jobs configured</div>
                </div>
              </section>

              {/* Active & Planned tests */}
              <section className="card" aria-labelledby="active-planned-title">
                <div className="card__head">
                  <div>
                    <h2 id="active-planned-title">Planned &amp; running tests</h2>
                    <p>Start a planned test to acquire the global lock, attach or create its monitor, and execute its Maneuver.</p>
                  </div>
                  <button
                    className="btn btn--sm btn--primary"
                    type="button"
                    disabled={noBackend}
                    onClick={openCreateTest}
                  >
                    <svg className="icon icon--sm" aria-hidden="true"><use href="#i-plus" /></svg>
                    Add test
                  </button>
                </div>

                {plannedAndRunningTests.length === 0 ? (
                  <div className="card__body">
                    <p className="muted">
                      No planned or running tests in this project.{' '}
                      {maneuvers.length === 0 ? (
                        <>
                          Create a Maneuver on the <Link to="/maneuver">Maneuver</Link> page first.
                        </>
                      ) : (
                        'Click "Add test" to schedule a Maneuver test.'
                      )}
                    </p>
                  </div>
                ) : (
                  <div className="table-wrap">
                    <table className="table table--sticky-first">
                      <thead>
                        <tr>
                          <th scope="col">Test</th>
                          <th scope="col">Target (ip, origin)</th>
                          <th scope="col">Monitor</th>
                          <th scope="col">Status</th>
                          <th scope="col"><span className="sr-only">Actions</span></th>
                        </tr>
                      </thead>
                      <tbody>
                        {plannedAndRunningTests.map((t) => {
                          const lockBlocked = Boolean(active && active.test_id !== t.id);
                          const liveRun = t.maneuver_run_id
                            ? runs.find((r) => r.run_id === t.maneuver_run_id)
                            : null;
                          return (
                            <tr key={t.id}>
                              <th scope="row" className="cell-primary">
                                {t.name}
                                <div className="cell-sub">
                                  Maneuver: {t.maneuver_name || t.maneuver_id}
                                  {t.agents?.length ? ` · ${t.agents.length} agent(s)` : ''}
                                </div>
                              </th>
                              <td>
                                <div className="mono">{t.target?.ip || '—'}</div>
                                {t.target?.origin && (
                                  <div className="cell-sub mono">{t.target.origin}</div>
                                )}
                              </td>
                              <td>
                                <MonitorChip monitor={t.monitor} />
                              </td>
                              <td>
                                <TestStatusChip test={t} />
                                {liveRun && t.status === 'running' && (
                                  <div className="cell-sub">Maneuver: {liveRun.state}</div>
                                )}
                              </td>
                              <td className="actions">
                                {t.status === 'planned' ? (
                                  <>
                                    <button
                                      className="btn btn--sm btn--primary"
                                      type="button"
                                      disabled={lockBlocked || busyKey === `start:${t.id}`}
                                      title={
                                        lockBlocked
                                          ? `Locked: ${active.test_name} is currently active`
                                          : 'Attach monitor and start test'
                                      }
                                      onClick={() => openStartDialog(t)}
                                    >
                                      <svg className="icon icon--sm" aria-hidden="true">
                                        <use href={lockBlocked ? '#i-lock' : '#i-play'} />
                                      </svg>
                                      {lockBlocked ? 'Locked' : 'Start'}
                                    </button>
                                    <button
                                      className="btn btn--sm btn--danger"
                                      type="button"
                                      disabled={busyKey === `del:${t.id}`}
                                      onClick={() => handleDeleteTest(selectedProject.id, t.id, t.name)}
                                    >
                                      Delete
                                    </button>
                                  </>
                                ) : (
                                  <button
                                    className="btn btn--sm btn--danger"
                                    type="button"
                                    disabled={busyKey === `stop:${t.id}`}
                                    onClick={() => handleStopTest(selectedProject.id, t.id, t.name)}
                                  >
                                    <svg className="icon icon--sm" aria-hidden="true"><use href="#i-stop" /></svg>
                                    {busyKey === `stop:${t.id}` ? 'Stopping...' : 'Stop'}
                                  </button>
                                )}
                              </td>
                            </tr>
                          );
                        })}
                      </tbody>
                    </table>
                  </div>
                )}
              </section>

              {/* Completed tests grouped by date */}
              <section className="card" aria-labelledby="completed-tests-title">
                <div className="card__head">
                  <div>
                    <h2 id="completed-tests-title">{selectedProject.name}: completed tests</h2>
                    <p>Newest first, grouped by date, with evidence and monitor window pointers.</p>
                  </div>
                  <div className="seg" role="radiogroup" aria-label="Filter by result">
                    {[
                      { id: 'all', label: 'All' },
                      { id: 'completed', label: 'Completed' },
                      { id: 'failed', label: 'Failed' },
                    ].map((opt) => (
                      <label key={opt.id}>
                        <input
                          type="radio"
                          name="res-filter"
                          checked={resultFilter === opt.id}
                          onChange={() => setResultFilter(opt.id)}
                        />
                        <span>{opt.label}</span>
                      </label>
                    ))}
                  </div>
                </div>

                {completedGroups.length === 0 ? (
                  <div className="card__body">
                    <p className="muted">No completed or failed tests match the current filter.</p>
                  </div>
                ) : (
                  <div className="table-wrap">
                    <table className="table table--sticky-first">
                      <caption className="sr-only">Completed tests of {selectedProject.name} grouped by date</caption>
                      <thead>
                        <tr>
                          <th scope="col">Test</th>
                          <th scope="col">Target (ip, origin)</th>
                          <th scope="col">Window</th>
                          <th scope="col">Monitor</th>
                          <th scope="col">Result</th>
                          <th scope="col">Report &amp; pointers</th>
                        </tr>
                      </thead>
                      <tbody>
                        {completedGroups.map((grp) => (
                          <DateRows
                            key={grp.date}
                            date={grp.date}
                            tests={grp.tests}
                            projectId={selectedProject.id}
                            onDelete={handleDeleteTest}
                            onDownloadReport={(t) => pushToast(`Downloading HTML report for "${t.name}".`)}
                            busyKey={busyKey}
                          />
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </section>
            </>
          )}
        </div>
      </div>

      {/* Create / Edit Project Dialog */}
      <dialog
        className="dialog"
        ref={projectDialogRef}
        aria-labelledby="dlg-project-title"
        onCancel={(e) => {
          e.preventDefault();
          setProjectDialog(null);
        }}
      >
        {projectDialog && (
          <form onSubmit={handleSaveProject}>
            <div className="dialog__head">
              <h2 id="dlg-project-title">
                {projectDialog.mode === 'create' ? 'New project' : 'Edit project'}
              </h2>
              <p>Projects group tests, customer/engagement metadata, and completed test history.</p>
            </div>
            <div className="dialog__body">
              {projectDialog.error && (
                <StatusAlert status={{ kind: 'danger', msg: projectDialog.error }} />
              )}
              <div className="field">
                <label htmlFor="proj-name">Project name</label>
                <input
                  id="proj-name"
                  className="input"
                  required
                  maxLength={120}
                  value={projectDialog.name}
                  onChange={(e) => setProjectDialog((p) => ({ ...p, name: e.target.value }))}
                  placeholder="e.g. Acme engagement"
                />
              </div>
              <div className="field">
                <label htmlFor="proj-customer">Customer / engagement</label>
                <input
                  id="proj-customer"
                  className="input"
                  value={projectDialog.customer}
                  onChange={(e) => setProjectDialog((p) => ({ ...p, customer: e.target.value }))}
                  placeholder="e.g. Acme Corp"
                />
              </div>
              <div className="field">
                <label htmlFor="proj-desc">Description</label>
                <input
                  id="proj-desc"
                  className="input"
                  value={projectDialog.description}
                  onChange={(e) => setProjectDialog((p) => ({ ...p, description: e.target.value }))}
                  placeholder="Scope or notes"
                />
              </div>
              <div className="field">
                <label htmlFor="proj-meta">Additional metadata (key=value per line)</label>
                <textarea
                  id="proj-meta"
                  className="textarea mono"
                  rows={3}
                  value={projectDialog.extraMeta}
                  onChange={(e) => setProjectDialog((p) => ({ ...p, extraMeta: e.target.value }))}
                  placeholder="env=staging&#10;owner=alice"
                />
              </div>
            </div>
            <div className="dialog__foot">
              <button className="btn" type="button" onClick={() => setProjectDialog(null)}>
                Cancel
              </button>
              <button
                className="btn btn--primary"
                type="submit"
                disabled={busyKey === 'save-project'}
              >
                {busyKey === 'save-project'
                  ? 'Saving...'
                  : projectDialog.mode === 'create'
                  ? 'Create project'
                  : 'Save changes'}
              </button>
            </div>
          </form>
        )}
      </dialog>

      {/* Create Test Dialog */}
      <dialog
        className="dialog"
        ref={testDialogRef}
        aria-labelledby="dlg-test-title"
        onCancel={(e) => {
          e.preventDefault();
          setTestDialog(null);
        }}
      >
        {testDialog && (
          <form onSubmit={handleCreateTest}>
            <div className="dialog__head">
              <h2 id="dlg-test-title">Add test to {selectedProject?.name}</h2>
              <p>A test references a Maneuver definition and attaches or creates a monitor when started.</p>
            </div>
            <div className="dialog__body">
              {testDialog.error && (
                <StatusAlert status={{ kind: 'danger', msg: testDialog.error }} />
              )}
              <div className="field">
                <label htmlFor="test-maneuver">Maneuver</label>
                {maneuvers.length === 0 ? (
                  <p className="muted">
                    No Maneuvers found. Create one in <Link to="/maneuver">Maneuver</Link> first.
                  </p>
                ) : (
                  <select
                    id="test-maneuver"
                    className="select"
                    value={testDialog.maneuver_id}
                    onChange={(e) => setTestDialog((t) => ({ ...t, maneuver_id: e.target.value }))}
                  >
                    {maneuvers.map((mv) => (
                      <option key={mv.id} value={mv.id}>
                        {mv.name} ({mv.target?.ip}{mv.target?.origin ? ` · ${mv.target.origin}` : ''})
                      </option>
                    ))}
                  </select>
                )}
              </div>
              <div className="field">
                <label htmlFor="test-name">Test name (optional)</label>
                <input
                  id="test-name"
                  className="input"
                  maxLength={120}
                  value={testDialog.name}
                  onChange={(e) => setTestDialog((t) => ({ ...t, name: e.target.value }))}
                  placeholder="Auto-numbered from Maneuver if omitted"
                />
              </div>
              <div className="field">
                <label htmlFor="test-monitor">Pre-attach monitor job (optional)</label>
                <select
                  id="test-monitor"
                  className="select"
                  value={testDialog.monitor_job_id}
                  onChange={(e) => setTestDialog((t) => ({ ...t, monitor_job_id: e.target.value }))}
                >
                  <option value="">Choose or create on start</option>
                  {monitorJobs.map((j) => (
                    <option key={j.id} value={j.id}>
                      {j.id} — {j.ip}{j.origin ? ` · ${j.origin}` : ''}
                    </option>
                  ))}
                </select>
              </div>
              <div className="field">
                <label htmlFor="test-desc">Notes (optional)</label>
                <input
                  id="test-desc"
                  className="input"
                  value={testDialog.description}
                  onChange={(e) => setTestDialog((t) => ({ ...t, description: e.target.value }))}
                  placeholder="Test hypothesis or run notes"
                />
              </div>
            </div>
            <div className="dialog__foot">
              <button className="btn" type="button" onClick={() => setTestDialog(null)}>
                Cancel
              </button>
              <button
                className="btn btn--primary"
                type="submit"
                disabled={maneuvers.length === 0 || busyKey === 'create-test'}
              >
                {busyKey === 'create-test' ? 'Creating...' : 'Create planned test'}
              </button>
            </div>
          </form>
        )}
      </dialog>

      {/* Start Test (Attach / Create Monitor) Dialog */}
      <dialog
        className="dialog"
        ref={startDialogRef}
        aria-labelledby="dlg-start-test-title"
        onCancel={(e) => {
          e.preventDefault();
          setStartDialog(null);
        }}
      >
        {startDialog && (
          <form onSubmit={(e) => handleStartSubmit(e, false)}>
            <div className="dialog__head">
              <h2 id="dlg-start-test-title">Start test: {startDialog.test.name}</h2>
              <p>
                A test needs a monitor watching its target. Attach an existing monitor job or create one for{' '}
                <span className="mono">
                  {startDialog.test.target?.ip || 'target'}
                  {startDialog.test.target?.origin ? ` · ${startDialog.test.target.origin}` : ''}
                </span>
                .
              </p>
            </div>
            <div className="dialog__body">
              {active && (
                <StatusAlert
                  status={{
                    kind: 'danger',
                    msg: `Global lock active: "${active.test_name}" in "${active.project_name}" is currently running. Concurrent starts will be refused.`,
                  }}
                />
              )}
              {startDialog.error && (
                <StatusAlert status={{ kind: 'danger', msg: startDialog.error }} />
              )}
              {startDialog.gateConfirm && (
                <div className="alert alert--warn">
                  <svg className="icon" aria-hidden="true"><use href="#i-alert" /></svg>
                  <div>
                    <strong>Dangerous command confirmation required:</strong>{' '}
                    <code>{startDialog.gateConfirm.command}</code> matched{' '}
                    <code>{startDialog.gateConfirm.pattern}</code>. Confirm to execute.
                  </div>
                </div>
              )}
              <div className="field">
                <label htmlFor="start-mon-select">Monitor job</label>
                <select
                  id="start-mon-select"
                  className="select"
                  value={startDialog.monitorChoice}
                  onChange={(e) => setStartDialog((s) => ({ ...s, monitorChoice: e.target.value, error: '' }))}
                >
                  <option value="__create__">
                    Create a new monitor job for target (or reuse matching)...
                  </option>
                  {monitorJobs.map((j) => (
                    <option key={j.id} value={j.id}>
                      Attach existing: {j.id} ({j.ip}{j.origin ? ` · ${j.origin}` : ''})
                    </option>
                  ))}
                </select>
                <span className="field__hint">
                  Monitor agents available: {monitorAgents.length}. Only one monitor job may exist per (ip, origin).
                </span>
              </div>
            </div>
            <div className="dialog__foot">
              <button className="btn" type="button" onClick={() => setStartDialog(null)}>
                Cancel
              </button>
              {startDialog.gateConfirm ? (
                <button
                  className="btn btn--danger"
                  type="button"
                  disabled={busyKey === `start:${startDialog.test.id}`}
                  onClick={() => handleStartSubmit(null, true)}
                >
                  Confirm &amp; start test
                </button>
              ) : (
                <button
                  className="btn btn--primary"
                  type="submit"
                  disabled={busyKey === `start:${startDialog.test.id}`}
                >
                  <svg className="icon icon--sm" aria-hidden="true"><use href="#i-play" /></svg>
                  {busyKey === `start:${startDialog.test.id}` ? 'Starting...' : 'Start test'}
                </button>
              )}
            </div>
          </form>
        )}
      </dialog>

      {/* Delete Project Confirm Dialog */}
      <dialog
        className="dialog"
        ref={deleteProjDialogRef}
        aria-labelledby="dlg-del-proj-title"
        onCancel={(e) => {
          e.preventDefault();
          setDeleteProjTarget(null);
        }}
      >
        {deleteProjTarget && (
          <div>
            <div className="dialog__head">
              <h2 id="dlg-del-proj-title">Delete project</h2>
              <p>
                Remove <strong>{deleteProjTarget.name}</strong> and its {(deleteProjTarget.tests || []).length} recorded test(s)?
              </p>
            </div>
            <div className="dialog__foot">
              <button className="btn" type="button" onClick={() => setDeleteProjTarget(null)}>
                Cancel
              </button>
              <button
                className="btn btn--danger"
                type="button"
                disabled={busyKey === 'delete-project'}
                onClick={handleConfirmDeleteProject}
              >
                {busyKey === 'delete-project' ? 'Deleting...' : 'Delete project'}
              </button>
            </div>
          </div>
        )}
      </dialog>
    </>
  );
}

function DateRows({ date, tests, projectId, onDelete, onDownloadReport, busyKey }) {
  return (
    <>
      <tr className="group">
        <th colSpan={6} scope="colgroup">{date}</th>
      </tr>
      {tests.map((t) => {
        const durMs =
          t.started_at && t.ended_at
            ? Math.max(0, new Date(t.ended_at).getTime() - new Date(t.started_at).getTime())
            : null;
        const reportUrl = `/api/projects/${encodeURIComponent(projectId)}/tests/${encodeURIComponent(t.id)}/report`;
        return (
          <tr key={t.id}>
            <th scope="row" className="cell-primary">
              {t.name}
              <div className="cell-sub">
                Maneuver: {t.maneuver_name || t.maneuver_id}
                {t.maneuver_run_id ? ` · ${t.maneuver_run_id}` : ''}
              </div>
            </th>
            <td>
              <div className="mono">{t.target?.ip || '—'}</div>
              {t.target?.origin && <div className="cell-sub mono">{t.target.origin}</div>}
            </td>
            <td className="nowrap">
              <div>{t.started_at ? fmtDateTime(t.started_at) : '—'}</div>
              <div className="cell-sub">
                {durMs != null ? `Duration ${fmtDur(durMs)}` : ''}
                {t.agents?.length ? ` · ${t.agents.length} agent(s)` : ''}
              </div>
            </td>
            <td>
              <MonitorChip monitor={t.monitor} />
            </td>
            <td>
              <TestStatusChip test={t} />
              {t.error && <div className="cell-sub">{t.error}</div>}
            </td>
            <td className="actions">
              <div style={{ display: 'flex', gap: 'var(--sp-1)', flexWrap: 'wrap', justifyContent: 'flex-end' }}>
                <a
                  className="btn btn--sm btn--primary"
                  href={reportUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                  title="Open self-contained interactive HTML report in a new tab"
                >
                  Report
                </a>
                <a
                  className="btn btn--sm"
                  href={`${reportUrl}?download=1`}
                  download={`report-${projectId}-${t.id}.html`}
                  title="Download self-contained HTML report"
                  aria-label={`Download report for ${t.name}`}
                  onClick={() => onDownloadReport?.(t)}
                >
                  <svg className="icon icon--sm" aria-hidden="true"><use href="#i-download" /></svg>
                </a>
                {t.result_pointers?.evidence && (
                  <Link className="btn btn--sm" to="/evidence" title={`Evidence window from ${fmtTime(t.result_pointers.evidence.from)}`}>
                    Evidence
                  </Link>
                )}
                {t.result_pointers?.monitor && (
                  <Link className="btn btn--sm" to="/monitor" title={`Monitor job ${t.result_pointers.monitor.job_id}`}>
                    Monitor
                  </Link>
                )}
                <button
                  className="btn btn--sm btn--danger"
                  type="button"
                  disabled={busyKey === `del:${t.id}`}
                  onClick={() => onDelete(projectId, t.id, t.name)}
                >
                  Delete
                </button>
              </div>
            </td>
          </tr>
        );
      })}
    </>
  );
}
