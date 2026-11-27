import { useCallback, useEffect, useRef, useState } from 'react';
import { Link, NavLink, Outlet, useLocation } from 'react-router-dom';
import { useTheme } from '../lib/theme';
import { NAV_GROUPS, ROUTES } from '../routes';
import { BgRain } from './BgRain';
import { Icons } from './Icons';

/**
 * Persistent SPA shell: SVG sprite, continuous Matrix rain canvas, sidebar nav,
 * mobile drawer (scrim z-50 < drawer z-60 inside .app), topbar, and <Outlet />.
 */
export function Shell() {
  const { theme, toggleTheme } = useTheme();
  const [navOpen, setNavOpen] = useState(false);
  const location = useLocation();
  const menuBtnRef = useRef(null);
  const sidebarRef = useRef(null);
  const wasOpenRef = useRef(false);

  const closeNav = useCallback(() => setNavOpen(false), []);

  // Close drawer on route change.
  useEffect(() => {
    setNavOpen(false);
  }, [location.pathname, location.search]);

  // Body scroll lock + focus management + Esc to close.
  useEffect(() => {
    document.body.classList.toggle('nav-open', navOpen);
    if (navOpen) {
      wasOpenRef.current = true;
      const firstLink = sidebarRef.current?.querySelector('.nav__link');
      if (firstLink) firstLink.focus({ preventScroll: true });

      const onKeyDown = (e) => {
        if (e.key === 'Escape') {
          e.preventDefault();
          setNavOpen(false);
        }
      };
      document.addEventListener('keydown', onKeyDown);
      return () => document.removeEventListener('keydown', onKeyDown);
    }

    if (wasOpenRef.current) {
      wasOpenRef.current = false;
      menuBtnRef.current?.focus({ preventScroll: true });
    }
    return undefined;
  }, [navOpen]);

  useEffect(() => () => {
    document.body.classList.remove('nav-open');
  }, []);

  const nextTheme = theme === 'dark' ? 'light' : 'dark';

  return (
    <>
      <Icons />
      <BgRain />
      <a className="skip-link" href="#main">Skip to content</a>
      <div className={`app app--spa${navOpen ? ' app--nav-open' : ''}`}>
        {navOpen && <div className="nav-scrim" aria-hidden="true" onClick={closeNav} />}

        <aside className="sidebar" id="sidebar" aria-label="Primary" ref={sidebarRef}>
          <Link className="brand" to="/machines" onClick={closeNav}>
            <span className="brand__mark">
              <svg className="icon" aria-hidden="true"><use href="#i-fleet" /></svg>
            </span>
            <span>
              Fleet panel
              <small>Control &amp; monitoring</small>
            </span>
          </Link>

          <nav className="nav" aria-label="Pages">
            {NAV_GROUPS.map((group) => {
              const items = ROUTES.filter((r) => r.group === group);
              return (
                <div className="nav__group" key={group}>
                  <div className="nav__label">{group}</div>
                  {items.map((r) => {
                    const content = (
                      <>
                        <svg className="icon" aria-hidden="true"><use href={`#${r.icon}`} /></svg>
                        {r.label}
                        {r.tag && <span className="chip chip--outline nav__tag">{r.tag}</span>}
                      </>
                    );
                    if (r.component) {
                      return (
                        <NavLink
                          key={r.id}
                          className="nav__link"
                          to={r.path}
                          onClick={closeNav}
                        >
                          {content}
                        </NavLink>
                      );
                    }
                    // Un-ported route: full-page load to the legacy server-rendered page.
                    return (
                      <a
                        key={r.id}
                        className="nav__link"
                        href={r.legacyPath}
                        onClick={closeNav}
                      >
                        {content}
                      </a>
                    );
                  })}
                </div>
              );
            })}
          </nav>

          <div className="sidebar__foot">
            <dl>
              <dt>Main server</dt><dd>fleet-server</dd>
              <dt>Panel</dt><dd className="mono">:6789</dd>
              <dt>Auth</dt><dd>via auth proxy</dd>
            </dl>
          </div>
        </aside>

        <div className="main">
          <header className="topbar">
            <button
              ref={menuBtnRef}
              className="icon-btn topbar__menu"
              type="button"
              aria-label={navOpen ? 'Close navigation' : 'Open navigation'}
              aria-controls="sidebar"
              aria-expanded={navOpen}
              onClick={() => setNavOpen((v) => !v)}
            >
              <svg className="icon" aria-hidden="true"><use href="#i-menu" /></svg>
            </button>
            <span className="chip chip--outline" title="Only one test can be active system-wide">
              <svg className="icon" aria-hidden="true"><use href="#i-clock" /></svg>
              Active test: none
            </span>
            <span className="topbar__spacer" />
            <button
              className="icon-btn icon-btn--bordered theme-toggle"
              type="button"
              aria-label={`Switch to ${nextTheme} theme`}
              title={`Switch to ${nextTheme} theme`}
              aria-pressed={theme === 'dark'}
              onClick={toggleTheme}
            >
              <svg className="icon icon-moon" aria-hidden="true"><use href="#i-moon" /></svg>
              <svg className="icon icon-sun" aria-hidden="true"><use href="#i-sun" /></svg>
            </button>
          </header>

          <main className="content" id="main">
            <Outlet />
          </main>
        </div>
      </div>
    </>
  );
}
