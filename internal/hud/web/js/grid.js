// The job view: one box per unit on a canvas.
//
// Light gray: a generator not yet evaluated (it may expand). Dark gray: a
// concrete unit waiting. Dark gray with a hatch: blocked by a dependency.
// Running units fill with green in place; measured progress is solid,
// estimated progress carries a fine stripe. Solid green: done. Red: failed.
// Retrying: red notches for failed attempts while the new attempt fills.
// Held: outlined in magenta. Waiting units projected to finish after the
// deadline carry a yellow mark. Selection is cyan.

import { ranges, stateArray, ST } from './rle.js';

let C = null;
const reduced = () => window.matchMedia('(prefers-reduced-motion: reduce)').matches || document.documentElement.dataset.motion === 'off';

function colors() {
  if (C) return C;
  const cs = getComputedStyle(document.documentElement);
  const v = (n) => cs.getPropertyValue(n).trim();
  C = {
    well: v('--well'), uneval: v('--u-uneval'), queued: v('--u-queued'), hatch: v('--u-hatch'),
    done: v('--u-done'), doneDeep: v('--u-done-deep'), failed: v('--bar-red'), held: v('--bar-magenta'),
    risk: v('--bar-yellow'), sel: v('--bar-cyan'), ink: v('--ink'), ink2: v('--ink-2'), ink3: v('--ink-3'),
    rule: v('--rule'), onDone: v('--u-on-done'), onQueued: v('--u-on-queued'), onUneval: v('--u-on-uneval'),
  };
  return C;
}

const patterns = new WeakMap();
function pattern(ctx, kind, dpr) {
  let m = patterns.get(ctx);
  if (!m) { m = {}; patterns.set(ctx, m); }
  const key = kind + dpr;
  if (m[key]) return m[key];
  const c = colors();
  const n = Math.round(6 * dpr);
  const t = document.createElement('canvas');
  t.width = t.height = n;
  const g = t.getContext('2d');
  if (kind === 'hatch') {
    g.fillStyle = c.queued;
    g.fillRect(0, 0, n, n);
    g.strokeStyle = c.hatch;
    g.lineWidth = 1.6 * dpr;
  } else {
    g.fillStyle = c.done;
    g.fillRect(0, 0, n, n);
    g.strokeStyle = c.doneDeep;
    g.lineWidth = 1.1 * dpr;
  }
  g.beginPath();
  for (let k = -n; k <= 2 * n; k += n) {
    g.moveTo(k, n);
    g.lineTo(k + n, 0);
  }
  g.stroke();
  m[key] = ctx.createPattern(t, 'repeat');
  return m[key];
}

const ease = (t) => 1 - Math.pow(1 - t, 3);

// normalize turns a job detail into per-unit arrays.
function normalize(d) {
  const hold = d.nudge && d.nudge.hold;
  const st = stateArray(d.unitStates, hold);
  const n = st.length;
  const pos = new Int32Array(n).fill(-1);
  const grp = new Int16Array(n).fill(-1);
  const att = new Uint8Array(n);
  const risk = new Uint8Array(n);
  const labels = new Array(n);
  const gi = new Map(d.groups.map((g, k) => [g.key, k]));
  if (d.unitList) {
    for (const u of d.unitList) {
      pos[u.i] = u.pos ?? -1;
      grp[u.i] = gi.get(u.group) ?? -1;
      att[u.i] = Math.min(255, u.attempts || 0);
      if (u.risk) risk[u.i] = 1;
      labels[u.i] = u.label;
    }
  }
  d.groups.forEach((g, k) => {
    let r = 0;
    for (const [a, b] of ranges(g.units)) {
      for (let i = a; i <= b; i++) {
        if (grp[i] < 0) grp[i] = k;
        if (pos[i] < 0) pos[i] = r;
        r++;
      }
    }
  });
  for (const [a, b] of ranges(d.riskUnits || '')) risk.fill(1, a, b + 1);
  const hidden = new Uint8Array(n);
  const expandedFrom = new Int32Array(n).fill(-1);
  for (const e of d.expansions || []) {
    if (e.generator < n) hidden[e.generator] = 1;
    for (const [a, b] of ranges(e.children)) for (let i = a; i <= b && i < n; i++) expandedFrom[i] = e.generator;
  }
  return { st, pos, grp, att, risk, labels, hidden, expandedFrom, n };
}

export class JobGrid {
  constructor(canvas, opts = {}) {
    this.cv = canvas;
    this.opts = opts;
    this.ctx = canvas.getContext('2d');
    this.items = [];
    this.prog = new Map();
    this.anims = [];
    this.seenExp = null;
    this.prevRects = new Map();
    this.selected = -1;
    this.raf = 0;
    canvas.addEventListener('click', (e) => this.onClick(e));
    canvas.addEventListener('mousemove', (e) => this.onMove(e));
    canvas.addEventListener('mouseleave', () => this.tip(null));
    canvas.addEventListener('keydown', (e) => this.onKey(e));
    this.ro = new ResizeObserver(() => this.layoutAndDraw());
    this.ro.observe(canvas.parentElement);
  }

  set(detail, { selected = -1, focus = null } = {}) {
    const first = !this.d;
    const prevItems = this.items;
    this.d = detail;
    this.focus = focus;
    this.selected = selected;
    this.u = normalize(detail);
    // Progress tween.
    const t = performance.now();
    const next = new Map();
    for (const [k, v] of Object.entries(detail.progress || {})) {
      const i = Number(k);
      const old = this.prog.get(i);
      const from = old ? old.cur(t) : v.fraction;
      next.set(i, { from, to: v.fraction, measured: v.measured, t0: t, cur(tt) {
        if (reduced()) return this.to;
        const f = Math.min(1, (tt - this.t0) / 900);
        return this.from + (this.to - this.from) * ease(f);
      } });
    }
    this.prog = next;
    // Remember where generators were drawn so their children can split
    // out of them.
    const genRects = new Map();
    for (const it of prevItems) if (it.i >= 0) genRects.set(it.i, it);
    const exps = (detail.expansions || []).map((e) => e.generator);
    if (!first && this.seenExp && !reduced()) {
      for (const e of detail.expansions || []) {
        if (this.seenExp.has(e.generator)) continue;
        const r = genRects.get(e.generator);
        if (!r) continue;
        const kids = [];
        for (const [a, b] of ranges(e.children)) for (let i = a; i <= b; i++) kids.push(i);
        this.anims.push({ kids: new Set(kids), from: r, t0: t, dur: 650 });
      }
    }
    this.seenExp = new Set(exps);
    this.layoutAndDraw();
  }

  // layout computes a rect for every visible unit or aggregate cell.
  layout() {
    const d = this.d;
    const u = this.u;
    const wrap = this.cv.parentElement;
    const W = Math.max(280, wrap.clientWidth);
    const items = [];
    const labels = [];
    const groups = d.groups;
    const genIdx = groups.findIndex((g) => g.generator);
    const frame = d.axis && d.axis.kind === 'frame';
    const start = (d.axis && d.axis.start) || 0;
    const step = (d.axis && d.axis.step) || 1;
    const focus = this.focus;

    // Units per group, by position.
    const per = groups.map(() => []);
    let maxPos = 0;
    let visible = 0;
    for (let i = 0; i < u.n; i++) {
      if (u.st[i] === ST.none || u.hidden[i] || u.grp[i] < 0) continue;
      if (focus && focus.group !== undefined && groups[u.grp[i]].key !== focus.group) continue;
      per[u.grp[i]].push(i);
      maxPos = Math.max(maxPos, u.pos[i] + 1);
      visible++;
    }
    const mode = visible <= 2000 ? 'boxes' : visible <= 20000 ? 'cells' : 'agg';
    this.mode = mode;
    const gap = mode === 'boxes' ? 2 : 1;
    const named = !frame || mode === 'agg';
    const gutter = mode === 'agg' ? Math.min(150, W * 0.16) : named ? Math.min(118, W * 0.14) : W < 520 ? 44 : 64;
    const Wc = W - gutter - 22;
    let y = 4;

    const banded = genIdx >= 0 && !focus;
    if (banded) {
      // Every concrete group is a sub-row of each band; a generator not yet
      // expanded covers the sub-rows its children will land in.
      const sub = groups.map((g, k) => k).filter((k) => !groups[k].generator);
      const spans = (groups[genIdx].spans || []).map((key) => sub.indexOf(groups.findIndex((g) => g.key === key))).filter((r) => r >= 0);
      const r0 = spans.length ? Math.min(...spans) : 0;
      const r1 = spans.length ? Math.max(...spans) : sub.length - 1;
      const rowLen = frame ? 24 : Math.max(1, Math.min(24, maxPos));
      const bw = Math.max(8, Math.min(frame ? 54 : 72, Math.floor((Wc - (rowLen - 1) * gap) / rowLen)));
      const sh = frame ? Math.max(9, Math.min(16, Math.round(bw * 0.42))) : 22;
      const bands = Math.max(1, Math.ceil(maxPos / rowLen));
      const bandH = sub.length * (sh + 2) - 2;
      const subY = (r) => r * (sh + 2);
      for (let b = 0; b < bands; b++) {
        const by = y + b * (bandH + 8);
        if (frame) {
          labels.push({ x: gutter - 8, y: by + bandH / 2, text: String(start + b * rowLen * step), sub: `${b}s` });
          if (b === 0) sub.forEach((g, r) => labels.push({ x: gutter + rowLen * (bw + gap) + 6, y: by + subY(r) + sh / 2, text: groups[g].label.slice(0, 1).toUpperCase(), tiny: true, left: true }));
        } else {
          sub.forEach((g, r) => labels.push({ x: gutter - 8, y: by + subY(r) + sh / 2, text: groups[g].label, name: true }));
        }
      }
      const at = (p) => ({ x: gutter + (p % rowLen) * (bw + gap), by: y + Math.floor(p / rowLen) * (bandH + 8) });
      for (const i of per[genIdx]) {
        const a = at(u.pos[i]);
        items.push({ i, x: a.x, y: a.by + subY(r0), w: bw, h: subY(r1) + sh - subY(r0), gen: true, label: true });
      }
      sub.forEach((g, r) => {
        for (const i of per[g]) {
          const a = at(u.pos[i]);
          items.push({ i, x: a.x, y: a.by + subY(r), w: bw, h: sh, label: r === 0 || !frame });
        }
      });
      y += bands * (bandH + 8);
      this.box = { bw, bh: sh, rowLen };
    } else {
      const shown = groups.map((g, k) => k).filter((k) => per[k].length > 0);
      let rowLen;
      let bw;
      let bh;
      let agg = 1;
      if (mode === 'boxes') {
        rowLen = frame ? 24 : Math.max(1, Math.min(24, maxPos));
        bw = Math.floor((Wc - (rowLen - 1) * gap) / rowLen);
        bw = rowLen === 1 ? Math.min(Wc, 900) : Math.max(8, Math.min(frame ? 46 : 64, bw));
        bh = rowLen === 1 ? 26 : Math.max(10, Math.min(26, Math.round(bw * 0.72)));
      } else if (mode === 'cells') {
        rowLen = 24;
        for (let k = 1; k <= 16; k++) {
          const c = Wc / (24 * k);
          if (c < 4) break;
          rowLen = 24 * k;
          const rows = shown.reduce((a, g) => a + Math.ceil(per[g].length / rowLen), 0);
          if (rows * c <= 460) break;
        }
        bw = Wc / rowLen - gap;
        bh = Math.max(3, Math.round(bw));
      } else {
        bh = 13;
        const longest = Math.max(...shown.map((g) => per[g].length));
        const target = Math.floor(Wc / 8);
        agg = Math.ceil(longest / target);
        rowLen = Math.ceil(longest / agg);
        bw = Wc / rowLen - gap;
      }
      this.box = { bw, bh, rowLen, agg };
      const multi = shown.length > 1;
      const heads = multi && mode !== 'agg' && rowLen > 1;
      for (const g of shown) {
        const list = per[g].sort((a, b) => u.pos[a] - u.pos[b]);
        if (heads) {
          const done = list.filter((i) => u.st[i] === ST.succeeded).length;
          labels.push({ x: gutter, y: y + 7, text: groups[g].label, head: true, count: `${done} of ${list.length} done` });
          items.push({ i: -2, group: groups[g].key, x: gutter, y, w: Wc, h: 14 });
          y += 18;
        }
        if (mode === 'agg') {
          labels.push({ x: gutter - 8, y: y + bh / 2, text: groups[g].label, name: true, group: groups[g].key });
          // Spread units evenly so no cell is a short remainder.
          const cells = Math.min(rowLen, Math.ceil(list.length / agg));
          for (let c = 0; c < cells; c++) {
            const a0 = Math.floor((c * list.length) / cells);
            const a1 = Math.floor(((c + 1) * list.length) / cells);
            items.push({ i: -1, agg: list.slice(a0, a1), group: groups[g].key, x: gutter + c * (Wc / cells), y, w: Wc / cells - gap, h: bh });
          }
          y += bh + 4;
          continue;
        }
        const rows = Math.ceil((list.length ? u.pos[list[list.length - 1]] + 1 : 0) / rowLen);
        const every = mode === 'boxes' ? 1 : Math.max(1, Math.ceil(12 / (bh + gap)));
        for (let r = 0; r < rows; r++) {
          const ly = y + r * (bh + gap) + bh / 2;
          if (rowLen === 1) labels.push({ x: gutter - 8, y: ly, text: groups[g].label, name: true });
          else if (r % every === 0) labels.push({ x: gutter - 8, y: ly, text: frame ? String(start + r * rowLen * step) : String(r * rowLen + 1), sub: frame && mode === 'boxes' ? `${(r * rowLen) / 24}s` : '' });
        }
        for (const i of list) {
          const p = u.pos[i];
          items.push({ i, x: gutter + (p % rowLen) * (bw + gap), y: y + Math.floor(p / rowLen) * (bh + gap), w: bw, h: bh, label: true });
        }
        y += rows * (bh + gap) + (heads ? 10 : rowLen === 1 ? 4 : 2);
      }
    }
    this.items = items;
    this.labels = labels;
    this.height = Math.max(40, y + 4);
    this.width = W;
    this.gutter = gutter;
  }
  layoutAndDraw() {
    if (!this.d) return;
    this.layout();
    const dpr = window.devicePixelRatio || 1;
    const cv = this.cv;
    if (cv.width !== Math.round(this.width * dpr) || cv.height !== Math.round(this.height * dpr)) {
      cv.width = Math.round(this.width * dpr);
      cv.height = Math.round(this.height * dpr);
      cv.style.height = this.height + 'px';
    }
    this.dpr = dpr;
    this.draw();
  }

  animating(t) {
    if (reduced()) return false;
    this.anims = this.anims.filter((a) => t - a.t0 < a.dur);
    if (this.anims.length) return true;
    for (const p of this.prog.values()) if (t - p.t0 < 900 && p.from !== p.to) return true;
    return false;
  }

  draw() {
    cancelAnimationFrame(this.raf);
    const t = performance.now();
    const ctx = this.ctx;
    const c = colors();
    const dpr = this.dpr;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, this.width, this.height);
    const u = this.u;
    const boxes = this.mode === 'boxes';
    const frameLabels = boxes && this.box.bw >= 24 && this.box.bh >= 12;
    ctx.font = `${Math.min(10, Math.max(8, this.box.bh * 0.42))}px "Martian Mono", ui-monospace, monospace`;
    ctx.textBaseline = 'middle';
    ctx.textAlign = 'center';
    const kidAnim = new Map();
    for (const a of this.anims) {
      const f = ease(Math.min(1, (t - a.t0) / a.dur));
      for (const k of a.kids) kidAnim.set(k, { from: a.from, f });
    }
    for (const it of this.items) {
      if (it.i === -2) continue;
      if (it.i === -1) { this.drawAgg(ctx, it); continue; }
      let { x, y, w, h } = it;
      const an = kidAnim.get(it.i);
      if (an) {
        x = an.from.x + (x - an.from.x) * an.f;
        y = an.from.y + (y - an.from.y) * an.f;
        w = an.from.w + (w - an.from.w) * an.f;
        h = an.from.h + (h - an.from.h) * an.f;
      }
      this.drawUnit(ctx, it.i, x, y, w, h, t, frameLabels && it.label);
    }
    if (this.selected >= 0) {
      const it = this.items.find((x) => x.i === this.selected);
      if (it) {
        ctx.strokeStyle = c.sel;
        ctx.lineWidth = 2;
        ctx.strokeRect(it.x - 1.5, it.y - 1.5, it.w + 3, it.h + 3);
      }
    }
    // Gutter labels.
    for (const l of this.labels) {
      if (l.show === false) continue;
      if (l.head) {
        ctx.textAlign = 'left';
        ctx.fillStyle = c.ink2;
        ctx.font = '600 10px "Archivo", sans-serif';
        const tw = ctx.measureText(l.text.toUpperCase()).width;
        ctx.fillText(l.text.toUpperCase(), l.x, l.y);
        ctx.fillStyle = c.ink3;
        ctx.font = '10px "Martian Mono", monospace';
        ctx.fillText(String(l.count), l.x + tw + 10, l.y);
        continue;
      }
      if (l.name) {
        ctx.textAlign = 'right';
        ctx.fillStyle = c.ink2;
        ctx.font = '500 11px "Archivo", sans-serif';
        let txt = l.text;
        while (txt.length > 4 && ctx.measureText(txt).width > this.gutter - 14) txt = txt.slice(0, -2);
        ctx.fillText(txt === l.text ? txt : txt + '…', l.x, l.y);
        continue;
      }
      ctx.textAlign = l.left ? 'left' : 'right';
      ctx.fillStyle = l.tiny ? c.ink3 : c.ink2;
      ctx.font = l.tiny ? '600 9px "Archivo", sans-serif' : '10px "Martian Mono", monospace';
      ctx.fillText(l.text, l.x, l.sub ? l.y - 5 : l.y);
      if (l.sub) {
        ctx.fillStyle = c.ink3;
        ctx.font = '9px "Martian Mono", monospace';
        ctx.fillText(l.sub, l.x, l.y + 6);
      }
    }
    if (this.animating(t)) this.raf = requestAnimationFrame(() => this.draw());
  }

  drawUnit(ctx, i, x, y, w, h, t, withLabel) {
    const c = colors();
    const u = this.u;
    const st = u.st[i];
    const gen = this.d.groups[u.grp[i]] && this.d.groups[u.grp[i]].generator;
    const p = this.prog.get(i);
    const dpr = this.dpr;
    let labelInk = c.onQueued;
    switch (st) {
      case ST.unevaluated:
        ctx.fillStyle = c.uneval;
        ctx.fillRect(x, y, w, h);
        labelInk = c.onUneval;
        break;
      case ST.blocked:
        ctx.fillStyle = pattern(ctx, 'hatch', dpr);
        ctx.fillRect(x, y, w, h);
        break;
      case ST.succeeded:
        ctx.fillStyle = c.done;
        ctx.fillRect(x, y, w, h);
        labelInk = c.onDone;
        break;
      case ST.failed:
        ctx.fillStyle = c.failed;
        ctx.fillRect(x, y, w, h);
        labelInk = c.ink;
        break;
      default:
        ctx.fillStyle = gen ? c.uneval : c.queued;
        ctx.fillRect(x, y, w, h);
        if (gen) labelInk = c.onUneval;
    }
    if (p && st !== ST.succeeded && st !== ST.failed) {
      const f = Math.max(0, Math.min(1, p.cur(t)));
      ctx.fillStyle = p.measured ? c.done : pattern(ctx, 'stripe', dpr);
      ctx.fillRect(x, y, w * f, h);
    }
    if (st === ST.held) {
      ctx.strokeStyle = c.held;
      ctx.lineWidth = w > 6 ? 1.5 : 1;
      ctx.strokeRect(x + 0.75, y + 0.75, w - 1.5, h - 1.5);
    }
    if (st === ST.retrying) {
      const fails = Math.max(1, u.att[i] - (p ? 1 : 0));
      ctx.fillStyle = c.failed;
      if (w >= 10) {
        for (let k = 0; k < fails; k++) ctx.fillRect(x + 2 + k * 5, y, 3, Math.max(3, h * 0.22));
      } else {
        ctx.fillRect(x, y, w, 1);
      }
    }
    if (u.risk[i] && st !== ST.succeeded && st !== ST.running) {
      ctx.fillStyle = c.risk;
      const rh = h >= 14 ? 3 : 1;
      ctx.fillRect(x, y + h - rh, w, rh);
    }
    if (withLabel && w >= 24) {
      const lab = u.labels[i] || '';
      const m = /^\d+/.exec(lab);
      if (m) {
        ctx.fillStyle = labelInk;
        ctx.fillText(m[0].slice(-4), x + w / 2, gen ? y + 8 : y + h / 2 + 0.5);
      }
    }
  }

  drawAgg(ctx, it) {
    const c = colors();
    const u = this.u;
    const counts = new Uint32Array(9);
    let risk = 0;
    for (const i of it.agg) {
      counts[u.st[i]]++;
      risk += u.risk[i];
    }
    const n = it.agg.length || 1;
    const order = [
      [ST.succeeded, c.done], [ST.running, c.done], [ST.retrying, c.done], [ST.failed, c.failed],
      [ST.queued, c.queued], [ST.held, c.queued], [ST.blocked, 'hatch'], [ST.unevaluated, c.uneval],
    ];
    let y = it.y + it.h;
    for (const [code, col] of order) {
      if (!counts[code]) continue;
      const hh = (counts[code] / n) * it.h;
      ctx.fillStyle = col === 'hatch' ? pattern(ctx, 'hatch', this.dpr) : col;
      if (code === ST.running || code === ST.retrying) ctx.fillStyle = pattern(ctx, 'stripe', this.dpr);
      ctx.fillRect(it.x, y - hh, it.w, hh);
      y -= hh;
    }
    if (risk) {
      ctx.fillStyle = c.risk;
      ctx.fillRect(it.x, it.y + it.h, it.w, 1);
    }
  }

  pick(e) {
    const r = this.cv.getBoundingClientRect();
    const x = e.clientX - r.left;
    const y = e.clientY - r.top;
    for (let k = this.items.length - 1; k >= 0; k--) {
      const it = this.items[k];
      if (x >= it.x - 1 && x <= it.x + it.w + 1 && y >= it.y - 1 && y <= it.y + it.h + 1) return it;
    }
    return null;
  }

  onClick(e) {
    const it = this.pick(e);
    if (!it) return;
    if (it.i >= 0) this.opts.onSelect && this.opts.onSelect(it.i);
    else if (it.group) this.opts.onFocus && this.opts.onFocus({ group: it.group });
  }

  onMove(e) {
    const it = this.pick(e);
    this.cv.style.cursor = it ? 'pointer' : 'default';
    if (!it) { this.tip(null); return; }
    if (it.i >= 0) {
      const p = this.prog.get(it.i);
      const names = ['', 'not yet evaluated', 'waiting', 'blocked by a dependency', 'running', 'retrying', 'done', 'failed', 'held'];
      let text = `${this.u.labels[it.i] || 'unit ' + it.i}: ${names[this.u.st[it.i]]}`;
      if (p) text += `, ${Math.round(p.to * 100)}% ${p.measured ? 'measured' : 'estimated'}`;
      if (this.u.risk[it.i]) text += ', projected after the deadline';
      this.tip(text, e);
    } else if (it.agg) {
      const done = it.agg.filter((i) => this.u.st[i] === ST.succeeded).length;
      this.tip(`${it.group}: ${it.agg.length} units, ${done} done. Click to show this group.`, e);
    } else {
      this.tip(`Show only ${it.group}`, e);
    }
  }

  tip(text, e) {
    const el = this.opts.tip;
    if (!el) return;
    if (!text) { el.hidden = true; return; }
    el.textContent = text;
    el.hidden = false;
    const r = this.cv.parentElement.getBoundingClientRect();
    el.style.left = Math.min(r.width - 220, e.clientX - r.left + 12) + 'px';
    el.style.top = e.clientY - r.top + 14 + 'px';
  }

  onKey(e) {
    const vis = this.items.filter((x) => x.i >= 0);
    if (!vis.length) return;
    let k = vis.findIndex((x) => x.i === this.selected);
    const rowLen = this.box.rowLen || 24;
    const moves = { ArrowRight: 1, ArrowLeft: -1, ArrowDown: rowLen, ArrowUp: -rowLen };
    if (e.key in moves) {
      e.preventDefault();
      k = k < 0 ? 0 : Math.max(0, Math.min(vis.length - 1, k + moves[e.key]));
      this.selected = vis[k].i;
      this.draw();
      this.opts.onCursor && this.opts.onCursor(this.selected);
    } else if (e.key === 'Enter' && k >= 0) {
      this.opts.onSelect && this.opts.onSelect(this.selected);
    } else if (e.key === 'Escape') {
      this.opts.onSelect && this.opts.onSelect(-1);
    }
  }

  destroy() {
    cancelAnimationFrame(this.raf);
    this.ro.disconnect();
  }
}

// drawMini draws a queue row's miniature from a JobSummary, in index order.
export function drawMini(cv, job) {
  const c = colors();
  const dpr = window.devicePixelRatio || 1;
  const W = cv.clientWidth || 220;
  const H = cv.clientHeight || 36;
  if (cv.width !== Math.round(W * dpr)) { cv.width = Math.round(W * dpr); cv.height = Math.round(H * dpr); }
  const ctx = cv.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);
  const st = stateArray(job.unitStates, job.nudge && job.nudge.hold);
  const N = st.length;
  if (!N) return;
  let cell = 0;
  let cols = 0;
  for (let sz = 9; sz >= 2; sz--) {
    cols = Math.floor((W + 1) / sz);
    if (Math.ceil(N / cols) * sz <= H + 1) { cell = sz; break; }
  }
  const fill = (code) => {
    switch (code) {
      case ST.unevaluated: return c.uneval;
      case ST.blocked: return pattern(ctx, 'hatch', dpr);
      case ST.succeeded: return c.done;
      case ST.failed: return c.failed;
      case ST.none: return null;
      default: return c.queued;
    }
  };
  if (cell) {
    const g = cell >= 4 ? 1 : cell >= 3 ? 1 : 0;
    const bw = cell - g;
    for (let i = 0; i < N; i++) {
      const f = fill(st[i]);
      if (!f) continue;
      const x = (i % cols) * cell;
      const y = Math.floor(i / cols) * cell;
      ctx.fillStyle = f;
      ctx.fillRect(x, y, bw, bw);
      if (st[i] === ST.running || st[i] === ST.retrying) {
        ctx.fillStyle = pattern(ctx, 'stripe', dpr);
        ctx.fillRect(x, y + bw / 2, bw, bw / 2);
      }
      if (st[i] === ST.held && bw >= 4) {
        ctx.strokeStyle = c.held;
        ctx.lineWidth = 1;
        ctx.strokeRect(x + 0.5, y + 0.5, bw - 1, bw - 1);
      }
    }
    return;
  }
  // Aggregate: each 3 px cell is a run of k units, showing its mix.
  const sz = 3;
  cols = Math.floor(W / sz);
  const rows = Math.floor(H / sz);
  const k = Math.ceil(N / (cols * rows));
  for (let cI = 0; cI * k < N; cI++) {
    let done = 0, run = 0, fail = 0, unev = 0, blk = 0, n = 0;
    for (let i = cI * k; i < Math.min(N, (cI + 1) * k); i++) {
      const v = st[i];
      if (v === ST.none) continue;
      n++;
      if (v === ST.succeeded) done++;
      else if (v === ST.running || v === ST.retrying) run++;
      else if (v === ST.failed) fail++;
      else if (v === ST.unevaluated) unev++;
      else if (v === ST.blocked) blk++;
    }
    if (!n) continue;
    const x = (cI % cols) * sz;
    const y = Math.floor(cI / cols) * sz;
    ctx.fillStyle = unev > n / 2 ? c.uneval : blk > n / 2 ? pattern(ctx, 'hatch', dpr) : c.queued;
    ctx.fillRect(x, y, sz - 1, sz - 1);
    const fd = (done + run * 0.5) / n;
    if (fd > 0) {
      ctx.fillStyle = c.done;
      ctx.fillRect(x, y + (sz - 1) * (1 - fd), sz - 1, (sz - 1) * fd);
    }
    if (fail) {
      ctx.fillStyle = c.failed;
      ctx.fillRect(x, y, sz - 1, 1);
    }
  }
}
