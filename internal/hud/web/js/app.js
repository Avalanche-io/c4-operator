// HUD shell: one snapshot stream, a hash router, the farm slate, and the
// controls. Views render plain DOM that is morphed into place.

import { h, morph } from './dom.js';
import { hydrate } from '../design/components/live.js';
import '../design/components/pylon.js';
import '../design/components/stack.js';
import { subscribe, post, get, as, staticMode } from './api.js';
import * as f from './fmt.js';
import queue from './views/queue.js';
import job from './views/job.js';
import pools from './views/pools.js';
import map from './views/map.js';
import wall from './views/wall.js';
import widget from './views/widget.js';
import tiers from './views/tiers.js';
import graph from './views/graph.js';
import cost from './views/cost.js';

const VIEWS = { queue, job, pools, workers: pools, map, tiers, graph, cost, wall, widget };
const NAV = [
  ['queue', 'Queue'],
  ['pools', 'Pools'],
  ['map', 'Map'],
  ['tiers', 'Tiers'],
  ['graph', 'Graph'],
  ['cost', 'Cost'],
  ['wall', 'Wall'],
];

if (staticMode || new URLSearchParams(location.search).get('motion') === 'off') {
  document.documentElement.dataset.motion = 'off';
}

const state = { snap: null, status: staticMode ? 'static' : 'connecting', route: null, current: null };

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
    h('a', { class: 'brand', href: href('queue'), 'aria-label': 'c4 HUD, the farm sheet' },
      h('span', { class: 'tick', 'aria-hidden': 'true' }),
      h('span', { class: 'wordmark' }, 'C4', h('b', null, 'HUD'))),
    h('div', { class: 'clock' },
      h('span', { class: 'num' }, s ? f.clockSec(s.now) : '--:--:--'),
      h('span', { class: 'lab' }, s ? `${f.dateLine(s.now)} \u00b7 ${f.offsetLabel()}` : '')),
    h('nav', { class: 'views', 'aria-label': 'Views' },
      NAV.map(([k, label]) => h('a', { href: href(k), 'aria-current': state.route && (state.route.name === k || (k === 'queue' && state.route.name === 'job') || (k === 'pools' && state.route.name === 'workers')) ? 'page' : false }, label))),
    h('div', { class: 'who' },
      h('span', { class: 'live', 'data-state': state.status, title: `Updates: ${state.status}` }),
      v ? h('span', null, h('span', { class: 'mono' }, v.user), ' ', h('span', { class: 'lab' }, `${role} \u00b7 ${v.allProjects ? 'all projects' : v.projects.join(', ')}`)) : null,
      v ? h('a', { href: viewAs(other) }, `View as ${other}`) : null));
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
  const bare = route.name === 'wall' || route.name === 'widget';
  document.documentElement.dataset.scale = route.name === 'wall' ? 'wall' : route.name === 'widget' ? 'widget' : 'desk';
  document.getElementById('top').hidden = bare;
  document.getElementById('slate').hidden = bare;
  const view = VIEWS[route.name];
  if (lastView && lastView !== view && lastView.leave) lastView.leave();
  lastView = view;
  morphInto(document.getElementById('top'), topBar());
  morphInto(document.getElementById('slate'), slate());
  const main = document.getElementById('main');
  if (!state.snap) return;
  let el;
  try {
    el = view.render(ctx);
  } catch (e) {
    console.error(e);
    el = h('div', { class: 'err' }, `This view failed to draw: ${e.message}`);
  }
  const wrap = h('main', { id: 'main', tabindex: '-1', 'data-view': route.name }, el);
  morph(main, wrap);
  hydrate(main);
  if (view.after) view.after(ctx, main);
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
    return [`/jobs/${d.job}/nudge`, { deadline: farmTime(input.value) }];
  },
  requeue: (d) => [`/jobs/${d.job}/requeue`, { units: d.units }],
  blacklist: (d) => [`/jobs/${d.job}/blacklist`, { node: d.node }],
  unblacklist: (d) => [`/jobs/${d.job}/blacklist`, { node: d.node, remove: true }],
  disable: (d) => [`/nodes/${d.node}/disable`, { disabled: true }],
  enable: (d) => [`/nodes/${d.node}/disable`, { disabled: false }],
  approve: (d) => [`/capacity/${d.pool}/approve`, {}],
};

// farmTime turns a datetime-local value into RFC 3339 in the farm's offset.
function farmTime(v) {
  const m = /([+-]\d\d:\d\d)$/.exec(state.snap.now);
  return `${v.length === 16 ? v + ':00' : v}${m ? m[1] : 'Z'}`;
}

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
