// Core of the GRATICULE components: tokens for drawing, work-weighted
// aggregation, and the glance numbers every gauge reads.

// tok reads the design tokens (design/tokens.css) for canvas and SVG
// drawing. Cached; call tok(true) after a scale change.
let cache = null;
export function tok(fresh = false) {
  const scale = document.documentElement.dataset.scale || '';
  if (cache && !fresh && cache.scale === scale) return cache;
  const cs = getComputedStyle(document.documentElement);
  const v = (n) => cs.getPropertyValue(n).trim();
  const g = [];
  for (let i = 0; i < 20; i++) g.push(v(`--g${String(i).padStart(2, '0')}`));
  cache = {
    g, done: v('--done'), settled: v('--done-settled'), run: v('--run'), runBody: v('--run-body'),
    temp: v('--temp'), tempBody: v('--temp-body'), fail: v('--fail'),
    field: v('--field'), well: v('--well'), plate: v('--plate'), plate2: v('--plate-2'),
    rule0: v('--rule-0'), rule1: v('--rule-1'), rule2: v('--rule-2'), neat: v('--neat'),
    ink: v('--ink'), ink2: v('--ink-2'), ink3: v('--ink-3'), ink4: v('--ink-4'), pencil: v('--pencil'), paper: v('--paper'),
    sans: v('--sans') || 'IBM Plex Sans Condensed', mono: v('--mono') || 'IBM Plex Mono', scale,
  };
  return cache;
}

export const reducedMotion = () =>
  window.matchMedia('(prefers-reduced-motion: reduce)').matches || document.documentElement.dataset.motion === 'off';

// A unit as the components draw it.
//   { state, fraction?, measured?, attempts?, risk?, settled?, work?: { spent, expected } }
// state is a unitStates key or 'held' or 'ghost' (expected, not yet real).

// agg sums a set of units into the shares a gauge draws. Shares are by
// work (slot-seconds) when every unit carries work, else by unit count,
// and `basis` says which.
export function agg(units) {
  const a = { n: 0, basis: 'estimate', total: 0, done: 0, runSpent: 0, runAll: 0, running: 0, retrying: 0, failed: 0, risk: 0, uneval: 0, waiting: 0 };
  let byWork = true;
  for (const u of units) if (u && u.state !== 'ghost' && !(u.work && u.work.expected > 0)) { byWork = false; break; }
  if (!byWork) a.basis = 'units';
  for (const u of units) {
    if (!u || u.state === 'ghost') continue;
    a.n++;
    const w = byWork ? u.work.expected : 1;
    const spent = byWork ? (u.work.spent || 0) : (u.fraction || 0);
    a.total += w;
    switch (u.state) {
      case 'succeeded': a.done += w; break;
      case 'retrying': a.retrying++; // falls through: a retry is running
      case 'running': a.running++; a.runAll += w; a.runSpent += Math.min(w, spent); break;
      case 'failed': a.failed++; break;
      case 'unevaluated': a.uneval++; a.waiting += w; break;
      default: a.waiting += w;
    }
    if (u.risk && u.state !== 'succeeded' && u.state !== 'failed') a.risk++;
  }
  a.fDone = a.total ? a.done / a.total : 0;
  a.fRunSpent = a.total ? a.runSpent / a.total : 0;
  a.fRun = a.total ? a.runAll / a.total : 0;
  return a;
}

// fromJob builds the same aggregate from a JobSummary's counts and work.
export function aggJob(j) {
  const u = j.units, w = j.work;
  const a = { n: u.total, running: u.running + u.retrying, retrying: u.retrying, failed: u.failed, uneval: u.unevaluated, risk: j.estimate && !j.estimate.feasible ? 1 : 0 };
  if (w && (w.doneSlotSeconds + w.runningSlotSeconds + w.remainingSlotSeconds) > 0) {
    const total = w.doneSlotSeconds + w.runningSlotSeconds + w.remainingSlotSeconds;
    a.basis = w.basis || 'estimate';
    a.total = total;
    a.fDone = w.doneSlotSeconds / total;
    a.fRunSpent = w.runningSlotSeconds / total;
    a.fRun = (w.runningSlotSeconds + (w.runningRemainingSlotSeconds || 0)) / total;
  } else {
    a.basis = 'units';
    a.total = u.total;
    a.fDone = u.total ? u.succeeded / u.total : 0;
    a.fRun = u.total ? (u.running + u.retrying) / u.total : 0;
    a.fRunSpent = a.fRun * 0.5;
  }
  return a;
}

// glance is what a PYLON reads from a JobSummary (and, if given, its detail).
//   done:   share done, by work where the estimate exists
//   run:    share of the work in flight; runSpent the part of it already done
//   par:    share of the job's units running now ("10% in parallel")
export function glance(j, detail) {
  const a = aggJob(j);
  const cs = (detail && detail.classSummary) || {};
  const e = j.estimate || {};
  const g = {
    name: j.name, basis: a.basis,
    done: a.fDone, run: a.fRun, runSpent: a.fRunSpent,
    par: j.units.total ? (j.units.running + j.units.retrying) / j.units.total : 0,
    retry: j.units.total ? j.units.retrying / j.units.total : 0,
    fail: j.units.total ? j.units.failed / j.units.total : 0,
    risk: 0,
    eta: e.remainingSeconds, p90: e.p90RemainingSeconds, slack: e.slackSeconds, feasible: e.feasible !== false,
    req: e.requiredSlots || 0, att: e.attainableSlots || 0, slots: e.runningSlots || 0,
    deadline: j.objective && j.objective.deadline ? Date.parse(j.objective.deadline) : null,
    phase: j.phase,
  };
  if (cs.totalSteps) { // ML: progress is steps
    g.done = cs.step / cs.totalSteps;
    g.basis = 'steps';
    if (j.phase !== 'Running') { g.run = 0; g.runSpent = 0; g.par = 0; }
  }
  if (detail && detail.riskUnits) {
    let n = 0;
    for (const p of detail.riskUnits.split(',')) { if (!p) continue; const [x, y] = p.split('-').map(Number); n += (y === undefined ? 0 : y - x) + 1; }
    g.risk = j.units.total ? n / j.units.total : 0;
  } else if (!g.feasible) g.risk = Math.max(0, 1 - g.done - g.run) * 0.5;
  return g;
}

export const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
