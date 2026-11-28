import { BackupPage } from './pages/BackupPage';
import { BaselinePage } from './pages/BaselinePage';
import { CommandsPage } from './pages/CommandsPage';
import { EvidencePage } from './pages/EvidencePage';
import { MachinesPage } from './pages/MachinesPage';
import { ManeuverPage } from './pages/ManeuverPage';
import { MonitorPage } from './pages/MonitorPage';
import { PastebinPage } from './pages/PastebinPage';
import { ProjectsPage } from './pages/ProjectsPage';
import { TerminalPage } from './pages/TerminalPage';
import { WatchdogPage } from './pages/WatchdogPage';

/**
 * Route registry for all operator-panel pages.
 *
 * Contract for porting remaining pages (T-0048 - T-0051):
 *   1. Create `web/src/pages/<Name>Page.jsx` exporting the page component.
 *   2. Import it here and set `component: <Name>Page` on its entry in `ROUTES`.
 *
 * Entries with `component: null` are un-ported:
 *   - Sidebar renders a full-page link (`legacyPath`, e.g. `/projects.html`) so
 *     the panel remains usable during incremental migration.
 *   - Client router still registers `path` with a `<LegacyFallback />` redirect
 *     so deep-linking or refreshing `/projects` works seamlessly.
 */
export const NAV_GROUPS = ['Operate', 'Plan', 'Observe', 'Administer'];

export const ROUTES = [
  // Operate
  {
    id: 'machines',
    path: '/machines',
    legacyPath: '/machines.html',
    group: 'Operate',
    label: 'Machines',
    icon: 'i-server',
    component: MachinesPage,
  },
  {
    id: 'projects',
    path: '/projects',
    legacyPath: '/projects.html',
    group: 'Operate',
    label: 'Projects & tests',
    icon: 'i-folder',
    component: ProjectsPage,
  },
  {
    id: 'commands',
    path: '/commands',
    legacyPath: '/commands.html',
    group: 'Operate',
    label: 'Command runner',
    icon: 'i-bolt',
    component: CommandsPage,
  },
  {
    id: 'terminal',
    path: '/terminal',
    legacyPath: '/terminal.html',
    group: 'Operate',
    label: 'Terminal',
    icon: 'i-terminal',
    component: TerminalPage,
  },
  // Plan
  {
    id: 'baseline',
    path: '/baseline',
    legacyPath: '/baseline.html',
    group: 'Plan',
    label: 'Baseline items',
    icon: 'i-layers',
    component: BaselinePage,
  },
  {
    id: 'maneuver',
    path: '/maneuver',
    legacyPath: '/maneuver.html',
    group: 'Plan',
    label: 'Maneuver',
    icon: 'i-sliders',
    component: ManeuverPage,
  },
  // Observe
  {
    id: 'monitor',
    path: '/monitor',
    legacyPath: '/monitor.html',
    group: 'Observe',
    label: 'Monitor data',
    icon: 'i-activity',
    component: MonitorPage,
  },
  {
    id: 'watchdog',
    path: '/watchdog',
    legacyPath: '/watchdog.html',
    group: 'Observe',
    label: 'Watchdog',
    icon: 'i-shield',
    component: WatchdogPage,
  },
  {
    id: 'evidence',
    path: '/evidence',
    legacyPath: '/evidence.html',
    group: 'Observe',
    label: 'Evidence',
    icon: 'i-evidence',
    component: EvidencePage,
  },
  // Administer
  {
    id: 'backup',
    path: '/backup',
    legacyPath: '/backup.html',
    group: 'Administer',
    label: 'Backup',
    icon: 'i-database',
    component: BackupPage,
  },
  {
    id: 'pastebin',
    path: '/pastebin',
    group: 'Administer',
    label: 'Pastebin',
    icon: 'i-copy',
    component: PastebinPage,
  },
];

/**
 * Returns the active href for a route (clean SPA path if ported, legacyPath if not).
 * Useful for cross-page links like Terminal ?machine=<id>.
 */
export function routeHref(id, search = '') {
  const entry = ROUTES.find((r) => r.id === id);
  if (!entry) return `/${id}${search}`;
  const base = entry.component ? entry.path : entry.legacyPath;
  return `${base}${search}`;
}

export function isRoutePorted(id) {
  const entry = ROUTES.find((r) => r.id === id);
  return Boolean(entry && entry.component);
}
