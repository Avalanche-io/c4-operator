// Levels: how much the HUD shows and offers the person using it, in any
// mode (mode.js), view and role (docs/intro/levels.md).
//
//   1 Day one   one row per job: its state in a word, one bar, ready by and
//               due; what needs you first, as root causes with their steps
//   2 Working   frames, stages and the plan as a render, time, the unit card,
//               the controls of a working day
//   3 Expert    everything: the farm's machinery, cost per frame, C4 IDs and
//               provenance, native names on the "how" line, every control
//
// The level comes from the link (?proficiency=N, this page only), else the
// person's choice for this view (levelByView), else their choice, else the
// role's default. Depth is never locked: a peek shows the next level for one
// thing, and the switch is always in the top bar.
//
// Advancement (the rule: "Point it out once, then leave the door open"): each
// level is offered at most once per person, ever, when their use suggests
// it. Taken or declined, it is never offered again. No badges, no streaks,
// no progress toward the next level.

import * as prefs from './prefs.js';

export const LEVELS = [
  { n: 1, name: 'Day one', q: 'Get today’s work done: what is running, when it will be ready, what needs you.' },
  { n: 2, name: 'Working', q: 'Frames, stages, time and the controls of a working day.' },
  { n: 3, name: 'Expert', q: 'Everything: the farm’s machinery, cost, provenance and every control.' },
];
export const MAX = 3;

// Defaults by role (approved): artist, supervisor, producer and facilities
// start at 1; wrangler, farm admin and pipeline TD at 2. Without a role, a
// viewer of every project is a wrangler and anyone else an artist.
const ROLE = { artist: 1, supervisor: 1, producer: 1, facilities: 1, wrangler: 2, 'farm-admin': 2, admin: 2, 'pipeline-td': 2, td: 2 };
export function roleOf(viewer) {
  return (viewer && (viewer.role || (viewer.allProjects ? 'wrangler' : 'artist'))) || 'artist';
}
export function roleDefault(viewer) {
  const r = ROLE[roleOf(viewer)];
  return r != null ? r : 1;
}

const clamp = (n) => Math.max(1, Math.min(MAX, Math.round(n)));
const user = (viewer) => (viewer && viewer.user) || '';

export function fromLink() {
  try {
    const v = new URLSearchParams(location.search).get('proficiency');
    return v == null || v === '' || !Number.isFinite(Number(v)) ? null : clamp(Number(v));
  } catch { return null; }
}

// current: the level to draw this view at, where it came from, and the
// person's own level for it
export function current(viewer, view) {
  const byView = prefs.get(user(viewer), 'levelByView') || {};
  const own = prefs.get(user(viewer), 'level');
  const mine = byView[view] != null ? byView[view] : own != null ? own : roleDefault(viewer);
  const source = byView[view] != null ? 'view' : own != null ? 'stored' : 'role';
  const link = fromLink();
  return link != null ? { level: link, source: 'link', mine } : { level: mine, source, mine };
}

// choose: n becomes the person's level, everywhere or for one view only
export function choose(viewer, n, view = null) {
  const u = user(viewer);
  if (view) {
    const byView = { ...(prefs.get(u, 'levelByView') || {}) };
    if (n == null) delete byView[view]; else byView[view] = clamp(n);
    prefs.set(u, 'levelByView', Object.keys(byView).length ? byView : null);
  } else prefs.set(u, 'level', n == null ? null : clamp(n));
  try {
    const p = new URLSearchParams(location.search);
    if (p.has('proficiency')) {
      p.delete('proficiency');
      const qs = p.toString();
      history.replaceState(null, '', `${location.pathname}${qs ? '?' + qs : ''}${location.hash}`);
    }
  } catch { /* no location: tests */ }
}

// ------------------------------------------------------------ use, and the one offer

// The HUD counts a few things a person does, on their own machine only, to
// notice when the next level would help. Nothing leaves the browser.
const store = {
  get(k) { try { return JSON.parse(localStorage.getItem(k) || 'null'); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch { /* private mode */ } },
};
const ukey = (viewer) => `c4hud:use:${user(viewer) || 'anyone'}`;
const okey = (viewer, n) => `c4hud:offer-level:${user(viewer) || 'anyone'}:${n}`;

// used: count one use of a kind (peek, unit, approve-link, …)
export function used(viewer, kind) {
  const u = store.get(ukey(viewer)) || {};
  u[kind] = (u[kind] || 0) + 1;
  store.set(ukey(viewer), u);
}
export const uses = (viewer) => store.get(ukey(viewer)) || {};

// offered: whether level n has been offered to this person (ever)
export const offered = (viewer, n) => !!store.get(okey(viewer, n));
// settle: the offer of level n was shown and answered; it never comes back
export function settle(viewer, n, answer) { store.set(okey(viewer, n), { answer, at: Date.now() }); }

// offer: the one offer to show now, or null. Level 2 is offered after the
// person has peeked past level 1 ten times; level 3 after they have opened
// expert detail by link or peek ten times. Once per level, ever.
export function offer(viewer, level) {
  if (level >= MAX) return null;
  const next = level + 1;
  if (offered(viewer, next)) return null;
  const u = uses(viewer);
  if (next === 2 && (u.peek || 0) >= 10) {
    return { level: 2, title: 'You often open the frames', msg: 'Level 2 shows frames, stages and time on every job. You can come back to level 1 any time.', yes: 'Use level 2' };
  }
  if (next === 3 && (u.expert || 0) >= 10) {
    return { level: 3, title: 'You often reach for the details', msg: 'Level 3 shows everything: the farm’s machinery, cost and provenance. You can come back any time.', yes: 'Use level 3' };
  }
  return null;
}
