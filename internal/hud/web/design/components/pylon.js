// PYLON: the glance gauge. Read in half a second: how full, how much is
// moving, how soon. A pro reads the rest: slack, the p50 to p90 band, the
// deadline, slots needed against slots the job can get.
//
// The column fills green bottom-up for committed work (by slot-seconds where
// the estimate exists, else by units, and says which); the share shown never
// goes down (core.js steady). Above it the work in flight, the bright blue
// band of rendering, at the frontier of the green. An
// orange notch on the edge is the share retrying; red from the top is
// failed, which needs a person; orchid is waiting work projected after the
// deadline: pale with a dashed edge at risk, hatched when it will miss at
// any capacity, solid once the deadline has passed. A readout rides the fill
// level. On the right, a square-root time tape: the p50 tick and p90 bar
// (estimates), the deadline bug; both turn orchid when the p90 lands after
// the deadline, and the slack is the gap.
// Under it, the slot scale: running slots (blue), slots needed (chevron),
// slots the job can get (bracket).
//
// Modes: full (scales and readouts), compact (readouts), mini (no text: a
// list-row icon, a wall rail cell).

import { tok, esc } from './core.js';
import * as f from './fmt.js';
import { register, tween, lerp, ms } from './live.js';

let seq = 0;

export function pylonSVG(g, o = {}) {
  const T = tok();
  o = Object.assign({ w: 220, h: 360, mode: 'full', title: '' }, o);
  const id = o.id || 'py' + (++seq);
  const W = o.w, H = o.h, full = o.mode === 'full', mini = o.mode === 'mini';
  const fs = Math.max(8, Math.round(H / 42));
  const pad = mini ? 1 : Math.round(H * 0.04);
  const top = pad + (o.title ? fs * 3 : 0) + (mini ? 0 : fs);
  const bot = H - pad - (full ? fs * 4.5 : mini ? 0 : fs * 1.6);
  const ch = Math.max(4, bot - top);
  const colX = mini ? 0 : full ? Math.round(W * 0.12) : Math.round(W * 0.04);
  const colW = mini ? Math.round(W * 0.55) : Math.round(W * (full ? 0.2 : 0.3));
  const tx = mini ? W - 2 : W - Math.round(W * (full ? 0.18 : 0.08));
  const y = (p) => bot - ch * Math.max(0, Math.min(1, p));
  const late = Number.isFinite(g.slack) && g.slack < 0;
  let s = `<defs><pattern id="${id}-rk" width="5" height="5" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><rect width="5" height="5" fill="${T.g[9]}"/><rect width="${g.feasible === false ? 2.2 : 1.3}" height="5" fill="${T.risk}"/></pattern>`
    + `<pattern id="${id}-un" width="3" height="3" patternUnits="userSpaceOnUse"><rect width="3" height="3" fill="${T.g[9]}"/><rect width="1" height="1" fill="${T.g[13]}"/></pattern></defs>`;
  if (o.title) s += `<text x="${colX}" y="${pad + fs * 1.4}" fill="${T.ink}" font-size="${fs * 1.2}" font-family="${T.sans}" font-weight="600" letter-spacing=".08em">${esc(o.title)}</text>`;
  // column and its inner graticule
  s += `<rect x="${colX}" y="${top}" width="${colW}" height="${ch}" fill="${T.g[9]}" fill-opacity=".5"/>`;
  if (!mini) for (let k = 1; k < 20; k++) s += `<line x1="${colX}" x2="${colX + colW}" y1="${y(k / 20)}" y2="${y(k / 20)}" stroke="${T.g[k % 5 ? 7 : 8]}"/>`;
  if (!mini) for (let k = 0; k <= 20; k++) {
    const yy = y(k / 20), major = k % 5 === 0;
    s += `<line x1="${colX - (major ? 8 : 4)}" x2="${colX - 1}" y1="${yy}" y2="${yy}" stroke="${T.g[major ? 10 : 8]}"/>`;
    if (major && full) s += `<text x="${colX - 11}" y="${yy + fs * 0.32}" fill="${T.g[13]}" font-size="${fs * 0.75}" text-anchor="end" font-family="${T.mono}">${k * 5}</text>`;
  }
  // failed from the top (needs a person), then waiting work projected after
  // the deadline: pale with a dashed edge at risk, hatched when it will miss
  // at any capacity, solid once the deadline has passed
  if (g.fail > 0) s += `<rect x="${colX}" y="${top}" width="${colW}" height="${Math.max(2, ch * g.fail)}" fill="${T.fail}"/>`;
  if (g.risk > 0) {
    const ry = top + ch * (g.fail || 0), rh = Math.max(0, ch * Math.min(g.risk, 1 - g.done - g.run - (g.fail || 0)));
    const passed = g.deadline && g.deadline < f.now();
    if (passed) s += `<rect x="${colX}" y="${ry}" width="${colW}" height="${rh}" fill="${T.risk}"/>`;
    else if (g.feasible === false) s += `<rect x="${colX}" y="${ry}" width="${colW}" height="${rh}" fill="url(#${id}-rk)"/>`;
    else s += `<rect x="${colX}" y="${ry}" width="${colW}" height="${rh}" fill="${T.riskPale}"/><rect x="${colX + 0.75}" y="${ry + 0.75}" width="${colW - 1.5}" height="${Math.max(0, rh - 1.5)}" fill="none" stroke="${T.risk}" stroke-width="1.5" stroke-dasharray="3 2"/>`;
  }
  // committed green, then the work in flight: the bright blue band of
  // rendering at the top of the green
  const r0 = g.done, r1 = Math.min(1, g.done + g.run);
  s += `<rect x="${colX}" y="${y(r0)}" width="${colW}" height="${ch * r0}" fill="${T.done}"/>`;
  if (g.run > 0) {
    s += `<rect x="${colX}" y="${y(r1)}" width="${colW}" height="${ch * (r1 - r0)}" fill="${T.run}"/>`;
  }
  if (g.retry > 0) s += `<rect x="${colX + colW - 4}" y="${y(r1)}" width="4" height="${Math.max(3, ch * g.retry)}" fill="${T.temp}"/>`;
  s += `<rect x="${colX + 0.5}" y="${top + 0.5}" width="${colW - 1}" height="${ch - 1}" fill="none" stroke="${T.g[10]}"/>`;
  // readout riding the fill level
  if (!mini) {
    const big = Math.round(Math.min(H * 0.11, W * 0.16));
    const wx = colX + colW + Math.round(W * 0.04), ww = big * 2.05, wh = big * 1.08;
    const wy = Math.max(top + wh / 2, Math.min(bot - wh / 2, y(r0)));
    s += `<path d="M${colX + colW + 1} ${y(r0)} L${wx} ${wy}" stroke="${T.done}" stroke-width="1.5"/>`;
    s += `<rect x="${wx}" y="${wy - wh / 2}" width="${ww}" height="${wh}" fill="${T.field}" stroke="${T.g[15]}"/>`;
    s += `<text x="${wx + ww - big * 0.42}" y="${wy + big * 0.36}" fill="${T.paper}" font-size="${big}" text-anchor="end" font-family="${T.sans}" font-weight="300">${Math.round(r0 * 100)}</text>`;
    s += `<text x="${wx + ww - big * 0.1}" y="${wy + big * 0.36}" fill="${T.ink3}" font-size="${big * 0.38}" text-anchor="end" font-family="${T.mono}">%</text>`;
    if (g.basis === 'units' && full) s += `<text x="${wx}" y="${wy + wh / 2 + fs}" fill="${T.pencil}" font-size="${fs * 0.75}" font-family="${T.mono}">by units</text>`;
    // the running share of units: "10% in parallel", the only blue text
    const pf = Math.max(fs * 1.3, big * 0.3);
    const py = Math.min(y(r1), wy - wh / 2 - fs * 0.6);
    if (g.par > 0) {
      s += `<rect x="${wx}" y="${py - pf * 0.78}" width="${Math.max(1.5, pf * 0.1)}" height="${pf * 0.8}" fill="${T.run}"/><rect x="${wx + pf * 0.24}" y="${py - pf * 0.78}" width="${Math.max(1.5, pf * 0.1)}" height="${pf * 0.8}" fill="${T.run}"/>`;
      s += `<text x="${wx + pf * 0.55}" y="${py}" fill="${T.run}" font-size="${pf}" font-family="${T.mono}">${Math.max(1, Math.round(g.par * 100))}%</text>`;
    }
  }
  // time tape: now at the bottom, later is up; square-root scale
  const now = f.now();
  const dl = g.deadline ? (g.deadline - now) / 1000 : 0;
  const p90 = Number.isFinite(g.p90) ? Math.max(0, g.p90) : 0, eta = Number.isFinite(g.eta) ? Math.max(0, g.eta) : 0;
  const horizon = Math.max(p90, dl, 600) * 1.2;
  const ty = (sec) => bot - ch * Math.sqrt(Math.max(0, Math.min(sec, horizon)) / horizon);
  s += `<line x1="${tx}" x2="${tx}" y1="${top}" y2="${bot}" stroke="${T.g[10]}"/>`;
  if (!mini) for (const [sec, l] of [[60, '1m'], [300, '5m'], [900, '15m'], [3600, '1h'], [14400, '4h'], [43200, '12h'], [86400, '1d'], [259200, '3d']]) {
    if (sec > horizon) break;
    s += `<line x1="${tx}" x2="${tx + 4}" y1="${ty(sec)}" y2="${ty(sec)}" stroke="${T.g[10]}"/>`;
    if (full) s += `<text x="${tx + 7}" y="${ty(sec) + fs * 0.3}" fill="${T.g[13]}" font-size="${fs * 0.75}" font-family="${T.mono}">${l}</text>`;
  }
  const etaC = late ? T.risk : T.pencil;
  if (g.phase !== 'Succeeded') {
    s += `<rect x="${tx - 5}" y="${ty(p90)}" width="5" height="${Math.max(2, ty(eta) - ty(p90))}" fill="${etaC}"/>`;
    s += `<line x1="${tx - (mini ? 6 : 14)}" x2="${tx}" y1="${ty(eta)}" y2="${ty(eta)}" stroke="${etaC}" stroke-width="2"/>`;
  }
  if (g.deadline) {
    const dy = ty(dl);
    s += `<line x1="${tx + 1.5}" x2="${tx + 1.5}" y1="${Math.min(dy, ty(p90))}" y2="${Math.max(dy, ty(p90))}" stroke="${late ? T.risk : T.g[13]}" stroke-width="3" stroke-dasharray="${late ? '2 2' : 'none'}"/>`;
    s += `<path d="M${tx} ${dy} l${mini ? 5 : 9} -${mini ? 4 : 6} v${mini ? 8 : 12} z" fill="${late ? T.risk : T.ink}"/>`;
    if (!mini) s += `<text x="${tx + 11}" y="${dy - 8}" fill="${late ? T.risk : T.ink}" font-size="${fs * 0.9}" font-family="${T.mono}">${f.clock(g.deadline, false)}</text>`;
    // the UTC complication, small, on its own line above the deadline time
    if (!mini && f.utc(g.deadline) && dy - 8 - fs * 1.6 > 0) s += `<text x="${tx + 11}" y="${(dy - 8 - fs * 0.95).toFixed(1)}" fill="${late ? T.risk : T.g[14]}"${late ? ' fill-opacity="0.7"' : ''} font-size="${(fs * 0.68).toFixed(1)}" font-family="${T.mono}">${f.utc(g.deadline)}</text>`;
    if (full && Number.isFinite(g.slack)) s += `<text x="${tx + 11}" y="${dy + fs * 0.9}" fill="${late ? T.risk : T.pencil}" font-size="${fs * 0.85}" font-family="${T.mono}">slack ${f.signed(g.slack)}</text>`;
  }
  if (!mini && g.phase !== 'Succeeded' && Number.isFinite(g.eta)) {
    s += `<text x="${tx - 18}" y="${ty(eta) + fs * 0.45}" fill="${T.ink}" font-size="${fs * (full ? 1.7 : 1.3)}" text-anchor="end" font-family="${T.sans}" font-weight="500">${f.approx(eta)}</text>`;
    if (full) s += `<text x="${tx - 18}" y="${ty(eta) + fs * 1.7}" fill="${T.pencil}" font-size="${fs * 0.8}" text-anchor="end" font-family="${T.mono}">p90 ${f.dur(p90)}</text>`;
  }
  // slot scale
  if (full) {
    const sy = H - pad - fs * 2.2, sx = colX, sw = tx - colX;
    const smax = Math.max(g.req, g.att, g.slots, 1) * 1.12, X = (v) => sx + sw * (v / smax);
    s += `<line x1="${sx}" x2="${sx + sw}" y1="${sy}" y2="${sy}" stroke="${T.g[8]}"/>`;
    for (let k = 0; k < Math.min(g.slots, 400); k++) s += `<rect x="${X(k) + 0.5}" y="${sy - 6}" width="${Math.max(1, X(1) - X(0) - 1)}" height="6" fill="${T.run}" fill-opacity=".85"/>`;
    s += `<path d="M${X(g.att)} ${sy - 10} v15 h-4" fill="none" stroke="${T.ink}"/>`;
    s += `<path d="M${X(g.req)} ${sy + 2} l-4 7 h8 z" fill="${g.req > g.att + 0.01 ? T.risk : T.pencil}"/>`;
    s += `<text x="${sx}" y="${sy + fs * 1.9}" fill="${T.ink3}" font-size="${fs * 0.85}" font-family="${T.mono}">slots · ${g.slots} running · ${(+g.req).toFixed(1)} needed · ${Math.round(g.att)} can get</text>`;
  }
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(ariaOf(g))}">${s}</svg>`;
}

export function ariaOf(g) {
  return `${Math.round(g.done * 100)}% done${g.par ? `, ${Math.round(g.par * 100)}% of units running` : ''}${Number.isFinite(g.eta) ? `, about ${f.dur(g.eta)} left` : ''}${Number.isFinite(g.slack) ? `, slack ${f.signed(g.slack)}` : ''}`;
}

const KEYS = ['done', 'run', 'runSpent', 'par', 'retry', 'fail', 'risk'];

// Pylon is the live component: it tweens between glances.
export class Pylon {
  constructor(el) { this.el = el; this.g = null; this.stop = () => {}; }
  update({ glance, ...o }) {
    const from = this.g || Object.assign({}, glance, { done: 0, run: 0, runSpent: 0 });
    const to = glance;
    this.stop();
    const id = this.id || (this.id = 'py' + (++seq));
    const same = this.g && KEYS.every((k) => Math.abs((from[k] || 0) - (to[k] || 0)) < 1e-4);
    this.g = to;
    if (same) { this.el.innerHTML = pylonSVG(to, Object.assign({ id }, o)); return; }
    this.stop = tween(ms('--fill-ms', 600), (t) => {
      const g = Object.assign({}, to);
      for (const k of KEYS) g[k] = lerp(from[k] || 0, to[k] || 0, t);
      this.el.innerHTML = pylonSVG(g, Object.assign({ id }, o));
    });
  }
}
register('pylon', Pylon);
