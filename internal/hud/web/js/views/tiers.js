// Cache tiers: hit rate per tier and the fan-in from requesters to the
// store. Each tier forwards only its misses, so the store sees a trickle.
// A sheet: the title block carries the glance numbers; the funnel, the
// publish and capture times and the tier table sit inside the neatline.

import { h } from '../dom.js';
import * as f from '../fmt.js';

function tcell(label, val, sub, cls = '') {
  return h('div', { class: 'cell' }, h('span', { class: 'lab' }, label), h('span', { class: 'val ' + cls }, val), sub ? h('span', { class: 'sub' }, sub) : null);
}

export default {
  render(ctx) {
    const t = ctx.snap.tiers;
    const fi = t.fanIn;
    const steps = [
      ['Pods', fi.requesters, 'running units reading their views'],
      ['Node daemons', fi.nodeRequests, 'object requests into the node tier'],
      ['Peers', fi.peerRequests, 'node misses, sent to rendezvous home peers'],
      ['Regional', fi.regionalRequests, 'peer misses'],
      ['Store', fi.storeRequests, 'regional misses: the only reads the store sees'],
    ];
    const max = Math.log10(Math.max(10, fi.nodeRequests));
    const ratio = fi.storeRequests ? Math.round(fi.nodeRequests / fi.storeRequests) : null;
    const node = t.tiers.find((x) => x.name === 'node');
    const shed = t.tiers.filter((x) => x.shedRate > 0.001);
    return h('section', { class: 'sheet vtiers', 'data-key': 'tiers', 'aria-label': 'Cache tiers' },
      h('div', { class: 'sheethead' },
        h('div', null,
          h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ' / cache tiers'),
          h('h1', null, 'Cache tiers'),
          h('div', { class: 'meta' }, 'Node daemon, then peers, then the regional cache, then the store. Every tier answers the same requests; each forwards only what it misses.')),
        h('div', { class: 'titleblock' },
          tcell('Fan-in', ratio ? h('span', null, h('small', null, '1 : '), f.num(ratio)) : '–', 'store reads per node request'),
          tcell('Node hit rate', node ? f.pct(node.hitRate) : '–', 'answered on the node'),
          tcell('Store reads', f.num(fi.storeRequests), 'per minute'),
          tcell('Publish p50', `${t.publishP50.toFixed(2)}s`, `p95 ${t.publishP95.toFixed(2)}s`),
          tcell('Shedding', h('span', { class: shed.length ? 't-temp' : '' }, shed.length ? shed.map((x) => x.name).join(', ') : 'none'), shed.length ? 'temporary: a tier is past its fill limit' : 'every tier is filling'))),
      h('div', { class: 'neat' },
        h('div', { class: 'edge w' }, '◂ ', h('a', { href: ctx.href('map') }, 'map'), ': where the tiers are'),
        h('div', { class: 'edge e' }, h('a', { href: ctx.href('cost') }, 'cost'), ': bytes avoided ▸'),
        h('div', { class: 'edge s' }, '▾ ', h('a', { href: ctx.href('pools') }, 'pools'), ': the nodes these tiers run on'),
        h('div', { class: 'vgrid2' },
          h('div', { class: 'vsec' },
            h('header', null, h('h2', null, 'Fan-in, requests per minute'),
              h('span', null, ratio ? `1 store read per ${f.num(ratio)} node requests` : 'no store reads')),
            h('div', { class: 'funnel' }, steps.map(([name, n, note], i) => h('div', { class: 'frow', 'data-key': name },
              h('div', null, h('div', { class: 'name' }, name), h('div', { class: 'note' }, note)),
              h('div', { class: 'bar', role: 'img', 'aria-label': `${f.num(n)}` },
                h('i', { style: { width: `${Math.max(0.6, (Math.log10(Math.max(1, n)) / max) * 100)}%`, background: `var(--g${[13, 17, 16, 15, 14][i]})` } })),
              h('div', { class: 'num' }, f.num(n))))),
            h('p', { class: 'vnote' }, 'Bars are on a log scale: each step down is a tier answering most of what reaches it.')),
          h('div', { class: 'vsec' },
            h('header', null, h('h2', null, 'Publish and capture'), h('span', null, 'measured, seconds')),
            h('div', { class: 'figs' },
              h('div', { class: 'fig' }, h('div', { class: 'lab' }, 'Publish p50'), h('div', { class: 'big ink' }, `${t.publishP50.toFixed(2)}s`)),
              h('div', { class: 'fig' }, h('div', { class: 'lab' }, 'Publish p95'), h('div', { class: 'big ink' }, `${t.publishP95.toFixed(2)}s`)),
              h('div', { class: 'fig' }, h('div', { class: 'lab' }, 'Capture p50'), h('div', { class: 'big ink' }, `${t.captureP50.toFixed(2)}s`))),
            h('p', { class: 'vnote' }, 'Publish: a captured changeset is in the store and its view is readable. Capture: an upper directory becomes a changeset.'))),
        h('div', { class: 'vsec' },
          h('header', null, h('h2', null, 'Tiers'), h('span', null, 'in fetch order')),
          h('table', { class: 'vt' },
            h('thead', null, h('tr', null, ['Tier', 'Hit rate', 'Requests in', 'Sent upstream', 'Bytes served', 'Shed', 'Fills in flight'].map((x, i) => h('th', { class: i ? 'r' : '' }, x)))),
            h('tbody', null, t.tiers.map((x) => h('tr', { 'data-key': x.name },
              h('td', { class: 'tname' }, x.name),
              h('td', { class: 'r' }, h('div', { class: 'hit' },
                h('span', { class: 'hbar' }, h('i', { style: { width: `${x.hitRate * 100}%` } })),
                h('span', { class: 'num' }, f.pct(x.hitRate)))),
              h('td', { class: 'r num' }, f.num(x.requestsIn)),
              h('td', { class: 'r num' }, f.num(x.upstreamRequests)),
              h('td', { class: 'r num' }, f.bytes(x.bytesServed)),
              h('td', { class: 'r num ' + (x.shedRate > 0.001 ? 't-temp' : '') }, f.pct(x.shedRate, 2)),
              h('td', { class: 'r num' }, String(x.inflightFills)))))))));
  },
};
