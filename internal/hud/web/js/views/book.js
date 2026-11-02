// The farm book (#/book, level 3): the farm-wide log of farm-level acts,
// each signed by whoever acted and sealed by the queue in signed segments
// that name the one before (c4-operator DESIGN 9.2, GET /api/v1/book/farm).
// Grants and their revocations, node disable and enable, people's devices,
// removals and the sign-in provider read here in one order, the order the
// farm judges them in.
//
// It shows:
//   - where the seal has reached, and how many entries wait for it
//   - each entry: its position, when its writer wrote it, what it says, who
//     acted and for whom, and its record (a C4 ID at level 3); pending ones
//     drawn open, flagged ones (a record signed outside the window its
//     position allows) in the late tone, redacted ones saying what was left
//     out
//   - entries the queue refused at the seal, and why: they never count
//   - a warning, kept until the person reads it, when the book goes back or
//     forks from what this browser saw (farmbook.js follows), or when the
//     farm's own reader found it rewritten (409)
//
// This browser keeps the last head it accepted, per farm, and reads from a
// little before it, so a book pointed back at an older segment shows here
// too: signing alone shows a rewrite, never a rollback.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get } from '../api.js';
import { bookList } from '../../design/components/ledger.js';
import { readPage, follows } from '../../design/components/farmbook.js';
import { c4idEl } from '../../design/components/c4id.js';

const LAST = 'c4-hud-farm-book-head';
// how many entries back from the last head a read starts, and how often
// the page reads again while it is open
const BACK = 200;
const EVERY = 5000;
// how many more pages a read follows toward the head
const PAGES = 50;
const st = { page: null, err: null, broke: null, none: false, busy: false, t: 0 };

function last() {
  try { return JSON.parse(localStorage.getItem(LAST) || 'null'); } catch (e) { return null; }
}

// read: the book from a little before the last head this browser
// accepted, following each page to the head however far the book has
// grown, so a fork or rollback between visits shows; the view keeps the
// last BACK entries. A first read of a long book jumps to its end.
async function read() {
  const was = last();
  const at = (since) => get(`/book/farm?since=${since}&limit=${BACK * 2}`);
  let page = await at(was ? Math.max(0, was.position - BACK) : 0);
  if (!was && page.more) page = await at(Math.max(0, page.head.position - BACK));
  const mine = was && was.farm === page.farm ? was : null;
  const why = follows(mine, page);
  for (let n = 0; page.more && n < PAGES; n++) {
    const next = await at(page.next);
    page = { ...next, segments: [...page.segments, ...next.segments].slice(-PAGES), entries: [...page.entries, ...next.entries].slice(-BACK) };
  }
  if (why || follows(mine, page)) {
    // never accept a head that went back: keep the warning, and the old head
    st.broke = why || follows(mine, page);
    return page;
  }
  localStorage.setItem(LAST, JSON.stringify({ farm: page.farm, position: page.head.position, entry: page.head.entry }));
  return page;
}

function load(ctx) {
  if (st.none || st.busy || Date.now() - st.t < EVERY) return;
  st.busy = true;
  st.t = Date.now();
  read().then((p) => { st.page = p; st.err = null; }, (e) => {
    if (e.status === 404) st.none = true;
    else if (e.status === 409) st.broke = e.message;
    else st.err = e.message;
  }).finally(() => { st.busy = false; ctx.refresh(); });
}

// waiting: the entries the seal hasn't reached, in a sentence
function waiting(pending) {
  const n = (pending || []).length;
  if (!n) return 'Nothing waits for the seal.';
  return n === 1 ? 'One entry waits for the seal: the farm already acts on it, and nothing outside the farm counts it yet.'
    : `${n} entries wait for the seal: the farm already acts on them, and nothing outside the farm counts them yet.`;
}

function refusedRow(ctx, r) {
  return h('div', { class: 'grow', 'data-key': 'r-' + r.submission },
    h('div', { class: 'gmain' },
      h('b', null, 'An entry refused at the seal'), h('span', { class: 'dim' }, ` · segment ${r.segment}, ${f.clock(r.at)}`),
      h('div', { class: 'gline' }, `Refused because ${r.why}. It never counts.`),
      ctx.level >= 3 && r.submission ? h('div', { class: 'gline dim' }, 'the entry ', c4idEl(r.submission, { tail: 4, label: 'refused entry' })) : null));
}

export default {
  render(ctx) {
    load(ctx);
    const head = h('div', { class: 'sheethead' }, h('div', null,
      h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ' / book'),
      h('h1', null, 'The farm book'),
      h('div', { class: 'meta vmeta' }, 'Each farm-level act, signed by whoever acted, sealed in order by the farm')));
    if (st.none) {
      return h('section', { class: 'sheet farmbook' }, head,
        h('p', { class: 'dim' }, 'This farm keeps no book yet: its install names no system store for it.'));
    }
    const p = st.page && readPage(st.page);
    return h('section', { class: 'sheet farmbook' }, head,
      st.broke ? h('p', { class: 'warn red' }, `Don’t trust this book until your farm admin looks: ${st.broke}.`) : null,
      st.err ? h('p', { class: 'warn red' }, st.err) : null,
      !p ? h('p', { class: 'dim' }, 'Reading…') : h('div', { class: 'vsec', 'data-key': 'book' },
        h('p', { class: 'gintro' },
          `Sealed through position ${p.head.position || 0}${p.sealed ? `, last at ${f.clock(p.sealed)}` : ''}. `,
          waiting(st.page.pending)),
        p.rows.length ? bookList(p.rows) : h('p', { class: 'dim' }, 'Nothing in the book yet.')),
      p && p.refused.length ? h('div', { class: 'vsec', 'data-key': 'refused' },
        h('header', null, h('h2', null, `Refused at the seal · ${p.refused.length}`)),
        h('p', { class: 'gintro' }, 'The farm checks each entry against the record it cites before sealing it. These said something their record didn’t, so they were never sealed.'),
        p.refused.map((r) => refusedRow(ctx, r))) : null);
  },
};
