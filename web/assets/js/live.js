/* Fleet panel - live wiring for the Operate pages (Machines, Command runner, Terminal) and the Observe/Admin
   pages (Evidence, Backup, Monitor data, Watchdog).
   Loaded after app.js. app.js keeps the shared behaviour (theme, drawer, menus, dialogs, tabs, stub toasts);
   this file adds fetch + WebSocket where a page is backed by the main server:
     GET/POST /api/...        JSON API (see internal/panel/api.go)
     GET      /fragments/...  server-rendered HTML pieces, swapped in when data changes
     WS       /ws/events      machine state and command lifecycle/output
     WS       /ws/shell       one interactive shell (xterm.js <-> agent PTY)
     WS       /ws/monitor     live probe results (Monitor data)
     WS       /ws/watchdog    watchdog events (Watchdog)
   Anything not backed yet keeps its data-stub marker. Plain ES5 + fetch, no framework, no build. */
(function () {
  'use strict';

  var page = document.body.getAttribute('data-page');
  var OPERATE = ['machines', 'commands', 'terminal'];
  if (OPERATE.concat(['evidence', 'backup', 'monitor', 'watchdog']).indexOf(page) === -1) return;

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
  if (OPERATE.indexOf(page) !== -1) connectEvents(); // the Observe/Admin pages open their own streams

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
     Token enrollment (T-0026): "Add agent (token)" and the enrolled agents.
     The server is /api/enroll/*; all server text goes in through textContent.
     ====================================================================== */
  function initEnroll() {
    var section = $('#enroll');
    var form = $('#enroll-form');
    var result = $('[data-enroll-result]');
    var tableBox = $('#enrolled');
    var rows = $('#enrolled-rows');
    if (!section || !form || !tableBox) return;
    var roleSel = $('#e-role');
    var scriptLink = $('[data-enroll-script]');
    var countdown = null;

    function setScriptLink() { scriptLink.href = '/api/enroll/script?role=' + encodeURIComponent(roleSel.value); }
    setScriptLink();
    roleSel.addEventListener('change', setScriptLink);

    function tick(expires) {
      if (countdown) window.clearInterval(countdown);
      var box = $('[data-enroll-expiry]');
      function upd() {
        var left = Math.round((new Date(expires) - Date.now()) / 1000);
        if (left <= 0) { box.textContent = 'This code has expired. Generate a new one.'; window.clearInterval(countdown); return; }
        box.textContent = 'Expires in ' + Math.floor(left / 60) + ':' + ('0' + (left % 60)).slice(-2);
      }
      upd();
      countdown = window.setInterval(upd, 1000);
    }

    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var btn = $('button[type="submit"]', form);
      btn.disabled = true;
      setStatus(form, 'info', 'Generating...');
      api('POST', '/api/enroll/tokens', { role: roleSel.value }).then(function (r) {
        btn.disabled = false;
        if (!r.ok) { result.hidden = true; setStatus(form, 'danger', r.data.error || ('Could not generate a token (' + r.status + ')')); return; }
        setStatus(form, 'ok', 'Token ready. It works once, for one machine, within 10 minutes.');
        $('[data-enroll-code]').textContent = r.data.code;
        $('[data-enroll-cmd="linux"]').textContent = r.data.linux;
        $('[data-enroll-cmd="macos"]').textContent = r.data.macos || '';
        $('[data-enroll-os="macos"]').hidden = !r.data.macos;
        result.hidden = false;
        tick(r.data.expires_at);
      });
    });
    $$('[data-enroll-copy]').forEach(function (b) {
      b.addEventListener('click', function () {
        var text = $('[data-enroll-cmd="' + b.getAttribute('data-enroll-copy') + '"]').textContent;
        (navigator.clipboard ? navigator.clipboard.writeText(text) : Promise.reject()).then(
          function () { toast('Copied.'); },
          function () { toast('Copy is blocked by the browser here; select the command and copy it.'); });
      });
    });

    /* The enrolled agents */
    function specText(sp) {
      if (!sp) return '';
      var parts = [];
      if (sp.os) parts.push(sp.os + (sp.arch ? '/' + sp.arch : ''));
      if (sp.os_version) parts.push(sp.os_version);
      if (sp.cpus) parts.push(sp.cpus + ' CPU');
      if (sp.mem_bytes) parts.push(fmtBytes(sp.mem_bytes) + ' RAM');
      if (sp.hostname) parts.push(sp.hostname);
      return parts.join(', ');
    }
    function renderRows(list) {
      rows.textContent = '';
      list.forEach(function (m) {
        var tr = el('tr');
        tr.setAttribute('data-id', m.id);
        var th = el('th', null, m.id); th.scope = 'row'; tr.appendChild(th);
        tr.appendChild(el('td', null, m.role === 'monitor' ? 'Monitor' : 'Worker'));
        var st = el('td');
        if (m.stopped) st.appendChild(chip('warn', 'Stopped', 'pause'));
        else if (m.online) st.appendChild(chip('ok', 'Online', 'check', true));
        else st.appendChild(chip('outline', 'Offline', 'clock'));
        tr.appendChild(st);
        tr.appendChild(el('td', 'muted', specText(m.specs)));
        var act = el('td');
        var stop = el('button', 'btn btn--sm', 'Stop');
        stop.type = 'button';
        stop.setAttribute('data-enroll-act', 'stop'); stop.setAttribute('data-machine', m.id);
        var det = el('button', 'btn btn--sm btn--danger', 'Detach');
        det.type = 'button';
        det.setAttribute('data-enroll-act', 'detach'); det.setAttribute('data-machine', m.id);
        act.appendChild(stop); act.appendChild(document.createTextNode(' ')); act.appendChild(det);
        tr.appendChild(act);
        rows.appendChild(tr);
      });
      $('[data-enrolled-empty]').hidden = list.length !== 0;
      $('[data-enrolled-count]').textContent = list.length + (list.length === 1 ? ' agent' : ' agents');
    }
    var busy = false;
    function refresh() {
      if (busy) return;
      api('GET', '/api/enroll/machines').then(function (r) {
        if (!r.ok) {
          // Enrollment is off on this server (no enroll_addr): say so instead of offering a dead button.
          $$('[data-toggle-panel="#enroll"]').forEach(function (b) { b.disabled = true; b.title = 'Token enrollment is not enabled on this server (set server.enroll_addr).'; });
          tableBox.hidden = true;
          return;
        }
        tableBox.hidden = false;
        renderRows(r.data.machines || []);
      });
    }
    var refreshSoon = debounce(refresh, 300);
    onEvent(function (m) { if (m.type === 'machine') refreshSoon(); });
    onReconnect(refresh);
    setInterval(refresh, 15000);
    refresh();

    /* Stop / Detach, each behind a confirmation */
    var dlg = $('#dlg-enroll-act');
    var aform = $('#enroll-act-form');
    var pending = null;
    document.addEventListener('click', function (e) {
      var b = e.target.closest('[data-enroll-act]');
      if (!b) return;
      pending = { kind: b.getAttribute('data-enroll-act'), machine: b.getAttribute('data-machine') };
      var stop = pending.kind === 'stop';
      $('#dlg-enroll-act-title').textContent = (stop ? 'Stop ' : 'Detach ') + pending.machine;
      $('[data-enroll-act-intro]').textContent = stop
        ? 'This disables the agent service on the machine: it stays down through a reboot. Someone must run "sudo fleet-agent start" on the machine to bring it back. The machine stays in the registry.'
        : 'This revokes the machine\'s certificate and takes it out of the registry. The agent can no longer connect. To add the machine again you need a fresh token.';
      var go = $('[data-enroll-act-submit]');
      go.textContent = stop ? 'Stop agent' : 'Detach machine';
      go.className = 'btn ' + (stop ? 'btn--primary' : 'btn--danger');
      setStatus(aform, 'info', '');
      dlg.showModal();
    });
    aform.addEventListener('submit', function (e) {
      e.preventDefault();
      var go = $('[data-enroll-act-submit]');
      go.disabled = true; busy = true;
      setStatus(aform, 'info', 'Working...');
      api('POST', '/api/enroll/machines/' + encodeURIComponent(pending.machine) + '/' + pending.kind, {}).then(function (r) {
        go.disabled = false; busy = false;
        if (!r.ok) { setStatus(aform, 'danger', r.data.error || ('Failed (' + r.status + ')')); return; }
        dlg.close();
        toast(pending.kind === 'stop' ? pending.machine + ' was stopped.' : pending.machine + ' was detached and its certificate revoked.');
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

  /* ======================================================================
     Shared helpers for the Observe/Admin pages
     ====================================================================== */
  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }
  function fmtBytes(n) {
    if (n == null) return '';
    if (n < 1000) return n + ' B';
    var u = ['kB', 'MB', 'GB', 'TB', 'PB'], i = -1;
    do { n /= 1000; i++; } while (n >= 1000 && i < u.length - 1);
    return (n >= 100 ? n.toFixed(0) : n.toFixed(1)) + ' ' + u[i];
  }
  function fmtDateTime(iso) {
    if (!iso) return '';
    var d = new Date(iso);
    return isNaN(d) || d.getFullYear() < 2000 ? '' : d.toLocaleString([], { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' });
  }
  function fmtAgo(iso) {
    if (!iso) return 'never';
    var d = new Date(iso);
    if (isNaN(d) || d.getFullYear() < 2000) return 'never';
    var s = Math.max(0, (Date.now() - d.getTime()) / 1000);
    if (s < 2) return 'just now';
    if (s < 60) return Math.floor(s) + ' s ago';
    if (s < 3600) return Math.floor(s / 60) + ' min ago';
    if (s < 172800) return Math.floor(s / 3600) + ' h ago';
    return Math.floor(s / 86400) + ' d ago';
  }
  function td(content, cls) {
    var c = el('td', cls || null);
    if (content != null && content.nodeType) c.appendChild(content);
    else if (content != null) c.textContent = content;
    return c;
  }
  function th(text, cls) { var c = el('th', cls || null, text); c.setAttribute('scope', 'row'); return c; }
  function emptyRow(tbody, cols, text) {
    clear(tbody);
    var tr = el('tr'), c = el('td', 'faint', text);
    c.colSpan = cols;
    tr.appendChild(c);
    tbody.appendChild(tr);
  }
  function spinnerPoll(fn, ms) {
    var t = null;
    return {
      start: function () { if (!t) t = setInterval(fn, ms); },
      stop: function () { if (t) { clearInterval(t); t = null; } }
    };
  }

  /* ======================================================================
     Evidence (internal/evidence): sessions, pull + sync, export
     ====================================================================== */
  function initEvidence() {
    var status = $('#ev-status');
    var tbody = $('#ev-sessions');
    var q = $('#e-q'), selM = $('#e-m'), selK = $('#e-k');
    var selected = null; // "machine/session"
    var sessions = [];
    var KIND = { oneshot: 'One-shot', pty: 'Shell' };

    function say(kind, msg) { setStatus(status.parentNode, kind, msg); }

    function loadSessions() {
      var qs = new URLSearchParams();
      if (q.value.trim()) qs.set('q', q.value.trim());
      if (selM.value) qs.set('machine', selM.value);
      if (selK.value) qs.set('kind', selK.value);
      return api('GET', '/api/evidence/sessions?' + qs.toString()).then(function (r) {
        if (!r.ok) { emptyRow(tbody, 8, r.data.error || 'The archive could not be read.'); return; }
        sessions = r.data.sessions || [];
        renderSessions();
        fillExportSessions();
      });
    }

    function renderSessions() {
      $('#ev-count').textContent = sessions.length + (sessions.length === 1 ? ' session' : ' sessions');
      if (!sessions.length) { emptyRow(tbody, 8, 'No evidence in the archive yet. Pull from a worker agent on the Sync tab.'); return; }
      clear(tbody);
      sessions.forEach(function (s) {
        var key = s.machine + '/' + s.session;
        var tr = el('tr');
        tr.tabIndex = 0;
        tr.style.cursor = 'pointer';
        if (key === selected) tr.setAttribute('aria-selected', 'true');
        tr.appendChild(th(s.session, 'mono'));
        tr.appendChild(td(s.machine));
        tr.appendChild(td(KIND[s.kind] || (s.kind ? s.kind : 'Unknown'), 'nowrap'));
        tr.appendChild(td(s.command || '', 'mono truncate'));
        tr.appendChild(td(fmtDateTime(s.started), 'nowrap'));
        tr.appendChild(td((s.shared ? '≤ ' : '') + s.packets.toLocaleString(), 'num'));
        tr.appendChild(td(String(s.dropped), 'num'));
        var st = s.online ? chip('ok', s.segments + (s.segments === 1 ? ' segment' : ' segments'), 'check') : chip('danger', 'Agent offline', 'x');
        tr.appendChild(td(st));
        function open() { selected = key; renderSessions(); showSession(s); }
        tr.addEventListener('click', open);
        tr.addEventListener('keydown', function (e) { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); } });
        tbody.appendChild(tr);
      });
    }

    function showSession(s) {
      var box = $('#ev-detail');
      box.hidden = false;
      $('#ev-detail-id').textContent = s.session;
      $('#ev-detail-sub').textContent = 'On ' + s.machine + '. Each row is one outbound packet.';
      $('#ev-detail-cmd').textContent = s.command || '(not in the command history)';
      $('#ev-detail-src').textContent = '';
      $('#ev-detail-dropped').textContent = s.dropped + ' dropped';
      var rows = $('#ev-records');
      emptyRow(rows, 8, 'Loading records...');
      api('GET', '/api/evidence/sessions/' + encodeURIComponent(s.machine) + '/' + encodeURIComponent(s.session) + '/records').then(function (r) {
        if (!r.ok) { emptyRow(rows, 8, r.data.error || 'Records could not be read.'); return; }
        var recs = r.data.records || [];
        if (!recs.length) { emptyRow(rows, 8, 'This session has no packet records.'); return; }
        clear(rows);
        var srcs = {};
        recs.forEach(function (p) {
          var tr = el('tr');
          tr.appendChild(td(fmtTime(p.time) + '.' + ('000' + new Date(p.time).getMilliseconds()).slice(-3), 'mono nowrap'));
          tr.appendChild(td(p.protocol));
          tr.appendChild(td(p.src, 'mono'));
          tr.appendChild(td(p.dst, 'mono'));
          tr.appendChild(td(p.flags || '', 'mono'));
          tr.appendChild(td(p.protocol === 'TCP' ? p.seq + ' / ' + p.ack : '-', 'num mono'));
          tr.appendChild(td(chip(p.payload_status === 'trimmed' ? 'warn' : 'outline', p.payload_status + ' · ' + p.payload_bytes + ' B')));
          var h = p.payload_sha256 || '';
          tr.appendChild(td(h ? h.slice(0, 8) + '...' + h.slice(-4) : '', 'hash'));
          rows.appendChild(tr);
          if (p.src_ip_used) srcs[p.src_ip_used] = true;
        });
        $('#ev-detail-src').textContent = Object.keys(srcs).join(', ') || 'not recorded';
        $('#ev-detail-more').textContent = r.data.more ? 'Showing the first ' + recs.length + ' records; export the session for all of them. ' : '';
      });
    }

    [q, selM, selK].forEach(function (c) { c.addEventListener(c === q ? 'input' : 'change', debounce(loadSessions, 250)); });

    /* ---- sync tab ---- */
    var syncRows = $('#ev-sync-rows');
    var poll = spinnerPoll(loadSync, 2000);

    function syncChip(row) {
      var s = row.sync;
      if (s.state === 'pulling') return chip('accent', 'Pulling', null, true);
      if (s.state === 'failed') { var c = chip('danger', 'Failed', 'x'); c.title = s.error || ''; return c; }
      if (s.state === 'done') {
        if (s.rejected) return chip('danger', s.rejected + ' rejected (bad SHA-256)', 'x');
        return chip('ok', s.fetched ? 'Synced, ' + s.fetched + ' fetched' : 'Up to date', 'check');
      }
      if (row.state !== 'online') return chip('danger', 'Agent offline', 'x');
      return chip('outline', 'Not pulled yet');
    }

    function loadSync() {
      return api('GET', '/api/evidence/sync').then(function (r) {
        if (!r.ok) { emptyRow(syncRows, 6, r.data.error || 'Sync status is unavailable.'); poll.stop(); return; }
        var workers = r.data.workers || [];
        var busy = false;
        if (!workers.length) emptyRow(syncRows, 6, 'No worker agents are registered.');
        else {
          clear(syncRows);
          workers.forEach(function (w) {
            var s = w.sync;
            if (s.state === 'pulling') busy = true;
            var tr = el('tr');
            tr.appendChild(th(w.machine, 'cell-primary'));
            tr.appendChild(td(w.archived_segments + ' segments, ' + fmtBytes(w.archived_bytes), 'mono'));
            tr.appendChild(td(s.state === 'done' || s.state === 'failed'
              ? s.segments + ' on agent, ' + s.fetched + ' fetched' + (s.resumed ? ' (' + s.resumed + ' resumed)' : '') + ', ' + s.skipped + ' already archived, ' + fmtBytes(s.bytes)
              : (s.state === 'pulling' ? 'in progress' : '-'), s.state === 'idle' ? 'faint' : ''));
            var cell = td(syncChip(w));
            if (s.error && s.state === 'failed') cell.appendChild(el('div', 'cell-sub', s.error));
            tr.appendChild(cell);
            tr.appendChild(td(s.finished ? fmtAgo(s.finished) + (s.trigger === 'push' ? ' (agent push)' : '') : '-', 'nowrap'));
            var btn = el('button', 'btn btn--sm' + (w.state === 'online' ? ' btn--primary' : ''));
            btn.type = 'button';
            btn.appendChild(icon('download', true));
            btn.appendChild(document.createTextNode('Pull'));
            if (w.state !== 'online' || s.state === 'pulling') { btn.setAttribute('aria-disabled', 'true'); btn.disabled = true; }
            btn.addEventListener('click', function () { pull(w.machine); });
            tr.appendChild(td(btn, 'actions'));
            syncRows.appendChild(tr);
          });
        }
        var d = r.data.disk || {};
        var disk = $('#ev-disk');
        var meter = $('.meter', disk);
        $('.meter__fill', meter).style.width = Math.min(100, d.used_percent || 0) + '%';
        meter.className = 'meter' + (d.state === 'paused' ? ' meter--danger' : d.state === 'warn' ? ' meter--warn' : '');
        var spans = $$('.meter__text span', meter);
        spans[0].textContent = 'Archive disk ' + (d.used_percent || 0).toFixed(0) + '% used' + (d.state === 'paused' ? ' - intake paused' : d.state === 'warn' ? ' - warning' : '');
        spans[1].textContent = (d.used_percent || 0).toFixed(0) + '%';
        if (busy) poll.start(); else poll.stop();
        if (busy) loadSessions();
      });
    }

    function pull(machine) {
      var body = machine ? { machine: machine } : {};
      api('POST', '/api/evidence/pull', body).then(function (r) {
        var res = (r.data.results || []);
        if (!r.ok) {
          say('danger', (r.data.error || (res[0] && res[0].error)) || ('Pull failed (' + r.status + ')'));
        } else {
          var failed = res.filter(function (x) { return !x.started; });
          say(failed.length ? 'info' : 'ok', res.length ? 'Pull started on ' + (res.length - failed.length) + ' worker agent(s).' + (failed.length ? ' Not started: ' + failed.map(function (x) { return x.machine + ' (' + x.error + ')'; }).join('; ') : '') : 'No worker agent is online to pull from.');
        }
        loadSync();
        poll.start();
      });
    }
    $('#ev-pull-all').addEventListener('click', function () { pull(''); });

    /* manifest */
    var manSel = $('#ev-man-m');
    function loadManifest() {
      var body = $('#ev-manifest');
      if (!manSel.value) { emptyRow(body, 6, 'Choose a worker agent.'); return; }
      api('GET', '/api/evidence/manifest/' + encodeURIComponent(manSel.value)).then(function (r) {
        var segs = r.data.segments || [];
        if (!r.ok || !segs.length) { emptyRow(body, 6, r.ok ? 'No segments archived for this machine yet.' : (r.data.error || 'Unavailable.')); return; }
        clear(body);
        segs.slice(-30).reverse().forEach(function (g) {
          var tr = el('tr');
          tr.appendChild(td(g.file, 'mono'));
          tr.appendChild(td(fmtTime(g.start) + '-' + fmtTime(g.end), 'nowrap'));
          tr.appendChild(td(fmtBytes(g.bytes), 'num'));
          tr.appendChild(td(g.records.toLocaleString(), 'num'));
          tr.appendChild(td(g.sha256 ? g.sha256.slice(0, 6) + '...' + g.sha256.slice(-4) : '', 'hash'));
          tr.appendChild(td(chip('ok', 'Verified', 'check')));
          body.appendChild(tr);
        });
      });
    }
    manSel.addEventListener('change', loadManifest);
    $('#etab-y').addEventListener('click', function () { loadSync(); loadManifest(); });

    /* ---- export ---- */
    var form = $('#ev-export-form');
    function scopeSync() {
      var scope = form.elements.scope.value;
      $$('[data-scope]', form).forEach(function (n) { n.hidden = n.getAttribute('data-scope') !== scope && !(scope === 'range' && n.getAttribute('data-scope') === 'machine'); });
    }
    $$('input[name="scope"]', form).forEach(function (r) { r.addEventListener('change', scopeSync); });
    scopeSync();
    function fillExportSessions() {
      var sel = $('#x-sess');
      var cur = sel.value;
      clear(sel);
      if (!sessions.length) { var o = el('option', null, 'No sessions in the archive yet'); o.value = ''; sel.appendChild(o); return; }
      sessions.forEach(function (s) {
        var o = el('option', null, s.session + ' · ' + s.machine + (s.command ? ' · ' + s.command : ''));
        o.value = s.machine + '/' + s.session;
        sel.appendChild(o);
      });
      if (cur) sel.value = cur;
    }
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var scope = form.elements.scope.value, fmt = form.elements.fmt.value;
      var qs = new URLSearchParams();
      qs.set('format', fmt);
      var label = 'archive';
      if (scope === 'session') {
        var v = $('#x-sess').value;
        if (!v) { say('danger', 'There is no session to export yet.'); return; }
        var i = v.indexOf('/');
        qs.set('machine', v.slice(0, i)); qs.set('session', v.slice(i + 1));
        label = v.slice(i + 1);
      } else {
        var m = $('#x-mach').value;
        if (m) { qs.set('machine', m); label = m; }
        if (scope === 'range') {
          var f = $('#x-from').value, t = $('#x-to').value;
          if (!f && !t) { say('danger', 'Pick a start or an end time for a time-range export.'); return; }
          if (f) qs.set('from', new Date(f).toISOString());
          if (t) qs.set('to', new Date(t).toISOString());
          label += ' ' + (f || '') + ' to ' + (t || 'now');
        }
      }
      var url = '/api/evidence/export?' + qs.toString();
      var list = $('#ev-exports');
      if (!list.getAttribute('data-has')) { clear(list); list.setAttribute('data-has', '1'); }
      var li = el('li', 'list__item'), main = el('div', 'list__main');
      main.appendChild(el('b', 'mono', label + '.' + fmt));
      main.appendChild(el('small', null, 'requested ' + fmtDateTime(new Date().toISOString())));
      li.appendChild(main);
      var a = el('a', 'btn btn--sm');
      a.href = url; a.setAttribute('download', '');
      a.appendChild(icon('download', true)); a.appendChild(document.createTextNode('Download again'));
      li.appendChild(a);
      list.insertBefore(li, list.firstChild);
      say('ok', 'Export started: your browser will download the ' + fmt + ' file.');
      window.location.href = url;
    });

    loadSessions();
    loadSync();
  }

  /* ======================================================================
     Backup (internal/backup): server, diff, back up now, import, schedule, reminder
     ====================================================================== */
  function initBackup() {
    var statusBox = $('#bk-status');
    function say(kind, msg) { setStatus(statusBox.parentNode, kind, msg); }
    var poll = spinnerPoll(load, 2000);
    var lastJobState = null;
    var haveServer = false;

    function renderServer(d) {
      haveServer = !!d.configured;
      var dl = $('#bk-server');
      $('#bk-noserver').hidden = haveServer;
      dl.hidden = !haveServer;
      $('#bk-noserver').textContent = 'No backup server is configured yet. Add one on the right: the archive exists in one place only until you do.';
      $('#bk-run').disabled = !haveServer;
      $('#bk-diff-btn').disabled = !haveServer;
      if (!haveServer) return;
      var s = d.server;
      $('[data-f="address"]', dl).textContent = s.host + (s.port && s.port !== 22 ? ':' + s.port : '');
      $('[data-f="user"]', dl).textContent = s.user;
      $('[data-f="path"]', dl).textContent = s.remote_path + '  (relative to the SSH login directory)';
      $('[data-f="auth"]', dl).textContent = s.auth === 'key' ? 'SSH key (the password was discarded)' : 'Password kept in a 0600 file (a key could not be installed)';
      $('[data-f="fp"]', dl).textContent = s.host_key_fp || 'not recorded';
      var last = d.last_backup;
      $('[data-f="last"]', dl).textContent = last ? fmtDateTime(last.finished) + (last.ok ? ', completed' : ', with ' + last.failed + ' failed file(s)') : 'never';
    }

    function renderRuns(d) {
      var tb = $('#bk-runs');
      var rows = [];
      if (d.job) {
        var j = d.job;
        rows.push({ started: j.started, what: j.kind === 'import' ? 'Import (panel)' : 'Back up now (panel)', files: j.uploaded, bytes: j.bytes,
          state: j.state, note: j.error || j.summary, conflicts: j.conflicts });
      }
      if (d.last_backup) {
        var l = d.last_backup;
        rows.push({ started: l.started, what: 'Last backup run', files: l.uploaded, bytes: l.bytes, state: l.ok ? 'done' : 'failed', note: l.ok ? '' : l.failed + ' file(s) failed' });
      }
      if (!rows.length) { emptyRow(tb, 5, 'No backup has run yet.'); return; }
      clear(tb);
      rows.forEach(function (r) {
        var tr = el('tr');
        tr.appendChild(td(fmtDateTime(r.started), 'nowrap'));
        tr.appendChild(td(r.what));
        tr.appendChild(td(String(r.files), 'num'));
        tr.appendChild(td(fmtBytes(r.bytes), 'num'));
        var c = r.state === 'running' ? chip('accent', 'Running', null, true) : r.state === 'done' ? chip('ok', 'Completed', 'check') : chip('danger', 'Failed', 'x');
        var cell = td(c);
        if (r.note) cell.appendChild(el('div', 'cell-sub', r.note));
        if (r.conflicts && r.conflicts.length) cell.appendChild(el('div', 'cell-sub', 'Kept (differ locally): ' + r.conflicts.slice(0, 5).join(', ') + (r.conflicts.length > 5 ? ' and ' + (r.conflicts.length - 5) + ' more' : '')));
        tr.appendChild(cell);
        tb.appendChild(tr);
      });
    }

    function renderSchedule(d) {
      var sc = d.schedule, form = $('#bk-sched-form');
      var sw = $('input[name="enabled"]', form);
      sw.checked = !!sc.enabled;
      sw.parentNode.lastElementChild.textContent = sc.enabled ? 'On' : 'Off';
      // Nothing saved yet (a zero schedule, off): offer the suggested time instead of 00:00.
      var shown = !sc.enabled && !sc.hour && !sc.minute ? d.suggested : sc;
      if (!form.getAttribute('data-touched')) form.elements.time.value = ('0' + shown.hour).slice(-2) + ':' + ('0' + shown.minute).slice(-2);
      $('#bk-sched-next').textContent = sc.enabled && sc.next ? 'Next run: ' + fmtDateTime(sc.next) : (sc.enabled ? '' : 'The schedule is off: nothing runs unless you use Back up now.');
      var sg = d.suggested;
      var box = $('#bk-suggest');
      clear(box);
      box.appendChild(el('strong', null, 'Suggested: ' + sg.text.replace('daily at', 'every night at') + '. '));
      box.appendChild(document.createTextNode(sg.reason || ''));
    }

    function renderReminder(d) {
      var box = $('#bk-reminder');
      box.hidden = !d.reminder;
      if (d.reminder) $('#bk-reminder-text').textContent = d.reminder.message;
    }

    function load() {
      return api('GET', '/api/backup').then(function (r) {
        if (!r.ok) { $('#bk-noserver').hidden = false; $('#bk-noserver').textContent = r.data.error || 'The backup service is unavailable.'; $('#bk-run').disabled = true; poll.stop(); return; }
        var d = r.data;
        renderServer(d); renderRuns(d); renderSchedule(d); renderReminder(d);
        var running = d.job && d.job.state === 'running';
        $('#bk-run').disabled = running || !haveServer;
        if (running) poll.start(); else poll.stop();
        if (lastJobState === 'running' && d.job && d.job.state !== 'running') {
          say(d.job.state === 'done' ? 'ok' : 'danger', (d.job.kind === 'import' ? 'Import ' : 'Backup ') + (d.job.state === 'done' ? 'finished. ' + (d.job.summary || '') : 'failed: ' + (d.job.error || '')));
          loadDiff();
        }
        lastJobState = d.job ? d.job.state : null;
      });
    }

    function renderDiff(d) {
      var tb = $('#bk-diff-rows');
      clear(tb);
      (d.groups || []).forEach(function (g) {
        var pend = g.missing + g.changed;
        var tr = el('tr');
        tr.appendChild(th(g.label, 'cell-primary'));
        tr.appendChild(td(g.missing ? String(g.missing) : 'none', 'num' + (g.missing ? '' : ' faint')));
        tr.appendChild(td(g.changed ? String(g.changed) : 'none', 'num' + (g.changed ? '' : ' faint')));
        tr.appendChild(td(pend ? fmtBytes(g.bytes) : 'none', 'num' + (pend ? '' : ' faint')));
        tr.appendChild(td(g.backup_only ? String(g.backup_only) : 'none', 'num' + (g.backup_only ? '' : ' faint')));
        tr.appendChild(td(pend ? chip('warn', 'Behind', 'clock') : chip('ok', 'In sync', 'check')));
        tb.appendChild(tr);
      });
      var foot = $('#bk-diff-foot');
      foot.textContent = 'Computed ' + fmtDateTime(d.computed) + ': ' + d.present + ' file(s) identical on both, ' + fmtBytes(d.pending_bytes) + ' to send.' +
        (d.scan_errors ? ' ' + d.scan_errors + ' file(s) on this server could not be read.' : '') +
        ' CA keys are part of the backup set: without them a restore would force every machine to be re-enrolled. Key material is never shown in this panel.';
      var reach = $('#bk-reach');
      clear(reach);
      reach.appendChild(chip('ok', 'Reachable', 'check'));
    }

    function loadDiff() {
      if (!haveServer) return Promise.resolve();
      var tb = $('#bk-diff-rows');
      emptyRow(tb, 6, 'Comparing with the backup server...');
      return api('GET', '/api/backup/diff').then(function (r) {
        if (!r.ok) {
          emptyRow(tb, 6, r.data.error || 'The backup server could not be reached.');
          var reach = $('#bk-reach');
          clear(reach); reach.appendChild(chip('danger', 'Not reachable', 'x'));
          return;
        }
        renderDiff(r.data);
      });
    }
    $('#bk-diff-btn').addEventListener('click', loadDiff);

    $('#bk-run').addEventListener('click', function () {
      $('#bk-run').disabled = true;
      api('POST', '/api/backup/run', {}).then(function (r) {
        if (!r.ok) { say('danger', r.data.error || 'The backup could not be started.'); $('#bk-run').disabled = !haveServer; return; }
        say('info', 'Backup started. Files are copied additively and each one is verified on the backup server.');
        lastJobState = 'running';
        load();
        poll.start();
      });
    });

    $('#bk-reminder-dismiss').addEventListener('click', function () {
      api('POST', '/api/backup/reminder/dismiss', {}).then(function () { $('#bk-reminder').hidden = true; });
    });

    var sched = $('#bk-sched-form');
    sched.addEventListener('input', function () { sched.setAttribute('data-touched', '1'); });
    sched.addEventListener('submit', function (e) {
      e.preventDefault();
      var parts = (sched.elements.time.value || '02:30').split(':');
      api('POST', '/api/backup/schedule', { enabled: sched.elements.enabled.checked, hour: parseInt(parts[0], 10), minute: parseInt(parts[1], 10) }).then(function (r) {
        if (!r.ok) { say('danger', r.data.error || 'The schedule was not saved.'); return; }
        sched.removeAttribute('data-touched');
        say('ok', r.data.schedule.enabled ? 'Schedule saved: ' + r.data.schedule.text + '.' : 'Schedule saved (off).');
        load();
      });
    });

    var add = $('#bk-add-form');
    add.addEventListener('submit', function (e) {
      e.preventDefault();
      var f = add.elements;
      var btn = $('button[type="submit"]', add);
      btn.disabled = true;
      say('info', 'Connecting to ' + f.host.value.trim() + ' and installing a key. This can take a moment.');
      api('POST', '/api/backup/server', { host: f.host.value.trim(), port: parseInt(f.port.value, 10) || 0, user: f.user.value.trim(), password: f.password.value }).then(function (r) {
        btn.disabled = false;
        f.password.value = '';
        if (!r.ok) { say('danger', r.data.error || ('The server could not be added (' + r.status + ')')); return; }
        say('ok', 'Backup server added. ' + (r.data.diff ? (r.data.diff.in_sync ? 'It is in sync.' : fmtBytes(r.data.diff.pending_bytes) + ' is not on it yet.') : 'The diff could not be computed: ' + (r.data.diff_error || '')));
        add.reset();
        load().then(function () { if (r.data.diff) renderDiff(r.data.diff); });
      });
    });

    var imp = $('#bk-import-form');
    imp.addEventListener('submit', function (e) {
      e.preventDefault();
      if (!imp.elements.confirm.checked) { toast('Tick the box to confirm the import first.'); return; }
      api('POST', '/api/backup/import', { confirm: true, overwrite: imp.elements.overwrite.checked }).then(function (r) {
        if (!r.ok) { say('danger', r.data.error || 'The import could not be started.'); return; }
        $('#dlg-import').close();
        imp.reset();
        say('info', 'Import started.');
        lastJobState = 'running';
        load();
        poll.start();
      });
    });

    load().then(loadDiff);
    setInterval(function () { if (!document.hidden) load(); }, 30000);
  }

  function wsURL(path) { return (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + path; }
  /* A WebSocket that reconnects by itself; onmsg gets each parsed frame, onopen runs after every (re)connect. */
  function liveSocket(path, onmsg, onopen) {
    var attempt = 0, closed = false;
    function open() {
      var ws = new WebSocket(wsURL(path));
      ws.onopen = function () { var again = attempt > 0; attempt = 0; if (onopen) onopen(again); };
      ws.onmessage = function (e) { var m; try { m = JSON.parse(e.data); } catch (err) { return; } onmsg(m); };
      ws.onclose = function () { if (closed) return; attempt++; setTimeout(open, Math.min(10000, 500 * attempt)); };
    }
    open();
    return { close: function () { closed = true; } };
  }
  function median(a) {
    if (!a.length) return null;
    var s = a.slice().sort(function (x, y) { return x - y; }), m = Math.floor(s.length / 2);
    return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2;
  }
  function pct(a, p) {
    if (!a.length) return null;
    var s = a.slice().sort(function (x, y) { return x - y; });
    return s[Math.min(s.length - 1, Math.floor(p * s.length))];
  }
  function fmtMs(v) { return v == null ? '-' : (v < 10 ? v.toFixed(1) : Math.round(v)) + ' ms'; }

  /* ======================================================================
     Monitor data (internal/monitor): live charts + probe-config editor
     ====================================================================== */
  function initMonitor() {
    var NS = 'http://www.w3.org/2000/svg';
    var DASH = ['', '6 3', '2 3', '8 3 2 3', '1 4'];
    var state = { series: {}, agents: [], jobs: [], targets: {}, commands: [], win: 60, view: 'merged', key: '', paused: false };
    var svg = $('#mon-svg');
    function say(kind, msg) { setStatus($('#mon-status').parentNode, kind, msg); }

    function keyOf(p) { return p.agent + '\u0000' + p.target; }

    function applySnapshot(d) {
      state.series = {};
      (d.series || []).forEach(function (sd) {
        state.series[keyOf(sd)] = { agent: sd.agent, target: sd.target, history_key: sd.history_key, type: sd.type,
          points: sd.points.map(function (p) { return { t: new Date(p.t).getTime(), ms: p.ms, ok: p.ok }; }) };
      });
      state.agents = d.agents || [];
      state.jobs = d.jobs || [];
      state.targets = d.targets || {};
      state.commands = (d.commands || []).map(function (c) {
        return { session: c.session, machine: c.machine, command: c.command, user: c.user, mode: c.mode,
          start: new Date(c.start).getTime(), end: c.end ? new Date(c.end).getTime() : null, packets: c.packets };
      });
      fillTargets();
    }

    function fillTargets() {
      var sel = $('#m-target'), cur = sel.value;
      clear(sel);
      var all = el('option', null, 'All targets'); all.value = ''; sel.appendChild(all);
      state.jobs.forEach(function (j) { var o = el('option', null, j.label); o.value = j.key; sel.appendChild(o); });
      sel.value = cur;
      state.key = sel.value;
    }

    function load() {
      return api('GET', '/api/monitor/live?window=' + state.win).then(function (r) {
        if (!r.ok) { $('#mon-empty').hidden = false; $('#mon-empty span').textContent = r.data.error || 'The monitor data is unavailable.'; return; }
        applySnapshot(r.data);
        render();
      });
    }

    /* New results from /ws/monitor are appended to the lines they belong to. */
    liveSocket('/ws/monitor', function (m) {
      if (m.type !== 'samples') return;
      (m.points || []).forEach(function (p) {
        var k = keyOf(p), s = state.series[k];
        if (!s) s = state.series[k] = { agent: p.agent, target: p.target, history_key: p.history_key, type: p.type, points: [] };
        if (p.history_key) s.history_key = p.history_key;
        s.points.push({ t: new Date(p.t).getTime(), ms: p.ms, ok: p.ok });
      });
      renderSoon();
    }, function (again) { if (again) load(); });

    function windowPoints() {
      var now = Date.now(), from = now - state.win * 1000, out = [];
      Object.keys(state.series).forEach(function (k) {
        var s = state.series[k];
        if (state.key && s.history_key !== state.key) return;
        s.points = s.points.filter(function (p) { return p.t >= now - 600000; });
        var pts = s.points.filter(function (p) { return p.t >= from && p.t <= now + 2000; });
        if (pts.length) out.push({ s: s, pts: pts });
      });
      return out;
    }

    function groupOf(view, s) {
      if (view === 'machine') return s.agent;
      if (view === 'target') return s.target + (state.targets[s.target] ? ' (' + state.targets[s.target] + ')' : '');
      return 'All monitors';
    }

    function render() {
      if (state.paused) return;
      var now = Date.now(), win = state.win * 1000, from = now - win;
      var sel = windowPoints();
      var all = [];
      sel.forEach(function (x) { all = all.concat(x.pts); });

      /* stats */
      var oks = all.filter(function (p) { return p.ok; }), mss = oks.map(function (p) { return p.ms; });
      $('#mon-avail').textContent = all.length ? (oks.length * 100 / all.length).toFixed(1) + ' %' : '-';
      $('#mon-avail-note').textContent = all.length ? (all.length - oks.length) + ' failed probe(s) of ' + all.length : 'No results yet';
      $('#mon-median').textContent = fmtMs(median(mss));
      $('#mon-p95').textContent = mss.length ? '95th percentile ' + fmtMs(pct(mss, 0.95)) : ' ';
      var lastT = all.reduce(function (a, p) { return Math.max(a, p.t); }, 0);
      $('#mon-last').textContent = lastT ? Math.max(0, (now - lastT) / 1000).toFixed(1) + ' s ago' : '-';
      var types = {};
      sel.forEach(function (x) { types[x.s.type] = true; });
      $('#mon-types').textContent = String(Object.keys(types).length || '-');
      $('#mon-types-note').textContent = Object.keys(types).map(function (t) { return t.toUpperCase(); }).join(', ') || ' ';

      /* lines */
      var groups = {}, order = [];
      sel.forEach(function (x) {
        var g = groupOf(state.view, x.s);
        if (!groups[g]) { groups[g] = []; order.push(g); }
        groups[g] = groups[g].concat(x.pts);
      });
      order.sort();
      var NB = 40, binMs = win / NB, lines = [], maxMs = 10;
      order.forEach(function (g) {
        var bins = [];
        groups[g].forEach(function (p) {
          if (!p.ok) return;
          var b = Math.min(NB - 1, Math.max(0, Math.floor((p.t - from) / binMs)));
          (bins[b] = bins[b] || []).push(p.ms);
        });
        var pts = [];
        bins.forEach(function (arr, b) { if (arr) { var v = median(arr); maxMs = Math.max(maxMs, v); pts.push([(b + 0.5) * 200 / NB, v]); } });
        lines.push({ name: g, pts: pts, fails: groups[g].filter(function (p) { return !p.ok; }) });
      });
      maxMs *= 1.15;
      while (svg.firstChild) svg.removeChild(svg.firstChild);
      lines.forEach(function (ln, i) {
        if (ln.pts.length) {
          var pl = document.createElementNS(NS, 'polyline');
          pl.setAttribute('fill', 'none');
          pl.setAttribute('stroke', 'var(--series-' + ((i % 5) + 1) + ')');
          pl.setAttribute('stroke-width', '1.6');
          if (DASH[i % 5]) pl.setAttribute('stroke-dasharray', DASH[i % 5]);
          pl.setAttribute('vector-effect', 'non-scaling-stroke');
          pl.setAttribute('points', ln.pts.map(function (p) { return p[0].toFixed(1) + ',' + (57 - p[1] / maxMs * 52).toFixed(1); }).join(' '));
          svg.appendChild(pl);
        }
        ln.fails.slice(-200).forEach(function (p) {
          var x = ((p.t - from) / win * 200).toFixed(1);
          var l = document.createElementNS(NS, 'line');
          l.setAttribute('x1', x); l.setAttribute('x2', x); l.setAttribute('y1', '53'); l.setAttribute('y2', '59.5');
          l.setAttribute('stroke', 'var(--danger)'); l.setAttribute('stroke-width', '1.4'); l.setAttribute('vector-effect', 'non-scaling-stroke');
          svg.appendChild(l);
        });
      });
      $('#mon-empty').hidden = all.length > 0;
      $('#mon-empty span').textContent = 'Waiting for results from the monitor agents';
      $('#mon-caption').textContent = 'Response time, ' + (state.view === 'merged' ? 'all monitors merged' : state.view === 'machine' ? 'per monitor machine' : 'per target') + ' (max ' + fmtMs(maxMs / 1.15) + ')';
      $('#mon-sub').textContent = 'Live window ' + (state.win >= 120 ? state.win / 60 + ' min' : state.win + ' s') + ' · median per ' + (binMs / 1000).toFixed(1) + ' s';

      /* legend */
      var legend = $('#mon-legend');
      clear(legend);
      lines.forEach(function (ln, i) {
        var it = el('span', 'legend__item'), sw = el('span', 'legend__swatch' + (i % 5 === 1 ? ' legend__swatch--dash' : i % 5 === 2 ? ' legend__swatch--dot' : ''));
        sw.style.borderTopColor = 'var(--series-' + ((i % 5) + 1) + ')';
        it.appendChild(sw); it.appendChild(document.createTextNode(ln.name));
        legend.appendChild(it);
      });

      /* axis */
      var ax = $('#mon-axis');
      clear(ax);
      [0, 25, 50, 75, 100].forEach(function (p) {
        var s = el('span', null, p === 100 ? 'now' : '-' + Math.round(state.win * (100 - p) / 100) + ' s');
        s.style.left = p + '%';
        ax.appendChild(s);
      });

      /* hover columns: commands that ran in each interval, and the median there */
      var cmds = state.commands.filter(function (c) { return (c.end || now) >= from && c.start <= now; });
      var cols = $('#mon-cols');
      clear(cols);
      var NC = 12, colMs = win / NC;
      for (var i = 0; i < NC; i++) {
        var a = from + i * colMs, b = a + colMs;
        var inCol = cmds.filter(function (c) { return c.start < b && (c.end || now) > a; });
        var vals = all.filter(function (p) { return p.ok && p.t >= a && p.t < b; }).map(function (p) { return p.ms; });
        var tip = [Math.round((a - now) / 1000) + ' s to ' + (i === NC - 1 ? 'now' : Math.round((b - now) / 1000) + ' s'),
          inCol.length ? 'Commands: ' + inCol.map(function (c) { return c.command.slice(0, 40) + ' (' + c.machine + ')'; }).join(', ').slice(0, 160) : 'No commands in this interval',
          vals.length ? 'Median ' + fmtMs(median(vals)) : 'No results in this interval'];
        var col = el('span', 'plot__col');
        col.setAttribute('data-tip', tip.join('|'));
        cols.appendChild(col);
      }

      /* command overlay */
      var ovl = $('#mon-ovl');
      $$('.ovl__mark', ovl).forEach(function (n) { n.remove(); });
      cmds.forEach(function (c) {
        var a = Math.max(c.start, from), b = Math.min(c.end || now, now);
        var left = (a - from) / win * 100, width = Math.max(0.8, (b - a) / win * 100);
        var dur = Math.max(0, Math.round(((c.end || now) - c.start) / 1000));
        var pk = c.packets == null ? 'packets: evidence not pulled yet' : c.packets.toLocaleString() + ' packets archived for the session';
        var tip = (c.command.length > 60 ? c.command.slice(0, 60) + '...' : c.command) + '|' + c.machine + ' · ' + dur + ' s' + (c.end ? '' : ' (running)') + '|' + pk;
        var m = el('button', 'ovl__mark');
        m.type = 'button';
        m.style.left = left.toFixed(2) + '%'; m.style.width = Math.min(width, 100 - left).toFixed(2) + '%';
        m.setAttribute('data-tip', tip);
        m.setAttribute('aria-label', tip.replace(/\|/g, ', '));
        ovl.appendChild(m);
      });

      /* data table */
      var tb = $('#mon-table'), rows = {};
      state.agents.forEach(function (a) { rows[a.id] = { state: a.state, last: null, types: {}, n: 0, ok: 0 }; });
      sel.forEach(function (x) {
        var r = rows[x.s.agent] || (rows[x.s.agent] = { state: 'unknown', last: null, types: {}, n: 0, ok: 0 });
        x.pts.forEach(function (p) {
          r.n++; if (p.ok) r.ok++;
          if (!r.last || p.t > r.last.t) { r.last = p; }
        });
        r.types[x.s.type] = true;
      });
      var ids = Object.keys(rows).sort();
      if (!ids.length) emptyRow(tb, 6, 'No monitor agent is registered.');
      else {
        clear(tb);
        ids.forEach(function (id) {
          var r = rows[id], tr = el('tr');
          tr.appendChild(th(id));
          tr.appendChild(td(chip(r.state === 'online' ? 'ok' : 'danger', r.state, r.state === 'online' ? 'check' : 'x')));
          tr.appendChild(td(Object.keys(r.types).map(function (t) { return t.toUpperCase(); }).join(', ') || '-'));
          tr.appendChild(td(r.last && r.last.ok ? fmtMs(r.last.ms) : (r.last ? 'failed' : '-'), 'num'));
          tr.appendChild(td(r.n ? (r.ok * 100 / r.n).toFixed(1) + '%' : '-', 'num'));
          tr.appendChild(td(r.last ? Math.max(0, (now - r.last.t) / 1000).toFixed(1) + ' s ago' : 'no results'));
          tb.appendChild(tr);
        });
      }
    }
    var renderSoon = debounce(render, 250);

    $$('input[name="view"]').forEach(function (r) { r.addEventListener('change', function () { state.view = r.value; render(); }); });
    $('#m-target').addEventListener('change', function () { state.key = this.value; render(); });
    $('#m-win').addEventListener('change', function () { state.win = parseInt(this.value, 10) || 60; load(); });
    var pauseBtn = $('#m-pause');
    pauseBtn.addEventListener('click', function () {
      state.paused = !state.paused;
      $('span', pauseBtn).textContent = state.paused ? 'Resume' : 'Pause';
      if (!state.paused) load();
    });
    load();
    setInterval(function () { if (!state.paused && !document.hidden) load(); }, 5000);
    setInterval(function () { if (!state.paused && !document.hidden) render(); }, 1000);

    /* ---- probe config ---- */
    var cfg = null;
    function loadConfig() {
      return api('GET', '/api/monitor/config').then(function (r) {
        if (!r.ok) { emptyRow($('#mon-jobs'), 7, r.data.error || 'Unavailable.'); return; }
        cfg = r.data;
        var f = $('#mon-defaults-form').elements, st = cfg.settings || {};
        f.interval_ms.value = st.interval_ms;
        ['http', 'tcp', 'icmp', 'dns'].forEach(function (t) { f[t].value = (st.timeout_ms || {})[t] || ''; });
        $('#d-freq-h').textContent = 'From ' + cfg.bounds.min_interval_ms + ' ms to ' + (cfg.bounds.max_interval_ms / 1000) + ' s.';
        renderJobs();
        $('#mon-agent-load').textContent = (cfg.agents || []).length
          ? 'Each monitor agent is assigned 2-3 targets. Load now: ' + cfg.agents.map(function (a) { return a.id + ' ' + a.targets + '/' + a.max + (a.state === 'online' ? '' : ' (' + a.state + ')'); }).join(', ') + '.'
          : 'No monitor agent is registered: onboard one on the Machines page before creating a job.';
      });
    }
    function renderJobs() {
      var tb = $('#mon-jobs');
      var jobs = cfg.jobs || [];
      if (!jobs.length) { emptyRow(tb, 7, 'No monitor job yet. Create one below.'); return; }
      clear(tb);
      jobs.forEach(function (j) {
        var tr = el('tr');
        tr.appendChild(th(j.origin || '(IP only)', 'cell-primary mono'));
        tr.appendChild(td(j.ip, 'mono'));
        tr.appendChild(td((j.paths || []).join(', ') || 'none', 'mono' + ((j.paths || []).length ? '' : ' faint')));
        var types = {}, ivs = {}, agents = {};
        (j.targets || []).forEach(function (t) { types[t.target.type] = true; if (t.target.interval_ms) ivs[t.target.interval_ms] = true; agents[t.agent] = true; });
        var pc = el('td');
        Object.keys(types).forEach(function (t) { pc.appendChild(chip('outline', t.toUpperCase())); pc.appendChild(document.createTextNode(' ')); });
        tr.appendChild(pc);
        var iv = Object.keys(ivs);
        tr.appendChild(td(iv.length ? iv.join(', ') + ' ms (override)' : (cfg.settings.interval_ms + ' ms (default)')));
        tr.appendChild(td(Object.keys(agents).sort().join(', ')));
        var act = el('td', 'actions');
        var eb = el('button', 'icon-btn icon-btn--sm'); eb.type = 'button'; eb.setAttribute('aria-label', 'Set frequency of monitor job ' + (j.origin || j.ip)); eb.title = 'Set frequency override';
        eb.appendChild(icon('edit'));
        eb.addEventListener('click', function () {
          var v = window.prompt('Frequency override in ms for every target of this job (300 to 60000; empty or 0 clears it):', iv[0] || '');
          if (v === null) return;
          var ms = parseInt(v, 10) || 0;
          Promise.all((j.targets || []).map(function (t) { return api('POST', '/api/monitor/jobs/' + encodeURIComponent(j.id) + '/interval', { target: t.target.id, interval_ms: ms }); })).then(function (rs) {
            var bad = rs.filter(function (r) { return !r.ok; })[0];
            if (bad) say('danger', bad.data.error || 'The frequency was not changed.'); else say('ok', 'Frequency updated and pushed.');
            loadConfig();
          });
        });
        var db = el('button', 'icon-btn icon-btn--sm'); db.type = 'button'; db.setAttribute('aria-label', 'Delete monitor job ' + (j.origin || j.ip)); db.title = 'Delete job';
        db.appendChild(icon('trash'));
        db.addEventListener('click', function () {
          if (!window.confirm('Delete this monitor job? Its history is kept; the agents stop probing its targets.')) return;
          api('POST', '/api/monitor/jobs/' + encodeURIComponent(j.id) + '/delete', {}).then(function (r) {
            say(r.ok ? 'ok' : 'danger', r.ok ? 'Job deleted; the agents were sent their new configuration.' : (r.data.error || 'The job was not deleted.'));
            loadConfig(); load();
          });
        });
        act.appendChild(eb); act.appendChild(db);
        tr.appendChild(act);
        tb.appendChild(tr);
      });
    }
    $('#mtab-cfg').addEventListener('click', loadConfig);

    $('#mon-defaults-form').addEventListener('submit', function (e) {
      e.preventDefault();
      var f = this.elements, to = {};
      ['http', 'tcp', 'icmp', 'dns'].forEach(function (t) { var v = parseInt(f[t].value, 10); if (v) to[t] = v; });
      api('POST', '/api/monitor/settings', { interval_ms: parseInt(f.interval_ms.value, 10) || 0, timeout_ms: to }).then(function (r) {
        say(r.ok ? 'ok' : 'danger', r.ok ? 'Defaults saved and pushed to the connected monitor agents.' : (r.data.error || 'Not saved.'));
        if (r.ok) loadConfig();
      });
    });
    $('#mon-job-form').addEventListener('submit', function (e) {
      e.preventDefault();
      var f = this.elements, form = this;
      var paths = f.paths.value.split(/[\s,]+/).filter(Boolean);
      api('POST', '/api/monitor/jobs', { url: f.url.value.trim(), ip: f.ip.value.trim(), paths: paths, interval_ms: parseInt(f.interval_ms.value, 10) || 0 }).then(function (r) {
        if (!r.ok) { say('danger', r.data.error || 'The job was not created.'); return; }
        say('ok', 'Monitor job ' + r.data.job.id + ' created with ' + r.data.job.targets.length + ' target(s); the agents were sent their configuration.');
        form.reset();
        loadConfig(); load();
      });
    });
  }

  /* ======================================================================
     Watchdog (T-0012/T-0022): hosts, policy editor, events
     ====================================================================== */
  function initWatchdog() {
    function say(kind, msg) { setStatus($('#wd-status').parentNode, kind, msg); }
    var COND = {
      'normal': ['ok', 'Normal', 'check'], 'breaching': ['warn', 'Breaching', 'alert'], 'throttled': ['warn', 'Throttled', 'alert'],
      'alert-only': ['danger', 'Alert-only', 'alert'], 'not-reporting': ['neutral', 'Not reporting', 'x']
    };
    var KIND = {
      'breach': ['warn', 'Breach'], 'recovered': ['ok', 'Recovered'], 'action': ['warn', 'Action'], 'action-failed': ['danger', 'Action failed'],
      'suppressed': ['info', 'Held back'], 'alert-only': ['danger', 'Alert-only'], 'unthrottled': ['ok', 'Throttle lifted'],
      'day-rollover': ['info', 'Day rollover'], 'policy': ['info', 'Policy'], 'counters-reset': ['info', 'Counter reset']
    };
    var STEP = { throttle: 'Throttle', kill_session: 'Kill session', restart: 'Service restart', reboot: 'Reboot', unthrottle: 'Unthrottle' };

    function ladder(level, alertOnly) {
      var l = el('span', 'ladder');
      l.setAttribute('aria-hidden', 'true');
      for (var i = 1; i <= 4; i++) l.appendChild(el('i', i <= level ? (alertOnly || i >= 3 ? 'on hi' : 'on') : ''));
      return l;
    }

    function loadHosts() {
      return api('GET', '/api/watchdog').then(function (r) {
        var tb = $('#wd-hosts');
        if (!r.ok) { emptyRow(tb, 7, r.data.error || 'The watchdog data is unavailable.'); return; }
        var s = r.data.summary;
        $('#wd-n-normal').textContent = s.normal; $('#wd-n-acting').textContent = s.acting;
        $('#wd-n-alert').textContent = s.alert_only; $('#wd-n-off').textContent = s.not_reporting;
        var pol = r.data.policy || {};
        $('#wd-ver').textContent = pol.stored ? 'v' + pol.version : 'no profile';
        $('#wd-inforce').textContent = pol.stored ? 'In force on ' + pol.in_force + ' of ' + pol.workers + ' worker agents' : 'Nothing pushed yet';
        var hosts = r.data.hosts || [];
        if (!hosts.length) { emptyRow(tb, 7, 'No worker agents are registered.'); return; }
        clear(tb);
        hosts.forEach(function (h) {
          var c = COND[h.condition] || COND.normal, tr = el('tr');
          var name = th(h.machine, 'cell-primary');
          name.appendChild(el('div', 'cell-sub', 'Worker agent' + (h.connection === 'online' ? '' : ' (' + h.connection + ')')));
          tr.appendChild(name);
          var st = td(chip(c[0], c[1], c[2]));
          if (h.condition !== 'not-reporting') { st.appendChild(document.createTextNode(' ')); st.appendChild(ladder(h.level, h.condition === 'alert-only')); }
          if (h.stage !== 'none') st.appendChild(el('div', 'cell-sub', 'last step: ' + (STEP[h.stage] || h.stage)));
          if (h.breaches && h.breaches.length) st.appendChild(el('div', 'cell-sub', 'breached: ' + h.breaches.join(', ')));
          tr.appendChild(st);
          var pc = el('td', 'nowrap');
          if (h.policy_acked && h.policy_ok) pc.textContent = 'v' + h.policy_in_force + ' in force';
          else if (h.policy_acked) { pc.appendChild(chip('danger', 'Refused v' + h.policy_pushed, 'x')); pc.appendChild(el('div', 'cell-sub', h.policy_error || '')); }
          else if (h.policy_pushed) pc.textContent = 'v' + h.policy_pushed + ' pushed, no ack yet';
          else { pc.textContent = 'default'; pc.className = 'nowrap faint'; }
          tr.appendChild(pc);
          tr.appendChild(td(h.condition === 'not-reporting' ? '-' : h.restarts_today + ' / ' + h.restart_cap, 'num' + (h.condition === 'not-reporting' ? ' faint' : '')));
          tr.appendChild(td(h.condition === 'not-reporting' ? '-' : h.reboots_today + ' / ' + h.reboot_cap, 'num' + (h.condition === 'not-reporting' ? ' faint' : '')));
          tr.appendChild(td(h.last_event ? fmtAgo(h.last_event.time) + ': ' + ((KIND[h.last_event.kind] || [0, h.last_event.kind])[1]) : 'none', h.last_event ? 'nowrap' : 'faint nowrap'));
          var btn = el('button', 'btn btn--sm'); btn.type = 'button'; btn.textContent = 'Reset counters';
          btn.setAttribute('data-stub', 'Resetting counters needs a control-channel message to the agent\'s watchdog, which does not exist yet (a later task). Until then a counter reset is done on the host.');
          tr.appendChild(td(btn, 'actions'));
          tb.appendChild(tr);
        });
      });
    }

    function evDetail(ev) {
      var bits = [];
      if (ev.detail) bits.push(ev.detail);
      if (ev.scope || ev.metric) bits.push((ev.scope ? ev.scope + ' ' : '') + (ev.metric || '') + (ev.value ? ' at ' + Math.round(ev.value * 10) / 10 : ''));
      if (ev.target) bits.push('target ' + ev.target);
      return bits.join(' · ');
    }
    function loadEvents() {
      return api('GET', '/api/watchdog/events').then(function (r) {
        var tb = $('#wd-events');
        if (!r.ok) { emptyRow(tb, 4, r.data.error || 'Unavailable.'); return; }
        var evs = r.data.events || [];
        if (!evs.length) { emptyRow(tb, 4, 'No watchdog event has been reported yet.'); return; }
        clear(tb);
        evs.forEach(function (ev) {
          var k = KIND[ev.kind] || ['info', ev.kind], tr = el('tr');
          tr.appendChild(td(fmtDateTime(ev.time), 'nowrap'));
          tr.appendChild(td(ev.machine));
          var label = k[1] + (ev.kind === 'action' && ev.step ? ': ' + (STEP[ev.step] || ev.step) : '');
          var tone = ev.kind === 'action' && (ev.step === 'restart' || ev.step === 'reboot') ? 'danger' : k[0];
          tr.appendChild(td(chip(tone, label)));
          tr.appendChild(td(evDetail(ev)));
          tb.appendChild(tr);
        });
      });
    }

    var yaml = $('#wd-yaml'), dirty = false, loadedVersion = 0;
    yaml.addEventListener('input', function () { dirty = true; });
    function row(dl, k, v) { dl.appendChild(el('dt', null, k)); dl.appendChild(el('dd', 'mono', v)); }
    function th2(t) { return t && t.trigger ? 'trigger ' + t.trigger + ', recover ' + t.recover : 'off'; }
    function loadPolicy(force) {
      return api('GET', '/api/watchdog/policy').then(function (r) {
        if (!r.ok) { yaml.value = ''; say('danger', r.data.error || 'The policy is unavailable.'); return; }
        var d = r.data;
        loadedVersion = d.version || 0;
        if (force || !dirty) {
          yaml.value = d.has_profile ? d.yaml.replace(/^version:\s*\d+/m, 'version: ' + (d.version + 1)) : d.yaml;
          dirty = false;
          $('#wd-yaml-h').textContent = (d.has_profile ? 'Stored profile v' + d.version + ', with the version already raised to ' + (d.version + 1) + ' for your edit. ' : 'No profile is stored yet: this is a starting point built from the baked-in default. ') +
            'Every key is optional; an absent key keeps the default. Per-host and per-group overrides go under overrides:. A profile that fails validation is refused and nothing is pushed.';
        }
        var e = d.effective, dl = $('#wd-effective');
        clear(dl);
        row(dl, 'Sampling interval / sustained breach', e.sample_interval + ' / ' + e.sustain);
        row(dl, 'CPU (machine)', th2(e.machine.cpu_percent) + ' %');
        row(dl, 'Memory (machine)', th2(e.machine.memory_percent) + ' %');
        row(dl, 'Load per CPU', th2(e.machine.load_per_cpu));
        row(dl, 'Disk', th2(e.machine.disk_percent) + ' %');
        row(dl, 'Ladder steps', ['throttle', 'kill_session', 'restart', 'reboot'].map(function (k) { return k + ' ' + (e.steps[k] ? 'on' : 'off'); }).join(', '));
        row(dl, 'Throttle', 'CPU quota ' + e.throttle.cpu_percent + ' % for ' + e.throttle.duration);
        row(dl, 'Cooldowns', 'throttle ' + e.cooldowns.throttle + ', kill ' + e.cooldowns.kill_session + ', restart ' + e.cooldowns.restart + ', reboot ' + e.cooldowns.reboot);
        row(dl, 'Daily caps', e.caps.restarts_per_day + ' restarts, ' + e.caps.reboots_per_day + ' reboots');
        row(dl, 'Boot stability / stage reset', e.boot_window + ' / ' + e.stage_reset);
        row(dl, 'Auto-reset counters at midnight', e.auto_reset_midnight ? 'yes' : 'no');
        row(dl, 'Overrides', (d.hosts.length ? 'hosts: ' + d.hosts.join(', ') : 'no host overrides') + '; ' + (d.groups.length ? 'groups: ' + d.groups.join(', ') : 'no group overrides'));
      });
    }
    $('#wd-revert').addEventListener('click', function () { dirty = false; loadPolicy(true); say('info', 'Edits discarded.'); });
    $('#wd-policy-form').addEventListener('submit', function (e) {
      e.preventDefault();
      var btn = $('button[type="submit"]', this);
      btn.disabled = true;
      api('POST', '/api/watchdog/policy', { yaml: yaml.value }).then(function (r) {
        btn.disabled = false;
        if (!r.ok) { say('danger', r.data.error || ('Not saved (' + r.status + ')')); return; }
        dirty = false;
        say('ok', 'Version ' + r.data.version + ' saved and pushed to ' + r.data.pushed_to + ' connected worker agent(s). Offline agents get it when they reconnect.');
        loadPolicy(true); loadHosts();
      });
    });

    var refresh = debounce(function () { loadHosts(); loadEvents(); }, 300);
    liveSocket('/ws/watchdog', function (m) { if (m.type === 'event') refresh(); }, function (again) { if (again) refresh(); });
    loadHosts(); loadEvents(); loadPolicy(true);
    setInterval(function () { if (!document.hidden) { loadHosts(); loadEvents(); } }, 15000);
  }

  // live.js is loaded with defer: the DOM is parsed, so start right away.
  if (page === 'monitor') initMonitor();
  if (page === 'watchdog') initWatchdog();
  if (page === 'evidence') initEvidence();
  if (page === 'backup') initBackup();
  if (page === 'machines') { initMachines(); initEnroll(); }
  if (page === 'commands') initCommands();
  if (page === 'terminal') initTerminal();
})();
