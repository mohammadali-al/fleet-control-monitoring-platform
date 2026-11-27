/* Fleet panel — tiny vanilla JS. No framework, no network, no build step.
   Loaded synchronously in <head> so the theme is wired cleanly.
   Behaviour is limited to UI-only affordances:
     - theme (dark default / light) with client-side localStorage persistence
     - mobile nav drawer
     - data-stub="..."  -> visible "not available yet" toast
     - data-toggle-panel="#id" -> show/hide a panel
     - [data-filter-table] -> client-side filter of the Machines rows
     - tabs, row menus, dialogs, tooltips (hover + focus), mock semantic-zoom levels, switch labels */
(function () {
  'use strict';
  var KEY = 'fleet-theme';
  var root = document.documentElement;

  /* ---------- Theme (client-side only, default dark) ---------- */
  function stored() { try { return localStorage.getItem(KEY); } catch (e) { return null; } }
  function apply(theme) {
    var t = theme === 'light' ? 'light' : 'dark';
    root.setAttribute('data-theme', t);
    var btn = document.querySelector('.theme-toggle');
    if (btn) {
      var next = t === 'dark' ? 'light' : 'dark';
      btn.setAttribute('aria-label', 'Switch to ' + next + ' theme');
      btn.setAttribute('title', 'Switch to ' + next + ' theme');
      btn.setAttribute('aria-pressed', String(t === 'dark'));
    }
  }
  var saved = stored();
  apply(saved === 'light' || saved === 'dark' ? saved : 'dark');

  document.addEventListener('DOMContentLoaded', function () {
    apply(root.getAttribute('data-theme'));

    var toggle = document.querySelector('.theme-toggle');
    if (toggle) toggle.addEventListener('click', function () {
      var next = root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
      apply(next);
      try { localStorage.setItem(KEY, next); } catch (e) { /* storage disabled */ }
    });

    /* ---------- Mobile nav drawer ---------- */
    var menuBtn = document.querySelector('.topbar__menu');
    function setNav(open) {
      document.body.classList.toggle('nav-open', open);
      if (menuBtn) menuBtn.setAttribute('aria-expanded', String(open));
    }
    if (menuBtn) {
      menuBtn.addEventListener('click', function () { setNav(!document.body.classList.contains('nav-open')); });
      document.addEventListener('keydown', function (e) { if (e.key === 'Escape') setNav(false); });
      document.addEventListener('click', function (e) {
        if (document.body.classList.contains('nav-open') && !e.target.closest('.sidebar') && !e.target.closest('.topbar__menu')) setNav(false);
      });
    }

    /* ---------- Toast for stubbed actions ---------- */
    var region = document.querySelector('.toast-region');
    function toast(msg) {
      if (!region) return;
      var t = document.createElement('div');
      t.className = 'toast';
      t.textContent = msg;
      region.appendChild(t);
      setTimeout(function () { t.remove(); }, 4500);
    }
    document.addEventListener('click', function (e) {
      var el = e.target.closest('[data-stub]');
      if (!el) return;
      e.preventDefault();
      toast(el.getAttribute('data-stub') || 'This action is not available yet.');
    });
    document.addEventListener('submit', function (e) {
      var f = e.target.closest('form[data-stub-form]');
      if (!f) return;
      e.preventDefault();
      var dlg = f.closest('dialog');
      if (dlg && dlg.open) dlg.close();
      toast(f.getAttribute('data-stub-form'));
    });

    /* ---------- Show/hide panels ---------- */
    document.querySelectorAll('[data-toggle-panel]').forEach(function (btn) {
      var panel = document.querySelector(btn.getAttribute('data-toggle-panel'));
      if (!panel) return;
      btn.addEventListener('click', function () {
        var open = panel.hidden;
        panel.hidden = !open;
        btn.setAttribute('aria-expanded', String(open));
        if (open) { var f = panel.querySelector('input,select,textarea'); if (f) f.focus(); }
      });
    });

    /* ---------- Row action menus (fixed-position so tables never clip them) ---------- */
    function closeMenus(except) {
      document.querySelectorAll('.menu__pop:not([hidden])').forEach(function (p) {
        if (p === except) return;
        p.hidden = true;
        var b = p.parentNode.querySelector('[data-menu-btn]');
        if (b) b.setAttribute('aria-expanded', 'false');
      });
    }
    document.addEventListener('click', function (e) {
      var btn = e.target.closest('[data-menu-btn]');
      if (btn) {
        var pop = btn.parentNode.querySelector('.menu__pop');
        var open = pop.hidden;
        closeMenus(open ? pop : null);
        pop.hidden = !open;
        btn.setAttribute('aria-expanded', String(open));
        if (open) {
          var r = btn.getBoundingClientRect();
          pop.style.top = Math.min(r.bottom + 4, window.innerHeight - pop.offsetHeight - 8) + 'px';
          pop.style.left = Math.max(8, r.right - pop.offsetWidth) + 'px';
          var first = pop.querySelector('[role="menuitem"]'); if (first) first.focus({ preventScroll: true });
        }
        return;
      }
      if (!e.target.closest('.menu__pop')) closeMenus(null);
      else if (e.target.closest('[role="menuitem"]')) closeMenus(null);
    });
    document.addEventListener('keydown', function (e) {
      var pop = e.target.closest && e.target.closest('.menu__pop');
      if (e.key === 'Escape') {
        var open = document.querySelector('.menu__pop:not([hidden])');
        if (open) { var b = open.parentNode.querySelector('[data-menu-btn]'); closeMenus(null); if (b) b.focus({ preventScroll: true }); }
      }
      if (pop && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) {
        e.preventDefault();
        var items = Array.prototype.slice.call(pop.querySelectorAll('[role="menuitem"]'));
        var i = items.indexOf(document.activeElement);
        items[(i + (e.key === 'ArrowDown' ? 1 : items.length - 1)) % items.length].focus({ preventScroll: true });
      }
    });
    window.addEventListener('scroll', function () { closeMenus(null); }, true);

    /* ---------- Dialogs ---------- */
    document.addEventListener('click', function (e) {
      var o = e.target.closest('[data-dialog-open]');
      if (o) {
        var d = document.querySelector(o.getAttribute('data-dialog-open'));
        if (d && d.showModal) d.showModal();
        return;
      }
      if (e.target.closest('[data-dialog-close]')) {
        var dd = e.target.closest('dialog');
        if (dd) dd.close();
      }
    });

    /* ---------- Tabs (roving tabindex, arrow keys) ---------- */
    document.querySelectorAll('[data-tabs]').forEach(function (list) {
      var tabs = Array.prototype.slice.call(list.querySelectorAll('[role="tab"]'));
      function select(tab, focus) {
        tabs.forEach(function (t) {
          var on = t === tab;
          t.setAttribute('aria-selected', String(on));
          t.tabIndex = on ? 0 : -1;
          var p = document.getElementById(t.getAttribute('aria-controls'));
          if (p) p.hidden = !on;
        });
        if (focus) tab.focus();
      }
      tabs.forEach(function (t, i) {
        t.addEventListener('click', function () { select(t, false); });
        t.addEventListener('keydown', function (e) {
          var n = null;
          if (e.key === 'ArrowRight') n = tabs[(i + 1) % tabs.length];
          if (e.key === 'ArrowLeft') n = tabs[(i + tabs.length - 1) % tabs.length];
          if (e.key === 'Home') n = tabs[0];
          if (e.key === 'End') n = tabs[tabs.length - 1];
          if (n) { e.preventDefault(); select(n, true); }
        });
      });
      select(tabs.filter(function (t) { return t.getAttribute('aria-selected') === 'true'; })[0] || tabs[0], false);
    });

    /* ---------- Tooltip: hover or focus on [data-tip] ---------- */
    var tip = document.createElement('div');
    tip.className = 'tip'; tip.setAttribute('role', 'tooltip'); tip.hidden = true;
    document.body.appendChild(tip);
    function showTip(el, x, y) {
      var raw = el.getAttribute('data-tip');
      tip.innerHTML = '';
      raw.split('|').forEach(function (line, i) {
        var n = document.createElement(i === 0 ? 'b' : 'span');
        n.textContent = line; tip.appendChild(n);
        if (i > 0) tip.appendChild(document.createElement('br'));
      });
      tip.hidden = false;
      var r = el.getBoundingClientRect();
      var px = x == null ? r.left + r.width / 2 : x;
      var py = y == null ? r.top : y;
      tip.style.left = Math.max(8, Math.min(px - tip.offsetWidth / 2, window.innerWidth - tip.offsetWidth - 8)) + 'px';
      tip.style.top = Math.max(8, py - tip.offsetHeight - 10) + 'px';
    }
    document.addEventListener('mouseover', function (e) { var el = e.target.closest('[data-tip]'); if (el) showTip(el, e.clientX, el.getBoundingClientRect().top); });
    document.addEventListener('mouseout', function (e) { if (e.target.closest('[data-tip]')) tip.hidden = true; });
    document.addEventListener('focusin', function (e) { var el = e.target.closest('[data-tip]'); if (el) showTip(el); });
    document.addEventListener('focusout', function (e) { if (e.target.closest('[data-tip]')) tip.hidden = true; });
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape') tip.hidden = true; });

    /* ---------- Semantic-zoom levels (mock): radios switch the visible level ---------- */
    document.querySelectorAll('[data-zoom]').forEach(function (group) {
      var radios = Array.prototype.slice.call(group.querySelectorAll('input[type="radio"]'));
      var scope = document.querySelector(group.getAttribute('data-zoom'));
      if (!scope) return;
      function sync() {
        var val = (radios.filter(function (r) { return r.checked; })[0] || {}).value;
        scope.querySelectorAll('.zoom__panel').forEach(function (p) { p.hidden = p.getAttribute('data-level') !== val; });
        var live = document.querySelector('[data-zoom-label]');
        if (live) live.textContent = 'Zoom level: ' + val;
      }
      radios.forEach(function (r) { r.addEventListener('change', sync); });
      document.querySelectorAll('[data-zoom-step]').forEach(function (b) {
        b.addEventListener('click', function () {
          var i = radios.findIndex(function (r) { return r.checked; });
          var n = Math.max(0, Math.min(radios.length - 1, i + Number(b.getAttribute('data-zoom-step'))));
          radios[n].checked = true; sync();
        });
      });
      sync();
    });

    /* ---------- Switch label (On/Off) follows the checkbox ---------- */
    document.addEventListener('change', function (e) {
      var sw = e.target.closest && e.target.closest('.switch');
      if (!sw) return;
      var last = sw.lastElementChild;
      if (last && (last.textContent === 'On' || last.textContent === 'Off')) last.textContent = e.target.checked ? 'On' : 'Off';
    });

    /* ---------- Mock filtering of the Machines table ---------- */
    var table = document.querySelector('[data-filter-table]');
    if (table) {
      var q = document.querySelector('[data-filter-q]');
      var state = document.querySelector('[data-filter-state]');
      var count = document.querySelector('[data-filter-count]');
      var empty = document.querySelector('[data-filter-empty]');
      var rows = Array.prototype.slice.call(table.querySelectorAll('tbody tr'));
      var run = function () {
        var role = (document.querySelector('input[name="role"]:checked') || {}).value || 'all';
        var s = state ? state.value : 'all';
        var text = q ? q.value.trim().toLowerCase() : '';
        var shown = 0;
        rows.forEach(function (r) {
          var ok = (role === 'all' || r.dataset.role === role) &&
                   (s === 'all' || r.dataset.state === s) &&
                   (!text || r.textContent.toLowerCase().indexOf(text) !== -1);
          r.hidden = !ok;
          if (ok) shown++;
        });
        if (count) count.textContent = 'Showing ' + shown + ' of ' + rows.length + ' machines';
        if (empty) empty.hidden = shown !== 0;
      };
      document.querySelectorAll('input[name="role"]').forEach(function (i) { i.addEventListener('change', run); });
      if (q) q.addEventListener('input', run);
      if (state) state.addEventListener('change', run);
      run();
    }
  });
})();
