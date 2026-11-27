/* Fleet panel — swappable background layer (default: monochrome Matrix rain).
   Driven by <html data-bg="matrix|none|..."> and <canvas id="bg-canvas">.
   Respects DPR cap (<= 2), prefers-reduced-motion (static frame),
   theme changes (data-theme), and pauses animation when document.hidden. */
(function () {
  'use strict';

  var root = document.documentElement;
  var canvas = document.getElementById('bg-canvas');
  if (!canvas) return;
  var ctx = canvas.getContext('2d');
  if (!ctx) return;

  var dpr = 1;
  var width = 0;
  var height = 0;
  var rafId = 0;
  var lastTick = 0;
  var activeName = '';
  var active = null;

  var motionMQ = window.matchMedia ? window.matchMedia('(prefers-reduced-motion: reduce)') : null;
  function isReduced() { return !!(motionMQ && motionMQ.matches); }

  /* ---------- Renderer: monochrome Matrix rain (Round 5 / 01) ---------- */
  function createMatrixRenderer() {
    var glyphs = '\uFF71\uFF72\uFF73\uFF74\uFF75\uFF76\uFF77\uFF78\uFF79\uFF7A\uFF7B\uFF7C\uFF7D\uFF7E\uFF7F\uFF80\uFF81\uFF82\uFF83\uFF84\uFF85\uFF86\uFF87\uFF88\uFF89\uFF8A\uFF8B\uFF8C\uFF8D\uFF8E\uFF8F\uFF90\uFF91\uFF92\uFF93\uFF94\uFF95\uFF96\uFF97\uFF98\uFF99\uFF9A\uFF9B\uFF9C\uFF9D0123456789ABCDEF:<>=*+-|';
    var fontSize = 14;
    var cols = 0;
    var drops = [];
    var lastTheme = '';

    function readColors() {
      var cs = getComputedStyle(root);
      return {
        fade: cs.getPropertyValue('--matrix-fade').trim() || 'rgb(9 9 11 / 0.11)',
        head: cs.getPropertyValue('--matrix-head').trim() || '#dcdce4',
        trail: cs.getPropertyValue('--matrix-trail').trim() || '#6e6e7a'
      };
    }

    function resize() {
      var newCols = Math.max(1, Math.ceil(width / fontSize));
      var nextDrops = [];
      for (var i = 0; i < newCols; i++) {
        nextDrops[i] = i < drops.length ? drops[i] : Math.floor(Math.random() * -40);
      }
      cols = newCols;
      drops = nextDrops;
    }

    function step() {
      var theme = root.getAttribute('data-theme') || 'dark';
      if (theme !== lastTheme) {
        ctx.clearRect(0, 0, width, height);
        lastTheme = theme;
      }
      var c = readColors();
      ctx.fillStyle = c.fade;
      ctx.fillRect(0, 0, width, height);
      ctx.font = '700 ' + fontSize + 'px "Red Hat Mono", ui-monospace, monospace';
      ctx.textBaseline = 'top';

      for (var i = 0; i < cols; i++) {
        var y = drops[i] * fontSize;
        if (y >= 0 && y < height + fontSize) {
          var ch = glyphs.charAt(Math.floor(Math.random() * glyphs.length));
          ctx.fillStyle = Math.random() > 0.86 ? c.head : c.trail;
          ctx.fillText(ch, i * fontSize, y);
        }
        if (y > height && Math.random() > 0.975) {
          drops[i] = Math.floor(Math.random() * -18);
        } else {
          drops[i]++;
        }
      }
    }

    function renderStatic() {
      ctx.clearRect(0, 0, width, height);
      lastTheme = root.getAttribute('data-theme') || 'dark';
      for (var n = 0; n < 24; n++) step();
    }

    return {
      intervalMs: 48,
      resize: resize,
      step: step,
      renderStatic: renderStatic
    };
  }

  var RENDERERS = {
    matrix: createMatrixRenderer
  };

  function resizeCanvas() {
    dpr = Math.min(window.devicePixelRatio || 1, 2);
    width = window.innerWidth;
    height = window.innerHeight;
    canvas.width = Math.floor(width * dpr);
    canvas.height = Math.floor(height * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    if (active && active.resize) active.resize();
  }

  function stopLoop() {
    if (rafId) {
      window.cancelAnimationFrame(rafId);
      rafId = 0;
    }
  }

  function loop(ts) {
    if (document.hidden || !active || isReduced()) {
      rafId = 0;
      return;
    }
    if (ts - lastTick >= (active.intervalMs || 48)) {
      active.step();
      lastTick = ts;
    }
    rafId = window.requestAnimationFrame(loop);
  }

  function syncState() {
    stopLoop();
    if (!active) {
      ctx.clearRect(0, 0, width, height);
      return;
    }
    if (isReduced()) {
      active.renderStatic();
      return;
    }
    if (!document.hidden) {
      lastTick = 0;
      rafId = window.requestAnimationFrame(loop);
    }
  }

  function selectRenderer(name) {
    var next = name || 'matrix';
    if (next === activeName && (active || next === 'none')) return;
    activeName = next;
    var factory = RENDERERS[next];
    active = factory ? factory() : null;
    resizeCanvas();
    syncState();
  }

  selectRenderer(root.getAttribute('data-bg') || 'matrix');

  window.addEventListener('resize', function () {
    resizeCanvas();
    if (active && isReduced()) active.renderStatic();
  });

  document.addEventListener('visibilitychange', syncState);

  if (motionMQ && motionMQ.addEventListener) {
    motionMQ.addEventListener('change', syncState);
  }

  if (window.MutationObserver) {
    new MutationObserver(function (mutations) {
      for (var i = 0; i < mutations.length; i++) {
        var attr = mutations[i].attributeName;
        if (attr === 'data-bg') {
          selectRenderer(root.getAttribute('data-bg') || 'matrix');
          return;
        }
        if (attr === 'data-theme' && active && isReduced()) {
          active.renderStatic();
        }
      }
    }).observe(root, { attributes: true, attributeFilter: ['data-theme', 'data-bg'] });
  }

  window.FleetBg = {
    set: function (name) {
      root.setAttribute('data-bg', name || 'matrix');
    },
    current: function () {
      return root.getAttribute('data-bg') || 'matrix';
    }
  };
})();
