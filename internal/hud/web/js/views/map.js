// Map: ATLAS. Where the farm is: sites as pool rings on a dot-matrix map,
// the store region, the bytes that flow from it, each site at the scale of
// its racks and nodes, and the fan-in that keeps the store quiet. Without
// declared sites the zones are drawn as a schematic, labeled as such.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { atlasSVG, siteScaleSVG, funnelSVG, fanRatio, sitesModel, zonesModel } from '../../design/components/atlas.js';
import { installTips } from '../../design/components/lattice.js';

function tcell(label, val, sub, cls = '') {
  return h('div', { class: 'cell' }, h('span', { class: 'lab' }, label), h('span', { class: 'val ' + cls }, val), sub ? h('span', { class: 'sub' }, sub) : null);
}

function svgHost(key, svg, cls = '') {
  const el = h('div', { class: 'svghost ' + cls, 'data-key': key });
  el.innerHTML = svg;
  return el;
}

// burstBlock: the burst regions, what is up, what is asked for and what the
// provider can still give; a pending request carries its approve control.
function burstBlock(ctx, snap) {
  const pools = snap.pools.filter((p) => p.burst);
  if (!pools.length) return null;
  const caps = snap.capacity || [];
  return h('div', { class: 'blk', 'data-key': 'burst' },
    h('div', { class: 'mblk-h' }, h('h2', null, 'Burst'), h('span', null, 'provisioned slots are ordinary slots; blue only while they run')),
    pools.map((p) => {
      const b = p.burst;
      const pend = caps.filter((c) => c.pool === p.name && c.state === 'PendingApproval');
      const prov = caps.filter((c) => c.pool === p.name && c.state === 'Provisioning');
      const head = Math.max(0, b.maxSlots - b.provisioned - pend.reduce((a, c) => a + c.slots, 0) - prov.reduce((a, c) => a + c.slots, 0));
      return h('div', { class: 'burst-row', 'data-key': 'b-' + p.name },
        h('div', { class: 'bn' }, h('b', null, p.name), ` · ${[p.attrs.gpuModel, p.attrs.capacityType].filter(Boolean).join(' · ')}`),
        h('div', { class: 'bv' },
          h('span', null, h('b', null, f.num(b.provisioned)), ' up'),
          prov.length ? h('span', null, `+${f.num(prov.reduce((a, c) => a + c.slots, 0))} provisioning`) : null,
          pend.length ? h('span', { class: 'pencil' }, `+${f.num(pend.reduce((a, c) => a + c.slots, 0))} requested`) : null,
          h('span', null, `${f.num(head)} headroom`),
          h('span', { class: 'dimmer' }, `${f.money(b.costPerSlotHour, true)}/slot-h · ≈${f.dur(b.provisioningSeconds)} to add`)),
        pend.map((c, k) => h('div', { class: 'lat-req', 'data-key': 'r' + k },
          h('div', { class: 'what' }, h('b', null, `+${c.slots} ${p.name} slots`), (c.jobs || []).length ? [' for ', (c.jobs || []).map((j, i) => [i ? ', ' : '', h('a', { href: ctx.href(`job/${j}`) }, f.jobName(j))])] : null,
            h('div', { class: 'why' }, c.reason)),
          h('span', { class: 'price pencil' }, `est. ${f.money(c.estCost)}`),
          h('button', { class: 'btn', 'data-action': 'approve', 'data-pool': p.name }, `Approve +${c.slots} · est. ${f.money(c.estCost)}`))));
    }));
}

export default {
  render(ctx) {
    installTips();
    const snap = ctx.snap;
    const M = sitesModel(snap);
    const groups = M.compute.length ? M.compute : zonesModel(snap);
    const scaleGroups = M.compute.length ? M.compute : [{ site: { name: 'farm', label: 'The farm, by zone', localities: [] }, nodes: snap.nodes, rings: groups.flatMap((g) => g.rings) }];
    const store = M.stores[0];
    const ratio = fanRatio(snap);
    const fi = (snap.tiers && snap.tiers.fanIn) || {};
    const avail = Math.max(480, innerWidth - 56 - 44);
    const wide = avail >= 1240;
    const mapSvg = atlasSVG(snap, { w: wide ? Math.min(1240, avail - 520 - 28) : avail, h: Math.max(420, Math.min(780, innerHeight - 330)) });
    const mapW = Number((/width="(\d+)"/.exec(mapSvg) || [])[1]) || 600;
    const side = Math.round(wide ? Math.max(420, Math.min(760, avail - mapW - 28)) : avail);
    const burst = snap.pools.filter((p) => p.burst);
    const pend = (snap.capacity || []).filter((c) => c.state === 'PendingApproval');
    return h('section', { class: 'sheet map', 'data-key': 'map', 'aria-label': 'Map: where the farm is' },
      h('div', { class: 'sheethead' },
        h('div', null,
          h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ' / map'),
          h('h1', null, 'Map'),
          h('div', { class: 'meta' }, M.compute.length
            ? `${M.compute.length} site${M.compute.length > 1 ? 's' : ''} with nodes${store ? ` · project stores in ${store.region}` : ''} · rings are pools, lit arcs slots in use, blue your running units`
            : `no sites declared · ${groups.length} zones drawn without a map`)),
        h('div', { class: 'titleblock' },
          tcell(M.compute.length ? 'Sites' : 'Zones', f.num(groups.length), M.compute.length ? groups.map((g) => g.site.kind === 'onprem' ? 'machine room' : 'cloud').filter((v, i, a) => a.indexOf(v) === i).join(' + ') : 'by node zone'),
          tcell('Nodes', f.num(snap.nodes.length), `${f.num(snap.farm.slotsTotal)} slots`),
          tcell('In use', h('span', null, f.num(snap.farm.slotsUsed), h('small', null, ` of ${f.num(snap.farm.slotsTotal)}`)), snap.viewer.allProjects ? 'all projects' : `${f.num(snap.farm.otherProjectsSlots)} other projects`),
          burst.length ? tcell('Burst', h('span', null, f.num(burst.reduce((a, p) => a + p.burst.provisioned, 0)), h('small', null, ` of ${f.num(burst.reduce((a, p) => a + p.burst.maxSlots, 0))}`)),
            pend.length ? `+${pend.reduce((a, c) => a + c.slots, 0)} awaiting approval` : 'none requested') : null,
          tcell('Store reads', `${f.num(fi.storeRequests)}`, 'per minute'),
          tcell('Fan-in', ratio ? h('span', null, h('small', null, '1 : '), f.num(ratio)) : '–', 'store reads per node request'))),
      h('div', { class: 'neat' },
        h('div', { class: 'edge w' }, '◂ ', h('a', { href: ctx.href('pools') }, 'pools'), ': what these slots are'),
        h('div', { class: 'edge e' }, h('a', { href: ctx.href('tiers') }, 'cache tiers'), ' in detail ▸'),
        h('div', { class: 'edge n' }, M.compute.length ? `▴ datum WGS 84 · equirectangular · land: Natural Earth 110m` : '▴ schematic: no datum'),
        h('div', { class: 'edge s' }, '▾ ', h('a', { href: ctx.href('cost') }, 'cost'), ': what the burst regions charge'),
        h('div', { class: 'map-grid', style: { gridTemplateColumns: wide ? `${mapW}px minmax(0, 1fr)` : '1fr' } },
          h('div', { class: 'map-main' },
            svgHost('atlas', mapSvg, 'atlas'),
            h('div', { class: 'map-key' },
              h('span', null, h('i', { class: 'k lit' }), 'slots in use'),
              h('span', null, h('i', { class: 'k run' }), 'running your units'),
              h('span', null, h('i', { class: 'k track' }), 'idle'),
              h('span', null, h('i', { class: 'k pend' }), 'requested, awaiting approval'),
              h('span', null, h('i', { class: 'k head' }), 'burst headroom'),
              h('span', null, h('i', { class: 'k flow' }), 'bytes from the store, width by slots'),
              h('span', null, h('i', { class: 'k store' }), 'the project stores'))),
          h('div', { class: 'map-side' },
            h('div', { class: 'blk' },
              h('div', { class: 'mblk-h' }, h('h2', null, 'Fan-in'), h('span', null, 'requests per minute entering each tier')),
              ratio ? h('div', { class: 'ratio-line' }, h('b', null, '1'), ' store read per ', h('b', null, f.num(ratio)), ' node requests') : null,
              svgHost('funnel', funnelSVG(snap, { w: Math.min(side, 600) })),
              h('div', { class: 'note' }, 'Each tier answers most of what reaches it and forwards only its misses. Bars are on a log scale.')),
            burstBlock(ctx, snap),
            scaleGroups.map((g) => h('div', { class: 'blk', 'data-key': 'ss-' + g.site.name },
              h('div', { class: 'mblk-h' },
                h('h2', null, g.site.label || g.site.name),
                h('span', null, `${g.nodes.length} nodes · ${poolSum(g.rings).join(' · ')}`)),
              svgHost('ss-svg-' + g.site.name, siteScaleSVG(snap, g, { w: Math.min(side, 600) }))))))));
  },
};

// poolSum: used of slots per pool across a group's rings.
function poolSum(rings) {
  const m = new Map();
  for (const q of rings) { const x = m.get(q.pool.name) || { used: 0, slots: 0 }; x.used += q.used; x.slots += q.slots; m.set(q.pool.name, x); }
  return [...m].map(([n, x]) => `${n} ${x.used}/${x.slots}`);
}
