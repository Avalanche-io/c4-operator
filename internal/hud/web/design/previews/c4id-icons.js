// The C4 ID mark's review page (c4id-icons.html), straight from the
// vendored c4-mark package: the bare and expanded marks, the gap between
// glyphs, the bits and the odds, why text shows the tail, a ten-ID
// comparison, and the behavior (the HUD's marks and <c4-id>).

import { VERSION, svg, markHTML, install, abbrev, odds, defineElement, MIN_GLYPH_PX } from '../vendor/c4-mark/c4mark.js';

const B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';
// a C4 ID from a made-up 64-byte digest
function idOf(bytes) {
  let n = 0n;
  for (const b of bytes) n = n * 256n + BigInt(b);
  let t = '';
  while (n > 0n) { t = B58[Number(n % 58n)] + t; n /= 58n; }
  return 'c4' + t.padStart(88, '1');
}
function digest(seed) {
  let x = (seed * 2654435761) >>> 0;
  return Uint8Array.from({ length: 64 }, () => { x ^= x << 13; x >>>= 0; x ^= x >>> 17; x ^= x << 5; x >>>= 0; return x & 255; });
}
function tweak(id, positions) {
  const a = id.split('');
  for (const p of positions) a[p] = B58[(B58.indexOf(a[p]) + 7) % 58];
  return a.join('');
}
const ID = idOf(digest(1036));
const EMPTY = 'c459dsjfscH38cYeXXYogktxf4Cd9ibshE3BHUo6a58hBXmRQdZrAkZzsWcbWtDg5oQstpDuni4Hirj75GEmTc1sFT'; // the empty input's ID

document.getElementById('ver').textContent = `c4-mark ${VERSION}`;

// ------------------------------------------------------------ sizes

function sizes(scheme) {
  const row = (name, opts) => `<tr><td class="n">${name}</td>${[12, 16, 24].map((px) => `<td style="font-size:${px}px">${markHTML(ID, opts)}</td>`).join('')}
    <td class="inl">output view ${markHTML(ID, { ...opts, tail: 4, label: 'output view' })} added</td></tr>`;
  return `<section class="panel" data-scheme="${scheme}"><h3>${scheme}</h3><table class="c">
    <tr><th></th><th>12 px</th><th>16 px</th><th>24 px</th><th>inline, 13 px, with a tail</th></tr>
    ${row('bare', {})}${row('1 glyph', { glyphs: 1, gap: 'none' })}${row('2 glyphs', { glyphs: 2, gap: 'none' })}${row('3 glyphs', { glyphs: 3, gap: 'none' })}</table>
    <p>Below ${MIN_GLYPH_PX} px a 5 × 5 cell is under 2.3 px and the grid turns to noise, so the glyphs give way to the bare mark (the 12 px and 13 px columns). Where the size allows, they are drawn.</p></section>`;
}

// ------------------------------------------------------------ gaps

function gaps(scheme) {
  const cell = (gap, k, px) => `<td style="font-size:${px}px">${svg(ID, { glyphs: k, gap })}</td>`;
  const row = (gap, name) => `<tr><td class="n">${name}</td>${[2, 3].map((k) => [16, 24, 48].map((px) => cell(gap, k, px)).join('')).join('')}</tr>`;
  return `<section class="panel" data-scheme="${scheme}"><h3>${scheme}</h3><table class="c">
    <tr><th></th><th>2 glyphs, 16 px</th><th>24</th><th>48</th><th>3 glyphs, 16 px</th><th>24</th><th>48</th></tr>
    ${row('default', 'tiles (the package default)')}${row('none', 'none: one field')}${row('hairline', 'hairline seam')}</table></section>`;
}

const gapNote = `<section class="panel"><h3>Which reads best</h3>
  <p><b>None, one continuous field.</b> Without a gap the glyphs read as one fingerprint, a 5 × 10 or 5 × 15 block in the brackets, which is what they are: one picture of one digest. Tiles read as two or three separate things, and a reader starts matching tiles instead of IDs. Joining them does not create false symmetry across the seam: each glyph is mirrored about its own centre, so the field repeats a left-right rhythm every five columns, and that rhythm marks where one glyph ends without a line. A hairline seam is under a pixel at 16 px and reads as dirt. So the package keeps tiles as its default, safe for any use, and the HUD's expanded mark uses <b>none</b>.</p></section>`;

// ------------------------------------------------------------ bits

const bits = `<p>A C4 ID is <b>c4</b> and the SHA-512 digest of the content, 512 bits, in base58. The glyphs show the digest itself: the package decodes the ID back to its 512 bits and folds them. With k glyphs, digest bit i is XORed into bit i mod 15k, and glyph g shows bits 15g to 15g + 14 as a 5 × 5 grid, mirrored left to right. Every bit of the digest counts in exactly one cell, so a difference anywhere in an ID changes the picture. The decoder is tested against the c4 library's own vectors; a string that does not decode is not an ID, and draws with dashed brackets (${markHTML('c4thisIsNotARealIDxx')}).</p>
  <table class="odds"><tr><th>glyphs</th><th>bits</th><th>two different IDs look the same</th><th>any two of ten on screen</th></tr>
  ${[1, 2, 3].map((k) => `<tr><td>${k}</td><td>${15 * k}</td><td>1 in ${odds(k).toLocaleString('en-US')}</td><td>about 1 in ${Math.round(odds(k) / 45).toLocaleString('en-US')}</td></tr>`).join('')}</table>
  <p>The odds are exact because SHA-512 bits are uniform.</p>`;

// ------------------------------------------------------------ text

const text = `<p>Where an ID shows as text, beside the mark or in the hover, it shows its <b>last</b> characters: <span class="mono">${abbrev(ID)}</span>, and beside the mark <span class="mono">…${ID.slice(-4)}</span>. An ID is the 512-bit digest in base58 padded to 88 digits, and 2<sup>512</sup> is about 5.07 × 58<sup>87</sup>. So the first digit after <b>c4</b> is one of <b>1</b> to <b>6</b> (one ID in five starts <b>c41</b>), about 1.9 bits where a uniform digit carries 5.86, and the next few are skewed. The last digits are uniform: four carry about 23.4 bits, the four after <b>c4</b> about 19.5. A head-and-tail form that skips the low-entropy head was the other choice; it is longer for little gain and still invites reading from the left.</p>`;

// ------------------------------------------------------------ the comparison

const XA = idOf(digest(101)), XB = idOf(digest(202)), XC = idOf(digest(303)), XD = idOf(digest(404)), XE = idOf(digest(505));
const ROWS = [
  [XA, 'A'], [XB, 'B'], [XA, 'A again: the same'], [XC, 'C'], [tweak(XC, [86]), 'C with one character changed near the end'],
  [XD, 'D'], [tweak(XD, [41, 42]), 'D with two characters changed in the middle'], [XE, 'E'], [XE.slice(0, 14) + idOf(digest(606)).slice(14), 'E’s first 12 characters, then different'], [XB, 'B again: the same'],
];
function compare(scheme) {
  const head = '<tr><th></th><th>head: 4 after c4</th><th>tail: last 4</th><th>bare</th><th>1 glyph</th><th>2 glyphs</th><th>3 glyphs</th><th>what is true</th></tr>';
  const g = (id, k) => svg(id, { glyphs: k, gap: 'none' });
  const body = ROWS.map(([id, what], i) => `<tr><td>${i + 1}</td><td>${id.slice(2, 6)}</td><td>…${id.slice(-4)}</td><td class="s20">${svg(id)}</td>`
    + `<td class="s20">${g(id, 1)}</td><td class="s20">${g(id, 2)}</td><td class="s20">${g(id, 3)}</td><td class="truth">${what}</td></tr>`).join('');
  return `<section class="panel" data-scheme="${scheme}"><h3>${scheme} · 20 px, no gap</h3><table class="cmp">${head}${body}</table></section>`;
}

// ------------------------------------------------------------ where, and the demo

const where = `<p>The HUD's marks take a density: <b>bare</b> (the mark alone, the default everywhere) or <b>expanded</b> (the mark and its glyphs, one continuous field). Hover, focus and copy work the same in both, and below ${MIN_GLYPH_PX} px expanded falls back to bare.</p>
  <ul><li><b>Proposed expanded by default where the point is checking that IDs match:</b> comparing outputs (a job's output against the view a downstream job read; two versions of a shot), the verification and derivation views, the handoff report of what a delivery carries, and the book. Two glyphs there (1 in 1.07 billion), three in the verification views.</li>
  <li><b>Bare everywhere else:</b> the title block, unit cards, the graph's edges, the queue, the CI and ML views.</li></ul>`;

defineElement();
const demo = `<section class="panel" data-scheme="dark"><h3>dark · the HUD's marks</h3>
  <p>Hover one: ${markHTML(ID, { label: 'output view' })} ${markHTML(ID, { tail: 4 })} <span style="font-size:20px">${markHTML(ID, { glyphs: 2, gap: 'none' })}</span></p>
  <p>Press <span class="kbd">Tab</span> to focus a mark, then <span class="kbd">Enter</span> or <span class="kbd">Space</span> to copy. <span class="kbd">Esc</span> hides the ID. Click copies and says “copied”; on a phone, a tap shows the ID and a Copy button.</p>
  <p>Paste here to check what was copied:</p><textarea aria-label="Paste a copied ID here"></textarea></section>
  <section class="panel" data-scheme="light"><h3>light · the &lt;c4-id&gt; element</h3>
  <p>&lt;c4-id value="…"&gt;: <c4-id value="${EMPTY}" label="the empty input"></c4-id> &nbsp; glyphs="3" tail="4" gap="none": <span style="font-size:20px"><c4-id value="${EMPTY}" glyphs="3" tail="4" gap="none"></c4-id></span></p>
  <p>Not an ID: <c4-id value="c4-hello"></c4-id></p>
  <p>The full ID of the empty input: <span class="mono" style="font-size:11px;word-break:break-all;color:var(--ink-3)">${EMPTY}</span></p></section>`;

document.getElementById('sizes').innerHTML = sizes('dark') + sizes('light');
document.getElementById('gaps').innerHTML = gaps('dark') + gaps('light') + gapNote;
document.getElementById('bits').innerHTML = bits;
document.getElementById('text').innerHTML = text;
document.getElementById('compare').innerHTML = compare('dark') + compare('light');
document.getElementById('where').innerHTML = where;
document.getElementById('demo').innerHTML = demo;
install(document);
