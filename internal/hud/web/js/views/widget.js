// The side-monitor widget, #/widget/<ns>/<name>: one job as a small sheet
// for a second screen or a docked browser window, 320 to 420 px wide (and a
// 240 px corner version with ?compact=1). The collar names the job, its
// owner and the datum; a mini PYLON and three numbers (done by work, the
// share running now, about how long); the shot collapsed to batches on one
// line; the deadline; the upstream and downstream jobs at its edges.
//
// Three states read differently and nothing else changes between them:
//   on time            quiet: greys, green, blue only on the running share
//   temporary trouble  an orange tick appears (pylon edge, a band, its frame label)
//   late               red: the neatline goes red pencil, and the fix and its price are named

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get } from '../api.js';
import { own } from '../../design/components/live.js';
import { tok, glance } from '../../design/components/core.js';
import { drawAgg } from '../../design/components/stack.js';
import { ranges } from '../../design/components/rle.js';

const st = { key: null, detail: null, graph: null, now: null, busy: false, err: null };
document.addEventListener('hud:changed', () => { st.now = null; });

const keyOf = (j) => `${j.namespace}/${j.name}`;

function ensure(ctx, k) {
  const now = ctx.snap.now;
  if (st.busy || st.now === now) return;
  st.busy = true;
  st.now = now;
  Promise.all([
    get(`/jobs/${k}`).then((d) => { st.detail = d; st.err = null; }, (e) => { st.err = e.message; }),
    get('/graph').then((g) => { st.graph = g; }, () => {}),
  ]).finally(() => { st.busy = false; if (st.key === k) ctx.refresh(); });
}

// units of a job detail with the live state, progress and risk applied
function unitsOf(d) {
  const state = new Map();
  for (const [s, set] of Object.entries(d.unitStates || {})) for (const [a, b] of ranges(set)) for (let i = a; i <= b; i++) state.set(i, s);
  if (d.nudge && d.nudge.hold) for (const [i, s] of state) if (s === 'queued') state.set(i, 'held');
  const risk = new Set();
  for (const [a, b] of ranges(d.riskUnits || '')) for (let i = a; i <= b; i++) risk.add(i);
  const prog = d.progress || {};
  if (d.unitList && d.unitList.length) return d.unitList.map((u) => ({ ...u, state: state.get(u.i) || u.state, fraction: prog[u.i] ? prog[u.i].fraction : null, risk: risk.has(u.i) || !!u.risk }));
  const out = [];
  for (const g of d.groups || []) { let k = 0; for (const [a, b] of ranges(g.units)) for (let i = a; i <= b; i++) out.push({ i, group: g.key, pos: k++, label: String(i), state: state.get(i) || 'queued', risk: risk.has(i), attempts: 0 }); }
  return out;
}

// bandsOf collapses the job to batches: frames in bands of axis.batch (at
// most sixteen bands), else one band per group (CI stages, ML ranks).
export function bandsOf(d, maxBands = 16) {
  const us = unitsOf(d);
  const ax = d.axis || {};
  const bands = [];
  if (ax.kind === 'frame') {
    const N = Math.max(1, ...us.map((u) => (u.pos || 0) + 1));
    let per = ax.batch || 24;
    while (Math.ceil(N / per) > maxBands) per *= 2;
    const start = ax.start || 0, step = ax.step || 1;
    for (let k = 0; k < Math.ceil(N / per); k++) {
      const a = k * per, z = Math.min(N, (k + 1) * per) - 1;
      const inB = us.filter((u) => u.pos >= a && u.pos <= z);
      bands.push({ units: inB, from: start + a * step, to: start + z * step, label: String(start + a * step) });
    }
    return { bands, per, unit: 'frames', from: start, to: start + (N - 1) * step };
  }
  for (const g of d.groups || []) {
    const inB = us.filter((u) => u.group === g.key);
    bands.push({ units: inB, label: g.label || g.key });
  }
  return { bands, per: null, unit: (ax.kind || 'unit') + 's' };
}

function markOf(b) {
  const fail = b.units.find((u) => u.state === 'failed');
  const retry = b.units.find((u) => u.state === 'retrying');
  const m = fail || retry;
  return m ? { label: m.label, state: m.state } : null;
}

function drawStrip(cv, bands) {
  const T = tok();
  const w = cv.clientWidth || 280, bh = Number(cv.dataset.h) || 24, lh = 11, H = bh + lh;
  const dpr = devicePixelRatio || 1;
  cv.width = Math.round(w * dpr); cv.height = Math.round(H * dpr);
  cv.style.height = H + 'px';
  const g = cv.getContext('2d');
  g.setTransform(dpr, 0, 0, dpr, 0, 0);
  g.clearRect(0, 0, w, H);
  const n = bands.length, gap = n > 10 ? 2 : 3, bw = (w - gap * (n - 1)) / Math.max(1, n);
  bands.forEach((b, k) => {
    const x = k * (bw + gap);
    drawAgg(g, T, { x, y: 0, w: bw, h: bh, units: b.units, small: bw < 30 });
    const m = markOf(b);
    g.font = `400 8px ${T.mono}`; g.textBaseline = 'alphabetic';
    g.fillStyle = m ? (m.state === 'failed' ? T.fail : T.temp) : T.g[13];
    const txt = m ? m.label : b.label;
    if (bw >= g.measureText(txt).width + 2 || m) g.fillText(txt, x, bh + lh - 2);
  });
}

function neighbours(ctx, k, j, graph) {
  const g = graph || { edges: [] };
  const up = [...new Set(g.edges.filter((e) => e.to === k).map((e) => e.from))];
  const down = [...new Set(g.edges.filter((e) => e.from === k).map((e) => e.to))];
  const desc = (x, dir) => {
    const o = ctx.job(x);
    const name = f.jobName(x);
    if (!o) return [name];
    const late = o.phase !== 'Succeeded' && Number.isFinite(o.estimate.slackSeconds) && o.estimate.slackSeconds < 0;
    if (o.phase === 'Succeeded') return [h('i', { class: 'sq settled' }), `${name} · done ${f.clock(o.estimate.etaP90, false)}`];
    const what = dir === 'down' ? (o.phase === 'Blocked' ? 'waits' : o.phase.toLowerCase()) : o.phase.toLowerCase();
    return [`${name} · ${what}`, late ? h('span', { class: 't-fail' }, ` · ≈${f.dur(-o.estimate.slackSeconds)} late`) : null];
  };
  const inputs = (j.inputs || []).filter((x) => x.materialization && !x.fromJob).map((x) => x.materialization);
  return {
    up: up.length ? ['UPSTREAM ', up.map((x, i) => [i ? ' · ' : '', desc(x, 'up')])] : inputs.length ? `INPUTS ${inputs.join(' · ')}` : 'NO UPSTREAM',
    down: down.length ? ['DOWNSTREAM ', down.map((x, i) => [i ? ' · ' : '', desc(x, 'down')])] : 'DOWNSTREAM to the client',
  };
}

function fixBlock(ctx, j) {
  const e = j.estimate;
  const b = e.burst;
  const p = (ctx.snap.projects || []).find((x) => x.name === j.project);
  if (!b) return h('div', { class: 'wg-fix', 'data-key': 'wg-fix' }, h('div', { class: 'a' }, h('span', null, 'No burst closes it')), h('div', { class: 'b' }, e.message || ''));
  const after = p ? p.burstSpendToday + b.cost : null;
  const over = p && after > p.burstBudgetPerDay ? after - p.burstBudgetPerDay : 0;
  const pending = (ctx.snap.capacity || []).some((c) => c.pool === b.pool && c.state === 'PendingApproval');
  return h('div', { class: 'wg-fix', 'data-key': 'wg-fix' },
    h('div', { class: 'a' }, h('span', null, `+${b.slots} ${b.pool} slots closes it`), h('span', { class: 'p pencil' }, `est. ${f.money(b.cost)}`)),
    h('div', { class: 'b' }, b.needsApproval ? 'awaiting approval' : 'within the burst budget; the queue asks for it',
      p ? ` · ${p.name} burst today ${f.money(p.burstSpendToday)} of ${f.money(p.burstBudgetPerDay)}` : '',
      p && b.needsApproval ? `; approved, ${f.money(after)}${over ? `, ${f.money(over)} over` : ''}` : ''),
    b.needsApproval && pending ? h('button', { class: 'btn', 'data-action': 'approve', 'data-pool': b.pool }, `Approve · est. ${f.money(b.cost)}`) : null);
}

export default {
  render(ctx) {
    const [ns, name] = ctx.route.parts;
    const k = `${ns}/${name}`;
    if (st.key !== k) Object.assign(st, { key: k, detail: null, graph: null, now: null, err: null });
    const compact = ctx.route.q.compact === '1' || new URLSearchParams(location.search).get('compact') === '1';
    document.documentElement.dataset.widget = compact ? 'compact' : 'side';
    ensure(ctx, k);
    const s = ctx.job(k);
    const d = st.detail;
    if (!s && !d) return h('div', { class: 'wg' + (compact ? ' compact' : '') }, h('div', { class: 'wg-sheet' }, h('div', { class: 'wg-empty' }, st.err ? `No job named ${name} in your projects.` : 'Reading the job…')));
    const j = d ? { ...d, ...(s || {}), unitStates: (s || d).unitStates } : s;
    return widgetSheet(ctx, k, j, d, st.graph, compact);
  },

  after(ctx, root) {
    if (!st.detail) return;
    const s = ctx.job(st.key);
    paintStrip(root, { ...st.detail, ...(s || {}), unitStates: (s || st.detail).unitStates }, document.documentElement.dataset.widget === 'compact');
  },

  leave() { delete document.documentElement.dataset.widget; },
};

// paintStrip draws the batches strip of a rendered widget.
export function paintStrip(root, j, compact) {
  const cv = root.querySelector('canvas.wg-stripcv');
  if (cv) drawStrip(cv, bandsOf(j, compact ? 8 : 12).bands);
}

// widgetSheet renders the widget for a job: j is the summary merged over
// its detail d (d may be null while it loads), graph the job graph.
export function widgetSheet(ctx, k, j, d, graph, compact) {
  const g = glance(j, d);
  const e = j.estimate;
  const done = j.phase === 'Succeeded';
  const late = !done && Number.isFinite(e.slackSeconds) && e.slackSeconds < 0 && !!(j.objective && j.objective.deadline);
  const trouble = j.units.retrying > 0;
  const B = d ? bandsOf(j, compact ? 8 : 12) : null;
  const nb = neighbours(ctx, k, j, graph);
  const dl = j.objective && j.objective.deadline;
  const slack = !dl || done ? null : late
    ? h('span', { class: 'rpen' }, `≈${f.dur(-e.slackSeconds).replace(/^(\d+)m$/, '$1 min')} late`)
    : h('span', { class: 'pencil' }, f.signed(e.slackSeconds));
  const failed = (j.unitStates.failed || '') && ranges(j.unitStates.failed);
  const firstFail = d && failed && failed.length ? (unitsOf(j).find((u) => u.i === failed[0][0]) || {}).label : null;
  const retr = d ? unitsOf(j).find((u) => u.state === 'retrying') : null;
  const pct = Math.round(g.done * 100), par = Math.max(g.par > 0 ? 1 : 0, Math.round(g.par * 100));
  const sheetHref = ctx.href(`job/${k}`);
  const datum = f.clockSec(ctx.snap.now);
  const pyl = own('pylon', `wg-pylon:${k}:${compact ? 'c' : 's'}`, { glance: g, w: compact ? 34 : 50, h: compact ? 96 : 150, mode: 'mini' }, { class: 'wg-pylon' });
  const basis = j.work && j.work.basis === 'estimate' ? 'by work' : 'by unit count';
  return h('div', { class: `wg${compact ? ' compact' : ''}${late ? ' late' : trouble ? ' trouble' : ''}`, 'data-key': 'wg-' + k, role: 'region', 'aria-label': `${j.name}: ${pct}% done${late ? ', projected late' : ''}` },
    compact ? null : h('div', { class: 'wg-edge l' }, h('span', null, nb.up)),
    compact ? null : h('div', { class: 'wg-edge r' }, h('span', null, nb.down)),
    h('div', { class: 'wg-sheet' + (late ? ' late' : '') },
      h('div', { class: 'wg-collar' },
        h('a', { class: 'nm', href: sheetHref, target: '_blank', rel: 'noopener', title: 'Open the job’s sheet' }, j.name, compact || !j.owner ? null : h('span', null, ` · ${j.owner}`)),
        h('span', { class: 'mt' }, compact ? datum.slice(0, 5) : `DATUM ${datum}`)),
      h('div', { class: 'wg-body' },
        pyl,
        h('div', { class: 'nums' },
          h('div', { class: 'r1' },
            h('div', null, compact ? null : h('div', { class: 'lbl' }, 'done'), h('div', { class: 'big' }, String(pct), h('small', null, '%'))),
            h('div', { class: 'runs' }, compact ? null : h('div', { class: 'lbl' }, 'running now'),
              h('div', { class: 'v' + (par ? '' : ' idle') }, h('i', { class: 'par', 'aria-hidden': 'true' }), `${par}%`))),
          done ? h('div', { class: 'r2' }, h('span', { class: 'eta ink' }, `done ${f.clock(e.etaP90, false)}`))
            : j.phase === 'Held' ? h('div', { class: 'r2' }, h('span', { class: 'eta' }, 'held'), h('span', { class: 'sm pencil' }, `released now ≈${f.clock(e.etaP90, false)}`))
            : h('div', { class: 'r2' }, h('span', { class: 'eta pencil' + (late ? ' rpen' : '') }, f.approx(e.remainingSeconds)), compact ? null : h('span', { class: 'sm pencil' }, `p90 ${f.dur(e.p90RemainingSeconds)}`)),
          h('div', { class: 'r3' }, dl ? h('span', { class: 'ink' }, `due ${f.clock(dl, false)}`) : h('span', { class: 'dimmer' }, 'no deadline'), slack),
          late && !compact && (j.units.failed || j.units.retrying) ? h('div', { class: 'r3 small' },
            j.units.failed ? h('span', { class: 't-fail' }, `${j.units.failed} failed${firstFail ? ` · ${firstFail}` : ''}`) : h('span'),
            j.units.retrying ? h('span', { class: 't-temp' }, `${j.units.retrying} retrying${retr ? ` · ${retr.label}` : ''}`) : null) : null)),
      B ? h('div', { class: 'wg-strip' },
        compact ? null : h('div', { class: 'h' }, h('span', null, B.unit === 'frames' ? `${B.from} to ${B.to} · ${B.bands.length} bands` : `${B.bands.length} ${B.bands.length === 1 ? 'group' : 'groups'}`, j.units.unevaluated ? ` · ${j.units.unevaluated} not yet expanded` : ''), h('span', null, j.shot ? `shot ${j.shot}` : '')),
        h('canvas', { class: 'wg-stripcv', 'data-key': 'strip-' + k, 'data-h': compact ? 16 : 24, role: 'img', 'aria-label': `${B.bands.length} bands: ${B.bands.map((b) => b.label).join(', ')}` })) : h('div', { class: 'wg-strip dimmer' }, 'reading the batches…'),
      late && !compact ? fixBlock(ctx, j) : null,
      late && compact && e.burst ? h('div', { class: 'wg-fixc' }, h('span', null, `+${e.burst.slots} ${e.burst.pool} closes it`), h('span', { class: 'pencil' }, `est. ${f.money(e.burst.cost)}`)) : null,
      compact ? null : h('div', { class: 'wg-collar bot' },
        h('span', { class: 'mt' }, B && B.per ? `1 band = ${B.per} frames · ${basis}` : basis),
        h('span', { class: 'mt' }, j.objective && j.objective.from ? `deadline: from ${f.jobName(j.objective.from)}` : dl ? 'deadline: set on the job' : ''))));
}
