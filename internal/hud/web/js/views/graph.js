// Dependency graph across classes: sim to render to comp to transcode to
// delivery, build to test to merge. Deadlines propagate upstream; the
// critical path is drawn heavy in ink; each edge carries the view that
// flows on it. A sheet: the title block counts what is late, a job
// projected past its deadline is outlined in red pencil (dashed), a failed
// one in red ink, a held one carries the grey hold mark.

import { h, s } from '../dom.js';
import * as f from '../fmt.js';
import { get } from '../api.js';
import { inFlight } from '../../design/components/core.js';
import { c4idSVGEl } from '../../design/components/c4id.js';

const st = { data: null, now: null, busy: false };
document.addEventListener('hud:changed', () => { st.now = null; });

const BW = 214;
const BH = 70;
const CG = 64;
const RG = 14;

function layout(data) {
  const ids = data.nodes.map((n) => n.id);
  const preds = new Map(ids.map((i) => [i, []]));
  const succs = new Map(ids.map((i) => [i, []]));
  for (const e of data.edges) {
    if (!preds.has(e.to) || !preds.has(e.from)) continue;
    preds.get(e.to).push(e.from);
    succs.get(e.from).push(e.to);
  }
  // Components.
  const comp = new Map();
  let c = 0;
  for (const id of ids) {
    if (comp.has(id)) continue;
    const stack = [id];
    while (stack.length) {
      const x = stack.pop();
      if (comp.has(x)) continue;
      comp.set(x, c);
      stack.push(...preds.get(x), ...succs.get(x));
    }
    c++;
  }
  const depth = new Map();
  const d = (id) => {
    if (depth.has(id)) return depth.get(id);
    depth.set(id, 0);
    const v = preds.get(id).length ? Math.max(...preds.get(id).map(d)) + 1 : 0;
    depth.set(id, v);
    return v;
  };
  ids.forEach(d);
  // Columns are as late as possible: each job sits one column before its
  // earliest dependent, so parallel chains line up at the sink.
  const hts = new Map();
  const ht = (id) => {
    if (hts.has(id)) return hts.get(id);
    hts.set(id, 0);
    const v = succs.get(id).length ? Math.max(...succs.get(id).map(ht)) + 1 : 0;
    hts.set(id, v);
    return v;
  };
  ids.forEach(ht);
  const groups = new Map();
  for (const id of ids) {
    const k = comp.get(id);
    if (!groups.has(k)) groups.set(k, []);
    groups.get(k).push(id);
  }
  const phase = new Map(data.nodes.map((n) => [n.id, n.phase]));
  const over = (g) => g.every((id) => phase.get(id) === 'Succeeded' || phase.get(id) === 'Failed');
  const finished = [...groups.values()].filter((g) => g.length > 1 && over(g))
    .map((g) => g.sort((a, b) => depth.get(a) - depth.get(b)));
  const multi = [...groups.values()].filter((g) => g.length > 1 && !over(g)).sort((a, b) => b.length - a.length);
  const single = [...groups.values()].filter((g) => g.length === 1).map((g) => g[0]);
  const pos = new Map();
  let y0 = 8;
  let width = 0;
  for (const g of multi) {
    const cols = new Map();
    const top = Math.max(...g.map((id) => hts.get(id)));
    for (const id of g) {
      const k = top - hts.get(id);
      if (!cols.has(k)) cols.set(k, []);
      cols.get(k).push(id);
    }
    let maxRows = 0;
    const keys = [...cols.keys()].sort((a, b) => a - b);
    for (const k of keys) {
      const col = cols.get(k);
      col.sort((a, b) => bary(a) - bary(b));
      col.forEach((id, r) => pos.set(id, { x: k * (BW + CG), y: y0 + r * (BH + RG) }));
      maxRows = Math.max(maxRows, col.length);
      width = Math.max(width, k * (BW + CG) + BW);
    }
    // Center short columns against the tallest.
    for (const k of keys) {
      const col = cols.get(k);
      const off = ((maxRows - col.length) * (BH + RG)) / 2;
      for (const id of col) pos.get(id).y += off;
    }
    y0 += maxRows * (BH + RG) + 26;
  }
  function bary(id) {
    const p = preds.get(id).map((x) => pos.get(x)).filter(Boolean);
    if (!p.length) return 0;
    return p.reduce((a, b) => a + b.y, 0) / p.length;
  }
  return { pos, height: y0, width, single, finished };
}

function nodeBox(ctx, n, p) {
  const j = ctx.job(n.id);
  const done = j ? j.units.succeeded / Math.max(1, j.units.total) : n.phase === 'Succeeded' ? 1 : 0;
  const run = j ? (inFlight(j.units)) / Math.max(1, j.units.total) : 0;
  const late = n.slackSeconds !== undefined && n.slackSeconds < 0 && n.phase !== 'Succeeded';
  const name = f.jobName(n.id);
  const phaseCol = { Failed: 'var(--fail)', Held: 'var(--g16)', Succeeded: 'var(--done)', Running: 'var(--run)' }[n.phase] || 'var(--ink-3)';
  return s('a', { href: ctx.href(`job/${n.id}`), 'data-key': n.id, class: 'gnode' + (n.critical ? ' crit' : '') },
    s('g', { class: 'gnode' + (n.critical ? ' crit' : ''), transform: `translate(${p.x} ${p.y})` },
      s('title', null, `${name}: ${n.phase}`),
      s('rect', { class: 'box' + (n.phase === 'Failed' ? ' failed' : late ? ' late' : ''), width: BW, height: BH }),
      n.phase === 'Held' ? s('g', { fill: 'var(--g16)' }, s('rect', { x: BW - 52, y: 29, width: 1.6, height: 8 }), s('rect', { x: BW - 48.4, y: 29, width: 1.6, height: 8 })) : null,
      s('text', { x: 10, y: 19, style: { 'font-weight': 650, 'font-size': '13px' } }, name.length > 27 ? name.slice(0, 26) + '…' : name),
      s('text', { x: BW - 10, y: 19, 'text-anchor': 'end', class: 'm', style: { 'text-transform': 'uppercase' } }, n.class),
      s('text', { x: 10, y: 36, class: 'm', style: { fill: phaseCol } }, n.phase),
      n.deadline ? s('text', { x: BW - 10, y: 36, 'text-anchor': 'end', class: 'm' }, `due ${f.clock(n.deadline)}`) : null,
      n.etaP90 ? s('text', { x: 10, y: 52, class: 'm' }, `p90 ${f.clock(n.etaP90)}`) : s('text', { x: 10, y: 52, class: 'm' }, 'finished'),
      n.slackSeconds !== undefined && n.phase !== 'Succeeded' ? s('text', { x: BW - 10, y: 52, 'text-anchor': 'end', class: 'm', style: { fill: late ? 'var(--risk)' : 'var(--pencil)' } }, `slack ${f.signed(n.slackSeconds)}`) : null,
      s('rect', { x: 0, y: BH - 4, width: BW, height: 4, fill: 'var(--g09)' }),
      s('rect', { x: 0, y: BH - 4, width: BW * done, height: 4, fill: 'var(--done)' }),
      run > 0 ? s('rect', { x: BW * done, y: BH - 4, width: Math.max(2, BW * run), height: 4, fill: 'var(--run)' }) : null));
}

export default {
  render(ctx) {
    if (!st.busy && st.now !== ctx.snap.now) {
      st.busy = true;
      st.now = ctx.snap.now;
      get('/graph').then((d) => { st.data = d; }).catch(() => {}).finally(() => { st.busy = false; ctx.refresh(); });
    }
    const data = st.data;
    const nodes = data ? data.nodes : [];
    const lateN = nodes.filter((n) => n.slackSeconds !== undefined && n.slackSeconds < 0 && n.phase !== 'Succeeded');
    const crit = nodes.filter((n) => n.critical);
    const tcell = (label, val, sub, cls = '') => h('div', { class: 'cell' }, h('span', { class: 'lab' }, label), h('span', { class: 'val ' + cls }, val), sub ? h('span', { class: 'sub' }, sub) : null);
    const head = h('div', { class: 'sheethead' },
      h('div', null,
        h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ' / dependencies'),
        h('h1', null, 'Dependencies'),
        h('div', { class: 'meta vmeta' }, 'Jobs across classes. A job is due no later than each dependent’s deadline minus that dependent’s duration, so a delivery’s deadline reaches back through comp and render to the sim. The critical path is drawn heavy.')),
      h('div', { class: 'titleblock' },
        tcell('Jobs', f.num(nodes.length), `${f.num(data ? data.edges.length : 0)} dependencies`),
        tcell('Critical path', f.num(crit.length), 'jobs that finish last'),
        tcell('Projected late', h('span', { class: lateN.length ? 't-risk' : '' }, f.num(lateN.length)), lateN.length ? 'projected past the deadline' : 'all on time')));
    if (!data) return h('section', { class: 'sheet vgraph' }, head, h('p', { class: 'dim' }, 'Reading the graph…'));
    const L = layout(data);
    const byId = new Map(data.nodes.map((n) => [n.id, n]));
    const edges = data.edges.filter((e) => L.pos.has(e.from) && L.pos.has(e.to)).map((e) => {
      const a = L.pos.get(e.from);
      const b = L.pos.get(e.to);
      const crit = byId.get(e.from).critical && byId.get(e.to).critical;
      const x1 = a.x + BW;
      const y1 = a.y + BH / 2;
      const x2 = b.x;
      const y2 = b.y + BH / 2;
      const mx = (x1 + x2) / 2;
      return [
        s('path', { class: 'e' + (crit ? ' crit' : ''), d: `M${x1} ${y1} C${mx} ${y1} ${mx} ${y2} ${x2 - 4} ${y2}`, 'data-key': `${e.from}>${e.to}` }),
        s('path', { d: `M${x2 - 6} ${y2 - 4} L${x2} ${y2} L${x2 - 6} ${y2 + 4}`, fill: 'none', stroke: crit ? 'var(--ink)' : 'var(--g13)', 'stroke-width': crit ? 2 : 1.2 }),
        // the view the edge carries, as the C4 mark
        e.view ? c4idSVGEl(e.view, mx - 6, (y1 + y2) / 2 - 17, 12, { label: 'view carried' }) : null,
      ];
    });
    const singles = L.single.map((id) => byId.get(id));
    return h('section', { class: 'sheet vgraph', 'data-key': 'graph', 'aria-label': 'Dependencies' }, head,
      h('div', { class: 'neat' },
      h('div', { class: 'edge w' }, '◂ ', h('a', { href: ctx.href('queue') }, 'queue'), ': the same jobs in run order'),
      h('div', { class: 'edge e' }, h('a', { href: ctx.href('cost') }, 'cost'), ': what the late chains need ▸'),
      h('div', { class: 'vgwell' },
        s('svg', { width: Math.max(L.width + 10, 600), height: L.height, viewBox: `-2 0 ${Math.max(L.width + 10, 600)} ${L.height}`, style: { width: `${Math.max(L.width + 10, 600)}px`, maxWidth: 'none' }, role: 'group', 'aria-label': 'Dependency graph' },
          edges, data.nodes.filter((n) => L.pos.has(n.id)).map((n) => nodeBox(ctx, n, L.pos.get(n.id))))),
      h('div', { class: 'legend vlegend' },
        h('span', null, h('i', { class: 'lg crit' }), 'critical path: the inputs that finish last'),
        h('span', null, h('i', { class: 'lg dep' }), 'dependency; the mark on it is the view that flows along it (hover to read, click to copy)'),
        h('span', null, h('i', { class: 'lg late' }), 'projected late (orchid, dashed)'),
        h('span', null, h('i', { class: 'lg failed' }), 'failed'),
        h('span', null, h('i', { class: 'lg bar' }), 'units done, and running now')),
      L.finished.length ? h('div', { class: 'vsec', style: { marginTop: '22px' } },
        h('header', null, h('h2', null, 'Finished chains')),
        h('table', { class: 'vt' }, h('tbody', null, L.finished.map((g) => h('tr', { 'data-key': g.join('>') },
          h('td', null, g.map((id, i) => [i ? ' → ' : '', h('a', { href: ctx.href(`job/${id}`) }, f.jobName(id))])),
          h('td', { class: 'dim' }, g.map((id) => byId.get(id).phase).includes('Failed') ? h('span', { class: 't-fail' }, 'ended with a failure') : 'all succeeded')))))) : null,
      singles.length ? h('div', { class: 'vsec', style: { marginTop: '22px' } },
        h('header', null, h('h2', null, 'Jobs with no dependencies')),
        h('table', { class: 'vt' }, h('tbody', null, singles.map((n) => h('tr', { 'data-key': n.id },
          h('td', null, h('a', { href: ctx.href(`job/${n.id}`) }, f.jobName(n.id))),
          h('td', { class: 'mono dim' }, n.class), h('td', { class: 'dim' }, n.phase),
          h('td', { class: 'num' }, n.deadline ? `due ${f.clock(n.deadline)}` : ''),
          h('td', { class: 'num r ' + (n.slackSeconds < 0 && n.phase !== 'Succeeded' ? 't-risk pencil' : 'pencil') }, n.slackSeconds !== undefined && n.phase !== 'Succeeded' ? `slack ${f.signed(n.slackSeconds)}` : '')))))) : null));
  },
};
