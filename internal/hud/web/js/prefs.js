// Preferences: one store for every setting a viewer can choose.
//
// The schema below is the whole contract: each preference has a type, a
// default and the words the Preferences sheet shows. Values are validated
// against it on the way in and on the way out, so a stored value from an
// older or newer HUD can never break a page; an unknown key is kept (a
// newer HUD may own it) and an invalid value falls back to the default.
//
// Where values live is an adapter. Today it is the browser (localStorage,
// per user). Later the server keeps them per viewer (GET and PUT
// /api/v1/prefs), so a wrangler's settings follow them from desk to wall;
// the store then reads the server's copy and writes through to both.
//
// A link can carry a preference for that page only, without changing the
// stored one: ?level=, ?tz=, ?clock=, ?utc=, ?scheme=. Captures pin them.

export const VERSION = 1;

// The zones the sheet offers first; any IANA zone can be typed.
export const ZONES = [
  'America/Los_Angeles', 'America/Denver', 'America/Chicago', 'America/New_York', 'America/Toronto', 'America/Vancouver',
  'America/Sao_Paulo', 'Europe/London', 'Europe/Paris', 'Europe/Berlin', 'Asia/Kolkata', 'Asia/Shanghai', 'Asia/Tokyo',
  'Asia/Seoul', 'Australia/Sydney', 'Pacific/Auckland', 'UTC',
];

export const SCHEMA = {
  level: {
    group: 'Detail', type: 'int', min: 0, max: 5, default: null, label: 'Detail level',
    help: 'How much of the HUD you see. Empty: the level for your role.', link: 'level',
  },
  clock: {
    group: 'Time', type: 'enum', values: ['auto', '24', '12'], default: 'auto', label: 'Clock',
    names: { auto: 'From your browser', 24: '24-hour', 12: '12-hour' }, link: 'clock',
  },
  utc: {
    group: 'Time', type: 'bool', default: true, label: 'Show UTC beside the clocks',
    help: 'The farm runs on UTC. A small UTC time sits beside the local time on clocks, the time base and deadline labels.', link: 'utc',
  },
  zone: {
    group: 'Time', type: 'zone', default: '', label: 'Time zone',
    help: 'Times show in this zone. Empty: your browser’s.', link: 'tz',
  },
  scheme: {
    group: 'Appearance', type: 'enum', values: ['system', 'dark', 'light'], default: 'system', label: 'Scheme',
    names: { system: 'Follow the system', dark: 'Dark', light: 'Light' }, link: 'scheme',
  },
  // Reserved, so stored values and the server's schema leave room for them:
  // motion (reduced), vision (color-vision variants), density, sounds,
  // notifications.
};

export const GROUPS = ['Detail', 'Time', 'Appearance'];

function validZone(z) {
  try { new Intl.DateTimeFormat('en-US', { timeZone: z }); return true; } catch { return false; }
}

// valid returns v if it fits the preference's type, else undefined
export function valid(key, v) {
  const S = SCHEMA[key];
  if (!S) return v;
  if (v === null || v === undefined) return S.default === null ? null : undefined;
  switch (S.type) {
    case 'int': { const n = Number(v); return Number.isInteger(n) && n >= S.min && n <= S.max ? n : undefined; }
    case 'enum': return S.values.includes(String(v)) ? String(v) : undefined;
    case 'bool': return v === true || v === false ? v : v === 'true' || v === '1' ? true : v === 'false' || v === '0' ? false : undefined;
    case 'zone': return v === '' || (typeof v === 'string' && validZone(v)) ? v : undefined;
    default: return undefined;
  }
}

// ------------------------------------------------------------ storage

// The browser adapter: one JSON record per user, { v, values }.
export const browserStore = {
  read(user) {
    try { return JSON.parse(localStorage.getItem(keyOf(user)) || 'null'); } catch { return null; }
  },
  write(user, rec) {
    try { localStorage.setItem(keyOf(user), JSON.stringify(rec)); } catch { /* private mode: this page only */ }
  },
};
const keyOf = (user) => `c4hud:prefs:${user || 'anyone'}`;

let adapter = browserStore;
// useStore swaps the adapter (the server's, later; a fake in tests).
export function useStore(a) { adapter = a; loaded = null; }

let loaded = null; // { user, values }
const listeners = new Set();

function load(user) {
  if (loaded && loaded.user === (user || 'anyone')) return loaded;
  const rec = adapter.read(user) || { v: VERSION, values: {} };
  const values = { ...(rec.values || {}) };
  // before preferences, the detail level lived on its own key
  if (values.level === undefined && adapter === browserStore) {
    try {
      const old = localStorage.getItem(`c4hud:level:${user || 'anyone'}`);
      if (old != null && valid('level', Number(old)) !== undefined) values.level = Number(old);
    } catch { /* no storage */ }
  }
  loaded = { user: user || 'anyone', values };
  return loaded;
}

// get: the stored value, or the default
export function get(user, key) {
  const v = valid(key, load(user).values[key]);
  return v === undefined ? SCHEMA[key].default : v;
}

// effective: what applies on this page: the link's value wins
export function effective(user, key) {
  const S = SCHEMA[key];
  if (S && S.link) {
    try {
      const raw = new URLSearchParams(location.search).get(S.link);
      if (raw !== null && raw !== '') {
        const v = valid(key, key === 'level' ? Number(raw) : raw);
        if (v !== undefined && v !== null) return { value: v, source: 'link' };
      }
    } catch { /* no location: tests */ }
  }
  const stored = valid(key, load(user).values[key]);
  return stored === undefined || stored === null ? { value: S.default, source: 'default' } : { value: stored, source: 'stored' };
}

// set: store a value (null or undefined resets it to the default)
export function set(user, key, v) {
  const rec = load(user);
  if (v === null || v === undefined || v === SCHEMA[key].default) delete rec.values[key];
  else {
    const ok = valid(key, v);
    if (ok === undefined) throw new Error(`${SCHEMA[key].label}: ${v} is not a valid value`);
    rec.values[key] = ok;
  }
  adapter.write(user, { v: VERSION, values: rec.values });
  for (const fn of listeners) fn(key, get(user, key));
}

export function reset(user) {
  const rec = load(user);
  for (const k of Object.keys(SCHEMA)) delete rec.values[k];
  adapter.write(user, { v: VERSION, values: rec.values });
  for (const fn of listeners) fn(null, null);
}

export function onChange(fn) { listeners.add(fn); return () => listeners.delete(fn); }
