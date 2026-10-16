// Formatting in the farm's own clock. Times arrive as RFC 3339 with the
// farm's offset; the HUD shows them in that offset, whatever the browser's
// time zone, so a wall display and a laptop agree.

let offsetMin = 0;
let nowMs = Date.now();

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const MINUS = '−';

export function setClock(iso) {
  const m = /([+-])(\d\d):(\d\d)$/.exec(iso || '');
  offsetMin = m ? (m[1] === '-' ? -1 : 1) * (Number(m[2]) * 60 + Number(m[3])) : 0;
  nowMs = Date.parse(iso);
}

export const now = () => nowMs;
export const ms = (iso) => (iso ? Date.parse(iso) : NaN);

function local(t) {
  return new Date(t + offsetMin * 60000);
}

const p2 = (n) => String(n).padStart(2, '0');

export function clock(iso, withDay = true) {
  const t = typeof iso === 'number' ? iso : ms(iso);
  if (!Number.isFinite(t)) return '';
  const d = local(t);
  const hm = `${p2(d.getUTCHours())}:${p2(d.getUTCMinutes())}`;
  if (!withDay) return hm;
  const today = local(nowMs).getUTCDate() === d.getUTCDate() && Math.abs(t - nowMs) < 864e5;
  return today ? `${DAYS[d.getUTCDay()]} ${hm}` : `${DAYS[d.getUTCDay()]} ${hm}`;
}

export function clockSec(iso) {
  const d = local(ms(iso));
  return `${p2(d.getUTCHours())}:${p2(d.getUTCMinutes())}:${p2(d.getUTCSeconds())}`;
}

export function dateLine(iso) {
  const d = local(ms(iso));
  return `${DAYS[d.getUTCDay()]} ${d.getUTCDate()} ${MONTHS[d.getUTCMonth()]}`;
}

export function offsetLabel() {
  const sign = offsetMin < 0 ? MINUS : '+';
  const a = Math.abs(offsetMin);
  return `UTC${sign}${p2(Math.floor(a / 60))}:${p2(a % 60)}`;
}

// dur formats seconds as 45s, 12m, 2h05, 3d 4h.
export function dur(sec) {
  if (!Number.isFinite(sec)) return '';
  const a = Math.abs(Math.round(sec));
  if (a < 60) return `${a}s`;
  if (a < 3600) return `${Math.round(a / 60)}m`;
  if (a >= 86400 && a < 72 * 3600) return `${Math.round(a / 3600)}h`;
  if (a < 86400) {
    const h = Math.floor(a / 3600);
    const m = Math.round((a % 3600) / 60);
    return m === 60 ? `${h + 1}h00` : `${h}h${p2(m)}`;
  }
  const d = Math.floor(a / 86400);
  return `${d}d ${Math.round((a % 86400) / 3600)}h`;
}

export function signed(sec) {
  if (!Number.isFinite(sec)) return '';
  if (Math.abs(sec) < 30) return '0m';
  return (sec < 0 ? MINUS : '+') + dur(sec);
}

export function num(n) {
  return Number.isFinite(n) ? Math.round(n).toLocaleString('en-US') : '';
}

export function bytes(n) {
  if (!Number.isFinite(n)) return '';
  const u = ['B', 'kB', 'MB', 'GB', 'TB', 'PB', 'EB'];
  let i = 0;
  let v = n;
  while (v >= 1000 && i < u.length - 1) { v /= 1000; i++; }
  return `${v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${u[i]}`;
}

export function money(n, cents = false) {
  if (!Number.isFinite(n)) return '';
  return '$' + (cents ? n.toFixed(2) : Math.round(n).toLocaleString('en-US'));
}

export function pct(f, digits = 1) {
  return Number.isFinite(f) ? `${(f * 100).toFixed(digits)}%` : '';
}

export function shortId(id) {
  if (!id) return '';
  return id.length > 14 ? id.slice(0, 12) + '…' : id;
}

export function jobName(key) {
  const i = key.indexOf('/');
  return i < 0 ? key : key.slice(i + 1);
}
