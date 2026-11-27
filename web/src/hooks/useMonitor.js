import { useCallback, useEffect, useRef, useState } from 'react';
import { api } from '../lib/api';
import { useEvents } from '../lib/events';

const MAX_RETAIN_MS = 600000; // 10 minutes, matching legacy live.js

function keyOf(p) {
  return `${p.agent}\u0000${p.target}`;
}

/**
 * Live-data and configuration hook for the Monitor page.
 * Feeds live probe series from GET /api/monitor/live + /ws/monitor,
 * and manages global probe settings and monitor jobs via /api/monitor/config.
 */
export function useMonitor() {
  const [win, setWin] = useState(60);
  const [paused, setPaused] = useState(false);
  const [noBackend, setNoBackend] = useState(false);

  const seriesMapRef = useRef({});
  const [liveSnapshot, setLiveSnapshot] = useState({
    snapNowMs: 0,
    windowSec: 60,
    agents: [],
    jobs: [],
    targets: {},
    commands: [],
    intervals: [],
  });
  const [liveLoaded, setLiveLoaded] = useState(false);
  const [liveError, setLiveError] = useState('');
  const [renderTick, setRenderTick] = useState(0);

  const [config, setConfig] = useState(null);
  const [configLoaded, setConfigLoaded] = useState(false);
  const [configError, setConfigError] = useState('');

  const mountedRef = useRef(true);
  const pausedRef = useRef(paused);
  const winRef = useRef(win);
  const debounceRef = useRef(0);
  const wsDebounceRef = useRef(0);

  pausedRef.current = paused;
  winRef.current = win;

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (debounceRef.current) window.clearTimeout(debounceRef.current);
      if (wsDebounceRef.current) window.clearTimeout(wsDebounceRef.current);
    };
  }, []);

  const loadLive = useCallback(async (overrideWin) => {
    const w = overrideWin ?? winRef.current;
    const r = await api('GET', `/api/monitor/live?window=${w}`);
    if (!mountedRef.current) return;
    setLiveLoaded(true);
    if (r.status === 503) {
      setNoBackend(true);
      setLiveError('');
      seriesMapRef.current = {};
      setLiveSnapshot({
        snapNowMs: Date.now(),
        windowSec: w,
        agents: [],
        jobs: [],
        targets: {},
        commands: [],
        intervals: [],
      });
      return;
    }
    if (!r.ok) {
      setLiveError(r.data.error || 'The monitor data is unavailable.');
      return;
    }
    setNoBackend(false);
    setLiveError('');
    const d = r.data || {};
    const nextSeries = {};
    (d.series || []).forEach((sd) => {
      nextSeries[keyOf(sd)] = {
        agent: sd.agent,
        target: sd.target,
        history_key: sd.history_key,
        type: sd.type,
        points: (sd.points || []).map((p) => ({
          t: new Date(p.t).getTime(),
          ms: p.ms,
          ok: p.ok,
        })),
      };
    });
    seriesMapRef.current = nextSeries;
    setLiveSnapshot({
      snapNowMs: d.now ? new Date(d.now).getTime() : Date.now(),
      windowSec: d.window_sec || w,
      agents: Array.isArray(d.agents) ? d.agents : [],
      jobs: Array.isArray(d.jobs) ? d.jobs : [],
      targets: d.targets || {},
      commands: (Array.isArray(d.commands) ? d.commands : []).map((c) => ({
        session: c.session,
        machine: c.machine,
        command: c.command,
        user: c.user,
        mode: c.mode,
        startMs: new Date(c.start).getTime(),
        endMs: c.end ? new Date(c.end).getTime() : null,
        packets: c.packets,
        interval_packets: Array.isArray(c.interval_packets) ? c.interval_packets : null,
      })),
      intervals: (Array.isArray(d.intervals) ? d.intervals : []).map((iv) => ({
        startMs: new Date(iv.start).getTime(),
        endMs: new Date(iv.end).getTime(),
        packets: iv.packets || 0,
      })),
    });
    setRenderTick((t) => t + 1);
  }, []);

  const loadConfig = useCallback(async () => {
    const r = await api('GET', '/api/monitor/config');
    if (!mountedRef.current) return;
    setConfigLoaded(true);
    if (r.status === 503) {
      setNoBackend(true);
      setConfigError('');
      setConfig(null);
      return;
    }
    if (!r.ok) {
      setConfigError(r.data.error || 'Unavailable.');
      return;
    }
    setNoBackend(false);
    setConfigError('');
    setConfig(r.data);
  }, []);

  const refreshSoon = useCallback(() => {
    if (debounceRef.current) window.clearTimeout(debounceRef.current);
    debounceRef.current = window.setTimeout(() => {
      debounceRef.current = 0;
      if (!pausedRef.current) loadLive();
      loadConfig();
    }, 300);
  }, [loadLive, loadConfig]);

  // Initial load + 5s snapshot poll + 1s render tick (when not paused).
  useEffect(() => {
    loadLive(win);
  }, [win, loadLive]);

  useEffect(() => {
    loadConfig();
    const pollIv = window.setInterval(() => {
      if (!pausedRef.current && !document.hidden) {
        loadLive();
      }
    }, 5000);
    const tickIv = window.setInterval(() => {
      if (!pausedRef.current && !document.hidden) {
        setRenderTick((t) => t + 1);
      }
    }, 1000);
    return () => {
      window.clearInterval(pollIv);
      window.clearInterval(tickIv);
    };
  }, [loadLive, loadConfig]);

  // Subscribe to shell /ws/events for monitor agent online/offline updates.
  useEvents(
    (msg) => {
      if (msg && msg.type === 'machine') {
        refreshSoon();
      }
    },
    () => {
      if (!pausedRef.current) loadLive();
      loadConfig();
    }
  );

  // Subscribe to /ws/monitor for live probe samples.
  useEffect(() => {
    if (noBackend) return undefined;
    let closed = false;
    let attempt = 0;
    let timer = 0;
    let ws = null;

    function scheduleRender() {
      if (wsDebounceRef.current) return;
      wsDebounceRef.current = window.setTimeout(() => {
        wsDebounceRef.current = 0;
        if (!mountedRef.current || pausedRef.current) return;
        setRenderTick((t) => t + 1);
      }, 250);
    }

    function connect() {
      if (closed) return;
      const proto = window.location.protocol === 'https:' ? 'wss://' : 'ws://';
      ws = new WebSocket(`${proto}${window.location.host}/ws/monitor`);
      ws.onopen = () => {
        const again = attempt > 0;
        attempt = 0;
        if (again && !pausedRef.current) {
          loadLive();
        }
      };
      ws.onmessage = (ev) => {
        let m;
        try {
          m = JSON.parse(ev.data);
        } catch {
          return;
        }
        if (!m || m.type !== 'samples') return;
        const now = Date.now();
        const cutoff = now - MAX_RETAIN_MS;
        const map = seriesMapRef.current;
        (m.points || []).forEach((p) => {
          const k = keyOf(p);
          let s = map[k];
          if (!s) {
            s = map[k] = {
              agent: p.agent,
              target: p.target,
              history_key: p.history_key,
              type: p.type,
              points: [],
            };
          }
          if (p.history_key) s.history_key = p.history_key;
          s.points.push({ t: new Date(p.t).getTime(), ms: p.ms, ok: p.ok });
          if (s.points.length > 500) {
            s.points = s.points.filter((pt) => pt.t >= cutoff);
          }
        });
        scheduleRender();
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
  }, [noBackend, loadLive]);

  const togglePause = useCallback(() => {
    setPaused((was) => {
      const next = !was;
      if (!next) {
        loadLive();
      }
      return next;
    });
  }, [loadLive]);

  const saveSettings = useCallback(async (payload) => {
    const r = await api('POST', '/api/monitor/settings', payload);
    if (r.ok) await loadConfig();
    return r;
  }, [loadConfig]);

  const createJob = useCallback(async (payload) => {
    const r = await api('POST', '/api/monitor/jobs', payload);
    if (r.ok) {
      await Promise.all([loadConfig(), loadLive()]);
    }
    return r;
  }, [loadConfig, loadLive]);

  const deleteJob = useCallback(async (id) => {
    const r = await api('POST', `/api/monitor/jobs/${encodeURIComponent(id)}/delete`, {});
    if (r.ok) {
      await Promise.all([loadConfig(), loadLive()]);
    }
    return r;
  }, [loadConfig, loadLive]);

  const setTargetInterval = useCallback(async (jobId, targetId, intervalMs) => {
    return api('POST', `/api/monitor/jobs/${encodeURIComponent(jobId)}/interval`, {
      target: targetId,
      interval_ms: intervalMs,
    });
  }, []);

  return {
    win,
    setWin,
    paused,
    togglePause,
    noBackend,
    seriesMapRef,
    liveSnapshot,
    liveLoaded,
    liveError,
    renderTick,
    loadLive,
    config,
    configLoaded,
    configError,
    loadConfig,
    saveSettings,
    createJob,
    deleteJob,
    setTargetInterval,
  };
}
