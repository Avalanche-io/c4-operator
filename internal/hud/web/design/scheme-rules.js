// The color schemes, derived from one set of rules. tokens.css is
// generated from this file (design/gen-tokens.mjs, `make tokens`), and
// the contrast test (test/contrast.test.mjs) evaluates the same rules.
//
// Anchors: the dark ramp and the five state hues, as Joshua approved them.
// Rules, the same for every scheme:
//   1. The ramp runs from the ground to the ink. Dark is the anchor ramp;
//      light mirrors it in CIELAB lightness (L* becomes 100 - L*, the cool
//      tint kept), so g00 is the white ground, g19 the black ink, and every
//      pair of steps keeps its perceptual distance. A view that steps up
//      the ramp for emphasis steps away from the ground in either scheme.
//      1b. A step that carries a text role moves toward the ink only as
//      far as its WCAG floor (TEXT) needs.
//   2. A state hue keeps its OKLCH hue and chroma. Its lightness moves
//      toward the ink only as far as a cell needs to stand 3:1 (WCAG
//      non-text) from every surface it sits on; chroma drops only where
//      sRGB runs out. 2b. Its text form (--done-text and so on) does the
//      same against 4.5:1 on every text surface.
//   3. A pale is its state hue at PALE opacity over the ground: conceptually
//      more transparent, never darker, so it reads the same on black and on
//      white. In CSS it is color-mix(in srgb, hue PALE, ground), which is
//      exactly what an alpha of PALE composites to.

import { hex2lab, lab2rgb, rgb2hex, hex2oklch, oklch2hex, hex2rgb, contrast, luminance } from './color.js';

// the anchor ramp: 20 steps even in L*, packed twice as densely below L*20
export const RAMP = [
  ['#020505', 'void: the room beyond the screen'],
  ['#06090a', 'deep: wall background, off-sheet margins'],
  ['#0b0d0e', 'field: the sheet'],
  ['#0f1111', 'well: recessed data wells'],
  ['#121415', 'plate: panels, the collar'],
  ['#161718', 'plate raised: hover, a container block'],
  ['#191b1b', 'rule 0: faintest graticule'],
  ['#1c1e1e', 'rule 1: minor graticule'],
  ['#202222', 'rule 2: major graticule, gutters'],
  ['#252727', 'wait: waiting units, empty gauges'],
  ['#2b2d2d', 'rule 3: neatlines, frames'],
  ['#323434', 'ghost: expected children, pencil marks'],
  ['#3b3d3d', 'not yet evaluated'],
  ['#464848', 'faint: IDs as texture, tertiary labels'],
  ['#555758', 'axis labels'],
  ['#696b6b', 'secondary labels'],
  ['#7f8282', 'quiet values, pencil numbers'],
  ['#999c9c', 'values'],
  ['#b7b9b9', 'ink: primary labels and traces'],
  ['#dbdddd', 'paper: the strongest mark, selection'],
];

// the state hues at full strength, as approved on the dark ground
export const HUES = {
  done: ['#3dc86a', 'committed: folded into the output and verified'],
  run: ['#4b93ff', 'rendering now, at the top edge of the green'],
  temp: ['#ff9433', 'temporary error the farm is handling: a retry running'],
  fail: ['#ee4136', 'needs a person: failed after retries, stuck, approval, blocked'],
  risk: ['#b269a3', 'deadline: will miss (hatched), missed (solid)'],
};
export const PALE_NOTES = {
  done: 'done, output still moving: capture, upload, verify (egress)',
  run: 'preflight: staging inputs, cache fetch, image, license',
  temp: 'backoff: a retry scheduled',
  fail: 'acknowledged or assigned, or accepted',
  risk: 'deadline: at risk, projected late (dashed)',
};
// the hand-computed dim tints approved before the opacity rule (OKLCH
// chroma x0.55, lightness x0.62), kept to report how far the rule moved them
export const APPROVED_PALES = { done: '#276639', run: '#294b80', temp: '#824e22', fail: '#77251e', risk: '#593652' };

// rule 3: a pale is the hue at this opacity over the ground. 48% is the
// opacity closest to the approved dim tints on the dark field.
export const PALE = 0.48;
// rule 2: a full-strength cell against every surface it sits on
export const NON_TEXT = 3;
// rule 2b: a state hue set as text, against every text surface
export const TEXT_HUE = 4.5;

// role names for steps of the ramp
export const ROLES = {
  void: 0, deep: 1, field: 2, well: 3, plate: 4, 'plate-2': 5, 'rule-0': 6, 'rule-1': 7, 'rule-2': 8, neat: 10,
  ink: 18, 'ink-2': 17, 'ink-3': 15, 'ink-4': 14, pencil: 16, paper: 19,
};
// the semantic layer: the ground pales sit over, the ink marks are made
// with, the line frames are drawn in
export const SEMANTIC = { ground: 'field', line: 'neat' };
// surfaces a state cell sits on, on the desk and on the wall (the wall
// drops three steps toward the void: its field is g01, well g02, plate g03)
export const CELL_SURFACES = [1, 2, 3, 4];
export const WALL = { field: 1, well: 2, plate: 3 };

// The callout plates: a violet at 15% over the void (rgb 22 17 33 on the
// dark void), so on white they are the same violet, faint.
export const NOTE = { hue: '#8755c0', alpha: 0.15 };

export const SCHEMES = ['dark', 'light'];

// Text roles against the ground and the plates (rule 1b). Body text needs
// 4.5:1. ink-3 is the secondary label (collar labels, subs, units,
// captions), held to 3:1. ink-4 (axis ticks, edge references) is texture:
// reported, not held.
export const TEXT = { ink: 4.5, 'ink-2': 4.5, pencil: 4.5, 'ink-3': 3, 'ink-4': 0 };
export const TEXT_ON = ['deep', 'field', 'well', 'plate', 'plate-2'];

// rule 1: the mirror; 1b: a step that carries text moves toward the ink,
// in steps of 0.1 L*, only as far as its floor on every text surface needs
export function ramp(scheme) {
  const g = scheme === 'dark' ? RAMP.map(([h]) => h)
    : RAMP.map(([h]) => { const [L, a, b] = hex2lab(h); return rgb2hex(lab2rgb([100 - L, a, b])); });
  const on = TEXT_ON.map((r) => g[ROLES[r]]);
  for (const [role, min] of Object.entries(TEXT)) {
    const i = ROLES[role];
    let [L, a, b] = hex2lab(g[i]);
    const dir = luminance(on[0]) > luminance(g[i]) ? -1 : 1;
    for (let n = 0; n < 200 && on.some((s) => contrast(g[i], s) < min); n++) { L += dir * 0.1; g[i] = rgb2hex(lab2rgb([L, a, b])); }
  }
  return g;
}

// rule 2
export function stateHue(anchor, surfaces, min = NON_TEXT) {
  const ok = (x) => surfaces.every((s) => contrast(x, s) >= min);
  if (ok(anchor)) return anchor;
  let [L, C, h] = hex2oklch(anchor);
  const dir = luminance(surfaces[0]) > luminance(anchor) ? -1 : 1; // toward the ink
  let hex = anchor;
  for (let i = 0; i < 1000 && !ok(hex); i++) { L += dir * 0.001; hex = oklch2hex([L, C, h]); }
  return hex;
}

// rule 3: top at alpha a over an opaque ground, composited as the browser
// does (sRGB, gamma-encoded)
export function over(top, ground, a) {
  const t = hex2rgb(top), g = hex2rgb(ground);
  return rgb2hex(t.map((v, i) => a * v + (1 - a) * g[i]));
}

export function derive(scheme) {
  const g = ramp(scheme);
  const role = Object.fromEntries(Object.entries(ROLES).map(([k, i]) => [k, g[i]]));
  const surfaces = CELL_SURFACES.map((i) => g[i]);
  const hues = {}, texts = {}, pales = {}, wallPales = {};
  const textOn = TEXT_ON.map((r) => role[r]);
  for (const [k, [anchor]] of Object.entries(HUES)) {
    hues[k] = stateHue(anchor, surfaces);
    texts[k] = stateHue(hues[k], textOn, TEXT_HUE);
    pales[k] = over(hues[k], role[SEMANTIC.ground], PALE);
    wallPales[k] = over(hues[k], g[WALL.field], PALE);
  }
  return { scheme, g, role, hues, texts, pales, wallPales, ground: role[SEMANTIC.ground], wallGround: g[WALL.field], note: over(NOTE.hue, g[0], NOTE.alpha) };
}

// css is the generated part of tokens.css.
export const BEGIN = '/* ---- begin generated: design/gen-tokens.mjs from design/scheme-rules.js; edit the rules, then `make tokens` ---- */';
export const END = '/* ---- end generated ---- */';
export function css() {
  const D = derive('dark'), L = derive('light');
  const pad = (s, n) => s + ' '.repeat(Math.max(1, n - s.length));
  const block = (d, notes, indent = '  ') => {
    const out = [`${indent}color-scheme: ${d.scheme};`];
    d.g.forEach((h, i) => out.push(`${indent}${pad(`--g${String(i).padStart(2, '0')}: ${h};`, 15)}${notes ? `/* ${RAMP[i][1]} */` : ''}`.trimEnd()));
    for (const k of Object.keys(HUES)) out.push(`${indent}${pad(`--${k}: ${d.hues[k]};`, 15)}${notes ? `/* ${HUES[k][1]} */` : ''}`.trimEnd());
    for (const k of Object.keys(HUES)) out.push(`${indent}${pad(`--${k}-text: ${d.texts[k]};`, 20)}${notes && k === 'done' ? '/* the hue set as text: 4.5:1 on every text surface */' : ''}`.trimEnd());
    return out.join('\n');
  };
  const roles = Object.entries(ROLES).map(([k, i]) => `--${k}: var(--g${String(i).padStart(2, '0')});`);
  const lines = [
    BEGIN,
    '/* The schemes: the ramp and the state hues at full strength. Dark is the',
    '   default; light mirrors the ramp in L* and moves a hue toward the ink',
    '   only where a cell needs 3:1. Pages without data-scheme follow the OS. */',
    ':root, [data-scheme="dark"] {',
    block(D, true),
    '}',
    '[data-scheme="light"] {',
    block(L, false),
    '}',
    '@media (prefers-color-scheme: light) {',
    '  :root:not([data-scheme]) {',
    block(L, false, '    '),
    '  }',
    '}',
    '',
    '/* Roles and semantic tokens, in every scheme (and on any element that sets',
    '   data-scheme, so a page can show both). Value carries depth and',
    '   hierarchy; color is reserved for state. */',
    ':root, [data-scheme] {',
    '  ' + roles.join('\n  '),
    `  --ground: var(--${SEMANTIC.ground});   /* what a pale is transparent over */`,
    `  --line: var(--${SEMANTIC.line});`,
    '  /* A pale is its state hue, more transparent: the same state with',
    '     mitigating context worth showing (README). */',
    ...Object.keys(HUES).map((k) => `  ${pad(`--${k}-pale: color-mix(in srgb, var(--${k}) ${Math.round(PALE * 100)}%, var(--ground));`, 71)}/* ${PALE_NOTES[k]} */`),
    '  /* retired names, kept as aliases for older views and lenses */',
    '  --done-settled: var(--done);',
    '  --run-body: var(--run-pale);',
    '  --temp-body: var(--temp-pale);',
    `  --note-tint: color-mix(in srgb, ${NOTE.hue} ${Math.round(NOTE.alpha * 100)}%, var(--void));   /* the callout plates' violet */`,
    '}',
    END,
  ];
  return lines.join('\n');
}

// palette is the data the palette page (docs/intro/palette/state-colors.html)
// draws both schemes from.
export function palette() {
  const out = {};
  for (const s of SCHEMES) {
    const d = derive(s);
    out[s] = { g: d.g, hues: d.hues, texts: d.texts, pales: d.pales, wallPales: d.wallPales, note: d.note };
  }
  return out;
}
