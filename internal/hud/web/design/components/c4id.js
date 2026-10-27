// C4ID: the HUD's C4 ID mark, a thin wrapper around the c4-mark package
// (vendored, pinned: design/vendor/c4-mark, hack/sync-c4-mark.sh).
//
// A C4 ID shows as the c4 logo in corner brackets (the maintainer's pick, F), in
// ink: an ID is verification metadata, not a state (design/README.md).
// Hover or focus shows c4… and the ID's last eight characters, with the full
// ID in the tooltip; click, Enter or Space copies it; on touch a tap shows
// it and a Copy button. Where IDs are compared, the expanded form adds one to
// three glyphs drawn from the ID's SHA-512 digest (5 x 5, mirrored), and a
// tail of the last characters can sit beside the mark. Below 16 px the
// glyphs give way to the bare mark (c4-mark's MIN_GLYPH_PX).
//
//   c4idHTML(id, opts)   an HTML string (views, SVG components, the intro)
//   c4idEl(id, opts)     a DOM element (views built with h())
//   c4idSVGEl / c4idSVG  the mark inside an SVG drawing (the graph's edges)
//   installC4ID(root)    the behavior, once per document
// opts: { tail: characters after the mark, label: what the ID names
// ("output view"), density: 'bare' | 'expanded', glyphs: 1..3, gap }

import { markHTML, install, svg, decode, fold, abbrev as abbrevMark, isValid } from '../vendor/c4-mark/c4mark.js';

export { decode as digestOf, fold, isValid };
export const abbrev = (id) => abbrevMark(id);
export const DEFAULT_ICON = 'reticle';

export function c4idHTML(id, { tail, prefix, label = 'C4 ID', density = 'bare', glyphs = 2, gap = 'default' } = {}) {
  // prefix is the old name for tail: the mark never shows the characters
  // after "c4", which carry little
  return markHTML(id, { glyphs: density === 'expanded' ? glyphs : 0, tail: tail ?? prefix ?? 0, label, gap });
}

export function c4idEl(id, opts) {
  const t = document.createElement('template');
  t.innerHTML = c4idHTML(id, opts);
  return t.content.firstChild || document.createTextNode('');
}

const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');

// c4idSVG: the bare mark in an SVG drawing, size px tall at x, y, with the
// same behavior as any mark
export function c4idSVG(id, x, y, size = 12, { label = 'C4 ID' } = {}) {
  if (!id) return '';
  const ok = isValid(id);
  const what = ok ? `${label} ${abbrev(id)}` : 'not a valid C4 ID';
  const inner = svg(id).replace(/^<svg[^>]*>/, '').replace(/<\/svg>$/, '');
  return `<g class="c4-mark${ok ? '' : ' c4-mark-invalid'}" tabindex="0" role="button" data-c4="${esc(id)}" aria-label="${esc(what)}. Press Enter to copy."><title>${esc(id)}</title>`
    + `<svg x="${x}" y="${y}" width="${size}" height="${size}" viewBox="0 0 1600 1600" overflow="visible"><rect width="1600" height="1600" fill="transparent"/>${inner}</svg></g>`;
}

export function c4idSVGEl(id, x, y, size = 12, opts) {
  const g = document.createElementNS('http://www.w3.org/2000/svg', 'g');
  g.innerHTML = c4idSVG(id, x, y, size, opts);
  return g.firstChild || g;
}

export const installC4ID = (root = document) => install(root);
