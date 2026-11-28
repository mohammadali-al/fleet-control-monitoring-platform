import { useCallback, useEffect, useRef, useState } from 'react';

export const HISTORY_METRICS = [
  { id: 'latency_ms', label: 'Response time (ms)', unit: 'ms' },
  { id: 'ok', label: 'Availability (%)', unit: '%' },
  { id: 'loss', label: 'Packet loss (%)', unit: '%' },
  { id: 'status', label: 'HTTP status code', unit: '' },
];

export const OVERVIEW_SPANS = [
  { id: '24h', label: 'Last 24 hours', ms: 24 * 3600 * 1000 },
  { id: '7d', label: 'Last 7 days', ms: 7 * 24 * 3600 * 1000 },
  { id: '30d', label: 'Last 30 days', ms: 30 * 24 * 3600 * 1000 },
  { id: '90d', label: 'Last 90 days', ms: 90 * 24 * 3600 * 1000 },
];

export const ZOOM_LEVELS = [
  { id: 'overview', label: 'Overview', spanMs: null },
  { id: 'day', label: 'Day', spanMs: 24 * 3600 * 1000 },
  { id: 'hour', label: 'Hour', spanMs: 3600 * 1000 },
  { id: 'detail', label: 'Detail', spanMs: 10 * 60 * 1000 },
];

const CLEAN_STEPS_SEC = [
  1, 2, 5, 10, 15, 30,
  60, 120, 300, 600, 900, 1800,
  3600, 7200, 14400, 21600, 43200, 86400,
];

const MIN_ZOOM_MS = 15 * 1000;
const TARGET_BUCKETS = 168;

/**
 * Picks a progressively finer query_range step (in seconds) as the operator zooms in.
 * Returns 0 (raw /api/v1/export samples) only when zoomed into <= 60 s.
 */
export function stepSecForRange(fromMs, toMs) {
  const spanSec = Math.max(1, (toMs - fromMs) / 1000);
  if (spanSec <= 60) return 0;
  const ideal = spanSec / TARGET_BUCKETS;
  for (let i = 0; i < CLEAN_STEPS_SEC.length; i++) {
    if (CLEAN_STEPS_SEC[i] >= ideal) return CLEAN_STEPS_SEC[i];
  }
  return 86400;
}

/**
 * Maps a visible time span (ms) to its semantic zoom tier:
 * overview (>36h) -> day (<=36h) -> hour (<=3h) -> detail (<=20m).
 */
export function zoomLevelForSpan(spanMs, overviewSpanMs) {
  if (overviewSpanMs && spanMs >= overviewSpanMs * 0.85) return 'overview';
  if (spanMs > 36 * 3600 * 1000) return 'overview';
  if (spanMs > 3 * 3600 * 1000) return 'day';
  if (spanMs > 20 * 60 * 1000) return 'hour';
  return 'detail';
}

export function formatStepLabel(stepSec) {
  if (!stepSec || stepSec <= 0) return 'raw samples';
  if (stepSec < 60) return `${stepSec} s`;
  if (stepSec < 3600) return `${Math.round(stepSec / 60)} min`;
  if (stepSec < 86400) return `${(stepSec / 3600).toFixed(stepSec % 3600 === 0 ? 0 : 1)} h`;
  return `${Math.round(stepSec / 86400)} d`;
}

function getSpanMs(spanId) {
  return (OVERVIEW_SPANS.find((s) => s.id === spanId) || OVERVIEW_SPANS[1]).ms;
}

function clampRange(fromMs, toMs, minMs, maxMs) {
  const totalBounds = Math.max(MIN_ZOOM_MS, maxMs - minMs);
  const span = Math.min(totalBounds, Math.max(MIN_ZOOM_MS, toMs - fromMs));
  let a = Math.max(minMs, fromMs);
  let b = a + span;
  if (b > maxMs) {
    b = maxMs;
    a = Math.max(minMs, b - span);
  }
  return { fromMs: Math.round(a), toMs: Math.round(b) };
}

async function fetchHistoryJSON(params, signal) {
  const url = `/api/monitor/history?${params.toString()}`;
  const resp = await fetch(url, {
    method: 'GET',
    headers: { Accept: 'application/json' },
    credentials: 'same-origin',
    signal,
  });
  const txt = await resp.text();
  let data = {};
  if (txt) {
    try {
      data = JSON.parse(txt);
    } catch {
      data = { error: txt || resp.statusText };
    }
  }
  return { ok: resp.ok, status: resp.status, data: data || {} };
}

function normalizeSeries(list) {
  if (!Array.isArray(list)) return [];
  return list.map((s) => ({
    metric: s.metric || '',
    agent: s.agent || '',
    target: s.target || '',
    history_key: s.history_key || '',
    type: s.type || '',
    points: (Array.isArray(s.points) ? s.points : []).map((p) => ({
      tMs: new Date(p.t).getTime(),
      value: Number(p.value) || 0,
    })),
  }));
}

/**
 * Semantic-zoom history hook for the Monitor page.
 * Queries GET /api/monitor/history starting from a coarse whole-timeline overview
 * and re-queries with progressively finer `step` as the operator zooms in.
 */
export function useMonitorHistory({ enabled = false } = {}) {
  const [overviewSpanId, setOverviewSpanId] = useState('7d');
  const [metric, setMetric] = useState('latency_ms');
  const [historyKey, setHistoryKey] = useState('');
  const [probeType, setProbeType] = useState('');

  const [anchorNowMs, setAnchorNowMs] = useState(() => Date.now());
  const overviewSpanMs = getSpanMs(overviewSpanId);
  const boundsMinMs = anchorNowMs - overviewSpanMs;
  const boundsMaxMs = anchorNowMs;

  const [range, setRange] = useState(() => {
    const now = Date.now();
    return { fromMs: now - getSpanMs('7d'), toMs: now };
  });
  const [zoomStack, setZoomStack] = useState([]);

  const [series, setSeries] = useState([]);
  const [okSeries, setOkSeries] = useState([]);
  const [stepSec, setStepSec] = useState(() => stepSecForRange(range.fromMs, range.toMs));
  const [loading, setLoading] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [noStore, setNoStore] = useState(false);
  const [error, setError] = useState('');
  const [reloadNonce, setReloadNonce] = useState(0);

  const mountedRef = useRef(true);
  const abortRef = useRef(null);
  const reqSeqRef = useRef(0);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (abortRef.current) abortRef.current.abort();
    };
  }, []);

  const currentSpanMs = Math.max(1, range.toMs - range.fromMs);
  const zoomLevel = zoomLevelForSpan(currentSpanMs, overviewSpanMs);
  const isZoomed = zoomLevel !== 'overview' || zoomStack.length > 0 || currentSpanMs < overviewSpanMs * 0.95;
  const canZoomIn = currentSpanMs > MIN_ZOOM_MS * 1.1;
  const canZoomOut = isZoomed;

  useEffect(() => {
    if (!enabled) return undefined;

    if (abortRef.current) abortRef.current.abort();
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    const seq = ++reqSeqRef.current;

    const plannedStep = stepSecForRange(range.fromMs, range.toMs);
    const buildParams = (mName) => {
      const p = new URLSearchParams();
      p.set('metric', mName);
      p.set('from', new Date(range.fromMs).toISOString());
      p.set('to', new Date(range.toMs).toISOString());
      if (plannedStep > 0) {
        p.set('step', `${plannedStep}s`);
      }
      if (historyKey) p.set('history_key', historyKey);
      if (probeType) p.set('type', probeType);
      return p;
    };

    setLoading(true);
    setError('');

    const reqs = [fetchHistoryJSON(buildParams(metric), ctrl.signal)];
    if (metric !== 'ok') {
      reqs.push(fetchHistoryJSON(buildParams('ok'), ctrl.signal));
    }

    Promise.all(reqs)
      .then((results) => {
        if (!mountedRef.current || ctrl.signal.aborted || seq !== reqSeqRef.current) return;
        const primary = results[0];
        const okRes = metric === 'ok' ? primary : results[1];
        setLoaded(true);
        setLoading(false);

        if (primary.status === 503) {
          setNoStore(true);
          setError('');
          setSeries([]);
          setOkSeries([]);
          setStepSec(plannedStep);
          return;
        }
        if (!primary.ok) {
          setNoStore(false);
          setError(primary.data.error || `Could not load history (${primary.status}).`);
          return;
        }

        setNoStore(false);
        setError('');
        const normPrimary = normalizeSeries(primary.data.series);
        setSeries(normPrimary);
        setStepSec(
          typeof primary.data.step_sec === 'number' ? primary.data.step_sec : plannedStep
        );
        if (okRes && okRes.ok) {
          setOkSeries(normalizeSeries(okRes.data.series));
        } else {
          setOkSeries([]);
        }
      })
      .catch((err) => {
        if (!mountedRef.current || ctrl.signal.aborted || err?.name === 'AbortError' || seq !== reqSeqRef.current) {
          return;
        }
        setLoaded(true);
        setLoading(false);
        setError(`Cannot reach the main server: ${err?.message || 'network error'}`);
      });

    return () => {
      ctrl.abort();
    };
  }, [enabled, metric, range.fromMs, range.toMs, historyKey, probeType, reloadNonce]);

  const setOverviewSpan = useCallback((nextSpanId) => {
    const now = Date.now();
    const spanMs = getSpanMs(nextSpanId);
    setOverviewSpanId(nextSpanId);
    setAnchorNowMs(now);
    setZoomStack([]);
    setRange({ fromMs: now - spanMs, toMs: now });
  }, []);

  const zoomToRange = useCallback(
    (nextFromMs, nextToMs, { pushHistory = true } = {}) => {
      const clamped = clampRange(nextFromMs, nextToMs, boundsMinMs, boundsMaxMs);
      setRange((prev) => {
        if (prev.fromMs === clamped.fromMs && prev.toMs === clamped.toMs) return prev;
        if (pushHistory) {
          setZoomStack((stk) => [...stk.slice(-15), prev]);
        }
        return clamped;
      });
    },
    [boundsMinMs, boundsMaxMs]
  );

  const selectZoomPreset = useCallback(
    (levelId) => {
      if (levelId === 'overview') {
        const now = Date.now();
        setAnchorNowMs(now);
        setZoomStack([]);
        setRange({ fromMs: now - overviewSpanMs, toMs: now });
        return;
      }
      const def = ZOOM_LEVELS.find((z) => z.id === levelId);
      if (!def || !def.spanMs) return;
      const targetSpan = Math.min(overviewSpanMs, def.spanMs);
      setRange((prev) => {
        const prevSpan = prev.toMs - prev.fromMs;
        // If zooming in from full overview, anchor at the most recent end; otherwise preserve center.
        const mid = prevSpan >= overviewSpanMs * 0.9 ? boundsMaxMs - targetSpan / 2 : (prev.fromMs + prev.toMs) / 2;
        const clamped = clampRange(mid - targetSpan / 2, mid + targetSpan / 2, boundsMinMs, boundsMaxMs);
        if (clamped.fromMs !== prev.fromMs || clamped.toMs !== prev.toMs) {
          setZoomStack((stk) => [...stk.slice(-15), prev]);
        }
        return clamped;
      });
    },
    [overviewSpanMs, boundsMinMs, boundsMaxMs]
  );

  const zoomIn = useCallback(
    (centerFrac = 0.5) => {
      setRange((prev) => {
        const span = prev.toMs - prev.fromMs;
        if (span <= MIN_ZOOM_MS * 1.05) return prev;
        const nextSpan = Math.max(MIN_ZOOM_MS, span * 0.5);
        const anchor = prev.fromMs + span * Math.max(0, Math.min(1, centerFrac));
        const nextFrom = anchor - nextSpan * Math.max(0, Math.min(1, centerFrac));
        const clamped = clampRange(nextFrom, nextFrom + nextSpan, boundsMinMs, boundsMaxMs);
        setZoomStack((stk) => [...stk.slice(-15), prev]);
        return clamped;
      });
    },
    [boundsMinMs, boundsMaxMs]
  );

  const zoomOut = useCallback(
    (centerFrac = 0.5) => {
      if (zoomStack.length > 0) {
        const prevTop = zoomStack[zoomStack.length - 1];
        setZoomStack((stk) => stk.slice(0, -1));
        setRange(clampRange(prevTop.fromMs, prevTop.toMs, boundsMinMs, boundsMaxMs));
        return;
      }
      setRange((prev) => {
        const span = prev.toMs - prev.fromMs;
        const nextSpan = Math.min(overviewSpanMs, span * 2);
        if (nextSpan >= overviewSpanMs * 0.95) {
          return { fromMs: boundsMinMs, toMs: boundsMaxMs };
        }
        const anchor = prev.fromMs + span * Math.max(0, Math.min(1, centerFrac));
        const nextFrom = anchor - nextSpan * Math.max(0, Math.min(1, centerFrac));
        return clampRange(nextFrom, nextFrom + nextSpan, boundsMinMs, boundsMaxMs);
      });
    },
    [zoomStack, overviewSpanMs, boundsMinMs, boundsMaxMs]
  );

  const panBy = useCallback(
    (fraction) => {
      setRange((prev) => {
        const span = prev.toMs - prev.fromMs;
        const delta = span * fraction;
        return clampRange(prev.fromMs + delta, prev.toMs + delta, boundsMinMs, boundsMaxMs);
      });
    },
    [boundsMinMs, boundsMaxMs]
  );

  const resetOverview = useCallback(() => {
    const now = Date.now();
    setAnchorNowMs(now);
    setZoomStack([]);
    setRange({ fromMs: now - overviewSpanMs, toMs: now });
  }, [overviewSpanMs]);

  const reload = useCallback(() => {
    if (!isZoomed) {
      const now = Date.now();
      setAnchorNowMs(now);
      setRange({ fromMs: now - overviewSpanMs, toMs: now });
    }
    setReloadNonce((n) => n + 1);
  }, [isZoomed, overviewSpanMs]);

  return {
    metric,
    setMetric,
    overviewSpanId,
    setOverviewSpan,
    historyKey,
    setHistoryKey,
    probeType,
    setProbeType,
    range,
    bounds: { minMs: boundsMinMs, maxMs: boundsMaxMs },
    zoomLevel,
    stepSec,
    canZoomIn,
    canZoomOut,
    isZoomed,
    series,
    okSeries,
    loading,
    loaded,
    noStore,
    error,
    zoomToRange,
    selectZoomPreset,
    zoomIn,
    zoomOut,
    panBy,
    resetOverview,
    reload,
  };
}
