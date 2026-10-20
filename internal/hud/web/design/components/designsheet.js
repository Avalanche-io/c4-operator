// designSheet: a catalogue design's plan (a plan written as data: blocks,
// each in a group, with the blocks it runs after) drawn as the HUD draws a
// plan job (plansheet.js), before anything runs. Rows are blocks, in the
// order they can run; each block sits in its group's column, so the graph
// reads left to right as a staircase; its dependencies are arcs in the
// gutter. Every block is grey: queued, nothing started; a wait for a
// person or another party is drawn as a wait, with whom it waits for.
//
// A generator (a block with a fan-out) decides at run time which blocks
// follow it. They are one expected row after it, "+ 3 to 10 workers", in a
// column of their own; its dependents wait for them, since children
// inherit their generator's dependents (DESIGN 10.6).
//
// The result is a job detail's shape, for buildSheet.

// order: blocks so that each comes after the blocks it runs after, keeping
// the file's order where the graph allows
function order(units) {
  const byName = new Map(units.map((u) => [u.name, u]));
  const out = [], seen = new Set(), busy = new Set();
  const visit = (u) => {
    if (seen.has(u.name) || busy.has(u.name)) return;
    busy.add(u.name);
    for (const a of u.after || []) if (byName.has(a)) visit(byName.get(a));
    busy.delete(u.name);
    seen.add(u.name);
    out.push(u);
  };
  for (const u of units) visit(u);
  return out;
}

// what a block is, in the HUD's words: a check (a reviewer, with its
// bound), a wait for someone, or work
function kindOf(u) {
  const p = u.params || {};
  if (p.review === 'true' || p.role === 'verdict') return 'check';
  if (u.wait || p.server) return 'wait';
  return 'work';
}

export const ADDED = 'added at run time';

export function designSheet(doc, { name = 'design' } = {}) {
  const blocks = order((doc && doc.units) || []);
  // the rows: each block, and after a generator, what it adds
  const rows = [];
  for (const u of blocks) {
    rows.push({ u, group: u.group || u.key || 'blocks' });
    const fan = u.params && u.params.fanOut;
    if (fan) rows.push({ adds: u, group: ADDED, name: `${u.name} adds`, label: `+ ${fan}` });
  }
  const rowOf = new Map(rows.map((r, k) => [r.u ? r.u.name : r.name, k]));
  const addsOf = new Map(rows.filter((r) => r.adds).map((r) => [r.adds.name, r.name]));
  const groups = [];
  for (const r of rows) if (!groups.find((g) => g.key === r.group)) groups.push({ key: r.group, label: r.group, units: '', perBlock: true, sparse: true });
  const unitList = rows.map((r, k) => {
    if (r.adds) {
      return { i: k, name: r.name, label: r.label, group: r.group, pos: k, state: 'planned',
        design: { kind: 'added', generator: r.adds.name, fanOut: r.adds.params.fanOut, holds: [], why: [] } };
    }
    const u = r.u, p = u.params || {};
    const bound = p.maxPasses ? `${p.maxPasses} passes, then ${String(p.onExhausted || 'a person').replace(/^escalate:/, '')}` : '';
    const kind = kindOf(u);
    return {
      i: k, name: u.name, label: u.label || u.name, group: r.group, pos: k, state: kind === 'wait' ? 'waits' : 'queued',
      reasoning: u.reasoning && (u.reasoning.does === 'model' || u.reasoning.does === 'procedure') ? u.reasoning : undefined,
      design: { key: u.key, kind, server: (u.wait && u.wait.server) || p.server, ask: u.wait && u.wait.ask, role: p.role, note: p.note, fanOut: p.fanOut, bound, holds: u.holds || [], why: (u.cites || []).map((c) => ({ after: c.after, text: c.text })) },
    };
  });
  for (const g of groups) g.units = unitList.filter((u) => u.group === g.key).map((u) => u.i).join(',');
  // edges: a dependent of a generator waits for what it adds
  const pairs = [];
  for (const u of blocks) for (const a of u.after || []) {
    if (!rowOf.has(a)) continue;
    pairs.push([rowOf.get(addsOf.get(a) || a), rowOf.get(u.name)]);
  }
  for (const [g, x] of addsOf) pairs.push([rowOf.get(g), rowOf.get(x)]);
  return {
    namespace: 'catalog', name, phase: 'Pending', axis: { kind: 'block' },
    groups, unitList, edges: pairs.map(([from, to]) => ({ from, to })),
    rowLabels: rows.map((r) => (r.u ? r.u.label || r.u.name : r.label)), laneEdges: pairs,
  };
}
