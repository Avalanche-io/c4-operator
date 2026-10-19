// DISPATCH: the pieces of the farm sheet (the queue) and the wall that read
// a JobSummary alone, with no detail fetch, so dozens of rows stay light.
//
//   batchStrip(j)         the job's units collapsed to batches: each cell an
//                         aggregate (green done from the bottom, a blue sliver
//                         running, orange and red ticks); a dashed red rule
//                         under waiting work projected late. By units.
//   datumBar(j)           slack against the datum (the deadline is sea level):
//                         height above it in pencil, depth below it in red
//   shotsOf(jobs)         every shot (JobSummary.shot, else the name's sh040
//                         prefix) with its jobs, the deliveries it feeds, and
//                         one glance for the whole shot
//   shotGlance(jobs)      a PYLON glance summed over jobs, by work when every
//                         job carries `work`, else by units
//   burstDecision(snap,j) what approving a pending burst costs and changes
//
// SVG is returned as strings, one element per row; the views wrap them.

import { tok, glance, esc, steady, inFlight, retries, willMiss as willMissOf } from './core.js';
import * as f from './fmt.js';
import { ranges } from './rle.js';

const keyOf = (j) => `${j.namespace}/${j.name}`;
const done = (j) => j.phase === 'Succeeded';

// ------------------------------------------------------------ batch strip

const STATES = ['succeeded', 'running', 'retrying', 'failed', 'flagged', 'unevaluated', 'planned', 'queued', 'blocked', 'waits'];
// the strip's slot for each state: flagged needs a person like failed, planned is expected work
const SLOT = { queued: 'waiting', blocked: 'waiting', waits: 'waiting', flagged: 'failed', planned: 'unevaluated' };

// batchSize: frames per display batch for a render job (24, a second of
// screen time), else whatever keeps the strip under `max` cells.
export function batchSize(j, max = 40, lens = '') {
  const n = j.units.total || 0;
  let b = lens === 'render' && n >= 48 ? 24 : 1;
  while (n / b > max) b *= 2;
  return b;
}

// buckets counts each state per batch from the run-length sets: O(ranges + cells).
export function buckets(j, size) {
  let max = (j.units.total || 0) - 1;
  for (const k of STATES) for (const [, b] of ranges((j.unitStates || {})[k])) max = Math.max(max, b);
  const n = Math.max(1, Math.ceil((max + 1) / size));
  const out = Array.from({ length: n }, () => ({ n: 0, succeeded: 0, running: 0, retrying: 0, failed: 0, unevaluated: 0, waiting: 0 }));
  // the operator sends planned and, for older clients, the same set as
  // unevaluated: count it once
  const us = j.unitStates || {};
  const skip = us.unevaluated && us.planned ? 'planned' : null;
  for (const k of STATES) {
    if (k === skip) continue;
    const slot = SLOT[k] || k;
    for (const [a, b] of ranges((j.unitStates || {})[k])) {
      for (let c = Math.floor(a / size); c <= Math.floor(b / size); c++) {
        const lo = Math.max(a, c * size), hi = Math.min(b, c * size + size - 1);
        if (hi < lo) continue;
        out[c][slot] += hi - lo + 1;
        out[c].n += hi - lo + 1;
      }
    }
  }
  return out;
}

export function batchStrip(j, o = {}) {
  const T = tok();
  const W = o.w || 200, H = o.h || 22;
  const size = o.size || batchSize(j, Math.max(4, Math.floor(W / 5)), o.lens);
  const cells = buckets(j, size);
  const gap = cells.length > 1 ? (W / cells.length > 6 ? 2 : 1) : 0;
  const cw = (W - gap * (cells.length - 1)) / cells.length;
  const settled = done(j);
  const held = j.nudge && j.nudge.hold;
  const late = !settled && Number.isFinite(j.estimate.slackSeconds) && j.estimate.slackSeconds < 0;
  const willMiss = late && willMissOf(j.estimate);
  const h = H - 4;
  let s = '';
  cells.forEach((c, k) => {
    const x = k * (cw + gap);
    s += `<rect x="${x.toFixed(2)}" y="0" width="${cw.toFixed(2)}" height="${h}" fill="${T.g[9]}"/>`;
    if (!c.n) { s += `<rect x="${(x + 0.5).toFixed(2)}" y=".5" width="${Math.max(0, cw - 1).toFixed(2)}" height="${h - 1}" fill="none" stroke="${T.g[11]}" stroke-dasharray="2 2"/>`; return; }
    const y = (share) => h * share;
    const hd = y(c.succeeded / c.n), hr = y((inFlight(c)) / c.n), hu = y(c.unevaluated / c.n);
    if (hu) s += `<rect x="${x.toFixed(2)}" y="0" width="${cw.toFixed(2)}" height="${hu.toFixed(2)}" fill="${T.g[8]}"/>`;
    if (hd) s += `<rect x="${x.toFixed(2)}" y="${(h - hd).toFixed(2)}" width="${cw.toFixed(2)}" height="${hd.toFixed(2)}" fill="${T.done}"/>`;
    if (hr) {
      s += `<rect x="${x.toFixed(2)}" y="${(h - hd - hr).toFixed(2)}" width="${cw.toFixed(2)}" height="${hr.toFixed(2)}" fill="${T.run}"/>`;
      s += `<rect x="${x.toFixed(2)}" y="${(h - hd - hr).toFixed(2)}" width="${cw.toFixed(2)}" height="1.5" fill="${T.run}"/>`;
    }
    if (c.failed) s += `<rect x="${(x + cw - Math.min(3, cw)).toFixed(2)}" y="0" width="${Math.min(3, cw).toFixed(2)}" height="${Math.max(3, y(c.failed / c.n)).toFixed(2)}" fill="${T.fail}"/>`;
    if (c.retrying) s += `<rect x="${(x + cw - Math.min(3, cw) - (c.failed ? Math.min(4, cw) : 0)).toFixed(2)}" y="0" width="${Math.min(3, cw).toFixed(2)}" height="${Math.max(3, y(c.retrying / c.n)).toFixed(2)}" fill="${T.temp}"/>`;
    // the deadline, under every batch that still has work waiting: a dashed
    // orchid rule at risk, a solid one when it will miss at any capacity
    if (late && c.waiting + c.unevaluated > 0) s += `<line x1="${x.toFixed(2)}" x2="${(x + cw).toFixed(2)}" y1="${H - 1}" y2="${H - 1}" stroke="${T.risk}" stroke-width="${willMiss ? 2.2 : 1.5}"${willMiss ? '' : ' stroke-dasharray="2 1.5"'}/>`;
  });
  if (held) {
    const hx = W - 9;
    s += `<rect x="${hx - 3}" y="${h / 2 - 6}" width="12" height="12" fill="${T.field}"/><rect x="${hx}" y="${h / 2 - 4}" width="1.5" height="8" fill="${T.g[16]}"/><rect x="${hx + 4}" y="${h / 2 - 4}" width="1.5" height="8" fill="${T.g[16]}"/>`;
  }
  const label = `${j.units.succeeded} of ${j.units.total} units done in batches of ${size}${inFlight(j.units) ? `, ${inFlight(j.units)} running` : ''}${retries(j.units) ? `, ${retries(j.units)} retrying` : ''}${j.units.failed ? `, ${j.units.failed} failed` : ''}`;
  return { svg: `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(label)}">${s}</svg>`, size, cells: cells.length };
}

// ------------------------------------------------------------ the datum

// Slack against the datum. Zero sits at DATUM_X of the column; depth below
// it reaches six hours, height above it three days, on a log scale so a CI
// pipeline's two minutes and a delivery's day share the column.
export const DATUM_X = 0.34;
const NEG = 6 * 3600, POS = 3 * 86400, K = 600;
export function datumX(sec) {
  if (!Number.isFinite(sec)) return DATUM_X;
  const v = (s, span) => Math.min(1, Math.log1p(Math.abs(s) / K) / Math.log1p(span / K));
  return sec < 0 ? DATUM_X * (1 - v(sec, NEG)) : DATUM_X + (1 - DATUM_X) * v(sec, POS);
}
export const DATUM_TICKS = [[-4 * 3600, '−4h'], [-3600, '−1h'], [-900, '−15m'], [0, '0'], [900, '+15m'], [3600, '+1h'], [4 * 3600, '+4h'], [86400, '+1d'], [3 * 86400, '+3d']];

let datumSeq = 0;
export function datumBar(j, o = {}) {
  const T = tok();
  const H = o.h || 22;
  const e = j.estimate;
  const has = j.objective && j.objective.deadline && !done(j) && Number.isFinite(e.slackSeconds);
  let s = '';
  for (const [sec] of DATUM_TICKS) if (sec) s += `<line x1="${(datumX(sec) * 100).toFixed(2)}%" x2="${(datumX(sec) * 100).toFixed(2)}%" y1="${H / 2 - 2}" y2="${H / 2 + 2}" stroke="${T.g[8]}"/>`;
  s += `<line x1="0" x2="100%" y1="${H / 2}" y2="${H / 2}" stroke="${T.g[7]}"/>`;
  if (has) {
    const sl = e.slackSeconds, x0 = DATUM_X * 100, x1 = datumX(sl) * 100;
    const a = Math.min(x0, x1), w = Math.max(0.6, Math.abs(x1 - x0));
    const passed = Date.parse(j.objective.deadline) < f.now();
    if (sl < 0) {
      // depth below the deadline line: a pale orchid box with a dashed edge
      // while projected, hatched when it will miss at any capacity, solid once
      // the deadline has passed
      const id = 'dl' + (++datumSeq);
      s += passed ? `<rect x="${a}%" y="${H / 2 - 4}" width="${w}%" height="8" fill="${T.risk}"/>`
        : willMissOf(e)
          ? `<defs><pattern id="${id}" width="5" height="5" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><rect width="5" height="5" fill="${T.riskPale}"/><rect width="2" height="5" fill="${T.risk}"/></pattern></defs><rect x="${a}%" y="${H / 2 - 4}" width="${w}%" height="8" fill="url(#${id})" stroke="${T.risk}" stroke-width="1.2"/>`
          : `<rect x="${a}%" y="${H / 2 - 4}" width="${w}%" height="8" fill="${T.riskPale}"/><rect x="${a}%" y="${H / 2 - 4}" width="${w}%" height="8" fill="none" stroke="${T.risk}" stroke-dasharray="3 2"/>`;
    } else {
      s += `<rect x="${a}%" y="${H / 2 - 3}" width="${w}%" height="6" fill="${T.g[12]}"/><rect x="${x1}%" y="${H / 2 - 5}" width="1.5" height="10" fill="${T.g[16]}"/>`;
    }
  } else if (done(j)) {
    s += `<rect x="${DATUM_X * 100 - 0.4}%" y="${H / 2 - 3}" width="6" height="6" fill="${T.done}"/>`;
  }
  const label = has ? `slack ${f.signed(e.slackSeconds)}` : done(j) ? 'finished' : 'no deadline';
  return `<svg xmlns="http://www.w3.org/2000/svg" width="100%" height="${H}" role="img" aria-label="${esc(label)}">${s}</svg>`;
}

// ------------------------------------------------------------ shots

// shotKey: the shot label, else (when no job in the snapshot carries one)
// the sh040 or ep104-sh0210 prefix of the name.
export function shotKeyFn(jobs) {
  const labelled = jobs.some((j) => j.shot);
  return (j) => {
    if (labelled) return j.shot || null;
    const m = /^((?:[a-z]{2}\d{2,4}-)?sh\d{2,4})-/.exec(j.name);
    return m ? m[1] : null;
  };
}

export function shotGlance(jobs) {
  const live = jobs.filter((j) => j.units && j.units.total);
  const byWork = live.length && live.every((j) => j.work && j.work.basis === 'estimate');
  let total = 0, dn = 0, run = 0, runSpent = 0;
  let units = 0, par = 0, retry = 0, fail = 0, riskW = 0;
  for (const j of live) {
    const u = j.units, w = j.work;
    units += u.total; par += inFlight(u); retry += retries(u); fail += u.failed;
    let jt, jd, jr, js;
    if (byWork) {
      jt = w.doneSlotSeconds + w.runningSlotSeconds + w.remainingSlotSeconds;
      jd = w.doneSlotSeconds; js = w.runningSlotSeconds; jr = w.runningSlotSeconds + (w.runningRemainingSlotSeconds || 0);
    } else {
      jt = u.total; jd = u.succeeded; jr = inFlight(u); js = jr * 0.5;
    }
    total += jt; dn += jd; run += jr; runSpent += js;
    if (!done(j) && j.estimate && j.estimate.slackSeconds < 0) riskW += Math.max(0, jt - jd - jr) * 0.5;
  }
  const open = jobs.filter((j) => !done(j));
  const worst = open.reduce((m, j) => (m && m.estimate.slackSeconds <= j.estimate.slackSeconds ? m : j), null);
  const last = jobs.reduce((m, j) => (j.objective && j.objective.deadline && (!m || Date.parse(j.objective.deadline) > Date.parse(m.objective.deadline)) ? j : m), null);
  const eta = Math.max(0, ...open.map((j) => j.estimate.remainingSeconds || 0));
  const p90 = Math.max(0, ...open.map((j) => j.estimate.p90RemainingSeconds || 0));
  return steady('shot:' + jobs.map((j) => j.name).sort().join('|'), {
    name: '', basis: byWork ? 'estimate' : 'units',
    done: total ? dn / total : 0, run: total ? run / total : 0, runSpent: total ? runSpent / total : 0,
    par: units ? par / units : 0, retry: units ? retry / units : 0, fail: units ? fail / units : 0, risk: total ? riskW / total : 0,
    eta, p90, slack: worst ? worst.estimate.slackSeconds : NaN, feasible: !worst || worst.estimate.feasible,
    req: 0, att: 0, slots: 0,
    deadline: last ? Date.parse(last.objective.deadline) : null,
    phase: open.length ? 'Running' : 'Succeeded',
    units: { total: units, running: par, retrying: retry, failed: fail },
  }, jobs.reduce((n, j) => n + ((j.units && j.units.succeeded) || 0), 0));
}

// shotsOf groups jobs by shot, in the order of each shot's most urgent job,
// and names the deliveries (jobs outside any shot that depend on its jobs).
export function shotsOf(jobs) {
  const sk = shotKeyFn(jobs);
  const byKey = new Map(jobs.map((j) => [keyOf(j), j]));
  const shots = new Map();
  for (const j of jobs) {
    const k = sk(j);
    if (!k) continue;
    if (!shots.has(k)) shots.set(k, { key: k, jobs: [], deliveries: [] });
    shots.get(k).jobs.push(j);
  }
  // deliveries: unshot jobs reached downstream from a shot's jobs
  const down = new Map();
  for (const j of jobs) for (const d of j.dependsOn || []) { if (!down.has(d)) down.set(d, []); down.get(d).push(keyOf(j)); }
  for (const s of shots.values()) {
    const seen = new Set(s.jobs.map(keyOf));
    const q = [...seen];
    while (q.length) {
      for (const n of down.get(q.shift()) || []) {
        if (seen.has(n)) continue;
        seen.add(n);
        const j = byKey.get(n);
        if (!j) continue;
        if (!sk(j)) s.deliveries.push(j);
        q.push(n);
      }
    }
  }
  const out = [...shots.values()].map((s) => {
    const g = shotGlance(s.jobs);
    const open = s.jobs.filter((j) => !done(j));
    const rank = Math.min(...s.jobs.map((j) => (j.rank > 0 ? j.rank : 1e6)));
    return { ...s, glance: g, open: open.length, late: open.some((j) => j.estimate.slackSeconds < 0), rank, project: s.jobs[0].project };
  });
  out.sort((a, b) => (a.open ? 0 : 1) - (b.open ? 0 : 1) || a.rank - b.rank);
  return out;
}

// ------------------------------------------------------------ burst decisions

// burstDecision: the price of a pending burst, the project's budget after it,
// and the late deliveries downstream that it would bring back on time.
export function burstDecision(snap, j) {
  const b = j.estimate && j.estimate.burst;
  if (!b || !b.needsApproval) return null;
  const p = (snap.projects || []).find((x) => x.name === j.project) || {};
  const spent = Number(p.burstSpendToday) || 0;
  const budget = Number(p.burstBudgetPerDay) || 0;
  const after = spent + (Number(b.cost) || 0);
  const req = (snap.capacity || []).find((c) => c.state === 'PendingApproval' && c.pool === b.pool && (c.jobs || []).includes(keyOf(j)));
  // dependents that are late with it
  const byKey = new Map(snap.jobs.map((x) => [keyOf(x), x]));
  const down = new Map();
  for (const x of snap.jobs) for (const d of x.dependsOn || []) { if (!down.has(d)) down.set(d, []); down.get(d).push(keyOf(x)); }
  const seen = new Set([keyOf(j)]), q = [keyOf(j)], late = [];
  while (q.length) for (const n of down.get(q.shift()) || []) {
    if (seen.has(n)) continue;
    seen.add(n); q.push(n);
    const x = byKey.get(n);
    if (x && !done(x) && x.estimate.slackSeconds < 0 && !(down.get(n) || []).length) late.push(x);
  }
  return {
    job: j, pool: b.pool, slots: b.slots, cost: Number(b.cost) || 0,
    spent, budget, after, over: budget ? Math.max(0, after - budget) : 0,
    until: req ? req.until : null, reason: req ? req.reason : null,
    lateDownstream: late,
  };
}

export function viewerSeal(viewer) {
  const u = (viewer && viewer.user) || 'you';
  const agent = /^agent\//.test(u);
  return { user: u, agent, text: `signs as ${u} via the HUD` };
}

export { keyOf, glance };
