// MARKS: the HUD's ink marks, one shape each, drawn the same in every place:
// canvas cells (STACK), legends, glossary swatches, lenses and cards. A mark
// adds a fact to a cell without changing its hue (design/README.md, "One
// vocabulary"): the cell keeps the color of its state.
//
//   clock   waits for a thing: capacity, a license, a party, a time
//   lock    locked by another unit
//   pause   yielding: asked to stop at its next safe point
//   rework  a check sent it back to run again with a change
//   tick    checked: a check passed
//   reuse   reused: the same inputs ran before, the output was reused
//   check   accepted: bad frames accepted (on pale red)
//   seal    acknowledged: someone has it (on pale red)
//   flag    needs you: the white corner flag (on red)
//   model   reasons with a model (a unit's reasoning, metadata: ink only)
//   procedure  reasons with a procedure: a command, no model
//
// Each is a path in a 10 x 10 box, stroked (or filled, for pause and flag).
//   drawMark(g, kind, x, y, size, color)   on a canvas, top-left at x, y
//   markSVG(kind, { size, color, title })  an inline <svg> string
//   MARK_KINDS                             the kinds, in the order above

const M = {
  clock: { d: 'M5 1.2A3.8 3.8 0 1 1 4.99 1.2ZM5 2.9V5.2H6.9', fill: false, w: 1.15 },
  lock: { d: 'M2.4 4.6H7.6V8.8H2.4ZM3.5 4.6V3.3A1.5 1.5 0 0 1 6.5 3.3V4.6', fill: false, w: 1.15 },
  pause: { d: 'M2.6 1.8H4.3V8.2H2.6ZM5.7 1.8H7.4V8.2H5.7Z', fill: true },
  rework: { d: 'M7.9 6.1A3.2 3.2 0 1 1 7.1 2.6M7.6 0.9V2.9H5.6', fill: false, w: 1.15 },
  tick: { d: 'M1.8 5.4L4.1 7.7L8.3 2.6', fill: false, w: 1.4 },
  reuse: { d: 'M1.6 3.6H6.4V8.4H1.6ZM3.6 3.6V1.6H8.4V6.4H6.4', fill: false, w: 1.1 },
  check: { d: 'M2.2 5.1L4.2 7.1L8 3', fill: false, w: 1.5 },
  seal: { d: 'M5 1.8A3.2 3.2 0 1 1 4.99 1.8Z', fill: false, w: 1.2 },
  flag: { d: 'M3 0H10V7Z', fill: true },
  model: { d: 'M5 1L6.2 3.8L9 5L6.2 6.2L5 9L3.8 6.2L1 5L3.8 3.8Z', fill: false, w: 1.1 },
  procedure: { d: 'M5 1.4L8.1 3.2V6.8L5 8.6L1.9 6.8V3.2ZM5 4.3A0.7 0.7 0 1 1 4.99 4.3Z', fill: false, w: 1.1 },
};

export const MARK_KINDS = Object.keys(M);

const paths = new Map();
function path(kind) {
  if (!paths.has(kind)) paths.set(kind, typeof Path2D === 'function' ? new Path2D(M[kind].d) : null);
  return paths.get(kind);
}

// drawMark draws a mark on a canvas, size px square, top-left at x, y.
export function drawMark(g, kind, x, y, size, color) {
  const m = M[kind], p = m && path(kind);
  if (!p || size <= 0) return;
  const k = size / 10;
  g.save();
  g.translate(x, y); g.scale(k, k);
  if (m.fill) { g.fillStyle = color; g.fill(p); } else {
    g.strokeStyle = color; g.lineWidth = Math.max(m.w, 1.1 / k); g.lineCap = 'round'; g.lineJoin = 'round';
    g.stroke(p);
  }
  g.restore();
}

const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');

// markSVG: the mark as an inline SVG string in currentColor (or color),
// for HTML legends, swatches, lenses and cards.
export function markSVG(kind, { size = 10, color = 'currentColor', title } = {}) {
  const m = M[kind];
  if (!m) return '';
  const paint = m.fill ? `fill="${color}"` : `fill="none" stroke="${color}" stroke-width="${m.w}" stroke-linecap="round" stroke-linejoin="round"`;
  const t = title ? `<title>${esc(title)}</title>` : '';
  return `<svg class="mark mark-${kind}" width="${size}" height="${size}" viewBox="0 0 10 10" ${title ? 'role="img"' : 'aria-hidden="true"'}>${t}<path d="${m.d}" ${paint}/></svg>`;
}

// markEl: markSVG as a DOM element (views built with h()).
export function markEl(kind, opts) {
  const t = document.createElement('template');
  t.innerHTML = markSVG(kind, opts);
  return t.content.firstChild;
}

// markAttrs: a mark as the attributes of an SVG <path> in a 10 x 10 box,
// for drawings built element by element (the lenses' charts).
export function markAttrs(kind, color = 'currentColor') {
  const m = M[kind];
  if (!m) return null;
  return m.fill ? { d: m.d, fill: color } : { d: m.d, fill: 'none', stroke: color, 'stroke-width': m.w, 'stroke-linecap': 'round', 'stroke-linejoin': 'round' };
}
