// ATLAS: capacity on the map. Each site is a set of concentric rings, one
// per pool: the ring is the pool's slots at the site, the lit arc is slots
// in use, blue only for the share running your units, a dashed arc a
// request awaiting approval and a dotted arc the burst headroom the
// provider can still give. The store region is a diamond; arcs carry bytes
// from it to the sites, and the tiers' fan-in says how few of the nodes'
// reads ever reach it. Land is a dot matrix from Natural Earth (land.js).
//
//   atlasSVG(snap, { w, h })        the map; a schematic of zones when no sites are declared
//   siteScaleSVG(snap, group, o)    one site's racks, nodes and slots
//   funnelSVG(snap, o)              requests per minute entering each tier

import { tok, esc } from './core.js';
import * as f from './fmt.js';
import { LAND, EXTENT } from './land.js';
import { nodeSlots, nodeTip } from './lattice.js';

const RAD = Math.PI / 180;

// ---------------------------------------------------------------- model

// sitesModel places nodes and pools at sites. A site with nodes is a
// compute site; a declared site with none is where the stores are.
export function sitesModel(snap) {
  const nodes = snap.nodes || [];
  const sites = snap.sites || [];
  const pending = (snap.capacity || []).filter((c) => c.state === 'PendingApproval');
  const compute = [], stores = [];
  for (const s of sites) {
    const ns = nodes.filter((n) => n.region && n.region === s.region);
    if (!ns.length) { stores.push(s); continue; }
    compute.push({ site: s, nodes: ns, rings: ringsFor(snap, ns, pending) });
  }
  return { compute, stores, declared: sites.length > 0 };
}

// zonesModel groups nodes by zone (or locality) when no sites are declared.
export function zonesModel(snap) {
  const nodes = snap.nodes || [];
  const pending = (snap.capacity || []).filter((c) => c.state === 'PendingApproval');
  const order = [];
  for (const n of nodes) { const z = n.zone || n.locality || 'unplaced'; if (!order.includes(z)) order.push(z); }
  return order.map((z) => {
    const ns = nodes.filter((n) => (n.zone || n.locality || 'unplaced') === z);
    return { site: { name: z, label: z, kind: ns.every((n) => (snap.pools.find((p) => p.name === n.pool) || { attrs: {} }).attrs.capacityType === 'spot') ? 'cloud' : 'onprem' }, nodes: ns, rings: ringsFor(snap, ns, pending) };
  });
}

function ringsFor(snap, ns, pending) {
  const out = [];
  for (const p of snap.pools || []) {
    const here = ns.filter((n) => n.pool === p.name);
    if (!here.length) continue;
    const all = (snap.nodes || []).filter((n) => n.pool === p.name);
    const share = all.length ? here.length / all.length : 1;
    const slots = here.reduce((a, n) => a + n.slots, 0);
    const used = here.reduce((a, n) => a + n.used, 0);
    const mine = here.reduce((a, n) => a + nodeSlots(n).mine.length, 0);
    const b = p.burst;
    const pend = Math.round(pending.filter((c) => c.pool === p.name).reduce((a, c) => a + c.slots, 0) * share);
    const head = b ? Math.max(0, Math.round((b.maxSlots - b.provisioned) * share) - pend) : 0;
    out.push({ pool: p, slots, used, mine, pend, head, cost: pending.filter((c) => c.pool === p.name).reduce((a, c) => a + c.estCost, 0) });
  }
  return out;
}

// ---------------------------------------------------------------- projection

function view(points, w, h) {
  const lats = points.map((p) => p.lat), lons = points.map((p) => p.lon);
  let la0 = Math.min(...lats), la1 = Math.max(...lats), lo0 = Math.min(...lons), lo1 = Math.max(...lons);
  const pad = Math.max(4, (la1 - la0) * 0.35, (lo1 - lo0) * 0.2);
  la0 -= pad; la1 += pad; lo0 -= pad; lo1 += pad;
  const west = EXTENT.west.box;
  const grid = lo0 >= west[0] - 2 && lo1 <= west[1] + 2 && la0 >= west[2] - 2 && la1 <= west[3] + 2 ? 'west' : 'world';
  let box;
  if (grid === 'west') {
    // the regional grid: all of it, and the sheet takes its shape
    box = west.slice();
    const k = Math.cos(((box[2] + box[3]) / 2) * RAD);
    const aspect = ((box[1] - box[0]) * k) / (box[3] - box[2]);
    if (w / h > aspect) w = Math.round(h * aspect); else h = Math.round(w / aspect);
  } else {
    // fit the panel's aspect by widening the narrow axis around the centre
    const k = Math.cos(((la0 + la1) / 2) * RAD);
    const cx = (lo0 + lo1) / 2, cy = (la0 + la1) / 2;
    let lonSpan = lo1 - lo0, latSpan = la1 - la0;
    if ((lonSpan * k) / latSpan < w / h) lonSpan = (latSpan * w) / h / k; else latSpan = (lonSpan * k * h) / w;
    lonSpan = Math.min(lonSpan, 360); latSpan = Math.min(latSpan, 136);
    box = [cx - lonSpan / 2, cx + lonSpan / 2, cy - latSpan / 2, cy + latSpan / 2];
  }
  const X = (lon) => ((lon - box[0]) / (box[1] - box[0])) * w;
  const Y = (lat) => ((box[3] - lat) / (box[3] - box[2])) * h;
  return { box, grid, X, Y, w, h, pxPerDeg: w / (box[1] - box[0]) };
}

// ---------------------------------------------------------------- rings

function arc(cx, cy, r, a0, a1) {
  // fractions of a turn from twelve o'clock, clockwise
  if (a1 - a0 >= 0.9999) return `M${cx} ${cy - r} a${r} ${r} 0 1 1 0 ${2 * r} a${r} ${r} 0 1 1 0 ${-2 * r}`;
  const p = (a) => [cx + r * Math.sin(a * 2 * Math.PI), cy - r * Math.cos(a * 2 * Math.PI)];
  const [x0, y0] = p(a0), [x1, y1] = p(a1);
  return `M${x0.toFixed(2)} ${y0.toFixed(2)} A${r} ${r} 0 ${a1 - a0 > 0.5 ? 1 : 0} 1 ${x1.toFixed(2)} ${y1.toFixed(2)}`;
}

// ringsSVG draws a site's pool rings around (cx, cy), outermost to innermost.
export function ringsSVG(T, g, cx, cy, o = {}) {
  const sw = o.sw || 3.5, gap = o.gap || 3, r0 = o.r0 || 9;
  let r = r0 + (g.rings.length - 1) * (sw + gap);
  let s = '';
  for (const q of g.rings) {
    const whole = q.slots + q.pend + q.head || 1;
    const fr = (v) => v / whole;
    const tip = `${g.site.label || g.site.name} · ${q.pool.name} | ${q.used} of ${q.slots} slots in use${q.mine ? `, ${q.mine} running your units` : ''}${q.pend ? ` | +${q.pend} requested · est. ${f.money(q.cost)} · awaiting approval` : ''}${q.head ? ` | ${q.head} more the provider can give` : ''}`;
    s += `<g data-tip="${esc(tip)}">`;
    s += `<path d="${arc(cx, cy, r, 0, fr(q.slots))}" fill="none" stroke="${T.g[9]}" stroke-width="${sw}"/>`;
    if (q.used) s += `<path d="${arc(cx, cy, r, 0, fr(q.used))}" fill="none" stroke="${T.g[16]}" stroke-width="${sw}"/>`;
    if (q.mine) s += `<path d="${arc(cx, cy, r, 0, fr(q.mine))}" fill="none" stroke="${T.run}" stroke-width="${Math.max(1.5, sw * 0.42)}"/>`;
    if (q.pend) s += `<path d="${arc(cx, cy, r, fr(q.slots), fr(q.slots + q.pend))}" fill="none" stroke="${T.g[15]}" stroke-width="${sw * 0.5}" stroke-dasharray="3 2"/>`;
    if (q.head) s += `<path d="${arc(cx, cy, r, fr(q.slots + q.pend), 1)}" fill="none" stroke="${T.g[13]}" stroke-width="${sw * 0.45}" stroke-dasharray="1 2.5"/>`;
    // a hit target the width of the ring
    s += `<circle cx="${cx}" cy="${cy}" r="${r}" fill="none" stroke="transparent" stroke-width="${sw + gap}"/></g>`;
    r -= sw + gap;
  }
  const suspect = g.nodes.some((n) => n.suspect), down = g.nodes.some((n) => n.state === 'notReady');
  s += `<circle cx="${cx}" cy="${cy}" r="${Math.max(2, r0 - sw - 2)}" fill="${down ? T.fail : suspect ? T.temp : T.ink}"/>`;
  return { svg: s, r: r0 + (g.rings.length - 1) * (sw + gap) + sw / 2 };
}

function siteSub(g) {
  return g.rings.map((q) => `${q.pool.name} ${q.used}/${q.slots}${q.head || q.pend ? ` of ${q.slots + q.pend + q.head}` : ''}`).join(' · ');
}

// ---------------------------------------------------------------- the map

export function atlasSVG(snap, o = {}) {
  const T = tok();
  o = Object.assign({ w: 900, h: 640 }, o);
  const M = sitesModel(snap);
  if (!M.compute.length) return schematicSVG(snap, o);
  const pts = [...M.compute.map((g) => g.site), ...M.stores];
  const V = view(pts, o.w, o.h);
  const { w, h } = V;
  let s = `<defs><radialGradient id="atl-vig" cx="50%" cy="50%" r="70%"><stop offset="60%" stop-color="${T.field}" stop-opacity="0"/><stop offset="100%" stop-color="${T.field}" stop-opacity=".85"/></radialGradient></defs>`;
  // graticule: every 5 degrees regionally, 30 on the world
  const stepL = V.box[1] - V.box[0] > 90 ? 30 : 5;
  for (let lon = Math.ceil(V.box[0] / stepL) * stepL; lon <= V.box[1]; lon += stepL) {
    s += `<line x1="${V.X(lon)}" x2="${V.X(lon)}" y1="0" y2="${h}" stroke="${T.rule1}"/>`;
    if (V.X(lon) < w - 40) s += `<text x="${V.X(lon) + 3}" y="${h - 4}" fill="${T.g[12]}" font-size="8" font-family='${T.mono}'>${Math.abs(lon)}°${lon < 0 ? 'W' : 'E'}</text>`;
  }
  for (let lat = Math.ceil(V.box[2] / stepL) * stepL; lat <= V.box[3]; lat += stepL) {
    s += `<line x1="0" x2="${w}" y1="${V.Y(lat)}" y2="${V.Y(lat)}" stroke="${T.rule1}"/>`;
    if (V.Y(lat) > 16) s += `<text x="3" y="${V.Y(lat) - 3}" fill="${T.g[12]}" font-size="8" font-family='${T.mono}'>${Math.abs(lat)}°${lat < 0 ? 'S' : 'N'}</text>`;
  }
  // land as a dot matrix
  const ext = EXTENT[V.grid];
  const dot = Math.max(1, Math.min(3.2, ext.step * V.pxPerDeg * 0.32));
  let d = '';
  for (const [lon, lat] of LAND[V.grid]) {
    if (lon < V.box[0] || lon > V.box[1] || lat < V.box[2] || lat > V.box[3]) continue;
    d += `M${(V.X(lon) - dot / 2).toFixed(1)} ${(V.Y(lat) - dot / 2).toFixed(1)}h${dot.toFixed(1)}v${dot.toFixed(1)}h-${dot.toFixed(1)}z`;
  }
  s += `<path d="${d}" fill="${T.g[12]}"/>`;
  s += `<rect width="${w}" height="${h}" fill="url(#atl-vig)"/>`;
  // arcs: bytes from the store region to each compute site, flowing
  const fi = (snap.tiers && snap.tiers.fanIn) || {};
  const tiers = (snap.tiers && snap.tiers.tiers) || [];
  const served = (n) => (tiers.find((t) => t.name === n) || {}).bytesServed || 0;
  const store = M.stores[0];
  const placed = [];
  if (store) {
    const sx = V.X(store.lon), sy = V.Y(store.lat);
    const totalSlots = M.compute.reduce((a, g) => a + g.rings.reduce((b, q) => b + q.slots, 0), 0) || 1;
    for (const g of M.compute) {
      const bx = V.X(g.site.lon), by = V.Y(g.site.lat);
      const dx = bx - sx, dy = by - sy, len = Math.hypot(dx, dy) || 1;
      const bend = Math.min(140, len * 0.35);
      const mx = (sx + bx) / 2 - (dy / len) * bend, my = (sy + by) / 2 + (dx / len) * bend;
      const share = g.rings.reduce((b, q) => b + q.slots, 0) / totalSlots;
      const wdt = 1 + 2.5 * share;
      const path = `M${sx} ${sy} Q${mx.toFixed(1)} ${my.toFixed(1)} ${bx} ${by}`;
      s += `<path d="${path}" fill="none" stroke="${T.g[10]}" stroke-width="${wdt + 2}"/>`;
      s += `<path class="atl-flow" d="${path}" fill="none" stroke="${T.g[17]}" stroke-width="${wdt}" stroke-dasharray="2 9" stroke-linecap="round" data-tip="${esc(`bytes from the store region to ${g.site.label || g.site.name} | the nodes read views through their cache tiers | regional tier ${f.num(fi.regionalRequests)} requests/min · ${f.bytes(served('regional'))} served | store ${f.num(fi.storeRequests)} reads/min, farm-wide`)}"/>`;
    }
    // the store: a diamond with its reads
    s += `<g data-tip="${esc(`${store.label || store.name} | the project stores · ${store.region} | ${f.num(fi.storeRequests)} reads/min reach the store · ${f.bytes(served('store'))} served`)}"><rect x="${sx - 6}" y="${sy - 6}" width="12" height="12" fill="${T.field}" stroke="${T.ink}" stroke-width="1.5" transform="rotate(45 ${sx} ${sy})"/><rect x="${sx - 2}" y="${sy - 2}" width="4" height="4" fill="${T.ink}" transform="rotate(45 ${sx} ${sy})"/></g>`;
    placed.push({ x: sx, y: sy, r: 10, label: 'PROJECT STORE', sub: `${store.region} · ${f.num(fi.storeRequests)} reads/min`, sub2: store.label || store.name });
  }
  for (const g of M.compute) {
    const cx = V.X(g.site.lon), cy = V.Y(g.site.lat);
    const R = ringsSVG(T, g, cx, cy, { r0: 13, sw: 5, gap: 3.5 });
    s += R.svg;
    const pend = g.rings.filter((q) => q.pend);
    placed.push({ x: cx, y: cy, r: R.r, label: (g.site.label || g.site.name).toUpperCase(), sub: siteSub(g), sub2: pend.length ? `+${pend.reduce((a, q) => a + q.pend, 0)} requested · est. ${f.money(pend.reduce((a, q) => a + q.cost, 0))} · awaiting approval` : `${g.nodes.length} nodes · ${g.site.kind === 'onprem' ? 'machine room' : 'cloud region'} · ${g.site.region}${f.siteClock(f.now(), g.site.tz) ? ' · ' + f.siteClock(f.now(), g.site.tz) : ''}` });
  }
  s += labels(T, placed, w, h);
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" role="img" aria-label="${esc(`Map: ${M.compute.length} sites with nodes${store ? ', the project stores in ' + store.region : ''}`)}">${s}</svg>`;
}

// labels sit beside their mark: right, then left, then further out on a
// leader line, wherever they clear the sheet's edge, the other marks and
// the labels already set. Crowded sites fan out instead of overprinting.
function labels(T, placed, w, h) {
  let s = '';
  const boxes = [];
  const overlap = (b) => {
    let n = 0;
    for (const o of boxes) if (b.x0 < o.x1 && b.x1 > o.x0 && b.y0 < o.y1 && b.y1 > o.y0) n += (Math.min(b.x1, o.x1) - Math.max(b.x0, o.x0)) * (Math.min(b.y1, o.y1) - Math.max(b.y0, o.y0));
    for (const p of placed) if (b.x0 < p.x + p.r && b.x1 > p.x - p.r && b.y0 < p.y + p.r && b.y1 > p.y - p.r) n += 4 * p.r * p.r;
    if (b.x0 < 2 || b.x1 > w - 2 || b.y0 < 0 || b.y1 > h) n += 1e6;
    return n;
  };
  for (const p of placed) {
    const tw = Math.max(p.label.length * 7.2, (p.sub || '').length * 5.6, (p.sub2 || '').length * 5.6);
    const lines = 1 + (p.sub ? 1 : 0) + (p.sub2 ? 1 : 0);
    const bh = lines * 12.5 + 4;
    const cands = [];
    for (const d of [10, 36, 70, 110]) {
      for (const [dx, dy] of [[1, 0], [-1, 0], [1, -1], [1, 1], [-1, -1], [-1, 1], [0, -1], [0, 1]]) {
        const ax = p.x + dx * (p.r + d), ay = p.y + dy * (p.r + d * 0.8);
        const anchor = dx < 0 ? 'end' : 'start';
        const x = dx === 0 ? ax - tw / 2 : ax;
        const y = dy < 0 ? ay - bh + 12 : dy > 0 ? ay + 12 : ay - 4;
        const x0 = anchor === 'end' ? x - tw : x;
        cands.push({ x: dx === 0 ? x0 : x, y, anchor: dx === 0 ? 'start' : anchor, x0, lead: d > 10 || dx === 0, ax, ay, d });
      }
    }
    let best = null, bestN = Infinity;
    for (const c of cands) {
      const n = overlap({ x0: c.x0 - 4, x1: c.x0 + tw + 4, y0: c.y - 12, y1: c.y - 12 + bh }) + c.d * 0.5;
      if (n < bestN) { bestN = n; best = c; }
      if (n < 60) break;
    }
    const { x, y, anchor, x0 } = best;
    boxes.push({ x0: x0 - 4, x1: x0 + tw + 4, y0: y - 12, y1: y - 12 + bh });
    if (best.lead) {
      const lx = anchor === 'end' ? x0 + tw + 3 : x0 - 3, ly = y - 4;
      const a = Math.atan2(ly - p.y, lx - p.x);
      s += `<path d="M${(p.x + Math.cos(a) * (p.r + 2)).toFixed(1)} ${(p.y + Math.sin(a) * (p.r + 2)).toFixed(1)} L${lx.toFixed(1)} ${ly.toFixed(1)}" stroke="${T.g[14]}" fill="none"/>`;
    }
    s += `<rect x="${x0 - 4}" y="${y - 12}" width="${tw + 8}" height="${bh}" fill="${T.field}" fill-opacity=".82"/>`;
    s += `<text x="${x}" y="${y}" text-anchor="${anchor}" fill="${T.paper}" font-size="11.5" font-weight="600" letter-spacing=".12em" font-family='${T.sans}'>${esc(p.label)}</text>`;
    if (p.sub) s += `<text x="${x}" y="${y + 13}" text-anchor="${anchor}" fill="${T.ink3}" font-size="9.5" font-family='${T.mono}'>${esc(p.sub)}</text>`;
    if (p.sub2) s += `<text x="${x}" y="${y + 25}" text-anchor="${anchor}" fill="${T.ink4}" font-size="9" font-family='${T.mono}'>${esc(p.sub2)}</text>`;
  }
  return s;
}

// schematicSVG: no sites declared. Zones in a row on an unprojected
// graticule, labeled as such; the store has no place, so it sits apart.
export function schematicSVG(snap, o = {}) {
  const T = tok();
  const { w, h } = o;
  const Z = zonesModel(snap);
  let s = '';
  for (let x = 0; x <= w; x += 40) s += `<line x1="${x}" x2="${x}" y1="0" y2="${h}" stroke="${x % 200 ? T.rule0 : T.rule1}"/>`;
  for (let y = 0; y <= h; y += 40) s += `<line x1="0" x2="${w}" y1="${y}" y2="${y}" stroke="${y % 200 ? T.rule0 : T.rule1}"/>`;
  s += `<text x="12" y="20" fill="${T.ink4}" font-size="9.5" font-family='${T.mono}'>no sites declared: zones drawn without a map, in the order the nodes report them</text>`;
  const n = Z.length;
  const cols = Math.min(n, Math.max(1, Math.floor((w - 140) / 200)));
  const rows = Math.ceil(n / cols);
  const cw = (w - 160) / cols, rh = Math.min(220, (h - 60) / Math.max(1, rows));
  const placed = [];
  const fi = (snap.tiers && snap.tiers.fanIn) || {};
  const sx = w - 60, sy = h / 2;
  Z.forEach((g, k) => {
    const cx = 60 + (k % cols) * cw + cw * 0.25, cy = h / 2 + (Math.floor(k / cols) - (rows - 1) / 2) * rh;
    s += `<path class="atl-flow" d="M${sx} ${sy} L${cx} ${cy}" fill="none" stroke="${T.g[13]}" stroke-width="1" stroke-dasharray="2 9" stroke-linecap="round"/>`;
    const R = ringsSVG(T, g, cx, cy, { r0: 13, sw: 5, gap: 3.5 });
    s += R.svg;
    placed.push({ x: cx, y: cy, r: R.r, label: g.site.label.toUpperCase(), sub: siteSub(g), sub2: `${g.nodes.length} nodes` });
  });
  s += `<rect x="${sx - 6}" y="${sy - 6}" width="12" height="12" fill="${T.field}" stroke="${T.ink}" stroke-width="1.5" transform="rotate(45 ${sx} ${sy})"/>`;
  placed.push({ x: sx, y: sy, r: 10, label: 'STORE', sub: `${f.num(fi.storeRequests)} reads/min`, sub2: 'place not declared' });
  s += labels(T, placed, w, h);
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" role="img" aria-label="Zones without a map: ${n} zones">${s}</svg>`;
}

// ---------------------------------------------------------------- site scale

// siteScaleSVG: one site's racks side by side, each node a row of slot
// cells (your units blue, other projects grey, idle outlined), suspect
// nodes outlined orange, failures counted in red.
export function siteScaleSVG(snap, g, o = {}) {
  const T = tok();
  o = Object.assign({ w: 520, cell: 9, gap: 2 }, o);
  const locs = [];
  for (const n of g.nodes) { const l = n.locality || n.zone || 'unplaced'; let L = locs.find((x) => x.name === l); if (!L) locs.push(L = { name: l, nodes: [] }); L.nodes.push(n); }
  const order = (g.site.localities || []);
  locs.sort((a, b) => (order.indexOf(a.name) + 1 || 99) - (order.indexOf(b.name) + 1 || 99));
  const c = o.cell, gp = o.gap, step = c + gp;
  const maxSlots = Math.max(...g.nodes.map((n) => n.slots));
  const perRow = Math.min(8, maxSlots);
  const colW = Math.max(perRow * step + 34, 92);
  const cols = Math.max(1, Math.floor((o.w + 14) / (colW + 14)));
  let s = '', x = 0, y = 0, rowH = 0, H = 0, k = 0;
  for (const L of locs) {
    if (k && k % cols === 0) { y += rowH + 14; x = 0; rowH = 0; }
    let yy = y;
    s += `<text x="${x}" y="${yy + 8}" fill="${T.ink3}" font-size="8.5" font-weight="600" letter-spacing=".14em" font-family='${T.sans}'>${esc(L.name.toUpperCase())}</text>`;
    yy += 14;
    for (const n of L.nodes) {
      const S = nodeSlots(n);
      const rows = Math.ceil(n.slots / perRow);
      let cells = '';
      for (let i = 0; i < n.slots; i++) {
        const cx = x + (i % perRow) * step, cy = yy + Math.floor(i / perRow) * step;
        if (i < S.mine.length) cells += `<rect x="${cx}" y="${cy}" width="${c}" height="${c}" fill="${T.run}"/><rect x="${cx}" y="${cy}" width="${c}" height="1.5" fill="${T.run}"/>`;
        else if (i < S.mine.length + S.other) cells += `<rect x="${cx}" y="${cy}" width="${c}" height="${c}" fill="${T.g[13]}"/>`;
        else cells += `<rect x="${cx + 0.5}" y="${cy + 0.5}" width="${c - 1}" height="${c - 1}" fill="none" stroke="${n.state === 'notReady' ? T.fail : T.g[12]}"/>`;
      }
      if (n.state === 'disabled') cells += `<line x1="${x}" x2="${x + perRow * step - gp}" y1="${yy + (rows * step) / 2}" y2="${yy + (rows * step) / 2}" stroke="${T.g[15]}"/>`;
      const bw = Math.min(n.slots, perRow) * step - gp, bh = rows * step - gp;
      if (n.suspect) cells += `<rect x="${x - 2.5}" y="${yy - 2.5}" width="${bw + 5}" height="${bh + 5}" fill="none" stroke="${T.temp}" stroke-width="1.5"/>`;
      if (n.failuresLastHour) cells += `<text x="${x + bw + 6}" y="${yy + c}" fill="${T.failText}" font-size="8.5" font-family='${T.mono}'>${n.failuresLastHour}×</text>`;
      s += `<g data-tip="${esc(nodeTip(n))}"><rect x="${x - 3}" y="${yy - 3}" width="${bw + 30}" height="${bh + 6}" fill="transparent"/>${cells}</g>`;
      yy += bh + 7;
    }
    rowH = Math.max(rowH, yy - y);
    H = Math.max(H, yy);
    x += colW + 14;
    k++;
  }
  const W = Math.min(o.w, cols * (colW + 14));
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H + 2}" viewBox="0 -3 ${W} ${H + 5}" role="img" aria-label="${esc(`${g.site.label || g.site.name}: ${locs.length} racks, ${g.nodes.length} nodes`)}">${s}</svg>`;
}

// ---------------------------------------------------------------- fan-in

// funnelSVG: requests per minute entering each tier, on a log scale. Each
// tier answers most of what reaches it and forwards only its misses.
export function funnelSVG(snap, o = {}) {
  const T = tok();
  o = Object.assign({ w: 520 }, o);
  const fi = (snap.tiers && snap.tiers.fanIn) || {};
  const tiers = (snap.tiers && snap.tiers.tiers) || [];
  const hit = (n) => { const t = tiers.find((x) => x.name === n); return t ? t.hitRate : null; };
  const rows = [
    ['PODS', fi.requesters, 'running units reading views', null, T.g[13]],
    ['NODE', fi.nodeRequests, 'node daemons', hit('node'), T.g[17]],
    ['PEER', fi.peerRequests, 'rendezvous homes in the zone', hit('peer'), T.g[16]],
    ['REGIONAL', fi.regionalRequests, 'regional replicas', hit('regional'), T.g[15]],
    ['STORE', fi.storeRequests, 'the only reads the store sees', hit('store'), T.g[14]],
  ];
  const max = Math.log10(Math.max(10, fi.nodeRequests || 10));
  const bw = o.w - 150;
  const rh = 34;
  let s = '';
  rows.forEach(([name, v, note, hr, col], k) => {
    const y = k * rh;
    const wv = Math.max(3, (Math.log10(Math.max(1, v || 0) + 1) / max) * bw);
    s += `<g data-tip="${esc(`${name.toLowerCase()} tier | ${f.num(v)} requests per minute${hr != null ? ` | ${f.pct(hr)} answered here` : ''} | ${note}`)}">`;
    s += `<rect x="0" y="${y}" width="${wv}" height="13" fill="${col}"/>`;
    s += `<text x="${wv + 8}" y="${y + 11}" fill="${T.paper}" font-size="13" font-family='${T.mono}'>${f.num(v)}</text>`;
    s += `<text x="0" y="${y + 25}" fill="${T.ink3}" font-size="8.5" font-weight="600" letter-spacing=".14em" font-family='${T.sans}'>${name}</text>`;
    s += `<text x="${name.length * 6.6 + 8}" y="${y + 25}" fill="${T.ink4}" font-size="9" font-family='${T.mono}'>${esc(note)}${hr != null && name !== 'STORE' ? ` · ${f.pct(hr)} hit` : ''}</text></g>`;
  });
  const H = rows.length * rh;
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${o.w}" height="${H}" viewBox="0 0 ${o.w} ${H}" role="img" aria-label="${esc(`Fan-in: ${f.num(fi.nodeRequests)} node requests a minute, ${f.num(fi.storeRequests)} reach the store`)}">${s}</svg>`;
}

export function fanRatio(snap) {
  const fi = (snap.tiers && snap.tiers.fanIn) || {};
  return fi.storeRequests ? Math.round(fi.nodeRequests / fi.storeRequests) : null;
}
