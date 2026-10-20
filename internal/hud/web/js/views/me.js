// Your page (#/me, from your name in the top bar): what you have let
// trackers do for you. A tracker (GitHub, Flow, …) turns issues and tasks
// into jobs; letting it submit work for you makes the jobs it makes from
// yours your own, run with your access. Every grant ends (30 days unless
// you say, at most 90), and you can take it back any time; both are signed
// entries in the book.
//
// By level (level.js):
//   1 each tracker in your projects, what it turns into jobs, and one
//     button: let it submit work for me (30 days); your grants, with Revoke
//   2 limit a grant to pools and kinds of work, and choose when it ends;
//     who else has let the tracker submit for them
//   3 each grant's signed record (its C4 ID)

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get } from '../api.js';
import { c4idEl } from '../../design/components/c4id.js';

const st = { trackers: null, grants: null, who: new Map(), err: null, terr: null, busy: false, at: null };
document.addEventListener('hud:changed', () => { st.at = null; st.who.clear(); });

function load(ctx) {
  if (st.busy || st.at === ctx.snap.now) return;
  st.busy = true;
  st.at = ctx.snap.now;
  // each on its own: a farm that does not list its trackers still shows
  // the grants you gave
  const grants = get('/grants').then((g) => { st.grants = g.grants || []; st.err = null; }, (e) => { st.err = e.message; });
  const trackers = get('/trackers').then((t) => { st.trackers = t.trackers || []; st.terr = null; }, (e) => { st.terr = e.message; });
  Promise.all([grants, trackers]).finally(() => { st.busy = false; ctx.refresh(); });
}

// who: the people who have let a tracker submit for them (level 2)
function who(ctx, t) {
  const k = `${t.namespace}/${t.name}`;
  if (!st.who.has(k)) {
    st.who.set(k, null);
    get(`/grants?tracker=${encodeURIComponent(k)}`).then((g) => { st.who.set(k, (g.grants || []).filter((x) => x.active)); ctx.refresh(); }, () => {});
  }
  return st.who.get(k);
}

const day = (iso) => f.when(iso);
const KIND = { github: 'GitHub', flow: 'Flow Production Tracking', jira: 'Jira', linear: 'Linear' };

// scope: what a grant lets the tracker submit, in words
function scope(g) {
  const kinds = (g.classes || []).length ? `${g.classes.join(' and ')} jobs` : 'any work';
  return (g.pools || []).length ? `${kinds} on ${g.pools.join(' or ')}` : kinds;
}

function grantRow(ctx, g) {
  return h('div', { class: 'grow' + (g.active ? '' : ' off'), 'data-key': 'g-' + g.namespace + '/' + g.name },
    h('div', { class: 'gmain' },
      h('b', null, g.tracker), h('span', { class: 'dim' }, ` · ${g.project}`),
      h('div', { class: 'gline' }, g.active ? `It can submit ${scope(g)} for you until ${day(g.until)}.` : `It cannot submit work for you: ${g.why}.`),
      ctx.level >= 3 && g.record ? h('div', { class: 'gline dim' }, 'signed record ', c4idEl(g.record, { tail: 4, label: 'grant record' })) : null),
    g.active ? h('button', { class: 'btn small', 'data-action': 'revoke', 'data-grant': `${g.namespace}/${g.name}`, title: 'Take it back now. A unit running under it finishes its attempt; nothing new starts for you.' }, 'Revoke') : null);
}

function giveForm(ctx, t, renew) {
  const id = `give-${t.namespace}-${t.name}`;
  const base = { 'data-action': 'grant', 'data-ns': t.namespace, 'data-tracker': t.name, 'data-form': id };
  if (ctx.level < 2) {
    return h('button', { type: 'button', class: 'btn small primary', ...base, 'data-until': '30d' }, renew ? 'Renew for 30 days' : 'Let it submit work for me');
  }
  const pools = (ctx.snap.pools || []).map((p) => p.name);
  const classes = (ctx.snap.classes || []).map((c) => c.name);
  return h('form', { class: 'gform', id },
    h('div', { class: 'gfield' }, h('span', { class: 'lab' }, 'Limit to'),
      h('span', { class: 'gopts' }, classes.map((c) => h('label', null, h('input', { type: 'checkbox', name: 'class', value: c }), c))),
      h('span', { class: 'gopts' }, pools.map((p) => h('label', null, h('input', { type: 'checkbox', name: 'pool', value: p }), `on ${p}`))),
      h('span', { class: 'dim' }, 'none ticked: any work')),
    h('div', { class: 'gfield' }, h('span', { class: 'lab' }, 'Until'),
      h('select', { name: 'until', 'aria-label': 'When it ends' },
        [['7d', '7 days from now'], ['30d', '30 days from now'], ['90d', '90 days from now (the most)']].map(([v, w]) => h('option', { value: v, selected: v === '30d' ? true : false }, w)))),
    h('button', { type: 'button', class: 'btn small primary', ...base }, renew ? 'Renew' : 'Let it submit work for me'));
}

function trackerCard(ctx, t) {
  const mine = t.mine && t.mine.active ? t.mine : null;
  const people = ctx.level >= 2 ? who(ctx, t) : null;
  return h('div', { class: 'panel gcard', 'data-key': 't-' + t.namespace + '/' + t.name },
    h('header', null, h('h2', null, t.name), h('span', { class: 'dim' }, `${KIND[t.kind] || t.kind} · ${t.project}`)),
    h('div', { class: 'body' },
      h('p', null, t.site),
      (t.syncs || []).length ? h('p', { class: 'dim' }, `Turns into jobs: ${t.syncs.join('; ')}.`) : null,
      t.ready ? null : h('p', { class: 't-fail' }, `Not connected: ${t.why}. A grant to it does nothing until it is.`),
      mine ? h('p', null, `It can submit ${scope(mine)} for you until ${day(mine.until)}.`) : h('p', { class: 'dim' }, 'It does not submit work for you.'),
      people ? h('p', { class: 'dim' }, people.length ? `People it submits for: ${people.map((g) => g.person).join(', ')}.` : 'Nobody has let it submit work for them yet.') : null,
      giveForm(ctx, t, !!mine)));
}

export default {
  render(ctx) {
    load(ctx);
    const v = ctx.snap.viewer || {};
    const mine = (st.grants || []);
    return h('section', { class: 'sheet me' },
      h('div', { class: 'sheethead' }, h('div', null,
        h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ' / you'),
        h('h1', null, v.user || 'You'),
        h('div', { class: 'meta vmeta' }, `${v.role || ''}${v.role ? ' · ' : ''}${v.allProjects ? 'all projects' : (v.projects || []).join(', ')}`))),
      st.err ? h('p', { class: 'warn red' }, st.err) : null,
      h('div', { class: 'vsec', 'data-key': 'grants' },
        h('header', null, h('h2', null, 'Trackers that submit work for you')),
        h('p', { class: 'gintro' }, 'A tracker turns issues and tasks into jobs. Letting it submit work for you makes the jobs it makes from yours your own, run with your access. It always ends, and you can take it back any time.'),
        st.grants == null ? h('p', { class: 'dim' }, 'Reading…')
          : mine.length ? mine.map((g) => grantRow(ctx, g)) : h('p', { class: 'dim' }, 'None. No tracker submits work for you.')),
      h('div', { class: 'vsec', 'data-key': 'trackers' },
        h('header', null, h('h2', null, 'Trackers in your projects')),
        st.terr && st.trackers == null ? h('p', { class: 'dim' }, 'This farm does not list its trackers here yet. Until it does, give a grant from a terminal: c4 farm grant TRACKER.')
          : st.trackers == null ? h('p', { class: 'dim' }, 'Reading…')
          : st.trackers.length ? h('div', { class: 'ggrid' }, st.trackers.map((t) => trackerCard(ctx, t))) : h('p', { class: 'dim' }, 'Your projects have no trackers connected.')));
  },
};
