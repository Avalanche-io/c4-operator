// LATTICE: pools as identical cells. Not where slots are but what they
// are: one cell per slot, sorted by project, so every pool reads the same
// way at a glance: how full, whose, how much was borrowed, how much burst is
// up, how much is asked for and how much more the provider can give.
//
//   a slot running your units    blue body, a blue leading edge (running now)
//   a slot of another project    grey ink: shown only as a total
//   an idle slot                 a grey outline
//   borrowed past nominal quota  the cell's corner notched
//   requested burst              a dashed outline; the price and the approve control sit under the pool
//   expected (provisioning)      a dashed outline with a dot: nodes on their way
//   burst headroom               dotted: what the provider can still give
//   capped                       grey hatched: slots a node's power cap (its
//                                slot limit) holds back, with the reason as
//                                text under the pool
//
// Under each project's run a bracket shows its nominal quota, with a tick
// where the nominal share ends and borrowing begins.
//
// The physical view is the same farm by site, rack and node. Up to a few
// hundred nodes each node is a card (views draw those); past that a node is
// one cell on a canvas, filled from the bottom by its slots in use, and the
// exceptions (suspect, failing, not ready, disabled) sort to the end of
// each pool, where the eye lands last and finds them together.

import { tok, esc } from './core.js';
import * as f from './fmt.js';
import { register } from './live.js';

// ---------------------------------------------------------------- model

// poolCells lists one entry per slot (or per `per` slots in a large pool)
// in display order: each project in scope by name, other projects, idle,
// capped (lost to a node's slot limit), then burst: expected (provisioning),
// requested, headroom.
export function poolCells(snap, pool, o = {}) {
  const max = o.maxCells || 640;
  const runs = [];
  let mine = 0;
  for (const p of snap.projects || []) {
    const q = (p.quotas || []).find((x) => x.pool === pool.name);
    if (!q || !q.used) continue;
    runs.push({ project: p.name, used: q.used, nominal: q.nominal, borrowingLimit: q.borrowingLimit });
    mine += q.used;
  }
  const other = Math.max(0, pool.used - mine);
  const idle = Math.max(0, pool.slots - pool.used);
  const caps = (snap.capacity || []).filter((c) => c.pool === pool.name);
  const pending = caps.filter((c) => c.state === 'PendingApproval');
  const provisioning = caps.filter((c) => c.state === 'Provisioning');
  const nPend = pending.reduce((a, c) => a + c.slots, 0);
  const nProv = provisioning.reduce((a, c) => a + c.slots, 0);
  const b = pool.burst;
  const headroom = b ? Math.max(0, b.maxSlots - b.provisioned - nPend - nProv) : 0;
  // A capped node offers fewer slots than its pool's shape; the difference
  // is drawn after idle, hatched, so the pool reads at its full shape. Right
  // after a cap drops, units can still run past it until they stop at the
  // end of their turns: those stay running cells, and `over` counts them.
  const capNodes = (snap.nodes || []).filter((nd) => nd.pool === pool.name && nd.capped);
  const capped = capNodes.reduce((a, nd) => a + Math.max(0, (nd.shapeSlots || nd.slots) - Math.max(nd.slots, nd.used || 0)), 0);
  const capHeld = capNodes.reduce((a, nd) => a + Math.max(0, (nd.shapeSlots || nd.slots) - nd.slots), 0);
  const over = capNodes.reduce((a, nd) => a + Math.max(0, (nd.used || 0) - nd.slots), 0);
  const capReasons = [...new Set(capNodes.map((nd) => nd.capacityReason).filter(Boolean))];
  const total = Math.max(pool.slots, pool.used) + capped + nPend + nProv + headroom;
  const per = total > max ? Math.ceil(total / max) : 1;
  const n = (x) => (per === 1 ? x : Math.ceil(x / per));
  const cells = [];
  for (const r of runs) {
    r.a = cells.length;
    const cn = n(r.used), nom = per === 1 ? r.nominal : Math.round(r.nominal / per);
    for (let k = 0; k < cn; k++) cells.push({ kind: 'mine', project: r.project, borrowed: k >= nom, run: r });
    r.z = cells.length - 1;
    r.nomCells = Math.min(nom, cn);
  }
  for (let k = 0; k < n(other); k++) cells.push({ kind: 'other' });
  for (let k = 0; k < n(idle); k++) cells.push({ kind: 'idle' });
  for (let k = 0; k < n(capped); k++) cells.push({ kind: 'capped' });
  for (const c of provisioning) for (let k = 0; k < n(c.slots); k++) cells.push({ kind: 'prov', req: c });
  for (const c of pending) for (let k = 0; k < n(c.slots); k++) cells.push({ kind: 'pending', req: c });
  for (let k = 0; k < n(headroom); k++) cells.push({ kind: 'headroom' });
  return { cells, runs, per, other, idle, capped, capHeld, over, capNodes, capReasons, pending, provisioning, headroom, nPend, nProv, burst: b };
}

// ---------------------------------------------------------------- regularized view

// latticeSVG draws one pool. o: { width, cell, gap }
export function latticeSVG(snap, pool, o = {}) {
  const T = tok();
  o = Object.assign({ width: 600, cell: 15, gap: 3 }, o);
  const M = poolCells(snap, pool, o);
  const c = M.per > 1 ? Math.max(7, Math.round(o.cell * 0.6)) : o.cell;
  const gp = M.per > 1 ? 2 : o.gap;
  const step = c + gp;
  const perRow = Math.max(4, Math.floor((o.width + gp) / step));
  const nRows = Math.max(1, Math.ceil(M.cells.length / perRow));
  // labels under a project's bracket when its opening row segment is wide enough
  const lw = (s) => s.length * 5.4 + 6;
  const labelRow = new Map();
  const label = (r) => `${r.project} ${f.num(r.used)} of ${f.num(r.nominal)} guaranteed${r.used > r.nominal ? `, ${f.num(r.used - r.nominal)} borrowed` : ''}`;
  const legend = [];
  let lastEnd = new Map();
  for (const r of M.runs) {
    const row = Math.floor(r.a / perRow);
    const segEnd = Math.min(r.z, (row + 1) * perRow - 1);
    const x0 = (r.a % perRow) * step;
    const room = Math.max((segEnd - r.a + 1) * step, o.width - x0);
    const prev = lastEnd.get(row) || -1;
    if (lw(label(r)) <= room && x0 > prev) { labelRow.set(r.project, row); lastEnd.set(row, x0 + lw(label(r))); }
    else legend.push(r);
  }
  const rowsWithLabel = new Set(labelRow.values());
  const rowY = [];
  let y = 0;
  for (let k = 0; k < nRows; k++) { rowY.push(y); y += step + 6 + (rowsWithLabel.has(k) ? 12 : 0); }
  // A capped pool says why under its cells, in plain text: one line per
  // reason (or one line when no reason was given).
  const capLines = M.capHeld ? (M.capReasons.length ? M.capReasons : ['']).map((r) =>
    `${f.num(M.capHeld)} ${M.capHeld === 1 ? 'slot' : 'slots'} capped${M.capNodes.length > 1 ? ` on ${f.num(M.capNodes.length)} nodes` : ''}${r ? `: ${r}` : ''}`) : [];
  if (M.over) capLines.push(`${f.num(M.over)} still running past the cap, each stopping at the end of its turn`);
  const H = y + 14 * (legend.length + capLines.length);
  const pos = (i) => [(i % perRow) * step, rowY[Math.floor(i / perRow)]];
  const tipOf = (cl) => {
    const unit = M.per > 1 ? `${M.per} slots` : 'slot';
    switch (cl.kind) {
      case 'mine': return `${pool.name} · ${cl.project} | running ${M.per > 1 ? `${M.per} slots of ` : ''}${cl.project}’s units | ${cl.borrowed ? `borrowed past its guaranteed ${cl.run.nominal}; the lender takes it back by preemption when it needs it` : `within its guaranteed share of ${cl.run.nominal}`}`;
      case 'other': return `${pool.name} · another project | ${unit} in use | other projects are shown only as a total`;
      case 'idle': return `${pool.name} · idle ${unit} | ${f.money(pool.costPerSlotHour, true)} per slot-hour`;
      case 'prov': return `${pool.name} · expected | +${cl.req.slots} slots on their way | ${cl.req.reason || ''}`;
      case 'capped': return `${pool.name} · capped ${unit} | a node’s slot limit takes it away${M.capReasons.length ? ` | ${M.capReasons.join('; ')}` : ''} | the node offers it again when the limit rises`;
      case 'pending': return `${pool.name} · requested, waits for approval | +${cl.req.slots} slots · est. ${f.money(cl.req.estCost)} | ${cl.req.reason || ''}`;
      default: return `${pool.name} · burst headroom | the provider can give ${f.num(M.headroom)} more slots | about ${f.dur(M.burst ? M.burst.provisioningSeconds : 0)} to provision at ${f.money(M.burst ? M.burst.costPerSlotHour : pool.costPerSlotHour, true)} per slot-hour`;
    }
  };
  let s = '';
  M.cells.forEach((cl, i) => {
    const [x, yy] = pos(i);
    let g;
    if (cl.kind === 'mine') {
      g = `<rect x="${x}" y="${yy}" width="${c}" height="${c}" fill="${T.run}"/>`;
      if (cl.borrowed) { const nn = Math.max(3, Math.round(c * 0.38)); g += `<path d="M${x + c - nn} ${yy} h${nn} v${nn} z" fill="${T.field}"/>`; }
    } else if (cl.kind === 'other') g = `<rect x="${x}" y="${yy}" width="${c}" height="${c}" fill="${T.g[13]}"/>`;
    else if (cl.kind === 'idle') g = `<rect x="${x + 0.5}" y="${yy + 0.5}" width="${c - 1}" height="${c - 1}" fill="${T.g[4]}" stroke="${T.g[12]}"/>`;
    else if (cl.kind === 'prov') g = `<rect x="${x + 0.5}" y="${yy + 0.5}" width="${c - 1}" height="${c - 1}" fill="none" stroke="${T.g[14]}" stroke-dasharray="3 2"/><rect x="${x + c / 2 - 1}" y="${yy + c / 2 - 1}" width="2" height="2" fill="${T.g[15]}"/>`;
    else if (cl.kind === 'capped') {
      g = `<rect x="${x}" y="${yy}" width="${c}" height="${c}" fill="${T.g[9]}"/><path d="`;
      for (let k = -c; k < c; k += 5) g += `M${x + Math.max(0, k)} ${yy + Math.max(0, -k)} L${x + Math.min(c, k + c)} ${yy + Math.min(c, c - k)} `;
      g += `" stroke="${T.g[12]}" fill="none"/>`;
    } else if (cl.kind === 'pending') g = `<rect x="${x + 0.5}" y="${yy + 0.5}" width="${c - 1}" height="${c - 1}" fill="none" stroke="${T.g[15]}" stroke-dasharray="3 2"/>`;
    else g = `<rect x="${x + 2.5}" y="${yy + 2.5}" width="${c - 5}" height="${c - 5}" fill="none" stroke="${T.g[13]}" stroke-dasharray="1 2"/>`;
    s += `<g data-tip="${esc(tipOf(cl))}">${g}</g>`;
  });
  // brackets: one per row segment of each run; caps at the run's ends; a tick where nominal ends
  for (const r of M.runs) {
    for (let a = r.a; a <= r.z;) {
      const row = Math.floor(a / perRow), z = Math.min(r.z, (row + 1) * perRow - 1);
      const [xa, ya] = pos(a), [xz] = pos(z);
      const by = ya + c + 3.5;
      s += `<path d="M${xa + 0.5} ${by - (a === r.a ? 3 : 0)} V${by} H${xz + c - 0.5} V${by - (z === r.z ? 3 : 0)}" fill="none" stroke="${T.g[15]}"/>`;
      const nomEnd = r.a + r.nomCells - 1;
      if (r.nomCells > 0 && nomEnd < r.z && nomEnd >= a && nomEnd <= z) {
        const xn = pos(nomEnd)[0] + c + gp / 2;
        s += `<line x1="${xn}" x2="${xn}" y1="${by - 6}" y2="${by + 3}" stroke="${T.ink}" stroke-width="1.5"/>`;
      }
      if (r.nomCells === 0 && a === r.a) s += `<line x1="${xa - gp / 2}" x2="${xa - gp / 2}" y1="${by - 6}" y2="${by + 3}" stroke="${T.ink}" stroke-width="1.5"/>`;
      if (labelRow.get(r.project) === row && a === r.a) s += `<text x="${xa}" y="${by + 11}" fill="${T.ink3}" font-size="9.5" font-family='${T.mono}'>${esc(label(r))}</text>`;
      a = z + 1;
    }
  }
  legend.forEach((r, k) => { s += `<text x="0" y="${y + 10 + k * 14}" fill="${T.ink3}" font-size="9.5" font-family='${T.mono}'><tspan fill="${T.g[15]}">⌐ </tspan>${esc(label(r))}</text>`; });
  capLines.forEach((t, k) => { s += `<text x="0" y="${y + 10 + (legend.length + k) * 14}" fill="${T.ink2}" font-size="9.5" font-family='${T.mono}'>${esc(t)}</text>`; });
  const W = Math.min(o.width, perRow * step);
  const aria = `${pool.name}: ${pool.used} of ${pool.slots} slots in use${capLines.length ? `, ${capLines.join('; ')}` : ''}${M.nPend ? `, ${M.nPend} requested` : ''}${M.headroom ? `, ${M.headroom} burst headroom` : ''}`;
  return { svg: `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(aria)}">${s}</svg>`, h: H, w: W, model: M };
}

// ---------------------------------------------------------------- nodes

// nodeSlots splits a node's slots: the viewer's running units (with what
// they run), other projects, idle, and capped (its pool's shape less what a
// slot limit lets it offer now).
export function nodeSlots(n) {
  const mine = [];
  for (const r of n.running || []) for (let k = 0; k < (r.slots || 1); k++) mine.push(r);
  const other = Math.max(0, (n.used || 0) - mine.length);
  // right after a cap drops, units still running past it count as `over`
  const capped = n.capped ? Math.max(0, (n.shapeSlots || n.slots) - Math.max(n.slots, n.used || 0)) : 0;
  const over = n.capped ? Math.max(0, (n.used || 0) - n.slots) : 0;
  return { mine, other, idle: Math.max(0, n.slots - mine.length - other), capped, over };
}

// exception rank: 0 is an ordinary node; higher sorts later
export function nodeRank(n) {
  if (n.state === 'notReady') return 5;
  if (n.suspect) return 4;
  if (n.failuresLastHour > 0) return 3;
  if (n.state === 'disabled') return 2;
  if (n.state === 'warming' || n.state === 'unprovisioned') return 1;
  return 0;
}

export function nodeTip(n) {
  const s = nodeSlots(n);
  const jobs = new Map();
  for (const r of s.mine) jobs.set(r.job, (jobs.get(r.job) || 0) + 1);
  const runs = [...jobs].slice(0, 4).map(([j, k]) => `${f.jobName(j)} ×${k}`);
  return [
    `${n.name} · ${n.state}${n.suspect ? ' · suspect' : ''}`,
    `${n.used} of ${n.slots} slots${s.other ? ` · ${s.other} other projects` : ''}`,
    ...(n.capped ? [`capped at ${n.slots} of ${n.shapeSlots}${n.capacityReason ? ` · ${n.capacityReason}` : ''}`] : []),
    ...(s.over ? [`${s.over} still running past the cap, stopping at the end of their turns`] : []),
    `${n.failuresLastHour} failed in the last hour`,
    `cache ${f.bytes(n.cacheBytes)} · ${f.pct(n.cacheHitRate)} hit`,
    `${n.zone || n.locality || ''}${n.region ? ' · ' + n.region : ''}`,
    ...runs,
  ].join(' | ');
}

// LatticeNodes: the physical view at scale. One cell per node on a
// canvas, a block per pool, ordinary nodes leading, by fill, exceptions last.
//   props: { snap, width, onSelect? }
export class LatticeNodes {
  constructor(el) {
    this.el = el;
    el.classList.add('lattice-nodes');
    this.cv = document.createElement('canvas');
    this.cv.tabIndex = 0;
    this.cv.setAttribute('role', 'img');
    el.append(this.cv);
    this.hits = [];
    this.cv.addEventListener('mousemove', (e) => this.onMove(e));
    this.cv.addEventListener('mouseleave', () => tipHide());
    this.cv.addEventListener('click', (e) => {
      const n = this.at(e);
      if (n) this.el.dispatchEvent(new CustomEvent('lattice:node', { bubbles: true, detail: { node: n.name } }));
    });
    this.ro = new ResizeObserver(() => this.paint());
    this.ro.observe(el);
  }
  update(p) { this.p = p; this.paint(); }
  paint() {
    if (!this.p) return;
    const T = tok();
    const snap = this.p.snap;
    const W = Math.max(320, this.el.clientWidth || this.p.width || 1200);
    const per = new Map();
    for (const n of snap.nodes) { if (!per.has(n.pool)) per.set(n.pool, []); per.get(n.pool).push(n); }
    const pools = (snap.pools || []).filter((p) => per.has(p.name));
    const total = snap.nodes.length;
    const cell = total > 8000 ? 6 : total > 3000 ? 8 : 10, gp = total > 3000 ? 1 : 2, step = cell + gp;
    const perRow = Math.floor((W + gp) / step);
    let y = 0;
    const blocks = [];
    for (const p of pools) {
      const ns = per.get(p.name).slice().sort((a, b) => nodeRank(a) - nodeRank(b) || (b.used / Math.max(1, b.slots)) - (a.used / Math.max(1, a.slots)) || b.failuresLastHour - a.failuresLastHour);
      const rows = Math.ceil(ns.length / perRow);
      blocks.push({ p, ns, y: y + 22, rows });
      y += 22 + rows * step + 18;
    }
    const H = Math.max(40, y);
    const dpr = devicePixelRatio || 1;
    this.cv.width = Math.round(W * dpr); this.cv.height = Math.round(H * dpr);
    this.cv.style.width = W + 'px'; this.cv.style.height = H + 'px';
    const g = this.cv.getContext('2d');
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, W, H);
    this.hits = [];
    let aria = [];
    for (const b of blocks) {
      const used = b.ns.reduce((a, n) => a + n.used, 0), slots = b.ns.reduce((a, n) => a + n.slots, 0);
      const exc = b.ns.filter((n) => nodeRank(n) >= 2).length;
      const caps = b.ns.filter((n) => n.capped);
      const capWhy = [...new Set(caps.map((n) => n.capacityReason).filter(Boolean))];
      g.font = `600 11px ${T.sans}`; g.fillStyle = T.ink; g.textBaseline = 'alphabetic';
      g.fillText(b.p.name.toUpperCase().split('').join(String.fromCharCode(8202)), 0, b.y - 8);
      const tw = g.measureText(b.p.name.toUpperCase()).width + 24;
      g.font = `400 10px ${T.mono}`; g.fillStyle = T.ink3;
      const sub = `${f.count(b.ns.length, 'node')} · ${f.num(used)} of ${f.num(slots)} slots · ${f.pct(used / Math.max(1, slots), 0)}${caps.length ? ` · ${f.num(caps.length)} capped${capWhy.length ? `: ${capWhy.join('; ')}` : ''}` : ''}${exc ? ` · ${f.num(exc)} need a look, at the end` : ''}`;
      g.fillText(sub, tw, b.y - 8);
      aria.push(`${b.p.name}: ${b.ns.length} nodes, ${used} of ${slots} slots`);
      b.ns.forEach((n, k) => {
        const x = (k % perRow) * step, yy = b.y + Math.floor(k / perRow) * step;
        const s = nodeSlots(n);
        const D = Math.max(1, n.slots + s.over + s.capped);
        const fm = s.mine.length / D, fo = s.other / D;
        g.fillStyle = T.g[5]; g.fillRect(x, yy, cell, cell);
        if (s.capped) {
          // the capped share, hatched grey across the top of the cell
          const hc = Math.max(2, cell * (s.capped / D));
          g.fillStyle = T.g[9]; g.fillRect(x, yy, cell, hc);
          g.save(); g.beginPath(); g.rect(x, yy, cell, hc); g.clip(); g.strokeStyle = T.g[12]; g.beginPath();
          for (let k = -hc; k < cell + hc; k += 4) { g.moveTo(x + k, yy); g.lineTo(x + k + hc, yy + hc); } g.stroke(); g.restore();
        }
        if (n.state === 'disabled') {
          g.strokeStyle = T.g[14]; g.beginPath(); g.moveTo(x + 1, yy + 1); g.lineTo(x + cell - 1, yy + cell - 1); g.moveTo(x + cell - 1, yy + 1); g.lineTo(x + 1, yy + cell - 1); g.stroke();
        } else {
          const hm = cell * fm, ho = cell * fo;
          if (ho) { g.fillStyle = T.g[13]; g.fillRect(x, yy + cell - ho, cell, ho); }
          if (hm) { g.fillStyle = T.run; g.fillRect(x, yy + cell - ho - hm, cell, hm); g.fillStyle = T.run; g.fillRect(x, yy + cell - ho - hm, cell, Math.min(1.5, hm)); }
        }
        if (n.state === 'warming' || n.state === 'unprovisioned') { g.strokeStyle = T.g[14]; g.setLineDash([2, 1]); g.strokeRect(x + 0.5, yy + 0.5, cell - 1, cell - 1); g.setLineDash([]); }
        if (n.suspect) { g.strokeStyle = T.temp; g.lineWidth = 1.5; g.strokeRect(x + 0.75, yy + 0.75, cell - 1.5, cell - 1.5); g.lineWidth = 1; }
        if (n.state === 'notReady') { g.strokeStyle = T.fail; g.lineWidth = 1.5; g.strokeRect(x + 0.75, yy + 0.75, cell - 1.5, cell - 1.5); g.lineWidth = 1; }
        if (n.failuresLastHour > 0) { g.fillStyle = T.fail; g.fillRect(x + cell - 3, yy, 3, 3); }
        if (this.p.sel === n.name) { g.strokeStyle = T.paper; g.lineWidth = 1.5; g.strokeRect(x - 1, yy - 1, cell + 2, cell + 2); g.lineWidth = 1; }
        this.hits.push({ x, y: yy, w: cell, h: cell, n });
      });
    }
    this.cv.setAttribute('aria-label', aria.join('; '));
    this.step = step;
  }
  at(e) {
    const r = this.cv.getBoundingClientRect();
    const x = e.clientX - r.left, y = e.clientY - r.top;
    for (const h of this.hits) if (x >= h.x - 1 && x < h.x + h.w + 1 && y >= h.y - 1 && y < h.y + h.h + 1) return h.n;
    return null;
  }
  onMove(e) {
    const n = this.at(e);
    if (!n) { tipHide(); return; }
    tipShow(e.clientX, e.clientY, nodeTip(n) + ' | click for the node card and its controls');
  }
}
register('lattice-nodes', LatticeNodes);

// ---------------------------------------------------------------- hover cards

// Hover cards for anything carrying data-tip="title | line | line". One
// card for the page, fixed to the viewport, installed once.
let tipEl = null;
function tipNode() {
  if (tipEl && tipEl.isConnected) return tipEl;
  tipEl = document.createElement('div');
  tipEl.className = 'tipcard';
  tipEl.setAttribute('role', 'tooltip');
  tipEl.style.position = 'fixed';
  tipEl.hidden = true;
  document.body.appendChild(tipEl);
  return tipEl;
}
export function tipHTML(txt) {
  const [h, ...rest] = String(txt).split(' | ');
  return `<b>${esc(h)}</b>${rest.map((r) => `<br>${esc(r)}`).join('')}`;
}
export function tipShow(cx, cy, txt) {
  const t = tipNode();
  t.innerHTML = tipHTML(txt);
  t.hidden = false;
  const w = t.offsetWidth || 250, hh = t.offsetHeight || 80;
  const x = cx + 16 + w > innerWidth ? cx - w - 12 : cx + 16;
  const y = cy + 16 + hh > innerHeight ? Math.max(4, cy - hh - 10) : cy + 16;
  t.style.left = x + 'px'; t.style.top = y + 'px';
}
export function tipHide() { if (tipEl) tipEl.hidden = true; }

let tipsOn = false;
export function installTips() {
  if (tipsOn) return;
  tipsOn = true;
  document.addEventListener('mousemove', (e) => {
    const t = e.target.closest && e.target.closest('[data-tip]');
    if (!t) { if (!e.target.closest || !e.target.closest('canvas')) tipHide(); return; }
    tipShow(e.clientX, e.clientY, t.getAttribute('data-tip'));
  });
  document.addEventListener('focusin', (e) => {
    const t = e.target.closest && e.target.closest('[data-tip]');
    if (!t) return;
    const r = t.getBoundingClientRect();
    tipShow(r.right, r.bottom, t.getAttribute('data-tip'));
  });
  document.addEventListener('focusout', () => tipHide());
  addEventListener('hashchange', () => tipHide());
}
