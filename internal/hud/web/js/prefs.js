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
// stored one: ?mode=, ?proficiency=, ?tz=, ?clock=, ?utc=, ?scheme=.
// Captures pin them.

export const VERSION = 1;

// The zones the sheet offers first; any IANA zone can be typed.
export const ZONES = [
  'America/Los_Angeles', 'America/Denver', 'America/Chicago', 'America/New_York', 'America/Toronto', 'America/Vancouver',
  'America/Sao_Paulo', 'Europe/London', 'Europe/Paris', 'Europe/Berlin', 'Asia/Kolkata', 'Asia/Shanghai', 'Asia/Tokyo',
  'Asia/Seoul', 'Australia/Sydney', 'Pacific/Auckland', 'UTC',
];

export const SCHEMA = {
  level: {
    group: 'Level', type: 'int', min: 1, max: 3, default: null, label: 'Level',
    help: 'How much the HUD shows and offers: 1 day one, 2 working, 3 everything. Empty: the level for your role.', link: 'proficiency',
  },
  levelByView: {
    group: 'Level', type: 'map', of: { type: 'int', min: 1, max: 3 }, default: null, label: 'Level, by view',
    help: 'A different level for one view, such as 3 in the job view and 1 everywhere else.',
  },
  mode: {
    group: 'Mode', type: 'int', min: 0, max: 5, default: null, label: 'Mode',
    help: 'Which question the HUD answers first: Queue, Frames, Time, Shots, Farm or Full. Empty: the mode for your role.', link: 'mode',
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
  // the HUD channel of policy's escalation.notify: a question to you that
  // turns pressing is sent as a browser notification while the HUD is open
  notifyPressing: {
    group: 'Notifications', type: 'bool', default: false, label: 'Tell me when a question to me turns pressing',
    help: 'An agent or a unit stopped for an answer only you can give, and it now costs a deadline or has waited too long. The browser shows it once, while the HUD is open; your browser asks first.',
  },
  // Reserved, so stored values and the server's schema leave room for them:
  // motion (reduced), vision (color-vision variants), density, sounds.
};

export const GROUPS = ['Level', 'Mode', 'Time', 'Appearance', 'Notifications'];

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
    case 'map': {
      // a map from a name to a value of type `of`; bad entries are dropped
      if (typeof v !== 'object' || Array.isArray(v)) return undefined;
      const out = {};
      for (const [k, x] of Object.entries(v)) {
        const n = Number(x);
        if (/^[a-z][a-z0-9-]*$/.test(k) && Number.isInteger(n) && n >= S.of.min && n <= S.of.max) out[k] = n;
      }
      return Object.keys(out).length ? out : null;
    }
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
      const q = new URLSearchParams(location.search);
      const raw = q.get(S.link);
      if (raw !== null && raw !== '') {
        const v = valid(key, S.type === 'int' ? Number(raw) : raw);
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
