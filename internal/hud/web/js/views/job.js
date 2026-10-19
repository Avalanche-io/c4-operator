// The job view: a sheet. The job's shot (every job carrying the same shot,
// and the deliveries they feed) is laid out as one STACK in the order work
// flows; the collar carries the title block, the PYLON, the unit card, the
// job's book and its controls; the neighbouring sheets are named at the
// edges.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get } from '../api.js';
import { own } from '../../design/components/live.js';
import { glance, inFlight } from '../../design/components/core.js';
import { buildSheet } from '../../design/components/stack.js';
import { bookList } from '../../design/components/ledger.js';
import { lensFor } from '../lenses/index.js';
import { classChip, phaseText, orderText, burstButton, failedUnits, personMark } from './common.js';
import { c4idEl } from '../../design/components/c4id.js';
import { unitWords, how } from '../../design/components/words.js';
import { markSVG } from '../../design/components/marks.js';

const st = {
  key: null, details: new Map(), graph: null, book: null, lastNow: null, fetching: false,
  sel: null, unit: null, unitFor: null, unitNow: null, err: null, lens: null,
  lineB: new Set(), contours: true, lineA: true, box: 10,
};

document.addEventListener('hud:changed', () => { st.lastNow = null; st.unitNow = null; st.unit0Now = null; });
document.addEventListener('stack:select', (e) => {
  const { job, i } = e.detail;
  if (!st.key) return;
  st.sel = { job, i };
  if (job === st.key) history.replaceState(null, '', `#/job/${job}/unit/${i}`);
  document.dispatchEvent(new CustomEvent('hud:refresh'));
});

const keyOf = (j) => `${j.namespace}/${j.name}`;

// sheetJobs: the shot's jobs and the deliveries they feed, in flow order.
function sheetJobs(ctx, k) {
  const jobs = ctx.snap.jobs;
  const me = ctx.job(k);
  const g = st.graph || { nodes: [], edges: [] };
  let set;
  if (me && me.shot) {
    set = new Set(jobs.filter((j) => j.shot === me.shot).map(keyOf));
    for (const e of g.edges) if (set.has(e.from)) { const t = jobs.find((j) => keyOf(j) === e.to); if (t && !t.shot) set.add(e.to); }
  } else {
    set = new Set([k]);
    for (const e of g.edges) { if (e.to === k) set.add(e.from); if (e.from === k) set.add(e.to); }
  }
  // topological order over the graph's edges inside the set
  const list = [...set];
  const indeg = new Map(list.map((x) => [x, 0]));
  for (const e of g.edges) if (set.has(e.from) && set.has(e.to)) indeg.set(e.to, indeg.get(e.to) + 1);
  const out = [], q = list.filter((x) => !indeg.get(x));
  while (q.length) {
    const x = q.shift();
    out.push(x);
    for (const e of g.edges) if (e.from === x && set.has(e.to)) { indeg.set(e.to, indeg.get(e.to) - 1); if (!indeg.get(e.to)) q.push(e.to); }
  }
  for (const x of list) if (!out.includes(x)) out.push(x);
  return out;
}

function ensure(ctx, k) {
  const now = ctx.snap.now;
  if (!st.fetching && st.lastNow !== now) {
    st.lastNow = now;
    st.fetching = true;
    const graphP = st.graph && st.graphNow === now ? Promise.resolve(st.graph) : get('/graph').then((g) => { st.graph = g; st.graphNow = now; return g; }, () => st.graph);
    graphP.then(() => {
      const keys = sheetJobs(ctx, k);
      st.sheetKeys = keys;
      return Promise.all(keys.map((x) => get(`/jobs/${x}`).then((d) => st.details.set(x, d), (e) => { if (x === k) st.err = e.message; })));
    }).then(() => get(`/book?job=${encodeURIComponent(k)}&limit=40`).then((b) => { st.book = b; }, () => { st.book = null; }))
      .finally(() => { st.fetching = false; if (st.key === k) ctx.refresh(); });
  }
  const sel = st.sel;
  if (sel && (st.unitFor !== sel.job + '#' + sel.i || st.unitNow !== now)) {
    const want = sel.job + '#' + sel.i;
    if (st.unitFor !== want) st.unit = null;
    st.unitFor = want;
    st.unitNow = now;
    get(`/jobs/${sel.job}/units/${sel.i}`)
      .then((u) => { if (st.unitFor === want) { st.unit = u; ctx.refresh(); } })
      .catch((e) => { st.unit = { error: e.message }; ctx.refresh(); });
  }
  // a lens that needs unit 0 (ML: rank 0's attempts draw the runs) gets it
  const j = (ctx.snap.jobs || []).find((x) => keyOf(x) === k);
  const cls = j && ctx.cls(j.class);
  const lens = lensFor(cls ? cls.lens : 'frames');
  if (lens.needs && lens.needs(j).unit0 && (st.unit0For !== k || st.unit0Now !== now)) {
    if (st.unit0For !== k) st.unit0 = null;
    st.unit0For = k;
    st.unit0Now = now;
    get(`/jobs/${k}/units/0`).then((u) => { if (st.unit0For === k) { st.unit0 = u; ctx.refresh(); } }, () => {});
  }
}

function reset(k) {
  Object.assign(st, { key: k, details: new Map(), book: null, lastNow: null, sel: null, unit: null, unitFor: null, unitNow: null, unit0: null, unit0For: null, unit0Now: null, err: null, sheetKeys: null });
}

function tcell(label, val, sub, cls = '') {
  return h('div', { class: 'cell' }, h('span', { class: 'lab' }, label), h('span', { class: 'val ' + cls }, val), sub ? h('span', { class: 'sub' }, sub) : null);
}

function titleBlock(j, level = 5) {
  const e = j.estimate, o = j.objective, w = j.work;
  const gl = glance(j, j);
  const done = gl.done;
  const late = Number.isFinite(e.slackSeconds) && e.slackSeconds < 0 && j.phase !== 'Succeeded';
  const src = o.from ? `from ${f.jobName(o.from)}` : o.type === 'latency' ? `ready + ${f.dur(o.latencyTargetSeconds)}` : o.deadline ? 'set on the job' : 'no objective';
  return h('div', { class: 'titleblock' },
    tcell('Deadline', o.deadline ? f.clock(o.deadline) : '–', src),
    e.noEta && j.phase !== 'Succeeded' ? tcell('ETA p50 · p90', '–', e.noEta)
      : tcell('ETA p50 · p90', j.phase === 'Succeeded' ? f.when(e.etaP90) : h('span', { class: 'pencil' }, `${f.when(e.etaP50)} · ${f.when(e.etaP90)}`),
        j.units.unevaluated ? `${j.units.unevaluated} generators to expand` : 'projected', ''),
    tcell('Slack', o.deadline && j.phase !== 'Succeeded' ? h('span', { class: late ? 't-risk pencil' : 'pencil' }, f.signed(e.slackSeconds)) : '–', !o.deadline ? 'no deadline' : e.feasible ? 'on track at current capacity' : 'will miss at current capacity', late ? 'late' : ''),
    tcell('Done', `${Math.round(done * 100)}%`, (Math.abs(gl.delivered - gl.done) > 0.005 ? `delivered ${Math.round(gl.delivered * 100)}% · ` : '') + (gl.basis === 'steps' ? `by steps · ${f.num(j.classSummary.step)} of ${f.num(j.classSummary.totalSteps)}` : w && w.basis === 'estimate' ? `by work · ${f.num(j.units.succeeded)} of ${f.num(j.units.total)} units` : `by units · ${f.num(j.units.succeeded)} of ${f.num(j.units.total)}`)),
    level < 4 ? null : tcell('Slots', h('span', null, h('span', { class: 't-run' }, String(e.runningSlots)), +e.requiredSlots > 0 ? h('small', null, ` of ${(+e.requiredSlots).toFixed(1)} needed`) : h('small', null, ' running')), `can get ${e.attainableSlots}`),
    j.output && level >= 5 ? tcell('Output view', h('span', { style: { fontSize: '15px' } }, c4idEl(j.output.view, { tail: 4, label: 'output view' })), `${f.num(j.output.changesets)} changesets in`) : null);
}

function controls(j, k) {
  const held = j.nudge.hold;
  const dl = j.objective.deadline ? f.inputValue(j.objective.deadline) : '';
  return h('div', { class: 'panel', 'data-key': 'ctl' },
    h('header', null, h('h2', null, 'Controls'), h('span', { class: 'dim mono', style: { fontSize: '10px' } }, 'each one is signed by the HUD, for you')),
    h('div', { class: 'body controls' },
      held ? h('button', { class: 'btn', 'data-action': 'release', 'data-job': k }, 'Release job')
        : h('button', { class: 'btn', 'data-action': 'hold', 'data-job': k, title: 'Release no new units; running units finish' }, 'Hold job'),
      !held && inFlight(j.units) > 0 ? h('button', { class: 'btn', 'data-action': 'holdstop', 'data-job': k }, 'Hold and stop') : null,
      h('span', { class: 'seg' }, [1, 1.25, 1.5, 2].map((b) => h('button', { class: 'btn small', 'data-action': 'boost', 'data-job': k, 'data-value': String(b), 'aria-pressed': Math.abs(j.nudge.boost - b) < 0.01 ? 'true' : 'false' }, `×${b}`))),
      h('input', { type: 'datetime-local', id: 'deadline-input', value: dl, 'data-key': 'dl-' + k, 'aria-label': 'Deadline' }),
      h('button', { class: 'btn small', 'data-action': 'deadline', 'data-job': k, 'data-input': 'deadline-input' }, 'Set deadline'),
      j.units.failed ? h('button', { class: 'btn small', 'data-action': 'requeue', 'data-job': k, 'data-units': failedUnits(j) }, `Requeue failed (${j.units.failed})`) : null,
      (j.blacklist || []).map((n) => h('button', { class: 'btn small', 'data-action': 'unblacklist', 'data-job': k, 'data-node': n, title: 'Blacklisted for this job' }, h('span', { class: 't-temp' }, n), ' allow again'))));
}

// cardUnit: the selected unit with what the job detail knows of it: its
// wait, its review, its interruptions, a soft stop
function cardUnit(u, d) {
  if (!d) return u;
  const row = (d.unitList || []).find((x) => x.i === u.i) || {};
  const wait = [...(d.waits || [])].reverse().find((w) => w.unit === u.i) || null;
  const intr = (d.interruptions || []).filter((x) => x.unit === u.i);
  return { ...row, ...u, state: u.state || row.state, wait, interruptions: intr.length ? intr : null };
}

const CHIP = { 't-fail': 'red', 't-temp': 'temp', 't-run': 'run', 't-done': 'green' };

// stoppedByFarm: an attempt that ended where and when the farm interrupted
// the unit (the job detail's interruptions)
function stoppedByFarm(u, a) {
  if (!a.end || !u.interruptions) return false;
  const end = Date.parse(a.end);
  return u.interruptions.some((x) => x.node === a.node && Math.abs(Date.parse(x.at) - end) < 180e3);
}

function unitCard(ctx, j) {
  const cls = ctx.cls(j.class);
  const lens = cls ? cls.lens : 'frames';
  if (!st.sel) return h('div', { class: 'panel', 'data-key': 'unit-hint' }, h('header', null, h('h2', null, 'Unit')),
    h('div', { class: 'body dim' }, lens === 'plan'
      ? 'Hover a lane for what it is; select one for its steps, attempts, log and output.'
      : ctx.level >= 3
        ? 'Hover any block for what it is; select a unit for its attempts, node, log, record and output. Click a batch or the shot to collapse or open it; the collapse mark on a stage collapses everything upstream into each frame.'
        : 'Hover a frame for what it is; select one for its attempts, the node it ran on, its log and why it needs you.'));
  const raw = st.unit;
  if (!raw) return h('div', { class: 'panel', 'data-key': 'unit' }, h('div', { class: 'body dim' }, 'Reading the unit…'));
  if (raw.error) return h('div', { class: 'panel', 'data-key': 'unit' }, h('div', { class: 'body t-fail' }, raw.error));
  const k = st.sel.job;
  const u = cardUnit(raw, st.details.get(k));
  const last = u.attempts[u.attempts.length - 1];
  const node = last && last.node;
  const wd = unitWords(u);
  return h('div', { class: 'panel', 'data-key': 'unit', role: 'region', 'aria-label': `Unit ${u.label}` },
    h('header', null, h('h2', null, u.label), h('span', { class: 'chip ' + (CHIP[wd.cls] || '') }, wd.word), h('span', { class: 'dim mono', style: { fontSize: '10px', marginLeft: 'auto' } }, f.jobName(k))),
    h('div', { class: 'body' },
      wd.why ? h('div', { class: 'why', style: { fontSize: '12px', marginBottom: '6px' } }, wd.why) : null,
      h('div', { class: 'mono', style: { fontSize: '11px', lineHeight: '1.6' } },
        h('div', null, h('span', { class: 'dim' }, 'node '), node || '–'),
        h('div', null, h('span', { class: 'dim' }, 'duration '), u.durationSeconds !== undefined ? `${f.dur(u.durationSeconds)} ` : '', h('span', { class: 'pencil' }, `est. ${f.dur(u.estimateSeconds)}`)),
        u.outputChangeset ? h('div', null, h('span', { class: 'dim' }, 'output '), c4idEl(u.outputChangeset, { tail: 4, label: 'output changeset' })) : null,
        u.expandedInto ? h('div', null, h('span', { class: 'dim' }, 'expanded into '), `units ${u.expandedInto}`) : null,
        ctx.level >= 5 ? h('div', { class: 'dim', style: { fontSize: '10px' } }, h('span', null, 'how '), how(u)) : null),
      h('div', { class: 'book', style: { marginTop: '8px' } }, u.attempts.map((a) => h('div', { class: 'e', 'data-key': 'a' + a.n },
        h('span', { class: 'seq' }, `#${a.n}`), h('span', { class: 'at' }, f.clockSec(a.start).slice(0, 5)), h('i', { class: 'seal node' + (a.end ? '' : ' open') }),
        // an attempt the farm stopped (an interruption) is not a failure: grey,
        // "interrupted", its exit code only at level 5
        stoppedByFarm(u, a)
          ? h('span', { class: 'msg dim' }, `${a.node} interrupted${ctx.level >= 5 && a.exitCode ? ` (exit ${a.exitCode})` : ''}`)
          : h('span', { class: 'msg' + (a.exitCode ? ' t-temp' : '') }, `${a.node}${a.end ? (a.exitCode ? ` exit ${a.exitCode}` : ' captured') : ' running'}`),
        a.reason ? h('span', { class: 'who' }, a.reason) : null))),
      u.provenance ? h('div', { class: 'mono', style: { fontSize: '10.5px', marginTop: '8px' } },
        h('span', { class: 'seal node' }), ' ', u.provenance.verified ? 'record verifies' : h('span', { class: 't-fail' }, 'record does not verify'),
        h('span', { class: 'dim' }, ` · ${u.provenance.signer || ''}`)) : null,
      h('div', { class: 'lab', style: { margin: '10px 0 4px' } }, 'Log tail'),
      h('pre', { class: 'mono', style: { margin: 0, fontSize: '10.5px', color: 'var(--ink-2)', background: 'var(--well)', padding: '6px', whiteSpace: 'pre-wrap', maxHeight: '120px', overflow: 'auto' } }, (u.logTail || []).join('\n') || '(no output yet)'),
      h('div', { class: 'controls', style: { marginTop: '8px' } },
        ['failed', 'succeeded'].includes(u.state) ? h('button', { class: 'btn small', 'data-action': 'requeue', 'data-job': k, 'data-units': String(u.i) }, u.state === 'failed' ? 'Requeue unit' : (j.class === 'render' ? 'Render again' : 'Run again')) : null,
        node ? h('button', { class: 'btn small', 'data-action': 'blacklist', 'data-job': k, 'data-node': node }, `Avoid ${node}`) : null)));
}

// adjacent sheets: upstream and downstream jobs outside this sheet, other shots of the project
function edges(ctx, j, keys) {
  const g = st.graph || { edges: [] };
  const inS = new Set(keys || []);
  const up = [...new Set(g.edges.filter((e) => inS.has(e.to) && !inS.has(e.from)).map((e) => e.from))];
  const down = [...new Set(g.edges.filter((e) => inS.has(e.from) && !inS.has(e.to)).map((e) => e.to))];
  const inputs = (j.inputs || []).filter((i) => i.materialization).map((i) => i.materialization);
  const shots = [...new Set(ctx.snap.jobs.filter((x) => x.project === j.project && x.shot).map((x) => x.shot))].sort();
  const si = shots.indexOf(j.shot);
  const link = (key) => h('a', { href: ctx.href(`job/${key}`) }, f.jobName(key));
  const shotLink = (s) => { const x = ctx.snap.jobs.find((y) => y.shot === s && y.project === j.project); return x ? h('a', { href: ctx.href(`job/${keyOf(x)}`) }, `shot ${s}`) : s; };
  return [
    h('div', { class: 'edge w' }, '◂ ', up.length ? up.map((x, i) => [i ? ' · ' : '', link(x)]) : inputs.length ? `inputs ${inputs.join(' · ')}` : 'no upstream'),
    h('div', { class: 'edge e' }, down.length ? down.map((x, i) => [i ? ' · ' : '', link(x)]) : 'to the client', ' ▸'),
    si > 0 ? h('div', { class: 'edge n' }, '▴ ', shotLink(shots[si - 1])) : h('div', { class: 'edge n' }, `${j.project} · ${j.namespace}`),
    si >= 0 && si < shots.length - 1 ? h('div', { class: 'edge s' }, '▾ ', shotLink(shots[si + 1])) : null,
  ];
}

export default {
  render(ctx) {
    const [ns, name, sub, idx] = ctx.route.parts;
    const k = `${ns}/${name}`;
    if (st.key !== k) reset(k);
    if (sub === 'unit' && !st.sel) st.sel = { job: k, i: Number(idx) };
    ensure(ctx, k);
    const summary = ctx.job(k);
    const detail = st.details.get(k);
    if (!summary && !detail) {
      return h('section', { class: 'sheet' }, h('div', { class: 'sheethead' }, h('div', null, h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm')), h('h1', null, name || 'Job'),
        h('p', { class: 'dim' }, st.err ? `No job named ${name} in your projects.` : 'Reading the job…'))));
    }
    const j = detail ? { ...detail, ...(summary || {}) } : summary;
    if (detail && summary) j.unitStates = detail.unitStates;
    const cls = ctx.cls(j.class);
    const lens = lensFor(cls ? cls.lens : 'frames');
    st.lens = lens;
    st.pickFrame = (frame) => {
      const d = st.details.get(k);
      if (!d || !d.unitList) return;
      const u = d.unitList.find((x) => x.label.startsWith(String(frame)));
      if (u) { st.sel = { job: k, i: u.i }; ctx.refresh(); }
    };
    const L = ctx.level;
    const keys = L >= 3 ? (st.sheetKeys || [k]) : [k];
    const details = keys.map((x) => st.details.get(x)).filter(Boolean).map((d) => ({ ...d, ...(ctx.job(keyOf(d)) || {}), unitStates: d.unitStates }))
      .map((d) => (L >= 2 ? d : { ...d, riskUnits: '', objective: {}, estimate: { ...(d.estimate || {}), feasible: true } }));
    const sheetTitle = j.shot ? `${j.shot}` : j.name;
    let anchor = k;
    {
      const width = (d) => Math.max(0, ...(d.groups || []).map((g) => (g.units || '').split(',').reduce((n, p) => { const [a, b] = p.split('-').map(Number); return n + (Number.isFinite(b) ? b - a + 1 : Number.isFinite(a) ? 1 : 0); }, 0)));
      const me = details.find((d) => keyOf(d) === k);
      let best = me ? width(me) : 0;
      for (const d of details) if (d.axis && me && d.axis.kind === me.axis.kind && width(d) > best) { best = width(d); anchor = keyOf(d); }
    }
    const sheet = details.length ? buildSheet({ key: `sheet:${keys.join('|')}`, title: sheetTitle, details, anchor, graph: st.graph, jobs: ctx.snap.jobs }) : null;
    const readers = details.filter((d) => (d.unitList || []).some((u) => u.reads));
    if (ctx.route.q && ctx.route.q.reads === '1') for (const d of readers) st.lineB.add(keyOf(d));
    const deadline = j.objective && j.objective.deadline ? Date.parse(j.objective.deadline) : null;
    const g = glance(j, detail);
    // CI, ML and agent bundles have their own views at level 5; below it every job opens to its units
    const ownView = cls && (cls.lens === 'ci' || cls.lens === 'ml' || cls.lens === 'agent' || cls.lens === 'plan');
    const planJob = cls && cls.lens === 'plan';
    const main = detail && !(sheet && sheet.cost) && (ownView ? L >= 5 : L >= 3) ? lens.main(ctx, j, st) : [];
    const side = detail && (ownView ? L >= 5 : true) ? lens.side(ctx, j, st) : [];
    const late = j.estimate.slackSeconds < 0 && j.phase !== 'Succeeded';
    return h('section', { class: 'sheet', 'data-key': 'sheet-' + k, 'aria-label': `Sheet ${sheetTitle}` },
      h('div', { class: 'sheethead' },
        h('div', null,
          h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ` / ${j.project} / `, j.shot ? `shot ${j.shot} / ` : '', j.namespace),
          h('h1', null, j.name),
          h('div', { class: 'meta' }, classChip(j, cls), ' ', phaseText(j), ` · ${orderText(j.order)} · ${j.pools.join(' + ')}${j.owner ? ' · ' + j.owner : ''}${j.rank > 0 && L >= 2 ? ` · #${j.rank} in run order` : ''}`, personMark(j) ? [' ', personMark(j)] : null)),
        L >= 2 ? titleBlock(j, L) : null),
      j.estimate.message && L >= 2 ? h('div', { class: 'warn' + (j.phase === 'Failed' ? ' red' : late ? ' risk' : ''), style: { marginBottom: '12px' }, role: 'status' },
        h('span', { class: j.phase === 'Failed' ? 't-fail' : late ? 't-risk' : '' }, j.estimate.message), L >= 4 ? burstButton(j) : null) : null,
      main.length && ownView ? h('div', { style: { marginBottom: '14px' } }, main) : null,
      h('div', { class: 'neat', style: { padding: '14px' } },
        L >= 3 ? edges(ctx, j, keys) : null,
        h('div', { class: 'layout-job' + (planJob ? ' plan' : '') },
          // a plan job's lens is its picture: no render pylon or shot stack
          planJob ? null : h('div', { style: { width: '250px' } },
            L >= 2 ? own('pylon', 'pylon:' + k, { glance: g, w: 250, h: 470, mode: 'full', title: j.name.toUpperCase() }) : null,
            h('div', { class: 'legend', style: { marginTop: '14px', flexDirection: 'column', alignItems: 'flex-start' } }, legend(L))),
          planJob ? null : h('div', { class: 'wellbox', 'data-key': 'stackbox' },
            h('div', { class: 'tools' },
              h('span', { class: 'lab' }, L >= 3 ? `Shot ${sheetTitle}` : `Job ${j.name}`),
              h('span', { class: 'dim mono', style: { fontSize: '10px' } }, sheet ? (L >= 3 ? `${sheet.cols.length} stages · ${f.num(sheet.N)} rows · ${sheet.batchFromData ? 'batches from the job script' : `${sheet.batch} frames per batch`}` : `${f.num(j.units.total)} units · ${orderText(j.order)}`) : ''),
              h('span', { style: { marginLeft: 'auto' } }),
              h('button', { class: 'btn small', 'data-stack': 'zoom-', title: 'Smaller boxes (-)' }, '−'),
              h('button', { class: 'btn small', 'data-stack': 'zoom+', title: 'Bigger boxes (+)' }, '+'),
              L < 3 ? null : h('label', null, h('input', { type: 'checkbox', 'data-stack': 'lineA', checked: st.lineA ? true : false }), 'other inputs'),
              (L < 3 ? [] : readers).map((d) => h('label', { 'data-key': 'lb-' + keyOf(d) }, h('input', { type: 'checkbox', 'data-stack': 'lineB', 'data-job': keyOf(d), checked: st.lineB.has(keyOf(d)) ? true : false }), `reads · ${d.name.replace(/^sh\d+-/, '')}`)),
              L < 3 ? null : h('label', null, h('input', { type: 'checkbox', 'data-stack': 'contours', checked: st.contours ? true : false }), 'time lines')),
            sheet ? own('stack', 'stack:' + k + (L >= 3 ? '' : ':flat'), { sheet, view: { flat: L < 3, deadline, sel: st.sel, box: st.box, lineA: L >= 3 && st.lineA, lineB: L >= 3 ? st.lineB : new Set(), contours: L >= 3 && st.contours, ...(ctx.route.q && ctx.route.q.collapse === 'shot' ? { shotCollapsed: true } : {}) } }) : h('div', { class: 'body dim' }, 'Reading the job…'),
            j.output && L >= 5 ? h('div', { class: 'edgecode', title: 'The job’s output view: one step per changeset added to its chain' },
              h('b', null, c4idEl(j.output.view, { tail: 4, label: 'output view' })), `${String(j.output.changesets).padStart(4, '0')}+00`) : null),
          h('div', null,
            unitCard(ctx, j),
            side,
            L >= 2 ? controls(j, k) : null,
            st.book && st.book.entries && L >= 5 ? h('div', { class: 'panel', 'data-key': 'book' },
              h('header', null, h('h2', null, 'The book'), h('span', { class: 'dim mono', style: { fontSize: '10px' } }, 'append only · signed by whoever acted')),
              // the book scrolls in its panel, so a long one never pushes the rest down
              h('div', { class: 'body', style: { maxHeight: '420px', overflow: 'auto' } }, bookList(st.book.entries.slice(-16)))) : null))),
      // a class with its own view (CI, ML) puts that view first
      main.length && !ownView ? h('div', { style: { marginTop: '14px' } }, main) : null);
  },

  after(ctx, root) {
    for (const el of root.querySelectorAll('[data-stack]')) {
      if (el.dataset.wired) continue;
      el.dataset.wired = '1';
      el.addEventListener(el.tagName === 'INPUT' ? 'change' : 'click', () => {
        const comp = root.querySelector('[data-comp="stack"]');
        const c = comp && comp.__comp;
        const a = el.dataset.stack;
        if (a === 'zoom+') st.box = Math.min(32, st.box + 2);
        if (a === 'zoom-') st.box = Math.max(3, st.box - 2);
        if (a === 'lineA') st.lineA = el.checked;
        if (a === 'contours') st.contours = el.checked;
        if (a === 'lineB') { if (el.checked) st.lineB.add(el.dataset.job); else st.lineB.delete(el.dataset.job); }
        if (c) { Object.assign(c.view, { box: st.box, lineA: st.lineA, contours: st.contours, lineB: st.lineB }); c.relayout(false); }
      });
    }
    const d = st.details.get(st.key);
    if (st.lens && st.lens.after && d) st.lens.after(ctx, root, { ...d, ...(ctx.job(st.key) || {}), unitStates: d.unitStates }, st);
  },

  leave() {
    if (st.lens && st.lens.leave) st.lens.leave();
  },
};

function legend(level = 5) {
  // a swatch in the cell's drawing; a mark sits in its corner, as on a cell
  const HATCH = 'repeating-linear-gradient(45deg, var(--g09) 0 3px, var(--g12) 3px 4px)';
  const sw = (fill, extra = '', mark = null, ink = 'var(--g17)') => {
    const i = h('i', { style: { position: 'relative', display: 'inline-block', width: '14px', height: '10px', background: fill, boxShadow: extra } });
    if (mark) i.innerHTML = `<span style="position:absolute;right:0;top:0;line-height:0;color:${ink}">${markSVG(mark, { size: 7 })}</span>`;
    return i;
  };
  const RISE = 'linear-gradient(var(--g09) 45%, var(--run) 45%)';
  const notch = (c) => `linear-gradient(${c}, ${c}) right 1px top 1px / 3px 2px no-repeat, var(--g09)`;
  return [
    [1, h('span', null, sw('var(--g09)'), 'queued')],
    [1, h('span', null, sw(HATCH), 'blocked on: earlier work or an input')],
    [1, h('span', null, sw(HATCH, '', 'clock'), 'waits for: capacity, a license, a time')],
    [1, h('span', null, sw('var(--g09)', '', 'pause'), 'held: a person paused it')],
    [1, h('span', null, sw(HATCH, '', 'lock'), 'locked by another unit')],
    [1, h('span', null, sw(notch('var(--g15)')), 'interrupted: the farm stopped it; it runs again')],
    [1, h('span', null, sw('var(--run-pale)', 'inset 0 0 0 1px var(--run)'), 'preparing')],
    [1, h('span', null, sw(RISE, 'inset 0 0 0 1px var(--run)'), 'rendering')],
    [1, h('span', null, sw(RISE, 'inset 0 0 0 1px var(--run)', 'pause', 'var(--paper)'), 'yielding: stops at its next safe point')],
    [1, h('span', null, sw('var(--temp-pale)', 'inset 0 0 0 1px var(--temp)'), 'backoff: a retry is scheduled')],
    [1, h('span', null, sw(RISE, 'inset 0 0 0 1.5px var(--temp)'), 'retrying')],
    [1, h('span', null, sw('var(--temp-pale)', 'inset 0 0 0 1px var(--temp)', 'rework', 'var(--temp)'), 'rework: a check sent it back')],
    [1, h('span', null, sw('linear-gradient(var(--done-pale) 40%, var(--done) 40%)'), 'moving: output still moving')],
    [1, h('span', null, sw('var(--done)'), 'done')],
    [1, h('span', null, sw('var(--done)', '', 'tick', 'var(--field)'), 'checked: a check passed')],
    [1, h('span', null, sw('linear-gradient(225deg, var(--paper) 0 4px, var(--fail) 4px)'), 'needs you')],
    [1, h('span', null, sw('var(--fail-pale)'), 'waits for someone: holds up nothing yet')],
    [1, h('span', null, sw('var(--fail-pale)', '', 'seal', 'var(--paper)'), 'acknowledged: someone has it')],
    [1, h('span', null, sw('var(--fail-pale)', '', 'check', 'var(--paper)'), 'accepted: no one needs to act')],
    [2, h('span', null, sw('var(--risk-pale)', 'inset 2px 0 0 0 var(--risk)'), 'at risk: projected late')],
    [2, h('span', null, sw('repeating-linear-gradient(45deg, var(--risk-pale) 0 2px, var(--risk) 2px 4px)'), 'will miss at any capacity')],
    [2, h('span', null, sw('var(--g09)', 'inset 3px 0 0 0 var(--risk)'), 'missed the deadline')],
    [2, h('span', null, h('span', { class: 'ink mono', style: { fontSize: '10px', minWidth: '30px' } }, '14:56'), 'measured: solid')],
    [2, h('span', null, h('span', { class: 'pencil mono', style: { fontSize: '10px', minWidth: '30px' } }, '14:58'), 'estimated: lighter, dashed')],
    [3, h('span', null, sw('radial-gradient(var(--g13) 0.6px, transparent 0.8px) 0 0 / 3px 3px, var(--g08)', 'inset 0 0 0 1px var(--g12)'), 'expected: work that will exist')],
    [3, h('span', null, h('i', { style: { display: 'inline-block', width: '5px', height: '14px', background: HATCH, margin: '0 4.5px' } }), 'gate: a wait or a check between stages, as long as the rows it holds')],
    [3, h('span', null, h('i', { style: { display: 'inline-block', width: '14px', height: '0', borderTop: '1.5px solid var(--temp)', position: 'relative' } }), 'rework: from the check back to the frame it sent back')],
  ].filter(([n]) => level >= n).map(([, el]) => el);
}
