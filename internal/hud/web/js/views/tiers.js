// Cache tiers: hit rate per tier and the fan-in from requesters to the
// store. Each tier forwards only its misses, so the store sees a trickle.

import { h } from '../dom.js';
import * as f from '../fmt.js';

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
    return h('section', null,
      h('div', { class: 'scopebar' },
        h('div', null, h('h1', null, 'Cache tiers'),
          h('div', { class: 'dim', style: { marginTop: '4px' } }, 'Node daemon, then peers, then the regional cache, then the store. Every tier answers the same requests; each forwards only what it misses.'))),
      h('div', { class: 'cols2' },
        h('div', { class: 'section' },
          h('header', null, h('h2', null, 'Fan-in, requests per minute'),
            h('span', { class: 'num dim' }, ratio ? `1 store read per ${f.num(ratio)} node requests` : 'no store reads')),
          h('div', { class: 'funnel' }, steps.map(([name, n, note], i) => h('div', { class: 'row', 'data-key': name },
            h('div', null, h('div', { class: 'name' }, name), h('div', { class: 'dimmer', style: { fontSize: '11px' } }, note)),
            h('div', { class: 'bar', role: 'img', 'aria-label': `${f.num(n)}` },
              h('i', { style: { width: `${Math.max(0.6, (Math.log10(Math.max(1, n)) / max) * 100)}%`, background: i === 0 ? 'var(--ink-3)' : 'var(--ink-2)' } })),
            h('div', { class: 'num', style: { fontSize: '18px', textAlign: 'right' } }, f.num(n))))),
          h('p', { class: 'dim', style: { fontSize: '12px' } }, 'Bars are on a log scale: each step down is a tier answering most of what reaches it.')),
        h('div', { class: 'section' },
          h('header', null, h('h2', null, 'Publish and capture')),
          h('div', { class: 'figs' },
            h('div', { class: 'fig' }, h('div', { class: 'lab' }, 'Publish p50'), h('div', { class: 'big' }, `${t.publishP50.toFixed(2)}s`)),
            h('div', { class: 'fig' }, h('div', { class: 'lab' }, 'Publish p95'), h('div', { class: 'big' }, `${t.publishP95.toFixed(2)}s`)),
            h('div', { class: 'fig' }, h('div', { class: 'lab' }, 'Capture p50'), h('div', { class: 'big' }, `${t.captureP50.toFixed(2)}s`))),
          h('p', { class: 'dim', style: { fontSize: '12px' } }, 'Publish: a captured changeset is in the store and its view is readable. Capture: an upper directory becomes a changeset.'))),
      h('div', { class: 'section' },
        h('header', null, h('h2', null, 'Tiers')),
        h('table', { class: 't' },
          h('thead', null, h('tr', null, ['Tier', 'Hit rate', 'Requests in', 'Sent upstream', 'Bytes served', 'Shed', 'Fills in flight'].map((x, i) => h('th', { class: i ? 'r' : '' }, x)))),
          h('tbody', null, t.tiers.map((x) => h('tr', { 'data-key': x.name },
            h('td', { style: { fontWeight: 650, textTransform: 'uppercase', letterSpacing: '0.06em', fontStretch: '70%' } }, x.name),
            h('td', { class: 'r' }, h('div', { style: { display: 'flex', justifyContent: 'flex-end', alignItems: 'center', gap: '8px' } },
              h('span', { style: { display: 'inline-block', width: '90px', height: '6px', background: 'var(--u-queued)' } },
                h('i', { style: { display: 'block', height: '100%', width: `${x.hitRate * 100}%`, background: 'var(--ink-2)' } })),
              h('span', { class: 'num' }, f.pct(x.hitRate)))),
            h('td', { class: 'r num' }, f.num(x.requestsIn)),
            h('td', { class: 'r num' }, f.num(x.upstreamRequests)),
            h('td', { class: 'r num' }, f.bytes(x.bytesServed)),
            h('td', { class: 'r num ' + (x.shedRate > 0.001 ? 't-yellow' : '') }, f.pct(x.shedRate, 2)),
            h('td', { class: 'r num' }, String(x.inflightFills))))))));
  },
};
