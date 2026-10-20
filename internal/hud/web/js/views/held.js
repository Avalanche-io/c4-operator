// Held tracker writes: a write to a tracker (a status, a comment, a check)
// that was interrupted where nothing the tracker offers can tell whether it
// happened. C4 neither sends it again nor drops it until a person looks in
// the tracker and says which. Shown only when something is held: on the job
// page and each tracker's card from level 2, and on your page, for the
// writes that are yours to check, at every level.
//
//   heldSection(ctx, query, opts)   the cards for GET /held?<query>, or null

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get } from '../api.js';

const cache = new Map(); // query -> { at, list }
document.addEventListener('hud:changed', () => cache.clear());

function load(ctx, q) {
  const c = cache.get(q);
  if (c && (c.at === ctx.snap.now || c.busy)) return c.list;
  const next = { at: ctx.snap.now, list: c ? c.list : null, busy: true };
  cache.set(q, next);
  get(`/held?${q}`).then((d) => { next.list = d.held || []; }, () => { next.list = next.list || []; })
    .finally(() => { next.busy = false; ctx.refresh(); });
  return next.list;
}

const KIND = { github: 'GitHub', flow: 'Flow', jira: 'Jira', linear: 'Linear' };

export function heldCard(ctx, w, { job = true } = {}) {
  const site = KIND[w.kind] || w.tracker;
  const where = `${w.entity.type} ${w.entity.id}`;
  const held = Math.max(0, (f.now() - Date.parse(w.since)) / 1000);
  const base = { 'data-action': 'held', 'data-ns': w.namespace, 'data-link': w.link, 'data-field': w.field };
  return h('div', { class: 'heldw', 'data-key': `held-${w.namespace}/${w.link}/${w.field}` },
    h('div', { class: 'hbody' },
      h('div', { class: 'ht' }, `C4 couldn’t tell whether this reached ${site}: ${w.says}.`),
      h('div', { class: 'hd' }, 'Look at ', w.entity.url ? h('a', { href: w.entity.url, target: '_blank', rel: 'noopener' }, where) : where,
        ` in ${site}, then say. Held for ${f.dur(held)}`, job && w.job ? [', for ', h('a', { href: ctx.href(`job/${w.job}`) }, f.jobName(w.job))] : null, '.'),
      ctx.level >= 3 ? h('div', { class: 'hd mono' }, `${w.tracker} · link ${w.link} · ${w.field}`) : null),
    h('div', { class: 'hact' },
      h('button', { type: 'button', class: 'btn small primary', ...base, 'data-happened': 'true', title: 'C4 takes it as written and carries on' }, 'It’s there'),
      h('button', { type: 'button', class: 'btn small', ...base, 'data-happened': 'false', title: `C4 writes it again on its next pass. If it is in ${site} after all, it will be there twice.` }, 'It’s not there, send it again')));
}

// heldSection: the held writes for a query (only: a filter), under a
// heading, or null when none is held (the usual case)
export function heldSection(ctx, q, { title = 'Writes to check', job = true, intro = null, only = null } = {}) {
  const list = (load(ctx, q) || []).filter(only || (() => true));
  if (!list || !list.length) return null;
  return h('div', { class: 'heldsec', 'data-key': 'held-' + q },
    title ? h('h2', null, title) : null,
    intro ? h('p', { class: 'dim' }, intro) : null,
    list.map((w) => heldCard(ctx, w, { job })));
}
