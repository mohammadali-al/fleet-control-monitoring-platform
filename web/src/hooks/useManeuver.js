import { useCallback, useEffect, useRef, useState } from 'react';
import { api } from '../lib/api';
import { useEvents } from '../lib/events';

/**
 * Live-data hook for Maneuver definitions and synchronized execution runs.
 * Loads /api/maneuvers and /api/maneuvers/runs, and subscribes to live
 * "maneuver" events over /ws/events (ready ACKs, missed, running, stopped, completed).
 */
export function useManeuver() {
  const [maneuvers, setManeuvers] = useState([]);
  const [runs, setRuns] = useState([]);
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
    const [mRes, rRes] = await Promise.all([
      api('GET', '/api/maneuvers'),
      api('GET', '/api/maneuvers/runs'),
    ]);
    if (!mountedRef.current) return;
    setLoading(false);
    if (mRes.status === 503) {
      setNoBackend(true);
      setError('');
      setManeuvers([]);
      setRuns([]);
      return;
    }
    if (!mRes.ok) {
      setError(mRes.data.error || `Could not load maneuvers (${mRes.status})`);
      return;
    }
    setNoBackend(false);
    setError('');
    setManeuvers(Array.isArray(mRes.data.maneuvers) ? mRes.data.maneuvers : []);
    if (rRes.ok && Array.isArray(rRes.data.runs)) {
      setRuns(rRes.data.runs);
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const handleEvent = useCallback((msg) => {
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
  }, []);

  useEvents(handleEvent, refresh);

  const createManeuver = useCallback(async (payload) => {
    const r = await api('POST', '/api/maneuvers', payload);
    if (r.ok) await refresh();
    return r;
  }, [refresh]);

  const updateManeuver = useCallback(async (id, payload) => {
    const r = await api('PUT', `/api/maneuvers/${encodeURIComponent(id)}`, payload);
    if (r.ok) await refresh();
    return r;
  }, [refresh]);

  const duplicateManeuver = useCallback(async (id, name) => {
    const r = await api('POST', `/api/maneuvers/${encodeURIComponent(id)}/duplicate`, name ? { name } : {});
    if (r.ok) await refresh();
    return r;
  }, [refresh]);

  const deleteManeuver = useCallback(async (id) => {
    const r = await api('DELETE', `/api/maneuvers/${encodeURIComponent(id)}`, {});
    if (r.ok) await refresh();
    return r;
  }, [refresh]);

  const previewManeuver = useCallback(async (payload) => {
    return api('POST', '/api/maneuvers/preview', payload);
  }, []);

  const startManeuver = useCallback(async (id, opts = {}) => {
    const r = await api('POST', `/api/maneuvers/${encodeURIComponent(id)}/start`, opts);
    if (r.ok && r.data.run) {
      handleEvent({ type: 'maneuver', maneuver: r.data.run });
    }
    return r;
  }, [handleEvent]);

  const stopManeuver = useCallback(async (runId) => {
    const r = await api('POST', `/api/maneuvers/runs/${encodeURIComponent(runId)}/stop`, {});
    if (r.ok && r.data.run) {
      handleEvent({ type: 'maneuver', maneuver: r.data.run });
    }
    return r;
  }, [handleEvent]);

  return {
    maneuvers,
    runs,
    noBackend,
    error,
    loading,
    refresh,
    createManeuver,
    updateManeuver,
    duplicateManeuver,
    deleteManeuver,
    previewManeuver,
    startManeuver,
    stopManeuver,
  };
}
