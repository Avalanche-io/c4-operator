// The glossary: every term the intro, the tutorials and the tours name, in
// the order a newcomer meets them. The number (T01...) is the term's place
// in this list, so callouts and the intro's key agree. Definitions are
// plain, short and use the plain names (docs/intro/jargon.md). Add new
// terms at the end; never reorder, or every number moves.

export const TERMS = {
  job: { term: 'Job', def: 'One piece of work you sent to the farm: render a shot, run a change’s tests, train a model.' },
  done: { term: 'Done', def: 'Green. It fills from the bottom as work finishes.', swatch: 'done' },
  running: { term: 'Running', def: 'Bright blue, rising as the frame renders. Green once it is done.', swatch: 'run' },
  ready: { term: 'Ready by', def: 'The farm’s estimate of when the job finishes. It moves as work lands.' },
  due: { term: 'Due', def: 'When you need it. The farm orders work by it; nobody sets priorities.' },
  level: { term: 'Level', def: 'The HUD starts simple. Each level adds one idea. Move up when you want it.' },
  frame: { term: 'Frame', def: 'A job is made of units. For a render, a unit is a frame: one row, filling from the bottom.' },
  binary: { term: 'Binary order', def: 'First and last frame, then the middle, then the quarters. The whole shot plays early, and costly frames show up early.' },
  retry: { term: 'Orange', def: 'Trouble the farm is handling: a retry, a slow node. Notches count earlier attempts.', swatch: 'temp' },
  failed: { term: 'Red', def: 'Failed after its retries. It needs you: open it, read why, requeue it.', swatch: 'fail' },
  slack: { term: 'Slack', def: 'Time to spare between when it will finish and when it is due. Below zero means late.' },
  pencil: { term: 'Measured \u00b7 Estimated', def: 'Measured numbers are solid and never move. Estimates are lighter, underlined with dashes, and redrawn every second.', swatch: 'est' },
  risk: { term: 'At risk', def: 'Work projected to finish after the deadline: orchid, dashed. Not late yet, still fixable.', swatch: 'risk' },
  nudge: { term: 'Hold · Boost', def: 'No priority numbers. Hold what can wait, boost what can’t (up to ×2), or move the deadline.' },
  shot: { term: 'Shot · Batch · Stage', def: 'A shot is every job for one shot. A batch is 24 frames, one second. Stages sit left to right, in the order work flows.' },
  adjacency: { term: 'Adjacency', def: 'Things sit next to what holds them. A batch spans its frames’ rows. No boxes, no lines.' },
  collapse: { term: 'Collapse', def: 'Any group folds into one block that sums everything inside it: green done, blue running, ticks for trouble.' },
  slot: { term: 'Slot · Pool', def: 'A slot is one unit of capacity, here one GPU. A pool is a set of identical slots.' },
  burst: { term: 'Burst', def: 'Rented capacity for a deadline. The farm prices it and asks before it spends.' },
  c4id: { term: 'C4 ID', def: 'Every file and every output is named by its content. Same content, same ID, anywhere.' },
  book: { term: 'The book', def: 'Every action is an entry signed by whoever did it: a node, the queue, or you through the HUD.' },
  // the phases added with the approved state colors (docs/intro/palette)
  moving: { term: 'Moving', def: 'Done, but the output is still being stored and named: pale green, solid green rising as it commits.', swatch: 'moving' },
  preflight: { term: 'Preparing', def: 'Placed on a machine, getting ready to render: inputs, cache, image, license. Pale blue with a blue outline.', swatch: 'preflight' },
  backoff: { term: 'Retry scheduled', def: 'An attempt failed and the farm will try again shortly: pale orange, dashed.', swatch: 'backoff' },
  taken: { term: 'Taken', def: 'Someone has acknowledged a failure or been assigned to it, so it no longer waits on a person: pale red with their mark.', swatch: 'taken' },
  willmiss: { term: 'Will miss', def: 'Late even with every slot it can get: orchid, hatched.', swatch: 'willmiss' },
  missed: { term: 'Missed', def: 'The deadline has passed and the work is unfinished: solid orchid.', swatch: 'missed' },
  delivered: { term: 'Delivered', def: 'The share of the work committed and named. It never goes down.' },
  runorder: { term: 'Run order', def: 'The order the farm will run work: from deadlines and the work left, never from priority numbers.' },
  deadlineline: { term: 'Deadline line', def: 'Every job\u2019s slack drawn against one line, the deadline. Above it is time to spare; below it is late.' },
  output: { term: 'Output view', def: 'A job\u2019s output so far, as one named, versioned tree that a later job can read.' },
  guaranteed: { term: 'Guaranteed share', def: 'The slots a project is promised in a pool. Unused ones are lent out and taken back when needed.' },
  accepted: { term: 'Accepted', def: 'A failure resolved by accepting the bad frames, so the job is not blocked and nobody has to act: pale red with a check.', swatch: 'accepted' },
  pale: { term: 'Pale', def: 'A pale tint means the state is true, with context that softens it: done but still copying, rendering but still preparing, failed but accepted. A new case takes the pale of its color.' },
  derivation: { term: 'Derivation class', def: 'How a unit\u2019s output can be checked: exact (the same inputs always give the same bytes), validated (checked against a tolerance) or volatile (not repeatable). It is verification, not quality: no class is better than another.' },
};
export const TERM_ORDER = Object.keys(TERMS);
export const numberOf = (k) => (TERM_ORDER.indexOf(k) + 1) || null;
