import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { StatusAlert } from '../components/StatusAlert';
import { useMachines } from '../hooks/useMachines';
import { useMonitor } from '../hooks/useMonitor';
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

  const [activeTab, setActiveTab] = useState('live'); // 'live' | 'grafana' | 'cfg'
  const tabRefs = useRef({});
  const [status, setStatus] = useState(null);
  const [grafanaDashUid, setGrafanaDashUid] = useState(GRAFANA_DASHBOARDS[0].uid);
  const [grafanaMounted, setGrafanaMounted] = useState(false);
  const [grafanaNonce, setGrafanaNonce] = useState(0);

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
    const order = ['live', 'grafana', 'cfg'];
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
