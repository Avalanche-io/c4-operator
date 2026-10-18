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
import { classChip, phaseText, orderText, burstButton, failedUnits } from './common.js';

const st = {
  key: null, details: new Map(), graph: null, book: null, lastNow: null, fetching: false,
  sel: null, unit: null, unitFor: null, unitNow: null, err: null, lens: null,
  lineB: new Set(), contours: true, lineA: true, box: 10,
};

document.addEventListener('hud:changed', () => { st.lastNow = null; st.unitNow = null; });
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
}

function reset(k) {
  Object.assign(st, { key: k, details: new Map(), book: null, lastNow: null, sel: null, unit: null, unitFor: null, unitNow: null, err: null, sheetKeys: null });
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
    tcell('ETA p50 · p90', j.phase === 'Succeeded' ? f.clock(e.etaP90, false) : h('span', { class: 'pencil' }, `${f.clock(e.etaP50, false)} · ${f.clock(e.etaP90, false)}`),
      j.units.unevaluated ? `${j.units.unevaluated} generators to expand` : 'projected', ''),
    tcell('Slack', o.deadline && j.phase !== 'Succeeded' ? h('span', { class: late ? 't-risk pencil' : 'pencil' }, f.signed(e.slackSeconds)) : '–', e.feasible ? 'on track at current capacity' : 'will miss at current capacity', late ? 'late' : ''),
    tcell('Done', `${Math.round(done * 100)}%`, (Math.abs(gl.delivered - gl.done) > 0.005 ? `delivered ${Math.round(gl.delivered * 100)}% · ` : '') + (gl.basis === 'steps' ? `by steps · ${f.num(j.classSummary.step)} of ${f.num(j.classSummary.totalSteps)}` : w && w.basis === 'estimate' ? `by work · ${f.num(j.units.succeeded)} of ${f.num(j.units.total)} units` : `by units · ${f.num(j.units.succeeded)} of ${f.num(j.units.total)}`)),
    level < 4 ? null : tcell('Slots', h('span', null, h('span', { class: 't-run' }, String(e.runningSlots)), h('small', null, ` of ${(+e.requiredSlots).toFixed(1)} needed`)), `can get ${e.attainableSlots}`),
    j.output && level >= 5 ? tcell('Output view', h('span', { title: j.output.view, style: { fontSize: '15px' } }, f.gridRef(j.output.view)), `${f.num(j.output.changesets)} changesets in`) : null);
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

function unitCard(ctx, j) {
  if (!st.sel) return h('div', { class: 'panel', 'data-key': 'unit-hint' }, h('header', null, h('h2', null, 'Unit')),
    h('div', { class: 'body dim' }, ctx.level >= 3
      ? 'Hover any block for what it is; select a unit for its attempts, node, log, record and output. Click a batch or the shot to collapse or open it; the fold mark on a stage folds everything upstream into each frame.'
      : 'Hover a frame for what it is; select one for its attempts, the node it ran on, its log and why it failed.'));
  const u = st.unit;
  if (!u) return h('div', { class: 'panel', 'data-key': 'unit' }, h('div', { class: 'body dim' }, 'Reading the unit…'));
  if (u.error) return h('div', { class: 'panel', 'data-key': 'unit' }, h('div', { class: 'body t-fail' }, u.error));
  const last = u.attempts[u.attempts.length - 1];
  const node = last && last.node;
  const chip = { failed: 'red', accepted: 'red', retrying: 'temp', backoff: 'temp', running: 'run', preflight: 'run', succeeded: 'green', egress: 'green' }[u.state] || '';
  const k = st.sel.job;
  return h('div', { class: 'panel', 'data-key': 'unit', role: 'region', 'aria-label': `Unit ${u.label}` },
    h('header', null, h('h2', null, u.label), h('span', { class: 'chip ' + chip }, u.state), h('span', { class: 'dim mono', style: { fontSize: '10px', marginLeft: 'auto' } }, f.jobName(k))),
    h('div', { class: 'body' },
      h('div', { class: 'mono', style: { fontSize: '11px', lineHeight: '1.6' } },
        h('div', null, h('span', { class: 'dim' }, 'node '), node || '–'),
        h('div', null, h('span', { class: 'dim' }, 'duration '), u.durationSeconds !== undefined ? `${f.dur(u.durationSeconds)} ` : '', h('span', { class: 'pencil' }, `est. ${f.dur(u.estimateSeconds)}`)),
        u.outputChangeset ? h('div', { title: u.outputChangeset }, h('span', { class: 'dim' }, 'output '), f.gridRef(u.outputChangeset)) : null,
        u.expandedInto ? h('div', null, h('span', { class: 'dim' }, 'expanded into '), `units ${u.expandedInto}`) : null),
      h('div', { class: 'book', style: { marginTop: '8px' } }, u.attempts.map((a) => h('div', { class: 'e', 'data-key': 'a' + a.n },
        h('span', { class: 'seq' }, `#${a.n}`), h('span', { class: 'at' }, f.clockSec(a.start).slice(0, 5)), h('i', { class: 'seal node' + (a.end ? '' : ' open') }),
        h('span', { class: 'msg' + (a.exitCode ? ' t-temp' : '') }, `${a.node}${a.end ? (a.exitCode ? ` exit ${a.exitCode}` : ' captured') : ' running'}`),
        a.reason ? h('span', { class: 'who' }, a.reason) : null))),
      u.provenance ? h('div', { class: 'mono', style: { fontSize: '10.5px', marginTop: '8px' } },
        h('span', { class: 'seal node' }), ' ', u.provenance.verified ? 'record verifies' : h('span', { class: 't-fail' }, 'record does not verify'),
        h('span', { class: 'dim' }, ` · ${u.provenance.signer || ''}`)) : null,
      h('div', { class: 'lab', style: { margin: '10px 0 4px' } }, 'Log tail'),
      h('pre', { class: 'mono', style: { margin: 0, fontSize: '10.5px', color: 'var(--ink-2)', background: 'var(--well)', padding: '6px', whiteSpace: 'pre-wrap', maxHeight: '120px', overflow: 'auto' } }, (u.logTail || []).join('\n') || '(no output yet)'),
      h('div', { class: 'controls', style: { marginTop: '8px' } },
        ['failed', 'succeeded'].includes(u.state) ? h('button', { class: 'btn small', 'data-action': 'requeue', 'data-job': k, 'data-units': String(u.i) }, u.state === 'failed' ? 'Requeue unit' : 'Render again') : null,
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
    // CI and ML have their own views at level 5; below it every job opens to its units
    const ownView = cls && (cls.lens === 'ci' || cls.lens === 'ml');
    const main = detail && !(sheet && sheet.cost) && (ownView ? L >= 5 : L >= 3) ? lens.main(ctx, j, st) : [];
    const side = detail && (ownView ? L >= 5 : true) ? lens.side(ctx, j, st) : [];
    const late = j.estimate.slackSeconds < 0 && j.phase !== 'Succeeded';
    return h('section', { class: 'sheet', 'data-key': 'sheet-' + k, 'aria-label': `Sheet ${sheetTitle}` },
      h('div', { class: 'sheethead' },
        h('div', null,
          h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ` / ${j.project} / `, j.shot ? `shot ${j.shot} / ` : '', j.namespace),
          h('h1', null, j.name),
          h('div', { class: 'meta' }, classChip(j, cls), ' ', phaseText(j), ` · ${orderText(j.order)} · ${j.pools.join(' + ')}${j.owner ? ' · ' + j.owner : ''}${j.rank > 0 && L >= 2 ? ` · #${j.rank} in run order` : ''}`)),
        L >= 2 ? titleBlock(j, L) : null),
      j.estimate.message && L >= 2 ? h('div', { class: 'warn' + (j.phase === 'Failed' ? ' red' : late ? ' risk' : ''), style: { marginBottom: '12px' }, role: 'status' },
        h('span', { class: j.phase === 'Failed' ? 't-fail' : late ? 't-risk' : '' }, j.estimate.message), L >= 4 ? burstButton(j) : null) : null,
      h('div', { class: 'neat', style: { padding: '14px' } },
        L >= 3 ? edges(ctx, j, keys) : null,
        h('div', { class: 'layout-job' },
          h('div', { style: { width: '250px' } },
            L >= 2 ? own('pylon', 'pylon:' + k, { glance: g, w: 250, h: 470, mode: 'full', title: j.name.toUpperCase() }) : null,
            h('div', { class: 'legend', style: { marginTop: '14px', flexDirection: 'column', alignItems: 'flex-start' } }, legend(L))),
          h('div', { class: 'wellbox', 'data-key': 'stackbox' },
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
              h('b', null, f.gridRef(j.output.view)), `${String(j.output.changesets).padStart(4, '0')}+00`, h('span', { class: 'dimmer' }, j.output.view)) : null),
          h('div', null,
            unitCard(ctx, j),
            side,
            L >= 2 ? controls(j, k) : null,
            st.book && st.book.entries && L >= 5 ? h('div', { class: 'panel', 'data-key': 'book' },
              h('header', null, h('h2', null, 'The book'), h('span', { class: 'dim mono', style: { fontSize: '10px' } }, 'append only · signed by whoever acted')),
              h('div', { class: 'body' }, bookList(st.book.entries.slice(-16)))) : null))),
      main.length ? h('div', { style: { marginTop: '14px' } }, main) : null);
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
  const sw = (fill, extra = '') => h('i', { style: { display: 'inline-block', width: '14px', height: '10px', background: fill, boxShadow: extra } });
  return [
    [1, h('span', null, sw('var(--done)'), 'done: committed')],
    [1, h('span', null, sw('linear-gradient(var(--done-pale) 40%, var(--done) 40%)'), 'done, output still moving')],
    [1, h('span', null, sw('linear-gradient(var(--g09) 45%, var(--run) 45%)', 'inset 0 0 0 1px var(--run)'), 'rendering: blue rising')],
    [1, h('span', null, sw('var(--run-pale)', 'inset 0 0 0 1px var(--run)'), 'preparing to render')],
    [1, h('span', null, sw('linear-gradient(var(--g09) 45%, var(--run) 45%)', 'inset 0 0 0 1.5px var(--temp)'), 'retrying')],
    [1, h('span', null, sw('var(--temp-pale)', 'inset 0 0 0 1px var(--temp)'), 'retry scheduled')],
    [1, h('span', null, sw('linear-gradient(225deg, var(--paper) 0 4px, var(--fail) 4px)'), 'failed: needs you')],
    [1, h('span', null, sw('var(--fail-pale)', 'inset 0 0 0 1px var(--paper)'), 'failed, taken or accepted: nobody needs to act')],
    [2, h('span', null, sw('var(--risk-pale)', 'inset 2px 0 0 0 var(--risk)'), 'at risk: projected late')],
    [2, h('span', null, sw('repeating-linear-gradient(45deg, var(--risk-pale) 0 2px, var(--risk) 2px 4px)'), 'will miss at any capacity')],
    [2, h('span', null, sw('var(--g09)', 'inset 3px 0 0 0 var(--risk)'), 'missed the deadline')],
    [2, h('span', null, h('span', { class: 'ink mono', style: { fontSize: '10px', minWidth: '30px' } }, '14:56'), 'measured: solid')],
    [2, h('span', null, h('span', { class: 'pencil mono', style: { fontSize: '10px', minWidth: '30px' } }, '14:58'), 'estimated: lighter, dashed')],
    [1, h('span', null, sw('var(--g09)'), 'waiting')],
    [3, h('span', null, sw('repeating-linear-gradient(45deg, var(--g09) 0 3px, var(--g12) 3px 4px)'), 'blocked')],
    [3, h('span', null, sw('var(--g08)', 'inset 0 0 0 1px var(--g13)'), 'not expanded yet')],
    [3, h('span', null, sw('transparent', 'inset 0 0 0 1px var(--g12)'), 'expected')],
  ].filter(([n]) => level >= n).map(([, el]) => el);
}
