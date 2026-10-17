// LEDGER marks. Nothing on the farm is overwritten: every action is an
// entry, signed by whoever acted. Measured values are ink and never move;
// estimates are pencil, redrawn every second. People and agents carry the
// same seal, named.
//
//   seal(signer, {open})   a signer's mark: node diamond, queue square,
//                          HUD circle (a person or agent), controller,
//                          importer and ingest triangle; dashed until signed
//   ink(text) / pencil(text)
//   bookList(entries)      the book: sequence, time, seal, entry, actor

import * as f from './fmt.js';

function el(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) if (v !== null && v !== undefined && v !== false) e.setAttribute(k, v === true ? '' : String(v));
  for (const k of kids.flat()) if (k !== null && k !== undefined && k !== false) e.append(k instanceof Node ? k : document.createTextNode(String(k)));
  return e;
}

export const SEALS = { node: 'node', queue: 'queue', hud: 'hud', controller: 'controller', importer: 'importer', ingest: 'ingest' };

export function seal(signer, { open = false, title } = {}) {
  return el('i', { class: `seal ${SEALS[signer] || 'hud'}${open ? ' open' : ''}`, title: title || `signed by the ${signer}`, 'aria-hidden': 'true' });
}

export const ink = (text) => el('span', { class: 'ink' }, text);
export const pencil = (text, title) => el('span', { class: 'pencil', title: title || 'an estimate, redrawn as the farm changes' }, text);

// kinds whose subject is temporary trouble or failure, for the entry's tone
const TONE = { failed: 't-fail', 'retry': 't-temp', retrying: 't-temp', backoff: 't-temp', suspect: 't-temp', yield: '', late: 't-fail' };

export function bookList(entries) {
  return el('div', { class: 'book', role: 'log', 'aria-label': 'The book: entries, newest last' },
    (entries || []).map((e) => el('div', { class: `e${e.superseded ? ' struck' : ''}${e.open ? ' open' : ''}`, 'data-key': 'b' + e.seq },
      el('span', { class: 'seq' }, String(e.seq).padStart(4, '0')),
      el('span', { class: 'at' + (e.approx ? ' pencil' : '') }, e.approx ? '<' + f.clock(e.at, false) : f.clockSec(e.at)),
      seal(e.signer, { open: !!e.open }),
      el('span', { class: 'msg ' + (TONE[e.kind] || '') }, e.message),
      el('span', { class: 'who' }, `${e.actor || ''}${e.signer ? ' · ' + e.signer : ''}${e.record ? ' · ' + f.gridRef(e.record) : ''}`))));
}
