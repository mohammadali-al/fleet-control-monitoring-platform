import { useCallback, useEffect, useRef, useState } from 'react';
import { StatusAlert } from '../components/StatusAlert';
import { api } from '../lib/api';
import { useEvents } from '../lib/events';
import { fmtAgo, fmtDateTime } from '../lib/fmt';
import { usePageTitle } from '../lib/title';

const RESET_NOTE =
  'The watchdog clears the counters at its next sample (a few seconds). Cooldowns are kept, so a reset does not allow an immediate second restart or reboot.';

const COND_MAP = {
  normal: ['ok', 'Normal', 'check'],
  breaching: ['warn', 'Breaching', 'alert'],
  throttled: ['warn', 'Throttled', 'alert'],
  'alert-only': ['danger', 'Alert-only', 'alert'],
  'not-reporting': ['neutral', 'Not reporting', 'x'],
};

const KIND_MAP = {
  breach: ['warn', 'Breach'],
  recovered: ['ok', 'Recovered'],
  action: ['warn', 'Action'],
  'action-failed': ['danger', 'Action failed'],
  suppressed: ['info', 'Held back'],
  'alert-only': ['danger', 'Alert-only'],
  unthrottled: ['ok', 'Throttle lifted'],
  'day-rollover': ['info', 'Day rollover'],
  policy: ['info', 'Policy'],
  'counters-reset': ['info', 'Counter reset'],
};

const STEP_MAP = {
  throttle: 'Throttle',
  kill_session: 'Kill session',
  restart: 'Service restart',
  reboot: 'Reboot',
  unthrottle: 'Unthrottle',
};

function fmtThreshold(t) {
  return t && t.trigger ? `trigger ${t.trigger}, recover ${t.recover}` : 'off';
}

function eventDetail(ev) {
  const bits = [];
  if (ev.detail) bits.push(ev.detail);
  if (ev.scope || ev.metric) {
    bits.push(
      `${ev.scope ? `${ev.scope} ` : ''}${ev.metric || ''}${
        ev.value ? ` at ${Math.round(ev.value * 10) / 10}` : ''
      }`
    );
  }
  if (ev.target) bits.push(`target ${ev.target}`);
  return bits.join(' · ');
}

function Ladder({ level, alertOnly }) {
  return (
    <span className="ladder" aria-hidden="true">
      {[1, 2, 3, 4].map((i) => {
        const cls = i <= level ? (alertOnly || i >= 3 ? 'on hi' : 'on') : '';
        return <i key={i} className={cls || undefined} />;
      })}
    </span>
  );
}

export function WatchdogPage() {
  usePageTitle('Watchdog');

  const [noBackend, setNoBackend] = useState(false);
  const [status, setStatus] = useState(null);

  // Hosts & summary
  const [hostsLoaded, setHostsLoaded] = useState(false);
  const [hostsError, setHostsError] = useState('');
  const [summary, setSummary] = useState(null);
  const [policyMeta, setPolicyMeta] = useState({});
  const [hosts, setHosts] = useState([]);

  // Events
  const [eventsError, setEventsError] = useState('');
  const [events, setEvents] = useState([]);

  // Policy editor
  const [policyTab, setPolicyTab] = useState('yaml'); // 'yaml' | 'form'
  const [yamlText, setYamlText] = useState('');
  const [policyDoc, setPolicyDoc] = useState(null);
  const [savingPolicy, setSavingPolicy] = useState(false);
  const dirtyRef = useRef(false);

  // Reset buttons
  const [resettingAll, setResettingAll] = useState(false);
  const [resettingHost, setResettingHost] = useState('');

  // Periodic tick to keep relative "ago" timestamps current
  const [, setTick] = useState(0);

  const tabRefs = useRef({});
  const debounceRef = useRef(0);

  const loadHosts = useCallback(async () => {
    const r = await api('GET', '/api/watchdog');
    setHostsLoaded(true);
    if (r.status === 503) {
      setNoBackend(true);
      setHostsError('');
      setSummary(null);
      setPolicyMeta({});
      setHosts([]);
      return;
    }
    if (!r.ok) {
      setHostsError(r.data.error || 'The watchdog data is unavailable.');
      return;
    }
    setNoBackend(false);
    setHostsError('');
    setSummary(r.data.summary || null);
    setPolicyMeta(r.data.policy || {});
    setHosts(Array.isArray(r.data.hosts) ? r.data.hosts : []);
  }, []);

  const loadEvents = useCallback(async () => {
    const r = await api('GET', '/api/watchdog/events');
    if (r.status === 503) {
      setNoBackend(true);
      setEventsError('');
      setEvents([]);
      return;
    }
    if (!r.ok) {
      setEventsError(r.data.error || 'Unavailable.');
      return;
    }
    setEventsError('');
    setEvents(Array.isArray(r.data.events) ? r.data.events : []);
  }, []);

  const loadPolicy = useCallback(async (force = false) => {
    const r = await api('GET', '/api/watchdog/policy');
    if (r.status === 503) {
      setNoBackend(true);
      setYamlText('');
      setPolicyDoc(null);
      return;
    }
    if (!r.ok) {
      setYamlText('');
      setStatus({ kind: 'danger', msg: r.data.error || 'The policy is unavailable.' });
      return;
    }
    const d = r.data;
    if (force || !dirtyRef.current) {
      const nextYaml = d.has_profile
        ? String(d.yaml || '').replace(/^version:\s*\d+/m, `version: ${(d.version || 0) + 1}`)
        : String(d.yaml || '');
      setYamlText(nextYaml);
      dirtyRef.current = false;
    }
    setPolicyDoc(d);
  }, []);

  const refreshHostsAndEvents = useCallback(() => {
    loadHosts();
    loadEvents();
  }, [loadHosts, loadEvents]);

  const refreshSoon = useCallback(() => {
    if (debounceRef.current) window.clearTimeout(debounceRef.current);
    debounceRef.current = window.setTimeout(() => {
      debounceRef.current = 0;
      refreshHostsAndEvents();
    }, 300);
  }, [refreshHostsAndEvents]);

  useEffect(() => {
    loadHosts();
    loadEvents();
    loadPolicy(true);
    const iv = window.setInterval(() => {
      setTick((t) => t + 1);
      if (!document.hidden) {
        loadHosts();
        loadEvents();
      }
    }, 15000);
    return () => {
      window.clearInterval(iv);
      if (debounceRef.current) window.clearTimeout(debounceRef.current);
    };
  }, [loadHosts, loadEvents, loadPolicy]);

  // Subscribe to persistent shell /ws/events for machine online/offline updates.
  useEvents(
    (msg) => {
      if (msg && msg.type === 'machine') refreshSoon();
    },
    refreshHostsAndEvents
  );

  // Subscribe to /ws/watchdog for live watchdog events pushed by worker and monitor agents.
  useEffect(() => {
    if (noBackend) return undefined;
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
        if (again) refreshHostsAndEvents();
      };
      ws.onmessage = (ev) => {
        let m;
        try {
          m = JSON.parse(ev.data);
        } catch {
          return;
        }
        if (m && m.type === 'event') refreshSoon();
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
  }, [noBackend, refreshHostsAndEvents, refreshSoon]);

  const handleReset = async (machine) => {
    const what = machine
      ? `the counters and alert-only latch of ${machine}`
      : 'the counters and alert-only latch of every connected worker agent';
    if (!window.confirm(`Reset ${what}? ${RESET_NOTE}`)) return;

    if (machine) setResettingHost(machine);
    else setResettingAll(true);

    const r = await api('POST', '/api/watchdog/reset', machine ? { machine } : {});

    if (machine) setResettingHost('');
    else setResettingAll(false);

    if (!r.ok) {
      setStatus({ kind: 'danger', msg: r.data.error || 'The counters were not reset.' });
      return;
    }
    const resetList = Array.isArray(r.data.reset) ? r.data.reset : [];
    const failedList = Array.isArray(r.data.failed) ? r.data.failed : [];
    const n = resetList.length;
    const bad = failedList.length;

    if (machine) {
      setStatus({ kind: 'ok', msg: `Reset sent to ${machine}. ${RESET_NOTE}` });
    } else if (!n && !bad) {
      setStatus({ kind: 'info', msg: 'No worker agent is connected; nothing was reset.' });
    } else {
      setStatus({
        kind: bad ? 'danger' : 'ok',
        msg: `Reset sent to ${n} worker agent${n === 1 ? '' : 's'}${
          bad ? `; failed for ${failedList.join(', ')}` : ''
        }. ${RESET_NOTE}`,
      });
    }
    loadHosts();
    loadEvents();
  };

  const handlePolicySave = async (e) => {
    e.preventDefault();
    setSavingPolicy(true);
    const r = await api('POST', '/api/watchdog/policy', { yaml: yamlText });
    setSavingPolicy(false);
    if (!r.ok) {
      setStatus({ kind: 'danger', msg: r.data.error || `Not saved (${r.status})` });
      return;
    }
    dirtyRef.current = false;
    setStatus({
      kind: 'ok',
      msg: `Version ${r.data.version} saved and pushed to ${r.data.pushed_to} connected worker agent(s). Offline agents get it when they reconnect.`,
    });
    loadPolicy(true);
    loadHosts();
  };

  const handlePolicyRevert = () => {
    dirtyRef.current = false;
    loadPolicy(true);
    setStatus({ kind: 'info', msg: 'Edits discarded.' });
  };

  const handlePolicyTabKeyDown = (e) => {
    const order = ['yaml', 'form'];
    const idx = order.indexOf(policyTab);
    let next = null;
    if (e.key === 'ArrowRight') next = order[(idx + 1) % order.length];
    else if (e.key === 'ArrowLeft') next = order[(idx + order.length - 1) % order.length];
    else if (e.key === 'Home') next = order[0];
    else if (e.key === 'End') next = order[order.length - 1];
    if (next) {
      e.preventDefault();
      setPolicyTab(next);
      tabRefs.current[next]?.focus();
    }
  };

  const eff = policyDoc?.effective;
  const hostById = Object.fromEntries(hosts.map((h) => [h.machine, h]));
  const verText = policyMeta.stored ? `v${policyMeta.version}` : 'no profile';
  const inForceText = policyMeta.stored
    ? `In force on ${policyMeta.in_force} of ${policyMeta.workers} worker agents`
    : policyDoc
      ? 'Nothing pushed yet'
      : '';

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Watchdog</h1>
          <p>
            Keeps a machine from hitting maximum resource use and becoming unreachable. It is a load safety net, not a
            tamper-proof supervisor: everything else is for the human watching this panel.
          </p>
        </div>
        <div className="page-head__actions">
          <button
            className="btn"
            type="button"
            id="wd-reset-all"
            title="Clear the daily restart and reboot counts, the alert-only latch and the ladder stage on every connected worker agent"
            disabled={resettingAll || noBackend}
            onClick={() => handleReset('')}
          >
            <svg className="icon" aria-hidden="true">
              <use href="#i-refresh" />
            </svg>
            Reset all counters
          </button>
        </div>
      </div>

      {noBackend && (
        <div className="alert alert--warn" role="status" style={{ marginBottom: 'var(--sp-4)' }}>
          <svg className="icon" aria-hidden="true">
            <use href="#i-alert" />
          </svg>
          <div>The panel is running without a backend, so watchdog data and counter resets are unavailable.</div>
        </div>
      )}

      <StatusAlert status={status} id="wd-status" style={{ marginBottom: 'var(--sp-4)' }} />

      <div className="alert" style={{ marginBottom: 'var(--sp-4)' }}>
        <svg className="icon" aria-hidden="true">
          <use href="#i-info" />
        </svg>
        <div>
          <strong>Thresholds sit above the Load Controller&apos;s operating ceiling.</strong> A Maneuver deliberately
          drives CPU and RAM toward its ceiling; the watchdog only acts as a backstop for load that escapes the
          controller, such as a runaway manual command.
        </div>
      </div>

      <section className="grid" aria-label="Fleet watchdog summary" style={{ marginBottom: 'var(--sp-4)' }}>
        <div className="card stat col-3 col-3--md-6">
          <div className="stat__label">Normal</div>
          <div className="stat__value">
            <span id="wd-n-normal">{summary ? summary.normal : '-'}</span> <small>hosts</small>
          </div>
          <div className="stat__note">Below every threshold</div>
        </div>
        <div className="card stat col-3 col-3--md-6">
          <div className="stat__label">Acting</div>
          <div className="stat__value" id="wd-n-acting">
            {summary ? summary.acting : '-'}
          </div>
          <div className="stat__note">Breaching or throttled</div>
        </div>
        <div className="card stat col-3 col-3--md-6">
          <div className="stat__label">Alert-only</div>
          <div className="stat__value" id="wd-n-alert">
            {summary ? summary.alert_only : '-'}
          </div>
          <div className="stat__note">Daily caps used up</div>
        </div>
        <div className="card stat col-3 col-3--md-6">
          <div className="stat__label">Not reporting</div>
          <div className="stat__value" id="wd-n-off">
            {summary ? summary.not_reporting : '-'}
          </div>
          <div className="stat__note">Offline agents</div>
        </div>
      </section>

      <div className="stack">
        {/* Per-host state */}
        <section className="card" aria-labelledby="ph-title">
          <div className="card__head">
            <div>
              <h2 id="ph-title">Hosts</h2>
              <p>Escalation stage, guard counters and policy state per agent (workers and headless monitor agents).</p>
            </div>
          </div>
          <div className="table-wrap">
            <table className="table table--sticky-first">
              <caption className="sr-only">Per-host watchdog state</caption>
              <thead>
                <tr>
                  <th scope="col">Host</th>
                  <th scope="col">Stage</th>
                  <th scope="col">Policy</th>
                  <th scope="col" className="num">
                    Restarts today
                  </th>
                  <th scope="col" className="num">
                    Reboots today
                  </th>
                  <th scope="col">Last event</th>
                  <th scope="col">
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody id="wd-hosts">
                {noBackend ? (
                  <tr>
                    <td colSpan={7} className="faint">
                      No backend is configured on this server.
                    </td>
                  </tr>
                ) : !hostsLoaded ? (
                  <tr>
                    <td colSpan={7} className="faint">
                      Loading...
                    </td>
                  </tr>
                ) : hostsError ? (
                  <tr>
                    <td colSpan={7} className="faint">
                      {hostsError}
                    </td>
                  </tr>
                ) : hosts.length === 0 ? (
                  <tr>
                    <td colSpan={7} className="faint">
                      No agents are registered.
                    </td>
                  </tr>
                ) : (
                  hosts.map((h) => {
                    const [tone, label, ic] = COND_MAP[h.condition] || COND_MAP.normal;
                    const notReporting = h.condition === 'not-reporting';
                    const isMonitor = h.role === 'monitor' || h.headless;
                    const lastEvtKind = h.last_event
                      ? (KIND_MAP[h.last_event.kind] || [null, h.last_event.kind])[1]
                      : '';
                    return (
                      <tr key={h.machine}>
                        <th scope="row" className="cell-primary">
                          {h.machine}
                          <div className="cell-sub">
                            {isMonitor ? 'Monitor agent (headless)' : 'Worker agent'}
                            {h.connection === 'online' ? '' : ` (${h.connection})`}
                          </div>
                        </th>
                        <td>
                          <span className={`chip chip--${tone}`}>
                            <svg className="icon icon--sm" aria-hidden="true">
                              <use href={`#i-${ic}`} />
                            </svg>
                            {label}
                          </span>
                          {!notReporting && (
                            <>
                              {' '}
                              <Ladder level={h.level} alertOnly={h.condition === 'alert-only'} />
                            </>
                          )}
                          {h.stage !== 'none' && (
                            <div className="cell-sub">last step: {STEP_MAP[h.stage] || h.stage}</div>
                          )}
                          {h.breaches && h.breaches.length > 0 && (
                            <div className="cell-sub">breached: {h.breaches.join(', ')}</div>
                          )}
                        </td>
                        <td
                          className={`nowrap${
                            isMonitor || (!h.policy_acked && !h.policy_pushed) ? ' faint' : ''
                          }`}
                        >
                          {isMonitor ? (
                            'headless'
                          ) : h.policy_acked && h.policy_ok ? (
                            `v${h.policy_in_force} in force`
                          ) : h.policy_acked ? (
                            <>
                              <span className="chip chip--danger">
                                <svg className="icon icon--sm" aria-hidden="true">
                                  <use href="#i-x" />
                                </svg>
                                {`Refused v${h.policy_pushed}`}
                              </span>
                              <div className="cell-sub">{h.policy_error || ''}</div>
                            </>
                          ) : h.policy_pushed ? (
                            `v${h.policy_pushed} pushed, no ack yet`
                          ) : (
                            'default'
                          )}
                        </td>
                        <td className={`num${notReporting ? ' faint' : ''}`}>
                          {notReporting ? '-' : `${h.restarts_today} / ${h.restart_cap}`}
                        </td>
                        <td className={`num${notReporting ? ' faint' : ''}`}>
                          {notReporting ? '-' : `${h.reboots_today} / ${h.reboot_cap}`}
                        </td>
                        <td className={`nowrap${h.last_event ? '' : ' faint'}`}>
                          {h.last_event ? `${fmtAgo(h.last_event.time)}: ${lastEvtKind}` : 'none'}
                        </td>
                        <td className="actions">
                          {isMonitor ? (
                            <span
                              className="faint nowrap"
                              title="Monitor agents run a headless watchdog without remote counter reset"
                            >
                              Headless
                            </span>
                          ) : (
                            <button
                              className="btn btn--sm"
                              type="button"
                              disabled={resettingHost === h.machine || noBackend}
                              onClick={() => handleReset(h.machine)}
                            >
                              Reset counters
                            </button>
                          )}
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
          <div className="card__foot">
            The stage and the day&apos;s counters are updated from the events each agent forwards (restarts and reboots
            taken today, the last step of the ladder, alert-only when the caps were hit) and persisted on the server
            across restarts (daily counters stay scoped to today). The watchdog runs as its own service on each host (
            <code>Restart=always</code>, with an independent timer checking it every 45 seconds: it restarts a stopped
            service and one whose heartbeat has gone stale, meaning hung). Monitor agents run a headless watchdog and
            are included here alongside worker agents.
          </div>
        </section>

        {/* Policy editor */}
        <section className="card" aria-labelledby="pol-title">
          <div className="card__head">
            <div>
              <h2 id="pol-title">Policy profile</h2>
              <p>
                Version-stamped YAML, stored on the main server, pushed over the control channel and applied live
                without a restart.
              </p>
            </div>
            <div className="row">
              <span className="chip chip--outline chip--mono" id="wd-ver">
                {verText}
              </span>
              <span className="chip chip--outline" id="wd-inforce">
                {inForceText}
              </span>
            </div>
          </div>

          <div className="tabs" role="tablist" aria-label="Policy editor view">
            <button
              ref={(el) => {
                tabRefs.current.yaml = el;
              }}
              className="tab"
              role="tab"
              id="ptab-yaml"
              aria-controls="pp-yaml"
              aria-selected={String(policyTab === 'yaml')}
              tabIndex={policyTab === 'yaml' ? 0 : -1}
              type="button"
              onClick={() => setPolicyTab('yaml')}
              onKeyDown={handlePolicyTabKeyDown}
            >
              YAML
            </button>
            <button
              ref={(el) => {
                tabRefs.current.form = el;
              }}
              className="tab"
              role="tab"
              id="ptab-form"
              aria-controls="pp-form"
              aria-selected={String(policyTab === 'form')}
              tabIndex={policyTab === 'form' ? 0 : -1}
              type="button"
              onClick={() => setPolicyTab('form')}
              onKeyDown={handlePolicyTabKeyDown}
            >
              Effective values
            </button>
          </div>

          <form id="wd-policy-form" onSubmit={handlePolicySave}>
            <div
              className="card__body tabpanel"
              role="tabpanel"
              id="pp-yaml"
              aria-labelledby="ptab-yaml"
              hidden={policyTab !== 'yaml'}
            >
              <div className="field">
                <label htmlFor="wd-yaml">Policy profile (YAML)</label>
                <textarea
                  className="textarea input--mono"
                  id="wd-yaml"
                  rows={26}
                  spellCheck={false}
                  style={{ minHeight: '24rem' }}
                  aria-describedby="wd-yaml-h"
                  value={yamlText}
                  onChange={(e) => {
                    dirtyRef.current = true;
                    setYamlText(e.target.value);
                  }}
                />
                <span className="field__hint" id="wd-yaml-h">
                  {policyDoc ? (
                    policyDoc.has_profile ? (
                      <>
                        {`Stored profile v${policyDoc.version}, with the version already raised to ${
                          (policyDoc.version || 0) + 1
                        } for your edit. `}
                        Every key is optional; an absent key keeps the default. Per-host and per-group overrides go
                        under <code>overrides:</code>. A profile that fails validation is refused and nothing is pushed.
                      </>
                    ) : (
                      <>
                        No profile is stored yet: this is a starting point built from the baked-in default. Every key
                        is optional; an absent key keeps the default. Per-host and per-group overrides go under{' '}
                        <code>overrides:</code>. A profile that fails validation is refused and nothing is pushed.
                      </>
                    )
                  ) : (
                    <>
                      Every key is optional; an absent key keeps the baked-in default. Raise <code>version</code> on
                      every save. Per-host and per-group overrides go under <code>overrides:</code>. A profile that
                      fails validation is refused here and nothing is pushed.
                    </>
                  )}
                </span>
              </div>
              <div className="field__hint" id="wd-hb-h" style={{ marginTop: 'var(--sp-3)' }}>
                <strong>Hang detection.</strong> <code>heartbeat_write</code> is how often the watchdog and the main
                agent write a heartbeat (2 to 15 s). <code>heartbeat_threshold</code> is how old the heartbeat may get
                before the liveness timer restarts the service (45 to 300 s, and at least 3 times{' '}
                <code>heartbeat_write</code>). The timer itself checks every 45 s; that cadence is fixed and cannot be
                changed here. A value outside these limits is refused on save.
              </div>
            </div>

            <div
              className="card__body tabpanel"
              role="tabpanel"
              id="pp-form"
              aria-labelledby="ptab-form"
              hidden={policyTab !== 'form'}
            >
              <p className="field__hint" style={{ marginBottom: 'var(--sp-3)' }}>
                What the stored default profile resolves to (read-only). Change it in the YAML tab.
              </p>
              <dl className="kv" id="wd-effective">
                {eff && (
                  <>
                    <dt>Sampling interval / sustained breach</dt>
                    <dd className="mono">{`${eff.sample_interval} / ${eff.sustain}`}</dd>

                    <dt>CPU (machine)</dt>
                    <dd className="mono">{`${fmtThreshold(eff.machine?.cpu_percent)} %`}</dd>

                    <dt>Memory (machine)</dt>
                    <dd className="mono">{`${fmtThreshold(eff.machine?.memory_percent)} %`}</dd>

                    <dt>Load per CPU</dt>
                    <dd className="mono">{fmtThreshold(eff.machine?.load_per_cpu)}</dd>

                    <dt>Disk</dt>
                    <dd className="mono">{`${fmtThreshold(eff.machine?.disk_percent)} %`}</dd>

                    <dt>Ladder steps</dt>
                    <dd className="mono">
                      {['throttle', 'kill_session', 'restart', 'reboot']
                        .map((k) => `${k} ${eff.steps?.[k] ? 'on' : 'off'}`)
                        .join(', ')}
                    </dd>

                    <dt>Throttle</dt>
                    <dd className="mono">{`CPU quota ${eff.throttle?.cpu_percent} % for ${eff.throttle?.duration}`}</dd>

                    <dt>Cooldowns</dt>
                    <dd className="mono">
                      {`throttle ${eff.cooldowns?.throttle}, kill ${eff.cooldowns?.kill_session}, restart ${eff.cooldowns?.restart}, reboot ${eff.cooldowns?.reboot}`}
                    </dd>

                    <dt>Daily caps</dt>
                    <dd className="mono">
                      {`${eff.caps?.restarts_per_day} restarts, ${eff.caps?.reboots_per_day} reboots`}
                    </dd>

                    <dt>Boot stability / stage reset</dt>
                    <dd className="mono">{`${eff.boot_window} / ${eff.stage_reset}`}</dd>

                    <dt>Auto-reset counters at midnight</dt>
                    <dd className="mono">{eff.auto_reset_midnight ? 'yes' : 'no'}</dd>

                    <dt>Heartbeat write / hang threshold</dt>
                    <dd className="mono">
                      {`${eff.heartbeat_write} / ${eff.heartbeat_threshold} (liveness check every 45 s, fixed)`}
                    </dd>

                    <dt>Overrides</dt>
                    <dd className="mono">
                      {`${
                        policyDoc.hosts?.length ? `hosts: ${policyDoc.hosts.join(', ')}` : 'no host overrides'
                      }; ${
                        policyDoc.groups?.length ? `groups: ${policyDoc.groups.join(', ')}` : 'no group overrides'
                      }`}
                    </dd>
                  </>
                )}
              </dl>
            </div>

            <div className="card__foot row row--between">
              <span>
                Saving pushes the new version to every connected worker agent; an agent that is offline gets it when it
                reconnects, and the last known profile keeps running if the main server is unreachable.
              </span>
              <span className="row">
                <button className="btn" type="button" id="wd-revert" disabled={noBackend} onClick={handlePolicyRevert}>
                  Revert
                </button>
                <button className="btn btn--primary" type="submit" disabled={savingPolicy || noBackend}>
                  Save and push
                </button>
              </span>
            </div>
          </form>
        </section>

        {/* Events */}
        <section className="card" aria-labelledby="ev-title">
          <div className="card__head">
            <div>
              <h2 id="ev-title">Events</h2>
              <p>Every watchdog action and every attempt to stop it is logged here.</p>
            </div>
          </div>
          <div className="table-wrap">
            <table className="table">
              <caption className="sr-only">Watchdog events</caption>
              <thead>
                <tr>
                  <th scope="col">Time</th>
                  <th scope="col">Host</th>
                  <th scope="col">Event</th>
                  <th scope="col">Detail</th>
                </tr>
              </thead>
              <tbody id="wd-events">
                {eventsError ? (
                  <tr>
                    <td colSpan={4} className="faint">
                      {eventsError}
                    </td>
                  </tr>
                ) : events.length === 0 ? (
                  <tr>
                    <td colSpan={4} className="faint">
                      No watchdog event has been reported yet.
                    </td>
                  </tr>
                ) : (
                  events.map((ev, idx) => {
                    const [baseTone, baseLabel] = KIND_MAP[ev.kind] || ['info', ev.kind];
                    const label = `${baseLabel}${
                      ev.kind === 'action' && ev.step ? `: ${STEP_MAP[ev.step] || ev.step}` : ''
                    }`;
                    const tone =
                      ev.kind === 'action' && (ev.step === 'restart' || ev.step === 'reboot') ? 'danger' : baseTone;
                    const hostMeta = hostById[ev.machine];
                    const isMonEv = hostMeta && (hostMeta.role === 'monitor' || hostMeta.headless);
                    return (
                      <tr key={`${ev.machine}-${ev.time}-${ev.kind}-${idx}`}>
                        <td className="nowrap">{fmtDateTime(ev.time)}</td>
                        <td>
                          {ev.machine}
                          {isMonEv && <div className="cell-sub">Monitor agent (headless)</div>}
                        </td>
                        <td>
                          <span className={`chip chip--${tone}`}>{label}</span>
                        </td>
                        <td>{eventDetail(ev)}</td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
          <div className="card__foot">
            The latest 200 events per host are kept in memory (the agent keeps the full log); they reset when the server
            restarts.
          </div>
        </section>
      </div>
    </>
  );
}
