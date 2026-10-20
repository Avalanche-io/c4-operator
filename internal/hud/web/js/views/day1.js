// Level 1, day one (level.js): low density, a few obvious actions, enough to
// get real work done. What needs you comes first, one item per root cause
// with its steps (design/components/incidents.js); then one row per job.
// Anything deeper is one click away: Open, or "Show the frames" (a peek at
// level 2 for that one job).
//
//   mine1   an artist's jobs (Queue mode)
//   farm1   a wrangler's farm (Queue mode)
//   job1    one job: its stages as progress
//   pools1  the farm's capacity: what is wrong, one bar per pool

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get } from '../api.js';
import { glance } from '../../design/components/core.js';
import { jobWords } from '../../design/components/words.js';
import { markEl } from '../../design/components/marks.js';
import { incidents, incidentOf } from '../../design/components/incidents.js';
import { decisions } from './controls.js';

const keyOf = (j) => `${j.namespace}/${j.name}`;

// ------------------------------------------------------------ root items

export function rootCard(ctx, it) {
  const next = it.steps.findIndex((s) => !s.done);
  // acknowledged: someone has it, so it goes pale with their seal (the pale rule)
  const ack = it.state === 'acknowledged';
  return h('div', { class: 'd1-root d1-' + it.sev + (ack ? ' d1-ack' : ''), 'data-key': 'root-' + it.id, role: it.sev === 'needs' && !ack ? 'alert' : 'status' },
    h('i', { class: 'bar', 'aria-hidden': 'true' }),
    h('div', { class: 'body' },
      h('div', { class: 't' }, it.sev === 'needs' ? (ack ? h('span', { class: 'clock', 'aria-hidden': 'true' }, markEl('seal', { size: 14 })) : h('i', { class: 'flag', 'aria-hidden': 'true' })) : h('span', { class: 'clock', 'aria-hidden': 'true' }, markEl('clock', { size: 14 })), it.title, ack ? h('span', { class: 'dim', style: { fontSize: '13px', fontWeight: 400 } }, ' · acknowledged') : null),
      it.detail ? h('div', { class: 'd' }, it.detail) : null,
      it.steps.length ? h('ol', null, it.steps.map((s, k) => h('li', { class: s.done ? 'done' : k === next ? 'next' : '', 'data-key': 's' + k },
        h('span', null, s.label),
        s.done ? null : s.href
          ? h('a', { class: 'btn small' + (k === next ? ' primary' : ''), href: ctx.href(s.href) }, s.act)
          : h('button', { class: 'btn small' + (k === next ? ' primary' : ''), 'data-action': s.action, 'data-job': s.job, 'data-units': s.units || '' }, s.act)))) : null),
    h('div', { class: 'n' }, f.num(it.count), h('small', null, it.unit)));
}

export function roots(ctx, items, { handled = true } = {}) {
  const needs = items.filter((x) => x.sev === 'needs'), done = items.filter((x) => x.sev === 'handled');
  return [
    h('h2', null, needs.length ? `Needs you · ${needs.length}` : 'Needs you'),
    needs.length ? needs.map((x) => rootCard(ctx, x)) : h('div', { class: 'd1-quiet' }, 'Nothing needs you. Everything below is on its way.'),
    handled && done.length ? [h('h2', null, `The farm is handling · ${done.length}`), done.map((x) => rootCard(ctx, x))] : null,
  ];
}

// ------------------------------------------------------------ a job row

export function bar(g, late) {
  const d = Math.max(0, Math.min(1, g.done)), r = Math.max(0, Math.min(1 - d, g.run || 0));
  return h('div', { class: 'd1-bar' + (late ? ' late' : ''), role: 'img', 'aria-label': `${Math.round(d * 100)}% done${r ? `, ${Math.round(r * 100)}% at work` : ''}` },
    h('i', { class: 'd', style: { width: `${(d * 100).toFixed(1)}%` } }),
    h('i', { class: 'r', style: { left: `${(d * 100).toFixed(1)}%`, width: `${(r * 100).toFixed(1)}%` } }));
}

function jobRow(ctx, j, items) {
  const e = j.estimate || {}, g = glance(j);
  const fin = j.phase === 'Succeeded';
  const late = !fin && e.slackSeconds < 0;
  const w = jobWords(j);
  const dl = j.objective && j.objective.deadline;
  // a job folded into a root item points to it, instead of its own red
  const it = items ? incidentOf(items, keyOf(j)) : null;
  return h('a', { class: 'd1-job', href: ctx.href(`job/${keyOf(j)}`), 'data-key': 'j-' + keyOf(j) },
    h('div', null, h('div', { class: 'nm' }, j.name), h('div', { class: 'st ' + (it ? '' : w.cls) }, w.why ? `${w.word} ${w.why}` : w.word),
      it ? h('div', { class: 'see' }, `see: ${it.title}`) : null),
    h('div', null, bar(g, late), h('div', { class: 'd1-pct' }, `${Math.round(g.done * 100)}% done`)),
    h('div', { class: 'd1-when' + (late ? ' late' : '') }, fin ? 'done' : e.noEta ? '–' : `~${f.when(e.etaP50)}`,
      h('small', null, fin ? '' : late ? `late: due ${f.when(dl)}` : dl ? `due ${f.when(dl)}` : 'no deadline')),
    h('div', { class: 'd1-act' }, h('span', { class: 'btn small' }, 'Open')));
}

const foot = () => h('div', { class: 'd1-foot' }, 'Level 1. Everything else is one click away: open any job for its frames, or change your level in the top bar.');

// ------------------------------------------------------------ views

export const mine1 = {
  render(ctx) {
    const s = ctx.snap, v = s.viewer || {};
    // my jobs, and any job that waits for my decision
    const waitsForMe = (j) => (j.waits || []).some((w) => !w.do && w.waitsFor === `person:${v.user}`);
    let jobs = s.jobs.filter((j) => j.owner === v.user || waitsForMe(j));
    const own = jobs.length > 0;
    if (!own) jobs = s.jobs.filter((j) => (v.projects || []).includes(j.project));
    const items = incidents(s, jobs).filter((x) => x.sev === 'needs');
    const open = jobs.filter((j) => j.phase !== 'Succeeded').sort((a, b) => (a.rank || 1e9) - (b.rank || 1e9));
    return h('section', { class: 'd1', 'data-key': 'd1-mine' },
      h('h1', null, own ? 'My jobs' : 'Jobs in your projects'),
      h('p', { class: 'd1-sub' }, `${open.length} working or waiting · the farm runs them by their deadlines`),
      roots(ctx, items),
      h('h2', null, 'Working and waiting'),
      open.length ? open.map((j) => jobRow(ctx, j, items)) : h('div', { class: 'd1-quiet' }, 'Nothing is running for you right now.'),
      foot());
  },
};

export const farm1 = {
  render(ctx) {
    const s = ctx.snap;
    const items = incidents(s);
    const open = s.jobs.filter((j) => j.phase !== 'Succeeded').sort((a, b) => (a.rank || 1e9) - (b.rank || 1e9));
    const late = open.filter((j) => (j.estimate || {}).slackSeconds < 0);
    const list = (late.length ? late : open).slice(0, 8);
    return h('section', { class: 'd1', 'data-key': 'd1-farm' },
      h('h1', null, 'The farm'),
      h('p', { class: 'd1-sub' }, `${f.num(open.length)} jobs working or waiting · ${late.length ? `${f.num(late.length)} projected late` : 'every deadline holds'} · ${f.num(s.farm.slotsUsed)} of ${f.num(s.farm.slotsTotal)} slots in use`),
      roots(ctx, items),
      h('h2', null, late.length ? `Projected late · ${f.num(late.length)}${late.length > list.length ? ` · the ${list.length} most at risk` : ''}` : 'Next in the queue'),
      list.map((j) => jobRow(ctx, j, items)),
      foot());
  },
};

// job1: a job at level 1: what it is doing, its stages as progress, the one
// thing to do if anything needs you, and a peek at its frames
const det = new Map();
function detailOf(ctx, k) {
  const c = det.get(k);
  if (!c || c.at !== ctx.snap.now) {
    det.set(k, { at: ctx.snap.now, d: c && c.d });
    get(`/jobs/${k}`).then((d) => { det.set(k, { at: ctx.snap.now, d }); ctx.refresh(); }, () => {});
  }
  return c && c.d;
}
const ORDER = ['charter', 'build', 'check', 'review', 'refute', 'merge', 'wait'];
export const job1 = {
  render(ctx) {
    const [ns, name] = ctx.route.parts;
    const k = `${ns}/${name}`;
    const j = ctx.job(k);
    if (!j) return h('div', { class: 'd1' }, 'Reading the job…');
    const d = detailOf(ctx, k);
    const g = glance(j, d);
    const e = j.estimate || {};
    const fin = j.phase === 'Succeeded', late = !fin && e.slackSeconds < 0;
    const dl = j.objective && j.objective.deadline;
    const w = jobWords(j);
    const items = incidents(ctx.snap, [j]).filter((x) => x.sev === 'needs');
    const by = new Map();
    for (const u of (d && d.unitList) || []) {
      if (!by.has(u.group)) by.set(u.group, { n: 0, done: 0, run: 0, label: ((d.groups || []).find((x) => x.key === u.group) || {}).label || u.group });
      const s = by.get(u.group);
      s.n++;
      if (u.state === 'succeeded' || u.state === 'accepted') s.done++;
      else if (u.state === 'running' || u.state === 'retrying' || u.state === 'preflight') s.run++;
    }
    const stages = [...by].sort((a, b) => (ORDER.indexOf(a[0]) + 1 || 50) - (ORDER.indexOf(b[0]) + 1 || 50)).map(([, s]) => s);
    const all = ctx.snap.viewer && ctx.snap.viewer.allProjects;
    return h('section', { class: 'd1', 'data-key': 'd1-job' },
      h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, all ? '← The farm' : '← My jobs')),
      h('h1', null, j.name),
      h('p', { class: 'd1-sub ' + w.cls }, `${w.why ? `${w.word} ${w.why}` : w.word} · ${fin ? 'done' : e.noEta ? 'no ETA yet' : `ready ~${f.when(e.etaP50)}`}${dl ? ` · due ${f.when(dl)}` : ''}${late ? ' · projected late' : ''}`),
      items.length ? roots(ctx, items, { handled: false }) : null,
      decisions(ctx, j, k),
      h('h2', null, 'Progress'),
      h('div', { class: 'd1-stage' }, h('span', { class: 'nm' }, 'All of it'), bar(g, late), h('span', { class: 'ct' }, `${Math.round(g.done * 100)}%`)),
      stages.length > 1 ? h('div', { class: 'd1-stages' }, stages.map((s) => h('div', { class: 'd1-stage', 'data-key': 'st-' + s.label },
        h('span', { class: 'nm' }, s.label), bar({ done: s.n ? s.done / s.n : 0, run: s.n ? s.run / s.n : 0 }, false), h('span', { class: 'ct' }, `${f.num(s.done)} of ${f.num(s.n)}`)))) : null,
      h('div', { class: 'd1-peek' },
        h('a', { class: 'btn', href: ctx.href(`job/${k}?peek=2`), 'data-peek': '1' }, 'Show the frames'),
        h('span', null, 'Opens this job at level 2: its frames, stages and time. Your level stays 1.')),
      foot());
  },
};

export const pools1 = {
  render(ctx) {
    const s = ctx.snap;
    const items = incidents(s);
    return h('section', { class: 'd1', 'data-key': 'd1-pools' },
      h('h1', null, 'Capacity'),
      h('p', { class: 'd1-sub' }, `${f.num(s.farm.slotsUsed)} of ${f.num(s.farm.slotsTotal)} slots in use`),
      roots(ctx, items),
      h('h2', null, 'Pools'),
      (s.pools || []).map((p) => h('div', { class: 'd1-pool', 'data-key': 'p-' + p.name },
        h('span', { class: 'nm' }, p.name),
        bar({ done: 0, run: p.slots ? p.used / p.slots : 0 }, false),
        h('span', { class: 'ct' }, `${f.num(p.used)} of ${f.num(p.slots)} slots`))),
      foot());
  },
};
