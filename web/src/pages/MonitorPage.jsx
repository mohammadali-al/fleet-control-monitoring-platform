import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { StatusAlert } from '../components/StatusAlert';
import { useMachines } from '../hooks/useMachines';
import { useMonitor } from '../hooks/useMonitor';
import {
  HISTORY_METRICS,
  OVERVIEW_SPANS,
  ZOOM_LEVELS,
  formatStepLabel,
  useMonitorHistory,
} from '../hooks/useMonitorHistory';
import { useTheme } from '../lib/theme';
import { usePageTitle } from '../lib/title';

const DASH_ARRAYS = ['', '6 3', '2 3', '8 3 2 3', '1 4'];
const GRAFANA_DASHBOARDS = [
  { uid: 'fleet-overview', slug: 'fleet-monitor-overview', label: 'Fleet Monitor — Overview' },
  { uid: 'fleet-agents', slug: 'fleet-monitor-per-agent-breakdown', label: 'Fleet Monitor — Per-Agent Breakdown' },
];
const PROBE_TYPES = [
  { id: 'icmp', label: 'ICMP', hint: 'Echo to IP' },
  { id: 'tcp', label: 'TCP', hint: 'Connect to origin host:port' },
  { id: 'http', label: 'HTTP(S)', hint: 'Request each path' },
  { id: 'dns', label: 'DNS', hint: 'Resolve query name' },
];
const NUM_BINS = 40;
const NUM_COLS = 12;

function median(arr) {
  if (!arr.length) return null;
  const s = arr.slice().sort((x, y) => x - y);
  const m = Math.floor(s.length / 2);
  return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
}

function pct(arr, p) {
  if (!arr.length) return null;
  const s = arr.slice().sort((x, y) => x - y);
  return s[Math.min(s.length - 1, Math.floor(p * s.length))];
}

function fmtMs(v) {
  if (v == null) return '-';
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ms`;
}

function fmtHistMetric(metric, v) {
  if (v == null || Number.isNaN(v)) return '-';
  if (metric === 'latency_ms') return fmtMs(v);
  if (metric === 'ok' || metric === 'loss') return `${(v * 100).toFixed(1)} %`;
  if (metric === 'status') return String(Math.round(v));
  return v.toFixed(2);
}

function pad2(n) {
  return n < 10 ? `0${n}` : String(n);
}

function fmtHistAxis(ms, spanMs) {
  const d = new Date(ms);
  const md = `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
  const hm = `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
  if (spanMs > 36 * 3600 * 1000) return md;
  if (spanMs > 3 * 3600 * 1000) return `${md} ${hm}`;
  if (spanMs > 20 * 60 * 1000) return hm;
  return `${hm}:${pad2(d.getSeconds())}`;
}

function fmtHistFull(ms) {
  const d = new Date(ms);
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(
    d.getMinutes()
  )}:${pad2(d.getSeconds())}`;
}

function fmtSpanDuration(ms) {
  const sec = Math.max(1, Math.round(ms / 1000));
  if (sec < 120) return `${sec} s`;
  const min = Math.round(sec / 60);
  if (min < 120) return `${min} min`;
  const hr = sec / 3600;
  if (hr < 48) return `${hr.toFixed(hr % 1 === 0 ? 0 : 1)} h`;
  const days = Math.round(hr / 24);
  return `${days} d`;
}

function buildSemanticSegments(fromMs, toMs, zoomLevel, metric, primarySeries, okSeries) {
  const spanMs = Math.max(1, toMs - fromMs);
  const bucketCount = zoomLevel === 'overview' ? 24 : zoomLevel === 'day' ? 16 : 12;
  const bucketMs = spanMs / bucketCount;

  const raw = Array.from({ length: bucketCount }, (_, i) => ({
    fromMs: fromMs + i * bucketMs,
    toMs: i === bucketCount - 1 ? toMs : fromMs + (i + 1) * bucketMs,
    vals: [],
    oks: [],
  }));

  const place = (tMs) => Math.min(bucketCount - 1, Math.max(0, Math.floor((tMs - fromMs) / bucketMs)));

  primarySeries.forEach((s) => {
    s.points.forEach((p) => {
      if (p.tMs >= fromMs && p.tMs <= toMs) {
        raw[place(p.tMs)].vals.push(p.value);
      }
    });
  });

  const okSrc = metric === 'ok' ? primarySeries : okSeries;
  okSrc.forEach((s) => {
    s.points.forEach((p) => {
      if (p.tMs >= fromMs && p.tMs <= toMs) {
        raw[place(p.tMs)].oks.push(p.value);
      }
    });
  });

  const classified = raw.map((b) => {
    const count = Math.max(b.vals.length, b.oks.length);
    const okPct = b.oks.length ? (b.oks.reduce((a, x) => a + x, 0) / b.oks.length) * 100 : null;
    const med = b.vals.length ? median(b.vals) : null;
    const p95 = b.vals.length ? pct(b.vals, 0.95) : null;
    let kind = 'ok';
    if (count === 0) kind = 'gap';
    else if (okPct != null && okPct < 80) kind = 'down';
    else if (okPct != null && okPct < 99) kind = 'warn';
    return { ...b, count, okPct, med, p95, kind };
  });

  // Semantic collapse: merge adjacent same-status intervals at coarse levels.
  const merged = [];
  classified.forEach((b) => {
    const prev = merged[merged.length - 1];
    const canMerge =
      prev &&
      prev.kind === b.kind &&
      (zoomLevel === 'overview' || (zoomLevel === 'day' && (b.kind === 'gap' || prev.spanBuckets < 3)));
    if (canMerge) {
      prev.toMs = b.toMs;
      prev.vals = prev.vals.concat(b.vals);
      prev.oks = prev.oks.concat(b.oks);
      prev.spanBuckets += 1;
    } else {
      merged.push({ ...b, spanBuckets: 1 });
    }
  });

  return merged.map((seg, idx) => {
    const count = Math.max(seg.vals.length, seg.oks.length);
    const okPct = seg.oks.length ? (seg.oks.reduce((a, x) => a + x, 0) / seg.oks.length) * 100 : null;
    const med = seg.vals.length ? median(seg.vals) : null;
    const p95 = seg.vals.length ? pct(seg.vals, 0.95) : null;
    const leftPct = ((seg.fromMs - fromMs) / spanMs) * 100;
    const widthPct = Math.max(1.2, ((seg.toMs - seg.fromMs) / spanMs) * 100);

    let label = 'No data';
    if (seg.kind !== 'gap') {
      const okShort = okPct != null ? `${okPct.toFixed(okPct === 100 ? 0 : 1)}% OK` : '';
      if (zoomLevel === 'overview') {
        const statusWord = seg.kind === 'ok' ? 'Healthy' : seg.kind === 'warn' ? 'Degraded' : 'Outage';
        if (widthPct >= 22 && med != null) {
          label = `${statusWord}${okShort ? ` · ${okShort}` : ''} · ${fmtHistMetric(metric, med)}`;
        } else if (widthPct >= 10) {
          label = okShort || fmtHistMetric(metric, med);
        } else {
          label = statusWord;
        }
      } else {
        const tStr = fmtHistAxis(seg.fromMs, spanMs);
        label = widthPct >= 10 && med != null ? `${tStr} · ${fmtHistMetric(metric, med)}` : tStr;
      }
    } else if (widthPct < 7) {
      label = '-';
    }

    const rangeText = `${fmtHistFull(seg.fromMs)} -> ${fmtHistFull(seg.toMs)}`;
    const tipLines = [
      rangeText,
      seg.kind === 'gap'
        ? 'No persisted samples in this interval'
        : `${count} sample(s)${okPct != null ? ` · ${okPct.toFixed(1)}% OK` : ''}`,
    ];
    if (med != null) {
      tipLines.push(`Median ${fmtHistMetric(metric, med)}${p95 != null ? ` · P95 ${fmtHistMetric(metric, p95)}` : ''}`);
    }
    tipLines.push('Click to zoom into this range');

    return {
      key: `${idx}-${Math.round(seg.fromMs)}`,
      fromMs: seg.fromMs,
      toMs: seg.toMs,
      kind: seg.kind,
      count,
      okPct,
      med,
      p95,
      leftPct,
      widthPct,
      label,
      rangeText,
      tipLines,
    };
  });
}

function groupOf(view, s, targets) {
  if (view === 'machine') return s.agent;
  if (view === 'target') return s.target + (targets[s.target] ? ` (${targets[s.target]})` : '');
  return 'All monitors';
}

function HoverTip({ tip }) {
  const ref = useRef(null);
  const [pos, setPos] = useState({ left: 8, top: 8 });

  useLayoutEffect(() => {
    const el = ref.current;
    if (!tip || !el) return;
    const left = Math.max(8, Math.min(tip.x - el.offsetWidth / 2, window.innerWidth - el.offsetWidth - 8));
    const top = Math.max(8, tip.y - el.offsetHeight - 10);
    setPos({ left, top });
  }, [tip]);

  if (!tip || !tip.lines || tip.lines.length === 0) return null;
  return (
    <div
      ref={ref}
      className="tip"
      role="tooltip"
      style={{ left: `${pos.left}px`, top: `${pos.top}px` }}
    >
      {tip.lines.map((line, i) =>
        i === 0 ? (
          <b key={i}>{line}</b>
        ) : (
          <span key={i}>
            {line}
            <br />
          </span>
        )
      )}
    </div>
  );
}

export function MonitorPage() {
  usePageTitle('Monitor data');

  const { theme } = useTheme();
  const { summary: machinesSummary } = useMachines();
  const {
    win,
    setWin,
    paused,
    togglePause,
    noBackend,
    seriesMapRef,
    liveSnapshot,
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
  } = useMonitor();

  const [activeTab, setActiveTab] = useState('live'); // 'live' | 'history' | 'grafana' | 'cfg'
  const tabRefs = useRef({});
  const [status, setStatus] = useState(null);
  const [grafanaDashUid, setGrafanaDashUid] = useState(GRAFANA_DASHBOARDS[0].uid);
  const [grafanaMounted, setGrafanaMounted] = useState(false);
  const [grafanaNonce, setGrafanaNonce] = useState(0);

  // History semantic-zoom state
  const history = useMonitorHistory({ enabled: activeTab === 'history' });
  const [histGroupMode, setHistGroupMode] = useState('auto'); // 'auto' | 'merged' | 'target' | 'series'
  const [brush, setBrush] = useState(null); // { anchorFrac, curFrac }
  const histPlotRef = useRef(null);
  const wheelAccRef = useRef({ timer: 0, factor: 1, centerFrac: 0.5 });

  // Live chart filters
  const [view, setView] = useState('merged'); // 'merged' | 'machine' | 'target'
  const [selectedKey, setSelectedKey] = useState('');
  const [tip, setTip] = useState(null);

  // Global defaults form state
  const [defForm, setDefForm] = useState({
    interval_ms: '',
    http: '',
    tcp: '',
    icmp: '',
    dns: '',
  });
  const [savingDefs, setSavingDefs] = useState(false);

  // Create job form state
  const [jobForm, setJobForm] = useState({
    url: '',
    ip: '',
    paths: '',
    interval_ms: '',
    dns_name: '',
    dns_server: '',
  });
  const [probeTypes, setProbeTypes] = useState(['icmp']);
  const [probeTypesCustom, setProbeTypesCustom] = useState(false);
  const [creatingJob, setCreatingJob] = useState(false);

  // Sync defaults form when config loads
  useEffect(() => {
    if (!config || !config.settings) return;
    const st = config.settings;
    const to = st.timeout_ms || {};
    setDefForm({
      interval_ms: st.interval_ms ? String(st.interval_ms) : '',
      http: to.http ? String(to.http) : '',
      tcp: to.tcp ? String(to.tcp) : '',
      icmp: to.icmp ? String(to.icmp) : '',
      dns: to.dns ? String(to.dns) : '',
    });
  }, [config]);

  // Clear selectedKey if that job was deleted
  useEffect(() => {
    if (!selectedKey) return;
    const exists = liveSnapshot.jobs.some((j) => j.key === selectedKey);
    if (!exists) setSelectedKey('');
  }, [liveSnapshot.jobs, selectedKey]);

  // Escape closes hover tooltip
  useEffect(() => {
    if (!tip) return undefined;
    const onKeyDown = (e) => {
      if (e.key === 'Escape') setTip(null);
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [tip]);

  // Freeze render timestamp while paused so the window stays still
  const frozenNowRef = useRef(Date.now());
  if (!paused) {
    frozenNowRef.current = Date.now();
  }
  const renderNow = frozenNowRef.current;

  // Derived live chart, KPIs, overlay, hover columns, and data table
  const liveView = useMemo(() => {
    // renderTick is referenced so this memo recomputes on WS batches and 1s ticks.
    void renderTick;
    const now = renderNow;
    const winMs = win * 1000;
    const from = now - winMs;
    const retainCutoff = now - 600000;

    const map = seriesMapRef.current;
    const sel = [];
    Object.keys(map).forEach((k) => {
      const s = map[k];
      if (selectedKey && s.history_key !== selectedKey) return;
      s.points = s.points.filter((p) => p.t >= retainCutoff);
      const pts = s.points.filter((p) => p.t >= from && p.t <= now + 2000);
      if (pts.length) sel.push({ s, pts });
    });

    const all = [];
    sel.forEach((x) => {
      for (let i = 0; i < x.pts.length; i++) all.push(x.pts[i]);
    });

    const oks = all.filter((p) => p.ok);
    const mss = oks.map((p) => p.ms);
    const lastT = all.reduce((acc, p) => Math.max(acc, p.t), 0);
    const typeSet = {};
    sel.forEach((x) => {
      if (x.s.type) typeSet[x.s.type] = true;
    });
    const typeKeys = Object.keys(typeSet);

    // Group into chart lines
    const groups = {};
    const order = [];
    sel.forEach((x) => {
      const g = groupOf(view, x.s, liveSnapshot.targets);
      if (!groups[g]) {
        groups[g] = [];
        order.push(g);
      }
      for (let i = 0; i < x.pts.length; i++) groups[g].push(x.pts[i]);
    });
    order.sort();

    const binMs = winMs / NUM_BINS;
    let rawMaxMs = 10;
    const preLines = order.map((g) => {
      const bins = new Array(NUM_BINS);
      const fails = [];
      groups[g].forEach((p) => {
        if (!p.ok) {
          fails.push(p);
          return;
        }
        const b = Math.min(NUM_BINS - 1, Math.max(0, Math.floor((p.t - from) / binMs)));
        if (!bins[b]) bins[b] = [];
        bins[b].push(p.ms);
      });
      const pts = [];
      bins.forEach((arr, b) => {
        if (!arr) return;
        const v = median(arr);
        rawMaxMs = Math.max(rawMaxMs, v);
        pts.push([((b + 0.5) * 200) / NUM_BINS, v]);
      });
      return { name: g, pts, fails: fails.slice(-200) };
    });

    const maxMs = rawMaxMs * 1.15;
    const lines = preLines.map((ln, idx) => ({
      name: ln.name,
      colorIdx: (idx % 5) + 1,
      dash: DASH_ARRAYS[idx % 5],
      pointsAttr: ln.pts.map((p) => `${p[0].toFixed(1)},${(57 - (p[1] / maxMs) * 52).toFixed(1)}`).join(' '),
      fails: ln.fails.map((p) => ((p.t - from) / winMs) * 200),
    }));

    // Commands in live window
    const cmds = liveSnapshot.commands.filter((c) => (c.endMs || now) >= from && c.startMs <= now);
    const snapIvs = liveSnapshot.intervals || [];

    // Hover columns (12 intervals) with per-interval packet counts (GAP-22)
    const colMs = winMs / NUM_COLS;
    const cols = [];
    for (let i = 0; i < NUM_COLS; i++) {
      const a = from + i * colMs;
      const b = a + colMs;
      const inCol = cmds.filter((c) => c.startMs < b && (c.endMs || now) > a);
      const vals = all.filter((p) => p.ok && p.t >= a && p.t < b).map((p) => p.ms);

      // Map snapshot intervals whose midpoint falls in this column
      const matchedIdx = [];
      for (let j = 0; j < snapIvs.length; j++) {
        const mid = (snapIvs[j].startMs + snapIvs[j].endMs) / 2;
        if (mid >= a && (i === NUM_COLS - 1 ? mid <= b + colMs : mid < b)) {
          matchedIdx.push(j);
        }
      }

      let colPackets = 0;
      if (matchedIdx.length > 0) {
        matchedIdx.forEach((j) => {
          colPackets += snapIvs[j]?.packets || 0;
        });
      }

      const cmdBreakdowns = [];
      let anyPulled = false;
      inCol.forEach((c) => {
        if (c.packets != null) {
          anyPulled = true;
          let cp = 0;
          if (Array.isArray(c.interval_packets) && matchedIdx.length > 0) {
            matchedIdx.forEach((j) => {
              cp += c.interval_packets[j] || 0;
            });
          }
          cmdBreakdowns.push(`${c.command.slice(0, 24)} (${c.machine}): ${cp.toLocaleString()}`);
        }
      });

      let pktLine;
      if (inCol.length > 0 && !anyPulled) {
        pktLine = 'Packets: evidence not pulled yet';
      } else if (anyPulled && inCol.length > 1) {
        pktLine = `Packets in interval: ${colPackets.toLocaleString()} (${cmdBreakdowns.join('; ').slice(0, 120)})`;
      } else {
        pktLine = `Packets in interval: ${colPackets.toLocaleString()}`;
      }

      const tipLines = [
        `${Math.round((a - now) / 1000)} s to ${i === NUM_COLS - 1 ? 'now' : `${Math.round((b - now) / 1000)} s`}`,
        inCol.length
          ? `Commands: ${inCol.map((c) => `${c.command.slice(0, 40)} (${c.machine})`).join(', ').slice(0, 160)}`
          : 'No commands in this interval',
        pktLine,
        vals.length ? `Median ${fmtMs(median(vals))}` : 'No results in this interval',
      ];
      cols.push({ key: i, tipLines });
    }

    // Command overlay markers with session + per-interval packet counts (GAP-22)
    const markers = cmds.map((c) => {
      const a = Math.max(c.startMs, from);
      const b = Math.min(c.endMs || now, now);
      const left = ((a - from) / winMs) * 100;
      const width = Math.min(Math.max(0.8, ((b - a) / winMs) * 100), 100 - left);
      const dur = Math.max(0, Math.round(((c.endMs || now) - c.startMs) / 1000));
      const pkLine =
        c.packets == null
          ? 'packets: evidence not pulled yet'
          : `${c.packets.toLocaleString()} packets archived for the session`;

      const tipLines = [
        c.command.length > 60 ? `${c.command.slice(0, 60)}...` : c.command,
        `${c.machine} · ${dur} s${c.endMs ? '' : ' (running)'}`,
        pkLine,
      ];
      if (c.packets != null && Array.isArray(c.interval_packets)) {
        const parts = [];
        c.interval_packets.forEach((cnt, idx) => {
          if (cnt > 0 && snapIvs[idx]) {
            const sA = Math.round((snapIvs[idx].startMs - now) / 1000);
            const sB = idx === c.interval_packets.length - 1 ? 'now' : `${Math.round((snapIvs[idx].endMs - now) / 1000)} s`;
            parts.push(`${sA}..${sB}: ${cnt}`);
          }
        });
        tipLines.push(
          parts.length
            ? `Per interval: ${parts.slice(0, 6).join(', ')}${parts.length > 6 ? '...' : ''}`
            : 'Per interval: 0 packets in current window'
        );
      }
      return {
        session: c.session,
        leftPct: left.toFixed(2),
        widthPct: width.toFixed(2),
        tipLines,
      };
    });

    // Agent table rows
    const rowMap = {};
    liveSnapshot.agents.forEach((a) => {
      rowMap[a.id] = { id: a.id, state: a.state, last: null, types: {}, n: 0, ok: 0 };
    });
    sel.forEach((x) => {
      if (!rowMap[x.s.agent]) {
        rowMap[x.s.agent] = { id: x.s.agent, state: 'unknown', last: null, types: {}, n: 0, ok: 0 };
      }
      const r = rowMap[x.s.agent];
      x.pts.forEach((p) => {
        r.n += 1;
        if (p.ok) r.ok += 1;
        if (!r.last || p.t > r.last.t) r.last = p;
      });
      if (x.s.type) r.types[x.s.type] = true;
    });
    const agentRows = Object.keys(rowMap)
      .sort()
      .map((id) => rowMap[id]);

    return {
      now,
      allLen: all.length,
      availText: all.length ? `${((oks.length * 100) / all.length).toFixed(1)} %` : '-',
      availNote: all.length ? `${all.length - oks.length} failed probe(s) of ${all.length}` : 'No results yet',
      medianText: fmtMs(median(mss)),
      p95Note: mss.length ? `95th percentile ${fmtMs(pct(mss, 0.95))}` : '\u00a0',
      lastText: lastT ? `${Math.max(0, (now - lastT) / 1000).toFixed(1)} s ago` : '-',
      typesText: String(typeKeys.length || '-'),
      typesNote: typeKeys.map((t) => t.toUpperCase()).join(', ') || '\u00a0',
      caption: `Response time, ${
        view === 'merged' ? 'all monitors merged' : view === 'machine' ? 'per monitor machine' : 'per target'
      } (max ${fmtMs(rawMaxMs)})`,
      sub: `Live window ${win >= 120 ? `${win / 60} min` : `${win} s`} · median per ${(binMs / 1000).toFixed(1)} s`,
      lines,
      cols,
      markers,
      agentRows,
    };
  }, [renderTick, renderNow, win, selectedKey, view, liveSnapshot, seriesMapRef]);

  const showTipFromEvent = useCallback((lines, e) => {
    const rect = e.currentTarget.getBoundingClientRect();
    const x = typeof e.clientX === 'number' && e.clientX > 0 ? e.clientX : rect.left + rect.width / 2;
    const y = rect.top;
    setTip({ lines, x, y });
  }, []);

  const hideTip = useCallback(() => setTip(null), []);

  const handleTabKeyDown = (e) => {
    const order = ['live', 'history', 'grafana', 'cfg'];
    const idx = order.indexOf(activeTab);
    let next = null;
    if (e.key === 'ArrowRight') next = order[(idx + 1) % order.length];
    else if (e.key === 'ArrowLeft') next = order[(idx + order.length - 1) % order.length];
    else if (e.key === 'Home') next = order[0];
    else if (e.key === 'End') next = order[order.length - 1];
    if (next) {
      e.preventDefault();
      setActiveTab(next);
      if (next === 'grafana') setGrafanaMounted(true);
      if (next === 'cfg') loadConfig();
      tabRefs.current[next]?.focus();
    }
  };

  const handleSaveDefaults = async (e) => {
    e.preventDefault();
    setSavingDefs(true);
    const to = {};
    ['http', 'tcp', 'icmp', 'dns'].forEach((t) => {
      const v = parseInt(defForm[t], 10);
      if (v) to[t] = v;
    });
    const r = await saveSettings({
      interval_ms: parseInt(defForm.interval_ms, 10) || 0,
      timeout_ms: to,
    });
    setSavingDefs(false);
    setStatus(
      r.ok
        ? { kind: 'ok', msg: 'Defaults saved and pushed to the connected monitor agents.' }
        : { kind: 'danger', msg: r.data.error || 'Not saved.' }
    );
  };

  const handleUrlChange = (val) => {
    setJobForm((prev) => ({ ...prev, url: val }));
    if (!probeTypesCustom) {
      setProbeTypes(val.trim() ? ['icmp', 'tcp', 'http'] : ['icmp']);
    }
  };

  const toggleProbeType = (pt, checked) => {
    setProbeTypesCustom(true);
    setProbeTypes((prev) => {
      if (checked) {
        return prev.includes(pt) ? prev : [...prev, pt];
      }
      return prev.filter((x) => x !== pt);
    });
  };

  const handleCreateJob = async (e) => {
    e.preventDefault();
    if (probeTypes.length === 0) {
      setStatus({ kind: 'danger', msg: 'Select at least one probe type.' });
      return;
    }
    setCreatingJob(true);
    const paths = jobForm.paths
      .split(/[\s,]+/)
      .map((s) => s.trim())
      .filter(Boolean);
    const payload = {
      url: jobForm.url.trim(),
      ip: jobForm.ip.trim(),
      paths,
      probe_types: probeTypes,
      interval_ms: parseInt(jobForm.interval_ms, 10) || 0,
    };
    if (probeTypes.includes('dns')) {
      if (jobForm.dns_name.trim()) payload.dns_name = jobForm.dns_name.trim();
      if (jobForm.dns_server.trim()) payload.dns_server = jobForm.dns_server.trim();
    }
    const r = await createJob(payload);
    setCreatingJob(false);
    if (!r.ok) {
      setStatus({ kind: 'danger', msg: r.data.error || 'The job was not created.' });
      return;
    }
    setStatus({
      kind: 'ok',
      msg: `Monitor job ${r.data.job.id} created with ${r.data.job.targets.length} target(s); the agents were sent their configuration.`,
    });
    setJobForm({ url: '', ip: '', paths: '', interval_ms: '', dns_name: '', dns_server: '' });
    setProbeTypes(['icmp']);
    setProbeTypesCustom(false);
  };

  const handleEditInterval = async (j) => {
    const curIvs = Array.from(
      new Set((j.targets || []).map((t) => t.target?.interval_ms).filter(Boolean))
    );
    const v = window.prompt(
      'Frequency override in ms for every target of this job (300 to 60000; empty or 0 clears it):',
      curIvs[0] ? String(curIvs[0]) : ''
    );
    if (v === null) return;
    const ms = parseInt(v, 10) || 0;
    const rs = await Promise.all(
      (j.targets || []).map((t) => setTargetInterval(j.id, t.target.id, ms))
    );
    const bad = rs.find((r) => !r.ok);
    if (bad) {
      setStatus({ kind: 'danger', msg: bad.data.error || 'The frequency was not changed.' });
    } else {
      setStatus({ kind: 'ok', msg: 'Frequency updated and pushed.' });
    }
    loadConfig();
  };

  const handleDeleteJob = async (j) => {
    if (!window.confirm('Delete this monitor job? Its history is kept; the agents stop probing its targets.')) {
      return;
    }
    const r = await deleteJob(j.id);
    setStatus(
      r.ok
        ? { kind: 'ok', msg: 'Job deleted; the agents were sent their new configuration.' }
        : { kind: 'danger', msg: r.data.error || 'The job was not deleted.' }
    );
  };

  const monitorsTotal =
    machinesSummary.monitors > 0 ? machinesSummary.monitors : liveSnapshot.agents.length;
  const monitorsOnline =
    machinesSummary.monitors > 0
      ? machinesSummary.monitors_online
      : liveSnapshot.agents.filter((a) => a.state === 'online').length;
  const allMonOnline = monitorsTotal > 0 && monitorsOnline === monitorsTotal;

  const dnsSelected = probeTypes.includes('dns');
  const bounds = config?.bounds || { min_interval_ms: 300, max_interval_ms: 60000 };
  const configJobs = config?.jobs || [];
  const configAgents = config?.agents || [];

  // Wheel-zoom handler on the History plot (non-passive so scrolling zooms instead of scrolling page).
  useEffect(() => {
    const el = histPlotRef.current;
    if (!el || activeTab !== 'history') return undefined;
    const onWheel = (e) => {
      if (Math.abs(e.deltaY) < 1) return;
      e.preventDefault();
      hideTip();
      const rect = el.getBoundingClientRect();
      const centerFrac = rect.width > 0 ? Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width)) : 0.5;
      const stepFactor = e.deltaY < 0 ? 0.65 : 1.5;
      const acc = wheelAccRef.current;
      acc.factor = Math.max(0.15, Math.min(6, acc.factor * stepFactor));
      acc.centerFrac = centerFrac;
      if (acc.timer) window.clearTimeout(acc.timer);
      acc.timer = window.setTimeout(() => {
        const f = acc.factor;
        const c = acc.centerFrac;
        acc.factor = 1;
        acc.timer = 0;
        if (f < 0.98) {
          const span = history.range.toMs - history.range.fromMs;
          const nextSpan = Math.max(15000, span * f);
          const anchor = history.range.fromMs + span * c;
          const nextFrom = anchor - nextSpan * c;
          history.zoomToRange(nextFrom, nextFrom + nextSpan);
        } else if (f > 1.02) {
          const span = history.range.toMs - history.range.fromMs;
          const nextSpan = span * f;
          const anchor = history.range.fromMs + span * c;
          const nextFrom = anchor - nextSpan * c;
          history.zoomToRange(nextFrom, nextFrom + nextSpan, { pushHistory: false });
        }
      }, 130);
    };
    el.addEventListener('wheel', onWheel, { passive: false });
    return () => {
      el.removeEventListener('wheel', onWheel);
      if (wheelAccRef.current.timer) {
        window.clearTimeout(wheelAccRef.current.timer);
        wheelAccRef.current.timer = 0;
        wheelAccRef.current.factor = 1;
      }
    };
  }, [activeTab, history.range.fromMs, history.range.toMs, history.zoomToRange, hideTip]);

  // Escape cancels active brush drag
  useEffect(() => {
    if (!brush) return undefined;
    const onKeyDown = (e) => {
      if (e.key === 'Escape') setBrush(null);
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [brush]);

  // Target choices merged from liveSnapshot.jobs + configJobs so History always lists targets
  const historyTargetOptions = useMemo(() => {
    const byKey = {};
    (liveSnapshot.jobs || []).forEach((j) => {
      if (j.key) byKey[j.key] = { key: j.key, label: j.label };
    });
    (configJobs || []).forEach((j) => {
      if (!j.history_key || byKey[j.history_key]) return;
      const lbl = j.origin || (j.dns_name ? `dns://${j.dns_name}` : j.ip);
      byKey[j.history_key] = { key: j.history_key, label: `${lbl} · ${j.ip}` };
    });
    return Object.values(byKey).sort((a, b) => a.label.localeCompare(b.label));
  }, [liveSnapshot.jobs, configJobs]);

  const historyView = useMemo(() => {
    const { fromMs, toMs } = history.range;
    const spanMs = Math.max(1, toMs - fromMs);
    const jobLabelByKey = {};
    historyTargetOptions.forEach((j) => {
      jobLabelByKey[j.key] = j.label;
    });
    const targetLabels = liveSnapshot.targets || {};

    const effectiveGroup =
      histGroupMode !== 'auto'
        ? histGroupMode
        : history.zoomLevel === 'overview'
        ? 'merged'
        : history.zoomLevel === 'day'
        ? 'target'
        : 'series';

    const groupLabelFor = (s) => {
      if (effectiveGroup === 'merged') return 'All monitors merged';
      if (effectiveGroup === 'target') {
        return (
          (s.history_key && jobLabelByKey[s.history_key]) ||
          targetLabels[s.target] ||
          s.target ||
          s.history_key ||
          'Target'
        );
      }
      const tgt = targetLabels[s.target] || s.target || (s.type ? s.type.toUpperCase() : 'probe');
      return s.agent ? `${s.agent} · ${tgt}` : tgt;
    };

    const grouped = {};
    const groupOrder = [];
    const allPts = [];
    const agentSet = {};
    const targetSet = {};
    history.series.forEach((s) => {
      if (s.agent) agentSet[s.agent] = true;
      if (s.target) targetSet[s.target] = true;
      const g = groupLabelFor(s);
      if (!grouped[g]) {
        grouped[g] = [];
        groupOrder.push(g);
      }
      s.points.forEach((p) => {
        if (p.tMs >= fromMs && p.tMs <= toMs) {
          grouped[g].push(p);
          allPts.push(p);
        }
      });
    });
    groupOrder.sort();

    const okSrc = history.metric === 'ok' ? history.series : history.okSeries;
    const allOkPts = [];
    const failXs = [];
    okSrc.forEach((s) => {
      s.points.forEach((p) => {
        if (p.tMs >= fromMs && p.tMs <= toMs) {
          allOkPts.push(p.value);
          if (p.value < 0.5) {
            failXs.push(((p.tMs - fromMs) / spanMs) * 200);
          }
        }
      });
    });

    const numBins = history.zoomLevel === 'hour' || history.zoomLevel === 'detail' ? 60 : NUM_BINS;
    const binMs = spanMs / numBins;
    let rawMax = history.metric === 'ok' || history.metric === 'loss' ? 1 : 10;

    const preLines = groupOrder.map((g) => {
      const bins = new Array(numBins);
      grouped[g].forEach((p) => {
        const b = Math.min(numBins - 1, Math.max(0, Math.floor((p.tMs - fromMs) / binMs)));
        if (!bins[b]) bins[b] = [];
        bins[b].push(p.value);
      });
      const pts = [];
      bins.forEach((arr, b) => {
        if (!arr) return;
        const v = history.metric === 'ok' || history.metric === 'loss' ? arr.reduce((a, x) => a + x, 0) / arr.length : median(arr);
        rawMax = Math.max(rawMax, v);
        pts.push([((b + 0.5) * 200) / numBins, v]);
      });
      return { name: g, pts };
    });

    const maxScale = history.metric === 'ok' || history.metric === 'loss' ? Math.max(1.05, rawMax * 1.05) : rawMax * 1.15;
    const lines = preLines.map((ln, idx) => {
      const coords = ln.pts.map((p) => [p[0], 57 - (p[1] / maxScale) * 52]);
      return {
        name: ln.name,
        colorIdx: (idx % 5) + 1,
        dash: DASH_ARRAYS[idx % 5],
        pointsAttr: coords.map((c) => `${c[0].toFixed(1)},${c[1].toFixed(1)}`).join(' '),
        dots: coords.length <= 12 ? coords : [],
      };
    });

    const colMs = spanMs / NUM_COLS;
    const cols = [];
    for (let i = 0; i < NUM_COLS; i++) {
      const a = fromMs + i * colMs;
      const b = i === NUM_COLS - 1 ? toMs : a + colMs;
      const inVals = allPts
        .filter((p) => p.tMs >= a && (i === NUM_COLS - 1 ? p.tMs <= b : p.tMs < b))
        .map((p) => p.value);
      const okInCol = [];
      okSrc.forEach((s) => {
        s.points.forEach((p) => {
          if (p.tMs >= a && (i === NUM_COLS - 1 ? p.tMs <= b : p.tMs < b)) okInCol.push(p.value);
        });
      });
      const okStr = okInCol.length
        ? `Availability ${((okInCol.reduce((s, v) => s + v, 0) / okInCol.length) * 100).toFixed(1)}%`
        : 'Availability -';
      const valStr = inVals.length
        ? `Median ${fmtHistMetric(history.metric, median(inVals))} · Max ${fmtHistMetric(
            history.metric,
            Math.max(...inVals)
          )} (${inVals.length} pts)`
        : 'No samples in this interval';
      cols.push({
        key: i,
        tipLines: [`${fmtHistFull(a)} -> ${fmtHistFull(b)}`, okStr, valStr, 'Drag or scroll to zoom'],
      });
    }

    const segments = buildSemanticSegments(
      fromMs,
      toMs,
      history.zoomLevel,
      history.metric,
      history.series,
      history.okSeries
    );

    const allVals = allPts.map((p) => p.value);
    const availPct = allOkPts.length
      ? (allOkPts.reduce((a, x) => a + x, 0) / allOkPts.length) * 100
      : null;
    const failCount = allOkPts.filter((v) => v < 0.5).length;

    const seriesRows = history.series.map((s) => {
      const vals = s.points.filter((p) => p.tMs >= fromMs && p.tMs <= toMs).map((p) => p.value);
      return {
        key: `${s.agent}\u0000${s.target}\u0000${s.type}`,
        label: (s.history_key && jobLabelByKey[s.history_key]) || targetLabels[s.target] || s.target || '-',
        agent: s.agent || '-',
        type: s.type ? s.type.toUpperCase() : '-',
        n: vals.length,
        med: vals.length ? fmtHistMetric(history.metric, median(vals)) : '-',
        p95: vals.length ? fmtHistMetric(history.metric, pct(vals, 0.95)) : '-',
        max: vals.length ? fmtHistMetric(history.metric, Math.max(...vals)) : '-',
      };
    });

    const metricMeta = HISTORY_METRICS.find((m) => m.id === history.metric) || HISTORY_METRICS[0];
    const groupDesc =
      effectiveGroup === 'merged'
        ? `merged (${history.series.length} series collapsed)`
        : effectiveGroup === 'target'
        ? `per target (${lines.length} line${lines.length === 1 ? '' : 's'})`
        : `per agent & probe (${lines.length} line${lines.length === 1 ? '' : 's'})`;

    return {
      fromMs,
      toMs,
      spanMs,
      effectiveGroup,
      allLen: allPts.length,
      availText: availPct != null ? `${availPct.toFixed(1)} %` : '-',
      availNote: allOkPts.length
        ? `${failCount} failed sample(s) of ${allOkPts.length}`
        : 'No availability samples',
      primaryLabel: metricMeta.label,
      primaryMedian: allVals.length ? fmtHistMetric(history.metric, median(allVals)) : '-',
      primaryNote: allVals.length
        ? `95th pct ${fmtHistMetric(history.metric, pct(allVals, 0.95))} · max ${fmtHistMetric(
            history.metric,
            Math.max(...allVals)
          )}`
        : '\u00a0',
      seriesCountText:
        effectiveGroup === 'merged'
          ? `Merged (${history.series.length})`
          : `${lines.length} line${lines.length === 1 ? '' : 's'}`,
      seriesCountNote: `${Object.keys(agentSet).length} agent(s) · ${Object.keys(targetSet).length} target(s)`,
      caption: `${metricMeta.label}, ${groupDesc}`,
      sub: `Window ${fmtSpanDuration(spanMs)} · step ${formatStepLabel(history.stepSec)} (max ${fmtHistMetric(
        history.metric,
        rawMax
      )})`,
      lines,
      failXs: failXs.slice(-200),
      cols,
      segments,
      seriesRows,
    };
  }, [history.range, history.series, history.okSeries, history.metric, history.zoomLevel, history.stepSec, histGroupMode, historyTargetOptions, liveSnapshot.targets]);

  const handleHistPointerDown = (e) => {
    if (e.button !== 0 || !histPlotRef.current) return;
    const rect = histPlotRef.current.getBoundingClientRect();
    if (rect.width <= 0) return;
    const frac = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
    hideTip();
    setBrush({ anchorFrac: frac, curFrac: frac });
    e.currentTarget.setPointerCapture?.(e.pointerId);
  };

  const handleHistPointerMove = (e) => {
    if (!brush || !histPlotRef.current) return;
    const rect = histPlotRef.current.getBoundingClientRect();
    if (rect.width <= 0) return;
    const frac = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
    setBrush((prev) => (prev ? { ...prev, curFrac: frac } : null));
  };

  const handleHistPointerUp = (e) => {
    if (!brush) return;
    const a = Math.min(brush.anchorFrac, brush.curFrac);
    const b = Math.max(brush.anchorFrac, brush.curFrac);
    setBrush(null);
    e.currentTarget.releasePointerCapture?.(e.pointerId);
    if (b - a >= 0.02) {
      const span = history.range.toMs - history.range.fromMs;
      history.zoomToRange(history.range.fromMs + a * span, history.range.fromMs + b * span);
    }
  };

  const grafanaDash = GRAFANA_DASHBOARDS.find((d) => d.uid === grafanaDashUid) || GRAFANA_DASHBOARDS[0];
  const gfTheme = theme === 'light' ? 'light' : 'dark';
  const grafanaBaseUrl = `/grafana/d/${grafanaDash.uid}/${grafanaDash.slug}?orgId=1&theme=${gfTheme}`;
  const grafanaEmbedUrl = `${grafanaBaseUrl}&kiosk`;

  return (
    <>
      <HoverTip tip={tip} />
      <div className="page-head">
        <div className="page-head__text">
          <h1>Monitor data</h1>
          <p>
            Availability and response time of the targets, probed continuously by the monitor agents. This is{' '}
            <strong>monitor data</strong>: it is stored and shown separately from worker evidence logs.
          </p>
        </div>
        <div className="page-head__actions">
          <span className={`chip ${allMonOnline ? 'chip--ok' : 'chip--warn'}`} id="mon-agents-chip">
            <svg className="icon icon--sm" aria-hidden="true">
              <use href={`#i-${allMonOnline ? 'check' : 'alert'}`} />
            </svg>
            {`${monitorsOnline} of ${monitorsTotal} monitor agents online`}
          </span>
        </div>
      </div>

      {noBackend && (
        <div className="alert alert--warn" role="status" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true">
            <use href="#i-alert" />
          </svg>
          <div>
            The panel is running without a backend, so live probe charts and job configuration are unavailable.
          </div>
        </div>
      )}

      <StatusAlert status={status} id="mon-status" style={{ marginBottom: 'var(--sp-4)' }} />

      <div className="card">
        <div className="tabs" role="tablist" aria-label="Monitor data views">
          <button
            ref={(el) => {
              tabRefs.current.live = el;
            }}
            className="tab"
            role="tab"
            id="mtab-live"
            aria-controls="mp-live"
            aria-selected={String(activeTab === 'live')}
            tabIndex={activeTab === 'live' ? 0 : -1}
            type="button"
            onClick={() => setActiveTab('live')}
            onKeyDown={handleTabKeyDown}
          >
            <svg className="icon icon--sm" aria-hidden="true">
              <use href="#i-activity" />
            </svg>
            Live
          </button>
          <button
            ref={(el) => {
              tabRefs.current.history = el;
            }}
            className="tab"
            role="tab"
            id="mtab-hist"
            aria-controls="mp-hist"
            aria-selected={String(activeTab === 'history')}
            tabIndex={activeTab === 'history' ? 0 : -1}
            type="button"
            onClick={() => setActiveTab('history')}
            onKeyDown={handleTabKeyDown}
          >
            <svg className="icon icon--sm" aria-hidden="true">
              <use href="#i-history" />
            </svg>
            History
          </button>
          <button
            ref={(el) => {
              tabRefs.current.grafana = el;
            }}
            className="tab"
            role="tab"
            id="mtab-grafana"
            aria-controls="mp-grafana"
            aria-selected={String(activeTab === 'grafana')}
            tabIndex={activeTab === 'grafana' ? 0 : -1}
            type="button"
            onClick={() => {
              setActiveTab('grafana');
              setGrafanaMounted(true);
            }}
            onKeyDown={handleTabKeyDown}
          >
            <svg className="icon icon--sm" aria-hidden="true">
              <use href="#i-grid" />
            </svg>
            Grafana
          </button>
          <button
            ref={(el) => {
              tabRefs.current.cfg = el;
            }}
            className="tab"
            role="tab"
            id="mtab-cfg"
            aria-controls="mp-cfg"
            aria-selected={String(activeTab === 'cfg')}
            tabIndex={activeTab === 'cfg' ? 0 : -1}
            type="button"
            onClick={() => {
              setActiveTab('cfg');
              loadConfig();
            }}
            onKeyDown={handleTabKeyDown}
          >
            <svg className="icon icon--sm" aria-hidden="true">
              <use href="#i-sliders" />
            </svg>
            Probe config
          </button>
        </div>

        {/* ================= LIVE ================= */}
        <div
          className="card__body tabpanel"
          role="tabpanel"
          id="mp-live"
          aria-labelledby="mtab-live"
          hidden={activeTab !== 'live'}
        >
          <div className="stack">
            <div className="row row--between">
              <div className="row">
                <div className="seg" role="radiogroup" aria-label="Chart view">
                  <label>
                    <input
                      type="radio"
                      name="view"
                      value="merged"
                      checked={view === 'merged'}
                      onChange={() => setView('merged')}
                    />
                    <span>All monitors merged</span>
                  </label>
                  <label>
                    <input
                      type="radio"
                      name="view"
                      value="machine"
                      checked={view === 'machine'}
                      onChange={() => setView('machine')}
                    />
                    <span>Per machine</span>
                  </label>
                  <label>
                    <input
                      type="radio"
                      name="view"
                      value="target"
                      checked={view === 'target'}
                      onChange={() => setView('target')}
                    />
                    <span>Per target</span>
                  </label>
                </div>
                <label className="sr-only" htmlFor="m-target">
                  Target
                </label>
                <select
                  className="select"
                  id="m-target"
                  style={{ width: 'auto' }}
                  value={selectedKey}
                  onChange={(e) => setSelectedKey(e.target.value)}
                >
                  <option value="">All targets</option>
                  {liveSnapshot.jobs.map((j) => (
                    <option key={j.key} value={j.key}>
                      {j.label}
                    </option>
                  ))}
                </select>
              </div>
              <div className="row">
                <label className="sr-only" htmlFor="m-win">
                  Live window
                </label>
                <select
                  className="select"
                  id="m-win"
                  style={{ width: 'auto' }}
                  value={String(win)}
                  onChange={(e) => {
                    const nextWin = parseInt(e.target.value, 10) || 60;
                    setWin(nextWin);
                    loadLive(nextWin);
                  }}
                >
                  <option value="60">Last 60 s (default)</option>
                  <option value="120">Last 2 min</option>
                  <option value="300">Last 5 min</option>
                </select>
                <button className="btn" type="button" id="m-pause" onClick={togglePause}>
                  <svg className="icon" aria-hidden="true">
                    <use href={`#i-${paused ? 'play' : 'pause'}`} />
                  </svg>
                  <span>{paused ? 'Resume' : 'Pause'}</span>
                </button>
              </div>
            </div>

            <section className="grid" aria-label="Current values">
              <div className="card stat col-3 col-3--md-6">
                <div className="stat__label">Availability</div>
                <div className="stat__value" id="mon-avail">
                  {liveView.availText}
                </div>
                <div className="stat__note" id="mon-avail-note">
                  {liveView.availNote}
                </div>
              </div>
              <div className="card stat col-3 col-3--md-6">
                <div className="stat__label">Response time, median</div>
                <div className="stat__value" id="mon-median">
                  {liveView.medianText}
                </div>
                <div className="stat__note" id="mon-p95">
                  {liveView.p95Note}
                </div>
              </div>
              <div className="card stat col-3 col-3--md-6">
                <div className="stat__label">Last result</div>
                <div className="stat__value" id="mon-last">
                  {liveView.lastText}
                </div>
                <div className="stat__note">Monitors flush every 2-3 s</div>
              </div>
              <div className="card stat col-3 col-3--md-6">
                <div className="stat__label">Probe types</div>
                <div className="stat__value" id="mon-types">
                  {liveView.typesText}
                </div>
                <div className="stat__note" id="mon-types-note">
                  {liveView.typesNote}
                </div>
              </div>
            </section>

            <figure className="chart-ph" aria-labelledby="chart-cap">
              <figcaption id="chart-cap" className="row row--between" style={{ marginBottom: 'var(--sp-2)' }}>
                <b id="mon-caption">{liveView.caption}</b>
                <span className="muted" id="mon-sub">
                  {liveView.sub}
                </span>
              </figcaption>
              <div className="chart-ph__plot plot" style={{ height: '12rem' }}>
                <svg
                  id="mon-svg"
                  viewBox="0 0 200 60"
                  preserveAspectRatio="none"
                  role="img"
                  aria-label="Response time chart"
                >
                  {liveView.lines.map((ln) => (
                    <g key={ln.name}>
                      {ln.pointsAttr && (
                        <polyline
                          fill="none"
                          stroke={`var(--series-${ln.colorIdx})`}
                          strokeWidth="1.6"
                          strokeDasharray={ln.dash || undefined}
                          vectorEffect="non-scaling-stroke"
                          points={ln.pointsAttr}
                        />
                      )}
                      {ln.fails.map((x, idx) => (
                        <line
                          key={idx}
                          x1={x.toFixed(1)}
                          x2={x.toFixed(1)}
                          y1="53"
                          y2="59.5"
                          stroke="var(--danger)"
                          strokeWidth="1.4"
                          vectorEffect="non-scaling-stroke"
                        />
                      ))}
                    </g>
                  ))}
                </svg>
                <div className="chart-ph__label" id="mon-empty" hidden={!liveError && liveView.allLen > 0}>
                  <span>{liveError || 'Waiting for results from the monitor agents'}</span>
                </div>
                <div className="plot__cols" id="mon-cols">
                  {liveView.cols.map((col) => (
                    <span
                      key={col.key}
                      className="plot__col"
                      tabIndex={0}
                      data-tip={col.tipLines.join('|')}
                      aria-label={col.tipLines.join(', ')}
                      onMouseEnter={(e) => showTipFromEvent(col.tipLines, e)}
                      onMouseMove={(e) => showTipFromEvent(col.tipLines, e)}
                      onMouseLeave={hideTip}
                      onFocus={(e) => showTipFromEvent(col.tipLines, e)}
                      onBlur={hideTip}
                    />
                  ))}
                </div>
              </div>
              <div
                className="ovl"
                id="mon-ovl"
                role="group"
                aria-label="Command overlay: commands mapped onto this timeline"
              >
                <span className="ovl__label">Commands</span>
                {liveView.markers.map((m) => (
                  <button
                    key={m.session}
                    type="button"
                    className="ovl__mark"
                    style={{ left: `${m.leftPct}%`, width: `${m.widthPct}%` }}
                    data-tip={m.tipLines.join('|')}
                    aria-label={m.tipLines.join(', ')}
                    onMouseEnter={(e) => showTipFromEvent(m.tipLines, e)}
                    onMouseMove={(e) => showTipFromEvent(m.tipLines, e)}
                    onMouseLeave={hideTip}
                    onFocus={(e) => showTipFromEvent(m.tipLines, e)}
                    onBlur={hideTip}
                  />
                ))}
              </div>
              <div className="zoom__axis" id="mon-axis" aria-hidden="true">
                {[0, 25, 50, 75, 100].map((p) => (
                  <span key={p} style={{ left: `${p}%` }}>
                    {p === 100 ? 'now' : `-${Math.round((win * (100 - p)) / 100)} s`}
                  </span>
                ))}
              </div>
              <div className="legend" id="mon-legend">
                {liveView.lines.map((ln, i) => {
                  const mod = i % 5;
                  const swCls = `legend__swatch${
                    mod === 1 ? ' legend__swatch--dash' : mod === 2 ? ' legend__swatch--dot' : ''
                  }`;
                  return (
                    <span key={ln.name} className="legend__item">
                      <span className={swCls} style={{ borderTopColor: `var(--series-${ln.colorIdx})` }} />
                      {ln.name}
                    </span>
                  );
                })}
              </div>
              <p className="field__hint" style={{ marginTop: 'var(--sp-2)' }}>
                Hover or focus a command marker (or hover the chart) to see which commands ran and per-interval packet
                counts. Packet counts come from the evidence on the main server, so a command shows its packets once
                that evidence has been pulled; the chart itself is live. Failed probes are marked at the bottom of the
                chart.
              </p>
            </figure>

            <details className="card">
              <summary className="card__head" style={{ cursor: 'pointer' }}>
                <h2>Data table (accessible alternative to the chart)</h2>
                <svg className="icon" aria-hidden="true">
                  <use href="#i-chevron" />
                </svg>
              </summary>
              <div className="table-wrap">
                <table className="table">
                  <caption className="sr-only">Latest samples per monitor agent</caption>
                  <thead>
                    <tr>
                      <th scope="col">Monitor agent</th>
                      <th scope="col">State</th>
                      <th scope="col">Probe</th>
                      <th scope="col" className="num">
                        Last response
                      </th>
                      <th scope="col" className="num">
                        Success (window)
                      </th>
                      <th scope="col">Last result</th>
                    </tr>
                  </thead>
                  <tbody id="mon-table">
                    {liveView.agentRows.length === 0 ? (
                      <tr>
                        <td colSpan={6} className="faint">
                          No monitor agent is registered.
                        </td>
                      </tr>
                    ) : (
                      liveView.agentRows.map((r) => (
                        <tr key={r.id}>
                          <th scope="row">{r.id}</th>
                          <td>
                            <span className={`chip chip--${r.state === 'online' ? 'ok' : 'danger'}`}>
                              <svg className="icon icon--sm" aria-hidden="true">
                                <use href={`#i-${r.state === 'online' ? 'check' : 'x'}`} />
                              </svg>
                              {r.state}
                            </span>
                          </td>
                          <td>
                            {Object.keys(r.types)
                              .map((t) => t.toUpperCase())
                              .join(', ') || '-'}
                          </td>
                          <td className="num">
                            {r.last ? (r.last.ok ? fmtMs(r.last.ms) : 'failed') : '-'}
                          </td>
                          <td className="num">
                            {r.n ? `${((r.ok * 100) / r.n).toFixed(1)}%` : '-'}
                          </td>
                          <td>
                            {r.last
                              ? `${Math.max(0, (liveView.now - r.last.t) / 1000).toFixed(1)} s ago`
                              : 'no results'}
                          </td>
                        </tr>
                      ))
                    )}
                  </tbody>
                </table>
              </div>
            </details>
          </div>
        </div>

        {/* ================= HISTORY (semantic zoom) ================= */}
        <div
          className="card__body tabpanel"
          role="tabpanel"
          id="mp-hist"
          aria-labelledby="mtab-hist"
          hidden={activeTab !== 'history'}
        >
          <div className="stack">
            {history.noStore && (
              <div className="alert alert--warn" role="status">
                <svg className="icon" aria-hidden="true">
                  <use href="#i-alert" />
                </svg>
                <div>
                  The monitor history store (VictoriaMetrics) is not configured on this server, so historical series
                  are unavailable.
                </div>
              </div>
            )}

            {history.error && (
              <div className="alert alert--danger" role="alert">
                <svg className="icon" aria-hidden="true">
                  <use href="#i-alert" />
                </svg>
                <div>{history.error}</div>
              </div>
            )}

            <div className="row row--between">
              <div className="row">
                <label className="sr-only" htmlFor="h-target">
                  History target
                </label>
                <select
                  className="select"
                  id="h-target"
                  style={{ width: 'auto' }}
                  value={history.historyKey}
                  onChange={(e) => history.setHistoryKey(e.target.value)}
                >
                  <option value="">All targets</option>
                  {historyTargetOptions.map((j) => (
                    <option key={j.key} value={j.key}>
                      {j.label}
                    </option>
                  ))}
                </select>

                <label className="sr-only" htmlFor="h-metric">
                  Metric
                </label>
                <select
                  className="select"
                  id="h-metric"
                  style={{ width: 'auto' }}
                  value={history.metric}
                  onChange={(e) => history.setMetric(e.target.value)}
                >
                  {HISTORY_METRICS.map((m) => (
                    <option key={m.id} value={m.id}>
                      {m.label}
                    </option>
                  ))}
                </select>

                <label className="sr-only" htmlFor="h-type">
                  Probe type
                </label>
                <select
                  className="select"
                  id="h-type"
                  style={{ width: 'auto' }}
                  value={history.probeType}
                  onChange={(e) => history.setProbeType(e.target.value)}
                >
                  <option value="">All probes</option>
                  {PROBE_TYPES.map((pt) => (
                    <option key={pt.id} value={pt.id}>
                      {pt.label}
                    </option>
                  ))}
                </select>

                <label className="sr-only" htmlFor="h-span">
                  Timeline span
                </label>
                <select
                  className="select"
                  id="h-span"
                  style={{ width: 'auto' }}
                  value={history.overviewSpanId}
                  onChange={(e) => history.setOverviewSpan(e.target.value)}
                >
                  {OVERVIEW_SPANS.map((sp) => (
                    <option key={sp.id} value={sp.id}>
                      {sp.label}
                    </option>
                  ))}
                </select>
              </div>

              <div className="row">
                <div className="seg" role="radiogroup" aria-label="Zoom level">
                  {ZOOM_LEVELS.map((zl) => (
                    <label key={zl.id}>
                      <input
                        type="radio"
                        name="hist-zoom-level"
                        value={zl.id}
                        checked={history.zoomLevel === zl.id}
                        onChange={() => history.selectZoomPreset(zl.id)}
                      />
                      <span>{zl.label}</span>
                    </label>
                  ))}
                </div>
                {history.isZoomed && (
                  <>
                    <button
                      className="icon-btn icon-btn--sm icon-btn--bordered"
                      type="button"
                      aria-label="Pan earlier"
                      title="Pan earlier"
                      onClick={() => history.panBy(-0.5)}
                    >
                      <svg className="icon" aria-hidden="true" style={{ transform: 'rotate(180deg)' }}>
                        <use href="#i-arrow" />
                      </svg>
                    </button>
                    <button
                      className="icon-btn icon-btn--sm icon-btn--bordered"
                      type="button"
                      aria-label="Pan later"
                      title="Pan later"
                      onClick={() => history.panBy(0.5)}
                    >
                      <svg className="icon" aria-hidden="true">
                        <use href="#i-arrow" />
                      </svg>
                    </button>
                  </>
                )}
                <button
                  className="icon-btn icon-btn--sm icon-btn--bordered"
                  type="button"
                  id="h-zoom-out"
                  aria-label="Zoom out"
                  title="Zoom out"
                  disabled={!history.canZoomOut}
                  onClick={() => history.zoomOut()}
                >
                  <svg className="icon" aria-hidden="true">
                    <use href="#i-minus" />
                  </svg>
                </button>
                <button
                  className="icon-btn icon-btn--sm icon-btn--bordered"
                  type="button"
                  id="h-zoom-in"
                  aria-label="Zoom in"
                  title="Zoom in"
                  disabled={!history.canZoomIn}
                  onClick={() => history.zoomIn()}
                >
                  <svg className="icon" aria-hidden="true">
                    <use href="#i-plus" />
                  </svg>
                </button>
                {history.isZoomed && (
                  <button
                    className="btn"
                    type="button"
                    id="h-reset"
                    onClick={history.resetOverview}
                  >
                    Overview
                  </button>
                )}
                <button
                  className="icon-btn icon-btn--sm icon-btn--bordered"
                  type="button"
                  id="h-reload"
                  aria-label="Refresh history"
                  title="Refresh history"
                  onClick={history.reload}
                >
                  <svg className="icon" aria-hidden="true">
                    <use href="#i-refresh" />
                  </svg>
                </button>
              </div>
            </div>

            <div className="row row--between">
              <div className="row">
                <span className="muted" id="h-zoom-status" aria-live="polite">
                  {`Semantic zoom: ${(ZOOM_LEVELS.find((z) => z.id === history.zoomLevel) || ZOOM_LEVELS[0]).label} · step ${formatStepLabel(
                    history.stepSec
                  )}${history.loading ? ' · loading...' : ''}`}
                </span>
              </div>
              <div className="row">
                <label className="sr-only" htmlFor="h-group">
                  Series grouping
                </label>
                <select
                  className="select"
                  id="h-group"
                  style={{ width: 'auto' }}
                  value={histGroupMode}
                  onChange={(e) => setHistGroupMode(e.target.value)}
                >
                  <option value="auto">Grouping: Auto (semantic)</option>
                  <option value="merged">Grouping: All merged</option>
                  <option value="target">Grouping: Per target</option>
                  <option value="series">Grouping: Per agent &amp; probe</option>
                </select>
              </div>
            </div>

            <section className="grid" aria-label="History summary">
              <div className="card stat col-3 col-3--md-6">
                <div className="stat__label">Availability (range)</div>
                <div className="stat__value" id="hist-avail">
                  {historyView.availText}
                </div>
                <div className="stat__note">{historyView.availNote}</div>
              </div>
              <div className="card stat col-3 col-3--md-6">
                <div className="stat__label">{historyView.primaryLabel}</div>
                <div className="stat__value" id="hist-median">
                  {historyView.primaryMedian}
                </div>
                <div className="stat__note">{historyView.primaryNote}</div>
              </div>
              <div className="card stat col-3 col-3--md-6">
                <div className="stat__label">Resolution</div>
                <div className="stat__value" id="hist-res">
                  {(ZOOM_LEVELS.find((z) => z.id === history.zoomLevel) || ZOOM_LEVELS[0]).label}
                </div>
                <div className="stat__note">{`Step ${formatStepLabel(history.stepSec)} · ${fmtSpanDuration(
                  historyView.spanMs
                )}`}</div>
              </div>
              <div className="card stat col-3 col-3--md-6">
                <div className="stat__label">Detail level</div>
                <div className="stat__value" id="hist-series-count">
                  {historyView.seriesCountText}
                </div>
                <div className="stat__note">{historyView.seriesCountNote}</div>
              </div>
            </section>

            <div className="zoom" id="hist-zoom-scope">
              <div className="row row--between" style={{ marginBottom: 'var(--sp-1)' }}>
                <b style={{ fontSize: 'var(--fs-xs)' }}>
                  {history.zoomLevel === 'overview'
                    ? 'Timeline overview (merged intervals)'
                    : `${(ZOOM_LEVELS.find((z) => z.id === history.zoomLevel) || ZOOM_LEVELS[0]).label} intervals`}
                </b>
                <span className="muted" style={{ fontSize: 'var(--fs-xs)' }}>
                  {`${fmtHistFull(historyView.fromMs)} -> ${fmtHistFull(historyView.toMs)}`}
                </span>
              </div>
              <div
                className="zoom__bar"
                role="group"
                aria-label={`Timeline intervals from ${fmtHistFull(historyView.fromMs)} to ${fmtHistFull(
                  historyView.toMs
                )}`}
              >
                {historyView.segments.map((seg) => {
                  let segStyle = { left: `${seg.leftPct.toFixed(2)}%`, width: `${seg.widthPct.toFixed(2)}%` };
                  let cls = 'zoom__seg';
                  if (seg.kind === 'gap') {
                    cls += ' zoom__seg--gap';
                  } else if (seg.kind === 'ok') {
                    segStyle = {
                      ...segStyle,
                      borderColor: 'var(--ok)',
                      background: 'var(--ok-soft)',
                      color: 'var(--ok)',
                    };
                  } else if (seg.kind === 'warn') {
                    segStyle = {
                      ...segStyle,
                      borderColor: 'var(--warn)',
                      background: 'var(--warn-soft)',
                      color: 'var(--warn)',
                    };
                  } else if (seg.kind === 'down') {
                    segStyle = {
                      ...segStyle,
                      borderColor: 'var(--danger)',
                      background: 'var(--danger-soft)',
                      color: 'var(--danger)',
                    };
                  }
                  return (
                    <button
                      key={seg.key}
                      type="button"
                      className={cls}
                      style={segStyle}
                      data-tip={seg.tipLines.join('|')}
                      aria-label={`${seg.rangeText}, ${seg.label}`}
                      onMouseEnter={(e) => showTipFromEvent(seg.tipLines, e)}
                      onMouseMove={(e) => showTipFromEvent(seg.tipLines, e)}
                      onMouseLeave={hideTip}
                      onFocus={(e) => showTipFromEvent(seg.tipLines, e)}
                      onBlur={hideTip}
                      onClick={() => {
                        hideTip();
                        const segSpan = seg.toMs - seg.fromMs;
                        if (segSpan >= 15000) {
                          history.zoomToRange(seg.fromMs, seg.toMs);
                        } else {
                          history.zoomIn((seg.fromMs + segSpan / 2 - historyView.fromMs) / historyView.spanMs);
                        }
                      }}
                    >
                      {seg.label}
                    </button>
                  );
                })}
              </div>
              <div className="zoom__axis" aria-hidden="true">
                {[0, 25, 50, 75, 100].map((p) => (
                  <span key={p} style={{ left: `${p}%` }}>
                    {fmtHistAxis(historyView.fromMs + (historyView.spanMs * p) / 100, historyView.spanMs)}
                  </span>
                ))}
              </div>
            </div>

            <figure className="chart-ph" aria-labelledby="hchart-cap">
              <figcaption id="hchart-cap" className="row row--between" style={{ marginBottom: 'var(--sp-2)' }}>
                <b id="hist-caption">{historyView.caption}</b>
                <span className="muted" id="hist-sub">
                  {historyView.sub}
                </span>
              </figcaption>
              <div
                ref={histPlotRef}
                className="chart-ph__plot plot"
                style={{ height: '12rem', cursor: 'crosshair', userSelect: 'none', touchAction: 'none' }}
                onPointerDown={handleHistPointerDown}
                onPointerMove={handleHistPointerMove}
                onPointerUp={handleHistPointerUp}
                onPointerCancel={() => setBrush(null)}
                onDoubleClick={(e) => {
                  const rect = e.currentTarget.getBoundingClientRect();
                  const frac = rect.width > 0 ? (e.clientX - rect.left) / rect.width : 0.5;
                  history.zoomIn(frac);
                }}
              >
                <svg
                  id="hist-svg"
                  viewBox="0 0 200 60"
                  preserveAspectRatio="none"
                  role="img"
                  aria-label="Historical monitor series chart"
                >
                  {historyView.lines.map((ln) => (
                    <g key={ln.name}>
                      {ln.pointsAttr && (
                        <polyline
                          fill="none"
                          stroke={`var(--series-${ln.colorIdx})`}
                          strokeWidth="1.6"
                          strokeDasharray={ln.dash || undefined}
                          vectorEffect="non-scaling-stroke"
                          points={ln.pointsAttr}
                        />
                      )}
                      {ln.dots.map((pt, i) => (
                        <circle
                          key={i}
                          cx={pt[0].toFixed(1)}
                          cy={pt[1].toFixed(1)}
                          r="1.4"
                          fill={`var(--series-${ln.colorIdx})`}
                        />
                      ))}
                    </g>
                  ))}
                  {historyView.failXs.map((x, idx) => (
                    <line
                      key={idx}
                      x1={x.toFixed(1)}
                      x2={x.toFixed(1)}
                      y1="53"
                      y2="59.5"
                      stroke="var(--danger)"
                      strokeWidth="1.4"
                      vectorEffect="non-scaling-stroke"
                    />
                  ))}
                </svg>
                <div
                  className="chart-ph__label"
                  id="hist-empty"
                  hidden={history.noStore || Boolean(history.error) || (!history.loading && historyView.allLen > 0)}
                >
                  <span>
                    {history.loading
                      ? 'Querying VictoriaMetrics history...'
                      : 'No persisted monitor samples in this time range'}
                  </span>
                </div>
                <div className="plot__cols" id="hist-cols">
                  {historyView.cols.map((col) => (
                    <span
                      key={col.key}
                      className="plot__col"
                      tabIndex={0}
                      data-tip={col.tipLines.join('|')}
                      aria-label={col.tipLines.join(', ')}
                      onMouseEnter={(e) => {
                        if (!brush) showTipFromEvent(col.tipLines, e);
                      }}
                      onMouseMove={(e) => {
                        if (!brush) showTipFromEvent(col.tipLines, e);
                      }}
                      onMouseLeave={hideTip}
                      onFocus={(e) => showTipFromEvent(col.tipLines, e)}
                      onBlur={hideTip}
                    />
                  ))}
                </div>
                {brush && (
                  <div
                    aria-hidden="true"
                    style={{
                      position: 'absolute',
                      top: 0,
                      bottom: 0,
                      left: `${(Math.min(brush.anchorFrac, brush.curFrac) * 100).toFixed(2)}%`,
                      width: `${(Math.abs(brush.curFrac - brush.anchorFrac) * 100).toFixed(2)}%`,
                      background: 'color-mix(in srgb, var(--accent) 18%, transparent)',
                      borderLeft: '1.5px solid var(--accent)',
                      borderRight: '1.5px solid var(--accent)',
                      pointerEvents: 'none',
                    }}
                  />
                )}
              </div>
              <div className="zoom__axis" id="hist-axis" aria-hidden="true">
                {[0, 25, 50, 75, 100].map((p) => (
                  <span key={p} style={{ left: `${p}%` }}>
                    {fmtHistAxis(historyView.fromMs + (historyView.spanMs * p) / 100, historyView.spanMs)}
                  </span>
                ))}
              </div>
              <div className="legend" id="hist-legend">
                {historyView.lines.map((ln, i) => {
                  const mod = i % 5;
                  const swCls = `legend__swatch${
                    mod === 1 ? ' legend__swatch--dash' : mod === 2 ? ' legend__swatch--dot' : ''
                  }`;
                  return (
                    <span key={ln.name} className="legend__item">
                      <span className={swCls} style={{ borderTopColor: `var(--series-${ln.colorIdx})` }} />
                      {ln.name}
                    </span>
                  );
                })}
              </div>
              <p className="field__hint" style={{ marginTop: 'var(--sp-2)' }}>
                Drag across the chart, scroll the wheel, click a timeline segment, or use the zoom controls to drill
                from the whole-timeline overview into per-agent and per-interval detail. Each zoom re-queries
                VictoriaMetrics at a finer resolution step.
              </p>
            </figure>

            <details className="card">
              <summary className="card__head" style={{ cursor: 'pointer' }}>
                <h2>
                  {history.zoomLevel === 'overview' || history.zoomLevel === 'day'
                    ? 'Timeline intervals (accessible alternative to the chart)'
                    : 'Series detail table (accessible alternative to the chart)'}
                </h2>
                <svg className="icon" aria-hidden="true">
                  <use href="#i-chevron" />
                </svg>
              </summary>
              <div className="table-wrap">
                {history.zoomLevel === 'overview' || history.zoomLevel === 'day' ? (
                  <table className="table">
                    <caption className="sr-only">Historical monitor intervals</caption>
                    <thead>
                      <tr>
                        <th scope="col">Interval</th>
                        <th scope="col">Status</th>
                        <th scope="col" className="num">
                          Samples
                        </th>
                        <th scope="col" className="num">
                          Availability
                        </th>
                        <th scope="col" className="num">
                          Median
                        </th>
                        <th scope="col" className="num">
                          95th pct
                        </th>
                        <th scope="col">
                          <span className="sr-only">Action</span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {historyView.segments.map((seg) => (
                        <tr key={seg.key}>
                          <th scope="row" className="mono">
                            {seg.rangeText}
                          </th>
                          <td>
                            <span
                              className={`chip chip--${
                                seg.kind === 'ok'
                                  ? 'ok'
                                  : seg.kind === 'warn'
                                  ? 'warn'
                                  : seg.kind === 'down'
                                  ? 'danger'
                                  : 'outline'
                              }`}
                            >
                              {seg.kind === 'ok'
                                ? 'Healthy'
                                : seg.kind === 'warn'
                                ? 'Degraded'
                                : seg.kind === 'down'
                                ? 'Outage'
                                : 'No data'}
                            </span>
                          </td>
                          <td className="num">{seg.count}</td>
                          <td className="num">{seg.okPct != null ? `${seg.okPct.toFixed(1)}%` : '-'}</td>
                          <td className="num">{seg.med != null ? fmtHistMetric(history.metric, seg.med) : '-'}</td>
                          <td className="num">{seg.p95 != null ? fmtHistMetric(history.metric, seg.p95) : '-'}</td>
                          <td className="actions">
                            <button
                              type="button"
                              className="btn"
                              onClick={() => history.zoomToRange(seg.fromMs, seg.toMs)}
                            >
                              Zoom in
                            </button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                ) : (
                  <table className="table">
                    <caption className="sr-only">Historical series detail</caption>
                    <thead>
                      <tr>
                        <th scope="col">Target</th>
                        <th scope="col">Monitor agent</th>
                        <th scope="col">Probe</th>
                        <th scope="col" className="num">
                          Points
                        </th>
                        <th scope="col" className="num">
                          Median
                        </th>
                        <th scope="col" className="num">
                          95th pct
                        </th>
                        <th scope="col" className="num">
                          Max
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {historyView.seriesRows.length === 0 ? (
                        <tr>
                          <td colSpan={7} className="faint">
                            No series in this range.
                          </td>
                        </tr>
                      ) : (
                        historyView.seriesRows.map((r) => (
                          <tr key={r.key}>
                            <th scope="row" className="cell-primary">
                              {r.label}
                            </th>
                            <td className="mono">{r.agent}</td>
                            <td>{r.type}</td>
                            <td className="num">{r.n}</td>
                            <td className="num">{r.med}</td>
                            <td className="num">{r.p95}</td>
                            <td className="num">{r.max}</td>
                          </tr>
                        ))
                      )}
                    </tbody>
                  </table>
                )}
              </div>
            </details>
          </div>
        </div>

        {/* ================= GRAFANA ================= */}
        <div
          className="card__body tabpanel"
          role="tabpanel"
          id="mp-grafana"
          aria-labelledby="mtab-grafana"
          hidden={activeTab !== 'grafana'}
        >
          <div className="stack">
            <div className="row row--between">
              <div className="row">
                <label className="sr-only" htmlFor="gf-dash">
                  Grafana dashboard
                </label>
                <select
                  className="select"
                  id="gf-dash"
                  style={{ width: 'auto' }}
                  value={grafanaDash.uid}
                  onChange={(e) => setGrafanaDashUid(e.target.value)}
                >
                  {GRAFANA_DASHBOARDS.map((d) => (
                    <option key={d.uid} value={d.uid}>
                      {d.label}
                    </option>
                  ))}
                </select>
              </div>
              <div className="row">
                <button
                  className="btn"
                  type="button"
                  id="gf-reload"
                  onClick={() => setGrafanaNonce((n) => n + 1)}
                >
                  <svg className="icon" aria-hidden="true">
                    <use href="#i-refresh" />
                  </svg>
                  <span>Reload</span>
                </button>
                <a
                  className="btn"
                  href={grafanaBaseUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  <svg className="icon" aria-hidden="true">
                    <use href="#i-arrow" />
                  </svg>
                  <span>Open in Grafana</span>
                </a>
              </div>
            </div>

            {grafanaMounted && (
              <iframe
                key={`${grafanaDash.uid}-${gfTheme}-${grafanaNonce}`}
                id="gf-frame"
                title={`Grafana: ${grafanaDash.label}`}
                src={grafanaEmbedUrl}
                style={{
                  width: '100%',
                  height: '70vh',
                  minHeight: '34rem',
                  border: '1.5px solid var(--border-strong)',
                  borderRadius: 'var(--radius)',
                  background: 'var(--surface-2)',
                }}
              />
            )}

            <p className="field__hint">
              Embedded under <code>/grafana</code> using the panel&apos;s Authelia SSO session, backed by the
              VictoriaMetrics time-series store.
            </p>
          </div>
        </div>

        {/* ================= PROBE CONFIG ================= */}
        <div
          className="card__body tabpanel"
          role="tabpanel"
          id="mp-cfg"
          aria-labelledby="mtab-cfg"
          hidden={activeTab !== 'cfg'}
        >
          <div className="stack">
            <section className="card" aria-labelledby="def-title">
              <div className="card__head">
                <div>
                  <h2 id="def-title">Global defaults</h2>
                  <p>Pushed to every monitor agent; a target can override frequency.</p>
                </div>
              </div>
              <form className="card__body stack" id="mon-defaults-form" onSubmit={handleSaveDefaults}>
                <div className="form-grid">
                  <div className="field">
                    <label htmlFor="d-freq">Probe frequency (ms)</label>
                    <input
                      className="input input--mono"
                      id="d-freq"
                      name="interval_ms"
                      inputMode="numeric"
                      required
                      value={defForm.interval_ms}
                      onChange={(e) => setDefForm((p) => ({ ...p, interval_ms: e.target.value }))}
                    />
                    <span className="field__hint" id="d-freq-h">
                      {`From ${bounds.min_interval_ms} ms to ${bounds.max_interval_ms / 1000} s.`}
                    </span>
                  </div>
                  <div className="field">
                    <label htmlFor="d-to-http">HTTP(S) timeout (ms)</label>
                    <input
                      className="input input--mono"
                      id="d-to-http"
                      name="http"
                      inputMode="numeric"
                      value={defForm.http}
                      onChange={(e) => setDefForm((p) => ({ ...p, http: e.target.value }))}
                    />
                  </div>
                  <div className="field">
                    <label htmlFor="d-to-tcp">TCP connect timeout (ms)</label>
                    <input
                      className="input input--mono"
                      id="d-to-tcp"
                      name="tcp"
                      inputMode="numeric"
                      value={defForm.tcp}
                      onChange={(e) => setDefForm((p) => ({ ...p, tcp: e.target.value }))}
                    />
                  </div>
                  <div className="field">
                    <label htmlFor="d-to-icmp">ICMP timeout (ms)</label>
                    <input
                      className="input input--mono"
                      id="d-to-icmp"
                      name="icmp"
                      inputMode="numeric"
                      value={defForm.icmp}
                      onChange={(e) => setDefForm((p) => ({ ...p, icmp: e.target.value }))}
                    />
                  </div>
                  <div className="field">
                    <label htmlFor="d-to-dns">DNS timeout (ms)</label>
                    <input
                      className="input input--mono"
                      id="d-to-dns"
                      name="dns"
                      inputMode="numeric"
                      value={defForm.dns}
                      onChange={(e) => setDefForm((p) => ({ ...p, dns: e.target.value }))}
                    />
                  </div>
                </div>
                <div className="row">
                  <button className="btn btn--primary" type="submit" disabled={savingDefs || noBackend}>
                    Save defaults and push
                  </button>
                  <span className="muted">The live window is chosen on the Live tab.</span>
                </div>
              </form>
            </section>

            <section className="card" aria-labelledby="jobs-title">
              <div className="card__head">
                <div>
                  <h2 id="jobs-title">Monitor jobs</h2>
                  <p>
                    Long-lived watchers, not tied to one test. One job per <code>(ip, origin)</code>.
                  </p>
                </div>
              </div>
              <div className="table-wrap">
                <table className="table">
                  <caption className="sr-only">Monitor jobs</caption>
                  <thead>
                    <tr>
                      <th scope="col">Origin</th>
                      <th scope="col">IP</th>
                      <th scope="col">Paths</th>
                      <th scope="col">Probes</th>
                      <th scope="col">Frequency</th>
                      <th scope="col">Monitor agents</th>
                      <th scope="col">
                        <span className="sr-only">Actions</span>
                      </th>
                    </tr>
                  </thead>
                  <tbody id="mon-jobs">
                    {noBackend ? (
                      <tr>
                        <td colSpan={7} className="faint">
                          No backend is configured on this server.
                        </td>
                      </tr>
                    ) : !configLoaded ? (
                      <tr>
                        <td colSpan={7} className="faint">
                          Loading...
                        </td>
                      </tr>
                    ) : configError ? (
                      <tr>
                        <td colSpan={7} className="faint">
                          {configError}
                        </td>
                      </tr>
                    ) : configJobs.length === 0 ? (
                      <tr>
                        <td colSpan={7} className="faint">
                          No monitor job yet. Create one below.
                        </td>
                      </tr>
                    ) : (
                      configJobs.map((j) => {
                        const typeSet = {};
                        const ivSet = {};
                        const agentSet = {};
                        (j.targets || []).forEach((t) => {
                          typeSet[t.target.type] = true;
                          if (t.target.interval_ms) ivSet[t.target.interval_ms] = true;
                          agentSet[t.agent] = true;
                        });
                        const ivs = Object.keys(ivSet);
                        const originLabel =
                          j.origin || (j.dns_name ? `dns://${j.dns_name}` : '(IP only)');
                        return (
                          <tr key={j.id}>
                            <th scope="row" className="cell-primary mono">
                              {originLabel}
                            </th>
                            <td className="mono">{j.ip}</td>
                            <td className={`mono${(j.paths || []).length ? '' : ' faint'}`}>
                              {(j.paths || []).join(', ') || 'none'}
                            </td>
                            <td>
                              {Object.keys(typeSet).map((t) => (
                                <span key={t}>
                                  <span className="chip chip--outline">{t.toUpperCase()}</span>{' '}
                                </span>
                              ))}
                            </td>
                            <td>
                              {ivs.length
                                ? `${ivs.join(', ')} ms (override)`
                                : `${config?.settings?.interval_ms || 1000} ms (default)`}
                            </td>
                            <td>{Object.keys(agentSet).sort().join(', ')}</td>
                            <td className="actions">
                              <button
                                type="button"
                                className="icon-btn icon-btn--sm"
                                aria-label={`Set frequency of monitor job ${j.origin || j.dns_name || j.ip}`}
                                title="Set frequency override"
                                onClick={() => handleEditInterval(j)}
                              >
                                <svg className="icon" aria-hidden="true">
                                  <use href="#i-edit" />
                                </svg>
                              </button>
                              <button
                                type="button"
                                className="icon-btn icon-btn--sm"
                                aria-label={`Delete monitor job ${j.origin || j.dns_name || j.ip}`}
                                title="Delete job"
                                onClick={() => handleDeleteJob(j)}
                              >
                                <svg className="icon" aria-hidden="true">
                                  <use href="#i-trash" />
                                </svg>
                              </button>
                            </td>
                          </tr>
                        );
                      })
                    )}
                  </tbody>
                </table>
              </div>
              <div className="card__foot" id="mon-agent-load">
                {configAgents.length > 0 ? (
                  `Each monitor agent is assigned 2-3 targets. Load now: ${configAgents
                    .map(
                      (a) =>
                        `${a.id} ${a.targets}/${a.max}${a.state === 'online' ? '' : ` (${a.state})`}`
                    )
                    .join(', ')}.`
                ) : (
                  <>
                    No monitor agent is registered: onboard one on the <Link to="/machines">Machines</Link> page before
                    creating a job.
                  </>
                )}
              </div>
            </section>

            {/* Create job: origin FIRST and most prominent, ip required, probe-type selector incl. DNS (GAP-19) */}
            <section className="card" aria-labelledby="new-title">
              <div className="card__head">
                <div>
                  <h2 id="new-title">Create monitor job</h2>
                  <p>Enter the URL first: the origin and path are derived from it.</p>
                </div>
              </div>
              <form className="card__body stack" id="mon-job-form" onSubmit={handleCreateJob}>
                <div className="field field--prominent">
                  <label htmlFor="j-url">Origin / URL</label>
                  <input
                    className="input input--mono"
                    id="j-url"
                    name="url"
                    type="url"
                    placeholder="https://shop.example.test/checkout"
                    aria-describedby="j-url-h"
                    value={jobForm.url}
                    onChange={(e) => handleUrlChange(e.target.value)}
                  />
                  <span className="field__hint" id="j-url-h">
                    Optional for ICMP or DNS-only jobs, required for TCP and HTTP. Derived: origin{' '}
                    <code>https://shop.example.test</code>, path <code>/checkout</code>. Two subdomains or two ports
                    are different origins.
                  </span>
                </div>
                <div className="form-grid">
                  <div className="field">
                    <label htmlFor="j-ip">
                      IP address <span className="faint">(required)</span>
                    </label>
                    <input
                      className="input input--mono"
                      id="j-ip"
                      name="ip"
                      inputMode="decimal"
                      placeholder="198.51.100.10"
                      required
                      aria-describedby="j-ip-h"
                      value={jobForm.ip}
                      onChange={(e) => setJobForm((p) => ({ ...p, ip: e.target.value }))}
                    />
                    <span className="field__hint" id="j-ip-h">
                      No duplicate job may exist for the same <code>(ip, origin)</code>.
                    </span>
                  </div>
                  <div className="field">
                    <label htmlFor="j-paths">Extra paths</label>
                    <input
                      className="input input--mono"
                      id="j-paths"
                      name="paths"
                      placeholder="/, /api/health"
                      value={jobForm.paths}
                      onChange={(e) => setJobForm((p) => ({ ...p, paths: e.target.value }))}
                    />
                    <span className="field__hint">Several paths of one origin are allowed.</span>
                  </div>
                  <div className="field">
                    <label htmlFor="j-freq">Frequency override (ms)</label>
                    <input
                      className="input input--mono"
                      id="j-freq"
                      name="interval_ms"
                      inputMode="numeric"
                      placeholder="default"
                      value={jobForm.interval_ms}
                      onChange={(e) => setJobForm((p) => ({ ...p, interval_ms: e.target.value }))}
                    />
                  </div>
                </div>

                <fieldset className="field">
                  <legend>Probe types</legend>
                  <div className="row" role="group" aria-label="Probe types" style={{ gap: 'var(--sp-4)' }}>
                    {PROBE_TYPES.map((pt) => {
                      const checked = probeTypes.includes(pt.id);
                      return (
                        <label key={pt.id} className="check" title={pt.hint}>
                          <input
                            type="checkbox"
                            name="probe_types"
                            value={pt.id}
                            checked={checked}
                            onChange={(e) => toggleProbeType(pt.id, e.target.checked)}
                          />
                          <span>{pt.label}</span>
                        </label>
                      );
                    })}
                  </div>
                  <span className="field__hint">
                    Choose which probe targets to create (ICMP, TCP, HTTP, DNS). Targets are spread over the monitor
                    agents (2-3 each).
                  </span>
                </fieldset>

                {dnsSelected && (
                  <div className="form-grid">
                    <div className="field">
                      <label htmlFor="j-dns-name">
                        DNS query name {!jobForm.url.trim() && <span className="faint">(required)</span>}
                      </label>
                      <input
                        className="input input--mono"
                        id="j-dns-name"
                        name="dns_name"
                        placeholder="shop.example.test"
                        required={!jobForm.url.trim()}
                        value={jobForm.dns_name}
                        onChange={(e) => setJobForm((p) => ({ ...p, dns_name: e.target.value }))}
                      />
                      <span className="field__hint">
                        Hostname to resolve. Defaults to the Origin / URL hostname when omitted.
                      </span>
                    </div>
                    <div className="field">
                      <label htmlFor="j-dns-server">DNS resolver (optional)</label>
                      <input
                        className="input input--mono"
                        id="j-dns-server"
                        name="dns_server"
                        placeholder="system resolver, or 198.51.100.12:53"
                        value={jobForm.dns_server}
                        onChange={(e) => setJobForm((p) => ({ ...p, dns_server: e.target.value }))}
                      />
                      <span className="field__hint">
                        UDP resolver <code>host:port</code> (or IP, defaults to port 53). Empty uses the agent&apos;s
                        system resolver.
                      </span>
                    </div>
                  </div>
                )}

                <div className="row">
                  <button className="btn btn--primary" type="submit" disabled={creatingJob || noBackend}>
                    <svg className="icon" aria-hidden="true">
                      <use href="#i-plus" />
                    </svg>
                    Create monitor job
                  </button>
                </div>
              </form>
            </section>
          </div>
        </div>
      </div>
    </>
  );
}
