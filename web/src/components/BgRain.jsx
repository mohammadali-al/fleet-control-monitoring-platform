import { useEffect, useRef } from 'react';

const GLYPHS =
  '\uFF71\uFF72\uFF73\uFF74\uFF75\uFF76\uFF77\uFF78\uFF79\uFF7A\uFF7B\uFF7C\uFF7D\uFF7E\uFF7F\uFF80' +
  '\uFF81\uFF82\uFF83\uFF84\uFF85\uFF86\uFF87\uFF88\uFF89\uFF8A\uFF8B\uFF8C\uFF8D\uFF8E\uFF8F\uFF90' +
  '\uFF91\uFF92\uFF93\uFF94\uFF95\uFF96\uFF97\uFF98\uFF99\uFF9A\uFF9B\uFF9C\uFF9D0123456789ABCDEF:<>=*+-|';

/**
 * Monochrome Matrix rain background canvas.
 * Mounted once at the persistent Shell level and never unmounted on route changes,
 * so drop state and animation stay continuous across navigation.
 */
export function BgRain() {
  const canvasRef = useRef(null);

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return undefined;
    const ctx = canvas.getContext('2d');
    if (!ctx) return undefined;

    const root = document.documentElement;
    const motionMQ = window.matchMedia ? window.matchMedia('(prefers-reduced-motion: reduce)') : null;
    const isReduced = () => Boolean(motionMQ && motionMQ.matches);

    let dpr = 1;
    let width = 0;
    let height = 0;
    let rafId = 0;
    let lastTick = 0;
    let activeName = '';
    let active = null;

    function createMatrixRenderer() {
      const fontSize = 14;
      let cols = 0;
      let drops = [];
      let lastTheme = '';

      function readColors() {
        const cs = getComputedStyle(root);
        return {
          fade: cs.getPropertyValue('--matrix-fade').trim() || 'rgb(9 9 11 / 0.11)',
          head: cs.getPropertyValue('--matrix-head').trim() || '#dcdce4',
          trail: cs.getPropertyValue('--matrix-trail').trim() || '#6e6e7a',
        };
      }

      function resize() {
        const newCols = Math.max(1, Math.ceil(width / fontSize));
        const nextDrops = [];
        for (let i = 0; i < newCols; i++) {
          nextDrops[i] = i < drops.length ? drops[i] : Math.floor(Math.random() * -40);
        }
        cols = newCols;
        drops = nextDrops;
      }

      function step() {
        const theme = root.getAttribute('data-theme') || 'dark';
        if (theme !== lastTheme) {
          ctx.clearRect(0, 0, width, height);
          lastTheme = theme;
        }
        const c = readColors();
        ctx.fillStyle = c.fade;
        ctx.fillRect(0, 0, width, height);
        ctx.font = `700 ${fontSize}px "Red Hat Mono", ui-monospace, monospace`;
        ctx.textBaseline = 'top';

        for (let i = 0; i < cols; i++) {
          const y = drops[i] * fontSize;
          if (y >= 0 && y < height + fontSize) {
            const ch = GLYPHS.charAt(Math.floor(Math.random() * GLYPHS.length));
            ctx.fillStyle = Math.random() > 0.86 ? c.head : c.trail;
            ctx.fillText(ch, i * fontSize, y);
          }
          if (y > height && Math.random() > 0.975) {
            drops[i] = Math.floor(Math.random() * -18);
          } else {
            drops[i] += 1;
          }
        }
      }

      function renderStatic() {
        ctx.clearRect(0, 0, width, height);
        lastTheme = root.getAttribute('data-theme') || 'dark';
        for (let n = 0; n < 24; n++) step();
      }

      return { intervalMs: 48, resize, step, renderStatic };
    }

    const RENDERERS = { matrix: createMatrixRenderer };

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
      const next = name || 'matrix';
      if (next === activeName && (active || next === 'none')) return;
      activeName = next;
      const factory = RENDERERS[next];
      active = factory ? factory() : null;
      resizeCanvas();
      syncState();
    }

    selectRenderer(root.getAttribute('data-bg') || 'matrix');

    const handleResize = () => {
      resizeCanvas();
      if (active && isReduced()) active.renderStatic();
    };
    window.addEventListener('resize', handleResize);
    document.addEventListener('visibilitychange', syncState);
    if (motionMQ && motionMQ.addEventListener) {
      motionMQ.addEventListener('change', syncState);
    }

    const observer = window.MutationObserver
      ? new MutationObserver((mutations) => {
          for (const m of mutations) {
            if (m.attributeName === 'data-bg') {
              selectRenderer(root.getAttribute('data-bg') || 'matrix');
              return;
            }
            if (m.attributeName === 'data-theme' && active && isReduced()) {
              active.renderStatic();
            }
          }
        })
      : null;

    if (observer) {
      observer.observe(root, { attributes: true, attributeFilter: ['data-theme', 'data-bg'] });
    }

    window.FleetBg = {
      set(name) {
        root.setAttribute('data-bg', name || 'matrix');
      },
      current() {
        return root.getAttribute('data-bg') || 'matrix';
      },
    };

    return () => {
      stopLoop();
      window.removeEventListener('resize', handleResize);
      document.removeEventListener('visibilitychange', syncState);
      if (motionMQ && motionMQ.removeEventListener) {
        motionMQ.removeEventListener('change', syncState);
      }
      if (observer) observer.disconnect();
    };
  }, []);

  return <canvas className="bg-canvas" id="bg-canvas" aria-hidden="true" ref={canvasRef} />;
}
