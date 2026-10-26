// Needs you (#/needs): only what needs this person, at any level. The
// first user's view (PLAN 3a, 1a.6): a developer whose agents run as farm
// jobs on their own laptop opens it and sees the questions their agents
// stopped for and the decisions that wait on them, each with its controls,
// and nothing else. Everything else is one line saying it is on its way.
//
//   questions   needsinformation items that need the viewer (incidents.js),
//               with Supply and Withdraw (day1.js rootCard)
//   decisions   open waits for the viewer, or escalated to them, by job
//               (controls.js decisions)
//   settled     questions lately answered or withdrawn, as resolved, so a
//               question that left is seen to have left
//   elsewhere   anything else that needs a person, as one line to the farm
//
// The list comes from the operator (GET /api/v1/needs-me, the same list
// MCP's c4_needs_me gives), so each button shown is one the viewer may
// press. An operator without it answers 404, and the list is read from the
// snapshot instead: the viewer's needsinformation items and the waits that
// name them.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get, objectURL } from '../api.js';
import { incidents } from '../../design/components/incidents.js';
import { ended } from '../../design/components/words.js';
import { rootCard } from './day1.js';
import { decisions } from './controls.js';
import { observed, stateOf } from './sessions.js';

const keyOf = (j) => `${j.namespace}/${j.name}`;

// the operator's list, fetched once per snapshot; null until it answers,
// false when this operator has none
const list = { at: null, items: null };
function needsMe(ctx) {
  if (list.at !== ctx.snap.now && list.items !== false) {
    list.at = ctx.snap.now;
    get('/needs-me').then((d) => { list.items = d.items || []; ctx.refresh(); }, () => { list.items = false; ctx.refresh(); });
  }
  return list.items;
}

// card: an operator item as a root item (day1.js rootCard), its steps the
// item's actions
function card(it) {
  const unit = it.unit === undefined ? '' : String(it.unit);
  return {
    id: 'needsinformation:' + it.job, sev: 'needs', severity: it.severity, state: 'open', title: it.title, count: 1, unit: 'question', jobs: [it.job],
    detail: `${it.ask}${it.why ? ` (${it.why})` : ''}${it.pressingWhy ? `; pressing: ${it.pressingWhy}` : ''}`,
    steps: [{ label: 'See what it was doing', act: 'Open', href: it.href, done: false },
      ...it.actions.filter((a) => a.action === 'supply' || a.action === 'withdraw-needs').map((a) => ({
        label: a.action === 'supply' ? 'Supply the information; it resumes where it stopped' : 'Or, if the question no longer applies, withdraw it; it runs on without an answer',
        act: a.act, action: a.action, job: it.job, units: unit, done: false }))],
  };
}

const state = { observed: undefined };
fetch(objectURL('/api/v1/observed'), { method: 'HEAD' }).then((r) => { state.observed = r.ok; }).catch(() => { state.observed = false; });

export default {
  render(ctx) {
    const s = ctx.snap, v = s.viewer || {};
    const items = incidents(s);
    const served = needsMe(ctx);
    const mineWait = (w) => !w.do && (w.waitsFor === `person:${v.user}` || (w.escalated && w.needsYou));
    const questions = served
      ? served.filter((x) => x.kind === 'question').map(card)
      : items.filter((x) => x.sev === 'needs' && x.id.startsWith('needsinformation:') && x.state !== 'acknowledged');
    const waiting = served
      ? [...new Set(served.filter((x) => x.kind === 'decision').map((x) => x.job))].map((k) => ctx.job(k)).filter(Boolean)
      : s.jobs.filter((j) => (j.waits || []).some(mineWait));
    const settled = items.filter((x) => x.state === 'resolved');
    const elsewhere = items.filter((x) => x.sev === 'needs' && !x.id.startsWith('needsinformation:') && !x.id.startsWith('decision:') && !x.id.startsWith('review:'));
    const open = s.jobs.filter((j) => !ended(j));
    const working = open.filter((j) => j.phase === 'Running').length;
    // sessions running outside the farm that asked their person something
    // (views/sessions.js), when something observes them
    const obs = state.observed !== false ? observed(ctx) : null;
    // a worker that waits on its lead is the lead's to answer, not the person's
    const asking = obs ? (obs.sessions || []).filter((x) => stateOf(x) === 'waiting' && x.waitsOn !== 'lead') : [];
    const n = questions.length + waiting.length + asking.length;
    return h('section', { class: 'd1 needsme', 'data-key': 'needs' },
      h('h1', null, 'Needs you'),
      h('p', { class: 'd1-sub' }, n
        ? `${n} ${n === 1 ? 'thing waits' : 'things wait'} for you · ${f.num(working)} of ${f.num(open.length)} jobs working meanwhile`
        : `${f.num(working)} of ${f.num(open.length)} jobs working · the farm runs them by their deadlines`),
      n ? null : h('div', { class: 'd1-quiet' }, 'Nothing needs you. Your agents and jobs are on their way.'),
      questions.length ? [h('h2', null, questions.length === 1 ? 'A question only you can answer' : `${questions.length} questions only you can answer`),
        questions.map((x) => rootCard(ctx, x))] : null,
      waiting.length ? [h('h2', null, waiting.length === 1 ? 'A decision' : `Decisions · ${waiting.length}`),
        waiting.map((j) => h('div', { class: 'needs-job', 'data-key': 'nj-' + keyOf(j) },
          h('a', { class: 'nm', href: ctx.href(`job/${keyOf(j)}`) }, j.name),
          decisions(ctx, j, keyOf(j))))] : null,
      asking.length ? [h('h2', null, asking.length === 1 ? 'A session waits for you' : `Sessions waiting for you · ${asking.length}`),
        asking.map((x) => h('div', { class: 'd1-root d1-needs d1-warning', 'data-key': 'ses-' + x.name },
          h('i', { class: 'bar', 'aria-hidden': 'true' }),
          h('div', { class: 'body' },
            h('div', { class: 't' }, h('i', { class: 'flag', 'aria-hidden': 'true' }), `${x.name} asks you`),
            x.needsYou ? h('div', { class: 'd' }, x.needsYou) : null,
            h('div', { class: 'd' }, h('span', null, 'Answer it in Claude Code, where it runs; this machine’s sessions are read here, not moved. '), h('a', { href: ctx.href('sessions') }, 'Your sessions'))),
          h('div', { class: 'n' }, '1', h('small', null, 'session'))))] : null,
      settled.length ? [h('h2', null, 'Lately resolved'), settled.map((x) => rootCard(ctx, x))] : null,
      h('div', { class: 'd1-foot' },
        elsewhere.length ? h('span', null, `Also on the farm: ${elsewhere.map((x) => x.title).join('; ')}. `) : null,
        h('a', { href: ctx.href('queue') }, 'Open the farm'), ' for everything on its way.'));
  },
};
