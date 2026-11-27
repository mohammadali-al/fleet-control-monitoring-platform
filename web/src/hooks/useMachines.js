import { useCallback, useEffect, useRef, useState } from 'react';
import { api } from '../lib/api';
import { useEvents } from '../lib/events';

const EMPTY_SUMMARY = {
  workers: 0,
  workers_online: 0,
  monitors: 0,
  monitors_online: 0,
  needs_attention: [],
  disk_warn: [],
  disk_critical: [],
};

/**
 * Live-data hook for the Machines registry.
 * Loads GET /api/machines, subscribes to /ws/events ("machine" events + reconnect),
 * and ticks every 15s so relative timestamps stay current.
 */
export function useMachines() {
  const [machines, setMachines] = useState([]);
  const [summary, setSummary] = useState(EMPTY_SUMMARY);
  const [noBackend, setNoBackend] = useState(false);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  const [, setTick] = useState(0);

  const debounceRef = useRef(0);
  const pausedRef = useRef(false);

  const refresh = useCallback(async () => {
    if (pausedRef.current) return;
    const r = await api('GET', '/api/machines');
    setLoading(false);
    if (r.status === 503) {
      setNoBackend(true);
      setError('');
      setMachines([]);
      setSummary(EMPTY_SUMMARY);
      return;
    }
    if (!r.ok) {
      setError(r.data.error || `Could not load machines (${r.status})`);
      return;
    }
    setNoBackend(false);
    setError('');
    setMachines(Array.isArray(r.data.machines) ? r.data.machines : []);
    setSummary(r.data.summary || EMPTY_SUMMARY);
  }, []);

  const refreshSoon = useCallback(() => {
    if (debounceRef.current) window.clearTimeout(debounceRef.current);
    debounceRef.current = window.setTimeout(() => {
      debounceRef.current = 0;
      refresh();
    }, 150);
  }, [refresh]);

  useEffect(() => {
    refresh();
    const iv = window.setInterval(() => {
      setTick((t) => t + 1);
      refresh();
    }, 15000);
    return () => {
      window.clearInterval(iv);
      if (debounceRef.current) window.clearTimeout(debounceRef.current);
    };
  }, [refresh]);

  useEvents(
    (msg) => {
      if (msg && msg.type === 'machine') {
        if (msg.machine && msg.machine.id && !pausedRef.current) {
          setMachines((prev) => {
            const idx = prev.findIndex((x) => x.id === msg.machine.id);
            if (idx === -1) return prev;
            const next = prev.slice();
            next[idx] = msg.machine;
            return next;
          });
        }
        refreshSoon();
      }
    },
    refresh
  );

  const setRefreshPaused = useCallback((paused) => {
    pausedRef.current = paused;
  }, []);

  return { machines, summary, noBackend, error, loading, refresh, setRefreshPaused };
}
