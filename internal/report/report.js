/* Fleet test report: page script. No network access, no dependencies. The
   charts are drawn server-side as inline SVG; this adds the hover readout (with
   the command overlay), section navigation, theme toggle and expand/collapse. */
(function () {
  "use strict";

  function pad(n) { return n < 10 ? "0" + n : "" + n; }
  function clock(ms) {
    var d = new Date(ms);
    return pad(d.getUTCHours()) + ":" + pad(d.getUTCMinutes()) + ":" + pad(d.getUTCSeconds()) + " UTC";
  }
  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) { e.className = cls; }
    if (text !== undefined) { e.textContent = text; }
    return e;
  }
  function fmt(v) { return (Math.round(v * 1000) / 1000).toString(); }

  // ---- charts: hover readout with the command overlay ----
  function initChart(svg) {
    var fig = svg.parentNode;
    var tip = fig.querySelector(".tip");
    var cursor = svg.querySelector(".cursor");
    if (!tip || !cursor) { return; }
    var plot = svg.getAttribute("data-plot").split(",").map(Number); // x, y, w, h
    var t0 = Number(svg.getAttribute("data-t0")), t1 = Number(svg.getAttribute("data-t1"));
    var unit = svg.getAttribute("data-unit") || "";
    var vbw = svg.viewBox.baseVal.width;

    var series = [];
    svg.querySelectorAll("path[data-pts]").forEach(function (p) {
      var pts = p.getAttribute("data-pts").split(";").map(function (s) {
        var i = s.indexOf(":");
        return [Number(s.slice(0, i)), Number(s.slice(i + 1))];
      });
      series.push({ name: p.getAttribute("data-name"), cls: p.getAttribute("class"), pts: pts });
    });
    var overlays = Array.prototype.map.call(svg.querySelectorAll("rect.ov"), function (r) {
      return { t0: Number(r.getAttribute("data-t0")), t1: Number(r.getAttribute("data-t1")), label: r.getAttribute("data-label") };
    });
    var marks = Array.prototype.map.call(svg.querySelectorAll(".mark"), function (m) {
      return { t: Number(m.getAttribute("data-t")), label: m.getAttribute("data-label") };
    });

    function nearest(pts, t) {
      var best = null, bd = Infinity;
      for (var i = 0; i < pts.length; i++) {
        var d = Math.abs(pts[i][0] - t);
        if (d < bd) { bd = d; best = pts[i]; }
      }
      return best;
    }

    function show(ev) {
      var r = svg.getBoundingClientRect();
      if (!r.width) { return; }
      var scale = vbw / r.width;
      var px = (ev.clientX - r.left) * scale;
      var frac = Math.min(1, Math.max(0, (px - plot[0]) / plot[2]));
      var t = t0 + frac * (t1 - t0);
      var x = plot[0] + frac * plot[2];
      cursor.setAttribute("x1", x); cursor.setAttribute("x2", x); cursor.removeAttribute("hidden");

      tip.textContent = "";
      tip.appendChild(el("b", "", clock(t)));
      series.forEach(function (s) {
        var p = nearest(s.pts, t);
        if (!p) { return; }
        var row = el("div", "row");
        row.appendChild(el("span", "", s.name));
        row.appendChild(el("span", "mono", fmt(p[1]) + " " + unit));
        tip.appendChild(row);
      });
      var span = (t1 - t0) * 0.015;
      overlays.forEach(function (o) {
        if (t >= o.t0 && t <= o.t1) { tip.appendChild(el("div", "cmdline", "▶ " + o.label)); }
      });
      marks.forEach(function (m) {
        if (Math.abs(m.t - t) <= span) { tip.appendChild(el("div", "cmdline", "⚠ " + m.label)); }
      });
      tip.removeAttribute("hidden");
      var fr = fig.getBoundingClientRect();
      var left = ev.clientX - fr.left + 14;
      if (left + tip.offsetWidth > fr.width) { left = ev.clientX - fr.left - tip.offsetWidth - 14; }
      tip.style.left = Math.max(0, left) + "px";
      tip.style.top = (ev.clientY - fr.top + 14) + "px";
    }
    function hide() { tip.setAttribute("hidden", ""); cursor.setAttribute("hidden", ""); }

    svg.addEventListener("pointermove", show);
    svg.addEventListener("pointerdown", show);
    svg.addEventListener("pointerleave", hide);
  }
  document.querySelectorAll("svg.chart[data-chart]").forEach(initChart);

  // ---- section navigation ----
  var links = Array.prototype.slice.call(document.querySelectorAll(".toc a[href^='#']"));
  var targets = links.map(function (a) { return document.getElementById(a.getAttribute("href").slice(1)); });
  function markActive() {
    var cur = 0;
    for (var i = 0; i < targets.length; i++) {
      if (targets[i] && targets[i].getBoundingClientRect().top < window.innerHeight * 0.35) { cur = i; }
    }
    links.forEach(function (a, i) { a.classList.toggle("on", i === cur); });
  }
  window.addEventListener("scroll", markActive, { passive: true });
  markActive();
  // Following a nav link opens a collapsed section.
  links.forEach(function (a, i) {
    a.addEventListener("click", function () { if (targets[i] && targets[i].tagName === "DETAILS") { targets[i].open = true; } });
  });

  // ---- theme + expand/collapse ----
  var root = document.documentElement;
  var theme = document.getElementById("theme-toggle");
  if (theme) {
    theme.addEventListener("click", function () {
      var dark = root.getAttribute("data-theme") === "dark" ||
        (!root.getAttribute("data-theme") && window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches);
      root.setAttribute("data-theme", dark ? "light" : "dark");
    });
  }
  var all = document.getElementById("toggle-all");
  if (all) {
    all.addEventListener("click", function () {
      var cards = document.querySelectorAll("details.card");
      var anyClosed = Array.prototype.some.call(cards, function (c) { return !c.open; });
      cards.forEach(function (c) { c.open = anyClosed; });
      all.textContent = anyClosed ? "Collapse all" : "Expand all";
    });
  }
  window.addEventListener("beforeprint", function () {
    document.querySelectorAll("details.card").forEach(function (c) { c.open = true; });
  });
})();
