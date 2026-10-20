// WORDS: the HUD's one vocabulary in code (design/README.md, "One
// vocabulary"). A unit's state, its wait, its review and its interruptions
// read the same in the tooltip, the unit card, the legend and the book.
// Native names (the operator's enum values, Kubernetes reasons) appear only
// in how(), shown in Full mode.
//
//   unitWords(u)        { word, cls, why }: the state in the vocabulary, its
//                       text class, and the reason or detail as text
//   who(waitsFor)       a wait's subject in words: "Noor", "license-nuke"
//   interruptedBy(r)    a native interruption reason in words
//   how(u)              the Full-mode line: what the words stand for natively

import * as f from './fmt.js';

// who: person:noor → Noor; party:license-nuke → license-nuke; clock:… → the time
export function who(waitsFor = '') {
  const m = /^(person|party|clock):(.*)$/.exec(waitsFor);
  if (!m) return waitsFor;
  if (m[1] === 'clock') return f.clock(m[2]);
  if (m[1] === 'person') return m[2].replace(/(^|[-_.\s])(\w)/g, (_, a, b) => (a ? ' ' : '') + b.toUpperCase());
  return m[2];
}
export const isPerson = (w) => !!(w && /^person:/.test(w.waitsFor || ''));

const INTERRUPTED = {
  PreemptionByScheduler: 'made room for other work',
  EvictionByEvictionAPI: 'evicted: the node is being drained',
  DeletionByTaintManager: 'the node went bad',
  TerminationByKubelet: 'the node shut down',
  DeletionByPodGC: 'the node was lost',
  DisruptionTarget: 'the node is leaving',
  Deleted: 'stopped by the farm',
};
export const interruptedBy = (r) => INTERRUPTED[r] || 'stopped by the farm';

// unitWords: the state as a wrangler reads it.
export function unitWords(u) {
  const st = u.state === 'unevaluated' ? 'planned' : u.state;
  const w = u.wait, r = u.review;
  const rework = r && r.do === 'rework' && !['succeeded', 'egress', 'accepted'].includes(st);
  const intr = u.interruptions && u.interruptions.length ? u.interruptions[u.interruptions.length - 1] : null;
  const reworkWhy = rework ? `rework${r.passes > 1 ? `, pass ${r.passes}` : ''}${r.why ? `: ${r.why}` : ''}` : '';
  switch (st) {
    case 'planned': return { word: 'expected', cls: 'dim', why: u.exists ? `${Math.round(u.exists * 100)}% likely to run` : 'work that will exist once what plans it runs' };
    case 'queued':
      if (rework) return { word: 'rework', cls: 't-temp', why: reworkWhy };
      if (intr) return { word: 'queued', cls: '', why: `interrupted ${f.clock(intr.at, false)}: ${interruptedBy(intr.reason)}${intr.node ? ` (${intr.node})` : ''}; it runs again, no retry spent` };
      return { word: 'queued', cls: '', why: 'ready; waits for a slot' };
    case 'blocked': return { word: 'blocked on', cls: '', why: u.blockedOn ? u.blockedOn : 'work before it' };
    case 'held': return { word: 'held', cls: '', why: 'a person paused it' };
    case 'locked': return { word: 'locked by', cls: '', why: u.lockedBy || 'another unit' };
    case 'waits': {
      if (!w) return { word: 'waits for', cls: '', why: 'a decision' };
      const p = isPerson(w);
      const time = w.expectSeconds ? `, ~${f.dur(w.expectSeconds)}` : '';
      if (p && (w.needsYou || w.blocking)) return { word: 'needs you', cls: 't-fail', why: `${who(w.waitsFor)} to decide${w.ask ? `: ${w.ask}` : ''}` };
      return { word: 'waits for', cls: p ? 't-fail' : '', why: `${who(w.waitsFor)}${w.ask ? `: ${w.ask}` : ''}${time}` };
    }
    case 'preflight': return { word: 'preparing', cls: 't-run', why: 'placed; getting its inputs, cache, image or license' };
    case 'running':
      if (rework) return { word: 'rework', cls: 't-temp', why: reworkWhy };
      if (u.stopping) return { word: 'yielding', cls: 't-run', why: `stops at its next safe point${u.stopping.reason ? `: ${u.stopping.reason}` : ''}` };
      return { word: 'rendering', cls: 't-run', why: '' };
    case 'retrying': return { word: rework ? 'rework' : 'retrying', cls: 't-temp', why: rework ? reworkWhy : 'it failed and runs again' };
    case 'backoff': return { word: rework ? 'rework' : 'backoff', cls: 't-temp', why: rework ? reworkWhy : 'it failed; a retry is scheduled' };
    case 'egress': return { word: 'moving', cls: 't-done', why: 'rendered; its output is still moving' };
    case 'succeeded': return { word: 'done', cls: 't-done', why: r && r.do === 'pass' ? 'checked' : u.reused ? 'reused' : '' };
    case 'accepted': return { word: 'accepted', cls: 't-fail', why: (r && r.note) || (u.accepted && u.accepted.note) || 'the bad frames were accepted' };
    case 'flagged': return { word: 'needs you', cls: 't-fail', why: `a check escalated it${r && r.to ? ` to ${who('person:' + r.to)}` : ''}${r && r.why ? `: ${r.why}` : ''}` };
    case 'failed': return u.ack ? { word: 'acknowledged', cls: 't-fail', why: u.ack.actor ? `${u.ack.actor} has it` : 'someone has it' } : { word: 'needs you', cls: 't-fail', why: 'failed after its retries' };
    default: return { word: st, cls: '', why: '' };
  }
}

// how: the native names behind the words, for Full mode.
export function how(u) {
  const out = [`state ${u.state}`];
  if (u.wait) out.push(`wait ${u.wait.waitsFor}${u.wait.escalated ? ' (escalated)' : ''}`);
  if (u.review) out.push(`review ${u.review.do} by unit ${u.review.by}${u.review.passes ? `, ${u.review.passes} passes` : ''}`);
  if (u.stopping) out.push(`yield annotation ${u.stopping.reason || ''}`.trim());
  if (u.safepoint) out.push(`safe point ${u.safepoint.seq}, next ${u.safepoint.next}`);
  for (const x of u.interruptions || []) out.push(`${x.reason}${x.node ? ` on ${x.node}` : ''}`);
  return out.join(' · ');
}

// jobWords: a job's state in one phrase, the Queue-mode word (INTEGRATE 4.4):
// "blocked on comp", "waits for license-nuke", "held", "needs you" (the
// caller's personMark says why), else its phase in the vocabulary.
const PHASE = { Queued: 'queued', Running: 'rendering', Blocked: 'blocked on', Held: 'held', Yielded: 'queued', Succeeded: 'done', Failed: 'needs you' };
export function jobWords(j) {
  const short = (k) => String(k).split('/').pop();
  if (j.phase === 'Blocked') return { word: 'blocked on', cls: '', why: (j.dependsOn || []).map(short).join(', ') || 'earlier work' };
  if (j.phase === 'Held') return { word: 'held', cls: 't-magenta', why: '' };
  const open = (j.waits || []).filter((w) => !w.do);
  const run = j.units ? (j.units.running || 0) + (j.units.retrying || 0) + (j.units.preflight || 0) : 0;
  if (open.length && !run && j.phase !== 'Succeeded') {
    const w = open[0];
    return { word: 'waits for', cls: isPerson(w) ? 't-red' : '', why: who(w.waitsFor) + (open.length > 1 ? ` and ${open.length - 1} more` : '') };
  }
  if (j.phase === 'Yielded') return { word: 'queued', cls: 'dim', why: 'yielded; resumes where it left off' };
  return { word: PHASE[j.phase] || String(j.phase || '').toLowerCase(), cls: j.phase === 'Failed' ? 't-red' : j.phase === 'Succeeded' ? 'settled' : '', why: '' };
}

// tempWords: the orange family in words: "2 retrying · 1 backoff"
export function tempWords(u) {
  const parts = [];
  if (u.retrying) parts.push(`${u.retrying} retrying`);
  if (u.backoff) parts.push(`${u.backoff} backoff`);
  return parts.join(' · ');
}

// reasoningWords: what a unit reasons with, in words, by level (level.js):
// at 2 a model (and which) or a procedure; at 3 also its effort and tools.
// What it declares comes first; what its latest attempt reported
// (observed) fills a gap, and at level 3 says where the run differed.
// Metadata, not state. Null when the unit does not say.
export function reasoningWords(r, level = 3) {
  if (!r || !r.does) return null;
  if (r.does === 'procedure') return 'a procedure: a command, no model';
  const o = r.observed || {};
  const ran = o.model || (o.models || []).join(', ');
  const ranAt = o.effort || (o.efforts || []).join(', ');
  const parts = [r.model ? `a model, ${r.model}` : ran ? `a model, ${ran} (as its run reported)` : 'a model (which one is not reported)'];
  if (level >= 3 && r.model && ran && ran !== r.model) parts.push(`ran on ${ran}`);
  if (level >= 3 && (r.effort || ranAt)) parts.push(`effort ${r.effort || ranAt}${r.effort && ranAt && ranAt !== r.effort ? ` (ran at ${ranAt})` : ''}`);
  if (level >= 3 && r.tools) parts.push(`tools ${r.tools}`);
  return parts.join(' · ');
}
