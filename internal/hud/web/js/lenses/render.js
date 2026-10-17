// Render lens: the cost-by-frame strip and the preview player.

import { h, s } from '../dom.js';
import * as f from '../fmt.js';
import { get, objectURL, staticMode } from '../api.js';

// costStrip: measured frame times as bars, interpolated estimates as a band
// with error bars, heavy regions marked.
export function costStrip(job) {
  const cbf = job.costByFrame || [];
  if (!cbf.length) return null;
  const n = cbf.length;
  const passes = (job.classSummary && job.classSummary.passes) || [];
  let max = 0;
  for (const c of cbf) max = Math.max(max, c.seconds || 0, c.high || 0);
  max = niceMax(max);
  const y = (v) => 100 - (v / max) * 100;
  const band = [];
  const line = [];
  const bars = [];
  const errs = [];
  let lo = [];
  let hi = [];
  const flush = () => {
    if (lo.length > 1) band.push(s('polygon', { points: [...hi, ...lo.reverse()].join(' '), fill: '#4a4a4a' }));
    lo = [];
    hi = [];
  };
  let measured = 0;
  cbf.forEach((c, i) => {
    if (c.seconds !== undefined) {
      measured++;
      flush();
      bars.push(s('rect', { x: i + 0.12, y: y(c.seconds), width: 0.76, height: 100 - y(c.seconds), fill: 'var(--ink-2)' }));
      line.push(`${i + 0.5},${y(c.seconds)}`);
      return;
    }
    lo.push(`${i + 0.5},${y(c.low)}`);
    hi.push(`${i + 0.5},${y(c.high)}`);
    line.push(`${i + 0.5},${y(c.estimate)}`);
    if (i % 6 === 3) errs.push(s('line', { x1: i + 0.5, x2: i + 0.5, y1: y(c.low), y2: y(c.high), stroke: 'var(--ink-3)', 'stroke-width': 1, 'vector-effect': 'non-scaling-stroke' }));
  });
  flush();
  const heavy = (job.heavyRegions || []).map((r) => {
    const a = cbf.findIndex((c) => c.frame >= r.from);
    const b = cbf.findIndex((c) => c.frame >= r.to);
    return { ...r, a, b: b < 0 ? n - 1 : b };
  });
  const plot = s('svg', { viewBox: `0 0 ${n} 100`, preserveAspectRatio: 'none', class: 'chart', style: { height: '118px' }, 'aria-hidden': 'true' },
    s('line', { x1: 0, x2: n, y1: 50, y2: 50, class: 'grid', 'vector-effect': 'non-scaling-stroke' }),
    heavy.map((r) => s('rect', { x: r.a, y: 0, width: r.b - r.a + 1, height: 100, fill: 'rgba(196,196,0,0.07)' })),
    band, errs, bars,
    s('polyline', { points: line.join(' '), fill: 'none', stroke: 'var(--ink-3)', 'stroke-width': 1, 'stroke-dasharray': '3 2', 'vector-effect': 'non-scaling-stroke' }),
    heavy.map((r) => s('rect', { x: r.a, y: 0, width: r.b - r.a + 1, height: 1.6, fill: 'var(--ink-2)' })),
    s('line', { x1: 0, x2: n, y1: 100, y2: 100, class: 'axis', 'vector-effect': 'non-scaling-stroke' }));
  const xt = [];
  const xstep = 24 * Math.max(1, Math.ceil(n / 24 / 10));
  for (let i = 0; i < n; i += xstep) xt.push(h('span', { style: { left: `${(i / n) * 100}%` } }, String(cbf[i].frame)));
  return h('div', { class: 'panel', 'data-key': 'coststrip' },
    h('header', null, h('h2', null, passes.length > 1 ? `Cost by frame, ${passes[passes.length - 1]}` : 'Cost by frame'),
      h('span', { class: 'dim' }, measured ? `${measured} of ${n} frames measured; the rest interpolated between measured neighbors.` : 'No frame measured yet: estimates come from this pass\u2019s history.')),
    h('div', { class: 'cost', 'data-cost': '1', style: { position: 'relative', paddingLeft: '34px' } },
      h('div', { class: 'num dimmer', style: { position: 'absolute', left: 0, top: '-2px', fontSize: '10px' } }, f.dur(max)),
      h('div', { class: 'num dimmer', style: { position: 'absolute', left: 0, top: '52px', fontSize: '10px' } }, f.dur(max / 2)),
      heavy.map((r) => h('div', { class: 'dim', style: { position: 'absolute', left: `calc(34px + (100% - 34px) * ${r.a / n})`, top: '4px', fontSize: '11px', paddingLeft: '4px', whiteSpace: 'nowrap' } },
        `${r.label ? r.label + ': ' : ''}frames ${r.from}-${r.to} run ${r.factor}× the median`)),
      plot,
      h('div', { class: 'xt num dimmer', style: { position: 'relative', height: '14px', fontSize: '10px' } },
        xt.map((x) => { x.style.position = 'absolute'; x.style.transform = 'translateX(-2px)'; return x; })),
      h('div', { class: 'tip', hidden: true, 'data-costtip': '1' })),
    h('div', { class: 'legend' },
      h('span', null, h('i', { style: { background: 'var(--ink-2)' } }), 'measured'),
      h('span', null, h('i', { style: { background: '#4a4a4a', borderTop: '1px dashed var(--ink-3)' } }), 'estimate, with its error band'),
      heavy.length ? h('span', null, h('i', { style: { background: 'var(--ink-2)', height: '3px' } }), 'heavy region, over 3× the median') : null));
}

function niceMax(v) {
  const steps = [60, 120, 300, 600, 900, 1200, 1800, 2400, 3600, 5400, 7200, 10800];
  for (const st of steps) if (v <= st) return st;
  return Math.ceil(v / 3600) * 3600;
}

export function wireCostStrip(root, job, onPick) {
  const el = root.querySelector('[data-cost]');
  if (!el || el.dataset.wired) return;
  el.dataset.wired = '1';
  const tip = el.querySelector('[data-costtip]');
  const at = (e) => {
    const svg = el.querySelector('svg');
    const r = svg.getBoundingClientRect();
    const cbf = el._job.costByFrame;
    const i = Math.floor(((e.clientX - r.left) / r.width) * cbf.length);
    return i >= 0 && i < cbf.length ? { i, c: cbf[i], r } : null;
  };
  el.addEventListener('mousemove', (e) => {
    const p = at(e);
    if (!p) { tip.hidden = true; return; }
    const c = p.c;
    tip.textContent = c.seconds !== undefined ? `${c.frame}: ${f.dur(c.seconds)} measured` : `${c.frame}: about ${f.dur(c.estimate)} (${f.dur(c.low)} to ${f.dur(c.high)}), estimated`;
    tip.hidden = false;
    const er = el.getBoundingClientRect();
    tip.style.left = Math.min(er.width - 220, e.clientX - er.left + 10) + 'px';
    tip.style.top = '20px';
  });
  el.addEventListener('mouseleave', () => { tip.hidden = true; });
  el.addEventListener('click', (e) => {
    const p = at(e);
    if (p) onPick(p.c.frame);
  });
  el._job = job;
}

export function updateCostStrip(root, job) {
  const el = root.querySelector('[data-cost]');
  if (el) el._job = job;
}

// The preview player plays the shot at its frame rate as soon as coarse
// frames exist, holding each rendered frame until the next rendered one.
export class Player {
  constructor() {
    this.imgs = new Map();
    this.frames = [];
    this.avail = [];
    this.playing = !staticMode && !window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    this.t0 = performance.now();
    this.pos = 0;
    this.raf = 0;
    this.fps = 24;
    this.key = null;
    this.lastFetch = 0;
    this.lastDone = -1;
  }

  async load(job, force) {
    const k = `${job.namespace}/${job.name}`;
    const done = job.units.succeeded;
    if (k === this.key && !force && done === this.lastDone) return;
    if (performance.now() - this.lastFetch < 1500 && k === this.key && !force) return;
    this.lastFetch = performance.now();
    this.lastDone = done;
    const keyChanged = k !== this.key;
    this.key = k;
    try {
      const p = await get(`/jobs/${job.namespace}/${job.name}/preview`);
      this.fps = p.fps || 24;
      this.frames = p.frames;
      this.avail = p.frames.filter((x) => x.url);
      for (const fr of this.avail) {
        if (this.imgs.has(fr.url)) continue;
        const im = new Image();
        im.decoding = 'async';
        im.onload = () => this.draw();
        im.src = objectURL(fr.url);
        this.imgs.set(fr.url, im);
      }
      if (keyChanged && staticMode) {
        const hv = (job.heavyRegions || [])[0];
        const target = hv ? Math.round((hv.from + hv.to) / 2) : p.frames[Math.floor(p.frames.length * 0.4)].frame;
        this.pos = Math.max(0, p.frames.findIndex((x) => x.frame >= target));
      }
      this.draw();
    } catch (e) {
      this.err = e.message;
    }
  }

  attach(root) {
    const cv = root.querySelector('canvas[data-player]');
    if (!cv) return;
    this.cv = cv;
    this.badge = root.querySelector('[data-pbadge]');
    this.strip = root.querySelector('canvas[data-pstrip]');
    this.btn = root.querySelector('[data-pbtn]');
    if (!cv.dataset.wired) {
      cv.dataset.wired = '1';
      this.btn.addEventListener('click', () => this.toggle());
      this.strip.addEventListener('click', (e) => {
        const r = this.strip.getBoundingClientRect();
        this.pos = Math.floor(((e.clientX - r.left) / r.width) * this.frames.length);
        this.t0 = performance.now() - (this.pos / this.fps) * 1000;
        this.draw();
      });
      cv.parentElement.addEventListener('keydown', (e) => {
        if (e.key === ' ' || e.key === 'k') { e.preventDefault(); this.toggle(); }
        if (e.key === 'ArrowRight') { this.step(1); }
        if (e.key === 'ArrowLeft') { this.step(-1); }
      });
    }
    this.loop();
  }

  step(d) {
    this.playing = false;
    this.pos = (this.pos + d + this.frames.length) % Math.max(1, this.frames.length);
    this.draw();
  }

  toggle() {
    this.playing = !this.playing;
    this.t0 = performance.now() - (this.pos / this.fps) * 1000;
    this.loop();
    this.draw();
  }

  loop() {
    cancelAnimationFrame(this.raf);
    if (!this.playing) return;
    const tick = () => {
      if (!this.cv || !this.cv.isConnected) return;
      const n = this.frames.length || 1;
      this.pos = Math.floor(((performance.now() - this.t0) / 1000) * this.fps) % n;
      this.draw();
      this.raf = requestAnimationFrame(tick);
    };
    this.raf = requestAnimationFrame(tick);
  }

  // shown is the rendered frame to display at playhead index p: the latest
  // rendered frame at or before it, wrapping to the end of the shot.
  shown(p) {
    if (!this.avail.length) return null;
    const fr = this.frames[p] ? this.frames[p].frame : 0;
    let best = null;
    for (const a of this.avail) if (a.frame <= fr) best = a;
    return best || this.avail[this.avail.length - 1];
  }

  draw() {
    if (!this.cv || !this.cv.isConnected) return;
    const cv = this.cv;
    const dpr = window.devicePixelRatio || 1;
    const W = cv.clientWidth;
    const H = cv.clientHeight;
    if (cv.width !== Math.round(W * dpr)) { cv.width = Math.round(W * dpr); cv.height = Math.round(H * dpr); }
    const g = cv.getContext('2d');
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.fillStyle = '#000';
    g.fillRect(0, 0, W, H);
    const cur = this.frames[this.pos];
    const sh = this.shown(this.pos);
    if (sh) {
      const im = this.imgs.get(sh.url);
      if (im && im.complete && im.naturalWidth) {
        g.imageSmoothingQuality = 'high';
        g.drawImage(im, 0, 0, W, H);
      }
    } else {
      g.fillStyle = '#7f7f7f';
      g.font = '13px Archivo';
      g.textAlign = 'center';
      g.fillText('No frames rendered yet', W / 2, H / 2);
    }
    if (this.badge && cur) {
      const held = sh && sh.frame !== cur.frame;
      this.badge.textContent = held ? `${cur.frame}  holding ${sh.frame}` : `${cur.frame}`;
    }
    if (this.btn) {
      this.btn.textContent = this.playing ? 'Pause' : 'Play';
      this.btn.setAttribute('aria-pressed', this.playing ? 'true' : 'false');
    }
    this.drawStrip();
  }

  drawStrip() {
    const cv = this.strip;
    if (!cv) return;
    const dpr = window.devicePixelRatio || 1;
    const W = cv.clientWidth;
    const H = cv.clientHeight;
    if (cv.width !== Math.round(W * dpr)) { cv.width = Math.round(W * dpr); cv.height = Math.round(H * dpr); }
    const g = cv.getContext('2d');
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, W, H);
    const n = this.frames.length;
    if (!n) return;
    const cs = getComputedStyle(document.documentElement);
    g.fillStyle = cs.getPropertyValue('--u-queued');
    g.fillRect(0, 2, W, H - 4);
    g.fillStyle = cs.getPropertyValue('--u-done');
    const w = Math.max(1, W / n);
    this.frames.forEach((fr, i) => { if (fr.url) g.fillRect((i / n) * W, 2, w, H - 4); });
    g.fillStyle = cs.getPropertyValue('--bar-cyan');
    g.fillRect((this.pos / n) * W - 1, 0, 2, H);
  }

  summary() {
    const n = this.frames.length;
    if (!n) return '';
    let gap = 0;
    let prev = null;
    for (const fr of this.frames) {
      if (fr.url) { prev = fr.frame; continue; }
      if (prev !== null) gap = Math.max(gap, fr.frame - prev);
    }
    return `${this.avail.length} of ${n} frames rendered. Plays at ${this.fps} fps; no frame is held longer than ${gap + 1} frames.`;
  }

  stop() {
    cancelAnimationFrame(this.raf);
  }
}

export function playerPanel(player, job) {
  return h('div', { class: 'panel', 'data-key': 'player' },
    h('header', null, h('h2', null, 'Preview'), h('span', { class: 'dim' }, `${job.classSummary.fps || 24} fps`)),
    h('div', { class: 'player', tabindex: '0', 'aria-label': 'Preview player. Space plays or pauses; arrow keys step.' },
      h('canvas', { 'data-player': '1', 'data-key': 'pcv' }),
      h('span', { class: 'badge num', 'data-pbadge': '1', 'data-own': '1' })),
    h('div', { class: 'pbar' },
      h('button', { class: 'btn small', 'data-pbtn': '1', 'data-own': '1', type: 'button' }, player.playing ? 'Pause' : 'Play'),
      h('canvas', { class: 'strip', 'data-pstrip': '1', 'aria-hidden': 'true' })),
    h('p', { class: 'dim', style: { margin: '6px 0 0', fontSize: '12px' } }, player.summary()));
}
