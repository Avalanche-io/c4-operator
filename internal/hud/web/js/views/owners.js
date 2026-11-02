// Owners (#/owners, level 3; accountability S2b, DESIGN 3.3): who answers
// for each part of the farm. What a part of the farm signs accrues to its
// owner: the farm binding's, unless a binding for that one deployment says
// otherwise (a node another party owns, a person's own Mac as a host
// runner, a tracker's sync, a project's importer, one of the manager's
// components). Each binding is a signed act booked in the farm book, in
// force from its position there.
//
//   the farm        its owner, whom it is run for, since when and by whom,
//                   the one-person line, standing or its end
//   bound apart     each deployment bound to its own owner now: what it
//                   signs, since when, standing or until when, whom it
//                   follows; a farm admin rotates it to a new owner or ends
//                   it, its owner ends their own
//   bind one        a farm admin binds a deployment: its kind and name, its
//                   owner, and an end, or none (standing)
//   ended           bindings no longer in force: when, by whom, and the
//                   binding that followed
//
// bind (made on the farm, or on the binding it follows) is on the touch
// list; end-binding is confirmed when someone other than its owner ends it
// and only signed when its owner does (GET /acts/needs says which; api.js
// asks for the touch when the farm refuses for want of one). A farm with
// no bindings route (404) keeps the tab away.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get, post } from '../api.js';
import { c4idEl } from '../../design/components/c4id.js';
import { deploymentWord } from '../../design/components/words.js';

const st = { list: null, farm: '', mayBind: false, none: false, err: null, at: null, busy: false, msg: null, rotating: null };
document.addEventListener('hud:changed', () => { st.at = null; });

function load(ctx) {
  if (st.none || st.busy || st.at === ctx.snap.now) return;
  st.busy = true;
  st.at = ctx.snap.now;
  get('/bindings').then((d) => { st.list = d.bindings || []; st.farm = d.farm || ''; st.mayBind = !!d.mayBind; st.err = null; },
    (e) => { if (e.status === 404 || e.status === 501) st.none = true; else st.err = e.message; })
    .finally(() => { st.busy = false; ctx.refresh(); });
}

// ------------------------------------------------------------ controls

const form = (id) => document.getElementById(id);
const value = (id, name) => { const el = form(id) && form(id).querySelector(`[name="${name}"]`); return el ? el.value.trim() : ''; };
const endOf = (id) => f.fromInput(value(id, 'end')) || undefined;

// KINDS: the deployments a binding names, after the farm (API.md
// "Deployment bindings")
export const KINDS = [['node', 'a node'], ['runner', 'a host runner'], ['sync', 'a tracker’s sync'], ['import', 'a project’s importer'], ['component', 'a component']];

// toOf: what a bind form's deployment names: <farm>/<kind>/<name>
export const toOf = (farm, kind, name) => `${farm}/${kind}/${name}`;

const ACTS = {
  bind: (d) => ['/bindings', { owner: value(d.form, 'owner'), to: toOf(st.farm, value(d.form, 'kind'), value(d.form, value(d.form, 'kind') === 'component' ? 'component' : 'name')), end: endOf(d.form) }],
  rotate: (d) => [`/bindings/${d.binding}/rotate`, { owner: value(d.form, 'owner'), end: endOf(d.form) }],
  end: (d) => [`/bindings/${d.binding}/end`, { owner: d.owner }],
};

document.addEventListener('click', async (e) => {
  const open = e.target.closest('[data-binding-rotate]');
  if (open) {
    e.preventDefault();
    st.rotating = st.rotating === open.dataset.bindingRotate ? null : open.dataset.bindingRotate;
    document.dispatchEvent(new CustomEvent('hud:refresh'));
    return;
  }
  const b = e.target.closest('[data-binding-act]');
  if (!b) return;
  e.preventDefault();
  b.disabled = true;
  try {
    const [path, body] = ACTS[b.dataset.bindingAct](b.dataset);
    const r = await post(path, body);
    st.msg = r.message || 'Done.';
    st.rotating = null;
    document.dispatchEvent(new CustomEvent('hud:changed'));
  } catch (err) {
    st.msg = err.message;
  } finally {
    b.disabled = false;
    document.dispatchEvent(new CustomEvent('hud:refresh'));
  }
});

// ------------------------------------------------------------ words

const day = (iso) => f.when(iso);
const isFarm = (b) => !b.deployment || b.deployment === '*';
// lasts: until when a binding holds: its end, or standing
const lasts = (b) => (b.standing ? h('span', { class: 'standing', title: 'No end: it holds until the farm has an annual review' }, 'standing') : b.end ? `until ${day(b.end)}` : null);

function farmBlock(ctx, b) {
  return h('div', { class: 'vsec', 'data-key': 'farm' },
    h('header', null, h('h2', null, 'The farm')),
    h('p', null, 'The farm is ', h('b', null, `${b.owner}’s`), b.runFor ? `, run for ${b.runFor}` : '', '. Everything on it answers to them, unless it is bound apart below.'),
    h('div', { class: 'gline dim' }, [`Bound ${day(b.start)} by ${b.by}`, lasts(b) ? ' · ' : null, lasts(b), b.record ? [' · ', c4idEl(b.record, { tail: 4, label: 'farm binding record' })] : null]),
    b.onePersonNote ? h('p', { class: 'dim' }, b.onePersonNote) : null);
}

function boundRow(ctx, b, v, admin, follows) {
  const own = v.party === b.owner || v.user === b.owner;
  const fid = `rot-${b.name}`;
  return h('div', { class: 'grow', 'data-key': 'b-' + b.name },
    h('div', { class: 'gmain' },
      h('b', null, deploymentWord(b.deployment)), h('span', null, ` · ${b.owner}’s`),
      h('div', { class: 'gline dim' }, [`Since ${day(b.start)}, bound by ${b.by}`, lasts(b) ? ' · ' : null, lasts(b), follows ? ` · follows ${follows.owner}’s` : null]),
      (b.for || []).length ? h('div', { class: 'gline dim' }, `It signs ${b.for.join(', ')} for them.`) : null,
      b.record ? h('div', { class: 'gline dim' }, 'record ', c4idEl(b.record, { tail: 4, label: 'binding record' })) : null,
      admin && st.rotating === b.name ? h('form', { class: 'gform', id: fid },
        h('div', { class: 'gfield' }, h('span', { class: 'lab' }, 'New owner'), h('input', { name: 'owner', placeholder: 'their name', 'aria-label': 'Its new owner' })),
        h('div', { class: 'gfield' }, h('span', { class: 'lab' }, 'Until'), h('input', { name: 'end', type: 'datetime-local', 'aria-label': 'When it ends; empty for standing' })),
        h('p', { class: 'dim' }, `The new binding follows this one: ${b.owner}’s ends at the same moment.`),
        h('button', { type: 'button', class: 'btn small primary', 'data-binding-act': 'rotate', 'data-binding': b.name, 'data-form': fid }, 'Rotate')) : null),
    h('span', { class: 'gbtns' },
      admin ? h('button', { class: 'btn small', 'data-binding-rotate': b.name, title: 'A new owner for it' }, st.rotating === b.name ? 'Cancel' : 'New owner') : null,
      admin || own ? h('button', { class: 'btn small', 'data-binding-act': 'end', 'data-binding': b.name, 'data-owner': b.owner,
        title: own ? 'Give up answering for it. It answers to the farm’s owner again.' : `End it now, with a touch. It answers to the farm’s owner again; certificates already issued run out their hour or day.` }, 'End') : null));
}

function bindForm() {
  const id = 'bind-one';
  return h('div', { class: 'vsec', 'data-key': 'bind' },
    h('header', null, h('h2', null, 'Bind a part of the farm')),
    h('form', { class: 'gform', id },
      h('div', { class: 'gfield' }, h('span', { class: 'lab' }, 'Part'),
        h('select', { name: 'kind', 'aria-label': 'What kind of part' }, KINDS.map(([k, w]) => h('option', { value: k }, w))),
        h('input', { name: 'name', placeholder: 'its name: laptop-1, flow, harbor', 'aria-label': 'Its name' }),
        h('select', { name: 'component', 'aria-label': 'Which component, for a component' }, ['hud', 'queue', 'controller'].map((c) => h('option', { value: c }, deploymentWord('component/' + c))))),
      h('div', { class: 'gfield' }, h('span', { class: 'lab' }, 'Owner'), h('input', { name: 'owner', placeholder: 'noor', 'aria-label': 'Who answers for it' })),
      h('div', { class: 'gfield' }, h('span', { class: 'lab' }, 'Until'), h('input', { name: 'end', type: 'datetime-local', 'aria-label': 'When it ends; empty for standing' })),
      h('p', { class: 'dim' }, 'What it signs then accrues to its owner instead of the farm’s. Left without an end it is standing. One binding per part: a bound one gets a new owner instead.'),
      h('button', { type: 'button', class: 'btn small primary', 'data-binding-act': 'bind', 'data-form': id }, 'Bind it')));
}

function endedRow(b, list) {
  const next = b.ended && b.ended.followedBy ? list.find((x) => x.record === b.ended.followedBy) : null;
  return h('div', { class: 'grow', 'data-key': 'x-' + b.name },
    h('div', { class: 'gmain dim' },
      `${deploymentWord(b.deployment)}, ${b.owner}’s, from ${day(b.start)}`,
      b.ended ? ` to ${day(b.ended.at)}, ended by ${b.ended.by}${next ? `; followed by ${next.owner}’s` : ''}` : b.end ? ` to ${day(b.end)}` : ''));
}

export default {
  render(ctx) {
    load(ctx);
    const v = ctx.snap.viewer || {};
    // a farm admin binds, gives a binding a new owner and ends anyone's:
    // the farm says who may (mayBind, by the rule its routes take)
    const admin = st.mayBind;
    const list = st.list || [];
    const farm = list.find(isFarm);
    const bound = list.filter((b) => !isFarm(b) && b.inForce);
    const ended = list.filter((b) => !isFarm(b) && !b.inForce);
    return h('section', { class: 'sheet owners' },
      h('div', { class: 'sheethead' }, h('div', null,
        h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ' / owners'),
        h('h1', null, 'Owners'),
        h('div', { class: 'meta vmeta' }, 'Who answers for each part of the farm: what a part signs accrues to its owner'))),
      st.err ? h('p', { class: 'warn red' }, st.err) : null,
      st.msg ? h('p', { class: 'dim' }, st.msg) : null,
      st.list == null ? h('p', { class: 'dim' }, st.none ? 'This farm keeps no bindings.' : 'Reading…') : [
        farm ? farmBlock(ctx, farm) : h('p', { class: 'warn' }, 'No one owns this farm yet: its install binds it to its owner.'),
        h('div', { class: 'vsec', 'data-key': 'bound' },
          h('header', null, h('h2', null, 'Bound apart from the farm')),
          bound.length ? bound.map((b) => boundRow(ctx, b, v, admin, b.follows ? list.find((x) => x.record === b.follows) : null))
            : h('p', { class: 'dim' }, farm ? `Nothing: every part of the farm answers to ${farm.owner}.` : 'Nothing.')),
        admin && farm ? bindForm() : null,
        ended.length ? h('div', { class: 'vsec', 'data-key': 'ended' },
          h('header', null, h('h2', null, 'Ended')),
          ended.map((b) => endedRow(b, list))) : null,
      ]);
  },
};
