// TIMEBASE: the wall. Every job is a channel sweeping toward its deadline.
// Left of the bright now line is history, kept by the HUD itself from the
// snapshots the stream delivers and fading over fifteen minutes like
// phosphor; right of it is the forecast in pencil, to the ETA band and the
// deadline (the trigger). Deadlines beyond the horizon are arrows at the
// edge. The running share rides the sweep in blue, retries are orange ticks,
// lateness is red (dashed while projected).
//
//   sweep                 the farm's clock between snapshots: it advances at
//                         the rate the snapshots do, never runs backwards, and
//                         stops when they stop
//   channelOf(j)          a channel from a JobSummary
//   TimeBase              live component 'timebase': the scope, one canvas
//   Clocks                live component 'clocks': mission clocks, T-minus in
//                         the farm's clock, and the production window's day
//   Strobe                live component 'strobe': a binary-order render's
//                         frames as a film strip under a dyadic ruler, lit
//                         coarse to fine; frames that just landed glow
//   Ticker                live component 'ticker': the book along the bottom
//
// One animation loop serves every component on the page. It draws at 24
// frames a second while anything moves, and not at all under reduced motion
// (each component then draws once per snapshot).

import { tok, glance, reducedMotion } from './core.js';
import * as f from './fmt.js';
import { ranges } from './rle.js';
import { register, ms as cssMs } from './live.js';

// ------------------------------------------------------------ the sweep clock

export const sweep = {
  base: NaN, at: 0, rate: 0, shown: NaN, seen: 0, frozen: false,
  // feed takes each snapshot's `now` (ms). The rate is farm milliseconds per
  // real millisecond, measured between distinct snapshots: 1 for a real
  // farm, faster for a simulation, 0 when frozen or static.
  feed(t, { frozen = false } = {}) {
    if (!Number.isFinite(t)) return;
    const p = performance.now();
    this.frozen = frozen;
    if (t === this.base) return;
    if (Number.isFinite(this.base) && t > this.base) {
      const r = (t - this.base) / Math.max(1, p - this.at);
      this.rate = this.seen > 1 ? this.rate * 0.6 + r * 0.4 : r;
      this.rate = Math.max(0, Math.min(this.rate, 600));
    }
    if (!Number.isFinite(this.shown) || t > this.shown + 60000 || t < this.base) this.shown = t;
    this.base = t; this.at = p; this.seen++;
  },
  now() {
    if (!Number.isFinite(this.base)) return f.now();
    if (this.frozen || !this.rate) return Math.max(this.base, Number.isFinite(this.shown) ? this.shown : this.base);
    const dt = Math.min(performance.now() - this.at, 15000);
    const est = this.base + dt * this.rate;
    this.shown = Math.max(this.shown, est);
    return this.shown;
  },
};

// ------------------------------------------------------------ one loop

const loop = { fns: new Set(), raf: 0, last: 0 };
function frame(t) {
  loop.raf = 0;
  if (t - loop.last >= 1000 / 24 - 2) {
    loop.last = t;
    for (const c of [...loop.fns]) {
      if (!c.el.isConnected) { loop.fns.delete(c); continue; }
      c.draw(t);
    }
  }
  if (loop.fns.size && !reducedMotion()) loop.raf = requestAnimationFrame(frame);
}
function animate(c) {
  loop.fns.add(c);
  if (!loop.raf && !reducedMotion()) loop.raf = requestAnimationFrame(frame);
}

// fitCanvas sizes a canvas to its host at the device's pixel ratio; k is
// the drawing scale against a 1920-pixel-wide wall.
function fitCanvas(host, cv) {
  const W = Math.max(1, host.clientWidth), H = Math.max(1, host.clientHeight);
  const dpr = Math.min(2, window.devicePixelRatio || 1);
  if (cv.width !== Math.round(W * dpr) || cv.height !== Math.round(H * dpr)) {
    cv.width = Math.round(W * dpr); cv.height = Math.round(H * dpr);
    cv.style.width = W + 'px'; cv.style.height = H + 'px';
  }
  const g = cv.getContext('2d');
  g.setTransform(dpr, 0, 0, dpr, 0, 0);
  return { g, W, H, k: Math.max(0.6, (document.documentElement.clientWidth || W) / 1920) };
}

const p2 = (n) => String(n).padStart(2, '0');
const MINUS = '−';

// tminusAt: mission time from `now` to `t`. Seconds inside a day, then
// days and hours.
export function tminusAt(t, now) {
  if (!Number.isFinite(t)) return '';
  let d = Math.round((t - now) / 1000);
  const sign = d >= 0 ? MINUS : '+';
  d = Math.abs(d);
  if (d >= 86400) return `T${sign}${Math.floor(d / 86400)}d ${p2(Math.floor((d % 86400) / 3600))}h`;
  return `T${sign}${p2(Math.floor(d / 3600))}:${p2(Math.floor((d % 3600) / 60))}:${p2(d % 60)}`;
}

const ORDER = { binary: 'binary', sequential: 'in order', reverse: 'reverse', 'failure-rate': 'failure-rate', gang: 'gang' };

// ------------------------------------------------------------ channels

export function channelOf(j, o = {}) {
  const g = glance(j);
  const e = j.estimate || {};
  const dl = j.objective && j.objective.deadline ? Date.parse(j.objective.deadline) : null;
  const fin = j.phase === 'Succeeded';
  const extra = { Yielded: ' · yielded', Blocked: ' · blocked', Held: ' · held', Failed: ' · failed', Queued: ' · queued' }[j.phase] || '';
  return {
    key: `${j.namespace}/${j.name}`, name: j.name,
    sub: `${Math.round(g.done * 100)}%${g.basis === 'units' ? ' by units' : ''}${extra} · ${j.class} · ${ORDER[j.order] || j.order}`,
    done: g.done, run: fin ? 0 : g.run, retry: g.retry, failed: j.units.failed, par: g.par,
    eta: Date.parse(e.etaP50), p90: Date.parse(e.etaP90), deadline: dl,
    slack: e.slackSeconds, late: !fin && dl !== null && Number.isFinite(e.slackSeconds) && e.slackSeconds < 0,
    fin, phase: j.phase, hi: !!o.hi,
  };
}

// samples: per channel, a ring of { t, v, r } kept across views and reloads
// (sessionStorage), so the wall's history survives a refresh.
const HIST_MS = 15 * 60000;
const store = { map: new Map(), saved: 0, key: '' };
export function useHistory(scope = 'farm') { loadSamples('c4hud-timebase:' + scope); }
function loadSamples(key) {
  if (store.key === key) return;
  store.key = key; store.map = new Map();
  try {
    const raw = sessionStorage.getItem(key);
    if (raw) for (const [k, v] of JSON.parse(raw)) store.map.set(k, v);
  } catch { /* storage unavailable: history starts now */ }
}
function saveSamples() {
  const p = performance.now();
  if (p - store.saved < 5000) return;
  store.saved = p;
  try { sessionStorage.setItem(store.key, JSON.stringify([...store.map])); } catch { /* full or unavailable */ }
}
export function record(chs, t, scope = 'farm') {
  loadSamples('c4hud-timebase:' + scope);
  for (const c of chs) {
    let a = store.map.get(c.key);
    if (!a) store.map.set(c.key, (a = []));
    const last = a[a.length - 1];
    if (last && last.t >= t) continue;
    a.push({ t, v: +c.done.toFixed(5), r: +c.run.toFixed(5) });
    while (a.length > 900 || (a.length > 2 && a[1].t < t - HIST_MS * 1.1)) a.shift();
  }
  for (const [k, a] of store.map) if (!a.length || a[a.length - 1].t < t - HIST_MS * 1.1) store.map.delete(k);
  saveSamples();
}
export const samples = (key) => store.map.get(key) || [];

// ------------------------------------------------------------ TimeBase

export class TimeBase {
  constructor(el) {
    this.el = el;
    el.classList.add('timebase');
    this.cv = document.createElement('canvas');
    this.cv.setAttribute('role', 'img');
    el.append(this.cv);
    this.chs = [];
    this.tw = new Map(); // key -> { from, to, t0 }
    this.o = {};
  }
  update({ channels, now, horizon = 4 * 3600, scope = 'farm', ...o }) {
    if (Number.isFinite(now)) record(channels, now, scope);
    const t0 = performance.now();
    const dur = reducedMotion() ? 0 : cssMs('--fill-ms', 600);
    for (const c of channels) {
      const prev = this.tw.get(c.key);
      const cur = prev ? this.valueOf(prev, t0) : { v: c.done, r: c.run };
      this.tw.set(c.key, { from: cur, to: { v: c.done, r: c.run }, t0, dur });
    }
    this.chs = channels;
    this.o = Object.assign({ horizon }, o);
    this.cv.setAttribute('aria-label', `${channels.length} channels: ${channels.filter((c) => c.late).length} projected late`);
    this.draw(t0);
    animate(this);
  }
  valueOf(tw, t) {
    const x = tw.dur ? Math.min(1, (t - tw.t0) / tw.dur) : 1;
    const e = 1 - Math.pow(1 - x, 3);
    return { v: tw.from.v + (tw.to.v - tw.from.v) * e, r: tw.from.r + (tw.to.r - tw.from.r) * e };
  }
  draw(tnow = performance.now()) {
    const T = tok();
    const { g, W, H, k } = fitCanvas(this.el, this.cv);
    const now = sweep.now();
    const o = this.o;
    const HZ = o.horizon * 1000;
    const labelW = Math.round(280 * k), edgeW = Math.round(170 * k);
    const x0 = labelW, x2 = W - edgeW, xNow = Math.round(x0 + (x2 - x0) * 0.2);
    const top = Math.round(40 * k), bot = Math.round(30 * k);
    const X = (t) => (t <= now ? xNow - (xNow - x0) * Math.min(1, (now - t) / HIST_MS) : xNow + (x2 - xNow) * Math.sqrt(Math.min(1, (t - now) / HZ)));
    const minLane = 40 * k;
    const cap = Math.max(1, Math.floor((H - top - bot) / minLane));
    const chs = this.chs.slice(0, cap);
    const more = this.chs.length - chs.length;
    const lanes = Math.max(chs.length, Math.min(cap, 8));
    const laneH = (H - top - bot) / lanes;
    const mono = (px, w = 400) => `${w} ${Math.round(px * k * 10) / 10}px ${T.mono}`;
    const sans = (px, w = 500) => `${w} ${Math.round(px * k * 10) / 10}px ${T.sans}`;
    g.clearRect(0, 0, W, H);
    g.textBaseline = 'alphabetic';
    // the scope's face
    g.fillStyle = T.g[3]; g.fillRect(x0, top, x2 - x0, H - top - bot);
    // history graticule: whole five minutes of the farm's clock, sliding left as time passes
    g.lineWidth = Math.max(1, k);
    g.font = mono(10); g.textAlign = 'center';
    const step = 5 * 60000;
    for (let t = Math.floor(now / step) * step; t > now - HIST_MS - step; t -= step) {
      if (t < now - HIST_MS) break;
      const x = Math.round(X(t)) + 0.5;
      g.strokeStyle = T.g[7]; g.beginPath(); g.moveTo(x, top); g.lineTo(x, H - bot); g.stroke();
      const ago = Math.round((now - t) / 60000);
      if (Math.abs(x - xNow) > 64 * k) { g.fillStyle = T.g[13]; g.fillText(f.clock(t, false), x, top - 8 * k); }
      if (ago > 0 && x - x0 > 20 * k) { g.fillStyle = T.g[14]; g.fillText(`${MINUS}${ago}m`, x, H - bot + 18 * k); }
    }
    // forecast graticule: fixed offsets from now, the clock above
    for (const [m, l] of [[5, '+5m'], [15, '+15m'], [30, '+30m'], [60, '+1h'], [120, '+2h'], [180, '+3h'], [240, '+4h'], [360, '+6h'], [480, '+8h'], [720, '+12h']]) {
      if (m * 60000 > HZ) break;
      const x = Math.round(X(now + m * 60000)) + 0.5;
      g.strokeStyle = m % 60 === 0 ? T.g[8] : T.g[7]; g.beginPath(); g.moveTo(x, top); g.lineTo(x, H - bot); g.stroke();
      g.fillStyle = T.g[14]; g.fillText(l, x, H - bot + 18 * k);
      if (x < x2 - 40 * k) { g.fillStyle = T.g[13]; g.fillText(f.clock(now + m * 60000, false), x, top - 8 * k); }
    }
    g.textAlign = 'left';
    // channels
    chs.forEach((c, i) => {
      const y0 = top + i * laneH, yb = y0 + laneH - 7 * k, yt = y0 + 9 * k;
      const Y = (v) => yb - (yb - yt) * Math.max(0, Math.min(1, v));
      const tw = this.tw.get(c.key);
      const cur = tw ? this.valueOf(tw, tnow) : { v: c.done, r: c.run };
      if (c.hi) { g.fillStyle = T.g[5]; g.fillRect(0, y0 + 1, x2, laneH - 2); g.fillStyle = T.paper; g.fillRect(0, y0 + 4 * k, 3 * k, laneH - 8 * k); }
      g.strokeStyle = T.g[6]; g.beginPath(); g.moveTo(x0, Math.round(y0 + laneH) + 0.5); g.lineTo(x2, Math.round(y0 + laneH) + 0.5); g.stroke();
      // label
      const ly = y0 + laneH * 0.5;
      g.fillStyle = T.g[14]; g.font = mono(10); g.fillText(`CH${p2(i + 1)}`, 14 * k, ly - 1 * k);
      g.fillStyle = c.late ? T.fail : c.fin ? T.ink3 : c.hi ? T.paper : T.ink; g.font = sans(15, 500);
      clip(g, c.name, 56 * k, ly - 1 * k, labelW - 66 * k);
      g.fillStyle = T.g[15]; g.font = mono(10.5);
      clip(g, c.sub, 56 * k, ly + 14 * k, labelW - 66 * k);
      // persistence: the trace fades with age; step-ups (captures) settle as dots
      const S = samples(c.key).filter((p) => p.t > now - HIST_MS - 60000 && p.t <= now);
      const trace = c.fin ? T.settled : T.done;
      if (S.length) {
        g.setLineDash([1 * k, 3 * k]); g.strokeStyle = T.g[10];
        g.beginPath(); g.moveTo(x0, Y(S[0].v)); g.lineTo(X(S[0].t), Y(S[0].v)); g.stroke(); g.setLineDash([]);
      } else {
        g.setLineDash([1 * k, 3 * k]); g.strokeStyle = T.g[10];
        g.beginPath(); g.moveTo(x0, Y(cur.v)); g.lineTo(xNow, Y(cur.v)); g.stroke(); g.setLineDash([]);
      }
      const pts = S.map((p) => [X(p.t), Y(p.v), p.t, p.v]);
      pts.push([xNow, Y(cur.v), now, cur.v]);
      for (let q = 1; q < pts.length; q++) {
        const a = pts[q - 1], b = pts[q], age = Math.min(1, Math.max(0, (now - b[2]) / HIST_MS));
        g.strokeStyle = trace;
        g.globalAlpha = Math.max(0.05, 0.28 - age * 0.22); g.lineWidth = 7 * k;
        g.beginPath(); g.moveTo(a[0], a[1]); g.lineTo(b[0], b[1]); g.stroke();
        g.globalAlpha = Math.max(0.12, 1 - age * 0.8); g.lineWidth = 1.8 * k;
        g.beginPath(); g.moveTo(a[0], a[1]); g.lineTo(b[0], b[1]); g.stroke();
        if (b[3] - a[3] > 0.0015 && q < pts.length - 1) {
          g.globalAlpha = Math.max(0.15, 1 - age); g.fillStyle = T.g[18];
          g.beginPath(); g.arc(b[0], b[1], 2.2 * k, 0, Math.PI * 2); g.fill();
        }
      }
      g.globalAlpha = 1; g.lineWidth = Math.max(1, k);
      // the running share rides the sweep; retries and failures tick beside it
      if (cur.r > 0.0005) { g.fillStyle = T.run; g.fillRect(xNow - 2.5 * k, Y(Math.min(1, cur.v + cur.r)), 5 * k, Math.max(2 * k, (yb - yt) * Math.min(cur.r, 1 - cur.v))); }
      if (c.retry > 0) { g.fillStyle = T.temp; g.fillRect(xNow + 5 * k, Y(Math.min(1, cur.v + cur.r)), 5 * k, Math.max(5 * k, (yb - yt) * c.retry)); }
      if (c.failed > 0) { g.fillStyle = T.fail; g.fillRect(xNow + (c.retry > 0 ? 12 : 5) * k, Y(Math.min(1, cur.v + cur.r)), 5 * k, 5 * k); }
      if (c.fin) return;
      // forecast in pencil: now to the p50, then the p50 to p90 band on the top line
      const late = c.late;
      const fx = (t) => Math.min(X(t), x2);
      const etaIn = Number.isFinite(c.eta) && c.eta <= now + HZ;
      const xe = Number.isFinite(c.eta) ? fx(Math.max(now, c.eta)) : x2;
      const ye = etaIn ? yt : Y(cur.v + (1 - cur.v) * (Number.isFinite(c.eta) ? Math.sqrt(HZ / Math.max(1, c.eta - now)) : 0.2));
      const xd = c.deadline ? X(c.deadline) : Infinity;
      g.lineWidth = 1.3 * k; g.setLineDash([4 * k, 4 * k]);
      if (late && xd < xe) {
        // past the deadline the pencil turns red: projected late
        const fr = (xd - xNow) / Math.max(1, xe - xNow);
        const ym = Y(cur.v) + (ye - Y(cur.v)) * Math.max(0, fr);
        g.strokeStyle = T.g[15]; g.beginPath(); g.moveTo(xNow, Y(cur.v)); g.lineTo(Math.max(xNow, xd), ym); g.stroke();
        g.strokeStyle = T.fail; g.beginPath(); g.moveTo(Math.max(xNow, xd), ym); g.lineTo(xe, ye); g.stroke();
      } else {
        g.strokeStyle = T.g[15]; g.beginPath(); g.moveTo(xNow, Y(cur.v)); g.lineTo(xe, ye); g.stroke();
      }
      g.setLineDash([]); g.lineWidth = Math.max(1, k);
      if (etaIn) {
        const xp = fx(c.p90);
        g.fillStyle = late ? T.fail : T.g[16];
        g.fillRect(xe, yt - 2.5 * k, Math.max(3 * k, xp - xe), 5 * k);
      }
      // the trigger: the deadline, a hard mark across the channel
      if (c.deadline && c.deadline <= now + HZ) {
        const passed = c.deadline < now;
        const xx = Math.round(xd);
        g.fillStyle = late || passed ? T.fail : T.g[18];
        g.fillRect(xx - 1 * k, y0 + 4 * k, 2 * k, laneH - 8 * k);
        g.beginPath(); g.moveTo(xx, y0 + 6 * k); g.lineTo(xx - 5 * k, y0); g.lineTo(xx + 5 * k, y0); g.closePath(); g.fill();
        g.font = mono(10.5); g.fillStyle = late || passed ? T.fail : T.g[16];
        const lab = `${f.clock(c.deadline, false)} ${Number.isFinite(c.slack) ? f.signed(c.slack) : ''}`;
        const lw = g.measureText(lab).width;
        g.fillText(lab, xx + 7 * k + lw > x2 ? xx - 7 * k - lw : xx + 7 * k, y0 + Math.min(laneH - 9 * k, 27 * k));
      } else if (c.deadline) {
        // beyond the horizon: an arrow at the edge names it
        const ym = y0 + laneH / 2;
        g.fillStyle = T.g[15];
        g.beginPath(); g.moveTo(x2 + 6 * k, ym - 7 * k); g.lineTo(x2 + 18 * k, ym); g.lineTo(x2 + 6 * k, ym + 7 * k); g.closePath(); g.fill();
        g.font = mono(11.5); g.fillStyle = T.g[16]; g.fillText(f.clock(c.deadline), x2 + 26 * k, ym + 1 * k);
        g.font = mono(10); g.fillStyle = T.g[13]; g.fillText(tminusAt(c.deadline, now), x2 + 26 * k, ym + 14 * k);
      }
    });
    if (more > 0) {
      g.font = mono(10.5); g.fillStyle = T.g[14];
      g.fillText(`+${more} more in dispatch order`, 14 * k, H - bot + 18 * k);
    }
    // the sweep: a bright now line with a faint phosphor wash behind it
    const grad = g.createLinearGradient(xNow - 28 * k, 0, xNow, 0);
    grad.addColorStop(0, 'rgba(219,221,221,0)'); grad.addColorStop(1, 'rgba(219,221,221,0.10)');
    g.fillStyle = grad; g.fillRect(xNow - 28 * k, top, 28 * k, H - top - bot);
    g.fillStyle = T.paper; g.fillRect(xNow - 0.75 * k, top - 16 * k, 1.5 * k, H - top - bot + 18 * k);
    g.font = mono(13, 500); g.textAlign = 'center';
    g.fillText(`NOW ${f.clockSec(new Date(now).toISOString())}`, xNow, top - 22 * k);
    g.textAlign = 'left';
    g.strokeStyle = T.g[10]; g.beginPath(); g.moveTo(Math.round(x2) + 0.5, top); g.lineTo(Math.round(x2) + 0.5, H - bot); g.stroke();
    g.font = mono(10); g.fillStyle = T.g[14]; g.fillText('beyond the horizon', x2 + 8 * k, top - 8 * k);
    g.textAlign = 'center'; g.fillText(`${MINUS}15m`, x0, H - bot + 18 * k); g.textAlign = 'left';
  }
}

function clip(g, s, x, y, w) {
  if (g.measureText(s).width <= w) { g.fillText(s, x, y); return; }
  let t = s;
  while (t.length > 1 && g.measureText(t + '…').width > w) t = t.slice(0, -1);
  g.fillText(t + '…', x, y);
}

// ------------------------------------------------------------ Clocks

// items: [{ label, target?: ms, value?: string, sub: html, late?, kind?: 'tminus'|'day'|'static', from?: ms }]
export class Clocks {
  constructor(el) { this.el = el; el.classList.add('clocks'); this.items = []; this.sig = ''; this.vals = []; }
  update({ items }) {
    this.items = items;
    const sig = JSON.stringify(items.map((c) => [c.label, c.sub, c.late, c.kind, c.value]));
    if (sig !== this.sig) {
      this.sig = sig;
      this.el.innerHTML = items.map((c) => `<div class="ck${c.late ? ' late' : ''}${c.kind === 'slots' ? ' slots' : ''}"><div class="k">${c.label}</div><div class="v"></div><div class="s">${c.sub}</div></div>`).join('');
      this.vals = [...this.el.querySelectorAll('.v')];
      this.last = [];
    }
    this.draw();
    animate(this);
  }
  draw() {
    const now = sweep.now();
    this.items.forEach((c, i) => {
      let v;
      if (c.kind === 'tminus') v = tminusAt(c.target, now);
      else if (c.kind === 'day') v = `D${Math.floor((now - c.from) / 864e5) + 1} ${f.clock(now, false)}`;
      else v = c.value;
      if (this.last[i] !== v) { this.last[i] = v; if (this.vals[i]) { if (c.kind === 'slots') this.vals[i].innerHTML = v; else this.vals[i].textContent = v; } }
    });
  }
}

// ------------------------------------------------------------ Strobe

// dyadic levels of positions 0..n-1: 0 the ends, 1 the middle, 2 the quarters ...
const levelCache = new Map();
export function levels(n) {
  if (levelCache.has(n)) return levelCache.get(n);
  const L = new Uint8Array(n).fill(12);
  if (n) { L[0] = 0; L[n - 1] = 0; }
  for (let l = 1; l <= 11; l++) {
    const step = (n - 1) / 2 ** l;
    if (step < 0.5) break;
    for (let q = 1; q < 2 ** l; q += 2) { const i = Math.round(q * step); if (L[i] > l) L[i] = l; }
  }
  levelCache.set(n, L);
  return L;
}

// frames of a job detail's last pass, in axis order, with their states
export function strobeFrames(d) {
  if (!d) return null;
  const st = new Map();
  for (const [k, set] of Object.entries(d.unitStates || {})) for (const [a, b] of ranges(set)) for (let i = a; i <= b; i++) st.set(i, k);
  const groups = (d.groups || []).filter((g) => !g.generator && g.units);
  const grp = groups.find((g) => g.key === 'beauty') || groups[groups.length - 1];
  if (!grp) return null;
  let units;
  if (d.unitList && d.unitList.length) units = d.unitList.filter((u) => u.group === grp.key).sort((a, b) => a.pos - b.pos).map((u) => ({ i: u.i, label: u.label, s: st.get(u.i) || u.state }));
  else { units = []; for (const [a, b] of ranges(grp.units)) for (let i = a; i <= b; i++) units.push({ i, label: String(i), s: st.get(i) || 'queued' }); }
  const n = units.length, L = levels(n);
  let level = -1;
  for (let l = 0; l <= 11; l++) {
    let ok = true;
    for (let i = 0; i < n; i++) if (L[i] <= l && units[i].s !== 'succeeded') { ok = false; break; }
    if (!ok) break;
    level = l;
  }
  return { units, n, level, group: grp.key, lit: units.filter((u) => u.s === 'succeeded').length };
}

export class Strobe {
  constructor(el) {
    this.el = el;
    el.classList.add('strobe');
    this.cap = document.createElement('div');
    this.cap.className = 'stcap';
    this.box = document.createElement('div');
    this.box.className = 'stbox';
    this.cv = document.createElement('canvas');
    this.cv.setAttribute('role', 'img');
    this.box.append(this.cv);
    el.append(this.cap, this.box);
    this.glow = new Map(); // unit index -> performance.now() it landed
    this.prev = null; this.fr = null; this.forKey = null;
  }
  update({ detail, title }) {
    const fr = strobeFrames(detail);
    const key = detail ? `${detail.namespace}/${detail.name}` : null;
    if (key !== this.forKey) { this.prev = null; this.glow.clear(); this.forKey = key; }
    if (fr && this.prev) {
      const t = performance.now();
      for (const u of fr.units) if (u.s === 'succeeded' && this.prev.get(u.i) !== 'succeeded') this.glow.set(u.i, t);
    }
    if (fr) this.prev = new Map(fr.units.map((u) => [u.i, u.s]));
    this.fr = fr;
    const every = fr && fr.level >= 0 ? `complete to 1/${2 ** fr.level}` : 'the ends still due';
    this.cap.innerHTML = `<div class="k">Strobe · ${title || ''}</div>`
      + (fr ? `<div class="s">${fr.lit} of ${fr.n} frames lit; ${every}</div><div class="s"><span class="t-run">blue running</span> · <span class="t-temp">orange retrying</span> · <span class="t-fail">red failed</span></div>` : '<div class="s">no binary-order render in flight</div>');
    this.cv.setAttribute('aria-label', fr ? `${title}: ${fr.lit} of ${fr.n} frames rendered, ${every}` : 'no render in flight');
    this.draw();
    animate(this);
  }
  draw(tn = performance.now()) {
    const T = tok();
    const { g, W, H, k } = fitCanvas(this.box, this.cv);
    g.clearRect(0, 0, W, H);
    const fr = this.fr;
    if (!fr || !fr.n) return;
    const n = fr.n, cw = W / n, L = levels(n);
    const rt = Math.round(30 * k), y = rt + 10 * k, h = H - y - 10 * k;
    g.fillStyle = T.g[1]; g.fillRect(0, rt + 2 * k, W, H - rt - 2 * k);
    for (let i = 0; i < n; i++) {
      const u = fr.units[i], x = i * cw, l = L[i];
      const tick = l >= 12 ? 3 * k : Math.max(4 * k, (28 - l * 3.5) * k);
      g.fillStyle = l <= fr.level ? T.g[18] : l <= 3 ? T.g[15] : l <= 6 ? T.g[13] : T.g[10];
      g.fillRect(Math.round(x + cw / 2), rt - tick, Math.max(1, k), tick);
      const fill = u.s === 'succeeded' ? T.g[17] : u.s === 'running' ? T.run : u.s === 'retrying' ? T.temp : u.s === 'failed' ? T.fail : u.s === 'unevaluated' ? T.g[6] : T.g[8];
      const gap = cw > 3 ? Math.max(0.6, cw * 0.12) : 0;
      g.fillStyle = fill; g.fillRect(x + gap / 2, y, Math.max(0.6, cw - gap), h);
      if (u.s === 'succeeded') { g.fillStyle = T.done; g.fillRect(x + gap / 2, y + h - 3 * k, Math.max(0.6, cw - gap), 3 * k); }
      if (u.s === 'running' && cw > 2) { g.fillStyle = T.runBody; g.fillRect(x + gap / 2, y, Math.max(0.6, cw - gap), h * 0.5); }
      // a frame that just landed glows and settles over a minute
      const t0 = this.glow.get(u.i);
      if (t0 && !reducedMotion()) {
        const a = 1 - (tn - t0) / 60000;
        if (a > 0) { g.globalAlpha = a; g.fillStyle = T.paper; g.fillRect(x + gap / 2 - 1, y - 2 * k, Math.max(0.6, cw - gap) + 2, h + 4 * k); g.globalAlpha = 1; }
        else this.glow.delete(u.i);
      }
      // sprockets
      if (i % 2 === 0 && cw > 2) { g.fillStyle = T.g[6]; g.fillRect(x + cw * 0.3, rt + 4 * k, cw * 0.4, 3 * k); g.fillRect(x + cw * 0.3, H - 6 * k, cw * 0.4, 3 * k); }
    }
    if (!this.glow.size) loop.fns.delete(this);
  }
}

// ------------------------------------------------------------ Ticker

const SEALS = {
  node: (g, x, y, s) => { g.save(); g.translate(x, y); g.rotate(Math.PI / 4); g.fillRect(-s * 0.36, -s * 0.36, s * 0.72, s * 0.72); g.restore(); },
  queue: (g, x, y, s) => g.fillRect(x - s * 0.4, y - s * 0.4, s * 0.8, s * 0.8),
  hud: (g, x, y, s) => { g.beginPath(); g.arc(x, y, s * 0.42, 0, Math.PI * 2); g.stroke(); },
  tri: (g, x, y, s) => { g.beginPath(); g.moveTo(x - s * 0.45, y + s * 0.4); g.lineTo(x + s * 0.45, y + s * 0.4); g.lineTo(x, y - s * 0.45); g.closePath(); g.fill(); },
};
const TONE = { failed: 'fail', retry: 'temp', retrying: 'temp', backoff: 'temp', suspect: 'temp', blacklist: 'temp', late: 'fail' };

// The book as a ticker: entries enter from the right, newest last, and the
// line cycles through the latest entries; new ones join the cycle as they
// are written. Drawn on a canvas so the scroll is one transform-free blit.
export class Ticker {
  constructor(el) {
    this.el = el;
    el.classList.add('ticker');
    this.cv = document.createElement('canvas');
    this.cv.setAttribute('role', 'log');
    el.append(this.cv);
    this.entries = []; this.queue = []; this.cursor = -1; this.off = 0; this.t = 0; this.widths = new Map();
  }
  update({ entries }) {
    const list = (entries || []).slice().sort((a, b) => a.seq - b.seq);
    this.entries = list;
    this.cv.setAttribute('aria-label', list.length ? `The book, ${list.length} latest entries; newest: ${list[list.length - 1].message}` : 'The book');
    if (!this.queue.length && list.length) { this.cursor = Math.max(-1, list.length - 9); }
    this.draw();
    animate(this);
  }
  next() {
    const L = this.entries;
    if (!L.length) return null;
    // the cursor walks by seq, so entries written since keep their place
    const curSeq = this.cursor >= 0 && L[this.cursor] ? L[this.cursor].seq : -1;
    let i = L.findIndex((e) => e.seq > curSeq);
    if (i < 0) i = 0;
    this.cursor = i;
    return L[i];
  }
  text(e) {
    const at = e.approx ? '<' + f.clock(e.at, false) : f.clockSec(e.at);
    return { at, msg: e.message, who: (e.actor || '') };
  }
  draw(tn = performance.now()) {
    const T = tok();
    const { g, W, H, k } = fitCanvas(this.el, this.cv);
    g.clearRect(0, 0, W, H);
    if (!this.entries.length) return;
    const fs = 13 * k, y = H / 2 + fs * 0.36;
    const speed = 46 * k; // px per second
    const moving = !reducedMotion();
    const dt = this.t ? Math.min(0.2, (tn - this.t) / 1000) : 0;
    this.t = tn;
    if (moving) this.off += dt * speed;
    // lay out entries from the queue; drop those that scrolled off; refill on the right
    const widthOf = (e) => {
      const tx = this.text(e);
      g.font = `500 ${fs}px ${T.mono}`; const a = g.measureText(tx.at).width;
      g.font = `400 ${fs}px ${T.mono}`; const b = g.measureText(tx.msg).width;
      g.font = `400 ${fs * 0.85}px ${T.mono}`; const c = g.measureText(tx.who).width;
      return a + b + c + 64 * k;
    };
    while (this.queue.length && this.queue[0].x + this.queue[0].w - this.off < 0) this.queue.shift();
    let end = this.queue.length ? this.queue[this.queue.length - 1].x + this.queue[this.queue.length - 1].w : this.off + (moving ? W * 0.35 : 24 * k);
    let guard = 0;
    while (end - this.off < W + 200 * k && guard++ < 60) {
      const e = this.next();
      if (!e) break;
      const w = widthOf(e);
      this.queue.push({ e, x: end, w });
      end += w;
    }
    g.textBaseline = 'alphabetic';
    for (const q of this.queue) {
      let x = q.x - this.off + 24 * k;
      if (x > W) break;
      const tx = this.text(q.e);
      g.font = `500 ${fs}px ${T.mono}`; g.fillStyle = q.e.approx ? T.pencil : T.g[18];
      g.fillText(tx.at, x, y); x += g.measureText(tx.at).width + 12 * k;
      g.fillStyle = T.g[17]; g.strokeStyle = T.g[17]; g.lineWidth = 1.2 * k;
      const sg = q.e.signer === 'node' ? 'node' : q.e.signer === 'queue' ? 'queue' : q.e.signer === 'hud' ? 'hud' : 'tri';
      SEALS[sg](g, x + 4 * k, y - fs * 0.34, 10 * k); x += 16 * k;
      g.font = `400 ${fs}px ${T.mono}`;
      const tone = TONE[q.e.kind];
      g.fillStyle = tone === 'fail' ? T.fail : tone === 'temp' ? T.temp : T.g[16];
      g.fillText(tx.msg, x, y); x += g.measureText(tx.msg).width + 8 * k;
      g.font = `400 ${fs * 0.85}px ${T.mono}`; g.fillStyle = T.g[13];
      g.fillText(tx.who, x, y); x += g.measureText(tx.who).width + 14 * k;
      g.fillStyle = T.g[10]; g.fillRect(x, y - fs * 0.5, 1 * k, fs * 0.6);
    }
  }
}

register('timebase', TimeBase);
register('clocks', Clocks);
register('strobe', Strobe);
register('ticker', Ticker);
