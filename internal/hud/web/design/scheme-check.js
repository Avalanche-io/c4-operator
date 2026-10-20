// The contrast checks on the schemes (scheme-rules.js): WCAG for text and
// state cells, CIEDE2000 between each pale and its full strength and the
// ground, under normal vision and the three simulations. Shared by the
// contrast test and `node design/gen-tokens.mjs --report`.

import { contrast, deltaE, simulate } from './color.js';
import { derive, SCHEMES, HUES, ROLES, NON_TEXT, TEXT_HUE, CELL_SURFACES, APPROVED_PALES, RAMP, TEXT, TEXT_ON } from './scheme-rules.js';

// The rule: every pale and full pair at least ΔE 18
// under the three simulations, every pale at least 11 from the field and
// the wall's field. On white the light scheme cannot keep all of 18 at one
// opacity: the floor it holds is PALE_FULL_FLOOR, and each phase also has a
// shape (README).
export const PALE_FULL = 18;
export const PALE_FULL_FLOOR = { dark: 18, light: 14 };
export const PALE_GROUND = 11;
export const VISIONS = ['normal', 'protan', 'deutan', 'tritan'];

const r2 = (x) => Math.round(x * 100) / 100;
const r1 = (x) => Math.round(x * 10) / 10;

export function checks() {
  const rows = [];
  for (const s of SCHEMES) {
    const d = derive(s);
    for (const [role, min] of Object.entries(TEXT)) {
      for (const on of TEXT_ON) {
        const v = contrast(d.role[role], d.role[on]);
        rows.push({ scheme: s, kind: 'text', what: `${role} on ${on}`, value: r2(v), min, ok: v >= min });
      }
    }
    for (const k of Object.keys(HUES)) {
      for (const on of TEXT_ON) {
        const v = contrast(d.texts[k], d.role[on]);
        rows.push({ scheme: s, kind: 'text', what: `${k}-text on ${on}`, value: r2(v), min: TEXT_HUE, ok: v >= TEXT_HUE });
      }
      for (const i of CELL_SURFACES) {
        const v = contrast(d.hues[k], d.g[i]);
        rows.push({ scheme: s, kind: 'cell', what: `${k} on g${String(i).padStart(2, '0')}`, value: r2(v), min: NON_TEXT, ok: v >= NON_TEXT });
      }
      for (const vis of VISIONS) {
        const v = deltaE(simulate(d.hues[k], vis), simulate(d.pales[k], vis));
        rows.push({ scheme: s, kind: 'pale-full', what: `${k} pale vs full, ${vis}`, value: r1(v), min: PALE_FULL_FLOOR[s], target: PALE_FULL, ok: v >= PALE_FULL_FLOOR[s] });
        for (const [gname, ground, pale] of [['field', d.ground, d.pales[k]], ['wall field', d.wallGround, d.wallPales[k]]]) {
          const w = deltaE(simulate(ground, vis), simulate(pale, vis));
          rows.push({ scheme: s, kind: 'pale-ground', what: `${k} pale vs ${gname}, ${vis}`, value: r1(w), min: PALE_GROUND, ok: w >= PALE_GROUND });
        }
      }
    }
  }
  return rows;
}

// shifts: how far the dark scheme's pales moved from the approved tints,
// and the light scheme's full hues from the dark ones
export function shifts() {
  const D = derive('dark'), L = derive('light');
  return {
    darkPales: Object.fromEntries(Object.keys(HUES).map((k) => [k, { approved: APPROVED_PALES[k], now: D.pales[k], dE: r2(deltaE(APPROVED_PALES[k], D.pales[k])) }])),
    lightHues: Object.fromEntries(Object.keys(HUES).map((k) => [k, { dark: D.hues[k], light: L.hues[k], dE: r2(deltaE(D.hues[k], L.hues[k])), lightText: L.texts[k] }])),
    darkTextsUnchanged: Object.keys(HUES).every((k) => D.texts[k] === HUES[k][0]),
    darkRampUnchanged: D.g.every((h, i) => h === RAMP[i][0]),
    darkHuesUnchanged: Object.keys(HUES).every((k) => D.hues[k] === HUES[k][0]),
  };
}

export { ROLES, TEXT, TEXT_ON };
