// FARM BOOK. The farm-wide, hash-linked log of farm-level acts (c4-operator
// DESIGN 9.2, GET /api/v1/book/farm): grants and their revocations, node
// disable and enable, the people's index (devices, removals, the sign-in
// provider). The HUD and the people's issuer write each entry; the queue
// checks it against the record it cites and seals a run of them in a
// signed segment that names the one before. An entry waiting for the seal
// is pending: the farm judges with it at once, and no verifier counts it.
//
//   readPage(page)        the page as rows for bookList (ledger.js), newest
//                         last, sealed then pending, with the refusals and
//                         the segments' seals
//   entryWords(e)         an entry in the book's words
//   follows(last, page)   whether the page follows the last head this
//                         browser accepted: null, or why not (went back,
//                         forked)
//
// The browser doesn't verify the segments' signatures: the HUD's own
// reader does, and answers 409 when the book was rewritten or rolled
// back. What the browser adds is its own last head, so a book pointed back
// at an older segment shows here too.

import * as fmt from './fmt.js';

const dec = (b64) => new TextDecoder().decode(Uint8Array.from(atob(b64), (c) => c.charCodeAt(0)));

// segmentOf: a signed segment's predicate (book/v1), read, not verified
export function segmentOf(bundle) {
  try {
    const b = typeof bundle === 'string' ? JSON.parse(bundle) : bundle;
    return JSON.parse(dec(b.dsseEnvelope.payload)).predicate;
  } catch (e) {
    return null;
  }
}

const short = (id) => (id ? String(id).slice(-6) : '');

// REFUSED: why the queue refused an entry at the seal, by its reason code
export const REFUSED = {
  missing: 'its entry wasn’t in the store',
  malformed: 'its entry wasn’t a book entry',
  'not-pending': 'it wasn’t waiting for the seal',
  'other-farm': 'it named another farm',
  'unknown-kind': 'its kind isn’t one the book holds',
  unsigned: 'it wasn’t signed by its writer',
  booked: 'its record was booked already',
  'record-missing': 'the record it cites wasn’t in the store',
  unverified: 'the record it cites didn’t verify',
  'wrong-signer': 'the record it cites wasn’t signed by whoever may sign it',
  disagrees: 'it said something its record didn’t',
  'not-in-force': 'what it relies on wasn’t in force',
  'over-limit': 'too many entries were waiting for the seal',
  rejected: 'the farm’s checks refused it',
};

// flagWords: why a sealed entry is flagged, in a clause
function flagWords(f) {
  switch (f.reason) {
    case 'no-time': return 'its record says no time it was signed';
    case 'unchecked': return 'adopted from before today’s checks, which refuse it: it counts for nothing';
    default: return `its record says ${fmt.clock(f.signedAt)}, outside the ${fmt.clock(f.from)} to ${fmt.clock(f.to)} its position allows`;
  }
}

// entryWords: what an entry says, in the book's words. A control entry is
// one of the HUD's control records: its act on its subject, by its actor;
// the rest are the people's index's kinds.
export function entryWords(e) {
  const of = (p) => (p ? `${p}’s` : 'a');
  switch (e.kind) {
    case 'control': return controlWords(e);
    case 'device': return `${of(e.person)} device enrolled (…${short(e.device)})`;
    case 'removal': return e.device ? `${of(e.person)} device removed (…${short(e.device)})` : `all of ${of(e.person)} devices removed`;
    case 'setting': return 'the sign-in provider set';
    case 'provider-key': return 'the sign-in provider’s key booked';
    case 'code': return 'a one-time browser code booked';
    case 'countersign': return `${of(e.person)} browser countersigned by the farm’s machine`;
    case 'approval': return `a machine approved for ${e.person || 'its person'}`;
    case 'flag': return `${of(e.person)} device seen from two places`;
    default: return e.kind || 'an entry';
  }
}

// controlWords: a control record's act on its subject (kind:name)
function controlWords(e) {
  const subject = e.subject || '';
  const name = subject.includes(':') ? subject.slice(subject.indexOf(':') + 1) : subject;
  switch (e.act) {
    case 'grant': return `granted ${name}`;
    case 'revoke': return `revoked ${name}`;
    case 'disable': return subject.startsWith('node:') ? `took node ${name} out` : `disabled ${subject}`;
    case 'enable': return subject.startsWith('node:') ? `put node ${name} back` : `enabled ${subject}`;
    case 'bind': return `the farm bound${e.person ? ' to ' + e.person : ''}`;
    default: return `${e.act}${subject ? ' ' + subject : ''}`;
  }
}

// who: who acted, and for whom when it isn't themselves
function who(e) {
  const by = e.by || e.person || '';
  return e.person && e.by && e.person !== e.by ? `${by} for ${e.person}` : by;
}

const parse = (x) => (typeof x === 'string' ? JSON.parse(x) : x);

// readPage: { head, rows, refused, sealed } from a page of GET /book/farm.
// rows are bookList entries: seq the position, at the writer's clock, the
// message, who, the record; pending rows drawn open, flagged rows in the
// late tone with the window their position allows, redacted rows saying
// what was left out.
export function readPage(page) {
  const segs = (page.segments || []).map(segmentOf).filter(Boolean);
  const sealedAt = new Map(segs.map((s) => [s.number, s.signedAt]));
  const flags = new Map();
  const refused = [];
  for (const s of segs) {
    for (const f of s.flags || []) flags.set(f.position, f);
    for (const r of s.refused || []) refused.push({ ...r, why: REFUSED[r.reason] || r.reason, segment: s.number, at: s.signedAt });
  }
  const row = (pe, pending) => {
    const e = parse(pe.entry) || {};
    const f = flags.get(pe.position);
    const notes = [];
    if (pending) notes.push('not sealed yet');
    if (f) notes.push(flagWords(f));
    const withheld = pe.entry == null || (pe.redacted || []).includes('entry');
    if (!withheld && (pe.redacted || []).includes('private')) notes.push('its addresses left out');
    return {
      // act: how the person's act an entry books was signed, which the
      // farm reads from the act's own record (actBy, words.js)
      seq: pe.position, id: pe.id, at: e.at, kind: f ? 'late' : e.kind, open: pending, act: pe.act,
      // the HUD writes control records; the people's issuer the rest
      signer: e.kind === 'control' ? 'hud' : 'issuer', day: true, record: e.record || pe.id,
      message: withheld ? 'an entry withheld from you: only its person and the farm’s admins read it' + (notes.length ? ` (${notes.join('; ')})` : '')
        : entryWords(e) + (notes.length ? ` (${notes.join('; ')})` : ''),
      actor: who(e),
      segment: pe.segment, sealedAt: sealedAt.get(pe.segment),
    };
  };
  return {
    head: page.head || {},
    rows: [...(page.entries || []).map((pe) => row(pe, false)), ...(page.pending || []).map((pe) => row(pe, true))],
    refused,
    sealed: segs.length ? segs[segs.length - 1].signedAt : null,
  };
}

// follows: null when the page follows the last head this browser accepted
// ({ position, entry }), or why not: the head went back, or the entry at
// the last head's position isn't the one it was (a fork)
export function follows(last, page) {
  if (!last || !last.position) return null;
  const h = page.head || {};
  if ((h.position || 0) < last.position) return `the book’s head went back from position ${last.position} to ${h.position || 0}`;
  const at = (page.entries || []).find((pe) => pe.position === last.position);
  if (at && at.id !== last.entry) return `the entry at position ${last.position} is not the one this browser saw there`;
  if (h.position === last.position && h.entry && h.entry !== last.entry) return `the head at position ${last.position} is not the one this browser saw`;
  return null;
}
