// Modes: which question about the work the screen answers. The HUD adds one
// idea per mode, up to mode 5, which is the whole HUD (docs/intro/ladder.md).
// A mode is drawn at the viewer's level (proficiency, level.js): Farm mode
// at level 1 is "slots in use, and is anything wrong", not every slot.
//
//   0 Queue   my jobs: progress, time left, due
//   1 Frames  a job opens to its frames: the colors, binary order, preview, requeue
//   2 Time    deadline, estimate, slack, measured and estimated, at risk; hold, boost
//   3 Shots   a shot's jobs side by side, batches, stages, collapse, the graph
//   4 Farm    slots, pools, guaranteed shares, burst and approval, cost, the map
//   5 Full    C4 IDs, output views, the book, cache tiers, the wall, CI and ML views
//
// The mode comes from the link (?mode=N, for this page only), else the viewer's own choice (a preference, prefs.js), else
// the role's default. It is set on <html data-mode> so CSS can follow it.

import * as prefs from './prefs.js';

export const MODES = [
  { n: 0, name: 'Queue', q: 'What is running, and when will it be done?' },
  { n: 1, name: 'Frames', q: 'What is inside a job?' },
  { n: 2, name: 'Time', q: 'Will it make its deadline, and what can I do?' },
  { n: 3, name: 'Shots', q: 'How does work flow from job to job?' },
  { n: 4, name: 'Farm', q: 'What capacity is there, and what would more cost?' },
  { n: 5, name: 'Full', q: 'Where did this come from, and who did it?' },
];
export const MAX = 5;

// Default modes by role (approved): artist 1, supervisor and producer 2,
// wrangler 3, farm admin and facilities 4, pipeline TD 5. Until the API
// names a role, a viewer of every project is a wrangler and anyone else an
// artist.
const ROLE = { artist: 1, supervisor: 2, producer: 2, wrangler: 3, 'farm-admin': 4, admin: 4, facilities: 4, 'pipeline-td': 5, td: 5 };
export function roleDefault(viewer) {
  const role = viewer && (viewer.role || (viewer.allProjects ? 'wrangler' : 'artist'));
  return ROLE[role] != null ? ROLE[role] : 3;
}

const clampMode = (n) => Math.max(0, Math.min(MAX, Math.round(n)));
const user = (viewer) => (viewer && viewer.user) || '';

export function stored(viewer) {
  const v = prefs.get(user(viewer), 'mode');
  return v == null ? null : clampMode(v);
}

// fromLink: ?mode=N
export function fromLink() {
  const p = new URLSearchParams(location.search);
  const v = p.get('mode');
  return v == null || v === '' || !Number.isFinite(Number(v)) ? null : clampMode(Number(v));
}

// current: the mode to draw, where it came from, and the viewer's own mode
export function current(viewer) {
  const mine = stored(viewer) ?? roleDefault(viewer);
  const link = fromLink();
  return link != null ? { mode: link, source: 'link', mine } : { mode: mine, source: stored(viewer) != null ? 'stored' : 'role', mine };
}

// choose makes n the viewer's mode, and drops a link's mode from the URL
// so the choice shows at once.
export function choose(viewer, n) {
  prefs.set(user(viewer), 'mode', clampMode(n));
  const p = new URLSearchParams(location.search);
  if (p.has('mode')) {
    p.delete('mode');
    const qs = p.toString();
    history.replaceState(null, '', `${location.pathname}${qs ? '?' + qs : ''}${location.hash}`);
  }
}

// keep: the link's mode becomes the viewer's own
export const keep = (viewer) => { const l = fromLink(); if (l != null) choose(viewer, l); };

// href with a mode, for docs and tutorials that link straight to one
export function withMode(n) {
  const p = new URLSearchParams(location.search);
  p.set('mode', String(n));
  return `${location.pathname}?${p.toString()}${location.hash}`;
}

// ------------------------------------------------------------ offers

// "Show me more": one offer at a time, when the next mode answers a
// question the viewer now has. Each mode is offered once per person, ever
// (the rule: point it out once, then leave the door open).
const okey = (viewer, n) => `c4hud:offer-mode:${(viewer && viewer.user) || 'anyone'}:${n}`;
function silenced(viewer, n) {
  try {
    return !!JSON.parse(localStorage.getItem(okey(viewer, n)) || 'null');
  } catch { return false; }
}
export function notNow(viewer, n) {
  try {
    const v = JSON.parse(localStorage.getItem(okey(viewer, n)) || 'null') || { count: 0 };
    localStorage.setItem(okey(viewer, n), JSON.stringify({ count: v.count + 1, at: Date.now() }));
  } catch { /* nothing to remember with */ }
}

const seen = new Map(); // shot -> jobs opened this session
export function opened(job) {
  if (!job || !job.shot) return;
  const k = `${job.project}/${job.shot}`;
  if (!seen.has(k)) seen.set(k, new Set());
  seen.get(k).add(`${job.namespace}/${job.name}`);
}

// offer: the one to show now, or null
export function offer(snap, route, mode, viewer) {
  if (!snap || mode >= MAX) return null;
  const mine = (j) => viewer && (viewer.allProjects || j.owner === viewer.user || (viewer.projects || []).includes(j.project));
  const open = snap.jobs.filter((j) => j.phase !== 'Succeeded' && mine(j));
  const next = mode + 1;
  if (silenced(viewer, next)) return null;
  let o = null;
  if (mode === 0 && (route.name === 'job' || open.some((j) => j.units.failed > 0))) {
    o = { title: 'See the frames', msg: 'Frames mode shows each frame of a job as it renders, coarse to fine, and why one failed.', yes: 'Show frames' };
  } else if (mode === 1) {
    const late = open.find((j) => j.objective && j.objective.deadline && j.estimate.slackSeconds < 0);
    if (late) o = { title: `Will ${late.name} make its deadline?`, msg: 'It may miss it now. Time mode shows its deadline, its slack and what you can do about it.', yes: 'Show time' };
  } else if (mode === 2) {
    for (const s of seen.values()) if (s.size >= 3) { o = { title: 'These jobs are one shot', msg: 'Shots mode shows a shot’s jobs side by side, in the order the work flows.', yes: 'Show shots' }; break; }
  } else if (mode === 3) {
    if (open.some((j) => j.estimate && j.estimate.burst && j.estimate.burst.needsApproval)) o = { title: 'More capacity?', msg: 'A burst waits for approval. Farm mode shows the farm’s slots and what more would cost.', yes: 'Show the farm' };
  } else if (mode === 4) {
    if (route.name === 'job') { const j = snap.jobs.find((x) => `${x.namespace}/${x.name}` === route.parts.slice(0, 2).join('/')); if (j && j.output) o = { title: 'Where did this come from?', msg: 'Full mode shows each output by its C4 ID and every step signed in the book.', yes: 'Show everything' }; }
  }
  return o ? { ...o, mode: next } : null;
}
