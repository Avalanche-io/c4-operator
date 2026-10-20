// The controls a person reaches for beyond hold, boost and requeue
// (c4-operator internal/hud/API.md, the job routes):
//
//   decisions(ctx, j, k)        what waits for a person: approve, a choice or
//                               fail; a check that escalated: accept, rework
//                               or fail; with a note
//   unitActions(ctx, j, k, u)   on a unit's card: take a failed unit or give
//                               it to someone; a message to a running unit,
//                               and the ones sent before
//   deadlineOwner(ctx, j, k)    who owns the deadline, and the farm's
//                               suggestion and next session, one click each
//
// Each is a button with data-action; app.js posts it and shows the farm's
// own sentence.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { who } from '../../design/components/words.js';

const words = (s) => String(s).replace(/[-_]/g, ' ');
const id = (...p) => p.join('-').replace(/[^\w-]/g, '_');

// decisions: the job's open waits for a person and the checks escalated to
// one, each with its ask and its choices
export function decisions(ctx, j, k) {
  const v = ctx.snap.viewer || {};
  const open = (j.waits || []).filter((w) => !w.do && (w.escalated || /^person:/.test(w.waitsFor || '')));
  if (!open.length) return null;
  return h('div', { class: 'panel decide', 'data-key': 'decide' },
    h('header', null, h('h2', null, open.length === 1 ? 'Waits for a decision' : `Waits for ${open.length} decisions`)),
    h('div', { class: 'body' }, open.map((w) => {
      const mine = w.waitsFor === `person:${v.user}` || w.needsYou;
      const note = id('note', k, w.unit);
      const reply = id('reply', k, w.unit);
      const dos = w.escalated ? ['accept', 'rework', 'fail'] : [...(w.choices && w.choices.length ? w.choices : ['approve']), 'fail'];
      const base = { 'data-action': 'decide', 'data-job': k, 'data-unit': String(w.unit), 'data-note': note, 'data-reply': w.reply ? reply : '' };
      return h('div', { class: 'dcard' + (mine ? ' mine' : ''), 'data-key': 'w' + w.unit },
        h('div', { class: 'dask' }, w.ask || (w.escalated ? 'A check could not settle this unit.' : 'A decision holds up work.')),
        h('div', { class: 'dmeta' }, w.escalated ? `${w.name || 'a unit'}: a check escalated it` : `waits for ${who(w.waitsFor)}`,
          w.since ? ` · since ${f.clock(w.since)}` : '', w.blocking ? ' · it holds up work' : ''),
        mine || v.allProjects ? [
          w.reply ? h('textarea', { id: reply, rows: 3, class: 'dreply', placeholder: 'Your answer: the units it holds up read it', 'aria-label': 'Your answer' }) : null,
          h('input', { id: note, type: 'text', class: 'dnote', placeholder: 'A note for the book (optional)', 'aria-label': 'Note' }),
          h('div', { class: 'dbtns' }, dos.map((d, i) => h('button', {
            type: 'button', class: 'btn small' + (i === 0 ? ' primary' : ''), ...base, 'data-do': d,
            title: d === 'fail' ? 'It fails, and what it holds up does not run' : d === 'rework' ? 'The unit runs again with your note' : '',
          }, d === 'fail' ? 'Fail it' : words(d).replace(/^./, (c) => c.toUpperCase()))))]
          : h('div', { class: 'dim', style: { fontSize: '12px' } }, `${who(w.waitsFor)} decides.`));
    })));
}

// unitActions: take a failed unit (you have it) or give it to someone, and
// send a running unit a message it reads at its next safe point
export function unitActions(ctx, j, k, u) {
  const out = [];
  if (u.state === 'failed' && ctx.level >= 2) {
    const to = id('assign', k, u.i);
    out.push(h('div', { class: 'uact', 'data-key': 'take' },
      h('button', { type: 'button', class: 'btn small', 'data-action': 'ack', 'data-job': k, 'data-units': String(u.i), 'data-kind': 'acknowledge', title: 'You have it: it stops counting against everyone else' }, 'I’ll take it'),
      h('input', { id: to, type: 'text', class: 'dnote', placeholder: 'a person', 'aria-label': 'Give it to' }),
      h('button', { type: 'button', class: 'btn small', 'data-action': 'ack', 'data-job': k, 'data-units': String(u.i), 'data-kind': 'assign', 'data-assignee': to }, 'Give it to them')));
  }
  const steers = (j.steers || []).filter((s) => s.unit === u.i);
  const lens = (ctx.cls(j.class) || {}).lens;
  const steerable = ['running', 'retrying', 'preflight', 'queued', 'blocked', 'waits'].includes(u.state) && (ctx.level >= 3 || (ctx.level >= 2 && (lens === 'agent' || lens === 'plan')));
  if (steerable || steers.length) {
    const t = id('steer', k, u.i);
    out.push(h('div', { class: 'uact col', 'data-key': 'steer' },
      steers.length ? h('div', { class: 'book' }, steers.map((s) => h('div', { class: 'e', 'data-key': 's' + s.seq },
        h('span', { class: 'seq' }, `#${s.seq}`), h('span', { class: 'at' }, f.clock(s.at)),
        h('span', { class: 'msg' }, s.text), h('span', { class: 'who' }, `${s.actor} · ${s.seen ? 'read' : s.delivered ? 'delivered' : 'waiting for it to start'}`)))) : null,
      steerable ? [
        h('textarea', { id: t, rows: 2, class: 'dreply', placeholder: 'A message to it while it runs: it reads it at its next safe point', 'aria-label': 'Message to the unit' }),
        h('button', { type: 'button', class: 'btn small', 'data-action': 'steer', 'data-job': k, 'data-unit': String(u.i), 'data-text': t }, 'Send it')] : null));
  }
  return out;
}

// deadlineOwner: who owns the deadline, and what the farm offers
export function deadlineOwner(ctx, j, k) {
  const o = j.objective || {};
  if (!o.deadline || o.from || o.type === 'latency') return null;
  const sg = o.suggestion;
  const owner = o.mode === 'Tracking' ? 'Production tracking sets this deadline.'
    : o.mode === 'Suggested' ? 'This deadline is the farm’s suggestion.'
      : 'This deadline is pinned: only a person moves it.';
  const btn = (mode, label, title) => h('button', { type: 'button', class: 'btn small', 'data-action': 'deadlinemode', 'data-job': k, 'data-mode': mode, title }, label);
  return h('div', { class: 'downer', 'data-key': 'downer' },
    h('span', { class: 'dim' }, owner),
    o.mode !== 'Pinned' ? btn('Pinned', 'Pin it here', 'Keep this deadline; nothing moves it but a person') : null,
    sg && o.mode !== 'Suggested' ? btn('Suggested', `Use the farm’s suggestion, ${f.when(sg.deadline)}`, `A time it is ${Math.round((sg.quantile || 0.95) * 100)}% sure of, without making other work late`) : null,
    sg && sg.nextSession ? h('button', { type: 'button', class: 'btn small primary', 'data-action': 'nextsession', 'data-job': k, title: 'It will miss its own session; move it to the next and pin it' }, `Move to the next session, ${f.when(sg.nextSession)}`) : null);
}
