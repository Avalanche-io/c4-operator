// STACK: the work tree of a shot (or a job) as merged cells, one row per
// unit position (a frame for render), in the order work flows: the shot,
// its batches, then each job's stages from upstream to downstream.
//
// Containment is adjacency. The shot spans its batches; a batch spans
// exactly the rows of its frames; a frame's stages sit side by side in its
// row. No line runs from a container to what it contains. Lines are an
// overlay for what grouping cannot say: A, a dependency on a job outside
// the sheet (on by default); B, a unit that reads other rows (off by
// default, per job).
//
// Any container collapses to one block whose fill is the aggregate of
// everything inside it and upstream of it, by work where the estimate
// exists: green done, a blue sliver running, an orange tick if anything
// inside is retrying, a red tick if anything failed, a dashed red outline if
// any of it is projected late. By default a batch is collapsed when nothing
// in it is moving, so the sheet opens where something is happening.

import { tok, agg, reducedMotion } from './core.js';
import * as f from './fmt.js';
import { ranges } from './rle.js';
import { register, tween, ms } from './live.js';

// ---------------------------------------------------------------- model

const stateOf = (detail) => {
  const m = new Map();
  for (const [k, set] of Object.entries(detail.unitStates || {})) for (const [a, b] of ranges(set)) for (let i = a; i <= b; i++) m.set(i, k);
  if (detail.nudge && detail.nudge.hold) for (const [i, k] of m) if (k === 'queued') m.set(i, 'held');
  return m;
};

// units of a job detail: from unitList, or laid out from groups when the
// list is omitted (very large jobs): the k-th member of a group is at k.
function unitsOf(d) {
  const st = stateOf(d);
  const prog = d.progress || {};
  const risk = new Set();
  for (const [a, b] of ranges(d.riskUnits || '')) for (let i = a; i <= b; i++) risk.add(i);
  const settled = d.phase === 'Succeeded';
  // the deadline: projected late (risk), late at any capacity (willMiss), passed (missed)
  const dl = d.objective && d.objective.deadline ? Date.parse(d.objective.deadline) : NaN;
  const missed = !settled && dl < f.now();
  const willMiss = !!(d.estimate && d.estimate.feasible === false);
  const mk = (u) => ({
    ...u, state: st.get(u.i) || u.state, fraction: prog[u.i] ? prog[u.i].fraction : null, measured: prog[u.i] ? prog[u.i].measured : null,
    risk: risk.has(u.i) || !!u.risk, missed, willMiss, settled, job: `${d.namespace}/${d.name}`, jobName: d.name,
    yielded: d.phase === 'Yielded',
  });
  if (d.unitList && d.unitList.length) return d.unitList.map(mk);
  const out = [];
  for (const g of d.groups || []) {
    let k = 0;
    for (const [a, b] of ranges(g.units)) for (let i = a; i <= b; i++) out.push(mk({ i, group: g.key, pos: k++, label: String(i), attempts: 0 }));
  }
  return out;
}

// groupEdges reduces unit edges to group edges.
function groupEdges(d, byI) {
  const e = new Set();
  for (const x of d.edges || []) {
    const a = byI.get(x.from), b = byI.get(x.to);
    if (a && b && a.group !== b.group) e.add(a.group + '>' + b.group);
  }
  return e;
}

// buildSheet lays a shot's jobs (details, in flow order) onto rows.
// anchor: the key of the job whose axis defines the rows.
// rankRows turns a job whose groups are single units (ML ranks, sweep trials)
// into one group with a row per unit, so ranks read down like frames.
function rankRows(d) {
  const gs = d.groups || [];
  if (gs.length < 2 || !gs.every((g) => unitsCount(g.units) === 1)) return d;
  const units = gs.map((g) => g.units).join(',');
  const label = (d.axis && d.axis.kind) === 'rank' ? 'ranks' : 'units';
  const list = (d.unitList || []).map((u, k) => ({ ...u, group: 'rows', pos: k, label: u.label || gs[k].label }));
  return { ...d, groups: [{ key: 'rows', label, units }], unitList: list.length ? list : d.unitList };
}

export function buildSheet({ key, title, details, anchor, graph, jobs }) {
  details = details.map(rankRows);
  const A = details.find((d) => `${d.namespace}/${d.name}` === anchor) || details[0];
  const aUnits = unitsOf(A);
  // rows: the anchor's widest group (frames for render, shards for CI, ranks for ML)
  const groups = A.groups || [];
  const counts = groups.map((g) => ({ g, n: unitsCount(g.units) }));
  const N = Math.max(1, ...counts.map((c) => c.n));
  const start = A.axis && A.axis.kind === 'frame' ? (A.axis.start || 0) : 0;
  const step = A.axis && A.axis.step ? A.axis.step : 1;
  const axisKind = (A.axis && A.axis.kind) || 'unit';
  // containers: generator groups smaller than a row per unit (the shot, batches)
  const containerGroups = groups.filter((g) => g.generator && unitsCount(g.units) < N);
  const cols = [];
  const sheet = { key, title, N, start, step, axisKind, cols, anchor: `${A.namespace}/${A.name}`, jobs: details.map((d) => `${d.namespace}/${d.name}`), containers: [], edgesOut: [], reads: [] };
  // the shot level: a nested generator group named shot, else the sheet itself
  const byI = new Map(aUnits.map((u) => [u.i, u]));
  const shotG = containerGroups.find((g) => unitsCount(g.units) === 1);
  const batchG = containerGroups.find((g) => unitsCount(g.units) > 1);
  const batchSize = batchG ? Math.ceil(N / unitsCount(batchG.units))
    : Math.max(A.axis && A.axis.batch ? A.axis.batch : 24, Math.ceil(N / 120 / 24) * 24 || 24);
  sheet.batch = batchSize;
  sheet.cost = A.costByFrame ? new Map(A.costByFrame.map((c) => [Math.round((c.frame - start) / step), c])) : null;
  sheet.heavy = (A.heavyRegions || []).map((h) => ({ a: Math.round((h.from - start) / step), z: Math.round((h.to - start) / step), factor: h.factor, label: h.label }));
  sheet.batchFromData = !!batchG;
  sheet.shotUnit = shotG ? aUnits.find((u) => u.group === shotG.key) : null;
  sheet.batchUnits = batchG ? aUnits.filter((u) => u.group === batchG.key).sort((a, b) => a.pos - b.pos) : null;
  // columns, job by job
  for (const d of details) {
    const us = d === A ? aUnits : unitsOf(d);
    const bi = new Map(us.map((u) => [u.i, u]));
    const ge = groupEdges(d, bi);
    const gs = (d.groups || []).filter((g) => !(d === A && containerGroups.includes(g)));
    const jobKey = `${d.namespace}/${d.name}`;
    const used = new Set();
    for (let gi = 0; gi < gs.length; gi++) {
      const g = gs[gi];
      if (used.has(g.key)) continue;
      const n0 = unitsCount(g.units);
      // a group is laid out a unit per row when it has a unit per row, or when
      // its units sit on the anchor's axis at sparse positions (passes of a
      // shot whose generators have not all expanded yet)
      const members0 = us.filter((u) => u.group === g.key);
      const maxPos = Math.max(-1, ...members0.map((u) => u.pos));
      const sameAxis = d.axis && A.axis && d.axis.kind === A.axis.kind;
      const n = n0 === N || (sameAxis && n0 > 1 && maxPos >= n0 && maxPos < N) ? N : n0;
      // lane mates: later groups with the same count, no edge between, a shared successor
      const lanes = [g];
      if (n === N) for (let gj = gi + 1; gj < gs.length; gj++) {
        const h = gs[gj];
        const hn = unitsCount(h.units), hMax = Math.max(-1, ...us.filter((u) => u.group === h.key).map((u) => u.pos));
        const hRow = hn === N || (sameAxis && hn > 1 && hMax >= hn && hMax < N);
        if (used.has(h.key) || !hRow || n !== N || ge.has(g.key + '>' + h.key) || ge.has(h.key + '>' + g.key)) continue;
        const succ = (x) => [...ge].filter((e) => e.startsWith(x + '>')).map((e) => e.split('>')[1]).sort().join();
        if (succ(g.key) && succ(g.key) === succ(h.key)) lanes.push(h);
      }
      lanes.forEach((x) => used.add(x.key));
      const short = d.name.replace(/^sh\d+-|^ep\d+-/, '').replace(/-v\d+$/, '').replace(/-wk\d+$/, '');
      // many parallel groups (a wedge of variants) are named by their short keys
      const label = gs.length === 1 && d !== A ? short : lanes.map((x) => (gs.length > 8 ? x.key : x.label || x.key)).join(' / ');
      const col = { key: jobKey + ':' + lanes.map((x) => x.key).join('+'), job: jobKey, jobName: d.name, phase: d.phase, label,
        lanes: lanes.length, perRow: n === N, cells: [], generator: !!g.generator, settled: d.phase === 'Succeeded' };
      if (n === N) {
        // a unit per row: position on the axis is the row
        const posMap = new Map();
        for (const x of lanes) for (const u of us.filter((u) => u.group === x.key)) {
          const r = Math.min(N - 1, Math.max(0, u.pos));
          if (!posMap.has(r)) posMap.set(r, []);
          posMap.get(r).push({ u, lane: lanes.indexOf(x) });
        }
        for (let r = 0; r < N; r++) {
          const here = posMap.get(r);
          if (!here) { col.cells.push({ a: r, z: r, lane: 0, lanes: 1, u: { state: 'ghost', job: jobKey }, ghost: true }); continue; }
          for (const { u, lane } of here) col.cells.push({ a: r, z: r, lane, lanes: lanes.length, u });
        }
      } else {
        // fewer units than rows: each spans the rows it covers, in order
        const members = us.filter((u) => u.group === g.key).sort((a, b) => a.pos - b.pos || a.i - b.i);
        const per = N / Math.max(1, members.length);
        members.forEach((u, k) => col.cells.push({ a: Math.floor(k * per), z: Math.max(Math.floor(k * per), Math.floor((k + 1) * per) - 1), lane: 0, lanes: 1, u, span: true }));
      }
      for (const c of col.cells) if (c.u.reads) sheet.reads.push({ col: col.key, cell: c });
      cols.push(col);
    }
  }
  // line A: dependencies on jobs outside the sheet, from the job graph
  if (graph) {
    const inSheet = new Set(sheet.jobs);
    for (const e of graph.edges || []) {
      if (inSheet.has(e.to) && !inSheet.has(e.from)) {
        const other = (jobs || []).find((j) => `${j.namespace}/${j.name}` === e.from);
        sheet.edgesOut.push({ to: e.to, from: e.from, slack: other && other.estimate ? other.estimate.slackSeconds : null, phase: other ? other.phase : null, dir: 'in' });
      }
      if (inSheet.has(e.from) && !inSheet.has(e.to)) {
        const other = (jobs || []).find((j) => `${j.namespace}/${j.name}` === e.to);
        sheet.edgesOut.push({ to: e.to, from: e.from, slack: other && other.estimate ? other.estimate.slackSeconds : null, phase: other ? other.phase : null, dir: 'out' });
      }
    }
  }
  return sheet;
}

function unitsCount(set) {
  let n = 0;
  for (const [a, b] of ranges(set || '')) n += b - a + 1;
  return n;
}

// ---------------------------------------------------------------- layout

// unitsIn collects every unit of the given columns over rows a..z.
function unitsIn(sheet, a, z, colFilter) {
  const out = [];
  for (const c of sheet.cols) {
    if (colFilter && !colFilter(c)) continue;
    for (const x of c.cells) if (!x.ghost && x.z >= a && x.a <= z) out.push(x.u);
  }
  return out;
}

// attention says a batch should open by default: something failed or is
// retrying, or it holds a heavy region. Running work alone does not open a
// batch (binary order runs frames in every batch); it shows as the blue
// sliver of the collapsed block.
function attention(sheet, a, z) {
  for (const u of unitsIn(sheet, a, z, (c) => c.perRow)) if (u.state === 'retrying' || u.state === 'failed') return true;
  for (const h of sheet.heavy || []) if (h.z >= a && h.a <= z) return true;
  return false;
}

// layout computes geometry for a view state.
//   v: { box, width, collapsed: Set|null (null = auto), shotCollapsed, upstream: number of leading per-row
//        columns folded into a FRAME aggregate, sel: {job, i} }
export function layout(sheet, v) {
  const b = Math.max(3, v.box || 10);
  const gy = Math.max(1, Math.round(b * 0.18));
  const gap = Math.max(2, Math.round(b * 0.35));
  const head = b >= 7 ? 34 : 4;
  const labelW = b >= 7 ? Math.max(34, Math.round(b * 3.2)) : 6;
  const NB = Math.ceil(sheet.N / sheet.batch);
  const auto = !v.collapsed;
  const collapsed = new Set();
  for (let k = 0; k < NB; k++) {
    const a = k * sheet.batch, z = Math.min(sheet.N, (k + 1) * sheet.batch) - 1;
    const isSel = v.sel && sheet.cols.some((c) => c.job === v.sel.job && c.cells.some((x) => x.u.i === v.sel.i && x.a >= a && x.a <= z));
    if (!v.flat && (auto ? (sheet.N > 120 && !attention(sheet, a, z) && !isSel) : v.collapsed.has(k))) collapsed.add(k);
  }
  const up = Math.max(0, Math.min(v.upstream || 0, sheet.cols.length - 1));
  const folded = sheet.cols.slice(0, up).filter((c) => c.perRow);
  const visCols = sheet.cols.filter((c) => !folded.includes(c));
  // columns
  const W = v.width || 1200;
  const cw = {
    shot: Math.max(b * 1.6, 14), batch: Math.max(b * 2.2, 18), frame: Math.max(b * 3, 24),
  };
  const xs = [];
  let x = labelW;
  const pushCol = (k, w, col) => { xs.push({ k, x, w, col }); x += w + gap; };
  // flat (detail levels below 3): the job's own units only, no shot or batch column
  if (v.flat) { xs.push({ k: 'shot', x, w: 0 }); xs.push({ k: 'batch', x, w: 0 }); } else {
    pushCol('shot', cw.shot);
    pushCol('batch', cw.batch);
  }
  if (folded.length) pushCol('frame', cw.frame);
  const costW = sheet.cost && !v.flat ? Math.min(180, Math.max(90, W * 0.12)) : 0;
  const rest = W - x - 8 - (costW ? costW + gap * 2 : 0);
  const perRowCols = visCols.filter((c) => c.perRow).length, spanCols = visCols.length - perRowCols;
  const unit = Math.max(b * 1.4, Math.min(b * 7, (rest - gap * visCols.length) / Math.max(1, perRowCols * 1.6 + spanCols)));
  for (const c of visCols) pushCol(c.key, Math.round(Math.min(c.perRow ? 150 : 90, c.perRow ? unit * 1.6 : unit)), c);
  const costX = costW ? x + gap : 0;
  // rows
  const lanesAt = new Array(sheet.N).fill(1);
  for (const c of visCols) if (c.perRow && c.lanes > 1) for (const cell of c.cells) if (!cell.ghost) lanesAt[cell.a] = Math.max(lanesAt[cell.a], cell.lanes);
  const rows = new Array(sheet.N);
  const bands = [];
  const bandH = Math.max(b * 2.2, 16);
  let y = head;
  if (v.shotCollapsed) {
    bands.push({ k: -1, a: 0, z: sheet.N - 1, y, h: bandH * 1.6 });
    y += bandH * 1.6 + gap;
  } else {
    for (let k = 0; k < NB; k++) {
      const a = k * sheet.batch, z = Math.min(sheet.N, (k + 1) * sheet.batch) - 1;
      const y0 = y;
      if (collapsed.has(k)) { y += bandH; bands.push({ k, a, z, y: y0, h: bandH, collapsed: true }); }
      else {
        for (let r = a; r <= z; r++) { const h = lanesAt[r] * (b + gy) - gy; rows[r] = { y, h }; y += h + gy * 2; }
        bands.push({ k, a, z, y: y0, h: y - y0, collapsed: false });
      }
      y += gap;
    }
  }
  return { b, gy, gap, head, labelW, xs, rows, bands, folded, visCols, collapsed, H: y + 4, W, up, NB, costX, costW };
}

// ---------------------------------------------------------------- drawing

function rectsOf(sheet, L) {
  // every drawable item with a stable key, so views can tween between layouts
  const out = [];
  const X = (k) => L.xs.find((c) => c.k === k);
  const sx = X('shot'), bx = X('batch');
  if (L.bands.length && L.bands[0].k === -1) {
    const band = L.bands[0], last = L.xs[L.xs.length - 1];
    out.push({ key: 'shot', kind: 'agg', x: sx.x, y: band.y, w: last.x + last.w - sx.x, h: band.h, units: unitsIn(sheet, 0, sheet.N - 1), label: `${sheet.title} · all stages`, toggle: '+', hit: { type: 'shot' } });
    return out;
  }
  const yTop = L.head, yBot = L.bands.length ? L.bands[L.bands.length - 1].y + L.bands[L.bands.length - 1].h : L.head;
  if (sx.w > 0) out.push({ key: 'shot', kind: 'agg', thin: true, x: sx.x, y: yTop, w: sx.w, h: yBot - yTop, units: unitsIn(sheet, 0, sheet.N - 1, (c) => c.perRow), toggle: L.b >= 7 ? '-' : null, label: null, vlabel: sheet.title, hit: { type: 'shot' } });
  const lastPerRow = [...L.xs].reverse().find((c) => c.k === 'frame' || (c.col && c.col.perRow));
  for (const band of L.bands) {
    const bu = sheet.batchUnits ? sheet.batchUnits[band.k] : null;
    const lab = `${f.num(sheet.start + band.a * sheet.step)}–${f.num(sheet.start + band.z * sheet.step)}`;
    if (band.collapsed) {
      out.push({ key: 'b' + band.k, kind: 'agg', x: bx.x, y: band.y, w: lastPerRow.x + lastPerRow.w - bx.x, h: band.h, units: unitsIn(sheet, band.a, band.z, (c) => c.perRow), label: lab, toggle: '+', hit: { type: 'batch', k: band.k }, gen: bu });
    } else {
      if (bx.w > 0) out.push({ key: 'b' + band.k, kind: 'agg', thin: true, x: bx.x, y: band.y, w: bx.w, h: band.h, units: unitsIn(sheet, band.a, band.z, (c) => c.perRow), toggle: L.b >= 7 ? '-' : null, hit: { type: 'batch', k: band.k }, gen: bu });
      for (let r = band.a; r <= band.z; r++) {
        const row = L.rows[r];
        if (!row) continue;
        if (L.b >= 7) out.push({ key: 'l' + r, kind: 'label', x: L.labelW - 6, y: row.y, w: 0, h: row.h, text: String(sheet.start + r * sheet.step) });
        if (L.folded.length) {
          const fc = X('frame');
          out.push({ key: 'f' + r, kind: 'agg', x: fc.x, y: row.y, w: fc.w, h: row.h, units: unitsIn(sheet, r, r, (c) => L.folded.includes(c)), small: true, hit: { type: 'frame', r } });
        }
        for (const c of L.xs) {
          if (!c.col || !c.col.perRow) continue;
          for (const cell of c.col.cells) {
            if (cell.a !== r) continue;
            const lh = (row.h - (cell.lanes - 1) * L.gy) / cell.lanes;
            out.push({ key: c.k + '#' + r + '.' + cell.lane, kind: 'unit', x: c.x, y: row.y + cell.lane * (lh + L.gy), w: c.w, h: lh, u: cell.u, ghost: cell.ghost, hit: cell.ghost ? null : { type: 'unit', job: c.col.job, i: cell.u.i } });
          }
        }
      }
    }
  }
  // spanning cells (transcode segments, delivery, a CI build): merged over the rows they read
  const yAt = (r, end) => {
    const band = L.bands.find((x) => r >= x.a && r <= x.z);
    if (!band) return null;
    if (band.collapsed) return end ? band.y + band.h : band.y;
    const row = L.rows[r];
    return end ? row.y + row.h : row.y;
  };
  for (const c of L.xs) {
    if (!c.col || c.col.perRow) continue;
    for (const cell of c.col.cells) {
      const y0 = yAt(cell.a, false), y1 = yAt(cell.z, true);
      if (y0 === null || y1 === null) continue;
      out.push({ key: c.k + '#' + cell.u.i, kind: 'unit', span: true, x: c.x, y: y0, w: c.w, h: y1 - y0, u: cell.u, hit: { type: 'unit', job: c.col.job, i: cell.u.i } });
    }
  }
  return out;
}

export function drawUnit(g, T, it, t, glowAt) {
  const { x, y, w, h, u } = it;
  if (w <= 0 || h <= 0) return;
  const st = u.state;
  if (it.ghost || st === 'ghost') { g.strokeStyle = T.g[12]; g.setLineDash([2, 2]); g.strokeRect(x + 0.5, y + 0.5, w - 1, h - 1); g.setLineDash([]); return; }
  if (st === 'unevaluated') {
    g.fillStyle = T.g[8]; g.fillRect(x, y, w, h);
    g.fillStyle = T.g[13];
    for (let yy = y + 1; yy < y + h; yy += 3) for (let xx = x + 1 + ((yy - y) % 6 ? 0 : 1.5); xx < x + w; xx += 3) g.fillRect(xx, yy, 1, 1);
    g.strokeStyle = T.g[13]; g.strokeRect(x + 0.5, y + 0.5, w - 1, h - 1);
  } else if (st === 'succeeded') {
    // committed: folded into the output and verified; a finished upstream job is the same green
    g.fillStyle = T.done; g.fillRect(x, y, w, h);
  } else if (st === 'egress') {
    // rendered, still moving: pale green, solid green rising as bytes commit
    g.fillStyle = T.donePale; g.fillRect(x, y, w, h);
    const fc = Math.max(0, Math.min(1, u.fraction || 0));
    g.fillStyle = T.done; g.fillRect(x, y + h - h * fc, w, h * fc);
  } else if (st === 'accepted' || (st === 'failed' && u.accepted)) {
    // a failure resolved by accepting the bad frames: pale red with a check; nobody has to act
    g.fillStyle = T.failPale; g.fillRect(x, y, w, h);
    if (h >= 7 && w >= 10) { const k = Math.min(h, w) * 0.22, cx = x + w / 2, cy = y + h / 2; g.strokeStyle = T.paper; g.lineWidth = 1.5; g.beginPath(); g.moveTo(cx - k, cy); g.lineTo(cx - k * 0.25, cy + k * 0.75); g.lineTo(cx + k * 1.2, cy - k); g.stroke(); g.lineWidth = 1; }
  } else if (st === 'failed') {
    if (u.ack) {
      // acknowledged: pale red with the seal of whoever took it
      g.fillStyle = T.failPale; g.fillRect(x, y, w, h);
      if (h >= 8 && w >= 8) { g.strokeStyle = T.paper; g.lineWidth = 1.2; g.beginPath(); g.arc(x + w / 2, y + h / 2, Math.min(h, w) * 0.22, 0, Math.PI * 2); g.stroke(); g.lineWidth = 1; }
    } else {
      // needs a person: solid red with a white corner flag
      g.fillStyle = T.fail; g.fillRect(x, y, w, h);
      if (h >= 6 && w >= 8) { const k = Math.min(7, h * 0.6, w * 0.4); g.fillStyle = T.paper; g.beginPath(); g.moveTo(x + w, y); g.lineTo(x + w - k, y); g.lineTo(x + w, y + k); g.closePath(); g.fill(); }
    }
  } else if (st === 'preflight') {
    // preparing to render: pale blue with a blue outline, nothing rising yet
    g.fillStyle = T.runPale; g.fillRect(x, y, w, h);
    if (h >= 4) { g.strokeStyle = T.run; g.strokeRect(x + 0.5, y + 0.5, w - 1, h - 1); }
  } else if (st === 'backoff') {
    // a retry scheduled: pale orange with a dashed orange outline
    g.fillStyle = T.tempPale; g.fillRect(x, y, w, h);
    if (h >= 4) { g.strokeStyle = T.temp; g.lineWidth = 1.4; g.setLineDash([3, 2]); g.strokeRect(x + 0.7, y + 0.7, w - 1.4, h - 1.4); g.setLineDash([]); g.lineWidth = 1; }
  } else if (st === 'running' || st === 'retrying') {
    // rendering: bright blue rising from the bottom as the unit renders (its
    // frontier advancing), grey above for what is not rendered yet. Green is
    // only for finished work, pale green only for output still moving.
    const fr = Math.max(0.04, Math.min(1, it.fill != null ? it.fill : (u.fraction == null ? 0.3 : u.fraction)));
    const edge = st === 'retrying' ? T.temp : T.run;
    g.fillStyle = T.g[9]; g.fillRect(x, y, w, h);
    const breathe = reducedMotion() ? 1 : 0.85 + 0.15 * Math.sin(t / 420 + x * 0.01);
    if (h < 6) {
      // too thin for a vertical fill: the blue grows sideways
      g.globalAlpha = breathe; g.fillStyle = T.run; g.fillRect(x, y, w * fr, h); g.globalAlpha = 1;
      if (st === 'retrying') { g.fillStyle = T.temp; g.fillRect(x + w * fr, y, Math.max(2, w * 0.04), h); }
    } else {
      const fh = h * fr;
      g.globalAlpha = breathe; g.fillStyle = T.run; g.fillRect(x, y + h - fh, w, fh); g.globalAlpha = 1;
      if (u.measured === false) {
        // elapsed over the estimate, not measured progress: striped
        g.save(); g.beginPath(); g.rect(x, y + h - fh, w, fh); g.clip();
        g.strokeStyle = T.field; g.globalAlpha = 0.5; g.lineWidth = 1.4;
        g.beginPath(); for (let k = -h; k < w + h; k += 4) { g.moveTo(x + k, y + h); g.lineTo(x + k + h, y); } g.stroke();
        g.restore();
      }
      g.strokeStyle = edge; g.lineWidth = st === 'retrying' ? 1.6 : 1; g.strokeRect(x + 0.5, y + 0.5, w - 1, h - 1); g.lineWidth = 1;
    }
  } else {
    g.fillStyle = T.g[9]; g.fillRect(x, y, w, h);
    if (st === 'blocked') {
      g.save(); g.beginPath(); g.rect(x, y, w, h); g.clip(); g.strokeStyle = T.g[12]; g.beginPath();
      for (let k = -h; k < w + h; k += 5) { g.moveTo(x + k, y); g.lineTo(x + k + h, y + h); } g.stroke(); g.restore();
    }
    // a yielded run keeps the fraction its latest checkpoint saved: finished work, green
    if (u.fraction > 0 && st !== 'blocked') { g.fillStyle = T.done; g.fillRect(x, y + h - h * Math.min(1, u.fraction), w, h * Math.min(1, u.fraction)); }
    if (st === 'held' && h >= 6 && w >= 8) { g.fillStyle = T.g[16]; g.fillRect(x + w / 2 - 2.5, y + h / 2 - 3, 1.5, 6); g.fillRect(x + w / 2 + 1, y + h / 2 - 3, 1.5, 6); }
  }
  // earlier failed attempts of a unit still going: orange notches
  const prior = (u.attempts || 0) - (st === 'running' || st === 'retrying' || st === 'succeeded' || st === 'egress' ? 1 : 0);
  if (st !== 'failed' && !u.yielded && h >= 4) { g.fillStyle = T.temp; for (let k = 0; k < Math.min(prior, 3); k++) g.fillRect(x + w - 3, y + 1 + k * 3.5, 3, 2.2); }
  // the deadline, for waiting work projected after it: a dashed orchid tick
  // at risk; a sparse orchid hatch when it will miss at any capacity; a solid
  // tick once the deadline has passed. (The job's gauge, strip and deadline
  // line carry the strong fills; a sheet of cells stays readable.)
  if (u.risk && (st === 'queued' || st === 'blocked' || st === 'held' || st === 'unevaluated' || st === 'backoff')) {
    if (u.missed) { g.fillStyle = T.risk; g.fillRect(x, y, Math.min(3, w), h); }
    else if (u.willMiss) {
      g.save(); g.beginPath(); g.rect(x, y, w, h); g.clip(); g.globalAlpha = 0.8;
      g.strokeStyle = T.risk; g.lineWidth = 1; g.beginPath(); for (let k = -h; k < w + h; k += 7) { g.moveTo(x + k, y + h); g.lineTo(x + k + h, y); } g.stroke(); g.restore();
      g.fillStyle = T.risk; g.fillRect(x, y, Math.min(2, w), h);
    } else {
      g.strokeStyle = T.risk; g.lineWidth = 1.6; g.setLineDash([2, 1.5]); g.beginPath(); g.moveTo(x + 1, y + 1); g.lineTo(x + 1, y + h - 1); g.stroke(); g.setLineDash([]); g.lineWidth = 1;
    }
  } else if (u.risk && st !== 'succeeded' && st !== 'failed') {
    g.strokeStyle = T.risk; g.lineWidth = 1.6; g.setLineDash(u.missed ? [] : [2, 1.5]); g.beginPath(); g.moveTo(x + 1, y + 1); g.lineTo(x + 1, y + h - 1); g.stroke(); g.setLineDash([]); g.lineWidth = 1;
  }
  // persistence: a unit that just finished glows and settles over a minute
  if (st === 'succeeded' && glowAt) {
    const age = (performance.now() - glowAt) / 60000;
    if (age < 1 && !reducedMotion()) { g.globalAlpha = (1 - age) * 0.9; g.strokeStyle = T.paper; g.strokeRect(x + 0.5, y + 0.5, w - 1, h - 1); g.globalAlpha = 1; }
  }
}

export function drawAgg(g, T, it) {
  const { x, y, w, h } = it;
  const a = agg(it.units);
  g.fillStyle = T.plate2; g.fillRect(x, y, w, h);
  const gw = it.thin ? Math.min(4, w * 0.3) : w;
  const hd = h * a.fDone, hr = a.fRun > 0 ? Math.max(1.5, h * a.fRun) : 0;
  if (a.uneval && !it.thin) { g.fillStyle = T.g[8]; g.fillRect(x, y, w, h * a.uneval / Math.max(1, a.n)); }
  g.fillStyle = T.done; g.fillRect(x, y + h - hd, gw, hd);
  if (hr) {
    // the work in flight: bright blue at the top of the green
    g.fillStyle = T.run; g.fillRect(x, y + h - hd - hr, gw, hr);
  }
  if (!it.small) { g.strokeStyle = T.g[13]; for (const q of [0.25, 0.5, 0.75]) { g.beginPath(); g.moveTo(x, y + h * (1 - q) + 0.5); g.lineTo(x + Math.min(6, w * 0.15), y + h * (1 - q) + 0.5); g.stroke(); } }
  g.strokeStyle = T.g[13]; g.strokeRect(x + 0.5, y + 0.5, w - 1, h - 1);
  let mx = x + w - 5;
  if (a.retrying) { g.fillStyle = T.temp; g.fillRect(mx, y + 2, 4, Math.min(10, h * 0.35)); mx -= 6; }
  if (a.failed) { g.fillStyle = T.fail; g.fillRect(mx, y + 2, 4, Math.min(10, h * 0.35)); }
  if (a.risk) {
    g.strokeStyle = T.risk; g.lineWidth = 1.4; g.setLineDash(a.missed ? [] : h >= 20 ? [3, 2] : [2, 1.5]);
    if (h >= 20) g.strokeRect(x + 0.5, y + 0.5, w - 1, h - 1); else { g.lineWidth = 1.5; g.beginPath(); g.moveTo(x + 1, y + 1); g.lineTo(x + 1, y + h - 1); g.stroke(); g.lineWidth = 1; }
    g.setLineDash([]); g.lineWidth = 1;
  }
  const tx0 = x + (it.toggle ? 18 : 8);
  if (it.label && w >= 70 && h >= 14) {
    const fs = Math.min(12, Math.max(9, h * 0.42));
    g.font = `500 ${fs}px ${T.mono}`; g.fillStyle = T.paper; g.textBaseline = 'middle';
    const pct = `${Math.round(a.fDone * 100)}%`;
    g.fillText(pct, tx0, y + h / 2);
    let tx = tx0 + g.measureText(pct).width + 8;
    if (a.running) { g.fillStyle = T.run; g.font = `400 ${fs * 0.85}px ${T.mono}`; const s = `${a.running} run`; g.fillText(s, tx, y + h / 2); tx += g.measureText(s).width + 8; }
    g.fillStyle = T.ink3; g.font = `400 ${fs * 0.85}px ${T.mono}`; g.fillText(it.label + (a.basis === 'units' ? '  by units' : ''), Math.max(tx, x + w - 8 - g.measureText(it.label).width - (a.basis === 'units' ? 60 : 0)), y + h / 2);
  }
  if (it.vlabel && h > 60 && w >= 12) {
    g.save(); g.translate(x + w / 2 + 3, y + 22); g.rotate(Math.PI / 2);
    g.font = `600 ${Math.min(11, w * 0.6)}px ${T.sans}`; g.fillStyle = T.g[16]; g.textBaseline = 'alphabetic';
    g.fillText(String(it.vlabel).toUpperCase().split('').join(String.fromCharCode(8202)), 0, 0); g.restore();
  }
  if (it.toggle && w >= 14 && h >= 14) {
    g.fillStyle = T.well; g.fillRect(x + 2, y + 2, 11, 11); g.strokeStyle = T.g[14]; g.strokeRect(x + 2.5, y + 2.5, 10, 10);
    g.strokeStyle = T.ink; g.beginPath(); g.moveTo(x + 4.5, y + 7.5); g.lineTo(x + 10.5, y + 7.5);
    if (it.toggle === '+') { g.moveTo(x + 7.5, y + 4.5); g.lineTo(x + 7.5, y + 10.5); }
    g.stroke();
  }
  it._agg = a;
}

// ---------------------------------------------------------------- component

export class Stack {
  constructor(el) {
    this.el = el;
    el.classList.add('stack');
    this.cv = document.createElement('canvas');
    this.cv.setAttribute('role', 'img');
    this.cv.tabIndex = 0;
    this.tip = document.createElement('div');
    this.tip.className = 'tipcard';
    this.tip.hidden = true;
    el.append(this.cv, this.tip);
    this.view = { box: 10, collapsed: null, shotCollapsed: false, upstream: 0, sel: null, lineA: true, lineB: new Set(), contours: true };
    this.items = [];
    this.prev = new Map();
    this.glow = new Map();
    this.anim = null;
    this.hover = null;
    this.raf = 0;
    this.cv.addEventListener('mousemove', (e) => this.onMove(e));
    this.cv.addEventListener('mouseleave', () => { this.hover = null; this.tip.hidden = true; this.paint(); });
    this.cv.addEventListener('click', (e) => this.onClick(e));
    this.cv.addEventListener('keydown', (e) => this.onKey(e));
    this.ro = new ResizeObserver(() => this.relayout(false));
    this.ro.observe(el);
    this.raf = 0;
  }

  // kick runs the paint loop only while something moves: a tween, a running
  // unit breathing, a finish still glowing. Idle sheets cost nothing.
  kick() {
    if (this.raf) return;
    const loop = (t) => {
      this.t = t;
      this.paint();
      this.raf = this.live() ? requestAnimationFrame(loop) : 0;
    };
    this.raf = requestAnimationFrame(loop);
  }

  live() {
    if (reducedMotion() || !this.items.length) return !!this.anim;
    return this.anim || this.items.some((it) => it.kind === 'unit' && it.u && (it.u.state === 'running' || it.u.state === 'retrying')) || this.glow.size > 0;
  }

  update({ sheet, view = {} }) {
    const T = tok();
    // remember finishes for the persistence glow
    if (this.sheet) {
      const was = new Map();
      for (const it of this.items) if (it.kind === 'unit' && it.u) was.set(it.u.job + '#' + it.u.i, it.u.state);
      for (const c of sheet.cols) for (const cell of c.cells) {
        const k = cell.u.job + '#' + cell.u.i;
        if (cell.u.state === 'succeeded' && was.has(k) && was.get(k) !== 'succeeded') this.glow.set(k, performance.now());
      }
      for (const [k, t] of this.glow) if (performance.now() - t > 60000) this.glow.delete(k);
    }
    const sheetChanged = !this.sheet || this.sheet.key !== sheet.key;
    if (sheetChanged) { this.view.collapsed = null; this.view.upstream = view.upstream || 0; this.view.shotCollapsed = false; }
    Object.assign(this.view, view);
    this.sheet = sheet;
    this.cv.setAttribute('aria-label', `${sheet.title}: ${sheet.N} rows, ${sheet.cols.length} stages`);
    this.relayout(!sheetChanged);
    void T;
  }

  relayout(animate) {
    if (!this.sheet) return;
    const width = Math.max(320, this.el.clientWidth || 1200);
    const L = layout(this.sheet, Object.assign({}, this.view, { width }));
    const items = rectsOf(this.sheet, L);
    // fills tween from their last value
    for (const it of items) {
      if (it.kind !== 'unit' || !it.u) continue;
      const p = this.prev.get(it.key);
      const target = it.u.state === 'succeeded' ? 1 : it.u.fraction == null ? 0.3 : it.u.fraction;
      it.fill0 = p && p.u && (p.u.state === 'running' || p.u.state === 'retrying') ? (p.fill != null ? p.fill : p.u.fraction) : target;
      it.fill1 = target;
    }
    const from = new Map(this.items.map((it) => [it.key, it]));
    this.L = L;
    this.items = items;
    this.cv.width = Math.round(width * devicePixelRatio);
    this.cv.height = Math.round(L.H * devicePixelRatio);
    this.cv.style.width = width + 'px';
    this.cv.style.height = L.H + 'px';
    if (this.anim) this.anim.stop();
    const dur = animate ? ms('--collapse-ms', 180) : 0;
    const fillDur = ms('--fill-ms', 600);
    const geom = items.map((it) => {
      const o = from.get(it.key);
      return o ? { x: o.x, y: o.y, w: o.w, h: o.h } : null;
    });
    const t0 = performance.now();
    const total = Math.max(dur, animate ? fillDur : 0);
    if (!total) { for (const it of items) if (it.kind === 'unit') it.fill = it.fill1; this.anim = null; this.prev = new Map(items.map((it) => [it.key, it])); this.paint(); this.kick(); return; }
    const ease = (t) => 1 - Math.pow(1 - t, 3);
    const end = items.map((it) => ({ x: it.x, y: it.y, w: it.w, h: it.h }));
    let stopped = false;
    this.anim = { stop: () => { stopped = true; } };
    const step = () => {
      if (stopped) return;
      const now = performance.now();
      const tg = dur ? ease(Math.min(1, (now - t0) / dur)) : 1;
      const tf = ease(Math.min(1, (now - t0) / fillDur));
      items.forEach((it, k) => {
        const a = geom[k], e = end[k];
        if (a) { it.x = a.x + (e.x - a.x) * tg; it.y = a.y + (e.y - a.y) * tg; it.w = a.w + (e.w - a.w) * tg; it.h = a.h + (e.h - a.h) * tg; it.alpha = 1; }
        else it.alpha = tg;
        if (it.kind === 'unit') it.fill = it.fill0 + (it.fill1 - it.fill0) * tf;
      });
      if (now - t0 < total) requestAnimationFrame(step);
      else { this.anim = null; }
    };
    step();
    this.prev = new Map(items.map((it) => [it.key, it]));
    this.kick();
  }

  paint() {
    if (!this.L) return;
    const T = tok();
    const g = this.cv.getContext('2d');
    const dpr = devicePixelRatio;
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    g.clearRect(0, 0, this.cv.width, this.cv.height);
    const L = this.L;
    // column headers and fold controls
    if (L.head > 8) {
      g.textBaseline = 'alphabetic';
      for (const c of L.xs) {
        const lab = c.k === 'shot' ? 'SHOT' : c.k === 'batch' ? (this.sheet.batchFromData ? 'BATCH' : 'BATCH') : c.k === 'frame' ? 'UPSTREAM' : c.col.label.toUpperCase();
        if (c.w + L.gap < 20) continue;
        g.font = `600 9px ${T.sans}`; g.fillStyle = T.ink2;
        clipText(g, lab, c.x, 11, c.w + L.gap - 2);
        g.font = `400 8px ${T.mono}`; g.fillStyle = T.ink4;
        const sub = c.k === 'shot' ? (this.sheet.shotUnit ? 'generator' : 'sheet') : c.k === 'batch' ? (this.sheet.batchFromData ? 'generator' : `${this.sheet.batch} per batch`) : c.k === 'frame' ? 'aggregate' : c.col.jobName.replace(/^sh\d+-/, '');
        clipText(g, sub, c.x, 22, c.w + L.gap - 2);
        if (c.col && c.col.perRow && c.w >= 18) { // fold-upstream control: folds this column and everything left of it
          const fx = c.x + c.w - 9, fy = 25;
          g.strokeStyle = T.g[13]; g.strokeRect(fx + 0.5, fy + 0.5, 8, 6);
          g.fillStyle = T.ink3; g.fillRect(fx + 2, fy + 3, 5, 1);
          c.fold = { x: fx, y: fy, w: 9, h: 7 };
        }
      }
    }
    for (const it of this.items) {
      g.globalAlpha = it.alpha == null ? 1 : it.alpha;
      if (it.kind === 'agg') drawAgg(g, T, it);
      else if (it.kind === 'unit') drawUnit(g, T, it, this.t || 0, this.glow.get(it.u.job + '#' + it.u.i));
      else if (it.kind === 'label') { g.font = `400 ${Math.min(10, Math.max(7, L.b * 0.6))}px ${T.mono}`; g.fillStyle = T.ink4; g.textAlign = 'right'; g.textBaseline = 'middle'; g.fillText(it.text, it.x, it.y + it.h / 2); g.textAlign = 'left'; }
      g.globalAlpha = 1;
    }
    this.paintCost(g, T);
    this.paintOverlays(g, T);
    // selection: a paper outline
    const sel = this.view.sel;
    if (sel) for (const it of this.items) if (it.hit && it.hit.type === 'unit' && it.hit.job === sel.job && it.hit.i === sel.i) {
      g.strokeStyle = T.paper; g.lineWidth = 1.5; g.strokeRect(it.x - 1.5, it.y - 1.5, it.w + 3, it.h + 3); g.lineWidth = 1;
    }
    if (this.hover && this.hover.hit && this.hover.kind === 'agg') { g.strokeStyle = T.g[17]; g.strokeRect(this.hover.x - 0.5, this.hover.y - 0.5, this.hover.w + 1, this.hover.h + 1); }
  }

  // cost per frame: measured seconds in ink, estimates as a pencil band with
  // their estimate tick; heavy regions bracketed with their factor
  paintCost(g, T) {
    const L = this.L, S = this.sheet;
    if (!S.cost || !L.costW) return;
    const x0 = L.costX, w = L.costW;
    let max = 1;
    for (const c of S.cost.values()) max = Math.max(max, c.seconds || c.high || c.estimate || 0);
    if (L.head > 8) {
      g.font = `600 9px ${T.sans}`; g.fillStyle = T.ink2; g.textBaseline = 'alphabetic'; g.fillText('COST / FRAME', x0, 11);
      g.font = `400 8px ${T.mono}`; g.fillStyle = T.ink4; g.fillText(`solid measured · light estimated · max ${f.dur(max)}`, x0, 22);
    }
    for (let r = 0; r < S.N; r++) {
      const row = L.rows[r], c = S.cost.get(r);
      if (!row || !c) continue;
      const my = row.y + row.h / 2, bh = Math.max(1, Math.min(row.h * 0.5, 4));
      if (c.seconds) { g.fillStyle = T.ink; g.fillRect(x0, my - bh / 2, (w * c.seconds) / max, bh); }
      else {
        g.strokeStyle = T.g[12]; g.lineWidth = bh; g.beginPath(); g.moveTo(x0 + (w * (c.low || 0)) / max, my); g.lineTo(x0 + (w * (c.high || 0)) / max, my); g.stroke(); g.lineWidth = 1;
        g.fillStyle = T.pencil; g.fillRect(x0 + (w * c.estimate) / max - 0.75, my - bh, 1.5, bh * 2);
      }
    }
    for (const hz of S.heavy || []) {
      const ys = [];
      for (let r = hz.a; r <= hz.z; r++) if (L.rows[r]) ys.push(L.rows[r].y, L.rows[r].y + L.rows[r].h);
      if (!ys.length) continue;
      const y0 = Math.min(...ys), y1 = Math.max(...ys);
      g.strokeStyle = T.ink2; g.beginPath(); g.moveTo(x0 - 4, y0); g.lineTo(x0 - 7, y0); g.lineTo(x0 - 7, y1); g.lineTo(x0 - 4, y1); g.stroke();
      g.save(); g.translate(x0 - 10, y0); g.rotate(Math.PI / 2); g.font = `400 9px ${T.mono}`; g.fillStyle = T.ink2; g.textBaseline = 'alphabetic';
      g.fillText(`${hz.factor}× median${hz.label ? ' · ' + hz.label : ''}`, 0, 0); g.restore();
    }
  }

  paintOverlays(g, T) {
    const L = this.L, S = this.sheet;
    // line A: the sheet's dependencies on jobs outside it, to the edge
    if (this.view.lineA && S.edgesOut.length) {
      let k = 0;
      for (const e of S.edgesOut) {
        const cells = this.items.filter((it) => it.kind === 'unit' && it.u && it.u.job === (e.dir === 'in' ? e.to : e.from));
        if (!cells.length) continue;
        const c = cells.reduce((a, b) => (b.x > a.x ? b : a));
        const ey = c.y + Math.min(c.h / 2, 14) + k * 12;
        const xe = L.W - 4;
        const late = Number.isFinite(e.slack) && e.slack < 0;
        g.strokeStyle = T.ink; g.lineWidth = 1.2;
        g.beginPath(); g.moveTo(c.x + c.w, ey); g.lineTo(xe, ey); g.stroke(); g.lineWidth = 1;
        g.fillStyle = T.ink; g.beginPath(); g.arc(c.x + c.w, ey, 2.5, 0, Math.PI * 2); g.fill();
        g.font = `400 9px ${T.mono}`; g.textAlign = 'right'; g.textBaseline = 'bottom';
        g.fillStyle = late ? T.risk : T.ink2;
        g.fillText(`${e.dir === 'in' ? 'also needs' : 'feeds'} ${f.jobName(e.dir === 'in' ? e.from : e.to)}${Number.isFinite(e.slack) ? ' ' + f.signed(e.slack) : ''}`, xe, ey - 2);
        g.textAlign = 'left';
        k++;
      }
    }
    // line B: units that read other rows (per job, off by default)
    if (this.view.lineB.size && S.reads.length) {
      // every reading unit gets a tick; the bracket to its inputs is drawn for
      // the hovered or selected unit only, so a temporal comp (every frame
      // reading its neighbours) stays legible
      const focus = (it) => (this.hover && this.hover.it === it) || (this.hover === it) ||
        (this.view.sel && it.u && this.view.sel.job === it.u.job && this.view.sel.i === it.u.i);
      g.lineWidth = 1;
      for (const { cell } of S.reads) {
        if (!this.view.lineB.has(cell.u.job)) continue;
        const me = this.items.find((it) => it.kind === 'unit' && it.u === cell.u);
        if (!me) continue;
        g.fillStyle = T.g[16]; g.fillRect(me.x - 3, me.y + me.h / 2 - 0.5, 3, 1);
        if (!focus(me)) continue;
        g.strokeStyle = T.paper;
        for (const rd of cell.u.reads) {
          const idx = new Set();
          for (const [a, b] of ranges(rd.units)) for (let i = a; i <= b; i++) idx.add(i);
          const src = this.items.filter((it) => it.kind === 'unit' && it.u && it.u.job === rd.job && idx.has(it.u.i));
          if (src.length < 2) continue;
          const bx = me.x - 3;
          const ys = src.map((s) => s.y + s.h / 2);
          g.beginPath();
          for (const s of src) { g.moveTo(s.x + s.w + 1, s.y + s.h / 2); g.lineTo(bx, s.y + s.h / 2); }
          g.moveTo(bx, Math.min(...ys)); g.lineTo(bx, Math.max(...ys));
          g.moveTo(bx, me.y + me.h / 2); g.lineTo(me.x, me.y + me.h / 2);
          g.stroke();
        }
      }
    }
    // contours: projected finish lines across the rows, and the deadline datum
    if (this.view.contours && this.view.deadline) this.paintContours(g, T);
  }

  paintContours(g, T) {
    // for each open row, the time a contour crosses is interpolated between stages' unit etas
    const rowsEta = new Map();
    for (const it of this.items) {
      if (it.kind !== 'unit' || !it.u || !it.u.eta || it.span) continue;
      const r = Math.round(it.y);
      if (!rowsEta.has(r)) rowsEta.set(r, []);
      rowsEta.get(r).push({ x: it.x + it.w / 2, t: Date.parse(it.u.eta), y: it.y + it.h / 2 });
    }
    if (rowsEta.size < 3) return;
    const levels = [];
    const now = f.now();
    const dl = this.view.deadline;
    for (let t = Math.ceil(now / 9e5) * 9e5; t < dl + 3 * 36e5; t += 9e5) levels.push(t);
    const rows = [...rowsEta.entries()].sort((a, b) => a[0] - b[0]);
    const draw = (lev, dl2) => {
      // where the contour crosses each row, then a moving average over nine
      // rows: binary order makes neighbouring frames finish far apart
      const xs = rows.map(([, pts]) => {
        pts.sort((a, b) => a.x - b.x);
        for (let k = 0; k + 1 < pts.length; k++) {
          const a = pts[k], b = pts[k + 1];
          if ((a.t <= lev && b.t >= lev) || (a.t >= lev && b.t <= lev)) return a.x + (b.x - a.x) * ((lev - a.t) / ((b.t - a.t) || 1));
        }
        return null;
      });
      g.beginPath();
      let on = false;
      rows.forEach(([, pts], k) => {
        if (xs[k] === null) { on = false; return; }
        let sum = 0, n = 0;
        for (let j = Math.max(0, k - 4); j <= Math.min(rows.length - 1, k + 4); j++) if (xs[j] !== null) { sum += xs[j]; n++; }
        const x = sum / n;
        if (!on) { g.moveTo(x, pts[0].y); on = true; } else g.lineTo(x, pts[0].y);
      });
      if (dl2) { g.strokeStyle = T.risk; g.setLineDash([4, 3]); g.lineWidth = 1.4; } else { g.strokeStyle = T.g[14]; g.setLineDash([1, 3]); g.lineWidth = 1; }
      g.stroke(); g.setLineDash([]); g.lineWidth = 1;
    };
    for (const lev of levels) draw(lev, false);
    draw(dl, true);
  }

  at(e) {
    const r = this.cv.getBoundingClientRect();
    const x = e.clientX - r.left, y = e.clientY - r.top;
    for (let k = this.items.length - 1; k >= 0; k--) {
      const it = this.items[k];
      if (it.hit && x >= it.x && x <= it.x + it.w && y >= it.y && y <= it.y + it.h) return { it, x, y };
    }
    for (const c of (this.L ? this.L.xs : [])) if (c.fold && x >= c.fold.x - 2 && x <= c.fold.x + c.fold.w + 2 && y >= c.fold.y - 2 && y <= c.fold.y + c.fold.h + 2) return { fold: c, x, y };
    return { x, y };
  }

  onMove(e) {
    const { it, fold, x, y } = this.at(e);
    this.cv.style.cursor = it || fold ? 'pointer' : 'default';
    if (this.hover !== it) { this.hover = it || null; this.paint(); }
    if (!it) { this.tip.hidden = true; if (fold) this.showTip(x, y, `<b>FOLD UPSTREAM</b><br>${fold.col.label} and every stage before it collapse into each frame's aggregate`); return; }
    this.showTip(x, y, this.tipFor(it));
  }

  showTip(x, y, html) {
    this.tip.innerHTML = html;
    this.tip.hidden = false;
    const w = this.el.clientWidth;
    this.tip.style.left = Math.min(w - 260, x + 16) + 'px';
    this.tip.style.top = y + 14 + 'px';
  }

  tipFor(it) {
    if (it.kind === 'agg') {
      const a = it._agg || agg(it.units);
      const what = it.hit.type === 'shot' ? `${this.sheet.title} · the shot` : it.hit.type === 'batch' ? `batch ${it.label || ''}` : `frame ${this.sheet.start + it.hit.r * this.sheet.step} · upstream`;
      return `<b>${f.esc ? f.esc(what) : what}</b><br>${Math.round(a.fDone * 100)}% done ${a.basis === 'units' ? '(by units)' : '(by work)'} · ${a.n} units`
        + `${a.running ? `<br><span class="t-run">${a.running} running</span>` : ''}${a.retrying ? ` · <span class="t-temp">${a.retrying} retrying</span>` : ''}${a.failed ? ` · <span class="t-fail">${a.failed} failed</span>` : ''}`
        + `${a.risk ? `<br><span class="t-risk pencil">${a.risk} projected after the deadline</span>` : ''}`
        + `${it.gen ? `<br><span class="dim">generator ${it.gen.label || it.gen.i} · ${it.gen.state}</span>` : it.hit.type === 'batch' && !this.sheet.batchFromData ? `<br><span class="dim">${this.sheet.batch} frames, a display grouping</span>` : ''}`
        + `<br><span class="dim">${it.toggle === '+' ? 'click or Enter expands' : 'click collapses'}</span>`;
    }
    const u = it.u;
    const st = u.state === 'retrying' ? '<span class="t-temp">retrying</span>' : u.state === 'running' ? '<span class="t-run">running</span>' : u.state === 'failed' ? (u.ack ? '<span class="t-fail">failed, taken</span>' : '<span class="t-fail">failed: needs you</span>') : u.state;
    return `<b>${u.label || u.i} · ${u.jobName}</b><br>${st}${u.fraction != null && (u.state === 'running' || u.state === 'retrying') ? ` ${Math.round(u.fraction * 100)}% <span class="dim">${u.measured === false ? 'estimated' : 'measured'}</span>` : ''}`
      + `<br><span class="dim">attempt</span> ${u.attempts || 0}${u.node ? ` <span class="dim">on</span> ${u.node}` : ''}`
      + `${u.work ? `<br><span class="dim">work</span> ${f.dur(u.work.spent || 0)} of ${f.dur(u.work.expected || 0)} slot-time` : ''}`
      + `${u.eta && u.state !== 'succeeded' ? `<br><span class="dim">projected</span> <span class="pencil">${f.clock(u.eta, false)}</span>` : ''}`
      + `${u.risk ? `<br><span class="t-risk${u.missed ? '' : ' pencil'}">${u.missed ? 'after the deadline, which has passed' : u.willMiss ? 'will miss the deadline at any capacity' : 'projected after the deadline'}</span>` : ''}`
      + `${u.reads ? `<br><span class="dim">reads</span> ${u.reads.map((r) => `${f.jobName(r.job)} ${r.units}`).join(', ')}` : ''}`;
  }

  onClick(e) {
    const { it, fold } = this.at(e);
    if (fold) {
      const idx = this.sheet.cols.findIndex((c) => c.key === fold.k);
      this.view.upstream = this.view.upstream === idx + 1 ? 0 : idx + 1;
      this.relayout(true);
      this.el.dispatchEvent(new CustomEvent('stack:view', { bubbles: true, detail: { upstream: this.view.upstream } }));
      return;
    }
    if (!it) return;
    if (it.hit.type === 'shot') { this.view.shotCollapsed = !this.view.shotCollapsed; this.relayout(true); return; }
    if (it.hit.type === 'batch') {
      if (!this.view.collapsed) this.view.collapsed = new Set(this.L.collapsed);
      if (this.view.collapsed.has(it.hit.k)) this.view.collapsed.delete(it.hit.k); else this.view.collapsed.add(it.hit.k);
      if (e.altKey) this.view.collapsed = this.view.collapsed.size ? new Set() : new Set([...Array(this.L.NB).keys()]);
      this.relayout(true);
      return;
    }
    if (it.hit.type === 'frame') { this.view.upstream = 0; this.relayout(true); return; }
    if (it.hit.type === 'unit') {
      this.view.sel = { job: it.hit.job, i: it.hit.i };
      this.paint();
      this.el.dispatchEvent(new CustomEvent('stack:select', { bubbles: true, detail: { job: it.hit.job, i: it.hit.i } }));
    }
  }

  onKey(e) {
    if (e.key === '+' || e.key === '=') { this.view.box = Math.min(32, this.view.box + 2); this.relayout(false); }
    else if (e.key === '-') { this.view.box = Math.max(3, this.view.box - 2); this.relayout(false); }
    else if (e.key === 'Escape') { this.view.sel = null; this.paint(); }
    else return;
    e.preventDefault();
  }

  zoom(box) { this.view.box = box; this.relayout(false); }
  toggleLineB(job) { if (this.view.lineB.has(job)) this.view.lineB.delete(job); else this.view.lineB.add(job); this.paint(); }
  destroy() { cancelAnimationFrame(this.raf); this.ro.disconnect(); }
}

function clipText(g, s, x, y, w) {
  let t = s;
  while (t.length > 1 && g.measureText(t).width > w) t = t.slice(0, -1);
  g.fillText(t === s ? s : t.slice(0, -1) + '…', x, y);
}

register('stack', Stack);
