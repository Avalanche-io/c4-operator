// Plan lens: a development plan run as a job (a plan class). One row per
// chapter or phase of the plan, one cell per lane. A lane is a planned block
// until its charter step runs; then it is its steps: the charter, the
// builder phases, the refutation and the merge, drawn as a strip inside the
// cell.
//
// States take the HUD's one vocabulary (design/README.md): a lane in the
// plan not chartered yet is expected (stippled, a dashed edge); chartered and
// waiting for a slot it is queued (flat grey); blocked on the lanes it
// follows it is hatched, with those lanes as text; at work (rendering) it is
// bright blue; merged is done. A lane a refuter sent back is rework: blue with
// an orange outline and a rework mark per pass. The marks come from marks.js.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { markEl } from '../../design/components/marks.js';

const HATCH = 'repeating-linear-gradient(45deg, var(--g12) 0 1px, var(--g09) 1px 3.5px)';
const STIPPLE = 'radial-gradient(var(--g13) 0.6px, transparent 0.8px) 0 0 / 3px 3px, var(--g08)';
const LANE = {
  planned: { bg: STIPPLE, word: 'expected: in the plan, not chartered yet', edge: '1px dashed var(--g12)' },
  blocked: { bg: HATCH, word: 'blocked on the lanes it follows' },
  waiting: { bg: 'var(--g09)', word: 'queued: chartered, waits for a slot' },
  chartering: { bg: 'var(--run)', word: 'rendering: its charter' },
  building: { bg: 'var(--run)', word: 'rendering: building' },
  checking: { bg: 'var(--run)', word: 'rendering: being checked' },
  reviewing: { bg: 'var(--run)', word: 'rendering: being reviewed' },
  refuting: { bg: 'var(--run)', word: 'rendering: being refuted' },
  merging: { bg: 'var(--run)', word: 'rendering: merging' },
  'sent back': { bg: 'var(--run)', word: 'rework: the refuter sent it back; its last phase runs again', ring: 'var(--temp)' },
  retrying: { bg: 'var(--run)', word: 'retrying: a step failed and runs again', ring: 'var(--temp)' },
  locked: { bg: HATCH, word: 'locked by the gate: one merge at a time' },
  merged: { bg: 'var(--done)', word: 'done: merged' },
  failed: { bg: 'var(--fail)', word: 'needs you: it failed after its retries' },
};
// a mark in a cell's top-right corner
const corner = (kind, color) => h('span', { style: { position: 'absolute', top: '3px', right: '3px', lineHeight: 0, color } }, markEl(kind, { size: 8 }));

const STEP = {
  unevaluated: 'var(--g12)', queued: 'var(--g12)', blocked: 'var(--g12)',
  running: 'var(--run)', retrying: 'var(--temp)', succeeded: 'var(--done)', failed: 'var(--fail)',
};

const laneOf = (l) => LANE[l.state] || LANE.planned;
const dark = (st) => st === 'merged' || st === 'failed' || LANE[st]?.bg === 'var(--run)';

// A wait block (a ruling, a review): what it waits for, in the grammar's
// terms. A person's open wait that holds up work needs them (red, with the
// corner flag); one that holds up nothing yet is pale red; a wait for an
// outside party is grey hatched with a clock mark, never red; a decided one
// is done.
function waitCell(ctx, job, l) {
  const w = l.wait || {};
  const who = (w.waitsFor || '').replace(/^(person|party|clock):/, '');
  const person = (w.waitsFor || '').startsWith('person:');
  let bg = 'var(--g09)', fg = 'var(--ink)', word = 'waits for ' + who, flag = false, clockMark = false;
  switch (l.state) {
    case 'waits':
      if (person && w.needsYou) { bg = 'var(--fail)'; fg = 'var(--paper)'; flag = true; word = `needs you: ${who} to decide; it holds up work`; }
      else if (person) { bg = 'var(--fail-pale)'; word = `waits for ${who}; nothing is blocked on it yet`; }
      else { bg = HATCH; clockMark = true; word = `waits for ${who}`; }
      break;
    case 'decided':
      bg = 'var(--done)'; fg = 'var(--paper)'; word = `done: decided by ${who}`;
      break;
    case 'blocked':
      bg = HATCH; word = 'blocked on the lanes before it';
      break;
    default:
      bg = STIPPLE; word = 'expected: opens when the lanes before it are done';
  }
  const tip = [
    `${l.name} · ${word}`,
    w.ask ? w.ask : null,
    w.expectSeconds ? `expected wait ~${f.dur(w.expectSeconds)}${w.since ? `, open since ${f.clock(w.since, false)}` : ''}` : null,
    'holds up the lanes that follow it',
  ].filter(Boolean).join(' | ');
  return h('a', {
    class: 'pcell pwait st-' + l.state, 'data-key': 'l-' + l.name, href: ctx.href(`job/${job.namespace}/${job.name}/unit/${l.unit}`), 'data-tip': tip,
    style: { position: 'relative', display: 'block', minWidth: '64px', padding: '4px 5px 4px', background: bg, color: fg,
      font: '500 10.5px/1.2 var(--mono)', textDecoration: 'none', borderRadius: '0 0 0 0' },
  },
  h('span', null, l.name.replace(/^R-/, '')),
  flag ? h('i', { style: { position: 'absolute', top: 0, right: 0, width: 0, height: 0, borderTop: '7px solid var(--paper)', borderLeft: '7px solid transparent' } }) : null,
  clockMark ? corner('clock', 'var(--g17)') : null,
  h('span', { style: { display: 'block', marginTop: '3px', font: '400 9px/1.1 var(--sans)', opacity: 0.85 } }, person ? 'ruling' : 'review'));
}

function cell(ctx, job, l, cs) {
  if (l.wait) return waitCell(ctx, job, l);
  const st = laneOf(l);
  const steps = (l.steps || []).filter((x) => x.kind !== 'charter' || !l.chartered);
  const strip = l.chartered
    ? h('span', { class: 'psteps', style: { display: 'flex', gap: '1px', marginTop: '3px' } },
      steps.map((x, k) => h('i', { 'data-key': 's' + k, style: { display: 'block', flex: '1 1 0', height: '4px', background: x.running ? 'var(--paper)' : STEP[x.state] || 'var(--g12)' } })))
    : null;
  const lock = l.state === 'locked' ? corner('lock', 'var(--g17)') : null;
  // a rework mark per pass the refuter sent it back
  const ticks = l.sentBack ? h('span', { class: 'pticks', style: { position: 'absolute', top: '3px', right: '3px', display: 'flex', gap: '1px', lineHeight: 0, color: dark(l.state) ? 'var(--paper)' : 'var(--temp)' } },
    Array.from({ length: Math.min(l.sentBack, 3) }, () => markEl('rework', { size: 8 }))) : null;
  const tip = [
    `${l.label} · ${st.word}`,
    l.after.length ? `after ${l.after.join(', ')}` : 'follows no lane',
    l.size ? `${l.size} ${cs.sizeOf || 'items'}` : null,
    l.filler ? 'a filler: runs beside any lane' : null,
    l.holds.length ? `holds ${l.holds.slice(0, 4).join(', ')}${l.holds.length > 4 ? ` and ${l.holds.length - 4} more` : ''}` : null,
    'click opens its charter step',
  ].filter(Boolean).join(' | ');
  return h('a', {
    class: 'pcell st-' + l.state.replace(' ', '-'), 'data-key': 'l-' + l.name, href: ctx.href(`job/${job.namespace}/${job.name}/unit/${l.unit}`), 'data-tip': tip,
    style: {
      position: 'relative', display: 'block', minWidth: '64px', padding: '4px 5px 4px', background: st.bg,
      color: dark(l.state) ? 'var(--paper)' : 'var(--ink)', font: '500 10.5px/1.2 var(--mono)', textDecoration: 'none',
      outline: st.edge || 'none', outlineOffset: '-1px', boxShadow: st.ring ? `inset 0 0 0 1.5px ${st.ring}` : 'none',
    },
  }, h('span', null, l.name), ticks, lock, strip);
}

export function planLens(ctx, job) {
  const cs = job.classSummary || {};
  const rows = cs.rows || [];
  const m = cs.measure || {};
  const nouns = cs.nouns || 'lanes';
  const measured = m.label ? ` · ${m.label} ${f.num(m.done || 0)} of ${f.num(m.total || 0)}` : '';
  const share = cs.modelShare != null ? ` · models did ${Math.round(cs.modelShare * 100)}% of the work so far` : '';
  const sw = (bg, more = {}, mark = null, ink = 'var(--g17)') => h('i', { style: { position: 'relative', background: bg, width: '14px', height: '9px', display: 'inline-block', ...more } },
    mark ? h('span', { style: { position: 'absolute', right: 0, top: 0, lineHeight: 0, color: ink } }, markEl(mark, { size: 7 })) : null);
  return [
    h('div', { class: 'panel', 'data-key': 'plan' },
      h('header', null, h('h2', null, 'Plan'),
        h('span', { class: 'dim' }, `${f.num(cs.lanes || 0)} ${nouns} · ${f.num(cs.merged || 0)} merged · ${f.num(cs.running || 0)} at work${measured}${share}${cs.needsYou ? ` · ${f.num(cs.needsYou)} ${cs.needsYou === 1 ? 'ruling needs' : 'rulings need'} a person` : cs.waits ? ` · ${f.num(cs.waits)} waiting for a decision` : ''}${cs.gateLine ? ` · ${f.num(cs.gateLine)} ${cs.gateLine === 1 ? 'merge waits' : 'merges wait'} for the gate` : ''}${cs.unfiled ? ` · ${f.num(cs.unfiled)} more ${cs.unfiled === 1 ? cs.noun || 'lane' : nouns} expected from later merges` : ''}`)),
      h('div', { class: 'well', style: { padding: '10px 12px' } },
        rows.map((r) => h('div', { class: 'prow', 'data-key': 'r-' + r.row, style: { display: 'grid', gridTemplateColumns: '96px minmax(0, 1fr)', gap: '10px', alignItems: 'start', marginBottom: '8px' } },
          h('div', { style: { font: '600 10px/1.3 var(--sans)', letterSpacing: '.12em', textTransform: 'uppercase', color: 'var(--ink-3)', paddingTop: '4px' } }, r.row),
          h('div', { style: { display: 'flex', flexWrap: 'wrap', gap: '3px' } }, (r.lanes || []).map((l) => cell(ctx, job, l, cs)))))),
      h('div', { class: 'legend', style: { padding: '8px 12px' } },
        h('span', null, sw(STIPPLE, { outline: '1px dashed var(--g12)', outlineOffset: '-1px' }), 'expected: not chartered yet'),
        h('span', null, sw('var(--g09)'), 'queued'),
        h('span', null, sw(HATCH), `blocked on the ${nouns} it follows`),
        h('span', null, sw('var(--run)'), `rendering: ${(cs.stepNames || ['charter', 'phases', 'refutation', 'merge']).join(', ')}`),
        h('span', null, sw('var(--run)', { boxShadow: 'inset 0 0 0 1.5px var(--temp)' }, 'rework', 'var(--paper)'), 'rework: a check or a refuter sent it back, a mark per pass'),
        h('span', null, sw(HATCH, {}, 'lock'), 'locked by the gate: its merge waits its turn'),
        h('span', null, sw(HATCH, {}, 'clock'), 'waits for an outside party'),
        h('span', null, sw('var(--done)'), 'done: merged, or decided'),
        h('span', null, sw('linear-gradient(225deg, var(--paper) 0 4px, var(--fail) 4px)'), 'needs you: a ruling that holds up work, or a failure'),
        h('span', null, sw('var(--fail-pale)'), 'waits for someone: holds up nothing yet'))),
  ];
}
