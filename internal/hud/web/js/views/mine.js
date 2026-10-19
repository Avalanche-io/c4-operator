// My shots: the queue as an artist sees it. Each of the viewer's own jobs
// is its sheet in miniature: a PYLON, the shot's STACK collapsed to its
// batches, the deadline and the slack, the output view as a grid
// reference, and links to the full sheet and to its side widget. The rest
// of the viewer's projects follow as one line each.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get } from '../api.js';
import { own } from '../../design/components/live.js';
import { glance, inFlight, retries } from '../../design/components/core.js';
import { buildSheet } from '../../design/components/stack.js';
import { phaseText } from './common.js';
import { c4idEl } from '../../design/components/c4id.js';
import { tempWords } from '../../design/components/words.js';

const st = { details: new Map(), graph: null, at: -Infinity, busy: false };
document.addEventListener('hud:changed', () => { st.at = -Infinity; });

const keyOf = (j) => `${j.namespace}/${j.name}`;
const REFRESH_MS = 5000;

// the jobs on a job's sheet: every job of its shot, and the deliveries they feed
function shotKeys(snap, g, j) {
  const k = keyOf(j);
  if (!j.shot) return [k];
  const set = new Set(snap.jobs.filter((x) => x.shot === j.shot && x.project === j.project).map(keyOf));
  for (const e of (g && g.edges) || []) if (set.has(e.from)) { const t = snap.jobs.find((x) => keyOf(x) === e.to); if (t && !t.shot) set.add(e.to); }
  const list = [...set];
  const indeg = new Map(list.map((x) => [x, 0]));
  for (const e of (g && g.edges) || []) if (set.has(e.from) && set.has(e.to)) indeg.set(e.to, indeg.get(e.to) + 1);
  const out = [], q = list.filter((x) => !indeg.get(x));
  while (q.length) { const x = q.shift(); out.push(x); for (const e of (g && g.edges) || []) if (e.from === x && set.has(e.to)) { indeg.set(e.to, indeg.get(e.to) - 1); if (!indeg.get(e.to)) q.push(e.to); } }
  for (const x of list) if (!out.includes(x)) out.push(x);
  return out;
}

function ensure(ctx, mine) {
  if (st.busy || performance.now() - st.at < REFRESH_MS) return;
  st.busy = true;
  st.at = performance.now();
  get('/graph').then((g) => { st.graph = g; }, () => {})
    .then(() => {
      const want = new Set();
      for (const j of mine) for (const k of shotKeys(ctx.snap, st.graph, j)) want.add(k);
      return Promise.all([...want].map((k) => get(`/jobs/${k}`).then((d) => st.details.set(k, d), () => {})));
    })
    .finally(() => { st.busy = false; ctx.refresh(); });
}

function tcell(label, val, sub, cls = '') {
  return h('div', { class: 'cell' }, h('span', { class: 'lab' }, label), h('span', { class: 'val ' + cls }, val), sub ? h('span', { class: 'sub' }, sub) : null);
}

function card(ctx, j) {
  const k = keyOf(j);
  const d = st.details.get(k);
  const g = glance(j, d);
  const e = j.estimate;
  const dl = j.objective && j.objective.deadline;
  const done = j.phase === 'Succeeded';
  const late = !done && dl && Number.isFinite(e.slackSeconds) && e.slackSeconds < 0;
  const keys = shotKeys(ctx.snap, st.graph, j);
  const details = keys.map((x) => st.details.get(x)).filter(Boolean).map((x) => ({ ...x, ...(ctx.job(keyOf(x)) || {}), unitStates: (ctx.job(keyOf(x)) || x).unitStates }));
  let sheet = null;
  if (details.length && details.some((x) => keyOf(x) === k)) {
    sheet = buildSheet({ key: `mine:${keys.join('|')}`, title: j.shot || j.name, details, anchor: k, graph: st.graph, jobs: ctx.snap.jobs });
  }
  const NB = sheet ? Math.ceil(sheet.N / sheet.batch) : 0;
  const view = { box: 8, collapsed: new Set([...Array(NB).keys()]), lineA: false, contours: false, deadline: dl ? Date.parse(dl) : null };
  const par = Math.round(g.par * 100);
  return h('article', { class: 'mjob' + (late ? ' late' : ''), 'data-key': 'mine-' + k, 'aria-label': `${j.name}: ${Math.round(g.done * 100)}% done${late ? ', projected late' : ''}` },
    h('header', { class: 'mcollar' },
      h('a', { class: 'nm', href: ctx.href(`job/${k}`) }, j.name),
      j.shot ? h('span', { class: 'mshot' }, `shot ${j.shot}`) : null,
      h('span', { class: 'mph' }, phaseText(j)),
      h('span', { class: 'go' },
        h('a', { href: ctx.href(`job/${k}`) }, 'job view'),
        h('a', { href: ctx.href(`widget/${k}`), target: '_blank', rel: 'noopener', title: 'Open the side widget in its own window; dock it beside your app' }, 'side widget ↗'),
        h('a', { href: ctx.href(`widget/${k}?compact=1`), target: '_blank', rel: 'noopener', title: 'The 240 px corner version' }, 'corner ↗'))),
    h('div', { class: 'mbody' },
      own('pylon', `mpylon:${k}`, { glance: g, w: 180, h: 250, mode: 'compact' }, { class: 'mpylon' }),
      h('div', { class: 'mstack' },
        h('div', { class: 'mst-h' }, h('span', { class: 'lab' }, j.shot ? `shot ${j.shot}` : 'job view'),
          h('span', null, sheet ? `${sheet.cols.length} stages · ${NB} batches of ${sheet.batch} ${sheet.axisKind === 'frame' ? 'frames' : 'rows'}` : 'reading the job…')),
        sheet ? own('stack', `mstack:${k}`, { sheet, view }) : h('div', { class: 'mst-wait' }),
        j.output ? h('div', { class: 'edgecode' }, h('b', null, c4idEl(j.output.view, { tail: 4, label: 'output view' })), `${String(j.output.changesets).padStart(4, '0')}+00`) : null),
      h('div', { class: 'mtb' },
        tcell('Deadline', dl ? f.clock(dl) : '–', j.objective && j.objective.from ? `from ${f.jobName(j.objective.from)}` : dl ? 'set on the job' : 'no objective'),
        tcell('Slack', dl && !done ? h('span', { class: late ? 't-risk pencil' : 'pencil' }, f.signed(e.slackSeconds)) : '–', late ? 'projected late' : done ? 'done' : e.feasible ? 'on time at current capacity' : 'will miss at current capacity'),
        tcell('Done', h('span', null, `${Math.round(g.done * 100)}`, h('small', null, '%')), par ? h('span', { class: 't-run' }, `${par}% running now`) : j.phase === 'Held' ? 'held' : 'nothing running'),
        tcell(done ? 'Finished' : 'About', done ? f.clock(e.etaP90, false) : h('span', { class: 'pencil' + (late ? ' t-risk' : '') }, f.approx(e.remainingSeconds)), done ? '' : `p90 ${f.clock(e.etaP90, false)}`),
        late && e.burst ? h('div', { class: 'mfix' }, `+${e.burst.slots} ${e.burst.pool} slots closes it · `, h('span', { class: 'pencil' }, `est. ${f.money(e.burst.cost)}`), e.burst.needsApproval ? ' · waits for approval' : '') : null,
        !late && retries(j.units) ? h('div', { class: 'mnote t-temp' }, tempWords(j.units)) : null,
        j.units.failed ? h('div', { class: 'mnote t-fail' }, `${j.units.failed} ${j.units.failed === 1 ? 'needs' : 'need'} you: requeue, or accept the bad frames`) : null)));
}

function row(ctx, j) {
  const k = keyOf(j);
  const g = glance(j);
  const e = j.estimate;
  const late = j.phase !== 'Succeeded' && j.objective && j.objective.deadline && e.slackSeconds < 0;
  return h('a', { class: 'mrow', href: ctx.href(`job/${k}`), 'data-key': 'r-' + k },
    own('pylon', `mrp:${k}`, { glance: g, w: 18, h: 34, mode: 'mini' }, { class: 'mrp', tag: 'span' }),
    h('span', { class: 'n' }, j.name, j.owner ? h('small', null, ` ${j.owner}`) : null),
    h('span', { class: 'm' }, `${Math.round(g.done * 100)}%`, g.par ? h('span', { class: 't-run' }, ` ${Math.round(g.par * 100)}% running`) : null),
    h('span', { class: 'm' }, phaseText(j)),
    h('span', { class: 'm r' }, j.objective && j.objective.deadline && j.phase !== 'Succeeded' ? h('span', { class: late ? 't-risk pencil' : 'pencil' }, `slack ${f.signed(e.slackSeconds)}`) : ''));
}

export default {
  render(ctx) {
    const snap = ctx.snap;
    const me = snap.viewer.user;
    let mine = snap.jobs.filter((j) => j.owner === me);
    const own_ = mine.length > 0;
    if (!own_) mine = snap.jobs.filter((j) => j.phase !== 'Succeeded').slice(0, 6);
    ensure(ctx, mine);
    const rest = snap.jobs.filter((j) => !mine.includes(j));
    const active = mine.filter((j) => j.phase !== 'Succeeded');
    const late = active.filter((j) => j.objective && j.objective.deadline && j.estimate.slackSeconds < 0);
    return h('section', { class: 'sheet mine', 'data-key': 'mine', 'aria-label': 'My shots' },
      h('div', { class: 'sheethead' },
        h('div', null,
          h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ` / ${snap.viewer.projects.join(', ')} / ${me}`),
          h('h1', null, own_ ? 'My shots' : 'Shots in your projects'),
          h('div', { class: 'meta' }, own_ ? `${mine.length} job${mine.length > 1 ? 's' : ''} you own · in run order · each opens as a side widget for your second screen` : `you own no jobs right now; these are your projects' active jobs`)),
        h('div', { class: 'titleblock' },
          tcell('Yours', f.num(mine.length), `${active.length} active`),
          tcell('Late', h('span', { class: late.length ? 't-risk' : '' }, f.num(late.length)), late.length ? 'projected past the deadline' : 'all on time'),
          tcell('Running now', h('span', { class: 't-run' }, f.num(mine.reduce((a, j) => a + inFlight(j.units), 0))), 'of your units'),
          tcell('Other projects', f.num(snap.farm.otherProjectsSlots), 'slots, shown only as a total'))),
      h('div', { class: 'mlist' }, mine.map((j) => card(ctx, j))),
      rest.length ? h('div', { class: 'mrest' },
        h('div', { class: 'mrest-h' }, h('h2', null, `Also in ${snap.viewer.projects.join(', ')}`), h('span', null, `${rest.length} jobs · in run order`)),
        rest.map((j) => row(ctx, j))) : null);
  },
};
