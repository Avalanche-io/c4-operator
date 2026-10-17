// Run-length index sets: "0-99,102".

export function ranges(str) {
  const out = [];
  if (!str) return out;
  for (const part of str.split(',')) {
    if (!part) continue;
    const dash = part.indexOf('-');
    const a = Number(dash < 0 ? part : part.slice(0, dash));
    const b = dash < 0 ? a : Number(part.slice(dash + 1));
    if (Number.isFinite(a) && Number.isFinite(b)) out.push([a, b]);
  }
  return out;
}

export function count(str) {
  let n = 0;
  for (const [a, b] of ranges(str)) n += b - a + 1;
  return n;
}

export function maxIndex(str) {
  const r = ranges(str);
  return r.length ? r[r.length - 1][1] : -1;
}

export function toArray(str) {
  const out = [];
  for (const [a, b] of ranges(str)) for (let i = a; i <= b; i++) out.push(i);
  return out;
}

export function encode(idx) {
  const v = [...idx].sort((x, y) => x - y);
  const parts = [];
  for (let i = 0; i < v.length; ) {
    let j = i;
    while (j + 1 < v.length && v[j + 1] === v[j] + 1) j++;
    parts.push(i === j ? `${v[i]}` : `${v[i]}-${v[j]}`);
    i = j + 1;
  }
  return parts.join(',');
}

// State codes shared by the grid renderers.
export const ST = { none: 0, unevaluated: 1, queued: 2, blocked: 3, running: 4, retrying: 5, succeeded: 6, failed: 7, held: 8 };
export const ST_NAMES = Object.keys(ST);

// stateArray expands a JobSummary's unitStates into one code per index.
export function stateArray(unitStates, hold) {
  let max = -1;
  for (const k of Object.keys(unitStates || {})) max = Math.max(max, maxIndex(unitStates[k]));
  const a = new Uint8Array(max + 1);
  for (const [name, str] of Object.entries(unitStates || {})) {
    let code = ST[name] || 0;
    if (name === 'queued' && hold) code = ST.held;
    for (const [x, y] of ranges(str)) a.fill(code, x, y + 1);
  }
  return a;
}
