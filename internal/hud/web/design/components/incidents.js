// INCIDENTS: alerting by root cause (docs/intro/levels.md, after
// FAA AC 25.1322-1 ¶8d and ¶10c). One item per cause, with everything it
// caused folded in, and an ordered checklist that leads to the action.
//
//   needs     a person must act: failures after their retries, by cause; a
//             decision or an escalated check that holds up work
//   handled   the farm is handling it: machines that stopped reporting, spot
//             machines taken back; the work they held moved, nothing to do;
//             a pool capped below what it has (a laptop's power); questions
//             a job's units answered for one another; a question that was
//             supplied or withdrawn (state resolved), for a few minutes
//
// A question a unit stopped for (needsinformation) is graded: severity
// warning while raised (drawn in the hue of a deadline at risk), urgent
// once pressing (orange, with why).
//
// The operator groups them (snapshot incidents[], always present); the HUD
// only reads them.
//
//   incidents(snap, jobs?)  the items, needs first, most work first; with
//                           jobs, only the items about one of them
//   incidentOf(items, key)  the item a job is folded into, or null

const keyOf = (j) => `${j.namespace}/${j.name}`;

export function incidents(snap, jobs = null) {
  const list = ((snap && snap.incidents) || []).map((x) => ({
    id: x.id, sev: x.needsYou ? 'needs' : 'handled', title: x.title, detail: x.detail || '', count: x.count || 0, unit: x.countOf || '',
    jobs: x.jobs || [], since: x.since, state: x.state || 'open', severity: x.severity || '',
    steps: (x.checklist || []).map((s) => ({ label: s.label, act: s.act || 'Open', href: s.href, action: s.action, job: s.job, units: s.units, done: !!s.done })),
  }));
  if (!jobs) return list;
  const keys = new Set(jobs.map(keyOf));
  return list.filter((x) => x.jobs.some((k) => keys.has(k)));
}

// needsCount: open items that need a person (the peripheral cue's input)
export const needsCount = (items) => items.filter((x) => x.sev === 'needs' && x.state !== 'acknowledged').length;

// needsTier: the peripheral cue's hue: 1 (red) for anything a person must
// act on, else urgent or warning for questions alone, else 0
export function needsTier(items) {
  const open = items.filter((x) => x.sev === 'needs' && x.state !== 'acknowledged');
  if (!open.length) return '0';
  if (open.some((x) => !x.severity)) return '1';
  return open.some((x) => x.severity === 'urgent') ? 'urgent' : 'warning';
}

// incidentOf: the item a job is folded into (the first, needs before handled)
export function incidentOf(items, key) {
  return items.find((x) => x.jobs.includes(key)) || null;
}
