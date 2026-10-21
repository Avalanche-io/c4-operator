// The glossary: every term the intro, the tutorials and the tours name, in
// the order a newcomer meets them. The number (T01...) is the term's place
// in this list, so callouts and the intro's key agree. Definitions are
// plain, short and use the plain names (docs/intro/jargon.md) and the one
// vocabulary (design/README.md). Add new terms at the end; never reorder, or
// every number moves.

import { markSVG } from './marks.js';

export const TERMS = {
  job: { term: 'Job', def: 'One piece of work you sent to the farm: render a shot, run a change’s tests, train a model.' },
  done: { term: 'Done', def: 'Green. It fills from the bottom as work finishes.', swatch: 'done' },
  running: { term: 'Rendering', def: 'A unit at work: bright blue, rising as it renders. Green once it is done. An agent, test or training run renders too.', swatch: 'run' },
  ready: { term: 'Ready by', def: 'The farm’s estimate of when the job finishes. It moves as work lands.' },
  due: { term: 'Due', def: 'When you need it. The farm orders work by it; nobody sets priorities.' },
  mode: { term: 'Mode', def: 'Which question the HUD answers: Queue, Frames, Time, Shots, Farm or Full. Each mode adds one idea to the last. Switch any time.' },
  frame: { term: 'Frame', def: 'A job is made of units. For a render, a unit is a frame: one row, filling from the bottom.' },
  binary: { term: 'Binary order', def: 'First and last frame, then the middle, then the quarters. The whole shot plays early, and costly frames show up early.' },
  retry: { term: 'Orange', def: 'Trouble the farm is handling: retrying, backoff, rework. Orange notches count earlier failed attempts.', swatch: 'temp' },
  failed: { term: 'Red · Needs you', def: 'A person must act: it failed after its retries, or a check escalated it, or a decision holds up work. Open it, read why, requeue or accept it.', swatch: 'fail' },
  slack: { term: 'Slack', def: 'Time to spare between when it will finish and when it is due. Below zero means late.' },
  pencil: { term: 'Measured \u00b7 Estimated', def: 'Measured numbers are solid and never move. Estimates are lighter, underlined with dashes, and redrawn every second.', swatch: 'est' },
  risk: { term: 'At risk', def: 'Work projected to finish after the deadline: orchid, dashed. Not late yet, still fixable.', swatch: 'risk' },
  nudge: { term: 'Hold · Boost', def: 'No priority numbers. Hold what can wait, boost what can’t (up to ×2), or move the deadline.' },
  shot: { term: 'Shot · Batch · Stage', def: 'A shot is every job for one shot. A batch is 24 frames, one second. Stages sit left to right, in the order work flows.' },
  adjacency: { term: 'Adjacency', def: 'Things sit next to what holds them. A batch spans its frames’ rows. No boxes, no lines.' },
  collapse: { term: 'Collapse', def: 'Any group folds into one block that sums everything inside it: green done, blue rendering, ticks for trouble.' },
  slot: { term: 'Slot · Pool', def: 'A slot is one unit of capacity, here one GPU. A pool is a set of identical slots.' },
  burst: { term: 'Burst', def: 'Rented capacity for a deadline. The farm prices it and asks before it spends.' },
  c4id: { term: 'C4 ID', swatch: 'c4id', def: 'Every file and every output is named by its content. Same content, same ID, anywhere. C4 IDs are standardized as SMPTE ST 2114, and in production use since before the standard.' },
  book: { term: 'The book', def: 'Every action is an entry signed by whoever did it: a node, the queue, or you through the HUD.' },
  // the phases added with the approved state colors (docs/intro/palette)
  moving: { term: 'Moving', def: 'Done, but the output is still being stored and named: pale green, solid green rising as it commits.', swatch: 'moving' },
  preflight: { term: 'Preparing', def: 'Placed on a machine, getting ready to render: inputs, cache, image, license. Pale blue with a blue outline.', swatch: 'preflight' },
  backoff: { term: 'Backoff', def: 'An attempt failed and a retry is scheduled: pale orange, dashed.', swatch: 'backoff' },
  taken: { term: 'Acknowledged', def: 'Someone has it: they acknowledged the failure or were assigned to it, so it no longer needs you. Pale red with their seal.', swatch: 'taken' },
  willmiss: { term: 'Will miss', def: 'Late even with every slot it can get: orchid, hatched.', swatch: 'willmiss' },
  missed: { term: 'Missed', def: 'The deadline has passed and the work is unfinished: solid orchid.', swatch: 'missed' },
  delivered: { term: 'Delivered', def: 'The share of the work committed and named. It never goes down.' },
  runorder: { term: 'Run order', def: 'The order the farm will run work: from deadlines and the work left, never from priority numbers.' },
  deadlineline: { term: 'Deadline line', def: 'Every job\u2019s slack drawn against one line, the deadline. Above it is time to spare; below it is late.' },
  output: { term: 'Output view', def: 'A job\u2019s output so far, as one named, versioned tree that a later job can read.' },
  guaranteed: { term: 'Guaranteed · Borrowed · Lent', def: 'A project’s slots in a pool: guaranteed are its own; borrowed are taken from another project while they sit idle; lent are its own, given to another and taken back when needed.' },
  accepted: { term: 'Accepted', def: 'A failure resolved by accepting the bad frames, so the job is not blocked and nobody has to act: pale red with a check.', swatch: 'accepted' },
  pale: { term: 'Pale', def: 'A pale tint means the state is true, with context that softens it: done but still moving, working but still preparing, failed but accepted. A new case takes the pale of its color.' },
  derivation: { term: 'Derivation class', def: 'How a unit\u2019s output can be checked: exact (the same inputs always give the same bytes), validated (checked against a tolerance) or volatile (not repeatable). It is verification, not quality: no class is better than another.' },
  // agent bundles
  bundle: { term: 'Agent bundle', def: 'A team of AI agents the farm runs as one job: each agent is a member, and the farm decides how many run at once and when.' },
  checkpoint: { term: 'Checkpoint mark', def: 'A small square on a member\u2019s strip: the end of its last turn, saved, where it resumes from. Nothing after it is lost.' },
  bundletime: { term: 'Bundle time', def: 'The bundle\u2019s own clock, a virtual time. It runs while the bundle runs and stops while the bundle waits for capacity or is held, so a pause never looks like lateness to the agents. Deadlines stay on the real clock.' },
  gap: { term: 'Stopped interval', def: 'A stretch of real time when the bundle waited for capacity (the queue resumes it itself) or was held (a person resumes it). Its bundle time does not move.' },
  capped: { term: 'Capped node', def: 'A node offering fewer slots than its pool gives it, because a limit takes the rest away, such as a laptop\u2019s power. The lost slots are grey hatched, and the reason is written beside them. They come back when the limit rises.', swatch: 'capped' },
  // the one vocabulary (design/README.md; c4 INTEGRATE.md 4.2)
  queued: { term: 'Queued', def: 'Ready to run; it waits for a slot. Flat grey.', swatch: 'queued' },
  blocked: { term: 'Blocked on', def: 'It waits for other work or an input to exist: grey, hatched, with what it is blocked on as text.', swatch: 'blocked' },
  waitsfor: { term: 'Waits for', def: 'A decision or a check outside the job. For a thing (capacity, a license, a party, a time): grey, hatched, with a clock, never red. For a person: pale red, and red when work is blocked on it.', swatch: 'waits' },
  held: { term: 'Held', def: 'A person paused it: two bars centred on grey (yielding wears the same mark small, in the corner of a blue cell). It runs again when they release it.', swatch: 'held' },
  locked: { term: 'Locked by', def: 'Another unit holds something this one needs to write, such as one merge at a time: grey, hatched, with a lock.', swatch: 'locked' },
  together: { term: 'Together', def: 'Units that start and stop as one, such as the ranks of a training run: one outline, one state.', swatch: 'together' },
  reserved: { term: 'Reserved', def: 'Slots set aside for work that will start soon: a dotted outline in the pools view.', swatch: 'reserved' },
  expected: { term: 'Expected', def: 'Work or capacity that will exist but does not yet: stippled, with a dashed edge. Work a step will plan, or nodes on their way.', swatch: 'expected' },
  yielding: { term: 'Yielding', def: 'Asked to stop at its next safe point, so other work can run: still blue, with the pause mark. It resumes where it left off.', swatch: 'yielding' },
  interrupted: { term: 'Interrupted', def: 'Stopped by the farm, not by its own fault: a spot node reclaimed, a node drained. Back to grey with a grey notch; it runs again and spends no retry. Never orange.', swatch: 'interrupted' },
  rework: { term: 'Rework', def: 'A check found a problem, and it runs again with a change, such as more memory or another node. Orange, with the rework mark and the reason.', swatch: 'rework' },
  checked: { term: 'Checked', def: 'A check passed: a small tick. The frame keeps the color of its state; a check never takes a color.', swatch: 'checked' },
  reused: { term: 'Reused', def: 'The same inputs ran before, so the output was reused instead of rendered again: done, with the reuse mark.', swatch: 'reused' },
  mayuse: { term: 'May use', def: 'The pools a job may run in, in the order it falls back: gpu, then gpu-spot.' },
  gate: { term: 'Gate', def: 'A wait or a check that sits between stages: a narrow column whose block spans the rows it holds. A check that sends a frame back draws an orange arrow to it.' },
  level: { term: 'Level', def: 'How much the HUD shows and offers you: 1 for day one, 2 for a working day, 3 for everything. Choose any time; anything deeper is one click away, and nothing is locked.' },
  reasoning: { term: 'Reasons with', def: 'What a step runs on: a model (which one, at what effort, with which tools) or a procedure, a command with no model. A small mark in ink; it is not a state and takes no color.', swatch: 'reasoning' },
  grant: { term: 'Grant', def: 'Your permission for a tracker, such as GitHub or Flow, to submit work for you: the jobs it makes from your issues or tasks are yours. It ends on the date you choose, at most 90 days out, and you can revoke it any time.' },
};
export const TERM_ORDER = Object.keys(TERMS);
export const numberOf = (k) => (TERM_ORDER.indexOf(k) + 1) || null;

// the ink mark a swatch carries, as on a cell (marks.js)
const SWATCH_MARK = { waits: ['clock', 'var(--g17)'], held: ['pause', 'var(--g16)'], locked: ['lock', 'var(--g17)'], yielding: ['pause', 'var(--paper)'],
  rework: ['rework', 'var(--temp)'], checked: ['tick', 'var(--field)'], reasoning: ['model', 'var(--g17)'], reused: ['reuse', 'var(--field)'], accepted: ['check', 'var(--paper)'], taken: ['seal', 'var(--paper)'] };
// swatchHTML: a term's swatch, with its mark
export function swatchHTML(swatch) {
  if (!swatch) return '';
  const m = SWATCH_MARK[swatch];
  return `<i class="sw sw-${swatch}">${m ? markSVG(m[0], { size: 8, color: m[1] }) : ''}</i>`;
}
