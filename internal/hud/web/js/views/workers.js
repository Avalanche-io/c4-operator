// Workers: nodes grouped by pool and locality, with state, slots, recent
// failures, the suspect flag and cache warmth. Node names and health are
// shared infrastructure; what a node runs is shown only for your projects.

import { h } from '../dom.js';
import * as f from '../fmt.js';

const STATE_CHIP = { busy: '', idle: '', disabled: 'red', warming: 'blue', unprovisioned: 'blue', notReady: 'red' };

function nodeCard(ctx, n, blacklistedBy) {
  const mineSlots = (n.running || []).reduce((acc, r) => acc + (r.slots || 1), 0);
  const other = Math.max(0, n.used - mineSlots);
  const slots = [];
  for (let i = 0; i < n.slots; i++) {
    let c = '';
    if (n.state === 'unprovisioned') c = 'off';
    else if (i < mineSlots) c = 'on';
    else if (i < mineSlots + other) c = 'other';
    slots.push(h('i', { class: c }));
  }
  const runs = (n.running || []).slice(0, 3).map((r, k) => {
    const j = ctx.job(r.job);
    return h('div', { 'data-key': `${r.job}#${r.unit}#${k}` },
      h('a', { href: ctx.href(`job/${r.job}/unit/${r.unit}`) }, f.jobName(r.job)), ` unit ${r.unit}`, j ? '' : '');
  });
  if ((n.running || []).length > 3) runs.push(h('div', { class: 'dimmer' }, `and ${(n.running || []).length - 3} more`));
  const disabled = n.state === 'disabled';
  return h('div', { class: `node ${n.suspect ? 'suspect' : ''} ${n.state}`, 'data-key': n.name },
    h('header', null,
      h('span', { class: 'nn' }, n.name),
      h('span', null,
        n.suspect ? h('span', { class: 'chip flag', title: 'Failed units across several jobs; usually hardware' }, 'suspect') : null, ' ',
        h('span', { class: 'chip ' + (STATE_CHIP[n.state] || '') }, n.state))),
    h('div', { class: 'slots', role: 'img', 'aria-label': `${n.used} of ${n.slots} slots in use` }, slots),
    h('div', { class: 'nrow' },
      h('span', { class: 'num' }, `${n.used}/${n.slots} slots`, other ? h('span', { class: 'dimmer' }, ` +${other} other`) : null),
      h('span', { class: 'num ' + (n.failuresLastHour ? 't-red' : 'dimmer') }, `${n.failuresLastHour} failed /h`)),
    h('div', { class: 'cachebar', title: `cache hit rate ${f.pct(n.cacheHitRate)}` }, h('i', { style: { width: `${(n.cacheHitRate * 100).toFixed(1)}%` } })),
    h('div', { class: 'nrow' },
      h('span', { class: 'num' }, n.state === 'unprovisioned' ? 'no cache yet' : `cache ${f.bytes(n.cacheBytes)}`),
      h('span', { class: 'num' }, n.state === 'unprovisioned' ? '' : `${f.pct(n.cacheHitRate)} hit`)),
    blacklistedBy.length ? h('div', { class: 't-yellow', style: { fontSize: '11px', marginTop: '4px' } }, `Blacklisted by ${blacklistedBy.map(f.jobName).join(', ')}`) : null,
    h('div', { class: 'runs' }, runs.length ? runs : h('span', { class: 'dimmer' }, n.used ? 'running work for other projects' : '')),
    n.state === 'unprovisioned' ? null : h('div', { style: { marginTop: '6px' } },
      disabled ? h('button', { class: 'btn small', 'data-action': 'enable', 'data-node': n.name }, 'Enable node')
        : h('button', { class: 'btn small', 'data-action': 'disable', 'data-node': n.name, title: 'Taint the node: running units finish, no new units start' }, 'Disable node farm-wide')));
}

export default {
  render(ctx) {
    const snap = ctx.snap;
    const bl = new Map();
    for (const j of snap.jobs) for (const n of j.blacklist || []) {
      if (!bl.has(n)) bl.set(n, []);
      bl.get(n).push(`${j.namespace}/${j.name}`);
    }
    const hot = snap.nodes.filter((n) => n.failuresLastHour > 0).sort((a, b) => b.failuresLastHour - a.failuresLastHour);
    return h('section', null,
      h('div', { class: 'scopebar' },
        h('div', null, h('h1', null, 'Workers'),
          h('div', { class: 'dim', style: { marginTop: '4px' } }, `${snap.nodes.length} nodes in ${snap.pools.length} pools. Green slots run your projects' units; gray slots run other projects'.`)),
        hot.length ? h('div', { class: 'warn red', style: { fontSize: '12.5px' } },
          `Failure hot spots: ${hot.slice(0, 3).map((n) => `${n.name} ${n.failuresLastHour}/h${n.suspect ? ' (suspect)' : ''}`).join(', ')}`) : null),
      snap.pools.map((p) => {
        const nodes = snap.nodes.filter((n) => n.pool === p.name);
        const locs = [...new Set(nodes.map((n) => n.locality))];
        const a = p.attrs;
        return h('div', { class: 'pool section', 'data-key': p.name },
          h('header', null,
            h('h2', null, p.name, ' ', h('span', { class: 'dim', style: { textTransform: 'none', letterSpacing: 0, fontWeight: 400 } },
              [a.class, a.gpuModel, a.vram, a.capacityType].filter(Boolean).join(' · '))),
            h('span', { class: 'num dim' }, `${p.used} of ${p.slots} slots · ${f.money(p.costPerSlotHour, true)} per slot-hour`,
              p.burst ? h('span', { class: 't-blue' }, ` · burst ${p.burst.provisioned} of ${p.burst.maxSlots} slots provisioned, ${p.burst.requested} requested; new nodes take ${f.dur(p.burst.provisioningSeconds)}`) : null)),
          h('div', { class: 'locs' }, locs.map((loc) => h('div', { 'data-key': loc, class: 'loc' },
            h('div', { class: 'lab', style: { margin: '0 0 5px' } }, loc),
            h('div', { class: 'nodes' }, nodes.filter((n) => n.locality === loc).map((n) => nodeCard(ctx, n, bl.get(n.name) || [])))))));
      }));
  },
};
