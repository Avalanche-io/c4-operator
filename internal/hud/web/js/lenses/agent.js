// Agent lens: a bundle of agents (an AgentBundle; its members are the job's
// units). One strip per member along time, two rulers (real time, and the
// bundle's own time, which stops while the bundle waits for capacity or is held),
// the cap against the members running, and each member's checkpoint.
//
// States take the HUD's one vocabulary (design/README.md): rendering is
// bright blue (an agent at work); queued is the flat grey of a unit not
// started; a member the queue stopped at a turn boundary for capacity waits
// for capacity: hatched, with the clock mark and the checkpoint mark where it
// resumes; held (a person paused the bundle) is the pause mark; yielding (it
// stops at the end of its turn) is blue with the pause mark; done is green; a
// failed member needs you.

import { h, s } from '../dom.js';
import * as f from '../fmt.js';
import { c4idEl } from '../../design/components/c4id.js';
import { markAttrs } from '../../design/components/marks.js';

const STATE = {
  running: { fill: 'var(--run)', word: 'rendering' },
  yielding: { fill: 'var(--run)', word: 'yielding: stops at the end of its turn' },
  waiting: { fill: 'var(--g09)', word: 'queued' },
  suspended: { fill: 'url(#ag-hatch)', word: 'waits for capacity; it resumes from its checkpoint' },
  held: { fill: 'var(--g09)', word: 'held by a person' },
  done: { fill: 'var(--done)', word: 'done' },
  failed: { fill: 'var(--fail)', word: 'needs you: it failed' },
};

// a mark (marks.js) in the chart, size px, top-left at x, y
const mark = (kind, x, y, size, color, dx = 0) => s('g', { transform: `translate(${dx} 0)` },
  s('svg', { x, y, width: size, height: size, viewBox: '0 0 10 10', overflow: 'visible' }, s('path', markAttrs(kind, color))));

const stateOf = (m) => STATE[m.state] || STATE.waiting;

// The bundle's time at real time t: its clock runs except across its
// suspended intervals.
function virtualAt(t, start, gaps) {
  let lost = 0;
  for (const g of gaps) {
    if (g.from >= t) break;
    lost += Math.min(t, g.to) - g.from;
  }
  return t - lost;
}

export function agentLens(ctx, job) {
  const cs = job.classSummary || {};
  const members = cs.members || [];
  const clock = cs.clock || {};
  const now = f.now();
  const gaps = (cs.suspended || []).map((g) => ({ from: f.ms(g.from), to: g.to ? f.ms(g.to) : now, open: !g.to, by: g.by || 'queue' }));
  const runs = members.flatMap((m) => (m.runs || []).map((r) => f.ms(r.from)));
  const start = clock.start ? f.ms(clock.start) : Math.min(now - 3600e3, ...runs, ...gaps.map((g) => g.from));
  const t0 = Math.min(start, now - 1800e3);
  const t1 = now + Math.max(600e3, (now - t0) * 0.06);
  const x = (t) => `${((t - t0) / (t1 - t0)) * 100}%`;
  const wx = (a, b) => `${Math.max(0.25, ((b - a) / (t1 - t0)) * 100)}%`;
  const rowY = (i) => 54 + i * 18;
  const bottom = rowY(Math.max(members.length, 1)) + 6;

  // Rulers: real time above, bundle time below it.
  const rulers = [
    s('text', { x: -8, y: 12, 'text-anchor': 'end' }, 'real'),
    s('text', { x: -8, y: 32, 'text-anchor': 'end' }, 'bundle'),
    s('line', { x1: 0, x2: '100%', y1: 16, y2: 16, stroke: 'var(--ink-3)', 'stroke-width': 1 }),
    s('line', { x1: 0, x2: '100%', y1: 36, y2: 36, stroke: 'var(--ink-3)', 'stroke-width': 1 }),
  ];
  const ticks = 6;
  for (let k = 0; k <= ticks; k++) {
    const t = t0 + ((t1 - t0) * k) / ticks;
    const iso = new Date(t).toISOString();
    const v = new Date(virtualAt(t, start, gaps)).toISOString();
    const anchor = k === 0 ? 'start' : k === ticks ? 'end' : 'middle';
    rulers.push(
      s('line', { x1: x(t), x2: x(t), y1: 14, y2: 18, stroke: 'var(--ink-3)' }),
      s('text', { x: x(t), y: 10, 'text-anchor': anchor }, f.clock(iso, false)),
      s('line', { x1: x(t), x2: x(t), y1: 34, y2: 38, stroke: 'var(--ink-3)' }),
      s('text', { x: x(t), y: 46, 'text-anchor': anchor, style: { fill: 'var(--ink-3)' } }, t <= start ? '' : f.clock(v, false)));
  }
  // A suspended interval: a gap with a tick on both rulers, and a neutral
  // grey band one step off the well across the rows (drawn under the strips).
  const gapMarks = gaps.flatMap((g) => [
    s('rect', { x: x(g.from), y: 20, width: wx(g.from, g.to), height: bottom - 20, fill: 'var(--g06)' }),
    s('line', { x1: x(g.from), x2: x(g.from), y1: 12, y2: 40, stroke: 'var(--ink-2)', 'stroke-width': 1.5 }),
    s('line', { x1: x(g.to), x2: x(g.to), y1: 12, y2: 40, stroke: 'var(--ink-2)', 'stroke-width': 1.5 }),
    s('title', null, `stopped interval, ${f.dur((g.to - g.from) / 1000)}, by ${g.by === 'person' ? 'a person' : 'the queue'}`),
  ]);

  const rows = members.flatMap((m, i) => {
    const y = rowY(i);
    const st = stateOf(m);
    const out = [s('text', { x: -8, y: y + 9, 'text-anchor': 'end' }, m.name)];
    const rs = (m.runs || []).map((r) => [f.ms(r.from), r.to ? f.ms(r.to) : now]);
    // Waiting before, between and after its runs is flat grey.
    out.push(s('rect', { x: x(t0), y, width: wx(t0, now), height: 11, fill: 'var(--g09)' }));
    for (const [a, b] of rs) out.push(s('rect', { x: x(a), y, width: wx(a, b), height: 11, fill: 'var(--run)' }));
    if (m.state === 'done' || m.state === 'failed') {
      const end = rs.length ? rs[rs.length - 1][1] : now;
      out.push(s('rect', { x: x(end), y, width: wx(end, now), height: 11, fill: st.fill }));
    }
    const last = rs.length ? rs[rs.length - 1][1] : t0;
    if (m.state === 'suspended') {
      // waits for capacity: hatched since its last turn, the clock at now
      out.push(s('rect', { x: x(last), y, width: wx(last, now), height: 11, fill: 'url(#ag-hatch)' }), mark('clock', x(now), y + 1, 9, 'var(--g17)', 4));
    }
    if (m.state === 'held') out.push(mark('pause', x(now), y + 1, 9, 'var(--g16)', 4));
    if (m.state === 'yielding') out.push(mark('pause', x(now), y + 1, 9, 'var(--run)', 4));
    if (m.checkpoint && (m.state === 'suspended' || m.state === 'held')) {
      // The checkpoint mark: where it resumes from.
      const at = rs.length ? rs[rs.length - 1][1] : now;
      out.push(s('rect', { x: x(at), y: y + 2, width: 7, height: 7, fill: 'var(--ink)', transform: 'translate(-3.5 0)' }));
    }
    out.push(s('title', null, `${m.name}: ${st.word}${m.node ? ' on ' + m.node : ''}${m.epoch ? ', lease ' + m.epoch : ''}`));
    return out;
  });
  const nowLine = s('line', { x1: x(now), x2: x(now), y1: 4, y2: bottom, stroke: 'var(--run)', 'stroke-width': 1.5 });
  const chart = s('svg', { class: 'chart', style: { height: `${bottom + 8}px`, marginLeft: '112px', width: 'calc(100% - 132px)', overflow: 'visible', fill: 'var(--ink-2)', font: '400 10px var(--mono)' },
    role: 'img', 'aria-label': `${members.length} members, ${cs.running || 0} running, ${gaps.length} stopped intervals` },
    s('defs', null, s('pattern', { id: 'ag-hatch', width: 5, height: 5, patternUnits: 'userSpaceOnUse', patternTransform: 'rotate(45)' },
      s('rect', { width: 5, height: 5, fill: 'var(--g09)' }), s('line', { x1: 0, y1: 0, x2: 0, y2: 5, stroke: 'var(--g12)', 'stroke-width': 1.2 }))),
    gapMarks, rulers, rows, nowLine);

  const drift = clock.driftSeconds || 0;
  const vnow = clock.virtualNow ? f.ms(clock.virtualNow) : null;
  const phase = { Held: 'held by a person', Holding: 'yielding for a person', Suspended: 'waits for capacity', Running: 'rendering', Pending: 'queued' }[cs.phase] || 'queued';
  return [
    h('div', { class: 'panel', 'data-key': 'agent-members' },
      h('header', null, h('h2', null, 'Members'),
        h('span', { class: 'dim' }, `Bundle ${cs.bundle || job.name}: ${phase}. ${cs.running || 0} rendering${cs.cap ? ` of at most ${cs.cap}` : ''}.`)),
      h('div', { class: 'well', style: { padding: '14px 0 6px' } }, chart),
      h('div', { class: 'legend', style: { padding: '8px 12px' } },
        h('span', null, h('i', { style: { background: 'var(--run)', width: '14px', height: '9px', display: 'inline-block' } }), 'rendering'),
        h('span', null, h('i', { style: { background: 'var(--g09)', width: '14px', height: '9px', display: 'inline-block' } }), 'queued'),
        h('span', null, h('i', { style: { background: 'repeating-linear-gradient(45deg, var(--g09) 0 3px, var(--g12) 3px 4px)', width: '14px', height: '9px', display: 'inline-block' } }), 'waits for capacity'),
        h('span', null, h('i', { style: { background: 'var(--ink)', width: '7px', height: '7px', display: 'inline-block' } }), 'checkpoint: where it resumes'),
        h('span', null, h('i', { style: { background: 'var(--g06)', width: '14px', height: '9px', display: 'inline-block' } }), 'stopped interval: the bundle’s time stops'))),
    h('div', { style: { display: 'grid', gridTemplateColumns: 'minmax(0, 3fr) minmax(0, 2fr)', gap: 'var(--gutter)', marginTop: 'var(--gutter)' } },
      h('div', { class: 'panel', 'data-key': 'agent-table' },
        h('header', null, h('h2', null, 'Members now')),
        h('table', { class: 'vt' },
          h('thead', null, h('tr', null, h('th', null, 'Member'), h('th', null, 'State'), h('th', null, 'Node'), h('th', null, 'Lease'), h('th', null, 'Checkpoint'))),
          h('tbody', null, members.map((m) => h('tr', { 'data-key': 'm-' + m.name },
            h('td', null, m.name, m.kind === 'task' ? h('span', { class: 'dim' }, ' task') : null),
            h('td', null, stateOf(m).word),
            h('td', null, m.node || ''),
            h('td', { class: 'num' }, m.epoch || ''),
            h('td', null, m.checkpoint ? [c4idEl(m.checkpoint.id, { tail: 4, label: 'checkpoint' }), ` turn ${m.checkpoint.seq}`] : '')))))),
      h('div', { class: 'panel', 'data-key': 'agent-clock' },
        h('header', null, h('h2', null, 'Bundle time')),
        h('table', { class: 'vt' }, h('tbody', null,
          h('tr', null, h('th', null, 'Now'), h('td', null, vnow ? [f.clock(new Date(vnow).toISOString()), ' ', h('small', { class: 'dim' }, f.utc(new Date(vnow).toISOString()))] : 'not started')),
          h('tr', null, h('th', null, 'Behind real time'), h('td', null, drift ? f.signed(-drift) : 'no drift')),
          h('tr', null, h('th', null, 'Suspended'), h('td', null, gaps.length ? gaps.map((g, i) => h('div', { 'data-key': 'g' + i },
            g.open ? `since ${f.clock(new Date(g.from).toISOString())}, by ${g.by === 'person' ? 'a person' : 'the queue'}`
              : `${f.clock(new Date(g.from).toISOString())} to ${f.clock(new Date(g.to).toISOString())}, ${f.dur((g.to - g.from) / 1000)}, by ${g.by === 'person' ? 'a person' : 'the queue'}`)) : 'none')))),
        h('p', { class: 'dim', style: { fontSize: '12px', padding: '8px 12px', margin: 0 } },
          'The bundle keeps its own clock. It runs while the bundle runs and stops while it waits for capacity or is held, so the agents’ own timers never see the pause. Deadlines stay on the real clock.'))),
  ];
}
