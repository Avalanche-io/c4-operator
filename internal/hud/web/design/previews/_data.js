// Data for the component previews: the captured fixtures (make fixtures writes
// them to build/fixtures/design; hudmock serves them at design/fixtures,
// frozen from hack/hudmock) when present, else the live HUD API of whatever
// serves this page (the mock, or a real manager in development mode).
import * as f from '../components/fmt.js';

async function json(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(`${r.status} ${url}`);
  return r.json();
}

export async function snapshot() {
  let s;
  try { s = await json('../fixtures/snapshot.json'); } catch { s = await json('/api/v1/snapshot?as=wrangler'); }
  f.setClock(s.now);
  return s;
}

export async function job(key) {
  try { return await json(`../fixtures/jobs/${key.replace('/', '__')}.json`); } catch { return json(`/api/v1/jobs/${key}?as=wrangler`); }
}

export async function graph() {
  try { return await json('../fixtures/graph.json'); } catch { return json('/api/v1/graph?as=wrangler'); }
}

export async function book(k) {
  try { return await json('../fixtures/book.json'); } catch { return json(`/api/v1/book?limit=40${k ? '&job=' + encodeURIComponent(k) : ''}&as=wrangler`).catch(() => ({ entries: [] })); }
}

// shotDetails: the shot's jobs and the deliveries they feed, in flow order
export async function shot(snap, g, key) {
  const me = snap.jobs.find((j) => `${j.namespace}/${j.name}` === key);
  const set = new Set();
  if (me && me.shot) { for (const j of snap.jobs) if (j.shot === me.shot) set.add(`${j.namespace}/${j.name}`); }
  else set.add(key);
  for (const e of g.edges) {
    if (me && me.shot && set.has(e.from) && !(snap.jobs.find((j) => `${j.namespace}/${j.name}` === e.to) || {}).shot) set.add(e.to);
    if (!(me && me.shot) && e.to === key) set.add(e.from);
    if (!(me && me.shot) && e.from === key) set.add(e.to);
  }
  const keys = [...set];
  const order = [];
  const indeg = new Map(keys.map((k) => [k, 0]));
  for (const e of g.edges) if (set.has(e.from) && set.has(e.to)) indeg.set(e.to, indeg.get(e.to) + 1);
  const q = keys.filter((k) => !indeg.get(k));
  while (q.length) { const k = q.shift(); order.push(k); for (const e of g.edges) if (e.from === k && set.has(e.to)) { indeg.set(e.to, indeg.get(e.to) - 1); if (!indeg.get(e.to)) q.push(e.to); } }
  for (const k of keys) if (!order.includes(k)) order.push(k);
  const details = await Promise.all(order.map((k) => job(k)));
  return { keys: order, details: details.map((d) => ({ ...d, ...(snap.jobs.find((j) => j.name === d.name) || {}), unitStates: d.unitStates })) };
}
