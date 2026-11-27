import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../lib/api';
import { useEvents } from '../lib/events';

function maxTimestampMs(m) {
  if (!m) return 0;
  const times = [m.updated_at, m.last_seen, m.public_ip_at, m.disk?.updated_at];
  let best = 0;
  for (const iso of times) {
    if (!iso) continue;
    const ms = Date.parse(iso);
    if (Number.isFinite(ms) && ms > best) best = ms;
  }
  return best;
}

/**
 * Live-data hook for a single machine's details drawer.
 * Uses the existing machine object from useMachines immediately when available,
 * fetches GET /api/machines/{id} for full details + watchdog stage, and stays
 * synchronized with /ws/events ("machine" events + reconnect) and /ws/watchdog.
 */
export function useMachineDetails(machineId, listMachine) {
  const [fetchedMachine, setFetchedMachine] = useState(null);
  const [watchdog, setWatchdog] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [notFound, setNotFound] = useState(false);

  const activeIdRef = useRef(machineId);
  activeIdRef.current = machineId;
  const debounceRef = useRef(0);

  const refresh = useCallback(async () => {
    const id = activeIdRef.current;
    if (!id) return;
    const r = await api('GET', `/api/machines/${encodeURIComponent(id)}`);
    if (activeIdRef.current !== id) return;
    setLoading(false);
    if (r.status === 404) {
      setNotFound(true);
      setWatchdog(null);
      setError(r.data.error || 'This machine is no longer in the registry.');
      return;
    }
    if (r.status === 503) {
      setNotFound(false);
      setWatchdog(null);
      setError('');
      return;
    }
    if (!r.ok) {
      setError(r.data.error || `Could not load machine details (${r.status})`);
      return;
    }
    setNotFound(false);
    setError('');
    setFetchedMachine(r.data.machine || null);
    setWatchdog(r.data.watchdog || null);
  }, []);

  const refreshSoon = useCallback(() => {
    if (debounceRef.current) window.clearTimeout(debounceRef.current);
    debounceRef.current = window.setTimeout(() => {
      debounceRef.current = 0;
      refresh();
    }, 150);
  }, [refresh]);

  useEffect(() => {
    if (!machineId) {
      setFetchedMachine(null);
      setWatchdog(null);
      setLoading(false);
      setError('');
      setNotFound(false);
      return undefined;
    }
    setFetchedMachine(null);
    setWatchdog(null);
    setError('');
    setNotFound(false);
    setLoading(true);
    refresh();
    const iv = window.setInterval(refresh, 15000);
    return () => {
      window.clearInterval(iv);
      if (debounceRef.current) {
        window.clearTimeout(debounceRef.current);
        debounceRef.current = 0;
      }
    };
  }, [machineId, refresh]);

  useEvents(
    (msg) => {
      if (!machineId || !msg || msg.type !== 'machine') return;
      if (!msg.machine || msg.machine.id === machineId) {
        if (msg.machine && msg.machine.id === machineId) {
          setFetchedMachine(msg.machine);
          setNotFound(false);
        }
        refreshSoon();
      }
    },
    () => {
      if (machineId) refresh();
    }
  );

  // When watchdog state is active for this machine, subscribe to /ws/watchdog
  // so ladder stage and counters update live while the drawer is open.
  const hasWatchdog = Boolean(machineId && watchdog);
  useEffect(() => {
    if (!hasWatchdog) return undefined;
    let closed = false;
    let attempt = 0;
    let timer = 0;
    let ws = null;

    function connect() {
      if (closed) return;
      const proto = window.location.protocol === 'https:' ? 'wss://' : 'ws://';
      ws = new WebSocket(`${proto}${window.location.host}/ws/watchdog`);
      ws.onopen = () => {
        const again = attempt > 0;
        attempt = 0;
        if (again) refresh();
      };
      ws.onmessage = (ev) => {
        let m;
        try {
          m = JSON.parse(ev.data);
        } catch {
          return;
        }
        if (m && m.type === 'event' && (!m.event?.machine || m.event.machine === machineId)) {
          refreshSoon();
        }
      };
      ws.onclose = () => {
        if (closed) return;
        attempt += 1;
        timer = window.setTimeout(connect, Math.min(10000, 500 * attempt));
      };
    }

    connect();
    return () => {
      closed = true;
      if (timer) window.clearTimeout(timer);
      if (ws) ws.close();
    };
  }, [hasWatchdog, machineId, refresh, refreshSoon]);

  const machine = useMemo(() => {
    if (!machineId) return null;
    if (notFound) return listMachine || fetchedMachine;
    const fetched = fetchedMachine && fetchedMachine.id === machineId ? fetchedMachine : null;
    const listed = listMachine && listMachine.id === machineId ? listMachine : null;
    if (!fetched) return listed;
    if (!listed) return fetched;
    return maxTimestampMs(listed) > maxTimestampMs(fetched) ? listed : fetched;
  }, [machineId, fetchedMachine, listMachine, notFound]);

  return { machine, watchdog, loading, error, notFound, refresh };
}
