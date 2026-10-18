// Detail levels: the HUD starts simple and adds one idea per level, up to
// level 5, which is the whole HUD (docs/intro/ladder.md).
//
//   0 Queue   my jobs: progress, time left, due
//   1 Frames  a job opens to its frames: the colors, binary order, preview, requeue
//   2 Time    deadline, estimate, slack, measured and estimated, at risk; hold, boost
//   3 Shots   a shot's jobs side by side, batches, stages, collapse, the graph
//   4 Farm    slots, pools, guaranteed shares, burst and approval, cost, the map
//   5 Full    C4 IDs, output views, the book, cache tiers, the wall, CI and ML views
//
// The level comes from the link (?level=N, for this page only), else the
// viewer's own choice (kept in the browser per user), else the role's
// default. It is set on <html data-level> so CSS can follow it.

export const LEVELS = [
  { n: 0, name: 'Queue', q: 'What is running, and when will it be done?' },
  { n: 1, name: 'Frames', q: 'What is inside a job?' },
  { n: 2, name: 'Time', q: 'Will it make its deadline, and what can I do?' },
  { n: 3, name: 'Shots', q: 'How does work flow from job to job?' },
  { n: 4, name: 'Farm', q: 'What capacity is there, and what would more cost?' },
  { n: 5, name: 'Full', q: 'Where did this come from, and who did it?' },
];
export const MAX = 5;

// Defaults by role (approved): artist 1, supervisor and producer 2,
// wrangler 3, farm admin and facilities 4, pipeline TD 5. Until the API
// names a role, a viewer of every project is a wrangler and anyone else an
// artist.
const ROLE = { artist: 1, supervisor: 2, producer: 2, wrangler: 3, 'farm-admin': 4, admin: 4, facilities: 4, 'pipeline-td': 5, td: 5 };
export function roleDefault(viewer) {
  const role = viewer && (viewer.role || (viewer.allProjects ? 'wrangler' : 'artist'));
  return ROLE[role] != null ? ROLE[role] : 3;
}

const clampLevel = (n) => Math.max(0, Math.min(MAX, Math.round(n)));
const key = (viewer) => `c4hud:level:${(viewer && viewer.user) || 'anyone'}`;

export function stored(viewer) {
  try {
    const v = localStorage.getItem(key(viewer));
    return v == null ? null : clampLevel(Number(v));
  } catch { return null; }
}

export function fromLink() {
  const v = new URLSearchParams(location.search).get('level');
  return v == null || v === '' || !Number.isFinite(Number(v)) ? null : clampLevel(Number(v));
}

// current: the level to draw, where it came from, and the viewer's own level
export function current(viewer) {
  const mine = stored(viewer) ?? roleDefault(viewer);
  const link = fromLink();
  return link != null ? { level: link, source: 'link', mine } : { level: mine, source: stored(viewer) != null ? 'stored' : 'role', mine };
}

// choose makes n the viewer's level, and drops a link's level from the URL
// so the choice shows at once.
export function choose(viewer, n) {
  try { localStorage.setItem(key(viewer), String(clampLevel(n))); } catch { /* private mode: the link carries it */ }
  const p = new URLSearchParams(location.search);
  if (p.has('level')) {
    p.delete('level');
    const qs = p.toString();
    history.replaceState(null, '', `${location.pathname}${qs ? '?' + qs : ''}${location.hash}`);
  }
}

// keep: the link's level becomes the viewer's own
export const keep = (viewer) => { const l = fromLink(); if (l != null) choose(viewer, l); };

// href with a level, for docs and tutorials that link straight to one
export function withLevel(n) {
  const p = new URLSearchParams(location.search);
  p.set('level', String(n));
  return `${location.pathname}?${p.toString()}${location.hash}`;
}

// ------------------------------------------------------------ offers

// "Show me more": one offer at a time, when the next level answers a
// question the viewer now has. Not now silences an offer for a week; the
// third Not now silences it for good.
const WEEK = 7 * 864e5;
const okey = (viewer, n) => `c4hud:offer:${(viewer && viewer.user) || 'anyone'}:${n}`;
function silenced(viewer, n) {
  try {
    const v = JSON.parse(localStorage.getItem(okey(viewer, n)) || 'null');
    return !!v && (v.count >= 3 || Date.now() - v.at < WEEK);
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
export function offer(snap, route, level, viewer) {
  if (!snap || level >= MAX) return null;
  const mine = (j) => viewer && (viewer.allProjects || j.owner === viewer.user || (viewer.projects || []).includes(j.project));
  const open = snap.jobs.filter((j) => j.phase !== 'Succeeded' && mine(j));
  const next = level + 1;
  if (silenced(viewer, next)) return null;
  let o = null;
  if (level === 0 && (route.name === 'job' || open.some((j) => j.units.failed > 0))) {
    o = { title: 'See the frames', msg: 'Level 1 shows each frame of a job as it renders, coarse to fine, and why one failed.', yes: 'Show frames' };
  } else if (level === 1) {
    const late = open.find((j) => j.objective && j.objective.deadline && j.estimate.slackSeconds < 0);
    if (late) o = { title: `Will ${late.name} make its deadline?`, msg: 'It may miss it now. Level 2 shows its deadline, its slack and what you can do about it.', yes: 'Show time' };
  } else if (level === 2) {
    for (const s of seen.values()) if (s.size >= 3) { o = { title: 'These jobs are one shot', msg: 'Level 3 shows a shot’s jobs side by side, in the order the work flows.', yes: 'Show shots' }; break; }
  } else if (level === 3) {
    if (open.some((j) => j.estimate && j.estimate.burst && j.estimate.burst.needsApproval)) o = { title: 'More capacity?', msg: 'A burst is waiting for approval. Level 4 shows the farm’s slots and what more would cost.', yes: 'Show the farm' };
  } else if (level === 4) {
    if (route.name === 'job') { const j = snap.jobs.find((x) => `${x.namespace}/${x.name}` === route.parts.slice(0, 2).join('/')); if (j && j.output) o = { title: 'Where did this come from?', msg: 'Level 5 shows each output by its C4 ID and every step signed in the book.', yes: 'Show everything' }; }
  }
  return o ? { ...o, level: next } : null;
}
