// ML lens: ranks in the job view, checkpoints as views on a timeline, yields
// and resumes, and useful against lost GPU hours.

import { h, s } from '../dom.js';
import * as f from '../fmt.js';

export function mlLens(ctx, job, rank0) {
  const cs = job.classSummary || {};
  const cks = cs.checkpoints || [];
  const ys = cs.yields || [];
  const atts = (rank0 && rank0.attempts) || [];
  const now = f.now();
  const t0 = Math.min(...[...atts.map((a) => f.ms(a.start)), ...cks.map((c) => f.ms(c.at)), now - 3600e3]);
  const t1 = Math.max(now, f.ms(job.estimate.etaP90)) + 300e3;
  const x = (t) => `${((t - t0) / (t1 - t0)) * 100}%`;
  const wx = (a, b) => `${Math.max(0.2, ((b - a) / (t1 - t0)) * 100)}%`;
  const ranks = cs.ranks || 4;
  const rows = [];
  const laneY = (r) => 30 + r * 16;
  for (let r = 0; r < ranks; r++) {
    rows.push(s('text', { x: -8, y: laneY(r) + 9, 'text-anchor': 'end' }, `rank ${r}`));
    for (const a of atts) {
      const a0 = f.ms(a.start);
      const a1 = a.end ? f.ms(a.end) : now;
      const failed = a.exitCode && a.exitCode !== 0;
      // Work up to the last checkpoint survived; after it, a failure loses it.
      const saved = failed ? Math.max(a0, ...cks.map((c) => f.ms(c.at)).filter((t) => t >= a0 && t <= a1)) : a1;
      rows.push(s('rect', { x: x(a0), y: laneY(r), width: wx(a0, saved), height: 11, fill: 'var(--u-done)' }));
      if (failed) rows.push(s('rect', { x: x(saved), y: laneY(r), width: wx(saved, a1), height: 11, fill: 'var(--bar-red)' }));
    }
    for (const y of ys) {
      const y0 = f.ms(y.at);
      const y1 = y.resumedAt ? f.ms(y.resumedAt) : now;
      rows.push(s('rect', { x: x(y0), y: laneY(r) + 0.75, width: wx(y0, y1), height: 9.5, fill: 'none', stroke: 'var(--bar-magenta)', 'stroke-width': 1.5 }));
    }
    rows.push(s('rect', { x: x(now), y: laneY(r), width: wx(now, f.ms(job.estimate.etaP90)), height: 11, fill: 'none', stroke: 'var(--ink-3)', 'stroke-dasharray': '2 2' }));
  }
  const ckTicks = cks.map((c, i) => [
    s('line', { x1: x(f.ms(c.at)), x2: x(f.ms(c.at)), y1: 14, y2: laneY(ranks - 1) + 14, stroke: 'var(--ink-3)', 'stroke-width': 1 }),
    s('rect', { x: x(f.ms(c.at)), y: 12, width: 5, height: 5, fill: 'var(--ink)', transform: 'translate(-2.5 0)' }),
    i % 2 === 0 || i === cks.length - 1 ? s('text', { x: x(f.ms(c.at)), y: 8, 'text-anchor': 'middle' }, String(c.step)) : null,
  ]);
  const nowLine = s('line', { x1: x(now), x2: x(now), y1: 4, y2: laneY(ranks - 1) + 18, stroke: 'var(--bar-cyan)', 'stroke-width': 1.5 });
  const total = cs.usefulGpuHours + cs.lostGpuHours || 1;
  const timeline = s('svg', { class: 'chart', style: { height: `${laneY(ranks - 1) + 40}px`, marginLeft: '48px', width: 'calc(100% - 48px)' },
    role: 'img', 'aria-label': `${cks.length} checkpoints, ${ys.length} yields` },
    ckTicks, rows, nowLine,
    s('text', { x: x(now), y: laneY(ranks - 1) + 32, 'text-anchor': 'middle', style: { fill: 'var(--t-cyan)' } }, `now, step ${cs.step}`),
    s('text', { x: 0, y: laneY(ranks - 1) + 32 }, f.clock(t0)),
    s('text', { x: '100%', y: laneY(ranks - 1) + 32, 'text-anchor': 'end' }, f.clock(t1)));
  const lastCk = cks[cks.length - 1];
  return [
    h('div', { class: 'panel', 'data-key': 'ml-gpu' },
      h('header', null, h('h2', null, 'GPU hours'), h('span', { class: 'dim' }, `Useful: work that survived in a checkpoint. Lost: work redone after a failure, and restore time.`)),
      h('div', { class: 'figs' },
        h('div', { class: 'fig' }, h('div', { class: 'lab' }, 'Useful'), h('div', { class: 'val' }, `${cs.usefulGpuHours.toFixed(1)} h`)),
        h('div', { class: 'fig' }, h('div', { class: 'lab' }, 'Lost'), h('div', { class: 'val t-red' }, `${cs.lostGpuHours.toFixed(2)} h`)),
        h('div', { class: 'fig' }, h('div', { class: 'lab' }, 'Steps'), h('div', { class: 'val' }, `${f.num(cs.step)} / ${f.num(cs.totalSteps)}`)),
        h('div', { class: 'fig' }, h('div', { class: 'lab' }, 'Wrapped'), h('div', { class: 'val' }, `${cs.wrapped}, ${ranks} ranks`))),
      h('div', { style: { display: 'flex', height: '8px', background: 'var(--well)' }, 'aria-hidden': 'true' },
        h('i', { style: { width: `${(cs.usefulGpuHours / total) * 100}%`, background: 'var(--u-done)' } }),
        h('i', { style: { width: `${(cs.lostGpuHours / total) * 100}%`, background: 'var(--bar-red)' } }))),
    h('div', { class: 'panel', 'data-key': 'ml-tl' },
      h('header', null, h('h2', null, 'Checkpoints and yields'), h('span', { class: 'dim' }, 'Squares are checkpoints (each a C4 view); magenta outlines are time yielded to other work.')),
      h('div', { class: 'well', style: { paddingTop: '16px' } }, timeline),
      h('div', { class: 'legend' },
        h('span', null, h('i', { class: 'lg-done' }), 'ran'),
        h('span', null, h('i', { class: 'lg-failed' }), 'lost: after the last checkpoint of a failed attempt'),
        h('span', null, h('i', { class: 'lg-held' }), 'yielded'),
        h('span', null, h('i', { style: { background: 'transparent', outline: '1px dashed var(--ink-3)', outlineOffset: '-1px' } }), 'remaining, at p90'))),
    h('div', { class: 'cols' },
      h('div', { class: 'panel', 'data-key': 'ml-yields' },
        h('header', null, h('h2', null, 'Yields')),
        ys.length ? h('table', { class: 't' },
          h('thead', null, h('tr', null, h('th', null, 'Yielded'), h('th', null, 'Resumed'), h('th', null, 'Why'))),
          h('tbody', null, ys.map((y, i) => h('tr', { 'data-key': 'y' + i },
            h('td', { class: 'num' }, f.clock(y.at)),
            h('td', { class: 'num' }, y.resumedAt ? f.clock(y.resumedAt) : h('span', { class: 't-magenta' }, 'waiting')),
            h('td', { style: { fontSize: '12px' } }, y.reason))))) : h('p', { class: 'dim' }, 'This run has not yielded.')),
      h('div', { class: 'panel', 'data-key': 'ml-views' },
        h('header', null, h('h2', null, 'Reproducible from')),
        h('dl', { class: 'kv' },
          h('dt', null, 'Dataset view'), h('dd', { class: 'mono', title: cs.datasetView }, f.shortId(cs.datasetView)),
          h('dt', null, 'Code view'), h('dd', { class: 'mono', title: cs.codeView }, f.shortId(cs.codeView)),
          lastCk ? [h('dt', null, 'Latest checkpoint'), h('dd', { class: 'mono', title: lastCk.view }, `${f.shortId(lastCk.view)}  step ${lastCk.step}`)] : null,
          lastCk ? [h('dt', null, 'Stored per checkpoint'), h('dd', null, `${f.bytes(lastCk.bytesNew)} new; ${f.bytes(lastCk.bytesDeduped)} already in the store`)] : null))),
  ];
}
