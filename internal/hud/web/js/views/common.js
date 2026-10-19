// Pieces shared by views: the time lane, chips, and job labels.

import { h, s } from '../dom.js';
import * as f from '../fmt.js';
import { jobWords, who } from '../../design/components/words.js';

export const key = (j) => `${j.namespace}/${j.name}`;

// The lane's time axis is logarithmic past now, so a CI pipeline due in
// twelve minutes and a delivery due on Monday share one column.
const SPAN = 3 * 86400;
const K = 600;
export function laneX(sec) {
  const v = Math.log1p(Math.max(0, sec) / K) / Math.log1p(SPAN / K);
  return Math.min(1, v) * 100;
}
export const LANE_TICKS = [
  [0, 'now'], [15 * 60, '15m'], [3600, '1h'], [4 * 3600, '4h'], [12 * 3600, '12h'], [86400, '1d'], [3 * 86400, '3d'],
];

export function laneHeader() {
  return s('svg', { class: 'lane', height: 16, 'aria-hidden': 'true', style: { height: '16px' } },
    LANE_TICKS.map(([t, label], k) => s('text', {
      x: `${laneX(t)}%`, y: 11, 'text-anchor': k === 0 ? 'start' : k === LANE_TICKS.length - 1 ? 'end' : 'middle',
      style: { font: '10px var(--mono)', 'font-stretch': '75%', fill: 'var(--ink-3)' },
    }, label)));
}

// lane draws one job's objective against time: the deadline as a white
// rule, the ETA band from p50 to p90, yellow past the deadline.
export function lane(j, { tall = false } = {}) {
  const now = f.now();
  const H = tall ? 60 : 34;
  const mid = tall ? 26 : 19;
  const bh = tall ? 10 : 8;
  const kids = [];
  for (const [t] of LANE_TICKS) kids.push(s('line', { x1: `${laneX(t)}%`, x2: `${laneX(t)}%`, y1: mid - 9, y2: mid + 9, stroke: 'var(--rule-soft)' }));
  kids.push(s('line', { x1: '0%', x2: '100%', y1: mid, y2: mid, stroke: 'var(--rule)' }));
  const e = j.estimate;
  const dl = j.objective && j.objective.deadline ? (f.ms(j.objective.deadline) - now) / 1000 : null;
  const p50 = (f.ms(e.etaP50) - now) / 1000;
  const p90 = (f.ms(e.etaP90) - now) / 1000;
  const done = j.phase === 'Succeeded';
  const held = j.phase === 'Held';
  if (done) {
    kids.push(s('rect', { x: 0, y: mid - 3, width: 6, height: 6, fill: 'var(--u-done)' }));
    kids.push(s('text', { x: 12, y: mid + 3.5, style: { font: '10px var(--mono)', 'font-stretch': '75%', fill: 'var(--ink-2)' } }, `done ${f.clock(e.etaP90)}`));
  } else {
    const x50 = laneX(p50);
    const x90 = Math.max(laneX(p90), x50 + 0.4);
    kids.push(s('line', { x1: '0%', x2: `${x50}%`, y1: mid, y2: mid, stroke: 'var(--ink-3)', 'stroke-width': 2 }));
    const xd = dl === null ? 101 : laneX(dl);
    const before = Math.min(x90, xd);
    if (before > x50) {
      kids.push(s('rect', { x: `${x50}%`, y: mid - bh / 2, width: `${before - x50}%`, height: bh,
        fill: held ? 'none' : 'var(--ink-2)', stroke: held ? 'var(--bar-magenta)' : 'none', 'stroke-dasharray': held ? '3 2' : null }));
    }
    if (x90 > xd) {
      const from = Math.max(x50, xd);
      kids.push(s('rect', { x: `${from}%`, y: mid - bh / 2, width: `${Math.max(0.4, x90 - from)}%`, height: bh, fill: 'var(--bar-yellow)' }));
    }
    if (tall) {
      const st = { font: '10px var(--mono)', 'font-stretch': '75%', fill: 'var(--ink-2)' };
      const a = f.clock(e.etaP50, false);
      const b = f.clock(e.etaP90, false);
      const text = a === b ? `ETA ${a}` : `ETA p50 ${a} to p90 ${b}`;
      const xc = (x50 + x90) / 2;
      kids.push(s('text', { x: `${xc}%`, y: mid + bh / 2 + 12, 'text-anchor': xc > 85 ? 'end' : xc < 12 ? 'start' : 'middle', style: st }, text));
    }
  }
  if (dl !== null) {
    const xd = dl < 0 ? 0 : laneX(dl);
    kids.push(s('rect', { x: `${xd}%`, y: mid - 12, width: 2, height: 24, fill: dl < 0 ? 'var(--bar-red)' : 'var(--ink)', transform: 'translate(-1 0)' }));
    kids.push(s('text', {
      x: `${xd}%`, y: mid - 14, 'text-anchor': xd > 88 ? 'end' : xd < 6 ? 'start' : 'middle',
      style: { font: '10px var(--mono)', 'font-stretch': '75%', fill: dl < 0 ? 'var(--risk)' : 'var(--ink)' },
    }, dl < 0 ? `due ${f.clock(j.objective.deadline)}, passed` : f.clock(j.objective.deadline)));
  }
  if (tall) {
    LANE_TICKS.forEach(([t, lab], k) => kids.push(s('text', {
      x: `${laneX(t)}%`, y: H - 1, 'text-anchor': k === 0 ? 'start' : k === LANE_TICKS.length - 1 ? 'end' : 'middle',
      style: { font: '9.5px var(--mono)', 'font-stretch': '75%', fill: 'var(--ink-3)' },
    }, lab)));
  }
  const label = done ? `Finished ${f.clock(e.etaP90)}` :
    `ETA ${f.clock(e.etaP50)} to ${f.clock(e.etaP90)}${dl !== null ? `, due ${f.clock(j.objective.deadline)}` : ''}`;
  return s('svg', { class: 'lane', height: H, style: { height: `${H}px` }, role: 'img', 'aria-label': label }, kids);
}

export function classChip(j, cls) {
  const lens = cls ? cls.lens : '';
  const text = { render: 'Render', ci: 'CI', ml: 'ML' }[lens] || j.class;
  return h('span', { class: 'chip', title: `class ${j.class}` }, text);
}

// phaseText: the job's state in the vocabulary ("blocked on comp", "waits
// for license-nuke"); the native phase only in the title, for level 5.
export function phaseText(j) {
  const w = jobWords(j);
  return h('span', { class: w.cls, title: `phase ${j.phase}` }, w.why ? `${w.word} ${w.why}` : w.word);
}

// personMark: a job that needs a person (needsPerson) in red with a white
// flag, or failures someone acknowledged in pale red with their seal: the
// state is still true, with the context that a person has it in hand.
const NEEDS = {
  failed: (j) => `${j.units.failed} failed`, stuck: () => 'stuck', approval: () => 'approve burst', quota: () => 'no slots it may use',
  permission: () => 'no permission', review: () => 'a check escalated it', decision: (j) => { const w = (j.waits || []).find((x) => x.needsYou && !x.do); return w ? `${who(w.waitsFor)} to decide` : 'a decision'; },
};
export function personMark(j) {
  const np = j.needsPerson;
  if (np) {
    const what = (NEEDS[np.reason] || (() => np.reason))(j);
    return h('span', { class: 'needs', title: `${np.message} (since ${f.clock(np.since)})` }, h('i', { class: 'flag', 'aria-hidden': 'true' }), `needs you · ${what}`);
  }
  const taken = (j.acks || []).filter((a) => a.kind !== 'accept');
  if (j.units.failed && taken.length) {
    const a = taken[taken.length - 1];
    const by = a.kind === 'assign' && a.assignee ? a.assignee : a.actor;
    return h('span', { class: 'taken', title: `${a.kind === 'assign' ? 'assigned to' : 'acknowledged by'} ${by} at ${f.clock(a.at)}${a.note ? ': ' + a.note : ''}` }, h('i', { class: 'seal', 'aria-hidden': 'true' }), `acknowledged · ${by}`);
  }
  return null;
}

export function slackCell(j) {
  const e = j.estimate;
  if (j.phase === 'Succeeded') return h('div', { class: 'slack dimmer' }, '–', h('small', null, 'done'));
  if (!j.objective.deadline) return h('div', { class: 'slack dimmer' }, '–', h('small', null, 'no deadline'));
  const neg = e.slackSeconds < 0;
  return h('div', { class: 'slack' + (neg ? ' t-risk' : '') }, f.signed(e.slackSeconds), h('small', null, 'slack'));
}

export function orderText(o) {
  return { binary: 'binary order', sequential: 'in order', reverse: 'reverse order', 'failure-rate': 'likely failures first', gang: 'together' }[o] || o;
}

export function burstButton(j) {
  const b = j.estimate.burst;
  if (!b || !b.needsApproval) return null;
  return h('button', { class: 'btn primary', 'data-action': 'approve', 'data-pool': b.pool },
    `Approve burst: +${b.slots} ${b.pool} slots, ${f.money(b.cost)}`);
}

export function failedUnits(j) {
  return j.unitStates.failed || '';
}

export function empty(text) {
  return h('p', { class: 'dim', style: { padding: '24px 0' } }, text);
}
