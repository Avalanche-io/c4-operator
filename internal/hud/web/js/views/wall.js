// The wall (#/wall): TIMEBASE. Mission clocks for the deliveries and the
// jobs that make them late; every job a channel sweeping toward its
// deadline; a strobe strip of the most urgent binary-order render; every
// shot collapsed to one aggregate; the book as a ticker along the bottom.
// The shell hides its bars and sets data-scale="wall". `?tour=1` moves the
// highlight from shot to shot every 20 seconds; `?shot=sh040` holds it on one;
// `?horizon=8` sets the forecast horizon in hours.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get, staticMode } from '../api.js';
import { own } from '../../design/components/live.js';
import { sweep, channelOf, samples, useHistory } from '../../design/components/timebase.js';
import { shotsOf, shotKeyFn, shotGlance } from '../../design/components/dispatch.js';
import { inFlight, retries } from '../../design/components/core.js';
import { heldByPerson } from '../../design/components/words.js';

const keyOf = (j) => `${j.namespace}/${j.name}`;
const fin = (j) => j.phase === 'Succeeded';
const TOUR_MS = 20000;

const st = {
  graph: null, graphAt: 0,
  book: null, bookAt: -1e9, bookMissing: false,
  strobeKey: null, strobe: null, strobeAt: 0, strobeNow: null,
  tour: 0, timer: 0, ctx: null,
};

function fetches(ctx, strobeKey) {
  const p = performance.now();
  if (!st.bookMissing && p - st.bookAt > 4000) {
    st.bookAt = p;
    get('/book?limit=20').then((b) => { st.book = b; ctx.refresh(); }, (e) => { if (/404|not found/i.test(e.message)) st.bookMissing = true; });
  }
  if (strobeKey && (strobeKey !== st.strobeKey || (st.strobeNow !== ctx.snap.now && p - st.strobeAt > 3000))) {
    if (strobeKey !== st.strobeKey) st.strobe = null;
    st.strobeKey = strobeKey; st.strobeAt = p; st.strobeNow = ctx.snap.now;
    get(`/jobs/${strobeKey}`).then((d) => { if (st.strobeKey === strobeKey) { st.strobe = d; ctx.refresh(); } }, () => {});
  }
}

// groups: every shot (with the deliveries it feeds), then the work outside
// shots: a project's CI as one block, anything else on its own.
function groupsOf(ctx, jobs) {
  const shots = shotsOf(jobs).map((s) => ({ ...s, jobs: [...s.jobs, ...s.deliveries.filter((d) => !s.jobs.includes(d))], kind: 'shot' }));
  const placed = new Set(shots.flatMap((s) => s.jobs.map(keyOf)));
  const rest = new Map();
  for (const j of jobs) {
    if (placed.has(keyOf(j)) || fin(j)) continue;
    const c = ctx.cls(j.class);
    const k = c && c.lens === 'ci' ? `ci:${j.project}` : keyOf(j);
    if (!rest.has(k)) rest.set(k, { key: k, kind: c && c.lens === 'ci' ? 'ci' : 'job', jobs: [], project: j.project });
    rest.get(k).jobs.push(j);
  }
  const out = [...shots.filter((s) => s.open), ...[...rest.values()].map((g) => {
    const open = g.jobs.filter((j) => !fin(j));
    return { ...g, glance: shotGlance(g.jobs), open: open.length, late: open.some((j) => j.estimate.slackSeconds < 0), rank: Math.min(...g.jobs.map((j) => j.rank || 1e6)) };
  })];
  return out;
}

const stage = (j, shot) => j.name.replace(shot + '-', '').replace(/-v\d+$/, '');

function groupTitle(g) {
  if (g.kind === 'ci') return [`CI · ${g.project}`, `${g.jobs.length} pipeline${g.jobs.length > 1 ? 's' : ''}`];
  if (g.kind === 'job') return [g.jobs[0].name, g.jobs[0].class];
  const flow = g.jobs.filter((j) => j.shot || shotKeyFn([j])(j)).slice().sort((a, b) => (fin(b) - fin(a)) || (Date.parse(a.objective.deadline || 0) - Date.parse(b.objective.deadline || 0)));
  const names = flow.map((j) => stage(j, g.key));
  return [g.key, names.length > 1 ? `${names[0]} to ${names[names.length - 1]}` : names[0] || ''];
}

function groupSub(g) {
  const gl = g.glance;
  const open = g.jobs.filter((j) => !fin(j));
  const held = open.length && open.every((j) => j.phase === 'Held');
  // held only by open questions: they wait on them, not on a person's release
  const asks = held && open.some((j) => !heldByPerson(j));
  const yielded = open.length && open.every((j) => j.phase === 'Yielded');
  const proj = g.project || (g.jobs[0] && g.jobs[0].project);
  if (g.late) return [`${proj} · late ${f.dur(-gl.slack)}`, true];
  if (held) return [`${proj} · ${asks ? 'waits on a question' : 'held'}`, false];
  if (yielded) return [`${proj} · queued`, false];
  if (Number.isFinite(gl.slack) && gl.slack < 6 * 3600) return [`${proj} · ${f.signed(gl.slack)}`, false];
  return [`${proj}${gl.deadline ? ` · due ${f.clock(gl.deadline)}` : ''}`, false];
}

function shotCard(g, hi, k) {
  const gl = g.glance;
  const [title, flow] = groupTitle(g);
  const [sub, late] = groupSub(g);
  const u = gl.units;
  const pr = Math.round(gl.par * 100);
  return h('div', { class: `wshot${g.late ? ' late' : ''}${hi ? ' hi' : ''}`, 'data-key': 'ws:' + g.key, role: 'listitem' },
    own('pylon', 'wsp:' + g.key, { glance: gl, w: Math.round(40 * k), h: Math.round(96 * k), mode: 'mini' }, { class: 'wspy' }),
    h('div', { class: 'wst' }, h('b', null, title.toUpperCase()), flow ? h('span', null, ` · ${flow}`) : null),
    h('div', { class: 'wss' + (late ? ' t-risk' : '') }, sub),
    h('div', { class: 'wsn' }, h('span', { class: 'big' }, String(Math.round(gl.done * 100))), h('span', { class: 'pc' }, '%'),
      gl.par > 0 ? h('span', { class: 'run' }, `${pr < 1 ? '<1' : pr}% run`) : null),
    h('div', { class: 'wsu' }, `${f.num(g.jobs.reduce((a, j) => a + j.units.succeeded, 0))} of ${f.num(u.total)} units${gl.basis === 'units' ? ', fill by units' : ', fill by work'}`));
}

// mission clocks: the deliveries, the jobs at the root of a lateness, the
// next deadline on a shot, the production window's day, the farm working now
function clocks(ctx, s, groups) {
  const jobs = s.jobs;
  const byKey = new Map(jobs.map((j) => [keyOf(j), j]));
  const late = (j) => !fin(j) && j.objective.deadline && j.estimate.slackSeconds < 0;
  const upstreamLate = (j) => (j.dependsOn || []).some((d) => { const x = byKey.get(d); return x && late(x); });
  const rootOf = (j, seen = new Set()) => {
    for (const d of j.dependsOn || []) {
      const x = byKey.get(d);
      if (!x || !late(x) || seen.has(d)) continue;
      seen.add(d);
      return upstreamLate(x) ? rootOf(x, seen) : x;
    }
    return null;
  };
  const sk = shotKeyFn(jobs);
  const deliveries = new Set(groups.filter((g) => g.kind === 'shot').flatMap((g) => g.deliveries || []).map(keyOf));
  const items = [];
  const cx = (iso) => (f.utc(iso) ? `<span class="cx">${f.utc(iso)}</span>` : '');
  const due = (iso) => `due ${f.clock(iso, false)}${cx(iso)}`;
  const nice = (j) => j.name.replace(/^[a-z]+-(?=delivery)/, '').replace(/-/g, ' ');
  for (const j of jobs.filter((x) => deliveries.has(keyOf(x)) && !fin(x)).sort((a, b) => Date.parse(a.objective.deadline) - Date.parse(b.objective.deadline))) {
    if (!j.objective.deadline) continue;
    const r = late(j) ? rootOf(j) : null;
    items.push({ kind: 'tminus', target: Date.parse(j.objective.deadline), late: late(j), label: `${nice(j)} · ${due(j.objective.deadline)}`,
      sub: late(j) ? `p90 ${f.clock(j.estimate.etaP90, false)} · <b>late ${f.dur(-j.estimate.slackSeconds)}</b>${r ? ` through ${sk(r) || r.name}` : ''}` : `p90 ${f.clock(j.estimate.etaP90, false)} · slack ${f.signed(j.estimate.slackSeconds)}` });
  }
  const roots = jobs.filter((j) => late(j) && !deliveries.has(keyOf(j)) && !upstreamLate(j));
  for (const j of roots) {
    const b = j.estimate.burst;
    items.push({ kind: 'tminus', target: Date.parse(j.objective.deadline), late: true, label: `${j.name} · ${due(j.objective.deadline)}`,
      sub: `<b>late ${f.dur(-j.estimate.slackSeconds)}</b>${b ? ` · +${b.slots} ${b.pool} closes it, ${f.money(b.cost)}${b.needsApproval ? ' · awaiting you' : ''}` : ` · p90 ${f.clock(j.estimate.etaP90, false)}`}` });
  }
  const used = new Set(items.map((c) => c.label));
  const next = jobs.filter((j) => !fin(j) && !late(j) && j.objective.deadline && sk(j) && !deliveries.has(keyOf(j)) && Date.parse(j.objective.deadline) > f.now())
    .sort((a, b) => Date.parse(a.objective.deadline) - Date.parse(b.objective.deadline));
  for (const j of next) {
    if (items.length >= 3) break;
    const label = `${j.name} · ${due(j.objective.deadline)}`;
    if (used.has(label)) continue;
    items.push({ kind: 'tminus', target: Date.parse(j.objective.deadline), late: false, label,
      sub: `slack ${f.signed(j.estimate.slackSeconds)}${j.units.unevaluated ? ` · ${f.num(j.units.unevaluated)} generators to expand` : ` · p90 ${f.clock(j.estimate.etaP90, false)}`}` });
  }
  const shown = items.slice(0, 3);
  const win = ((s.usage && s.usage.productionWindows) || []).find((w) => w.active);
  if (win) shown.push({ kind: 'day', from: Date.parse(win.from), label: `${win.project} window · production day`, sub: `${win.name.toLowerCase()} · to ${f.dateLine(win.to)}` });
  const fm = s.farm;
  const retrying = jobs.reduce((a, j) => a + retries(j.units), 0);
  const failed = jobs.reduce((a, j) => a + j.units.failed, 0);
  const suspect = (s.nodes || []).filter((n) => n.suspect).map((n) => n.name);
  shown.push({ kind: 'slots', label: 'working now', value: `<span class="t-run">${fm.slotsUsed}</span><small> / ${fm.slotsTotal} slots</small>`,
    sub: [`${f.num(fm.unitsPerHour)} units/h`, retrying ? `<span class="t-temp">${retrying} retrying</span>` : null, suspect.length ? `<span class="t-temp">${suspect.join(', ')} suspect</span>` : null, failed ? `<span class="t-fail">${failed} failed</span>` : null].filter(Boolean).join(' · ') });
  return shown;
}

export default {
  render(ctx) {
    const s = ctx.snap;
    const q = ctx.route.q;
    st.ctx = ctx;
    sweep.feed(Date.parse(s.now), { frozen: staticMode });
    const k = Math.max(0.6, (document.documentElement.clientWidth || 1920) / 1920);
    const scope = s.viewer.user || 'farm';
    useHistory(scope);
    const groups = groupsOf(ctx, s.jobs);
    const shotGroups = groups.filter((g) => g.kind === 'shot');
    // the highlight: a held shot, the tour's shot, or none
    if (q.tour === '1' && !st.timer && !staticMode) st.timer = setInterval(() => { st.tour++; if (st.ctx) st.ctx.refresh(); }, TOUR_MS);
    if (q.tour !== '1' && st.timer) { clearInterval(st.timer); st.timer = 0; }
    const hiKey = q.shot || (q.tour === '1' && shotGroups.length ? shotGroups[st.tour % shotGroups.length].key : null);
    const hiGroup = groups.find((g) => g.key === hiKey);
    const hiSet = new Set(hiGroup ? hiGroup.jobs.map(keyOf) : []);
    // channels: unfinished jobs in dispatch order, then jobs that finished while the wall watched
    const open = s.jobs.filter((j) => !fin(j));
    const recent = s.jobs.filter((j) => fin(j) && samples(keyOf(j)).some((p) => p.v < 0.999));
    const channels = [...open, ...recent].map((j) => channelOf(j, { hi: hiSet.has(keyOf(j)) }));
    // strobe: the highlighted shot's binary-order render, else the most urgent one
    const binR = (j) => !fin(j) && j.order === 'binary' && (ctx.cls(j.class) || {}).lens === 'render';
    const sj = (hiGroup && hiGroup.jobs.find(binR)) || open.find(binR) || open.find((j) => j.order === 'binary');
    fetches(ctx, sj ? keyOf(sj) : null);
    const horizon = Math.max(1, Math.min(48, Number(q.horizon) || 4)) * 3600;
    const book = st.book && st.book.entries ? st.book.entries : null;
    return h('section', { class: 'wall', 'data-key': 'wall', 'aria-label': 'The wall: every job against its deadline' },
      own('clocks', 'w:clocks', { items: clocks(ctx, s, groups) }, { class: 'wclocks', role: 'group', 'aria-label': 'Deadline clocks' }),
      own('timebase', 'w:tb', { channels, now: Date.parse(s.now), horizon, scope }, { class: 'wscope' }),
      h('div', { class: 'wband wstrobe' },
        own('strobe', 'w:strobe', { detail: st.strobe && sj && keyOf(st.strobe) === keyOf(sj) ? { ...st.strobe, unitStates: st.strobe.unitStates } : null, title: sj ? sj.name : '' }, { class: 'wstrobe-c' })),
      h('div', { class: 'wband wshots' },
        h('div', { class: 'wlab' }, h('div', { class: 'k' }, q.tour === '1' ? 'The shots · touring' : 'The shots, collapsed'),
          h('div', { class: 's' }, 'each shot is one block: the aggregate of everything in it and upstream of it')),
        h('div', { class: 'wcards', role: 'list' }, groups.slice(0, 7).map((g) => shotCard(g, g.key === hiKey, k)))),
      book ? own('ticker', 'w:tick', { entries: book }, { class: 'wtick', 'aria-label': 'The book' }) : h('div', { class: 'wtick empty', 'data-key': 'w:tick-empty' }));
  },

  leave() {
    if (st.timer) { clearInterval(st.timer); st.timer = 0; }
  },
};
