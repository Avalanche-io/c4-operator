// Queue: every job in scope, in dispatch order, each with its job view in
// miniature, its objective on a shared time lane, and its slack.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { drawMini } from '../grid.js';
import { count } from '../rle.js';
import { key, lane, laneHeader, classChip, phaseText, slackCell, orderText, burstButton, failedUnits, empty } from './common.js';

const FILTERS = [['all', 'All'], ['render', 'Render'], ['ci', 'CI'], ['ml', 'ML'], ['other', 'Sim and media']];

function lensOf(ctx, j) {
  const c = ctx.cls(j.class);
  return c ? c.lens : '';
}

function matches(ctx, j, flt) {
  if (flt === 'all') return true;
  const lens = lensOf(ctx, j);
  if (flt === 'other') return !['render', 'ci', 'ml'].includes(lens);
  return lens === flt;
}

function row(ctx, j, multiProject) {
  const k = key(j);
  const u = j.units;
  const e = j.estimate;
  const done = j.phase === 'Succeeded';
  const msg = [];
  if (j.phase === 'Failed') {
    msg.push(h('span', { class: 'warn red' }, e.message || `${j.name} failed.`));
    msg.push(h('button', { class: 'btn small', 'data-action': 'requeue', 'data-job': k, 'data-units': failedUnits(j) }, `Requeue failed units (${u.failed})`));
  } else if (j.phase === 'Held') {
    msg.push(h('span', { class: 'warn mag' }, e.message || 'Held: no new units are released.'));
    msg.push(h('button', { class: 'btn small', 'data-action': 'release', 'data-job': k }, 'Release job'));
  } else if (e.message) {
    msg.push(h('span', { class: 'warn' }, e.message));
    msg.push(burstButton(j));
  } else if (u.failed > 0) {
    msg.push(h('span', { class: 'warn red' }, `${u.failed} unit${u.failed > 1 ? 's' : ''} out of retries.`));
    msg.push(h('button', { class: 'btn small', 'data-action': 'requeue', 'data-job': k, 'data-units': failedUnits(j) }, `Requeue failed units (${u.failed})`));
  }
  const capt = done ? `${f.num(u.succeeded)} done` :
    [`${f.num(u.succeeded)}/${f.num(u.total)} done`, u.running + u.retrying ? `${u.running + u.retrying} running` : null, u.failed ? `${u.failed} failed` : null, u.unevaluated ? `${u.unevaluated} to expand` : null].filter(Boolean).join(' · ');
  return h('div', { class: 'qrow' + (done ? ' done' : ''), 'data-key': k, role: 'listitem' },
    h('div', { class: 'qrank' }, String(j.rank)),
    h('div', { class: 'qname' },
      h('a', { class: 'jn', href: ctx.href(`job/${j.namespace}/${j.name}`) }, j.name),
      h('div', { class: 'qmeta' },
        classChip(j, ctx.cls(j.class)),
        phaseText(j),
        multiProject ? h('span', null, j.project) : null,
        j.owner ? h('span', { class: 'dimmer' }, j.owner) : null,
        h('span', { class: 'dimmer' }, orderText(j.order)),
        h('span', { class: 'num dimmer' }, j.pools.join('+')),
        j.blacklist && j.blacklist.length ? h('span', { class: 'chip flag', title: 'Nodes this job will not use' }, `blacklist ${j.blacklist.join(', ')}`) : null)),
    h('div', { class: 'mcell' },
      h('canvas', { class: 'mini', 'data-key': 'mini-' + k, 'data-mini': k, 'aria-label': `Job view of ${j.name}: ${capt}`, role: 'img' }),
      h('div', { class: 'minicap' }, h('span', { class: 'num' }, capt))),
    h('div', { class: 'lcell' }, lane(j)),
    slackCell(j),
    msg.length ? h('div', { class: 'qmsg' }, msg) : null);
}

export default {
  render(ctx) {
    const s = ctx.snap;
    const flt = ctx.route.q.class || 'all';
    const jobs = s.jobs.filter((j) => matches(ctx, j, flt));
    const multiProject = s.viewer.projects.length > 1;
    const atRisk = s.jobs.filter((j) => !j.estimate.feasible && j.phase !== 'Succeeded').length;
    const counts = Object.fromEntries(FILTERS.map(([k]) => [k, s.jobs.filter((j) => matches(ctx, j, k)).length]));
    return h('section', { 'aria-labelledby': 'qtitle' },
      h('div', { class: 'scopebar' },
        h('div', null,
          h('h1', { id: 'qtitle' }, 'Queue'),
          h('div', { class: 'dim', style: { marginTop: '4px' } },
            `${s.jobs.length} jobs in dispatch order. Order follows each job's deadline and estimate; nobody sets priorities. `,
            atRisk ? h('span', { class: 't-yellow' }, `${atRisk} will miss ${atRisk > 1 ? 'their deadlines' : 'its deadline'} at current capacity.`) : 'Every job is on track.')),
        h('div', { class: 'filters', role: 'group', 'aria-label': 'Filter by work class' },
          h('span', { class: 'lab' }, 'Show'),
          h('span', { class: 'seg' }, FILTERS.map(([k, label]) => h('a', {
            class: 'btn small', href: ctx.href(k === 'all' ? 'queue' : `queue?class=${k}`), 'aria-pressed': flt === k ? 'true' : 'false',
          }, `${label} ${counts[k]}`))))),
      h('div', { class: 'qhead', 'aria-hidden': 'true' },
        h('span', { class: 'lab', style: { textAlign: 'right' } }, '#'),
        h('span', { class: 'lab' }, 'Job'),
        h('span', { class: 'lab' }, 'Units'),
        laneHeader(),
        h('span', { class: 'lab', style: { textAlign: 'right' } }, 'Slack')),
      jobs.length ? h('div', { role: 'list' }, jobs.map((j) => row(ctx, j, multiProject))) : empty('No jobs match this filter.'));
  },
  after(ctx, root) {
    for (const cv of root.querySelectorAll('canvas[data-mini]')) {
      const j = ctx.job(cv.dataset.mini);
      if (j) drawMini(cv, j);
    }
  },
};

export { count };
