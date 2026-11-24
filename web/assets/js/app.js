/* Fleet panel — tiny vanilla JS. No framework, no network, no build step.
   Loaded synchronously in <head> so the theme is set before first paint.
   Behaviour is limited to UI-only affordances:
     - theme (light/dark) with persistence
     - mobile nav drawer
     - data-stub="..."  -> visible "not available yet" toast (visible-by-default, arch. 12.3)
     - data-toggle-panel="#id" -> show/hide a panel
     - [data-filter-table] -> client-side filter of the mock Machines rows            */
(function () {
  'use strict';
  var KEY = 'fleet-panel-theme';
  var root = document.documentElement;

  /* ---------- Theme ---------- */
  function stored() { try { return localStorage.getItem(KEY); } catch (e) { return null; } }
  function systemDark() { return window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches; }
  function apply(theme) {
    root.setAttribute('data-theme', theme);
    var btn = document.querySelector('.theme-toggle');
    if (btn) {
      var next = theme === 'dark' ? 'light' : 'dark';
      btn.setAttribute('aria-label', 'Switch to ' + next + ' theme');
      btn.setAttribute('title', 'Switch to ' + next + ' theme');
    }
  }
  var saved = stored();
  apply(saved === 'light' || saved === 'dark' ? saved : (systemDark() ? 'dark' : 'light'));
  if (window.matchMedia) {
    matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function (e) {
      if (!stored()) apply(e.matches ? 'dark' : 'light');
    });
  }

  document.addEventListener('DOMContentLoaded', function () {
    apply(root.getAttribute('data-theme'));

    var toggle = document.querySelector('.theme-toggle');
    if (toggle) toggle.addEventListener('click', function () {
      var next = root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
      apply(next);
      try { localStorage.setItem(KEY, next); } catch (e) { /* file:// privacy modes */ }
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
