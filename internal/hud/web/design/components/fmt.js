// Formatting in the viewer's clock. The farm runs UTC internally and the
// HUD API sends every time as RFC 3339 in UTC ("Z"); nothing here reads an
// offset from a time string. The presentation layer shows times in the
// viewer's zone: the browser's own, or ?tz=<IANA zone> on the link
// (captures pin one so they come out the same on any machine). utc() is the small UTC complication shown beside the local
// time on the clocks; siteClock() formats in a site's own zone where a site
// is the subject.

let nowMs = Date.now();

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const MINUS = '−';

const p2 = (n) => String(n).padStart(2, '0');

// ------------------------------------------------------------ the zone

function validZone(z) {
  try { new Intl.DateTimeFormat('en-US', { timeZone: z }); return true; } catch { return false; }
}

function browserZone() {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'; } catch { return 'UTC'; }
}

function linkZone() {
  try {
    const z = new URLSearchParams(globalThis.location ? globalThis.location.search : '').get('tz');
    return z && validZone(z) ? z : '';
  } catch { return ''; }
}

let zone = linkZone() || browserZone();

// setZone sets the presentation zone (an IANA name); an empty or unknown
// zone falls back to the link's, then the browser's.
export function setZone(z) {
  zone = z && validZone(z) ? z : linkZone() || browserZone();
  return zone;
}
export const tz = () => zone;

// offsets are read from Intl once per zone and quarter hour
const offFmt = new Map();
const offMemo = new Map();
function offsetAt(t, z = zone) {
  const key = `${z}|${Math.floor(t / 9e5)}`;
  let o = offMemo.get(key);
  if (o !== undefined) return o;
  let F = offFmt.get(z);
  if (!F) {
    F = new Intl.DateTimeFormat('en-US', { timeZone: z, hourCycle: 'h23', year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', second: 'numeric' });
    offFmt.set(z, F);
  }
  const P = {};
  for (const x of F.formatToParts(t)) P[x.type] = x.value;
  const wall = Date.UTC(+P.year, +P.month - 1, +P.day, +P.hour % 24, +P.minute, +P.second);
  o = Math.round((wall - Math.floor(t / 1000) * 1000) / 60000);
  if (offMemo.size > 4000) offMemo.clear();
  offMemo.set(key, o);
  return o;
}

// wall returns a Date whose UTC fields read as the wall clock in zone z.
function wall(t, z = zone) {
  return new Date(t + offsetAt(t, z) * 60000);
}

function utcLabel(off) {
  if (!off) return 'UTC';
  const a = Math.abs(off);
  return `UTC${off < 0 ? MINUS : '+'}${Math.floor(a / 60)}${a % 60 ? ':' + p2(a % 60) : ''}`;
}

// a zone's short name: PDT, EDT, BST, AEDT; UTC±h where no locale names it
const abbrFmt = new Map();
function abbrAt(t, z) {
  for (const loc of ['en-US', 'en-GB', 'en-AU']) {
    const key = loc + '|' + z;
    let F = abbrFmt.get(key);
    if (!F) { F = new Intl.DateTimeFormat(loc, { timeZone: z, timeZoneName: 'short' }); abbrFmt.set(key, F); }
    const p = F.formatToParts(t).find((x) => x.type === 'timeZoneName');
    if (p && !/^(GMT|UTC)[+−-]/.test(p.value)) return p.value;
  }
  return utcLabel(offsetAt(t, z));
}

// ------------------------------------------------------------ the clock

// setClock sets now from the snapshot's time.
export function setClock(iso) {
  nowMs = Date.parse(iso);
}

export const now = () => nowMs;
export const ms = (iso) => (iso ? Date.parse(iso) : NaN);
const at = (iso) => (typeof iso === 'number' ? iso : ms(iso));

function hm(d) { return `${p2(d.getUTCHours())}:${p2(d.getUTCMinutes())}`; }

// ------------------------------------------------------------ display options

// The clock is 24-hour or 12-hour: the viewer's preference, "auto" from the
// browser's locale, or ?clock=24 / ?clock=12 on the link (captures pin
// 24). The UTC complication can be turned off (?utc=0). Machine forms
// (inputValue, fromInput) and the UTC complication itself stay 24-hour.
function linkParam(k) {
  try { return new URLSearchParams(globalThis.location ? globalThis.location.search : '').get(k); } catch { return null; }
}
function localeHour12() {
  try {
    const hc = new Intl.DateTimeFormat(undefined, { hour: 'numeric' }).resolvedOptions().hourCycle;
    return hc === 'h11' || hc === 'h12';
  } catch { return false; }
}
let hour12 = false, showUTC = true;
// setClockStyle: 'auto', '24' or '12'; the link's ?clock= wins.
export function setClockStyle(pref = 'auto') {
  const v = linkParam('clock') || pref;
  hour12 = v === '12' ? true : v === '24' ? false : localeHour12();
  return hour12 ? '12' : '24';
}
// setShowUTC: whether the UTC complication shows; the link's ?utc=0|1 wins.
export function setShowUTC(on = true) {
  const v = linkParam('utc');
  showUTC = v === '0' ? false : v === '1' ? true : !!on;
  return showUTC;
}
export const clockStyle = () => (hour12 ? '12' : '24');
setClockStyle();
setShowUTC();

// shown: hours and minutes as the viewer reads them: 14:20 or 2:20 PM
function shown(d, sec = false) {
  const s = sec ? ':' + p2(d.getUTCSeconds()) : '';
  if (!hour12) return `${hm(d)}${s}`;
  const h = d.getUTCHours();
  return `${h % 12 || 12}:${p2(d.getUTCMinutes())}${s} ${h < 12 ? 'AM' : 'PM'}`;
}

// clock: 14:20, or Thu 14:20 with the day, in the viewer's zone.
export function clock(iso, withDay = true) {
  const t = at(iso);
  if (!Number.isFinite(t)) return '';
  const d = wall(t);
  return withDay ? `${DAYS[d.getUTCDay()]} ${shown(d)}` : shown(d);
}

export function clockSec(iso) {
  const t = at(iso);
  if (!Number.isFinite(t)) return '';
  const d = wall(t);
  return shown(d, true);
}

export function dateLine(iso) {
  const t = at(iso);
  if (!Number.isFinite(t)) return '';
  const d = wall(t);
  return `${DAYS[d.getUTCDay()]} ${d.getUTCDate()} ${MONTHS[d.getUTCMonth()]}`;
}

// inputStamp is a time the way a US browser's date-time field shows it, in
// the viewer's zone: 10/22/2026, 03:00 PM.
export function inputStamp(iso) {
  const t = at(iso);
  if (!Number.isFinite(t)) return '';
  const d = wall(t);
  const h = d.getUTCHours();
  return `${p2(d.getUTCMonth() + 1)}/${p2(d.getUTCDate())}/${d.getUTCFullYear()}, ${p2(h % 12 || 12)}:${p2(d.getUTCMinutes())} ${h < 12 ? 'AM' : 'PM'}`;
}

// zoneAbbr names the viewer's zone at an instant (now by default): PDT.
export function zoneAbbr(iso) {
  const t = iso === undefined ? nowMs : at(iso);
  return abbrAt(Number.isFinite(t) ? t : Date.now(), zone);
}

// offsetLabel names the viewer's zone for the clock's caption: PDT.
export function offsetLabel(iso) {
  return zoneAbbr(iso);
}

// utc is the complication: the same instant in UTC, small, beside the local
// time on the clocks: 21:20Z, or 21:20:31Z with seconds. Empty when the
// viewer's zone is UTC at that instant, where it would only repeat itself.
export function utc(iso, sec = false) {
  const t = at(iso);
  if (!showUTC || !Number.isFinite(t) || offsetAt(t) === 0) return '';
  const d = new Date(t);
  return `${hm(d)}${sec ? ':' + p2(d.getUTCSeconds()) : ''}Z`;
}

// siteClock formats in a site's own zone, for where a site is the subject:
// 05:20 BST. Empty when the site names no zone or an unknown one.
export function siteClock(iso, siteZone, withDay = false) {
  const t = at(iso);
  if (!Number.isFinite(t) || !siteZone || !validZone(siteZone)) return '';
  const d = wall(t, siteZone);
  return `${withDay ? DAYS[d.getUTCDay()] + ' ' : ''}${shown(d)} ${abbrAt(t, siteZone)}`;
}

// inputValue is a time as a datetime-local field holds it, in the viewer's
// zone: 2026-10-22T15:00.
export function inputValue(iso) {
  const t = at(iso);
  if (!Number.isFinite(t)) return '';
  const d = wall(t);
  return `${d.getUTCFullYear()}-${p2(d.getUTCMonth() + 1)}-${p2(d.getUTCDate())}T${hm(d)}`;
}

// fromInput reads a datetime-local value as the viewer's wall clock and
// returns the instant as RFC 3339 in UTC, the way the API takes times:
// 2026-10-22T22:00:00Z. Empty when the value does not parse.
export function fromInput(v) {
  const m = /^(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d)(?::(\d\d))?/.exec(v || '');
  if (!m) return '';
  const guess = Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +(m[6] || 0));
  let t = guess - offsetAt(guess) * 60000;
  t = guess - offsetAt(t) * 60000; // once more, across a change of offset
  return new Date(t).toISOString().replace(/\.\d{3}Z$/, 'Z');
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

// gridRef prints a C4 ID as a grid reference: the opening and closing groups
// of four, the full ID left for hover (SURVEY: an ID is a coordinate).
export function gridRef(id) {
  if (!id) return '';
  return id.length > 16 ? `${id.slice(0, 4)} ${id.slice(4, 8)} … ${id.slice(-4)}` : id;
}

// tminus prints a time as mission time to or since now: T-02:06:24, T+00:03:10.
export function tminus(iso) {
  const t = typeof iso === 'number' ? iso : ms(iso);
  if (!Number.isFinite(t)) return '';
  let d = Math.round((t - nowMs) / 1000);
  const sign = d >= 0 ? MINUS : '+';
  d = Math.abs(d);
  return `T${sign}${p2(Math.floor(d / 3600))}:${p2(Math.floor((d % 3600) / 60))}:${p2(d % 60)}`;
}

// approx prints an estimate the way pencil is written: about 5 min, about 2h46.
export function approx(sec) {
  if (!Number.isFinite(sec)) return '';
  const d = dur(sec);
  return '≈' + (/^\d+m$/.test(d) ? d.replace('m', ' min') : d);
}
