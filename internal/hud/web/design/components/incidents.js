// INCIDENTS: alerting by root cause (c4 proposals/hud-proficiency 6, after
// FAA AC 25.1322-1 ¶8d and ¶10c). One item per cause, with everything it
// caused folded in, and an ordered checklist that leads to the action.
//
//   needs     a person must act: failures after their retries, by cause; a
//             decision or an escalated check that holds up work
//   handled   the farm is handling it: machines that stopped reporting, spot
//             machines taken back; the work they held moved, nothing to do
//
// The operator's snapshot carries incidents[] when it groups them itself
// (the shape agreed with c4-sched, .handoff/c4-hud-intro-to-c4-sched.md);
// until then this module groups on the client, from the jobs' needsPerson
// and waits and the book's not-ready and interrupted entries.
//
//   incidents(snap, book)   the items, needs first, most work first
//   incidentOf(items, key)  the item a job is folded into, or null

import * as f from './fmt.js';
import { who } from './words.js';

const rackOf = (node) => String(node || '').replace(/-\d+$/, '');
const keyOf = (j) => `${j.namespace}/${j.name}`;

// fromOperator maps the operator's incidents[] to items
function fromOperator(list) {
  return list.map((x) => ({
    id: x.id, sev: x.needsYou ? 'needs' : 'handled', title: x.title, detail: x.detail || '', count: x.count || 0, unit: x.countOf || '',
    jobs: x.jobs || [], since: x.since, state: x.state || 'open',
    steps: (x.checklist || []).map((s) => ({ label: s.label, act: s.act || 'Open', href: s.href, action: s.action, job: s.job, units: s.units, done: !!s.done })),
  }));
}

export function incidents(snap, book) {
  if (snap && Array.isArray(snap.incidents)) return fromOperator(snap.incidents);
  const items = [];
  const me = snap.viewer && snap.viewer.user;
  // failures and decisions needing a person, grouped by cause
  const byCause = new Map();
  for (const j of snap.jobs || []) {
    const np = j.needsPerson;
    if (!np) continue;
    // the cause, without the message's own advice (the checklist gives it)
    const cause = (np.reason === 'failed' ? (np.message.split(':').slice(1).join(':').trim() || 'failed after its retries') : np.message)
      .replace(/\s*Open them, requeue, assign or accept\.?$/, '');
    const k = np.reason + '|' + (np.reason === 'failed' ? cause.replace(/\d+/g, '#').slice(0, 80) : keyOf(j));
    if (!byCause.has(k)) byCause.set(k, { reason: np.reason, cause, jobs: [] });
    byCause.get(k).jobs.push(j);
  }
  for (const g of byCause.values()) {
    const first = g.jobs[0], key = keyOf(first);
    if (g.reason === 'failed') {
      const n = g.jobs.reduce((a, j) => a + (j.units.failed || 0), 0);
      // someone has it: every job's failures acknowledged (the pale rule)
      const taken = g.jobs.every((j) => (j.acks || []).some((a) => a.kind !== 'accept'));
      items.push({
        id: 'failed:' + key, sev: 'needs', count: n, unit: n === 1 ? 'frame' : 'frames', jobs: g.jobs.map(keyOf), state: taken ? 'acknowledged' : 'open',
        title: `${f.num(n)} frame${n === 1 ? '' : 's'} on ${g.jobs.length === 1 ? first.name : `${g.jobs.length} jobs`} failed after ${n === 1 ? 'its' : 'their'} retries`,
        detail: g.cause,
        // the controls act on one job, so a cause across jobs leads to each
        steps: g.jobs.length === 1 ? [
          { label: 'Read why in one failed frame’s log', act: 'Open', href: `job/${key}` },
          { label: 'If a machine is at fault, avoid it for this job, then requeue', act: 'Requeue failed', action: 'requeue', job: key, units: (first.unitStates || {}).failed || '' },
          { label: 'Or, if the frames are acceptable as they are', act: 'Accept them', action: 'accept', job: key, units: (first.unitStates || {}).failed || '' },
        ] : [
          { label: 'Read why in one failed frame’s log', act: 'Open', href: `job/${key}` },
          { label: `The same cause on ${g.jobs.length} jobs: requeue or accept each from its job`, act: 'Open the next', href: `job/${keyOf(g.jobs[1])}` },
        ],
      });
    } else if (g.reason === 'decision' || g.reason === 'review') {
      const w = (first.waits || []).find((x) => x.needsYou && !x.do);
      const id = w ? w.waitsFor.replace(/^person:/, '') : '';
      const whom = !id ? '' : id === me ? 'you' : who(w.waitsFor);
      items.push({
        id: g.reason + ':' + key, sev: 'needs', count: 1, unit: 'decision', jobs: [key],
        title: g.reason === 'review' ? `A check sent ${first.name} to ${whom || 'a person'}` : `${first.name} waits for ${whom === 'you' ? 'your' : whom ? whom + '’s' : 'a'} decision`,
        detail: (w && w.ask) || g.cause,
        steps: [{ label: 'See what it holds up', act: 'Open', href: `job/${key}` }, { label: 'Decide: approve, rework or accept', act: 'Decide', href: `job/${key}` }],
      });
    } else {
      items.push({ id: g.reason + ':' + key, sev: 'needs', count: 1, unit: '', jobs: [key], title: `${first.name} needs you`, detail: g.cause, steps: [{ label: 'Open the job', act: 'Open', href: `job/${key}` }] });
    }
  }
  // the farm's own trouble in the last hour, by rack: machines that stopped
  // reporting and the frames they held are one item, not dozens
  const t0 = Date.parse(snap.now) - 3600e3;
  const racks = new Map();
  for (const e of book || []) {
    if (Date.parse(e.at) < t0) continue;
    let node = null;
    if (e.kind === 'not-ready' && e.object && e.object.node) node = e.object.node;
    else if (e.kind === 'interrupted') { const m = /(?:on|node lost:) ([\w.-]+?)(?:\)| stopped)/.exec(e.message || ''); node = m ? m[1] : null; }
    if (!node) continue;
    const r = rackOf(node);
    if (!racks.has(r)) racks.set(r, { nodes: new Set(), units: 0, jobs: new Set() });
    const g = racks.get(r);
    if (e.kind === 'not-ready') g.nodes.add(node);
    else { g.units++; if (e.object && e.object.job) g.jobs.add(e.object.job); }
  }
  for (const [r, g] of racks) {
    if (g.nodes.size < 2 && g.units < 5) continue;
    items.push({
      id: 'rack:' + r, sev: 'handled', count: g.units, unit: 'frames moved', jobs: [...g.jobs],
      title: g.nodes.size ? `${g.nodes.size} machines in ${r} stopped reporting` : `The cloud took back spot machines in ${r}`,
      detail: `${f.num(g.units)} frame${g.units === 1 ? '' : 's'} on ${g.jobs.size} job${g.jobs.size === 1 ? '' : 's'} were interrupted and queued again on other machines, without spending a retry. Nothing to do now.`,
      steps: g.nodes.size ? [{ label: 'If they are not back within the hour, tell facilities', act: 'Open Pools', href: 'pools' }] : [],
    });
  }
  return items.sort((a, b) => (a.sev === b.sev ? b.count - a.count : a.sev === 'needs' ? -1 : 1));
}

// needsCount: open items that need a person (the peripheral cue's input)
export const needsCount = (items) => items.filter((x) => x.sev === 'needs' && x.state !== 'acknowledged').length;

// incidentOf: the item a job is folded into (the first, needs before handled)
export function incidentOf(items, key) {
  return items.find((x) => x.jobs.includes(key)) || null;
}
