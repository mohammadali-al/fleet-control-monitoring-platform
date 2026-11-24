/* Fleet panel - live wiring for the Operate pages (Machines, Command runner, Terminal).
   Loaded after app.js. app.js keeps the shared behaviour (theme, drawer, menus, dialogs, tabs, stub toasts);
   this file adds fetch + WebSocket where a page is backed by the main server:
     GET/POST /api/...        JSON API (see internal/panel/api.go)
     GET      /fragments/...  server-rendered HTML pieces, swapped in when data changes
     WS       /ws/events      machine state and command lifecycle/output
     WS       /ws/shell       one interactive shell (xterm.js <-> agent PTY)
   Anything not backed yet keeps its data-stub marker. Plain ES5 + fetch, no framework, no build. */
(function () {
  'use strict';

  var page = document.body.getAttribute('data-page');
  if (['machines', 'commands', 'terminal'].indexOf(page) === -1) return;

  /* ---------- helpers ---------- */
  function $(sel, root) { return (root || document).querySelector(sel); }
  function $$(sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); }
  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }
  function icon(name, small) {
    var ns = 'http://www.w3.org/2000/svg';
    var s = document.createElementNS(ns, 'svg');
    s.setAttribute('class', small ? 'icon icon--sm' : 'icon');
    s.setAttribute('aria-hidden', 'true');
    var u = document.createElementNS(ns, 'use');
    u.setAttribute('href', '#i-' + name);
    s.appendChild(u);
    return s;
  }
  function chip(tone, text, ic, live) {
    var c = el('span', 'chip chip--' + tone + (live ? ' chip--live' : ''));
    if (ic) c.appendChild(icon(ic, true));
    c.appendChild(document.createTextNode(text));
    return c;
  }
  function toast(msg) {
    var region = $('.toast-region');
    if (!region) return;
    var t = el('div', 'toast', msg);
    region.appendChild(t);
    setTimeout(function () { t.remove(); }, 6000);
  }
  function debounce(fn, ms) {
    var t = null;
    return function () { if (t) clearTimeout(t); t = setTimeout(function () { t = null; fn(); }, ms); };
  }
  function fmtTime(iso) {
    if (!iso) return '';
    var d = new Date(iso);
    return isNaN(d) ? '' : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  }
  function fmtDur(ms) {
    if (ms == null) return '';
    var s = ms / 1000;
    if (s < 10) return s.toFixed(1) + ' s';
    s = Math.round(s);
    var m = Math.floor(s / 60);
    return m ? m + ':' + ('0' + (s % 60)).slice(-2) : s + ' s';
  }

  function api(method, url, body) {
    var opts = { method: method, headers: { 'Accept': 'application/json' }, credentials: 'same-origin' };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    return fetch(url, opts).then(function (r) {
      return r.text().then(function (txt) {
        var data = null;
        try { data = txt ? JSON.parse(txt) : null; } catch (e) { data = { error: txt || r.statusText }; }
        return { ok: r.ok, status: r.status, data: data || {} };
      });
    }, function (err) {
      return { ok: false, status: 0, data: { error: 'Cannot reach the main server: ' + err.message } };
    });
  }
  function fragment(name) {
    return fetch('/fragments/' + name, { credentials: 'same-origin' })
      .then(function (r) { if (!r.ok) throw new Error(r.statusText); return r.text(); });
  }

  /* Status line inside a form or dialog: kind is info | ok | danger. */
  function setStatus(root, kind, msg) {
    var box = $('[data-form-status]', root);
    if (!box) return;
    if (!msg) { box.hidden = true; box.textContent = ''; return; }
    box.hidden = false;
    box.className = 'alert' + (kind === 'info' ? '' : ' alert--' + kind);
    box.textContent = '';
    box.appendChild(icon(kind === 'ok' ? 'check' : kind === 'danger' ? 'alert' : 'info'));
    box.appendChild(el('div', null, msg));
  }

  /* ---------- /ws/events with automatic reconnect ---------- */
  var listeners = [];
  var reconnectListeners = [];
  var evSocket = null;
  var evAttempt = 0;
  function connectEvents() {
    var proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    var ws = new WebSocket(proto + location.host + '/ws/events');
    evSocket = ws;
    ws.onopen = function () {
      var again = evAttempt > 0;
      evAttempt = 0;
      if (again) reconnectListeners.forEach(function (f) { f(); }); // missed events: refetch state
    };
    ws.onmessage = function (e) {
      var m;
      try { m = JSON.parse(e.data); } catch (err) { return; }
      listeners.forEach(function (f) { f(m); });
    };
    ws.onclose = function () {
      evAttempt++;
      setTimeout(connectEvents, Math.min(10000, 500 * evAttempt));
    };
  }
  function onEvent(f) { listeners.push(f); }
  function onReconnect(f) { reconnectListeners.push(f); }
  connectEvents();

  /* ======================================================================
     Machines
     ====================================================================== */
  function initMachines() {
    var tiles = $('#machines-tiles');
    var rows = $('#machines-rows');
    var q = $('[data-filter-q]');
    var stateSel = $('[data-filter-state]');
    var count = $('[data-filter-count]');
    var empty = $('[data-filter-empty]');

    function applyFilter() {
      var role = ($('input[name="role"]:checked') || {}).value || 'all';
      var st = stateSel ? stateSel.value : 'all';
      var text = q ? q.value.trim().toLowerCase() : '';
      var all = $$('tr[data-id]', rows);
      var shown = 0;
      all.forEach(function (r) {
        var ok = (role === 'all' || r.dataset.role === role) &&
                 (st === 'all' || r.dataset.state === st) &&
                 (!text || r.textContent.toLowerCase().indexOf(text) !== -1);
        r.hidden = !ok;
        if (ok) shown++;
      });
      if (count) count.textContent = 'Showing ' + shown + ' of ' + all.length + ' machines';
      if (empty) empty.hidden = all.length === 0 || shown !== 0;
    }
    $$('input[name="role"]').forEach(function (i) { i.addEventListener('change', applyFilter); });
    if (q) q.addEventListener('input', applyFilter);
    if (stateSel) stateSel.addEventListener('change', applyFilter);

    var lastSig = '';
    function refresh() {
      // Do not swap rows while a row menu is open: it would close under the cursor.
      if ($('.menu__pop:not([hidden])')) return;
      Promise.all([fragment('machines-tiles'), fragment('machines-rows')]).then(function (parts) {
        if ($('.menu__pop:not([hidden])')) return;
        var sig = parts[0] + parts[1];
        if (sig === lastSig) return; // nothing changed: leave the DOM (and any click in flight) alone
        lastSig = sig;
        tiles.innerHTML = parts[0];
        rows.innerHTML = parts[1];
        applyFilter();
      }).catch(function () { /* the next event or tick retries */ });
    }
    var refreshSoon = debounce(refresh, 150);
    onEvent(function (m) { if (m.type === 'machine') refreshSoon(); });
    onReconnect(refresh);
    setInterval(refresh, 15000); // keeps the relative "ago" times honest
    applyFilter();

    /* Onboarding form */
    var form = $('#onboard-form');
    $$('input[name="auth"]', form).forEach(function (r) {
      r.addEventListener('change', function () {
        var key = form.elements.auth.value === 'key';
        $('[data-auth="password"]', form).hidden = key;
        $('[data-auth="key"]', form).hidden = !key;
      });
    });
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var f = form.elements;
      var useKey = f.auth.value === 'key';
      var body = {
        host: f.host.value.trim(), port: parseInt(f.port.value, 10) || 0, user: f.user.value.trim(),
        password: useKey ? '' : f.password.value, private_key: useKey ? f.private_key.value : '',
        role: f.role.value, connect_mode: f.connect_mode.value
      };
      var btn = $('button[type="submit"]', form);
      btn.disabled = true;
      setStatus(form, 'info', 'Provisioning ' + body.host + '. This installs the agent over SSH and can take a minute.');
      api('POST', '/api/machines', body).then(function (r) {
        btn.disabled = false;
        if (!r.ok) { setStatus(form, 'danger', r.data.error || ('Provisioning failed (' + r.status + ')')); return; }
        var m = r.data.machine;
        setStatus(form, 'ok', 'Provisioned ' + m.id + (r.data.created ? ' (new machine)' : ' (existing machine)') +
          '. It shows as online once its agent connects.');
        f.password.value = ''; f.private_key.value = ''; // credentials are never kept
        refresh();
      });
    });

    /* Re-provision / remove: a dialog collects one-off credentials */
    var dlg = $('#dlg-cred');
    var cform = $('#cred-form');
    var action = null;
    document.addEventListener('click', function (e) {
      var b = e.target.closest('[data-action]');
      if (!b) return;
      action = { kind: b.getAttribute('data-action'), machine: b.getAttribute('data-machine') };
      var remove = action.kind === 'remove';
      $('#dlg-cred-title').textContent = (remove ? 'Remove ' : 'Re-provision ') + action.machine;
      $('[data-cred-intro]').textContent = remove
        ? 'Removing stops the agent over SSH, decommissions the machine and records its client certificate for revocation. The record stays in the registry.'
        : 'Re-provisioning is idempotent: same machine ID, same identity; a certificate is issued only if missing. Credentials are used once and never stored.';
      $('[data-cred-submit]').textContent = remove ? 'Remove machine' : 'Re-provision';
      $('[data-cred-submit]').className = 'btn ' + (remove ? 'btn--danger' : 'btn--primary');
      $('[data-cred-skip]').hidden = !remove;
      setStatus(cform, 'info', '');
      dlg.showModal();
    });
    cform.addEventListener('submit', function (e) {
      e.preventDefault();
      var f = cform.elements;
      var body = { user: f.user.value.trim(), password: f.password.value, private_key: f.private_key.value, port: parseInt(f.port.value, 10) || 0 };
      var url = '/api/machines/' + encodeURIComponent(action.machine) + '/' + action.kind;
      if (action.kind === 'remove') body.skip_remote = f.skip_remote.checked;
      var btn = $('[data-cred-submit]');
      btn.disabled = true;
      setStatus(cform, 'info', 'Working...');
      api('POST', url, body).then(function (r) {
        btn.disabled = false;
        if (!r.ok) { setStatus(cform, 'danger', r.data.error || ('Failed (' + r.status + ')')); return; }
        f.password.value = ''; f.private_key.value = '';
        dlg.close();
        toast(action.kind === 'remove' ? action.machine + ' was removed.' : action.machine + ' was re-provisioned.');
        refresh();
      });
    });
  }

  /* ======================================================================
     Command runner
     ====================================================================== */
  function b64bytes(s) {
    var bin = atob(s), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  function initCommands() {
    var targets = $('#targets');
    var form = $('#run-form');
    var runsBox = $('#runs');
    var runsEmpty = $('#runs-empty');
    var tpl = $('#tpl-run');
    var histBody = $('#history-rows');
    var histFilter = $('[data-history-filter]');
    var liveChip = $('[data-live-chip]');
    var liveCmd = $('[data-live-command]');
    var runs = {}; // session id -> {el, out, decoders, machine, command, state}
    var lastHistory = [];

    /* -- target list -- */
    function checkedIds() { return $$('input[type="checkbox"]:checked', targets).map(function (c) { return c.value; }); }
    function updateLabel() {
      var n = checkedIds().length;
      $('[data-run-label]').textContent = 'Run on ' + n + ' worker agent' + (n === 1 ? '' : 's');
      $('[data-run-submit]').disabled = n === 0;
    }
    function refreshTargets() {
      var keep = checkedIds();
      fragment('command-targets').then(function (html) {
        targets.innerHTML = html;
        $$('input[type="checkbox"]', targets).forEach(function (c) { if (!c.disabled && keep.indexOf(c.value) !== -1) c.checked = true; });
        updateLabel();
      }).catch(function () {});
    }
    var refreshTargetsSoon = debounce(refreshTargets, 200);
    targets.addEventListener('change', updateLabel);
    $$('[data-select]', form).forEach(function (b) {
      b.addEventListener('click', function () {
        var all = b.getAttribute('data-select') === 'online';
        $$('input[type="checkbox"]', targets).forEach(function (c) { c.checked = all && !c.disabled; });
        updateLabel();
      });
    });
    updateLabel();

    /* -- run cards -- */
    function stepState(card, state, bad) {
      var order = ['queued', 'running', 'exited'];
      var at = order.indexOf(state);
      $$('.step', card).forEach(function (s, i) {
        s.className = 'step' + (i < at ? ' step--done' : i === at ? (state === 'exited' ? (bad ? ' step--bad' : ' step--done') : ' step--now') : '');
      });
    }
    function exitChip(x) {
      var code = x.exit_code;
      switch (x.reason) {
        case 'exited': return code === 0 ? chip('ok', 'Exited 0', 'check') : chip('danger', 'Exited ' + code, 'x');
        case 'timeout': return chip('warn', 'Timed out', 'clock');
        case 'terminated': return chip('warn', 'Terminated', 'stop');
        case 'refused': return chip('danger', 'Refused (no confirmation)', 'x');
        case 'cancelled': return chip('outline', 'Cancelled');
        case 'lost': return chip('danger', 'Connection lost', 'x');
        default: return chip('danger', 'Error', 'x');
      }
    }
    function setCard(r, c) {
      // c is a fleet.Command (history shape) or an event shape with the same fields
      var head = $('[data-run-chip]', r.el);
      head.textContent = '';
      var meta = $('[data-run-meta]', r.el);
      var cancel = $('[data-run-cancel]', r.el);
      if (c.state === 'queued') {
        head.appendChild(chip('warn', 'Queued' + (c.position ? ' #' + c.position : ''), 'clock'));
        stepState(r.el, 'queued');
        meta.textContent = c.position ? 'position ' + c.position + ' in the queue' : 'waiting for the machine';
        cancel.textContent = 'Cancel'; cancel.className = 'btn btn--sm'; cancel.hidden = false;
      } else if (c.state === 'running') {
        head.appendChild(chip('accent', 'Running', null, true));
        stepState(r.el, 'running');
        meta.textContent = 'started ' + fmtTime(c.started_at);
        cancel.textContent = 'Terminate'; cancel.className = 'btn btn--sm btn--danger'; cancel.hidden = false;
      } else {
        var x = c.exit || { reason: 'error', exit_code: -1 };
        head.appendChild(exitChip(x));
        stepState(r.el, 'exited', !(x.reason === 'exited' && x.exit_code === 0));
        meta.textContent = fmtDur(x.duration_ms) + (x.error ? ' · ' + x.error : '');
        cancel.hidden = true;
      }
      r.state = c.state;
    }
    function ensureCard(session, machine, command) {
      if (runs[session]) return runs[session];
      var node = tpl.content.firstElementChild.cloneNode(true);
      $('[data-run-machine]', node).textContent = machine;
      node.setAttribute('data-session', session);
      $('[data-run-out]', node).setAttribute('aria-label', 'Output of ' + machine);
      var r = { el: node, out: $('.term-ph__body', node), machine: machine, command: command, session: session, dec: {}, state: '' };
      $('[data-run-cancel]', node).addEventListener('click', function () {
        api('POST', '/api/commands/' + encodeURIComponent(session) + '/cancel', {}).then(function (res) {
          if (!res.ok) toast(res.data.error || 'Could not cancel.');
        });
      });
      runs[session] = r;
      runsBox.insertBefore(node, runsBox.firstChild);
      runsEmpty.hidden = true;
      return r;
    }
    function appendOutput(r, stream, bytes) {
      var dec = r.dec[stream] || (r.dec[stream] = new TextDecoder('utf-8'));
      var text = dec.decode(bytes, { stream: true });
      if (!text) return;
      var body = r.out;
      var near = body.scrollTop + body.clientHeight >= body.scrollHeight - 24;
      var span = stream === 'stderr' ? el('span', 'out-err') : document.createElement('span');
      span.textContent = text;
      body.appendChild(span);
      while (body.childNodes.length > 400) body.removeChild(body.firstChild); // bound the DOM
      if (near) body.scrollTop = body.scrollHeight;
    }
    function loadOutput(r) {
      api('GET', '/api/commands/' + encodeURIComponent(r.session)).then(function (res) {
        if (!res.ok) return;
        r.out.textContent = '';
        (res.data.output || []).forEach(function (seg) {
          var span = seg.stream === 'stderr' ? el('span', 'out-err') : el('span', seg.stream === 'note' ? 'term-dim' : null);
          span.textContent = seg.text;
          r.out.appendChild(span);
        });
      });
    }
    function updateLive(list) {
      var active = list.filter(function (c) { return c.state !== 'finished'; });
      var running = active.filter(function (c) { return c.state === 'running'; }).length;
      liveChip.className = 'chip chip--' + (active.length ? 'accent chip--live' : 'outline');
      liveChip.textContent = active.length ? running + ' running, ' + (active.length - running) + ' queued' : 'Idle';
      var newest = list[0];
      liveCmd.textContent = newest ? newest.command : 'Nothing running';
    }

    /* -- history -- */
    function renderHistory() {
      var text = histFilter.value.trim().toLowerCase();
      histBody.textContent = '';
      var shown = lastHistory.filter(function (c) {
        return !text || (c.command + ' ' + c.machine + ' ' + c.session).toLowerCase().indexOf(text) !== -1;
      });
      if (!shown.length) {
        var tr = el('tr'); var td = el('td', 'empty', lastHistory.length ? 'No commands match the filter.' : 'No commands yet.');
        td.colSpan = 7; tr.appendChild(td); histBody.appendChild(tr);
        return;
      }
      shown.forEach(function (c) {
        var tr = el('tr');
        tr.appendChild(el('td', 'mono', c.session));
        tr.appendChild(el('td', 'mono truncate', c.command));
        tr.appendChild(el('td', 'nowrap mono', c.machine));
        tr.appendChild(el('td', 'nowrap', fmtTime(c.queued_at)));
        var dur = el('td', 'num', c.state === 'finished' && c.exit ? fmtDur(c.exit.duration_ms) : c.state);
        tr.appendChild(dur);
        var res = el('td');
        if (c.state === 'queued') res.appendChild(chip('warn', 'Queued' + (c.position ? ' #' + c.position : ''), 'clock'));
        else if (c.state === 'running') res.appendChild(chip('accent', 'Running', null, true));
        else res.appendChild(exitChip(c.exit || { reason: 'error', exit_code: -1 }));
        tr.appendChild(res);
        var act = el('td', 'actions');
        var rerun = el('button', 'btn btn--sm'); rerun.type = 'button';
        rerun.appendChild(icon('refresh', true)); rerun.appendChild(document.createTextNode('Re-run'));
        rerun.addEventListener('click', function () {
          $('#cmd').value = c.command; $('#cmd').focus();
          var box = $('input[value="' + c.machine + '"]', targets);
          if (box && !box.disabled) { $$('input[type="checkbox"]', targets).forEach(function (x) { x.checked = false; }); box.checked = true; updateLabel(); }
        });
        act.appendChild(rerun);
        tr.appendChild(act);
        histBody.appendChild(tr);
      });
    }
    function refreshHistory() {
      api('GET', '/api/commands?mode=oneshot').then(function (res) {
        if (!res.ok) { histBody.textContent = ''; var tr = el('tr'); var td = el('td', 'empty', res.data.error || 'History is unavailable.'); td.colSpan = 7; tr.appendChild(td); histBody.appendChild(tr); return; }
        lastHistory = res.data.commands || [];
        renderHistory();
        updateLive(lastHistory);
        // Keep cards in step (queue positions, state) and restore cards for sessions started elsewhere.
        lastHistory.slice().reverse().forEach(function (c) {
          var known = runs[c.session];
          if (!known && c.state === 'finished') return;
          var r = known || ensureCard(c.session, c.machine, c.command);
          if (!known) loadOutput(r);
          setCard(r, c);
        });
      });
    }
    var refreshHistorySoon = debounce(refreshHistory, 120);
    histFilter.addEventListener('input', renderHistory);

    /* -- live events -- */
    onEvent(function (m) {
      if (m.type === 'machine') { refreshTargetsSoon(); return; }
      if (m.type === 'chunk') {
        var r = runs[m.session];
        if (!r) return;
        appendOutput(r, m.stream, b64bytes(m.data || ''));
        return;
      }
      if (m.type === 'command' && m.mode === 'oneshot') {
        if (m.event === 'queued' && !runs[m.session]) loadOutput(ensureCard(m.session, m.on, m.command));
        if (runs[m.session] && m.event === 'running') setCard(runs[m.session], { state: 'running', started_at: m.started_at });
        if (runs[m.session] && m.event === 'exited') setCard(runs[m.session], { state: 'finished', exit: m.exit });
        refreshHistorySoon();
        refreshTargetsSoon();
      }
    });
    onReconnect(function () { refreshHistory(); refreshTargets(); });
    refreshHistory();

    /* -- submit, with the confirmation gate -- */
    var dlg = $('#dlg-gate');
    var phrase = $('#gate-phrase');
    var confirmBtn = $('[data-gate-confirm]');
    var pending = null;
    phrase.addEventListener('input', function () { confirmBtn.disabled = phrase.value.trim() !== 'I understand'; });

    function submit(confirm) {
      var cmd = $('#cmd').value;
      var body = {
        machines: checkedIds(), command: cmd,
        timeout_sec: parseInt($('#to').value, 10) || 0, confirm: !!confirm
      };
      var btn = $('[data-run-submit]');
      btn.disabled = true;
      return api('POST', '/api/commands', body).then(function (r) {
        updateLabel();
        if (r.status === 409 && r.data.needs_confirmation) {
          pending = body;
          $('[data-gate-pattern]').textContent = r.data.pattern;
          $('[data-gate-command]').textContent = r.data.command + '  (on ' + r.data.machines.join(', ') + ')';
          phrase.value = ''; confirmBtn.disabled = true;
          dlg.showModal(); phrase.focus();
          setStatus(form, 'info', '');
          return;
        }
        if (!r.ok && !r.data.results) { setStatus(form, 'danger', r.data.error || ('Failed (' + r.status + ')')); return; }
        var errors = [];
        (r.data.results || []).forEach(function (res) {
          if (res.error) { errors.push(res.machine + ': ' + res.error); return; }
          var card = ensureCard(res.session, res.machine, cmd);
          setCard(card, { state: 'queued' });
        });
        setStatus(form, errors.length ? 'danger' : 'info', errors.join(' | '));
        refreshHistorySoon();
      });
    }
    form.addEventListener('submit', function (e) { e.preventDefault(); setStatus(form, 'info', ''); submit(false); });
    $('#gate-form').addEventListener('submit', function (e) {
      e.preventDefault();
      if (phrase.value.trim() !== 'I understand' || !pending) return;
      dlg.close();
      var body = pending; pending = null;
      // Re-submit the same command with the operator's confirmation; the main server mints the token.
      $('#cmd').value = body.command;
      submit(true);
    });
  }

  /* ======================================================================
     Terminal
     ====================================================================== */
  function initTerminal() {
    var tabs = $('#term-tabs');
    var panels = $('#term-panels');
    var emptyMsg = $('#term-empty');
    var tpl = $('#tpl-term');
    var form = $('#shell-form');
    var sessionsBox = $('#sessions');
    var count = $('[data-session-count]');
    var sessions = {}; // key -> {id, machine, tab, panel, ws, term, fit, state}
    var seq = 0;

    var xterm = window.Terminal;
    var Fit = window.FitAddon && window.FitAddon.FitAddon;
    if (!xterm) { setStatus(form, 'danger', 'xterm.js did not load; terminals are unavailable.'); }

    function themeFromCss() {
      var cs = getComputedStyle(document.documentElement);
      function v(n, d) { return (cs.getPropertyValue(n) || '').trim() || d; }
      return { background: v('--term-bg', '#0a0f1a'), foreground: v('--term-fg', '#d6deeb'), cursor: v('--term-fg', '#d6deeb') };
    }
    var enc = new TextEncoder();

    function select(s, focus) {
      Object.keys(sessions).forEach(function (k) {
        var o = sessions[k], on = o === s;
        o.tab.setAttribute('aria-selected', String(on));
        o.tab.tabIndex = on ? 0 : -1;
        o.panel.hidden = !on;
      });
      if (s && s.fit) { try { s.fit.fit(); } catch (e) { /* hidden panel */ } }
      if (s && focus) s.term.focus();
    }
    function setChip(s, kind) {
      var host = $('[data-term-chip]', s.panel);
      host.textContent = '';
      if (kind === 'queued') host.appendChild(chip('warn', 'Waiting for the machine', 'clock'));
      else if (kind === 'running') host.appendChild(chip('ok', 'Connected', 'check'));
      else if (kind === 'ended') host.appendChild(chip('outline', 'Ended'));
      else host.appendChild(chip('info', 'Opening...', 'info'));
      s.state = kind;
      var stop = $('[data-term-stop]', s.panel);
      stop.lastChild.textContent = kind === 'ended' ? 'Close tab' : 'Terminate session';
    }
    function removeSession(s) {
      try { if (s.ws) s.ws.close(); } catch (e) { /* closed */ }
      if (s.term) s.term.dispose();
      s.tab.remove(); s.panel.remove();
      delete sessions[s.key];
      var keys = Object.keys(sessions);
      emptyMsg.hidden = keys.length > 0;
      if (keys.length) select(sessions[keys[keys.length - 1]], true);
      refreshSessions();
    }

    /* The in-terminal confirmation for a dangerous line the agent refused */
    var dlg = $('#dlg-tgate');
    var phrase = $('#tgate-phrase');
    var confirmBtn = $('[data-tgate-confirm]');
    var pendingConfirm = null;
    phrase.addEventListener('input', function () { confirmBtn.disabled = phrase.value.trim() !== 'I understand'; });
    $('#tgate-form').addEventListener('submit', function (e) {
      e.preventDefault();
      if (phrase.value.trim() !== 'I understand' || !pendingConfirm) return;
      var p = pendingConfirm; pendingConfirm = null;
      dlg.close();
      if (p.s.ws && p.s.ws.readyState === 1) p.s.ws.send(JSON.stringify({ type: 'confirm', line: p.line }));
      p.s.term.focus();
    });
    dlg.addEventListener('close', function () { if (pendingConfirm) { pendingConfirm.s.term.focus(); pendingConfirm = null; } });

    function openShell(machine) {
      if (!xterm) return;
      setStatus(form, 'info', '');
      var key = 'k' + (++seq);
      var panel = tpl.content.firstElementChild.cloneNode(true);
      var tab = el('button', 'tab'); tab.type = 'button'; tab.setAttribute('role', 'tab'); tab.id = 'tab-' + key;
      panel.id = 'pnl-' + key; panel.setAttribute('aria-labelledby', tab.id);
      tab.setAttribute('aria-controls', panel.id);
      tab.appendChild(icon('terminal', true));
      tab.appendChild(document.createTextNode(machine + ' '));
      var sid = el('span', 'faint mono', '...'); tab.appendChild(sid);
      var s = { key: key, machine: machine, tab: tab, panel: panel, state: '' };
      sessions[key] = s;
      tabs.appendChild(tab); panels.appendChild(panel);
      emptyMsg.hidden = true;

      s.term = new xterm({ cursorBlink: true, fontFamily: getComputedStyle(document.documentElement).getPropertyValue('--font-mono') || 'monospace', fontSize: 13, theme: themeFromCss(), scrollback: 5000 });
      s.fit = Fit ? new Fit() : null;
      if (s.fit) s.term.loadAddon(s.fit);
      select(s, false);
      s.term.open($('[data-term-mount]', panel));
      if (s.fit) s.fit.fit();
      $('[data-term-title]', panel).textContent = machine + ' · opening';
      $('[data-term-size]', panel).textContent = s.term.cols + ' x ' + s.term.rows;
      setChip(s, 'opening');

      tab.addEventListener('click', function () { select(s, true); });
      tab.addEventListener('keydown', function (e) {
        var list = Object.keys(sessions).map(function (k) { return sessions[k]; });
        var i = list.indexOf(s), n = null;
        if (e.key === 'ArrowRight') n = list[(i + 1) % list.length];
        if (e.key === 'ArrowLeft') n = list[(i + list.length - 1) % list.length];
        if (n) { e.preventDefault(); select(n, true); n.tab.focus(); }
      });
      $('[data-term-copy]', panel).addEventListener('click', function () {
        var t = s.term.getSelection();
        if (!t) { toast('Select some terminal text first.'); return; }
        (navigator.clipboard ? navigator.clipboard.writeText(t) : Promise.reject()).then(function () { toast('Copied.'); }, function () { toast('Copy is blocked by the browser here; use Ctrl-Shift-C.'); });
      });
      $('[data-term-stop]', panel).addEventListener('click', function () {
        if (s.state === 'ended') { removeSession(s); return; }
        try { s.ws.close(); } catch (e) { /* already closed */ }
      });

      var proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
      var url = proto + location.host + '/ws/shell?machine=' + encodeURIComponent(machine) + '&cols=' + s.term.cols + '&rows=' + s.term.rows;
      var ws = new WebSocket(url);
      ws.binaryType = 'arraybuffer';
      s.ws = ws;
      ws.onmessage = function (e) {
        if (typeof e.data !== 'string') { s.term.write(new Uint8Array(e.data)); return; }
        var m; try { m = JSON.parse(e.data); } catch (err) { return; }
        switch (m.type) {
          case 'session':
            s.id = m.session; sid.textContent = m.session;
            $('[data-term-title]', panel).textContent = machine + ' · session ' + m.session;
            refreshSessions();
            break;
          case 'state':
            setChip(s, m.state);
            if (m.state === 'queued') s.term.write('\x1b[2m[fleet] waiting: a command is running on this machine; the shell starts when it ends.\x1b[0m\r\n');
            if (m.state === 'running') {
              if (s.fit) s.fit.fit();
              ws.send(JSON.stringify({ type: 'resize', cols: s.term.cols, rows: s.term.rows }));
              s.term.focus();
            }
            refreshSessions();
            break;
          case 'refused':
            if (m.line) {
              pendingConfirm = { s: s, line: m.line };
              $('[data-tgate-line]').textContent = m.line;
              $('[data-tgate-machine]').textContent = machine;
              phrase.value = ''; confirmBtn.disabled = true;
              dlg.showModal(); phrase.focus();
            } else {
              toast('The agent refused a line (' + (m.reason || 'dangerous') + '). Retype it to try again.');
            }
            break;
          case 'exit':
            s.term.write('\r\n\x1b[2m[fleet] session ended (' + (m.reason || 'exited') + (m.code != null ? ', code ' + m.code : '') + ')\x1b[0m\r\n');
            break;
          case 'error':
            s.term.write('\r\n\x1b[31m[fleet] ' + (m.error || 'error') + '\x1b[0m\r\n');
            setStatus(form, 'danger', m.error || 'The shell could not be opened.');
            break;
        }
      };
      ws.onclose = function () {
        if (s.state !== 'ended') { setChip(s, 'ended'); s.term.write('\r\n\x1b[2m[fleet] disconnected\x1b[0m\r\n'); }
        refreshSessions();
      };
      s.term.onData(function (d) { if (ws.readyState === 1 && s.state === 'running') ws.send(enc.encode(d)); });
      s.term.onResize(function (sz) {
        $('[data-term-size]', panel).textContent = sz.cols + ' x ' + sz.rows;
        if (ws.readyState === 1 && s.state === 'running') ws.send(JSON.stringify({ type: 'resize', cols: sz.cols, rows: sz.rows }));
      });
    }
    window.addEventListener('resize', debounce(function () {
      Object.keys(sessions).forEach(function (k) { var s = sessions[k]; if (!s.panel.hidden && s.fit) { try { s.fit.fit(); } catch (e) { /* ignore */ } } });
    }, 150));

    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var m = form.elements.machine.value;
      if (!m) { setStatus(form, 'danger', 'Pick an online worker agent.'); return; }
      openShell(m);
    });

    /* -- sessions list (shells of every operator tab) -- */
    function refreshSessions() {
      api('GET', '/api/commands?mode=pty&active=1').then(function (res) {
        var list = res.ok ? (res.data.commands || []) : [];
        sessionsBox.textContent = '';
        count.textContent = list.length + ' open';
        if (!list.length) {
          var li = el('li', 'list__item'); var main = el('div', 'list__main'); main.appendChild(el('small', null, 'No shells.'));
          li.appendChild(main); sessionsBox.appendChild(li); return;
        }
        list.forEach(function (c) {
          var li = el('li', 'list__item');
          var main = el('div', 'list__main');
          var b = el('b'); b.appendChild(document.createTextNode(c.machine + ' ')); b.appendChild(el('span', 'mono faint', c.session));
          main.appendChild(b);
          main.appendChild(el('small', null, (c.state === 'queued' ? 'waiting' : 'open since ' + fmtTime(c.started_at)) + ' · ' + c.user));
          li.appendChild(main);
          var stop = el('button', 'btn btn--sm btn--danger', 'Terminate'); stop.type = 'button';
          stop.setAttribute('aria-label', 'Terminate session ' + c.session + ' on ' + c.machine);
          stop.addEventListener('click', function () {
            api('POST', '/api/commands/' + encodeURIComponent(c.session) + '/cancel', {}).then(function (r) { if (!r.ok) toast(r.data.error || 'Could not terminate.'); refreshSessions(); });
          });
          li.appendChild(stop);
          sessionsBox.appendChild(li);
        });
      });
    }
    var refreshSessionsSoon = debounce(refreshSessions, 150);

    /* -- host list follows machine state -- */
    function refreshHosts() {
      var sel = form.elements.machine, keep = sel.value;
      fragment('shell-hosts').then(function (html) {
        sel.innerHTML = html;
        if (keep) { var o = $('option[value="' + keep + '"]', sel); if (o && !o.disabled) sel.value = keep; }
      }).catch(function () {});
    }
    var refreshHostsSoon = debounce(refreshHosts, 200);
    onEvent(function (m) {
      if (m.type === 'machine') refreshHostsSoon();
      if (m.type === 'command' && m.mode === 'pty') refreshSessionsSoon();
    });
    onReconnect(function () { refreshHosts(); refreshSessions(); });
    refreshSessions();

    /* Arriving from the Machines page: terminal.html?machine=ID opens a shell straight away. */
    var wanted = new URLSearchParams(location.search).get('machine');
    if (wanted) {
      var opt = $('option[value="' + wanted + '"]', form.elements.machine);
      if (opt && !opt.disabled) openShell(wanted);
      history.replaceState(null, '', location.pathname);
    }
  }

  // live.js is loaded with defer: the DOM is parsed, so start right away.
  if (page === 'machines') initMachines();
  if (page === 'commands') initCommands();
  if (page === 'terminal') initTerminal();
})();
