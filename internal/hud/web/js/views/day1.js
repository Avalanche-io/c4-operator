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
import { jobWords, fromTracker } from '../../design/components/words.js';
import { markEl } from '../../design/components/marks.js';
import { incidents, incidentOf } from '../../design/components/incidents.js';
import { decisions } from './controls.js';
import { outsideQueue } from './sessions.js';
import { farmSetup } from './setup.js';

const keyOf = (j) => `${j.namespace}/${j.name}`;

// ------------------------------------------------------------ root items

// openNeed: the open question an item's steps act on, from its job
function openNeed(ctx, it) {
  const s = it.steps.find((x) => x.action === 'supply');
  const j = s && ctx.job(s.job);
  return j ? (j.needs || []).find((n) => String(n.unit) === String(s.units) && !n.supplied && !n.withdrawn) : null;
}

// a step's input, for the controls that carry words: the answer a unit
// stopped for, or why its question no longer applies
const INPUT = {
  supply: { attr: 'data-text', tag: 'textarea', ph: 'Your answer. The agent resumes with it where it stopped.' },
  'withdraw-needs': { attr: 'data-why', tag: 'input', ph: 'Why it no longer applies' },
};

export function rootCard(ctx, it) {
  const next = it.steps.findIndex((s) => !s.done);
  // acknowledged: someone has it, so it goes pale with their seal (the pale rule)
  const ack = it.state === 'acknowledged', resolved = it.state === 'resolved';
  // a question's tier: at risk while raised, orange once pressing
  const tier = it.sev === 'needs' && it.severity ? ' d1-' + it.severity : '';
  const need = it.id.startsWith('needsinformation:') ? openNeed(ctx, it) : null;
  const [ask, press] = it.detail.split('; pressing: ');
  const icon = it.sev === 'needs'
    ? (ack ? h('span', { class: 'clock', 'aria-hidden': 'true' }, markEl('seal', { size: 14 })) : h('i', { class: 'flag', 'aria-hidden': 'true' }))
    : h('span', { class: 'clock', 'aria-hidden': 'true' }, markEl(resolved ? 'check' : 'clock', { size: 14 }));
  return h('div', { class: 'd1-root d1-' + it.sev + tier + (ack ? ' d1-ack' : '') + (resolved ? ' d1-resolved' : ''), 'data-key': 'root-' + it.id, role: it.sev === 'needs' && !ack ? 'alert' : 'status' },
    h('i', { class: 'bar', 'aria-hidden': 'true' }),
    h('div', { class: 'body' },
      h('div', { class: 't' }, icon, it.title, ack ? h('span', { class: 'dim', style: { fontSize: '13px', fontWeight: 400 } }, ' · acknowledged') : resolved ? h('span', { class: 'dim', style: { fontSize: '13px', fontWeight: 400 } }, ' · resolved') : null),
      ask ? h('div', { class: 'd' }, ask) : null,
      press ? h('div', { class: 'd press' }, `Pressing: ${press}.`)
        : need && need.answerBy ? h('div', { class: 'd by' }, `Answer by ${f.when(need.answerBy)} and ${it.jobs[0].split('/').pop()} still makes its deadline; the rest of it runs on meanwhile.`) : null,
      it.steps.length ? h('ol', null, it.steps.map((s, k) => {
        const inp = !s.done && s.action && INPUT[s.action], id = inp ? `${s.action}-${s.job}-${s.units}`.replace(/[^\w-]/g, '-') : '';
        return h('li', { class: (s.done ? 'done' : k === next ? 'next' : '') + (inp ? ' with-input' : ''), 'data-key': 's' + k },
          h('span', null, s.label),
          inp ? h(inp.tag, { id, class: 'd1-in', placeholder: inp.ph, 'aria-label': inp.ph, rows: inp.tag === 'textarea' ? 2 : undefined }) : null,
          s.done ? null : s.href
            ? h('a', { class: 'btn small' + (k === next ? ' primary' : ''), href: ctx.href(s.href) }, s.act)
            : h('button', { class: 'btn small' + (k === next ? ' primary' : ''), 'data-action': s.action, 'data-job': s.job, 'data-units': s.units || '', ...(inp ? { [inp.attr]: id } : {}) }, s.act));
      })) : null),
    h('div', { class: 'n' }, f.num(it.count), h('small', null, it.unit)));
}

// (empty: nothing is listed below, so nothing is "on its way")
export function roots(ctx, items, { handled = true, empty = false } = {}) {
  const needs = items.filter((x) => x.sev === 'needs'), done = items.filter((x) => x.sev === 'handled');
  return [
    h('h2', null, needs.length ? `Needs you · ${needs.length}` : 'Needs you'),
    needs.length ? needs.map((x) => rootCard(ctx, x)) : h('div', { class: 'd1-quiet' }, empty ? 'Nothing needs you.' : 'Nothing needs you. Everything below is on its way.'),
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

// agentGlance: an agent bundle's progress is its work, not its units (one
// member is one unit until it ends): what it has done and is doing, of all
// it is expected to need
function agentGlance(j) {
  const w = j.work || {};
  const spent = (w.doneSlotSeconds || 0) + (w.runningSlotSeconds || 0), left = w.remainingSlotSeconds || 0;
  return { done: spent + left > 0 ? spent / (spent + left) : 0, run: 0, left };
}

// agentWords: an agent bundle in its own words: working, or paused at a
// turn's end for capacity, or waiting for your answer
function agentWords(j) {
  const open = (j.needs || []).filter((n) => !n.supplied && !n.withdrawn);
  const run = j.units ? j.units.running || 0 : 0;
  if (open.length) {
    const p = open.some((n) => n.state === 'pressing');
    return { word: run ? `working · ${open[0].name || 'an agent'} waits for your answer` : 'waits for your answer', cls: p ? 't-temp' : 't-risk', why: '' };
  }
  if (j.phase === 'Running') return { word: `working${j.units.total > 1 ? ` · ${run} of ${j.units.total} agents` : ''}`, cls: 't-run', why: '' };
  if (/checkpoint/.test((j.estimate || {}).noEta || '')) return { word: "paused at a turn's end", cls: '', why: '· resumes from its checkpoint when capacity returns' };
  return null;
}
const isAgent = (j) => j.driver === 'agent';

// memberWord: one agent of a bundle in a word: working, asks you, paused
// at a turn's end, done
function memberWord(j, u) {
  if (u.state === 'succeeded') return 'done';
  if (u.state === 'running' || u.state === 'preflight') return 'working';
  if (u.state === 'needs' || (j.needs || []).some((n) => n.unit === u.i && !n.supplied && !n.withdrawn)) return 'asks you';
  if (u.state === 'failed') return 'needs you';
  return (u.attempts || u.epoch) || /checkpoint/.test((j.estimate || {}).noEta || '') ? 'paused' : 'queued';
}

function jobRow(ctx, j, items) {
  const e = j.estimate || {}, agent = isAgent(j), g = agent ? agentGlance(j) : glance(j);
  const fin = j.phase === 'Succeeded';
  const late = !fin && e.slackSeconds < 0;
  const w = (agent && !fin && agentWords(j)) || jobWords(j);
  const dl = j.objective && j.objective.deadline;
  // a job folded into a root item points to it, instead of its own red
  const it = items ? incidentOf(items, keyOf(j)) : null;
  return h('a', { class: 'd1-job', href: ctx.href(`job/${keyOf(j)}`), 'data-key': 'j-' + keyOf(j) },
    h('div', null, h('div', { class: 'nm' }, j.name), h('div', { class: 'st ' + (it ? '' : w.cls) }, w.why ? `${w.word} ${w.why}` : w.word),
      it ? h('div', { class: 'see' }, `see: ${it.title}`) : null,
      // the row is a link to the job, so where it came from is words here,
      // and a link on the job's page
      fromTracker(j) ? h('div', { class: 'see' }, fromTracker(j).text) : null),
    h('div', null, bar(g, late), h('div', { class: 'd1-pct' }, agent && !fin ? (g.left ? `about ${f.dur(g.left)} of work left` : '') : `${Math.round(g.done * 100)}% done`)),
    h('div', { class: 'd1-when' + (late ? ' late' : '') }, fin ? 'done' : e.noEta ? '–' : `~${f.when(e.etaP50)}`,
      h('small', null, fin ? '' : late ? `late: due ${f.when(dl)}` : dl ? `due ${f.when(dl)}` : 'no deadline')),
    h('div', { class: 'd1-act' }, h('span', { class: 'btn small' }, 'Open')));
}

// slotWords: slots in use; past what is offered (a pool just capped, say by
// a laptop's power), how many stop at their next turn's end or safe point
export function slotWords(s) {
  const used = s.farm.slotsUsed, total = s.farm.slotsTotal;
  if (used <= total) return `${f.num(used)} of ${f.num(total)} slots in use`;
  const over = used - total;
  const agents = s.jobs.filter((j) => j.phase === 'Running').every(isAgent);
  return `${f.num(used)} running, ${f.num(total)} slots offered now: ${f.num(over)} ${over === 1 ? 'stops' : 'stop'} at the end of ${agents ? (over === 1 ? 'its turn' : 'their turns') : (over === 1 ? 'its next safe point' : 'their next safe points')}`;
}

// trackerLink: where a job came from, linked when it can be
export const trackerLink = (t) => (t.href ? h('a', { href: t.href, target: t.href.startsWith('/') ? null : '_blank', rel: 'noopener' }, t.text) : t.text);

const foot = () => h('div', { class: 'd1-foot' }, 'Level 1. Everything else is one click away: open any job for its frames, or change your level in the top bar.');

// firstJob: where a farm with nothing on it sends a newcomer next
// (FIRST-JOB.md will give each kind of project its own first job)
const firstJob = () => h('span', null, 'Submit a job: ', h('code', null, 'c4 farm submit'), ', or see the quickstart.');

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
      h('p', { class: 'd1-sub' }, open.length ? `${open.length} working or waiting · the farm runs them by their deadlines` : 'No jobs yet'),
      roots(ctx, items, { empty: !open.length }),
      h('h2', null, 'Working and waiting'),
      open.length ? open.map((j) => jobRow(ctx, j, items)) : h('div', { class: 'd1-quiet' }, 'Nothing is running for you right now. ', firstJob()),
      outsideQueue(ctx),
      farmSetup(ctx),
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
      h('p', { class: 'd1-sub' }, open.length ? `${f.count(open.length, 'job')} working or waiting · ${late.length ? `${f.num(late.length)} projected late` : 'every deadline holds'} · ${slotWords(s)}` : `No jobs yet · ${slotWords(s)}`),
      roots(ctx, items, { empty: !open.length }),
      h('h2', null, late.length ? `Projected late · ${f.num(late.length)}${late.length > list.length ? ` · the ${list.length} most at risk` : ''}` : 'Next in the queue'),
      list.length ? list.map((j) => jobRow(ctx, j, items)) : h('div', { class: 'd1-quiet' }, 'Nothing is running on the farm yet. ', firstJob()),
      outsideQueue(ctx),
      farmSetup(ctx),
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
    const g = isAgent(j) ? agentGlance(j) : glance(j, d);
    const e = j.estimate || {};
    const fin = j.phase === 'Succeeded', late = !fin && e.slackSeconds < 0;
    const dl = j.objective && j.objective.deadline;
    const aw = isAgent(j) && !fin ? agentWords(j) : null;
    const w = aw || jobWords(j);
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
      fromTracker(j) ? h('p', { class: 'd1-from' }, trackerLink(fromTracker(j))) : null,
      h('p', { class: 'd1-sub ' + w.cls }, `${w.why ? `${w.word} ${w.why}` : w.word}${aw && e.noEta ? '' : ` · ${fin ? 'done' : e.noEta ? 'no ETA yet' : `ready ~${f.when(e.etaP50)}`}`}${dl ? ` · due ${f.when(dl)}` : ''}${late ? ' · projected late' : ''}`),
      items.length ? roots(ctx, items, { handled: false }) : null,
      decisions(ctx, j, k),
      h('h2', null, 'Progress'),
      h('div', { class: 'd1-stage' }, h('span', { class: 'nm' }, 'All of it'), bar(g, late), h('span', { class: 'ct' }, `${Math.round(g.done * 100)}%`)),
      isAgent(j) ? h('div', { class: 'd1-stages' }, ((d && d.unitList) || []).map((u) => h('div', { class: 'd1-stage', 'data-key': 'st-' + u.label },
        h('span', { class: 'nm' }, u.label), bar({ done: u.state === 'succeeded' ? 1 : 0, run: u.state === 'running' ? 1 : 0 }, false), h('span', { class: 'ct' }, memberWord(j, u)))))
        : stages.length > 1 ? h('div', { class: 'd1-stages' }, stages.map((s) => h('div', { class: 'd1-stage', 'data-key': 'st-' + s.label },
          h('span', { class: 'nm' }, s.label), bar({ done: s.n ? s.done / s.n : 0, run: s.n ? s.run / s.n : 0 }, false), h('span', { class: 'ct' }, `${f.num(s.done)} of ${f.num(s.n)}`)))) : null,
      h('div', { class: 'd1-peek' },
        h('a', { class: 'btn', href: ctx.href(`job/${k}?peek=2`), 'data-peek': '1' }, isAgent(j) ? 'Show the agents' : 'Show the frames'),
        h('span', null, isAgent(j) ? 'Opens this job at level 2: each agent along time, its checkpoints and the bundle’s own clock. Your level stays 1.' : 'Opens this job at level 2: its frames, stages and time. Your level stays 1.')),
      foot());
  },
};

export const pools1 = {
  render(ctx) {
    const s = ctx.snap;
    const items = incidents(s);
    return h('section', { class: 'd1', 'data-key': 'd1-pools' },
      h('h1', null, 'Capacity'),
      h('p', { class: 'd1-sub' }, slotWords(s)),
      roots(ctx, items),
      h('h2', null, 'Pools'),
      (s.pools || []).map((p) => h('div', { class: 'd1-pool', 'data-key': 'p-' + p.name },
        h('span', { class: 'nm' }, p.name),
        bar({ done: 0, run: p.slots ? p.used / p.slots : 0 }, false),
        h('span', { class: 'ct' }, `${f.num(p.used)} of ${f.num(p.slots)} slots`))),
      foot());
  },
};
