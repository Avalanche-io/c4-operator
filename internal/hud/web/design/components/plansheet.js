// planSheet: a plan job drawn as a multi-stage render (Shots mode and up,
// from level 2). Rows are its blocks, one per charter; columns are its step
// kinds (charter, build, check, review, refute, merge) in the order work
// flows; waits are gates. A block's dependencies on other blocks become
// laneEdges, drawn by the stack as arcs in a gutter beside the block names:
// the DAG on display, the way a render sheet shows sim, gen, render, comp
// and transcode.
//
// The plan's units already carry the shape: groups are step kinds, a step's
// name is <block>/<step>, and the edges are in the job detail.

// blockOf: a step's block, from its name: text/scan/translate is text/scan's
export const blockOf = (u) => {
  if (u.group === 'charter') return u.name;
  const p = String(u.name || '').split('/');
  p.pop();
  return p.join('/');
};

// the step kinds in flow order; a kind not named here goes after them
const ORDER = ['charter', 'build', 'check', 'review', 'refute', 'merge', 'wait'];
// waits are gates between the stages, never a stage (stack.js gateOf)
const WAITS = /^(waits?|gates?)$/i;
const rank = (g) => (ORDER.indexOf(g.key) + 1) || ORDER.length + 1;

export function planSheet(d) {
  const list = d.unitList || [];
  const charters = list.filter((u) => u.group === 'charter').sort((a, b) => a.pos - b.pos || a.i - b.i);
  const row = new Map(charters.map((u, k) => [u.name, k]));
  const byI = new Map(list.map((u) => [u.i, u]));
  const unitList = list.map((u) => {
    const r = row.get(blockOf(u));
    return r == null ? u : { ...u, pos: r };
  });
  // a dependency is an edge into another block's charter, once per pair
  const seen = new Set(), laneEdges = [];
  for (const e of d.edges || []) {
    const a = byI.get(e.from), b = byI.get(e.to);
    if (!a || !b || b.group !== 'charter') continue;
    const ra = row.get(blockOf(a)), rb = row.get(blockOf(b));
    if (ra == null || rb == null || ra === rb) continue;
    const k = ra + '>' + rb;
    if (seen.has(k)) continue;
    seen.add(k);
    laneEdges.push([ra, rb]);
  }
  // a group whose every step belongs to a block sits a step per block row,
  // however few blocks have expanded yet; waits stay gates
  const placed = new Set(unitList.filter((u) => row.has(blockOf(u))).map((u) => u.i));
  const groups = [...(d.groups || [])].sort((a, b) => rank(a) - rank(b)).map((g) => {
    const members = unitList.filter((u) => u.group === g.key);
    return members.length && !WAITS.test(g.key || '') && members.every((u) => placed.has(u.i)) ? { ...g, perBlock: true } : g;
  });
  return { ...d, unitList, groups, axis: { kind: 'block' }, rowLabels: charters.map((u) => u.name), laneEdges };
}
