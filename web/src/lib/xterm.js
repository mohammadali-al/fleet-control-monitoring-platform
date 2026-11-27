let loadPromise = null;

function loadStylesheet(href) {
  if (document.querySelector(`link[rel="stylesheet"][href="${href}"]`)) {
    return;
  }
  const link = document.createElement('link');
  link.rel = 'stylesheet';
  link.href = href;
  const appCss = document.querySelector('link[href*="/assets/dist/app.css"]');
  if (appCss && appCss.parentNode) {
    appCss.parentNode.insertBefore(link, appCss);
  } else {
    document.head.appendChild(link);
  }
}

function loadScript(src) {
  return new Promise((resolve, reject) => {
    const existing = document.querySelector(`script[src="${src}"]`);
    if (existing) {
      if (existing.dataset.loaded === '1') {
        resolve();
        return;
      }
      existing.addEventListener('load', () => resolve(), { once: true });
      existing.addEventListener('error', () => reject(new Error(`Failed to load ${src}`)), { once: true });
      return;
    }
    const s = document.createElement('script');
    s.src = src;
    s.async = false;
    s.addEventListener(
      'load',
      () => {
        s.dataset.loaded = '1';
        resolve();
      },
      { once: true }
    );
    s.addEventListener('error', () => reject(new Error(`Failed to load ${src}`)), { once: true });
    document.head.appendChild(s);
  });
}

/**
 * Lazily loads the self-hosted vendored xterm.js, addon-fit.js, and xterm.css
 * from /assets/vendor/xterm/ once per SPA session.
 */
export function ensureXterm() {
  if (typeof window === 'undefined') {
    return Promise.reject(new Error('No window'));
  }
  loadStylesheet('/assets/vendor/xterm/xterm.css');
  if (window.Terminal && window.FitAddon?.FitAddon) {
    return Promise.resolve({
      Terminal: window.Terminal,
      FitAddon: window.FitAddon.FitAddon,
    });
  }
  if (!loadPromise) {
    loadPromise = loadScript('/assets/vendor/xterm/xterm.js')
      .then(() => loadScript('/assets/vendor/xterm/addon-fit.js'))
      .then(() => {
        if (!window.Terminal) {
          throw new Error('xterm.js did not expose window.Terminal');
        }
        return {
          Terminal: window.Terminal,
          FitAddon: window.FitAddon?.FitAddon || null,
        };
      })
      .catch((err) => {
        loadPromise = null;
        throw err;
      });
  }
  return loadPromise;
}
