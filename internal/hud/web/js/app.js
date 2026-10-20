// HUD shell: one snapshot stream, a hash router, the farm slate, and the
// controls. Views render plain DOM that is morphed into place.

import { h, morph } from './dom.js';
import { hydrate } from '../design/components/live.js';
import { setScheme } from '../design/scheme.js';
import '../design/components/pylon.js';
import '../design/components/stack.js';
import { subscribe, post, del, get, as, staticMode, objectURL } from './api.js';
import * as f from './fmt.js';
import * as md from './mode.js';
import * as lvl from './level.js';
import { mine1, farm1, job1, pools1 } from './views/day1.js';
import { incidents, needsCount } from '../design/components/incidents.js';
import * as prefs from './prefs.js';
import { installC4ID } from '../design/components/c4id.js';
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
import catalog from './views/catalog.js';
import yourPage from './views/me.js';

const VIEWS = { queue, job, pools, workers: pools, map, tiers, graph, cost, wall, widget, mine, catalog, me: yourPage };
// each view and the mode it arrives at (docs/intro/ladder.md)
// [view, label, mode, level]: a tab shows from that mode, at that level
const NAV = [
  ['queue', 'Queue', 3, 1],
  ['pools', 'Pools', 4, 1],
  ['map', 'Map', 4, 2],
  ['tiers', 'Tiers', 5, 3],
  ['graph', 'Graph', 3, 2],
  ['cost', 'Cost', 4, 2],
  ['wall', 'Wall', 5, 3],
  ['catalog', 'Catalogue', 3, 1],
];

if (staticMode || new URLSearchParams(location.search).get('motion') === 'off') {
  document.documentElement.dataset.motion = 'off';
}

const state = { snap: null, status: staticMode ? 'static' : 'connecting', route: null, current: null, mode: { mode: 5, source: 'role', mine: 5 }, level: { level: 3, source: 'role', mine: 3 }, sheet: false };

// applyPrefs puts the viewer's preferences into effect: the clock, the UTC
// complication, the zone and the scheme (the mode is read where it is
// drawn). A link's ?clock= ?utc= ?tz= ?scheme= win for that page.
const me = () => (state.snap && state.snap.viewer && state.snap.viewer.user) || '';
function applyPrefs() {
  const u = me();
  f.setClockStyle(prefs.effective(u, 'clock').value);
  f.setShowUTC(prefs.effective(u, 'utc').value);
  f.setZone(prefs.effective(u, 'zone').value);
  applyScheme(prefs.effective(u, 'scheme').value);
}
// 'system' | 'dark' | 'light': design/scheme.js sets data-scheme, follows
// the OS under 'system', and fires c4:scheme on a change
function applyScheme(v) {
  setScheme(v);
}
applyPrefs();
// every C4 ID is a mark: hover or focus shows it, click or Enter copies it
installC4ID(document);

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
  // the mode this view draws at: a job opens at Frames or above
  get mode() { return state.route && state.route.name === 'job' ? Math.max(1, state.mode.mode) : state.mode.mode; },
  // the person's level for this view (level.js); a peek (?peek=N on the
  // route) shows one thing at a higher level without changing theirs
  get level() { const p = Number(state.route && state.route.q && state.route.q.peek); return Math.max(state.level.level, Number.isFinite(p) ? Math.min(3, p) : 0); },
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
  const role = v ? v.role || (v.allProjects ? 'wrangler' : 'artist') : '';
  const other = role === 'wrangler' ? 'artist' : 'wrangler';
  return h('header', { class: 'top' },
    h('a', { class: 'brand', href: href('queue'), 'aria-label': 'c4 HUD, the queue' },
      h('span', { class: 'tick', 'aria-hidden': 'true' }),
      h('span', { class: 'wordmark' }, 'C4', h('b', null, 'HUD'))),
    h('div', { class: 'clock' },
      h('span', { class: 'num' }, s ? f.clockSec(s.now) : '--:--:--', s && f.utc(s.now) ? h('span', { class: 'cx', title: 'UTC' }, f.utc(s.now)) : null),
      h('span', { class: 'lab' }, s ? `${f.dateLine(s.now)} \u00b7 ${f.offsetLabel()}` : '')),
    h('nav', { class: 'views', 'aria-label': 'Views' },
      NAV.filter(([k, , n, l]) => (state.mode.mode >= n && ctx.level >= l && (k !== 'catalog' || state.catalog)) || (state.route && state.route.name === k)).map(([k, label]) => h('a', { href: href(k), 'aria-current': state.route && (state.route.name === k || (k === 'queue' && state.route.name === 'job') || (k === 'pools' && state.route.name === 'workers')) ? 'page' : false }, label))),
    levelSwitch(),
    detail(),
    h('div', { class: 'who' },
      h('span', { class: 'live', 'data-state': state.status, title: `Updates: ${state.status}` }),
      v ? h('a', { class: 'me-link', href: href('me'), title: 'Your page: the trackers that submit work for you' }, h('span', { class: 'mono' }, v.user), ' ', h('span', { class: 'lab' }, `${role} \u00b7 ${v.allProjects ? 'all projects' : v.projects.join(', ')}`)) : null,
      v ? h('a', { href: viewAs(other) }, `View as ${other}`) : null,
      h('button', { class: 'prefs-btn', 'data-prefs': 'open', 'aria-expanded': state.sheet ? 'true' : 'false', title: 'Preferences' }, 'Preferences')));
}

// ------------------------------------------------------------ preferences

// sheet: the Preferences sheet, one row per preference in the schema,
// grouped. Every change applies at once and is kept for the viewer.
function sheet() {
  if (!state.sheet) return h('div', { class: 'pf-sheet', hidden: true });
  const u = me();
  const row = (key) => {
    const S = prefs.SCHEMA[key];
    const eff = prefs.effective(u, key);
    const cur = prefs.get(u, key);
    let input;
    if (S.type === 'enum') {
      input = h('select', { 'data-pref': key, id: 'pref-' + key }, S.values.map((x) => h('option', { value: x, selected: x === cur ? 'selected' : null }, S.names ? S.names[x] : x)));
    } else if (S.type === 'bool') {
      input = h('input', { type: 'checkbox', 'data-pref': key, id: 'pref-' + key, checked: cur ? 'checked' : null });
    } else if (S.type === 'zone') {
      const zones = [...new Set([...(cur ? [cur] : []), ...prefs.ZONES])];
      input = h('select', { 'data-pref': key, id: 'pref-' + key },
        h('option', { value: '', selected: cur === '' ? 'selected' : null }, `Your browser\u2019s (${browserZone()})`),
        zones.map((z) => h('option', { value: z, selected: z === cur ? 'selected' : null }, z.replace(/_/g, ' '))));
    } else if (key === 'level') {
      input = h('span', { class: 'pf-modes' },
        lvl.LEVELS.map((L) => h('button', { class: 'btn small' + (cur === L.n ? ' on' : ''), 'data-pref': 'level', 'data-value': String(L.n), title: L.q }, `${L.n} ${L.name}`)),
        h('button', { class: 'btn small' + (cur == null ? ' on' : ''), 'data-pref': 'level', 'data-value': '' }, `For my role (${lvl.roleDefault(state.snap && state.snap.viewer)})`));
    } else if (key === 'levelByView') {
      const view = state.route ? (state.route.name === 'workers' ? 'pools' : state.route.name) : 'queue';
      const own = (cur || {})[view];
      input = h('span', { class: 'pf-modes' }, h('span', { class: 'dim', style: { marginRight: '6px' } }, `This view (${view}):`),
        lvl.LEVELS.map((L) => h('button', { class: 'btn small' + (own === L.n ? ' on' : ''), 'data-pref-view': view, 'data-value': String(L.n) }, String(L.n))),
        h('button', { class: 'btn small' + (own == null ? ' on' : ''), 'data-pref-view': view, 'data-value': '' }, 'Same as everywhere'));
    } else if (key === 'mode') {
      input = h('span', { class: 'pf-modes' },
        md.MODES.map((M) => h('button', { class: 'btn small' + (cur === M.n ? ' on' : ''), 'data-pref': 'mode', 'data-value': String(M.n), title: M.q }, `${M.n} ${M.name}`)),
        h('button', { class: 'btn small' + (cur == null ? ' on' : ''), 'data-pref': 'mode', 'data-value': '' }, `For my role (${md.roleDefault(state.snap && state.snap.viewer)})`));
    }
    return h('div', { class: 'pf-row' },
      h('label', { class: 'pf-lab', for: 'pref-' + key }, S.label),
      h('div', { class: 'pf-in2' }, input,
        eff.source === 'link' ? h('span', { class: 'pf-note' }, `This page\u2019s link sets ${S.link}=${eff.value}.`) : null,
        S.help ? h('span', { class: 'pf-help' }, S.help) : null));
  };
  return h('div', { class: 'pf-sheet', role: 'dialog', 'aria-label': 'Preferences' },
    h('div', { class: 'pf-in' },
      h('div', { class: 'pf-hd' }, h('span', { class: 'lab' }, 'Preferences'),
        h('span', { class: 'dim' }, u ? `kept for ${u} in this browser` : 'kept in this browser'),
        h('button', { class: 'btn small', 'data-prefs': 'close' }, 'Done')),
      prefs.GROUPS.map((g) => h('section', { class: 'pf-group' }, h('h3', { class: 'lab' }, g),
        Object.keys(prefs.SCHEMA).filter((k) => prefs.SCHEMA[k].group === g).map(row))),
      h('div', { class: 'pf-ft' }, h('button', { class: 'btn small', 'data-prefs': 'reset' }, 'Reset to defaults'))));
}

function browserZone() {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'; } catch { return 'UTC'; }
}

function setPref(key, value) {
  try { prefs.set(me(), key, value); } catch (e) { toast(e.message, true); return; }
  applyPrefs();
  render();
}

document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-prefs]');
  if (b) {
    e.preventDefault();
    const what = b.dataset.prefs;
    if (what === 'open') state.sheet = !state.sheet;
    else if (what === 'close') state.sheet = false;
    else if (what === 'reset') { prefs.reset(me()); applyPrefs(); }
    render();
    return;
  }
  const lb = e.target.closest('.pf-sheet [data-pref="level"]');
  if (lb) { e.preventDefault(); setPref('level', lb.dataset.value === '' ? null : Number(lb.dataset.value)); return; }
  const vb = e.target.closest('.pf-sheet [data-pref-view]');
  if (vb) { e.preventDefault(); lvl.choose(state.snap && state.snap.viewer, vb.dataset.value === '' ? null : Number(vb.dataset.value), vb.dataset.prefView); applyPrefs(); render(); return; }
  const mb = e.target.closest('.pf-sheet [data-pref="mode"]');
  if (mb) { e.preventDefault(); setPref('mode', mb.dataset.value === '' ? null : Number(mb.dataset.value)); }
});
document.addEventListener('change', (e) => {
  const el = e.target.closest('.pf-sheet [data-pref]');
  if (!el) return;
  setPref(el.dataset.pref, el.type === 'checkbox' ? el.checked : el.value);
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && state.sheet) { state.sheet = false; render(); }
});

// levelSwitch: the person's level, 1 2 3, always in reach. A cell sets it
// everywhere; Preferences sets one view apart.
function levelSwitch() {
  const L = state.level;
  const cur = lvl.LEVELS[L.level - 1];
  return h('div', { class: 'detail levelsw', role: 'group', 'aria-label': `Level ${L.level}, ${cur.name}` },
    h('span', { class: 'lab' }, 'Level'),
    h('span', { class: 'cells' }, lvl.LEVELS.map((x) => h('button', {
      class: 'cell-l' + (x.n <= L.level ? ' on' : '') + (x.n === L.level ? ' cur' : ''), 'data-level': String(x.n),
      title: `${x.n} · ${x.name}: ${x.q}`, 'aria-pressed': x.n === L.level ? 'true' : 'false',
    }))),
    h('span', { class: 'lvname' }, `${L.level} · ${cur.name}`));
}

// detail: the mode switch. Six cells and the mode's name; a cell sets the
// viewer's own mode. [ and ] step down and up.
function detail() {
  const M = state.mode, cur = md.MODES[M.mode];
  return h('div', { class: 'detail', role: 'group', 'aria-label': `Mode ${M.mode}, ${cur.name}` },
    h('span', { class: 'lab' }, 'Mode'),
    h('span', { class: 'cells' }, md.MODES.map((x) => h('button', {
      class: 'cell-l' + (x.n <= M.mode ? ' on' : '') + (x.n === M.mode ? ' cur' : ''), 'data-mode': String(x.n),
      title: `${x.n} · ${x.name}: ${x.q}`, 'aria-pressed': x.n === M.mode ? 'true' : 'false',
    }))),
    h('span', { class: 'lvname' }, `${M.mode} · ${cur.name}`));
}

// offerBar: one "show me more" offer, or the note that a link set the mode
function offerBar() {
  const s = state.snap, v = s && s.viewer, L = state.mode;
  // static pages (fixtures, screenshots) show the mode they were given, no offers
  if (!s || staticMode) return h('div', { class: 'offerbar', hidden: true });
  if (L.source === 'link' && L.mode !== L.mine) {
    return h('div', { class: 'offerbar note', role: 'status' },
      h('span', { class: 'msg' }, `Showing ${md.MODES[L.mode].name} mode from this link. Yours is ${md.MODES[L.mine].name}.`),
      h('button', { class: 'btn small', 'data-offer': 'keep' }, `Keep ${md.MODES[L.mode].name}`),
      h('button', { class: 'btn small', 'data-offer': 'back' }, `Back to ${md.MODES[L.mine].name}`));
  }
  const lo = lvl.offer(v, state.level.level);
  if (lo) {
    return h('div', { class: 'offerbar', role: 'status' },
      h('i', { class: 'mark', 'aria-hidden': 'true' }),
      h('span', { class: 'pt' }, h('span', { class: 'lab' }, lo.title), h('span', { class: 'msg' }, lo.msg)),
      h('button', { class: 'btn primary small', 'data-offer': 'level-yes', 'data-level-to': String(lo.level) }, lo.yes),
      h('button', { class: 'btn small', 'data-offer': 'level-no', 'data-level-to': String(lo.level) }, 'No thanks'));
  }
  const o = md.offer(s, state.route, L.mode, v);
  if (!o) return h('div', { class: 'offerbar', hidden: true });
  return h('div', { class: 'offerbar', role: 'status' },
    h('i', { class: 'mark', 'aria-hidden': 'true' }),
    h('span', { class: 'pt' }, h('span', { class: 'lab' }, o.title), h('span', { class: 'msg' }, o.msg)),
    h('button', { class: 'btn primary small', 'data-offer': 'yes', 'data-mode': String(o.mode) }, o.yes),
    h('button', { class: 'btn small', 'data-offer': 'notnow', 'data-mode': String(o.mode) }, 'No thanks'));
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
      h('span', { class: 'meter', style: { width: '150px' }, 'aria-hidden': 'true' }, seg),
      // right after a cap drops, units still running past it until their turns end
      fm.slotsUsed > total ? h('span', { class: 'sub' }, `${f.num(fm.slotsUsed - total)} over the cap, stopping`) : null),
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
    // bytes avoided mean something only on a farm that mounts data
    !fm.bytesMaterialized ? null : h('div', { class: 'cell head' }, h('span', { class: 'lab' }, 'Savings: bytes avoided today'),
      h('span', { class: 'val' }, f.bytes(fm.bytesAvoided)),
      h('span', { class: 'ratio', 'aria-hidden': 'true' }, h('i', { style: { width: `${(avoidedFrac * 100).toFixed(2)}%` } })),
      h('span', { class: 'sub' }, `of ${f.bytes(fm.bytesMaterialized)} mounted; ${f.bytes(fm.bytesFetchedFromStore)} came from the store`)));
}

let lastView = null, lastRoute = null;
// the Catalogue tab shows only on a farm that serves a catalogue of designs
fetch(objectURL('/api/v1/catalog'), { method: 'HEAD' }).then((r) => { state.catalog = r.ok; if (r.ok && state.snap) render(); }).catch(() => {});
function render() {
  const route = state.route;
  state.mode = md.current(state.snap && state.snap.viewer);
  state.level = lvl.current(state.snap && state.snap.viewer, route.name === 'workers' ? 'pools' : route.name);
  document.documentElement.dataset.mode = String(ctx.mode);
  document.documentElement.dataset.level = String(ctx.level);
  // the peripheral cue: a steady red edge on the top bar while anything
  // needs a person, on every page and in every mode (incidents.js)
  if (state.snap) document.documentElement.dataset.needs = needsCount(incidents(state.snap)) > 0 ? '1' : '0';
  // reaching for expert detail at level 2 counts toward the one offer of 3
  if (state.snap && state.level.level === 2 && lastRoute !== location.hash && (ctx.mode >= 5 || ['tiers', 'wall', 'cost'].includes(route.name))) lvl.used(state.snap.viewer, 'expert');
  lastRoute = location.hash;
  if (route.name === 'job' && state.snap) md.opened(ctx.job(route.parts.slice(0, 2).join('/')));

  const bare = route.name === 'wall' || route.name === 'widget';
  document.documentElement.dataset.scale = route.name === 'wall' ? 'wall' : route.name === 'widget' ? 'widget' : 'desk';
  document.getElementById('top').hidden = bare;
  document.getElementById('slate').hidden = bare;
  // an artist's queue is their own shots
  const mineView = route.name === 'queue' && state.snap && state.snap.viewer && !state.snap.viewer.allProjects && ctx.mode >= 2;
  let view = mineView ? VIEWS.mine : VIEWS[route.name];
  // level 1: the day-one form of the views that have one
  if (ctx.level === 1 && state.snap) {
    const D1 = { queue: state.snap.viewer && state.snap.viewer.allProjects ? farm1 : mine1, job: job1, pools: pools1, workers: pools1 };
    if (D1[route.name]) view = D1[route.name];
  }
  if (lastView && lastView !== view && lastView.leave) lastView.leave();
  lastView = view;
  morphInto(document.getElementById('top'), topBar());
  morphInto(document.getElementById('slate'), slate());
  document.getElementById('slate').hidden = bare || ctx.mode < 4 || ctx.level < 3;
  morphInto(document.getElementById('offer'), offerBar());
  morphInto(document.getElementById('prefs'), sheet());
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
  const wrap = h('main', { id: 'main', tabindex: '-1', 'data-view': mineView ? 'mine' : route.name, 'data-level': String(ctx.level) }, el);
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
  accept: (d) => [`/jobs/${d.job}/ack`, { units: d.units, kind: 'accept' }],
  blacklist: (d) => [`/jobs/${d.job}/blacklist`, { node: d.node }],
  unblacklist: (d) => [`/jobs/${d.job}/blacklist`, { node: d.node, remove: true }],
  disable: (d) => [`/nodes/${d.node}/disable`, { disabled: true }],
  enable: (d) => [`/nodes/${d.node}/disable`, { disabled: false }],
  approve: (d) => [`/capacity/${d.pool}/approve`, {}],
  // a grant: the form's limits (level 2), else any work for 30 days
  grant: (d) => {
    const form = d.form && document.getElementById(d.form);
    const ticked = (name) => (form ? [...form.querySelectorAll(`input[name="${name}"]:checked`)].map((x) => x.value) : []);
    const until = (form && form.querySelector('select[name="until"]')) ? form.querySelector('select[name="until"]').value : d.until || '30d';
    return ['/grants', { namespace: d.ns, tracker: d.tracker, classes: ticked('class'), pools: ticked('pool'), until }];
  },
  revoke: (d) => [`/grants/${d.grant}`, null, 'DELETE'],
};

document.addEventListener('click', async (e) => {
  const b = e.target.closest('[data-action]');
  if (!b) return;
  e.preventDefault();
  const fn = ACTIONS[b.dataset.action];
  if (!fn) return;
  try {
    const [path, body, method] = fn(b.dataset);
    b.disabled = true;
    const r = method === 'DELETE' ? await del(path) : await post(path, body);
    toast(r.message || 'Done.');
    document.dispatchEvent(new CustomEvent('hud:changed'));
  } catch (err) {
    toast(err.message, true);
  } finally {
    b.disabled = false;
  }
});

// the mode switch, the offers and the [ ] keys
document.addEventListener('click', (e) => {
  const v = state.snap && state.snap.viewer;
  const lc = e.target.closest('.levelsw [data-level]');
  if (lc) { e.preventDefault(); lvl.choose(v, Number(lc.dataset.level)); render(); return; }
  if (e.target.closest('[data-peek]')) lvl.used(v, 'peek');
  const cell = e.target.closest('.detail [data-mode]');
  if (cell) { e.preventDefault(); md.choose(v, Number(cell.dataset.mode)); render(); return; }
  const b = e.target.closest('[data-offer]');
  if (!b) return;
  e.preventDefault();
  const what = b.dataset.offer;
  if (what === 'level-yes') { lvl.settle(v, Number(b.dataset.levelTo), 'yes'); lvl.choose(v, Number(b.dataset.levelTo)); }
  else if (what === 'level-no') lvl.settle(v, Number(b.dataset.levelTo), 'no');
  else if (what === 'yes') { md.notNow(v, Number(b.dataset.mode)); md.choose(v, Number(b.dataset.mode)); }
  else if (what === 'notnow') md.notNow(v, Number(b.dataset.mode));
  else if (what === 'keep') md.keep(v);
  else if (what === 'back') md.choose(v, state.mode.mine);
  render();
});
document.addEventListener('keydown', (e) => {
  if (e.metaKey || e.ctrlKey || e.altKey || /^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)) return;
  if (e.key !== '[' && e.key !== ']') return;
  const n = state.mode.mode + (e.key === ']' ? 1 : -1);
  if (n < 0 || n > md.MAX) return;
  md.choose(state.snap && state.snap.viewer, n);
  render();
});

document.addEventListener('hud:refresh', () => render());
// canvases and SVG drawn from tok() redraw in the new scheme
window.addEventListener('c4:scheme', () => { if (state.route) render(); });
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
  const was = me();
  state.snap = snap;
  // the viewer is known now: their preferences apply
  if (me() !== was) applyPrefs();
  render();
}, (st, msg) => {
  state.status = staticMode ? 'static' : st;
  if (st === 'error' && !state.snap) document.getElementById('main').textContent = `The farm did not answer: ${msg}`;
});

export { as };
