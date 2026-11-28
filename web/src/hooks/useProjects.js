import { useCallback, useEffect, useRef, useState } from 'react';
import { api } from '../lib/api';
import { useEvents } from '../lib/events';

/**
 * Live-data hook for Projects, per-project Tests, the system-wide single
 * active-test lock, Maneuver references, and Monitor jobs.
 */
export function useProjects() {
  const [projects, setProjects] = useState([]);
  const [active, setActive] = useState(null);
  const [maneuvers, setManeuvers] = useState([]);
  const [runs, setRuns] = useState([]);
  const [monitorJobs, setMonitorJobs] = useState([]);
  const [monitorAgents, setMonitorAgents] = useState([]);
  const [noBackend, setNoBackend] = useState(false);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const refresh = useCallback(async () => {
    const [pRes, mRes, rRes, monRes] = await Promise.all([
      api('GET', '/api/projects'),
      api('GET', '/api/maneuvers'),
      api('GET', '/api/maneuvers/runs'),
      api('GET', '/api/monitor/config'),
    ]);
    if (!mountedRef.current) return;
    setLoading(false);

    if (pRes.status === 503) {
      setNoBackend(true);
      setError('');
      setProjects([]);
      setActive(null);
      setManeuvers([]);
      setRuns([]);
      setMonitorJobs([]);
      setMonitorAgents([]);
      return;
    }
    if (!pRes.ok) {
      setError(pRes.data.error || `Could not load projects (${pRes.status})`);
      return;
    }

    setNoBackend(false);
    setError('');
    setProjects(Array.isArray(pRes.data.projects) ? pRes.data.projects : []);
    setActive(pRes.data.active || null);

    if (mRes.ok && Array.isArray(mRes.data.maneuvers)) {
      setManeuvers(mRes.data.maneuvers);
    }
    if (rRes.ok && Array.isArray(rRes.data.runs)) {
      setRuns(rRes.data.runs);
    }
    if (monRes.ok) {
      setMonitorJobs(Array.isArray(monRes.data.jobs) ? monRes.data.jobs : []);
      setMonitorAgents(Array.isArray(monRes.data.agents) ? monRes.data.agents : []);
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const handleEvent = useCallback(
    (msg) => {
      if (!msg || msg.type !== 'maneuver' || !msg.maneuver) return;
      const incoming = msg.maneuver;
      setRuns((prev) => {
        const idx = prev.findIndex((r) => r.run_id === incoming.run_id);
        if (idx === -1) {
          return [incoming, ...prev].slice(0, 30);
        }
        const next = [...prev];
        next[idx] = incoming;
        return next;
      });
      if (['running', 'completed', 'aborted', 'stopped'].includes(incoming.state)) {
        refresh();
      }
    },
    [refresh]
  );

  useEvents(handleEvent, refresh);

  const createProject = useCallback(
    async (payload) => {
      const r = await api('POST', '/api/projects', payload);
      if (r.ok) await refresh();
      return r;
    },
    [refresh]
  );

  const updateProject = useCallback(
    async (id, payload) => {
      const r = await api('PUT', `/api/projects/${encodeURIComponent(id)}`, payload);
      if (r.ok) await refresh();
      return r;
    },
    [refresh]
  );

  const deleteProject = useCallback(
    async (id) => {
      const r = await api('DELETE', `/api/projects/${encodeURIComponent(id)}`, {});
      if (r.ok) await refresh();
      return r;
    },
    [refresh]
  );

  const createTest = useCallback(
    async (projectId, payload) => {
      const r = await api('POST', `/api/projects/${encodeURIComponent(projectId)}/tests`, payload);
      if (r.ok) await refresh();
      return r;
    },
    [refresh]
  );

  const deleteTest = useCallback(
    async (projectId, testId) => {
      const r = await api(
        'DELETE',
        `/api/projects/${encodeURIComponent(projectId)}/tests/${encodeURIComponent(testId)}`,
        {}
      );
      if (r.ok) await refresh();
      return r;
    },
    [refresh]
  );

  const startTest = useCallback(
    async (projectId, testId, payload) => {
      const r = await api(
        'POST',
        `/api/projects/${encodeURIComponent(projectId)}/tests/${encodeURIComponent(testId)}/start`,
        payload
      );
      await refresh();
      return r;
    },
    [refresh]
  );

  const stopTest = useCallback(
    async (projectId, testId, payload = {}) => {
      const r = await api(
        'POST',
        `/api/projects/${encodeURIComponent(projectId)}/tests/${encodeURIComponent(testId)}/stop`,
        payload
      );
      await refresh();
      return r;
    },
    [refresh]
  );

  return {
    projects,
    active,
    maneuvers,
    runs,
    monitorJobs,
    monitorAgents,
    noBackend,
    error,
    loading,
    refresh,
    createProject,
    updateProject,
    deleteProject,
    createTest,
    deleteTest,
    startTest,
    stopTest,
  };
}
