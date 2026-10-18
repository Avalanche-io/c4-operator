// HUD shell: one snapshot stream, a hash router, the farm slate, and the
// controls. Views render plain DOM that is morphed into place.

import { h, morph } from './dom.js';
import { hydrate } from '../design/components/live.js';
import '../design/components/pylon.js';
import '../design/components/stack.js';
import { subscribe, post, get, as, staticMode } from './api.js';
import * as f from './fmt.js';
import * as lv from './level.js';
import queue from './views/queue.js';
import job from './views/job.js';
import pools from './views/pools.js';
import map from './views/map.js';
import wall from './views/wall.js';
import widget from './views/widget.js';
import mine from './views/mine.js';
import tiers from './views/tiers.js';
import graph from './views/graph.js';
import cost from './views/cost.js';

const VIEWS = { queue, job, pools, workers: pools, map, tiers, graph, cost, wall, widget, mine };
// each view and the detail level it arrives at (docs/intro/ladder.md)
const NAV = [
  ['queue', 'Queue', 3],
  ['pools', 'Pools', 4],
  ['map', 'Map', 4],
  ['tiers', 'Tiers', 5],
  ['graph', 'Graph', 3],
  ['cost', 'Cost', 4],
  ['wall', 'Wall', 5],
];

if (staticMode || new URLSearchParams(location.search).get('motion') === 'off') {
  document.documentElement.dataset.motion = 'off';
}

const state = { snap: null, status: staticMode ? 'static' : 'connecting', route: null, current: null, level: { level: 5, source: 'role', mine: 5 } };

function parseRoute() {
  const raw = location.hash.replace(/^#\/?/, '');
  const [path, query] = raw.split('?');
  const parts = path.split('/').filter(Boolean).map(decodeURIComponent);
  const q = Object.fromEntries(new URLSearchParams(query || ''));
  const name = VIEWS[parts[0]] ? parts[0] : 'queue';
  return { name, parts: parts.slice(1), q };
}

export function href(hash) {
  return `${location.pathname}${location.search}#/${hash}`;
}

const ctx = {
  get snap() { return state.snap; },
  get route() { return state.route; },
  // the detail level this view draws at: a job opens at level 1 or above
  get level() { return state.route && state.route.name === 'job' ? Math.max(1, state.level.level) : state.level.level; },
  href,
  go(hash) { location.hash = '#/' + hash; },
  refresh: () => render(),
  job(key) { return state.snap && state.snap.jobs.find((j) => `${j.namespace}/${j.name}` === key); },
  cls(name) { return state.snap && state.snap.classes.find((c) => c.name === name); },
  toast,
};

function viewAs(who) {
  const p = new URLSearchParams(location.search);
  p.set('as', who);
  return `${location.pathname}?${p.toString()}${location.hash}`;
}

function topBar() {
  const s = state.snap;
  const v = s && s.viewer;
  const role = v ? (v.allProjects ? 'wrangler' : 'artist') : '';
  const other = role === 'wrangler' ? 'artist' : 'wrangler';
  return h('header', { class: 'top' },
    h('a', { class: 'brand', href: href('queue'), 'aria-label': 'c4 HUD, the queue' },
      h('span', { class: 'tick', 'aria-hidden': 'true' }),
      h('span', { class: 'wordmark' }, 'C4', h('b', null, 'HUD'))),
    h('div', { class: 'clock' },
      h('span', { class: 'num' }, s ? f.clockSec(s.now) : '--:--:--', s && f.utc(s.now) ? h('span', { class: 'cx', title: 'UTC' }, f.utc(s.now)) : null),
      h('span', { class: 'lab' }, s ? `${f.dateLine(s.now)} \u00b7 ${f.offsetLabel()}` : '')),
    h('nav', { class: 'views', 'aria-label': 'Views' },
      NAV.filter(([k, , n]) => state.level.level >= n || (state.route && state.route.name === k)).map(([k, label]) => h('a', { href: href(k), 'aria-current': state.route && (state.route.name === k || (k === 'queue' && state.route.name === 'job') || (k === 'pools' && state.route.name === 'workers')) ? 'page' : false }, label))),
    detail(),
    h('div', { class: 'who' },
      h('span', { class: 'live', 'data-state': state.status, title: `Updates: ${state.status}` }),
      v ? h('span', null, h('span', { class: 'mono' }, v.user), ' ', h('span', { class: 'lab' }, `${role} \u00b7 ${v.allProjects ? 'all projects' : v.projects.join(', ')}`)) : null,
      v ? h('a', { href: viewAs(other) }, `View as ${other}`) : null));
}

// detail: the level switch. Six cells and the level's name; a cell sets
// the viewer's own level. [ and ] step down and up.
function detail() {
  const L = state.level, cur = lv.LEVELS[L.level];
  return h('div', { class: 'detail', role: 'group', 'aria-label': `Detail level ${L.level}, ${cur.name}` },
    h('span', { class: 'lab' }, 'Detail'),
    h('span', { class: 'cells' }, lv.LEVELS.map((x) => h('button', {
      class: 'cell-l' + (x.n <= L.level ? ' on' : '') + (x.n === L.level ? ' cur' : ''), 'data-level': String(x.n),
      title: `${x.n} · ${x.name}: ${x.q}`, 'aria-pressed': x.n === L.level ? 'true' : 'false',
    }))),
    h('span', { class: 'lvname' }, `${L.level} · ${cur.name}`));
}

// offerBar: one "show me more" offer, or the note that a link set the level
function offerBar() {
  const s = state.snap, v = s && s.viewer, L = state.level;
  // static pages (fixtures, screenshots) show the level they were given, no offers
  if (!s || staticMode) return h('div', { class: 'offerbar', hidden: true });
  if (L.source === 'link' && L.level !== L.mine) {
    return h('div', { class: 'offerbar note', role: 'status' },
      h('span', { class: 'msg' }, `Showing level ${L.level}, ${lv.LEVELS[L.level].name}, from this link. Your level is ${L.mine}, ${lv.LEVELS[L.mine].name}.`),
      h('button', { class: 'btn small', 'data-offer': 'keep' }, `Keep ${L.level}`),
      h('button', { class: 'btn small', 'data-offer': 'back' }, `Back to ${L.mine}`));
  }
  const o = lv.offer(s, state.route, L.level, v);
  if (!o) return h('div', { class: 'offerbar', hidden: true });
  return h('div', { class: 'offerbar', role: 'status' },
    h('i', { class: 'mark', 'aria-hidden': 'true' }),
    h('span', { class: 'pt' }, h('span', { class: 'lab' }, o.title), h('span', { class: 'msg' }, o.msg)),
    h('button', { class: 'btn primary small', 'data-offer': 'yes', 'data-level': String(o.level) }, o.yes),
    h('button', { class: 'btn small', 'data-offer': 'notnow', 'data-level': String(o.level) }, 'Not now'));
}

function slate() {
  const s = state.snap;
  if (!s) return h('div', { class: 'slate' });
  const fm = s.farm;
  const total = fm.slotsTotal;
  const mine = fm.slotsUsed - fm.otherProjectsSlots;
  const seg = [];
  const step = total > 160 ? Math.ceil(total / 160) : 1;
  for (let i = 0; i < total; i += step) {
    seg.push(h('i', { class: i < mine ? 'on' : i < fm.slotsUsed ? 'other' : '' }));
  }
  const avoidedFrac = fm.bytesMaterialized ? fm.bytesAvoided / fm.bytesMaterialized : 0;
  return h('div', { class: 'slate', role: 'region', 'aria-label': 'Farm at a glance' },
    h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Slots in use'),
      h('span', { class: 'val' }, f.num(fm.slotsUsed), h('small', null, `of ${f.num(total)}`)),
      h('span', { class: 'meter', style: { width: '150px' }, 'aria-hidden': 'true' }, seg)),
    fm.otherProjectsSlots || !s.viewer.allProjects ? h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Other projects'),
      h('span', { class: 'val' }, f.num(fm.otherProjectsSlots), h('small', null, 'slots')),
      h('span', { class: 'sub' }, 'shown only as a total')) : null,
    h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Units waiting'), h('span', { class: 'val' }, f.num(fm.queuedUnits))),
    h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Units per hour'), h('span', { class: 'val' }, f.num(fm.unitsPerHour))),
    h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Failures, last hour'),
      h('span', { class: 'val' + (fm.failuresLastHour ? ' t-red' : '') }, f.num(fm.failuresLastHour))),
    h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Burst in flight'),
      h('span', { class: 'val' + (fm.burstSlots ? ' t-blue' : '') }, f.num(fm.burstSlots), h('small', null, 'slots')),
      h('span', { class: 'sub' }, fm.burstSlots ? `${f.money(fm.burstCostPerHour, true)} per hour` : 'none')),
    h('div', { class: 'cell head' }, h('span', { class: 'lab' }, 'Savings: bytes avoided today'),
      h('span', { class: 'val' }, f.bytes(fm.bytesAvoided)),
      h('span', { class: 'ratio', 'aria-hidden': 'true' }, h('i', { style: { width: `${(avoidedFrac * 100).toFixed(2)}%` } })),
      h('span', { class: 'sub' }, `of ${f.bytes(fm.bytesMaterialized)} mounted; ${f.bytes(fm.bytesFetchedFromStore)} came from the store`)));
}

let lastView = null;
function render() {
  const route = state.route;
  state.level = lv.current(state.snap && state.snap.viewer);
  document.documentElement.dataset.level = String(ctx.level);
  if (route.name === 'job' && state.snap) lv.opened(ctx.job(route.parts.slice(0, 2).join('/')));

  const bare = route.name === 'wall' || route.name === 'widget';
  document.documentElement.dataset.scale = route.name === 'wall' ? 'wall' : route.name === 'widget' ? 'widget' : 'desk';
  document.getElementById('top').hidden = bare;
  document.getElementById('slate').hidden = bare;
  // an artist's queue is their own shots
  const mineView = route.name === 'queue' && state.snap && state.snap.viewer && !state.snap.viewer.allProjects && ctx.level >= 2;
  const view = mineView ? VIEWS.mine : VIEWS[route.name];
  if (lastView && lastView !== view && lastView.leave) lastView.leave();
  lastView = view;
  morphInto(document.getElementById('top'), topBar());
  morphInto(document.getElementById('slate'), slate());
  document.getElementById('slate').hidden = bare || ctx.level < 4;
  morphInto(document.getElementById('offer'), offerBar());
  document.getElementById('offer').hidden = bare;
  const main = document.getElementById('main');
  if (!state.snap) return;
  let el;
  try {
    el = view.render(ctx);
  } catch (e) {
    console.error(e);
    el = h('div', { class: 'err' }, `This view failed to draw: ${e.message}`);
  }
  const wrap = h('main', { id: 'main', tabindex: '-1', 'data-view': mineView ? 'mine' : route.name }, el);
  morph(main, wrap);
  hydrate(main);
  if (view.after) view.after(ctx, main);
  // ?notes=[{"target":"css selector","term":"slack","side":"below"}...]: callouts
  // over the page, for tutorial screenshots and tours (design/components/callouts.js)
  const notes = new URLSearchParams(location.search).get('notes');
  if (notes) import('../design/components/callouts.js').then((m) => { try { m.overlay(JSON.parse(notes)); } catch (e) { console.error('notes:', e.message); } });
}

function morphInto(host, el) {
  if (!host.firstChild) host.appendChild(el);
  else morph(host.firstChild, el);
}

let toastTimer;
function toast(msg, err = false) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.className = 'toast' + (err ? ' err' : '');
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 4200);
}

// Controls. Every button names what it does; the farm's answer is shown.
const ACTIONS = {
  hold: (d) => [`/jobs/${d.job}/nudge`, { hold: true }],
  release: (d) => [`/jobs/${d.job}/nudge`, { hold: false }],
  holdstop: (d) => [`/jobs/${d.job}/nudge`, { holdStop: true }],
  boost: (d) => [`/jobs/${d.job}/nudge`, { boost: Number(d.value) }],
  deadline: (d) => {
    const input = document.getElementById(d.input);
    if (!input || !input.value) throw new Error('Pick a date and time first.');
    return [`/jobs/${d.job}/nudge`, { deadline: f.fromInput(input.value) }];
  },
  requeue: (d) => [`/jobs/${d.job}/requeue`, { units: d.units }],
  blacklist: (d) => [`/jobs/${d.job}/blacklist`, { node: d.node }],
  unblacklist: (d) => [`/jobs/${d.job}/blacklist`, { node: d.node, remove: true }],
  disable: (d) => [`/nodes/${d.node}/disable`, { disabled: true }],
  enable: (d) => [`/nodes/${d.node}/disable`, { disabled: false }],
  approve: (d) => [`/capacity/${d.pool}/approve`, {}],
};

document.addEventListener('click', async (e) => {
  const b = e.target.closest('[data-action]');
  if (!b) return;
  e.preventDefault();
  const fn = ACTIONS[b.dataset.action];
  if (!fn) return;
  try {
    const [path, body] = fn(b.dataset);
    b.disabled = true;
    const r = await post(path, body);
    toast(r.message || 'Done.');
    document.dispatchEvent(new CustomEvent('hud:changed'));
  } catch (err) {
    toast(err.message, true);
  } finally {
    b.disabled = false;
  }
});

// the level switch, the offers and the [ ] keys
document.addEventListener('click', (e) => {
  const v = state.snap && state.snap.viewer;
  const cell = e.target.closest('.detail [data-level]');
  if (cell) { e.preventDefault(); lv.choose(v, Number(cell.dataset.level)); render(); return; }
  const b = e.target.closest('[data-offer]');
  if (!b) return;
  e.preventDefault();
  const what = b.dataset.offer;
  if (what === 'yes') lv.choose(v, Number(b.dataset.level));
  else if (what === 'notnow') lv.notNow(v, Number(b.dataset.level));
  else if (what === 'keep') lv.keep(v);
  else if (what === 'back') lv.choose(v, state.level.mine);
  render();
});
document.addEventListener('keydown', (e) => {
  if (e.metaKey || e.ctrlKey || e.altKey || /^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)) return;
  if (e.key !== '[' && e.key !== ']') return;
  const n = state.level.level + (e.key === ']' ? 1 : -1);
  if (n < 0 || n > lv.MAX) return;
  lv.choose(state.snap && state.snap.viewer, n);
  render();
});

document.addEventListener('hud:refresh', () => render());
document.addEventListener('hud:changed', () => {
  get('/snapshot').then((snap) => { f.setClock(snap.now); state.snap = snap; render(); }, () => {});
});

window.addEventListener('hashchange', () => {
  const prev = state.route;
  state.route = parseRoute();
  render();
  if (!prev || prev.name !== state.route.name) document.getElementById('main').focus({ preventScroll: true });
});

state.route = parseRoute();
subscribe((snap) => {
  f.setClock(snap.now);
  state.snap = snap;
  render();
}, (st, msg) => {
  state.status = staticMode ? 'static' : st;
  if (st === 'error' && !state.snap) document.getElementById('main').textContent = `The farm did not answer: ${msg}`;
});

export { as };
