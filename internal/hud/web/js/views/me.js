// Your page (#/me, from your name in the top bar): what you have let
// trackers do for you. A tracker (GitHub, Flow, …) turns issues and tasks
// into jobs; letting it submit work for you makes the jobs it makes from
// yours your own, run with your access. Every grant ends (30 days unless
// you say, at most 90), and you can take it back any time; both are signed
// entries in the book.
//
// Your tracker logins: a tracker offers its logins as people ("Flow says
// @noor.k is you"), and a mapping counts once you confirm it here, as
// yourself; never an agent. Confirmed, a decision that login makes in the
// tracker is relayed as yours. Not me only puts the offer aside here: the
// project fixes the tracker.
//
// By level (level.js):
//   1 each tracker in your projects, what it turns into jobs, and one
//     button: let it submit work for me (30 days); your grants, with Revoke;
//     your tracker logins to confirm, and those confirmed, with Revoke
//   2 limit a grant to pools and kinds of work, and choose when it ends;
//     who else has let the tracker submit for them
//     who has confirmed their login on each tracker
//   3 each grant's and confirmation's signed record (its C4 ID)

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get } from '../api.js';
import { c4idEl } from '../../design/components/c4id.js';
import { heldSection } from './held.js';

const st = { trackers: null, grants: null, people: null, who: new Map(), logins: new Map(), err: null, terr: null, busy: false, at: null, notMe: new Set() };
document.addEventListener('hud:changed', () => { st.at = null; st.who.clear(); st.logins.clear(); });
// Not me: the offer is put aside on this page; the project fixes the tracker
document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-not-me]');
  if (!b) return;
  st.notMe.add(b.dataset.notMe);
  document.dispatchEvent(new CustomEvent('hud:refresh'));
});

function load(ctx) {
  if (st.busy || st.at === ctx.snap.now) return;
  st.busy = true;
  st.at = ctx.snap.now;
  // each on its own, so one that fails leaves the other readable
  const grants = get('/grants').then((g) => { st.grants = g.grants || []; st.err = null; }, (e) => { st.err = e.message; });
  const trackers = get('/trackers').then((t) => { st.trackers = t.trackers || []; st.terr = null; }, (e) => { st.terr = e.message; });
  // an operator without people answers 404: the section stays away
  const people = get('/people?mine=1').then((p) => { st.people = p.entries || []; }, () => { st.people = false; });
  Promise.all([grants, trackers, people]).finally(() => { st.busy = false; ctx.refresh(); });
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

// logins: who has confirmed their login on a tracker, and who has not (level 2)
function logins(ctx, t) {
  const k = `${t.namespace}/${t.name}`;
  if (!st.logins.has(k)) {
    st.logins.set(k, null);
    get(`/people?tracker=${encodeURIComponent(k)}`).then((p) => { st.logins.set(k, p.entries || []); ctx.refresh(); }, () => { st.logins.set(k, false); });
  }
  return st.logins.get(k);
}

const personName = (p) => String(p).replace(/^party:/, 'the party ');

// loginRow: a login offered as you, to confirm; or confirmed, to revoke
function loginRow(ctx, e) {
  const k = `${e.namespace}/${e.tracker}/${e.login}`;
  const site = KIND_OF(e.tracker);
  if (e.state === 'confirmed') {
    return h('div', { class: 'grow', 'data-key': 'p-' + k },
      h('div', { class: 'gmain' },
        h('b', null, e.login), h('span', { class: 'dim' }, ` in ${e.tracker} · ${e.project}`),
        h('div', { class: 'gline' }, `Confirmed as ${e.person === ctx.snap.viewer.user ? 'you' : personName(e.person)}${e.confirmed ? ` on ${day(e.confirmed.at)}` : ''}: ${e.lets}.`),
        ctx.level >= 3 && e.confirmed && e.confirmed.record ? h('div', { class: 'gline dim' }, 'signed record ', c4idEl(e.confirmed.record, { tail: 4, label: 'confirmation record' })) : null),
      h('button', { class: 'btn small', 'data-action': 'people-revoke', 'data-name': `${e.namespace}/${e.name}`, title: 'Take it back now. Decisions this login makes are no longer relayed as yours.' }, 'Revoke'));
  }
  return h('div', { class: 'grow', 'data-key': 'p-' + k },
    h('div', { class: 'gmain' },
      h('b', null, `${site} says ${e.login} is ${e.person === ctx.snap.viewer.user ? 'you' : personName(e.person) + ', and you are in it'}`), h('span', { class: 'dim' }, ` · ${e.project}`),
      h('div', { class: 'gline dim', style: { fontSize: '12px' } }, `Confirming it means ${e.lets}.`)),
    h('span', { class: 'gbtns' },
      h('button', { class: 'btn small primary', 'data-action': 'people-confirm', 'data-ns': e.namespace, 'data-tracker': e.tracker, 'data-login': e.login }, 'That’s me'),
      h('button', { class: 'btn small', 'data-not-me': k, title: 'Put it aside. The project fixes the tracker.' }, 'Not me')));
}

const day = (iso) => f.when(iso);
const KIND = { github: 'GitHub', flow: 'Flow Production Tracking', jira: 'Jira', linear: 'Linear' };
// KIND_OF: a tracker's site in words, from its name when the list is not read yet
function KIND_OF(name) {
  const t = (st.trackers || []).find((x) => x.name === name);
  return t ? (KIND[t.kind] || t.kind) : name;
}

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

// loginLine: whose logins on a tracker count, and whose wait to be confirmed (level 2)
function loginLine(ctx, t) {
  const es = ctx.level >= 2 ? logins(ctx, t) : null;
  if (!es || !es.length) return null;
  const yes = es.filter((e) => e.state === 'confirmed'), no = es.filter((e) => e.state === 'offered');
  return h('p', { class: 'dim' },
    yes.length ? `Logins confirmed: ${yes.map((e) => `${e.login} (${personName(e.person)})`).join(', ')}. ` : '',
    no.length ? `Not confirmed yet, so their decisions are relayed as unattributed: ${no.map((e) => `${e.login} (${personName(e.person)})`).join(', ')}.` : '');
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
      ctx.level >= 2 ? heldSection(ctx, `tracker=${encodeURIComponent(`${t.namespace}/${t.name}`)}`, { title: null, only: (w) => !w.yours }) : null,
      people ? h('p', { class: 'dim' }, people.length ? `People it submits for: ${people.map((g) => g.person).join(', ')}.` : 'Nobody has let it submit work for them yet.') : null,
      loginLine(ctx, t),
      giveForm(ctx, t, !!mine)));
}

// peopleSection: your logins in trackers, to confirm and confirmed
function peopleSection(ctx) {
  if (!st.people) return null;
  const offered = st.people.filter((e) => e.state === 'offered' && !st.notMe.has(`${e.namespace}/${e.tracker}/${e.login}`));
  const confirmed = st.people.filter((e) => e.state === 'confirmed');
  if (!offered.length && !confirmed.length) return null;
  return h('div', { class: 'vsec', 'data-key': 'people' },
    h('header', null, h('h2', null, offered.length ? `Tracker logins to confirm · ${offered.length}` : 'Your tracker logins')),
    h('p', { class: 'gintro' }, 'A tracker says which of its logins is you. It counts once you confirm it here: then a decision you make in the tracker is relayed as yours. Only you can confirm it, never an agent.'),
    offered.map((e) => loginRow(ctx, e)), confirmed.map((e) => loginRow(ctx, e)));
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
      heldSection(ctx, 'mine=1', { title: 'Writes for you to check', intro: 'A write to a tracker was interrupted, and C4 can’t tell whether it reached the tracker. It holds it, neither sending it again nor dropping it, until you look and say.' }),
      peopleSection(ctx),
      h('div', { class: 'vsec', 'data-key': 'grants' },
        h('header', null, h('h2', null, 'Trackers that submit work for you')),
        h('p', { class: 'gintro' }, 'A tracker turns issues and tasks into jobs. Letting it submit work for you makes the jobs it makes from yours your own, run with your access. It always ends, and you can take it back any time.'),
        st.grants == null ? h('p', { class: 'dim' }, 'Reading…')
          : mine.length ? mine.map((g) => grantRow(ctx, g)) : h('p', { class: 'dim' }, 'None. No tracker submits work for you.')),
      h('div', { class: 'vsec', 'data-key': 'trackers' },
        h('header', null, h('h2', null, 'Trackers in your projects')),
        st.terr && st.trackers == null ? h('p', { class: 'warn red' }, st.terr)
          : st.trackers == null ? h('p', { class: 'dim' }, 'Reading…')
          : st.trackers.length ? h('div', { class: 'ggrid' }, st.trackers.map((t) => trackerCard(ctx, t))) : h('p', { class: 'dim' }, 'Your projects have no trackers connected.')));
  },
};
