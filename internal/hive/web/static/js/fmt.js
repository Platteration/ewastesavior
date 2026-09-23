// Formatting helpers (pure; no DOM).

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB'];

// bytes formats a byte count with 1024-based units.
export function bytes(n) {
  n = Number(n) || 0;
  let i = 0;
  while (Math.abs(n) >= 1024 && i < UNITS.length - 1) {
    n /= 1024;
    i++;
  }
  return (i === 0 || Math.abs(n) >= 100 ? Math.round(n) : Math.round(n * 10) / 10) + ' ' + UNITS[i];
}

// mb formats a size given in MB (the protocol's unit for memory and disk).
export function mb(n) {
  return bytes((Number(n) || 0) * 1048576);
}

// num formats a number with at most d decimals.
export function num(n, d) {
  const f = Math.pow(10, d || 0);
  return String(Math.round((Number(n) || 0) * f) / f);
}

// zeroTime reports whether an RFC 3339 string is missing or Go's zero time.
export function zeroTime(s) {
  return !s || /^0001-01-01/.test(s);
}

// time formats an RFC 3339 timestamp in the browser's locale, or '—'.
export function time(s) {
  if (zeroTime(s)) return '—';
  const d = new Date(s);
  return isNaN(d.getTime()) ? String(s) : d.toLocaleString();
}

// dur formats seconds as a short duration ("3 m 20 s").
export function dur(s) {
  s = Math.max(0, Math.round(Number(s) || 0));
  if (s < 60) return s + ' s';
  if (s < 3600) return Math.floor(s / 60) + ' m ' + (s % 60) + ' s';
  if (s < 86400) return Math.floor(s / 3600) + ' h ' + Math.floor((s % 3600) / 60) + ' m';
  return Math.floor(s / 86400) + ' d ' + Math.floor((s % 86400) / 3600) + ' h';
}

// ago formats how long before now an RFC 3339 timestamp was.
export function ago(s, now) {
  if (zeroTime(s)) return 'never';
  const t = new Date(s).getTime();
  if (isNaN(t)) return String(s);
  const d = ((now || Date.now()) - t) / 1000;
  if (d < -5) return 'in ' + dur(-d);
  if (d < 5) return 'just now';
  return dur(d) + ' ago';
}

// plural returns "1 node" / "3 nodes".
export function plural(n, word, many) {
  return n + ' ' + (n === 1 ? word : (many || word + 's'));
}

// shortHash abbreviates a SHA-256 hex digest.
export function shortHash(s) {
  s = String(s || '');
  return s.length > 16 ? s.slice(0, 12) + '…' : s;
}

// pct formats a 0-100 value.
export function pct(n) {
  return Math.round(Number(n) || 0) + '%';
}
