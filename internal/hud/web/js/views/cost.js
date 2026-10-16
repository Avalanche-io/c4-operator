// Cost and capacity: bytes avoided (the value metric), per pool capacity
// and burst, spend against budget, the required-slots curve for the next 48
// hours against capacity, and a what-if for burst.

import { h, s } from '../dom.js';
import * as f from '../fmt.js';
import { key } from './common.js';

const st = { pool: null, add: null };

document.addEventListener('input', (e) => {
  const el = e.target.closest('[data-whatif]');
  if (!el) return;
  if (el.dataset.whatif === 'pool') { st.pool = el.value; st.add = null; }
  if (el.dataset.whatif === 'add') st.add = Number(el.value);
  document.dispatchEvent(new CustomEvent('hud:refresh'));
});

const HOURS = 48;

// demand spreads each unfinished job's required slots over its pools, in
// proportion to pool size, from when it can start until its deadline.
function demand(snap) {
  const now = f.now();
  const pools = new Map(snap.pools.map((p) => [p.name, new Float64Array(HOURS)]));
  const size = new Map(snap.pools.map((p) => [p.name, Math.max(1, p.slots)]));
  const byKey = new Map(snap.jobs.map((j) => [key(j), j]));
  for (const j of snap.jobs) {
    if (['Succeeded', 'Failed', 'Held'].includes(j.phase) || !j.objective.deadline) continue;
    let start = now;
    for (const d of j.dependsOn || []) {
      const u = byKey.get(d);
      if (u && u.phase !== 'Succeeded') start = Math.max(start, f.ms(u.estimate.etaP50));
    }
    // Needed from when the job can start until its deadline. A job whose
    // inputs arrive after its deadline needs what it can get until it ends.
    const dl = f.ms(j.objective.deadline);
    const late = start >= dl;
    const end = late ? f.ms(j.estimate.etaP90) : dl;
    const R = late ? j.estimate.attainableSlots : Math.max(j.estimate.requiredSlots, 0);
    const tot = j.pools.reduce((a, p) => a + (size.get(p) || 0), 0) || 1;
    for (const p of j.pools) {
      const arr = pools.get(p);
      if (!arr) continue;
      const share = (size.get(p) || 0) / tot;
      for (let hr = 0; hr < HOURS; hr++) {
        const t0 = now + hr * 3600e3;
        const t1 = t0 + 3600e3;
        const ov = Math.max(0, Math.min(t1, end) - Math.max(t0, start)) / 3600e3;
        arr[hr] += R * share * ov;
      }
    }
  }
  return pools;
}

function curve(p, need, extra) {
  const cap = p.slots + extra;
  const max = Math.max(cap, ...need) * 1.15 || 1;
  const W = 100;
  const x = (hr) => (hr / HOURS) * W;
  const y = (v) => 60 - (v / max) * 56;
  let d = `M0 ${y(0)}`;
  const over = [];
  need.forEach((v, hr) => {
    d += ` L${x(hr)} ${y(v)} L${x(hr + 1)} ${y(v)}`;
    if (v > cap) over.push(s('rect', { x: x(hr), y: y(v), width: x(1), height: y(cap) - y(v), fill: 'var(--bar-yellow)' }));
  });
  d += ` L${W} ${y(0)} Z`;
  const peak = Math.max(...need);
  const ticks = [0, 12, 24, 36, 48].map((hr) => h('span', { style: { position: 'absolute', left: `${(hr / HOURS) * 100}%`, transform: hr === 48 ? 'translateX(-100%)' : hr ? 'translateX(-50%)' : null } },
    hr === 0 ? 'now' : `+${hr}h`));
  return h('div', { class: 'panel', 'data-key': 'curve-' + p.name },
    h('header', null, h('h2', null, p.name),
      h('span', { class: 'num dim' }, `peak need ${peak.toFixed(1)} slots · capacity ${cap}${extra ? ` (+${extra} what-if)` : ''}`)),
    h('div', { class: 'well', style: { padding: '8px 10px 4px' } },
      s('svg', { class: 'chart', viewBox: '0 0 100 62', preserveAspectRatio: 'none', style: { height: '92px' }, role: 'img', 'aria-label': `${p.name}: peak need ${peak.toFixed(1)} slots against ${cap}` },
        s('path', { d, fill: '#555555', stroke: 'var(--ink-2)', 'stroke-width': 1, 'vector-effect': 'non-scaling-stroke' }),
        over,
        s('line', { x1: 0, x2: W, y1: y(p.slots), y2: y(p.slots), stroke: 'var(--ink)', 'stroke-width': 2, 'vector-effect': 'non-scaling-stroke' }),
        extra ? s('line', { x1: 0, x2: W, y1: y(cap), y2: y(cap), stroke: 'var(--bar-blue)', 'stroke-width': 2, 'stroke-dasharray': '4 3', 'vector-effect': 'non-scaling-stroke' }) : null),
      h('div', { class: 'num dimmer', style: { position: 'relative', height: '14px', fontSize: '10px' } }, ticks)));
}

export default {
  render(ctx) {
    const snap = ctx.snap;
    const fm = snap.farm;
    const burstPools = snap.pools.filter((p) => p.burst);
    if (!st.pool && burstPools.length) st.pool = burstPools[0].name;
    const wp = snap.pools.find((p) => p.name === st.pool);
    const infeasible = snap.jobs.filter((j) => !j.estimate.feasible && j.phase !== 'Succeeded' && j.phase !== 'Failed' && j.pools.includes(st.pool));
    const offer = infeasible.map((j) => j.estimate.burst).find((b) => b && b.pool === st.pool);
    const maxAdd = wp && wp.burst ? wp.burst.maxSlots - wp.burst.requested : 0;
    if (st.add === null) st.add = offer ? Math.min(offer.slots, maxAdd) : 0;
    const add = st.add;
    const fixes = infeasible.map((j) => {
      const b = j.estimate.burst;
      const need = b && b.pool === st.pool ? b.slots : Math.ceil(Math.max(0, j.estimate.requiredSlots - j.estimate.attainableSlots));
      const hours = Math.max(0.25, (f.ms(j.objective.deadline || j.estimate.etaP90) - f.now()) / 3600e3);
      const cost = b && b.pool === st.pool ? (b.cost * add) / b.slots : add * (wp ? (wp.burst ? wp.burst.costPerSlotHour : wp.costPerSlotHour) : 0) * hours;
      return { j, need, ok: add >= need && need > 0, cost };
    });
    const fixed = fixes.filter((x) => x.ok);
    const whatCost = fixed.length ? Math.max(...fixed.map((x) => x.cost)) : 0;
    const need = demand(snap);
    const projects = snap.projects;
    return h('section', null,
      h('div', { class: 'scopebar' },
        h('div', null, h('h1', null, 'Cost & capacity'),
          h('div', { class: 'dim', style: { marginTop: '4px' } }, 'What the farm has, what the deadlines need, and what a burst would buy.'))),
      h('div', { class: 'section' },
        h('header', null, h('h2', null, 'Bytes avoided today'),
          h('span', { class: 'dim' }, 'Bytes mounted into pods minus bytes fetched from the store: what content addressing, the cache tiers and the overlays saved.')),
        h('div', { class: 'figs', style: { alignItems: 'flex-end' } },
          h('div', { class: 'fig' }, h('div', { class: 'lab' }, snap.viewer.allProjects ? 'Farm' : 'Farm, all projects'), h('div', { class: 'big' }, f.bytes(fm.bytesAvoided)),
            h('div', { class: 'dim num', style: { fontSize: '11px', marginTop: '4px' } }, `${f.pct(fm.bytesAvoided / fm.bytesMaterialized)} of ${f.bytes(fm.bytesMaterialized)} mounted`)),
          projects.map((p) => h('div', { class: 'fig', 'data-key': p.name }, h('div', { class: 'lab' }, p.name), h('div', { class: 'val', style: { fontSize: '26px' } }, f.bytes(p.bytesAvoided)),
            h('div', { class: 'dim num', style: { fontSize: '11px', marginTop: '4px' } }, `${f.num(p.workerHours.reduce((a, w) => a + w.hours, 0))} worker-hours · ${p.gpuHours.toFixed(1)} GPU-hours`))))),
      h('div', { class: 'cols2' },
        h('div', { class: 'section' },
          h('header', null, h('h2', null, 'Pools')),
          h('table', { class: 't' },
            h('thead', null, h('tr', null, h('th', null, 'Pool'), h('th', { style: { width: '42%' } }, 'Slots in use'), h('th', { class: 'r' }, 'Per slot-hour'), h('th', { class: 'r' }, 'Burst'))),
            h('tbody', null, snap.pools.map((p) => {
              const b = p.burst;
              const tot = Math.max(p.slots + (b ? Math.max(0, b.requested - b.provisioned) : 0), 1);
              return h('tr', { 'data-key': p.name },
                h('td', null, h('span', { class: 'mono' }, p.name), h('div', { class: 'dimmer', style: { fontSize: '11px' } }, [p.attrs.class, p.attrs.gpuModel, p.attrs.capacityType].filter(Boolean).join(' · '))),
                h('td', null,
                  h('div', { style: { display: 'flex', height: '10px', background: 'var(--u-queued)' }, 'aria-hidden': 'true' },
                    h('i', { style: { width: `${(p.used / tot) * 100}%`, background: 'var(--u-done)' } }),
                    b && b.requested > b.provisioned ? h('i', { style: { marginLeft: 'auto', width: `${((b.requested - b.provisioned) / tot) * 100}%`, background: 'repeating-linear-gradient(135deg, var(--bar-blue) 0 3px, transparent 3px 6px)' } }) : null),
                  h('div', { class: 'num dim', style: { fontSize: '11px', marginTop: '3px' } }, `${p.used} of ${p.slots}`)),
                h('td', { class: 'r num' }, f.money(p.costPerSlotHour, true)),
                h('td', { class: 'r num' }, b ? h('span', { class: 't-blue' }, `${b.provisioned}/${b.maxSlots}`) : h('span', { class: 'dimmer' }, '–')));
            })))),
        h('div', { class: 'section' },
          h('header', null, h('h2', null, 'Burst spend today')),
          projects.map((p) => {
            const frac = p.burstBudgetPerDay ? p.burstSpendToday / p.burstBudgetPerDay : 0;
            return h('div', { 'data-key': p.name, style: { marginBottom: '12px' } },
              h('div', { class: 'nrow' }, h('span', { class: 'mono' }, p.name), h('span', { class: 'num' }, `${f.money(p.burstSpendToday)} of ${f.money(p.burstBudgetPerDay)}`)),
              h('div', { style: { height: '8px', background: 'var(--u-queued)', marginTop: '4px' } },
                h('i', { style: { display: 'block', height: '100%', width: `${Math.min(100, frac * 100)}%`, background: frac > 0.9 ? 'var(--bar-yellow)' : 'var(--bar-blue)' } })),
              h('div', { class: 'dimmer', style: { fontSize: '11px', marginTop: '3px' } }, `Bursts under ${f.money(Math.max(0, p.burstBudgetPerDay - p.burstSpendToday))} are requested without asking; larger ones wait for approval.`));
          }),
          !snap.viewer.allProjects ? h('p', { class: 'dim', style: { fontSize: '12px' } }, `Other projects: ${fm.otherProjectsSlots} slots in use. Their costs are not shown.`) : null)),
      h('div', { class: 'section' },
        h('header', null, h('h2', null, 'Capacity requests'), h('span', { class: 'dim' }, 'The scheduler asks for capacity when a deadline is at risk; nodes pre-warm the listed views before units land.')),
        snap.capacity.length ? h('table', { class: 't' },
          h('thead', null, h('tr', null, ['Pool', 'Slots', 'Why', 'Until', 'Est. cost', 'Pre-warm', 'State', ''].map((x, i) => h('th', { class: i === 1 || i === 4 ? 'r' : '' }, x)))),
          h('tbody', null, snap.capacity.map((c, i) => h('tr', { 'data-key': `${c.pool}-${i}` },
            h('td', { class: 'mono' }, c.pool), h('td', { class: 'r num' }, `+${c.slots}`),
            h('td', { style: { fontSize: '12px' } }, c.reason), h('td', { class: 'num' }, f.clock(c.until)),
            h('td', { class: 'r num' }, f.money(c.estCost)), h('td', { class: 'num dim' }, `${c.prewarmViews.length} views`),
            h('td', null, h('span', { class: 'chip ' + (c.state === 'PendingApproval' ? 'flag' : 'blue') }, c.state === 'PendingApproval' ? 'awaiting approval' : c.state.toLowerCase())),
            h('td', { class: 'r' }, c.state === 'PendingApproval' ? h('button', { class: 'btn primary small', 'data-action': 'approve', 'data-pool': c.pool }, `Approve burst, ${f.money(c.estCost)}`) : null))))) :
          h('p', { class: 'dim' }, 'No capacity requests.')),
      h('div', { class: 'section' },
        h('header', null, h('h2', null, 'Slots needed, next 48 hours'),
          h('span', { class: 'dim' }, 'Gray: slots the deadlines need. White line: capacity. Yellow: need above capacity.')),
        h('div', { class: 'cols' }, snap.pools.map((p) => curve(p, Array.from(need.get(p.name)), p.name === st.pool ? add : 0)))),
      burstPools.length ? h('div', { class: 'section' },
        h('header', null, h('h2', null, 'What if')),
        h('div', { class: 'controls', style: { justifyContent: 'flex-start', gap: '12px' } },
          h('label', { class: 'lab', for: 'wi-pool' }, 'Add to pool'),
          h('select', { id: 'wi-pool', 'data-whatif': 'pool', class: 'btn' }, burstPools.map((p) => h('option', { value: p.name, selected: p.name === st.pool }, p.name))),
          h('input', { type: 'range', id: 'wi-add', 'data-whatif': 'add', min: 0, max: Math.max(0, maxAdd), step: wp ? Math.max(1, Math.round(wp.slots / Math.max(1, snap.nodes.filter((n) => n.pool === wp.name).length)) || 4) : 4, value: String(add), style: { width: '260px' }, 'aria-label': 'Slots to add' }),
          h('span', { class: 'num', style: { fontSize: '18px' } }, `+${add} slots`)),
        h('p', { style: { fontSize: '15px', margin: '10px 0 4px' } },
          fixed.length ? [`+${add} ${st.pool} slots makes `, fixed.map((x, i) => [i ? ', ' : '', h('a', { href: ctx.href(`job/${key(x.j)}`) }, x.j.name)]), ` feasible for about ${f.money(whatCost)}.`] :
            infeasible.length ? `${infeasible.length} job${infeasible.length > 1 ? 's' : ''} using ${st.pool} will miss; +${add} slots is not enough for any of them.` : `Every job that can use ${st.pool} is on track.`),
        fixes.length ? h('table', { class: 't', style: { maxWidth: '720px' } },
          h('tbody', null, fixes.map((x) => h('tr', { 'data-key': key(x.j) },
            h('td', null, x.j.name), h('td', { class: 'num' }, `needs +${x.need}`),
            h('td', { class: 'num' }, x.ok ? h('span', { class: 't-green' }, 'feasible') : h('span', { class: 't-yellow' }, `misses by ${f.dur(-x.j.estimate.slackSeconds)}`)))))) : null) : null);
  },
};
