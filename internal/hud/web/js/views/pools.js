// Pools: a sheet in two halves. On the left LATTICE, the regularized view:
// what the slots are, one identical cell per slot, sorted by project, with
// quotas, borrowing, burst, requests and headroom. On the right the same
// farm by site, rack and node: where the slots are, what state each node
// is in, what it runs for your projects, its failures and cache warmth, and
// the controls that act on it. Past a thousand nodes the right half becomes
// one cell per node on a canvas, exceptions at the end of each pool.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { own } from '../../design/components/live.js';
import { latticeSVG, poolCells, nodeSlots, nodeRank, installTips } from '../../design/components/lattice.js';

const st = { node: null };
const SCALE = 1000;

document.addEventListener('lattice:node', (e) => {
  st.node = e.detail.node;
  document.dispatchEvent(new CustomEvent('hud:refresh'));
});

const keyOf = (j) => `${j.namespace}/${j.name}`;

function tcell(label, val, sub, cls = '') {
  return h('div', { class: 'cell' }, h('span', { class: 'lab' }, label), h('span', { class: 'val ' + cls }, val), sub ? h('span', { class: 'sub' }, sub) : null);
}

// sitesOf groups nodes by site and locality. Without declared sites the
// nodes group by zone (or locality) under the farm itself.
export function sitesOf(snap) {
  const nodes = snap.nodes || [];
  const out = [];
  const taken = new Set();
  for (const s of snap.sites || []) {
    const ns = nodes.filter((n) => n.region && n.region === s.region);
    if (!ns.length) { out.push({ site: s, locs: [], nodes: [] }); continue; }
    ns.forEach((n) => taken.add(n.name));
    const order = [...(s.localities || [])];
    for (const n of ns) if (!order.includes(n.locality)) order.push(n.locality);
    out.push({ site: s, nodes: ns, locs: order.map((l) => ({ name: l, nodes: ns.filter((n) => n.locality === l) })).filter((l) => l.nodes.length) });
  }
  const rest = nodes.filter((n) => !taken.has(n.name));
  if (rest.length) {
    const order = [];
    for (const n of rest) { const l = n.zone || n.locality || 'unplaced'; if (!order.includes(l)) order.push(l); }
    out.push({ site: snap.sites && snap.sites.length ? { name: 'elsewhere', label: 'Nodes with no declared site' } : null, nodes: rest,
      locs: order.map((l) => ({ name: l, nodes: rest.filter((n) => (n.zone || n.locality || 'unplaced') === l) })) });
  }
  return out;
}

function poolBlock(ctx, p, width) {
  const snap = ctx.snap;
  const L = latticeSVG(snap, p, { width });
  const M = L.model;
  const a = p.attrs || {};
  const host = h('div', { class: 'lat-svg', 'data-key': 'svg-' + p.name });
  host.innerHTML = L.svg;
  const b = p.burst;
  const borrowed = M.runs.reduce((n, r) => n + Math.max(0, r.used - r.nominal), 0);
  const jobsOf = (c) => (c.jobs || []).map((k) => {
    const j = ctx.job(k);
    return j ? h('a', { href: ctx.href(`job/${k}`) }, j.name) : f.jobName(k);
  });
  return h('div', { class: 'lat-pool', 'data-key': 'pool-' + p.name, role: 'group', 'aria-label': `Pool ${p.name}` },
    h('header', null,
      h('h2', null, p.name),
      h('span', { class: 'attrs' }, [a.class, a.gpuModel, a.vram, a.capacityType].filter(Boolean).join(' · ')),
      h('span', { class: 'use' }, h('b', null, f.num(p.used)), ` of ${f.num(p.slots)} slots`, ` · ${f.money(p.costPerSlotHour, true)}/slot-h`)),
    host,
    h('div', { class: 'lat-foot' },
      M.per > 1 ? h('span', null, `1 cell = ${f.num(M.per)} slots`) : null,
      borrowed ? h('span', null, `${f.num(borrowed)} borrowed past guaranteed`) : null,
      M.other ? h('span', null, `${f.num(M.other)} in use by other projects`) : null,
      b ? h('span', null, `burst ${f.num(b.provisioned)} of ${f.num(b.maxSlots)} up · `, h('span', { class: 'pencil' }, `≈${f.dur(b.provisioningSeconds)}`), ` to add more · ${f.money(b.costPerSlotHour, true)}/slot-h`) : null),
    M.provisioning.map((c, k) => h('div', { class: 'lat-req prov', 'data-key': 'prov' + k },
      h('span', null, `+${c.slots} ${p.name} slots expected`, c.jobs && c.jobs.length ? [' for ', jobsOf(c)] : null))),
    M.pending.map((c, k) => h('div', { class: 'lat-req', 'data-key': 'req' + k },
      h('div', { class: 'what' },
        h('b', null, `+${c.slots} ${p.name} slots`), c.jobs && c.jobs.length ? [' for ', jobsOf(c)] : null,
        h('div', { class: 'why' }, c.reason, ' · until ', f.clock(c.until))),
      h('span', { class: 'price pencil' }, `est. ${f.money(c.estCost)}`),
      h('button', { class: 'btn', 'data-action': 'approve', 'data-pool': p.name, title: 'Approve the pending request; your approval is an entry in the book' }, `Approve +${c.slots} · est. ${f.money(c.estCost)}`))));
}

function slotCells(ctx, n) {
  const s = nodeSlots(n);
  const out = [];
  s.mine.forEach((r, k) => out.push(h('a', { class: 's mine', href: ctx.href(`job/${r.job}/unit/${r.unit}`), 'data-key': 'm' + k, 'data-tip': `${n.name} · slot ${k + 1} of ${n.slots} | ${f.jobName(r.job)} · unit ${r.unit}${r.slots > 1 ? ` · holds ${r.slots} slots` : ''} | click opens the unit` })));
  for (let k = 0; k < s.other; k++) out.push(h('i', { class: 's other', 'data-tip': `${n.name} · slot ${s.mine.length + k + 1} of ${n.slots} | another project’s unit | shown only as a total` }));
  for (let k = 0; k < s.idle; k++) out.push(h('i', { class: 's idle', 'data-tip': `${n.name} · slot ${s.mine.length + s.other + k + 1} of ${n.slots} | ${n.state === 'disabled' ? 'disabled: no new units start' : n.state === 'notReady' ? 'not ready' : 'idle'}` }));
  for (let k = 0; k < s.capped; k++) out.push(h('i', { class: 's capped', 'data-tip': `${n.name} · capped slot ${n.shapeSlots - s.capped + k + 1} of ${n.shapeSlots} | the node’s slot limit takes it away${n.capacityReason ? ` | ${n.capacityReason}` : ''} | offered again when the limit rises` }));
  return out;
}

function nodeCard(ctx, n, bl) {
  const s = nodeSlots(n);
  const runs = new Map();
  for (const r of s.mine) runs.set(r.job, (runs.get(r.job) || 0) + 1);
  const off = n.state === 'unprovisioned';
  const stateCls = { notReady: 'red', disabled: '', warming: '', unprovisioned: '' }[n.state];
  return h('div', { class: `ncard st-${n.state}${n.suspect ? ' suspect' : ''}${st.node === n.name ? ' sel' : ''}`, 'data-key': 'n-' + n.name, role: 'group', 'aria-label': `Node ${n.name}, ${n.state}${n.suspect ? ', suspect' : ''}` },
    h('div', { class: 'nh' },
      h('span', { class: 'nn' }, n.name),
      n.suspect ? h('span', { class: 'chip temp', title: 'Node faults across two or more jobs in the last hour' }, 'suspect') : null,
      h('span', { class: 'chip ' + (stateCls || '') }, n.state === 'notReady' ? 'not ready' : n.state)),
    h('div', { class: 'nslots', role: 'img', 'aria-label': `${n.used} of ${n.slots} slots in use${n.capped ? `, capped at ${n.slots} of ${n.shapeSlots}` : ''}` }, slotCells(ctx, n)),
    h('div', { class: 'nrow' },
      h('span', null, h('b', null, `${n.used}`), `/${n.slots} slots`, s.other ? h('span', { class: 'dimmer' }, ` · ${s.other} other`) : null),
      h('span', { class: n.failuresLastHour ? 't-fail' : 'dimmer' }, `${n.failuresLastHour} failed/h`)),
    n.capped ? h('div', { class: 'ncap' }, `capped at ${n.slots} of ${n.shapeSlots}${n.capacityReason ? `: ${n.capacityReason}` : ''}`,
      s.over ? h('span', { class: 'dimmer' }, ` · ${s.over} still running past the cap, each stopping at the end of its turn`) : null) : null,
    off ? h('div', { class: 'nrow dimmer' }, 'no cache yet') : h('div', { class: 'ncache', 'data-tip': `${n.name} · cache | ${f.bytes(n.cacheBytes)} held | ${f.pct(n.cacheHitRate)} of reads answered on the node` },
      h('i', { style: { width: `${(n.cacheHitRate * 100).toFixed(1)}%` } }),
      h('span', null, `cache ${f.bytes(n.cacheBytes)} · ${f.pct(n.cacheHitRate)} hit`)),
    runs.size ? h('div', { class: 'nruns' }, [...runs].map(([j, k], i) => h('span', { 'data-key': j },
      i ? ' · ' : '', h('a', { href: ctx.href(`job/${j}`) }, f.jobName(j)), h('span', { class: 'dimmer' }, ` ×${k}`)))) : null,
    bl.length ? h('div', { class: 'nbl' }, bl.map((k) => h('span', { 'data-key': 'bl-' + k },
      h('span', { class: 't-temp' }, `avoided by ${f.jobName(k)}`),
      h('button', { class: 'btn small', 'data-action': 'unblacklist', 'data-job': k, 'data-node': n.name, title: `Let ${f.jobName(k)} use ${n.name} again` }, 'allow again')))) : null,
    off ? null : h('div', { class: 'nctl' },
      n.state === 'disabled'
        ? h('button', { class: 'btn small', 'data-action': 'enable', 'data-node': n.name }, 'Enable node')
        : h('button', { class: 'btn small', 'data-action': 'disable', 'data-node': n.name, title: 'Taint the node farm-wide: running units finish, no new units start' }, 'Disable node')));
}

function physical(ctx, bl) {
  const snap = ctx.snap;
  const groups = sitesOf(snap);
  if ((snap.nodes || []).length >= SCALE) {
    const sel = st.node && snap.nodes.find((n) => n.name === st.node);
    return [
      own('lattice-nodes', 'lattice-nodes', { snap, sel: st.node }),
      sel ? h('div', { class: 'nsel', 'data-key': 'nsel' }, h('div', { class: 'lab' }, 'Selected node'), nodeCard(ctx, sel, bl.get(sel.name) || [])) : h('p', { class: 'dim small', 'data-key': 'nsel' }, 'Hover a node for its card; click it for its controls.'),
    ];
  }
  return groups.map((g) => h('div', { class: 'psite', 'data-key': 'site-' + (g.site ? g.site.name : 'farm') },
    h('div', { class: 'psite-h' },
      h('span', { class: 'sn' }, g.site ? g.site.label || g.site.name : 'The farm'),
      g.site && g.site.kind ? h('span', { class: 'sk' }, g.site.kind === 'onprem' ? 'machine room' : 'cloud region') : null,
      g.site && g.site.region ? h('span', { class: 'sk' }, g.site.region) : null,
      g.site && f.siteClock(f.now(), g.site.tz) ? h('span', { class: 'sk mono', title: `local time at ${g.site.label || g.site.name}` }, f.siteClock(f.now(), g.site.tz)) : null,
      h('span', { class: 'sk r' }, g.nodes.length ? `${f.count(g.nodes.length, 'node')} · ${f.num(g.nodes.reduce((a, n) => a + n.used, 0))} of ${f.num(g.nodes.reduce((a, n) => a + n.slots, 0))} slots` : 'no nodes: the project stores')),
    g.locs.map((l) => h('div', { class: 'prack', 'data-key': 'loc-' + l.name },
      h('div', { class: 'prack-h' }, h('span', { class: 'lab' }, l.name), h('span', { class: 'dimmer mono' }, [...new Set(l.nodes.map((n) => n.pool))].join(' · '))),
      h('div', { class: 'ncards' }, l.nodes.slice().sort((a, b) => nodeRank(a) - nodeRank(b)).map((n) => nodeCard(ctx, n, bl.get(n.name) || [])))))));
}

export default {
  render(ctx) {
    installTips();
    const snap = ctx.snap;
    const fm = snap.farm;
    const bl = new Map();
    for (const j of snap.jobs) for (const n of j.blacklist || []) { if (!bl.has(n)) bl.set(n, []); bl.get(n).push(keyOf(j)); }
    const models = snap.pools.map((p) => poolCells(snap, p));
    const borrowed = models.reduce((a, M) => a + M.runs.reduce((n, r) => n + Math.max(0, r.used - r.nominal), 0), 0);
    const pending = (snap.capacity || []).filter((c) => c.state === 'PendingApproval');
    const burstUp = snap.pools.reduce((a, p) => a + (p.burst ? p.burst.provisioned : 0), 0);
    const burstMax = snap.pools.reduce((a, p) => a + (p.burst ? p.burst.maxSlots : 0), 0);
    const suspect = snap.nodes.filter((n) => n.suspect);
    const notReady = snap.nodes.filter((n) => n.state === 'notReady');
    const sites = (snap.sites || []).filter((s) => snap.nodes.some((n) => n.region === s.region));
    const hits = snap.nodes.filter((n) => n.state !== 'unprovisioned');
    const warm = hits.length ? hits.reduce((a, n) => a + n.cacheHitRate, 0) / hits.length : 0;
    const v = snap.viewer;
    const width = 600;
    return h('section', { class: 'sheet pools', 'data-key': 'pools', 'aria-label': 'Pools: what the slots are and where they are' },
      h('div', { class: 'sheethead' },
        h('div', null,
          h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ' / pools'),
          h('h1', null, 'Pools'),
          h('div', { class: 'meta' }, `${f.count(fm.slotsTotal, 'slot')} in ${f.count(snap.pools.length, 'pool')} · ${f.count(snap.nodes.length, 'node')}${sites.length ? ` at ${sites.length} site${sites.length > 1 ? 's' : ''}` : ''} · ${v.allProjects ? 'all projects' : `your projects: ${v.projects.join(', ')}; others as totals`}`)),
        h('div', { class: 'titleblock' },
          tcell('Slots in use', h('span', null, f.num(fm.slotsUsed), h('small', null, ` of ${f.num(fm.slotsTotal)}`)), fm.slotsUsed > fm.slotsTotal ? `${f.num(fm.slotsUsed - fm.slotsTotal)} over the cap, stopping` : `${f.num(fm.slotsTotal - fm.slotsUsed)} idle`),
          tcell('Borrowed', f.num(borrowed), 'past guaranteed shares'),
          burstMax ? tcell('Burst', h('span', null, f.num(burstUp), h('small', null, ` of ${f.num(burstMax)}`)), `${f.money(fm.burstCostPerHour, true)} per hour`) : null,
          pending.length ? tcell('Requested', h('span', { class: 'pencil' }, `+${f.num(pending.reduce((a, c) => a + c.slots, 0))}`), `est. ${f.money(pending.reduce((a, c) => a + c.estCost, 0))} · waits for approval`) : null,
          tcell('Nodes to look at', h('span', { class: suspect.length ? 't-temp' : notReady.length ? 't-fail' : '' }, f.num(suspect.length + notReady.length)),
            [suspect.length ? `${suspect.length} suspect` : null, notReady.length ? `${notReady.length} not ready` : null].filter(Boolean).join(' · ') || 'none'),
          tcell('Failures', h('span', { class: fm.failuresLastHour ? 't-fail' : '' }, f.num(fm.failuresLastHour)), 'in the last hour'),
          tcell('Cache warmth', f.pct(warm, 0), 'mean node hit rate'))),
      h('div', { class: 'neat' },
        h('div', { class: 'edge w' }, '◂ ', h('a', { href: ctx.href('queue') }, 'queue'), ': the work that fills these slots'),
        h('div', { class: 'edge e' }, h('a', { href: ctx.href('map') }, 'map'), ': where these nodes are ▸'),
        h('div', { class: 'edge n' }, '▴ ', h('a', { href: ctx.href('tiers') }, 'cache tiers'), ' over these nodes'),
        h('div', { class: 'edge s' }, '▾ ', h('a', { href: ctx.href('cost') }, 'cost'), ': what the slots and the burst cost'),
        h('div', { class: 'pools-grid' },
          h('div', { class: 'pools-reg' },
            h('div', { class: 'half-h' }, h('h2', null, 'What the slots are'), h('span', null, 'one cell per slot · sorted by project · brackets are guaranteed shares')),
            snap.pools.map((p) => poolBlock(ctx, p, width)),
            key()),
          h('div', { class: 'pools-phys' },
            h('div', { class: 'half-h' }, h('h2', null, 'Where they are'), h('span', null, snap.nodes.length >= SCALE ? 'one cell per node · filled by slots in use · exceptions at the end' : 'site · rack · node · slot')),
            physical(ctx, bl)))));
  },
};

function key() {
  const sw = (cls) => h('i', { class: 'sw ' + cls });
  return h('div', { class: 'lat-key', 'aria-label': 'Key' },
    h('span', null, sw('mine'), 'running your units'),
    h('span', null, sw('other'), 'another project'),
    h('span', null, sw('idle'), 'idle'),
    h('span', null, sw('capped'), 'capped: a node’s slot limit takes it away, the reason beside it'),
    h('span', null, sw('mine notch'), 'borrowed past guaranteed'),
    h('span', null, sw('pending'), 'requested, with its price'),
    h('span', null, sw('headroom'), 'burst headroom'),
    h('span', null, sw('suspect'), 'suspect node'),
    h('span', null, sw('notready'), 'not ready'));
}
