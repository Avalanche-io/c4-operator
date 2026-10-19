// c4-mark: a C4 ID drawn as a small mark instead of 90 characters.
//
// A C4 ID (SMPTE ST 2114) is "c4" and the 512-bit SHA-512 digest of the
// content in base58, padded to 88 digits. Shown raw it is noise. This
// module draws it as the c4 logo in corner brackets, optionally followed by
// up to three glyphs: 5 x 5 mirrored grids drawn from the digest itself, so
// IDs that differ look different at a glance. Copyright 2026 Joshua Ian
// Kolden. Apache License 2.0.
//
// Three layers, use what you need:
//   pure:      decode, isValid, fold, glyphBits, tail, abbrev, svg
//   HTML:      markHTML(id, opts) for templates and server-free pages, and
//              install(root) for the behavior (hover, focus, copy, touch)
//   element:   <c4-id value="c4…" glyphs="2" tail="4" label="output view">
//              (defineElement(), or import 'c4-mark/element')
//
// The SVG is drawn on an integer grid (the viewBox is 1600 high) so this
// module and the Go package in the same repository write byte-identical
// SVG for the same inputs.

export const VERSION = '0.1.0';

const B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';

// ------------------------------------------------------------ the ID

// decode: the 64-byte digest a C4 ID names, or null when the string is not
// a C4 ID (not "c4" and 88 base58 digits, or a value over 512 bits).
export function decode(id) {
  if (typeof id !== 'string' || id.length !== 90 || !id.startsWith('c4')) return null;
  let n = 0n;
  for (let i = 2; i < 90; i++) {
    const v = B58.indexOf(id[i]);
    if (v < 0) return null;
    n = n * 58n + BigInt(v);
  }
  if (n >> 512n) return null;
  const out = new Uint8Array(64);
  for (let i = 63; i >= 0; i--) { out[i] = Number(n & 255n); n >>= 8n; }
  return out;
}

export const isValid = (id) => decode(id) !== null;

// fold: a digest's 512 bits XOR-folded into w bits (0 and 1): digest bit i,
// counting from the most significant bit of the first byte, lands in folded
// bit i mod w. Every bit counts in exactly one place.
export function fold(digest, w) {
  const out = new Array(w).fill(0);
  for (let i = 0; i < 512; i++) if ((digest[i >> 3] >> (7 - (i & 7))) & 1) out[i % w] ^= 1;
  return out;
}

// glyphBits: the bits of each of k glyphs (15 each) for an ID, or null
export function glyphBits(id, k) {
  const d = decode(id);
  if (!d) return null;
  const f = fold(d, 15 * k);
  return Array.from({ length: k }, (_, g) => f.slice(15 * g, 15 * g + 15));
}

// tail: the ID's last n characters. Text that tells IDs apart shows the
// tail, never the characters after "c4": 2^512 is about 5.07 x 58^87, so
// the first digit is one of 1 to 6 and the next few are skewed, while the
// last digits are uniform.
export const tail = (id, n = 4) => (typeof id === 'string' ? id.slice(-n) : '');
export const abbrev = (id) => (typeof id === 'string' && id.length > 16 ? `c4…${id.slice(-8)}` : id || '');

// odds: the chance that two different IDs draw the same with k glyphs
export const odds = (k) => 2 ** (15 * k);

// ------------------------------------------------------------ the SVG

const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
// a value in hundredths, written with two decimals: integer arithmetic only,
// so every implementation writes the same digits
const hundredths = (h) => `${Math.floor(h / 100)}.${String(h % 100).padStart(2, '0')}`;

// geometry, in 1/100 of the 16-unit design grid
const H = 1600, X0 = 1520, G = 1120, Y0 = 240, CELL = 224, RIGHT = 240;
// the space between glyphs: 'default' tiles, 'none' one continuous field,
// 'hairline' a thin seam. (The logo keeps its own space either way.)
export const GAPS = { default: 160, none: 0, hairline: 48 };
const gapOf = (g) => (g === 0 || g === '0' ? 0 : GAPS[g] ?? GAPS.default);
const LOGO = '<path d="M736 567A275 275 0 1 0 736 1033" fill="none" stroke="INK" stroke-width="150" stroke-linecap="round"/>'
  + '<path d="M1200 540V1060M1200 540L950 930H1300" fill="none" stroke="INK" stroke-width="150" stroke-linejoin="round" stroke-linecap="round"/>';

export function width(k, gap = 'default') {
  return k > 0 ? X0 + k * G + (k - 1) * gapOf(gap) + RIGHT : H;
}

// svg: the mark as an SVG string.
//   glyphs: 0 (the bare mark) to 3
//   size:   the height in px (an integer); omitted, 1em tall and as wide as it is
//   ink:    the color of every stroke and cell (default currentColor)
//   gap:    between glyphs: 'default', 'none' or 'hairline'
// A string that is not a C4 ID draws the bare mark with dashed brackets.
export function svg(id, { glyphs = 0, size, ink = 'currentColor', gap = 'default' } = {}) {
  const valid = isValid(id);
  const k = valid ? Math.max(0, Math.min(3, Math.floor(Number(glyphs) || 0))) : 0;
  const sp = gapOf(gap);
  const W = width(k, gap);
  const h = W / 16; // the width in hundredths of the height (W is a multiple of 16)
  const c = esc(ink);
  const wh = size ? `width="${hundredths(Math.floor(size) * h)}" height="${Math.floor(size)}"` : `width="${hundredths(h)}em" height="1em"`;
  let s = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${W} ${H}" ${wh} aria-hidden="true" focusable="false">`;
  s += `<path d="M100 500V100h400M${W - 500} 100h400v400M${W - 100} 1100v400h-400M500 1500H100v-400" fill="none" stroke="${c}" stroke-width="140"${valid ? '' : ' stroke-dasharray="160 140"'}/>`;
  s += LOGO.replaceAll('INK', c);
  if (k > 0) {
    const bits = glyphBits(id, k);
    s += `<g fill="${c}">`;
    for (let g = 0; g < k; g++) {
      const gx = X0 + g * (G + sp);
      for (let r = 0; r < 5; r++) {
        for (let col = 0; col < 3; col++) {
          if (!bits[g][r * 3 + col]) continue;
          for (const x of col === 2 ? [2] : [col, 4 - col]) s += `<rect x="${gx + x * CELL}" y="${Y0 + r * CELL}" width="${CELL}" height="${CELL}"/>`;
        }
      }
    }
    s += '</g>';
  }
  return s + '</svg>';
}

// ------------------------------------------------------------ HTML and behavior

// The smallest font size (px) at which glyphs are drawn: a 5 x 5 glyph cell
// is 1/7 of the height, so at 12 px a cell is under 2 px and the grid turns
// to noise. Below this the mark falls back to bare.
export const MIN_GLYPH_PX = 16;

// markHTML: a focusable mark for templates. Glyphs, when asked for, are
// drawn alongside a bare mark, and install() shows whichever fits the
// rendered size.
//   opts: { glyphs: 0..3, tail: characters shown after the mark, label: what
//           the ID names, for screen readers ("output view") }
export function markHTML(id, { glyphs = 0, tail: n = 0, label = 'C4 ID', gap = 'default' } = {}) {
  if (!id) return '';
  const valid = isValid(id);
  const k = valid ? Math.max(0, Math.min(3, Math.floor(Number(glyphs) || 0))) : 0;
  const what = valid ? `${label} ${abbrev(id)}` : `not a valid C4 ID: ${id.length > 24 ? id.slice(0, 12) + '…' + id.slice(-8) : id}`;
  const art = k > 0 ? `<span class="c4-mark-art c4-mark-full">${svg(id, { glyphs: k, gap })}</span><span class="c4-mark-art c4-mark-bare">${svg(id)}</span>` : svg(id);
  const t = n > 0 && valid ? `<span class="c4-mark-tail">…${esc(tail(id, n))}</span>` : '';
  return `<span class="c4-mark${valid ? '' : ' c4-mark-invalid'}${k ? ' c4-mark-glyphs' : ''}" tabindex="0" role="button" data-c4="${esc(id)}" title="${esc(id)}" aria-label="${esc(what)}. Press Enter to copy.">${art}${t}</span>`;
}

let pop = null, popFor = null, hideT = 0;
const installed = new WeakSet();

function popover() {
  if (pop && pop.isConnected) return pop;
  pop = document.createElement('div');
  pop.className = 'c4-mark-pop';
  pop.setAttribute('role', 'status');
  pop.hidden = true;
  pop.innerHTML = '<span class="c4-mark-ab"></span><button type="button" class="c4-mark-copy">Copy</button><span class="c4-mark-ok" hidden>copied</span>';
  pop.querySelector('.c4-mark-copy').addEventListener('click', (e) => { e.stopPropagation(); if (popFor) copy(popFor); });
  document.body.appendChild(pop);
  return pop;
}

function show(el, touch = false) {
  clearTimeout(hideT);
  const p = popover();
  popFor = el;
  const id = el.dataset.c4;
  const ab = p.querySelector('.c4-mark-ab');
  ab.textContent = el.classList.contains('c4-mark-invalid') ? 'not a valid C4 ID' : abbrev(id);
  ab.title = id;
  p.querySelector('.c4-mark-ok').hidden = true;
  p.classList.toggle('touch', touch);
  p.hidden = false;
  const r = el.getBoundingClientRect();
  const w = p.offsetWidth;
  const x = Math.max(6, Math.min(window.innerWidth - w - 6, r.left + r.width / 2 - w / 2));
  const below = r.bottom + 6 + p.offsetHeight < window.innerHeight;
  p.style.left = `${Math.round(x + window.scrollX)}px`;
  p.style.top = `${Math.round((below ? r.bottom + 6 : r.top - 6 - p.offsetHeight) + window.scrollY)}px`;
}

function hide(soon = false) {
  clearTimeout(hideT);
  const go = () => { if (pop) pop.hidden = true; popFor = null; };
  if (soon) hideT = setTimeout(go, 160); else go();
}

async function copy(el) {
  const id = el.dataset.c4;
  let ok = false;
  try { await navigator.clipboard.writeText(id); ok = true; } catch {
    const ta = document.createElement('textarea');
    ta.value = id; ta.setAttribute('readonly', ''); ta.style.position = 'fixed'; ta.style.opacity = '0';
    document.body.appendChild(ta); ta.select();
    try { ok = document.execCommand('copy'); } catch { ok = false; }
    ta.remove();
  }
  show(el, pop && pop.classList.contains('touch'));
  const m = pop.querySelector('.c4-mark-ok');
  m.textContent = ok ? 'copied' : 'copy failed';
  m.hidden = false;
  el.classList.add('c4-mark-copied');
  setTimeout(() => { el.classList.remove('c4-mark-copied'); m.hidden = true; }, 1400);
}

// fit: under MIN_GLYPH_PX the glyphs give way to the bare mark
export function fit(root = document) {
  for (const el of root.querySelectorAll ? root.querySelectorAll('.c4-mark-glyphs') : []) {
    const px = parseFloat(getComputedStyle(el).fontSize) || 16;
    el.classList.toggle('c4-mark-small', px < MIN_GLYPH_PX);
  }
}

// install: the behavior for every mark under root, now and later
// (delegated): hover or focus shows the ID abbreviated with the full ID in
// the tooltip; click, Enter or Space copies it; on touch a first tap shows
// it and a Copy button; Escape hides it.
export function install(root = document) {
  if (installed.has(root)) return;
  installed.add(root);
  let touched = null;
  const of = (e) => e.target && e.target.closest && e.target.closest('.c4-mark');
  root.addEventListener('pointerover', (e) => { const el = of(e); if (el && e.pointerType !== 'touch') show(el); });
  root.addEventListener('pointerout', (e) => { const el = of(e); if (el && e.pointerType !== 'touch' && !el.contains(e.relatedTarget)) hide(true); });
  root.addEventListener('focusin', (e) => { const el = of(e); if (el) show(el, touched === el); });
  root.addEventListener('focusout', (e) => { const el = of(e); if (el && !(pop && pop.contains(e.relatedTarget))) hide(true); });
  root.addEventListener('pointerdown', (e) => { touched = e.pointerType === 'touch' ? of(e) : null; });
  root.addEventListener('click', (e) => {
    const el = of(e);
    if (!el) { if (pop && !pop.hidden && !pop.contains(e.target)) hide(); return; }
    e.preventDefault();
    if (touched === el) { show(el, true); return; }
    copy(el);
  });
  root.addEventListener('keydown', (e) => {
    const el = of(e);
    if (e.key === 'Escape') { hide(); return; }
    if (el && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); copy(el); }
  });
  window.addEventListener('scroll', () => hide(), { passive: true });
  // the size check, now and whenever marks arrive
  const body = root === document ? document.body : root;
  let queued = false;
  const again = () => { if (queued) return; queued = true; requestAnimationFrame(() => { queued = false; fit(root); }); };
  fit(root);
  if (typeof MutationObserver !== 'undefined' && body) new MutationObserver(again).observe(body, { childList: true, subtree: true });
  window.addEventListener('resize', again, { passive: true });
}

// ------------------------------------------------------------ the element

// <c4-id value="c4…" glyphs="2" tail="4" label="output view">: the mark as
// a custom element. (The attribute is value, not id: an element's id is
// HTML's own, and two marks of one C4 ID would collide.)
export function defineElement(name = 'c4-id') {
  if (typeof customElements === 'undefined' || customElements.get(name)) return;
  class C4ID extends HTMLElement {
    static get observedAttributes() { return ['value', 'glyphs', 'tail', 'label', 'gap']; }
    connectedCallback() { install(document); this.draw(); }
    attributeChangedCallback() { if (this.isConnected) this.draw(); }
    draw() {
      this.innerHTML = markHTML(this.getAttribute('value') || '', {
        glyphs: Number(this.getAttribute('glyphs') || 0), tail: Number(this.getAttribute('tail') || 0),
        label: this.getAttribute('label') || 'C4 ID', gap: this.getAttribute('gap') || 'default',
      });
      fit(this);
    }
  }
  customElements.define(name, C4ID);
}
