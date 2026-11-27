const UNITS = ['kB', 'MB', 'GB', 'TB', 'PB'];

export function fmtBytes(n) {
  if (n == null) return '';
  if (n < 1000) return `${n} B`;
  let v = n;
  let i = -1;
  do {
    v /= 1000;
    i++;
  } while (v >= 1000 && i < UNITS.length - 1);
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${UNITS[i]}`;
}

export function humanBytes(n) {
  if (n == null) return '';
  if (n < 1000) return `${n} B`;
  let v = n;
  let i = -1;
  do {
    v /= 1000;
    i++;
  } while (v >= 1000 && i < UNITS.length - 1);
  return `${v.toFixed(0)} ${UNITS[i]}`;
}

export function fmtAgo(iso) {
  if (!iso) return 'never';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return 'never';
  const s = Math.max(0, (Date.now() - d.getTime()) / 1000);
  if (s < 2) return 'just now';
  if (s < 60) return `${Math.floor(s)} s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 172800) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}

export function fmtTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

export function fmtDur(ms) {
  if (ms == null) return '';
  let s = ms / 1000;
  if (s < 10) return `${s.toFixed(1)} s`;
  s = Math.round(s);
  const m = Math.floor(s / 60);
  return m ? `${m}:${String(s % 60).padStart(2, '0')}` : `${s} s`;
}

// Disk guard thresholds (architecture 6.5, 7.3).
const DISK_WARN_PCT = 80;
const DISK_CRITICAL_PCT = 90;

export function diskView(disk) {
  if (!disk || !disk.total_bytes || disk.free_bytes > disk.total_bytes) {
    return null;
  }
  const used = disk.total_bytes - disk.free_bytes;
  const pct = Math.floor((used * 100) / disk.total_bytes);
  let cls = '';
  let word = '';
  if (pct >= DISK_CRITICAL_PCT) {
    cls = 'danger';
    word = 'intake paused';
  } else if (pct >= DISK_WARN_PCT) {
    cls = 'warn';
    word = 'warning';
  }
  return {
    pct,
    used: humanBytes(used),
    total: humanBytes(disk.total_bytes),
    className: cls,
    word,
  };
}
