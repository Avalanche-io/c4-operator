// CI lens: changes as rows of pipelines, queue wait against run time per
// stage, checkout and cache outcome, time to green against its targets,
// flaky targets, the merge queue and runner pools.

import { h, s } from '../dom.js';
import * as f from '../fmt.js';

const STAGES = [['build', 'Build'], ['test', 'Test shards'], ['integration', 'Integration'], ['speculative', 'Land']];

function stageCell(p, scale, current) {
  if (!p) return h('td', { class: 'dimmer' }, '–');
  const w = (v) => `${Math.max(0, Math.min(100, (v / scale) * 100)).toFixed(1)}%`;
  const runCol = { passed: 'var(--u-done)', failed: 'var(--bar-red)', running: 'url(#ci-stripe)' }[p.state];
  const runBg = { passed: 'var(--u-done)', failed: 'var(--bar-red)' }[p.state] ||
    (p.state === 'running' ? 'linear-gradient(90deg, var(--done) 0 45%, var(--run) 45% 47%, var(--run-body) 47%)' : 'transparent');
  void runCol;
  const label = p.state === 'blocked' ? 'waiting on the stage before' : `waited ${f.dur(p.queueWaitSeconds)}, ran ${f.dur(p.runSeconds)}`;
  return h('td', { title: label, class: current ? 'cur' : null },
    h('div', { style: { display: 'flex', height: '10px', background: 'var(--well)' }, 'aria-hidden': 'true' },
      h('i', { style: { width: w(p.queueWaitSeconds), background: 'repeating-linear-gradient(135deg, var(--u-queued) 0 3px, var(--u-hatch) 3px 5px)' } }),
      h('i', { style: { width: w(p.runSeconds), background: runBg } })),
    h('div', { class: 'num', style: { fontSize: '10.5px', marginTop: '3px', display: 'flex', justifyContent: 'space-between', gap: '6px' } },
      h('span', { class: 'dim' }, p.state === 'blocked' ? 'blocked' : `${f.dur(p.queueWaitSeconds)} / ${f.dur(p.runSeconds)}`),
      p.checkoutSeconds ? h('span', { class: p.cacheOutcome === 'cold' ? 'ink' : 'dimmer' }, `${p.checkoutSeconds.toFixed(1)}s ${p.cacheOutcome}`) : null),
    h('span', { class: 'sr' }, label));
}

function ttgChart(t) {
  const max = Math.max(t.targetP95 * 1.5, ...t.samples, 60);
  const x = (v) => `${(v / max) * 100}%`;
  const dots = t.samples.map((v, i) => s('circle', { cx: x(v), cy: 16 + ((i * 37) % 17), r: 2.6, fill: v > t.targetP95 ? 'var(--fail)' : 'var(--ink-2)' }));
  const tick = (v, lab, dash, col) => [
    s('line', { x1: x(v), x2: x(v), y1: 4, y2: 40, stroke: col, 'stroke-width': dash ? 1 : 2, 'stroke-dasharray': dash ? '3 2' : null }),
    s('text', { x: x(v), y: 52, 'text-anchor': 'middle' }, lab),
  ];
  return s('svg', { class: 'chart', style: { height: '58px' }, role: 'img', 'aria-label': `Time to green: p50 ${f.dur(t.p50)}, p95 ${f.dur(t.p95)}; targets ${f.dur(t.targetP50)} and ${f.dur(t.targetP95)}` },
    s('line', { x1: 0, x2: '100%', y1: 40, y2: 40, class: 'axis' }),
    dots,
    tick(t.targetP50, `target p50 ${f.dur(t.targetP50)}`, true, 'var(--ink-3)'),
    tick(t.targetP95, `target p95 ${f.dur(t.targetP95)}`, true, 'var(--ink-3)'),
    s('line', { x1: x(t.p50), x2: x(t.p50), y1: 6, y2: 40, stroke: 'var(--ink)', 'stroke-width': 2 }),
    s('line', { x1: x(t.p95), x2: x(t.p95), y1: 6, y2: 40, stroke: t.p95 > t.targetP95 ? 'var(--fail)' : 'var(--ink)', 'stroke-width': 2 }),
    s('text', { x: x(t.p50), y: 4, dy: -2, 'text-anchor': 'middle', style: { fill: 'var(--ink)' } }, 'p50'),
    s('text', { x: x(t.p95), y: 4, dy: -2, 'text-anchor': 'middle', style: { fill: 'var(--ink)' } }, 'p95'));
}

export function ciLens(ctx, job) {
  const cs = job.classSummary || {};
  const changes = cs.changes || [];
  if (!changes.length) return [h('p', { class: 'dim' }, 'No CI pipelines in your projects.')];
  let scale = 60;
  for (const c of changes) for (const p of c.pipelines) scale = Math.max(scale, p.queueWaitSeconds + p.runSeconds);
  const curChange = job.name.split('-')[0];
  const mq = cs.mergeQueue;
  const ttg = cs.ttg;
  const flaky = cs.flaky || [];
  const maxFlips = Math.max(1, ...flaky.map((x) => x.flips));
  return [
    h('div', { class: 'panel', 'data-key': 'ci-changes' },
      h('header', null, h('h2', null, 'Changes'), h('span', { class: 'dim' }, 'Each bar: queue wait (hatched), then run time. Then checkout time and how the cache answered.')),
      h('div', { style: { overflowX: 'auto' } },
        h('table', { class: 't ci' },
          h('thead', null, h('tr', null, h('th', null, 'Change'), STAGES.map(([, l]) => h('th', { style: { minWidth: '120px' } }, l)), h('th', { class: 'r' }, 'Time to green'))),
          h('tbody', null, changes.map((c) => {
            const by = Object.fromEntries(c.pipelines.map((p) => [p.stage, p]));
            const integ = by.integration;
            const cur = c.id === curChange;
            return h('tr', { 'data-key': c.id, style: cur ? { boxShadow: 'inset 3px 0 0 var(--bar-cyan)' } : null },
              h('td', { style: { paddingLeft: '8px', maxWidth: '300px' } },
                h('a', { class: 'mono', href: ctx.href(`job/${c.pipelines[0].job}`) }, c.id), ' ',
                h('span', { class: 'dimmer' }, c.author),
                h('div', { style: { fontSize: '12px' } }, c.title)),
              STAGES.map(([k]) => stageCell(by[k], scale, false)),
              h('td', { class: 'r num' }, integ && integ.timeToGreenSeconds ? h('span', { class: integ.timeToGreenSeconds > ttg.targetP95 ? 't-fail' : '' }, f.dur(integ.timeToGreenSeconds)) :
                c.pipelines.some((p) => p.state === 'failed') ? h('span', { class: 't-red' }, 'red') : h('span', { class: 'dimmer' }, 'pending')));
          }))))),
    h('div', { class: 'cols' },
      h('div', { class: 'panel', 'data-key': 'ci-ttg' },
        h('header', null, h('h2', null, 'Time to green'), h('span', { class: 'num dim' }, `p50 ${f.dur(ttg.p50)} · p95 ${f.dur(ttg.p95)} · ${ttg.samples.length} changes`)),
        h('div', { class: 'well' }, ttgChart(ttg)),
        h('p', { class: 'dim', style: { fontSize: '12px', margin: '6px 0 0' } },
          ttg.p95 <= ttg.targetP95 ? `Inside the p95 target, so CI deadlines relax toward it and CI takes no more of the farm than it needs.` :
            `Over the p95 target, so CI deadlines tighten until it recovers.`)),
      h('div', { class: 'panel', 'data-key': 'ci-flaky' },
        h('header', null, h('h2', null, 'Flaky targets'), h('span', { class: 'dim' }, 'failed, then passed, on the same input view')),
        flaky.length ? h('table', { class: 't' },
          h('thead', null, h('tr', null, h('th', null, 'Target'), h('th', null, 'Flips'), h('th', { class: 'r' }, 'Last seen'))),
          h('tbody', null, flaky.map((x) => h('tr', { 'data-key': x.target },
            h('td', { class: 'mono', style: { fontSize: '11px' } }, x.target, h('div', { class: 'dimmer', style: { fontSize: '10px' } }, `${x.views.length} view${x.views.length > 1 ? 's' : ''}, latest ${f.shortId(x.views[x.views.length - 1])}`)),
            h('td', { style: { width: '34%' } }, h('div', { style: { display: 'flex', alignItems: 'center', gap: '6px' } },
              h('i', { style: { display: 'block', height: '8px', width: `${(x.flips / maxFlips) * 100}%`, background: 'var(--bar-yellow)' } }),
              h('span', { class: 'num' }, String(x.flips)))),
            h('td', { class: 'r num dim' }, f.clock(x.lastSeen)))))) : h('p', { class: 'dim' }, 'No flaky targets seen.'))),
    h('div', { class: 'cols' },
      h('div', { class: 'panel', 'data-key': 'ci-mq' },
        h('header', null, h('h2', null, 'Merge queue')),
        h('div', { class: 'figs' },
          fig('Depth', String(mq.depth)), fig('Wait p50', f.dur(mq.p50WaitSeconds)), fig('Wait p95', f.dur(mq.p95WaitSeconds)),
          fig('Ejected', f.pct(mq.ejectRate, 0)), fig('Merges per hour', String(mq.mergesPerHour))),
        h('table', { class: 't' },
          h('thead', null, h('tr', null, h('th', null, 'Pos'), h('th', null, 'Change'), h('th', null, 'State'), h('th', null, 'Speculative base'))),
          h('tbody', null, mq.entries.map((e) => h('tr', { 'data-key': e.change },
            h('td', { class: 'num' }, e.position ? String(e.position) : '–'),
            h('td', { class: 'mono' }, e.change),
            h('td', null, h('span', { class: 'chip ' + ({ merged: 'green', ejected: 'red', testing: '' }[e.state] || '') }, e.state)),
            h('td', { class: 'mono dim', title: e.speculativeBase }, f.shortId(e.speculativeBase))))))),
      h('div', { class: 'panel', 'data-key': 'ci-pools' },
        h('header', null, h('h2', null, 'Runner pools')),
        (cs.runnerPools || []).map((p) => h('div', { 'data-key': p.pool, style: { marginBottom: '10px' } },
          h('div', { class: 'nrow' }, h('span', { class: 'mono' }, p.pool), h('span', { class: 'num' }, `${p.used} of ${p.slots} slots`)),
          h('div', { class: 'slots' }, Array.from({ length: p.slots }, (_, i) => h('i', { class: i < p.used ? 'on' : '' }))))))),
  ];
}

function fig(label, val) {
  return h('div', { class: 'fig' }, h('div', { class: 'lab' }, label), h('div', { class: 'val' }, val));
}
