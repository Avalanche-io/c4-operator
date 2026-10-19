// CALLOUTS: the shared annotation layer for the intro, the tutorial
// screenshots and later the in-product tours (docs/intro/callouts.md).
//
// A reticle of four corner brackets locks on the thing being named, a leader
// with one 45-degree knee runs to a plate, and the plate carries the term's
// glossary number, the term and one plain sentence. White lines, a little
// heavier than the HUD's own; the plate's faint violet fill is the only
// violet. The style lives in callouts.css.
//
// Animated (the intro): build once, then draw every frame with the seconds
// elapsed since the callout began. Static (screenshots): overlay(root,
// notes) places finished callouts on any page.

import { esc } from './core.js';
import { TERMS, numberOf, swatchHTML } from './glossary.js';

const NS = 'http://www.w3.org/2000/svg';
const svgEl = (tag, parent, attrs = {}) => { const n = document.createElementNS(NS, tag); for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v); parent.appendChild(n); return n; };
const clamp = (x, a = 0, b = 1) => Math.max(a, Math.min(b, x));
const ease = (x) => { x = clamp(x); return x < 0.5 ? 4 * x * x * x : 1 - Math.pow(-2 * x + 2, 3) / 2; };
const lerp = (a, b, t) => a + (b - a) * t;

// timing, in seconds from the callout's start
export const TIMING = { lock: 0.45, lead: [0.1, 0.35], type: [0.25, 0.4], def: [0.55, 0.4] };

// text: a glossary key ('slack'), or { term, def, swatch, num }
export function textOf(k) {
  if (typeof k === 'string') { const T = TERMS[k] || { term: k, def: '' }; return { ...T, num: numberOf(k) }; }
  return k;
}

// build: the callout's SVG group (in an <svg> layer) and its plate (in a
// positioned host). Returns the handle draw() takes.
export function build(svg, host, k) {
  const T = textOf(k);
  const g = svgEl('g', svg, { class: 'c-callout' });
  const ret = svgEl('path', g, { class: 'ret' });
  const lead = svgEl('path', g, { class: 'lead' });
  const dot = svgEl('rect', g, { class: 'dot', width: 7, height: 7 });
  const plate = document.createElement('div');
  plate.className = 'c-plate';
  plate.innerHTML = `<div class="hd">${T.num ? `<span class="num">T${String(T.num).padStart(2, '0')}</span>` : ''}`
    + `${swatchHTML(T.swatch)}<span class="term"></span></div>`
    + `<div class="def">${esc(T.def || '')}</div><i class="marks"></i>`;
  host.appendChild(plate);
  return { g, ret, lead, dot, plate, termEl: plate.querySelector('.term'), defEl: plate.querySelector('.def'), text: String(T.term || '').toUpperCase() };
}

// draw: place the callout on rect r (x, y, w, h in the layer's pixels).
//   spec:   { side: right|left|above|below, dx, dy, w } or { at: [x, y], w }
//   o:      { elapsed (s since it began; Infinity = finished), alpha, bounds: { x0, y0, x1, y1 } }
export function draw(h, r, spec, o = {}) {
  const e = o.elapsed == null ? Infinity : o.elapsed;
  const a = clamp(o.alpha == null ? 1 : o.alpha);
  const b = o.bounds || { x0: 12, y0: 12, x1: 1e6, y1: 1e6 };
  if (a <= 0 || !r) { h.g.setAttribute('opacity', 0); h.plate.style.opacity = 0; h.plate.style.visibility = 'hidden'; return; }
  // the reticle locks on: brackets close in from 1.25x
  const lock = ease(clamp(e / TIMING.lock));
  const m = 5, s = lerp(1.25, 1, lock);
  const cx = r.x + r.w / 2, cy = r.y + r.h / 2;
  const hw = (r.w / 2 + m) * s, hh = (r.h / 2 + m) * s;
  const arm = Math.min(12, hw, hh) * lock + 2;
  const x0 = cx - hw, x1 = cx + hw, y0 = cy - hh, y1 = cy + hh;
  h.ret.setAttribute('d', `M${x0} ${y0 + arm}V${y0}H${x0 + arm}M${x1 - arm} ${y0}H${x1}V${y0 + arm}M${x1} ${y1 - arm}V${y1}H${x1 - arm}M${x0 + arm} ${y1}H${x0}V${y1 - arm}`);
  // the plate
  const w = spec.w || 340;
  let px, py;
  if (spec.at) { px = spec.at[0]; py = spec.at[1]; }
  else if (spec.side === 'right') { px = x1 + (spec.dx || 40); py = r.y + (spec.dy || 0); }
  else if (spec.side === 'left') { px = x0 - w + (spec.dx || -40); py = r.y + (spec.dy || 0); }
  else if (spec.side === 'above') { px = r.x + (spec.dx || 0); py = y0 - 90 + (spec.dy || 0); }
  else { px = r.x + (spec.dx || 0); py = y1 + (spec.dy || 30); }
  px = clamp(px, b.x0, b.x1 - w);
  py = clamp(py, b.y0, b.y1 - 100);
  h.plate.style.transform = `translate(${px.toFixed(1)}px, ${py.toFixed(1)}px)`;
  h.plate.style.width = `${w}px`;
  h.plate.style.opacity = a.toFixed(3);
  h.plate.style.visibility = 'visible';
  // the leader: out of the reticle on the side that faces the plate, one
  // 45-degree knee, then straight into the plate's near edge
  const ph = h.plate.offsetHeight || 70;
  const pts = route({ x0, y0, x1, y1, cx, cy }, { x0: px, x1: px + w, y0: py, y1: py + ph });
  h.lead.setAttribute('d', 'M' + pts.map((p) => `${p.x.toFixed(1)} ${p.y.toFixed(1)}`).join('L'));
  let len = 1;
  for (let q = 1; q < pts.length; q++) len += Math.hypot(pts[q].x - pts[q - 1].x, pts[q].y - pts[q - 1].y);
  const dl = clamp((e - TIMING.lead[0]) / TIMING.lead[1]);
  h.lead.setAttribute('stroke-dasharray', `${len}`);
  h.lead.setAttribute('stroke-dashoffset', `${(len * (1 - ease(dl))).toFixed(1)}`);
  h.dot.setAttribute('x', pts[0].x - 3.5); h.dot.setAttribute('y', pts[0].y - 3.5);
  h.g.setAttribute('opacity', a.toFixed(3));
  // the term types on, the definition follows
  const n = Math.round(h.text.length * clamp((e - TIMING.type[0]) / TIMING.type[1]));
  const shown = h.text.slice(0, n) + (n < h.text.length ? '█' : '');
  if (h.termEl.textContent !== shown) h.termEl.textContent = shown;
  h.defEl.style.opacity = ease(clamp((e - TIMING.def[0]) / TIMING.def[1])).toFixed(3);
}

// route: the leader's points from the reticle box R to the plate box P
export function route(R, P) {
  const below = P.y0 > R.y1 + 8, above = P.y1 < R.y0 - 8;
  const side = P.x0 > R.x1 + 8 || P.x1 < R.x0 - 8;
  if (side || !(below || above)) {
    const right = P.x0 > R.cx;
    const S = { x: right ? R.x1 : R.x0, y: R.cy };
    const E = { x: right ? P.x0 : P.x1, y: clamp(R.cy, P.y0 + 14, P.y1 - 14) };
    const dx = Math.abs(E.x - S.x), dy = E.y - S.y;
    const k = Math.min(Math.abs(dy), dx * 0.7);
    const K1 = { x: S.x + (right ? 1 : -1) * Math.min(14, dx * 0.2), y: S.y };
    const K2 = { x: K1.x + (right ? 1 : -1) * k, y: S.y + Math.sign(dy) * k };
    return [S, K1, K2, { x: K2.x, y: E.y }, E];
  }
  const S = { x: R.cx, y: below ? R.y1 : R.y0 };
  const E = { x: clamp(R.cx, P.x0 + 24, P.x1 - 24), y: below ? P.y0 : P.y1 };
  const dx = E.x - S.x, dy = Math.abs(E.y - S.y);
  const k = Math.min(Math.abs(dx), dy * 0.7);
  const K1 = { x: S.x, y: S.y + (below ? 1 : -1) * Math.min(14, dy * 0.2) };
  const K2 = { x: S.x + Math.sign(dx) * k, y: K1.y + (below ? 1 : -1) * k };
  return [S, K1, K2, { x: E.x, y: K2.y }, E];
}

// overlay: finished callouts on a live page, for screenshots and tours.
//   notes: [{ target: CSS selector, term: glossary key or { term, def }, side, dx, dy, w, at }]
// Targets are found in the document; callouts are drawn in page pixels in a
// layer over root (default document.body). Call again after a re-render; it
// replaces the previous overlay.
export function overlay(notes, root = document.body) {
  let layer = root.querySelector(':scope > .c-overlay');
  if (layer) layer.remove();
  layer = document.createElement('div');
  layer.className = 'c-overlay';
  const W = Math.max(document.documentElement.scrollWidth, window.innerWidth), H = Math.max(document.documentElement.scrollHeight, window.innerHeight);
  layer.style.cssText = `position:absolute;left:0;top:0;width:${W}px;height:${H}px;pointer-events:none;z-index:1000`;
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('class', 'c-ann');
  svg.setAttribute('width', W); svg.setAttribute('height', H);
  layer.appendChild(svg);
  const host = document.createElement('div');
  host.className = 'c-plates';
  layer.appendChild(host);
  root.appendChild(layer);
  const missing = [];
  for (const nt of notes || []) {
    const el = typeof nt.target === 'string' ? document.querySelector(nt.target) : nt.target;
    if (!el) { missing.push(nt.target); continue; }
    const b = el.getBoundingClientRect();
    const r = { x: b.left + window.scrollX, y: b.top + window.scrollY, w: b.width, h: b.height };
    const h = build(svg, host, nt.term);
    draw(h, r, nt, { bounds: { x0: 12, y0: 12, x1: W - 12, y1: H - 12 } });
  }
  return missing;
}
