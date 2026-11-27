import { useEffect } from 'react';
import { BrowserRouter, Link, Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { Shell } from './components/Shell';
import { EventsProvider } from './lib/events';
import { ThemeProvider } from './lib/theme';
import { ToastProvider } from './lib/toast';
import { usePageTitle } from './lib/title';
import { ROUTES } from './routes';

function LegacyFallback({ to, label }) {
  const location = useLocation();
  usePageTitle(label);

  useEffect(() => {
    window.location.replace(`${to}${location.search}${location.hash}`);
  }, [to, location.search, location.hash]);

  return (
    <div className="page-head">
      <div className="page-head__text">
        <h1>{label}</h1>
        <p>
          Loading legacy page (<a href={to}>{to}</a>)...
        </p>
      </div>
    </div>
  );
}

function NotFoundPage() {
  usePageTitle('Not found');
  return (
    <div className="stack">
      <div className="page-head">
        <div className="page-head__text">
          <h1>Page not found</h1>
          <p>No panel view is registered for this path.</p>
        </div>
        <div className="page-head__actions">
          <Link className="btn btn--primary" to="/machines">
            Back to Machines
          </Link>
        </div>
      </div>
    </div>
  );
}

export function App() {
  return (
    <BrowserRouter>
      <ThemeProvider>
        <ToastProvider>
          <EventsProvider>
            <Routes>
              <Route element={<Shell />}>
                <Route path="/" element={<Navigate to="/machines" replace />} />
                <Route path="/index.html" element={<Navigate to="/machines" replace />} />
                {ROUTES.map((r) => {
                  const Page = r.component;
                  return (
                    <Route
                      key={r.id}
                      path={r.path}
                      element={Page ? <Page /> : <LegacyFallback to={r.legacyPath} label={r.label} />}
                    />
                  );
                })}
                <Route path="*" element={<NotFoundPage />} />
              </Route>
            </Routes>
          </EventsProvider>
        </ToastProvider>
      </ThemeProvider>
    </BrowserRouter>
  );
}
