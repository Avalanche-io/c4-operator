// The farm sheet, the HUD's home: every job in scope in dispatch order. Each
// row carries the job's PYLON, its batches collapsed to aggregates, its
// deadline, and its slack against the datum (the deadline is sea level:
// height above it in pencil, depth below it in red). Above the neatline, the
// shots and the deliveries they feed, so a coordinator sees what is at risk,
// and the decisions waiting on the viewer (a burst over budget), each with
// its price, the budget after it and the seal approving it writes.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { own } from '../../design/components/live.js';
import { glance } from '../../design/components/core.js';
import { seal } from '../../design/components/ledger.js';
import { batchStrip, datumBar, DATUM_TICKS, datumX, DATUM_X, shotsOf, shotKeyFn, burstDecision, viewerSeal } from '../../design/components/dispatch.js';
import { classChip, orderText, failedUnits, empty } from './common.js';

const FILTERS = [['all', 'All'], ['render', 'Render'], ['ci', 'CI'], ['ml', 'ML'], ['other', 'Sim and media']];

const st = { dismissed: new Set() };

const keyOf = (j) => `${j.namespace}/${j.name}`;

function lensOf(ctx, j) {
  const c = ctx.cls(j.class);
  return c ? c.lens : '';
}

function matches(ctx, j, flt) {
  if (flt === 'all') return true;
  const lens = lensOf(ctx, j);
  if (flt === 'other') return !['render', 'ci', 'ml'].includes(lens);
  return lens === flt;
}

// queue hrefs keep the other filter
function qhref(ctx, q) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(q)) if (v && v !== 'all') p.set(k, v);
  const s = p.toString();
  return ctx.href(s ? `queue?${s}` : 'queue');
}

function raw(tag, attrs, html) {
  const el = h(tag, attrs);
  el.innerHTML = html;
  return el;
}

const isLate = (j) => j.phase !== 'Succeeded' && j.objective && j.objective.deadline && Number.isFinite(j.estimate.slackSeconds) && j.estimate.slackSeconds < 0;

function holdMark() {
  return h('i', { class: 'holdmark', 'aria-hidden': 'true' });
}

function phase(j) {
  if (j.phase === 'Held') return h('span', { class: 'ph held' }, holdMark(), 'held');
  if (j.phase === 'Failed') return h('span', { class: 'ph t-fail' }, 'failed');
  if (j.phase === 'Running' && j.units.retrying) return h('span', { class: 'ph' }, h('span', { class: 't-run' }, 'running'), h('span', { class: 't-temp' }, ` · ${j.units.retrying} retrying`));
  if (j.phase === 'Running') return h('span', { class: 'ph t-run' }, 'running');
  if (j.phase === 'Succeeded') return h('span', { class: 'ph t-done' }, 'done');
  return h('span', { class: 'ph' }, j.phase.toLowerCase());
}

// ------------------------------------------------------------ rows

function row(ctx, j, multiProject, sk) {
  const k = keyOf(j);
  const u = j.units, e = j.estimate;
  const fin = j.phase === 'Succeeded';
  const late = isLate(j);
  const lens = lensOf(ctx, j);
  const strip = batchStrip(j, { w: 200, h: 22, lens });
  const shot = sk(j);
  const dl = j.objective && j.objective.deadline;
  const passed = dl && Date.parse(dl) < f.now() && !fin;
  const capt = fin ? `${f.num(u.succeeded)} done` : [
    `${f.num(u.succeeded)}/${f.num(u.total)}`,
    u.running + u.retrying ? h('span', { class: 't-run' }, `${u.running + u.retrying} run`) : null,
    u.retrying ? h('span', { class: 't-temp' }, `${u.retrying} retry`) : null,
    u.failed ? h('span', { class: 't-fail' }, `${u.failed} failed`) : null,
    u.unevaluated ? `${f.num(u.unevaluated)} to expand` : null,
  ].filter(Boolean).flatMap((x, i) => (i ? [' · ', x] : [x]));
  const b = e.burst;
  const act = [];
  if (!fin && b && b.needsApproval) {
    act.push(h('button', { class: 'btn request', 'data-action': 'approve', 'data-pool': b.pool, title: `Approve the pending request: ${b.slots} ${b.pool} slots for ${j.name}` },
      h('span', null, `+${b.slots} ${b.pool}`), h('b', null, f.money(b.cost))));
  } else if (j.phase === 'Held') {
    act.push(h('button', { class: 'btn small', 'data-action': 'release', 'data-job': k }, 'Release'));
  } else if (u.failed > 0) {
    act.push(h('button', { class: 'btn small', 'data-action': 'requeue', 'data-job': k, 'data-units': failedUnits(j) }, `Requeue ${u.failed} failed`));
  }
  const dueCell = dl ? h('div', { class: 'qdue' },
    h('span', { class: 'ink' + (passed ? ' t-fail' : '') }, f.clock(dl)),
    fin ? h('span', { class: 'sub' }, `done ${f.clock(e.etaP90, false)}`)
      : h('span', { class: 'sub' }, 'p90 ', h('span', { class: 'pencil' + (late ? ' t-fail' : '') }, f.clock(e.etaP90, false))))
    : h('div', { class: 'qdue' }, h('span', { class: 'dimmer' }, '–'), h('span', { class: 'sub' }, fin ? `done ${f.clock(e.etaP90, false)}` : 'no deadline'));
  const slack = dl && !fin && Number.isFinite(e.slackSeconds)
    ? h('div', { class: 'qslack' + (late ? ' late' : '') }, h('span', { class: late && !passed ? 'pencil' : '' }, f.signed(e.slackSeconds)), h('small', null, late ? (passed ? 'past due' : 'projected late') : 'slack'))
    : h('div', { class: 'qslack none' }, '–');
  return h('div', { class: `qr${fin ? ' fin' : ''}${late ? ' late' : ''}${j.phase === 'Held' ? ' held' : ''}`, 'data-key': k, role: 'listitem' },
    h('div', { class: 'qrank' }, j.rank > 0 && !fin ? String(j.rank) : ''),
    own('pylon', 'qpy:' + k, { glance: glance(j), w: 30, h: 46, mode: 'mini' }, { class: 'qpy' }),
    h('div', { class: 'qname' },
      h('a', { class: 'jn', href: ctx.href(`job/${j.namespace}/${j.name}`) }, j.name),
      h('div', { class: 'qmeta' },
        classChip(j, ctx.cls(j.class)), phase(j),
        j.owner ? h('span', null, j.owner) : null,
        multiProject ? h('span', { class: 'dimmer' }, j.project) : null,
        h('span', { class: 'dimmer' }, orderText(j.order)),
        j.blacklist && j.blacklist.length ? h('span', { class: 't-temp', title: 'Nodes this job will not use' }, `avoids ${j.blacklist.join(', ')}`) : null)),
    h('div', { class: 'qshot' }, shot ? h('a', { href: qhref(ctx, { ...ctx.route.q, shot }), class: 'shotref', title: `Only sheet ${shot}` }, shot) : h('span', { class: 'dimmer' }, '–')),
    h('div', { class: 'qstrip' },
      raw('div', { class: 'strip', 'data-key': 'strip' }, strip.svg),
      h('div', { class: 'cap' }, capt, j.work && j.work.basis === 'estimate' ? null : h('span', { class: 'dimmer' }, ' · by units'))),
    dueCell,
    raw('div', { class: 'qdatum' }, datumBar(j, { h: 22 })),
    slack,
    h('div', { class: 'qact' }, act));
}

function head() {
  return h('div', { class: 'qr qhead', 'aria-hidden': 'true' },
    h('span', { class: 'lab', style: { textAlign: 'right' } }, '#'),
    h('span'),
    h('span', { class: 'lab' }, 'Job'),
    h('span', { class: 'lab' }, 'Shot'),
    h('span', { class: 'lab' }, 'Batches'),
    h('span', { class: 'lab' }, 'Deadline · p90'),
    h('div', { class: 'datumhead' },
      h('span', { class: 'lab' }, 'Datum'),
      DATUM_TICKS.map(([sec, l]) => h('span', { class: 'tk' + (sec === 0 ? ' zero' : ''), style: { left: `${(datumX(sec) * 100).toFixed(2)}%` } }, l))),
    h('span', { class: 'lab', style: { textAlign: 'right' } }, 'Slack'),
    h('span'));
}

// ------------------------------------------------------------ the shots band

function shotsBand(ctx, shots, cur) {
  if (!shots.length) return null;
  return h('div', { class: 'qshots', role: 'list', 'aria-label': 'Shots and the deliveries they feed' },
    h('div', { class: 'bandlab' }, h('span', { class: 'lab' }, 'Shots'), h('span', { class: 'sub' }, 'each shot one aggregate: its jobs and what they feed')),
    shots.map((s) => {
      const g = s.glance;
      const sl = s.open ? g.slack : NaN;
      const dlv = s.deliveries.filter((d) => d.phase !== 'Succeeded');
      return h('a', { class: `shot${s.late ? ' late' : ''}${s.open ? '' : ' fin'}${cur === s.key ? ' sel' : ''}`, href: qhref(ctx, { ...ctx.route.q, shot: cur === s.key ? '' : s.key }), 'data-key': 'sh:' + s.key, role: 'listitem', 'aria-current': cur === s.key ? 'true' : false },
        own('pylon', 'qshot:' + s.key, { glance: g, w: 24, h: 40, mode: 'mini' }, { class: 'spy' }),
        h('span', { class: 'sname' }, s.key),
        h('span', { class: 'sslack' + (s.late ? ' t-fail' : '') }, s.open ? f.signed(sl) : 'done'),
        h('span', { class: 'ssub' }, `${s.project} · ${s.jobs.length} job${s.jobs.length > 1 ? 's' : ''} · ${Math.round(g.done * 100)}%${g.basis === 'units' ? ' by units' : ''}`),
        dlv.length ? h('span', { class: 'sdel' + (dlv.some(isLate) ? ' t-fail' : '') }, `▸ ${dlv.map((d) => `${d.name.replace(/^[a-z]+-/, '')} ${f.clock(d.objective.deadline, false)}`).join(' · ')}`) : h('span', { class: 'sdel dimmer' }, g.deadline ? `due ${f.clock(g.deadline)}` : ''));
    }));
}

// ------------------------------------------------------------ decisions

function budgetBar(d) {
  const max = Math.max(d.budget, d.after) * 1.06 || 1;
  const X = (v) => (100 * v / max).toFixed(2) + '%';
  return raw('div', { class: 'budget', 'aria-hidden': 'true' },
    `<svg width="100%" height="22"><defs><pattern id="dc-h" width="5" height="5" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><rect width="5" height="5" fill="var(--g09)"/><rect width="1.4" height="5" fill="var(--g16)"/></pattern></defs>`
    + `<rect x="0" y="6" width="100%" height="10" fill="var(--g07)"/>`
    + `<rect x="0" y="6" width="${X(d.spent)}" height="10" fill="var(--g16)"/>`
    + `<rect x="${X(d.spent)}" y="6" width="${X(d.cost)}" height="10" fill="url(#dc-h)"/>`
    + `<rect x="${X(d.spent)}" y="6.5" width="${X(d.cost)}" height="9" fill="none" stroke="var(--g15)" stroke-dasharray="3 2"/>`
    + (d.budget ? `<rect x="${X(d.budget)}" y="0" width="2" height="22" fill="${d.over ? 'var(--fail)' : 'var(--ink)'}"/>` : '')
    + '</svg>');
}

function decisionCard(ctx, d, i, n) {
  const j = d.job, k = keyOf(j), e = j.estimate;
  const s = viewerSeal(ctx.snap.viewer);
  const lateBy = -e.slackSeconds;
  const dl = j.objective.deadline;
  const down = d.lateDownstream.filter((x) => keyOf(x) !== k);
  return h('div', { class: 'dc', 'data-key': 'dc:' + k, role: 'region', 'aria-label': `Decision: ${d.slots} ${d.pool} slots for ${j.name}` },
    h('header', null, h('span', { class: 'lab' }, 'Capacity · approve or decline'), h('span', { class: 'dim mono' }, `decision ${i + 1} of ${n}`)),
    h('div', { class: 'dct' }, h('b', null, `+${d.slots} ${d.pool} slots`), h('span', null, ' for ', h('a', { href: ctx.href(`job/${j.namespace}/${j.name}`) }, j.name))),
    h('p', { class: 'dcp' },
      'Without them it finishes ', h('span', { class: 'pencil' }, `≈${f.clock(e.etaP90, false)}`), ', ',
      lateBy > 0 ? h('span', { class: 't-fail' }, `about ${f.dur(lateBy)} past`) : 'inside', ` its ${f.clock(dl, false)} deadline`,
      down.length ? [', and ', down.map((x, q) => [q ? ' and ' : '', h('b', null, x.name), ` (${x.owner ? x.owner + ', ' : ''}${f.clock(x.objective.deadline, false)})`]), ' slips with it'] : null,
      '. With them, the farm expects it ', h('span', { class: 'pencil' }, 'on time'), '.'),
    h('div', { class: 'dcb' },
      h('div', { class: 'row' }, h('span', { class: 'lab' }, `${j.project} burst budget, today`), h('span', { class: 'mono dim' }, `price est. ${f.money(d.cost)}${d.until ? ` · until ${f.clock(d.until, false)}` : ''}`)),
      budgetBar(d),
      h('div', { class: 'row mono' },
        h('span', null, `${f.money(d.spent)} spent · `, h('span', { class: 'pencil' }, `+${f.money(d.cost)}`)),
        h('span', { class: d.over ? 't-fail' : 'ink' }, `${f.money(d.after)} of ${f.money(d.budget)}${d.over ? ` · ${f.money(d.over)} over` : ''}`))),
    h('div', { class: 'dcs' },
      h('div', { class: 'row' }, h('span', { class: 'lab' }, 'Approving writes one entry to the book'), h('span', { class: 'mono dimmer' }, 'control/v1')),
      h('div', { class: 'entry' }, seal('hud', { open: true, title: 'unsigned until you approve' }),
        h('div', null,
          h('div', { class: 'mono' }, `approve +${d.slots} ${d.pool} · est. ${f.money(d.cost)}${d.over ? ` · ${f.money(d.over)} over budget` : ''}`),
          h('div', { class: 'dim' }, 'signs as ', h('b', null, s.user), ' via the HUD'))),
      h('div', { class: 'entry quiet' }, seal('hud', { open: true, title: 'an agent signs with the same seal' }),
        h('div', { class: 'dim' }, s.agent ? 'A person approving gets the same seal, named.' : ['An agent approving gets the same seal, named: ', h('span', { class: 'mono' }, `agent/… for ${s.user}`), '.']))),
    h('div', { class: 'dcbtn' },
      h('button', { class: 'btn primary', 'data-action': 'approve', 'data-pool': d.pool }, `Approve · est. ${f.money(d.cost)}`),
      h('button', { class: 'btn', 'data-qdismiss': k }, 'Not now')),
    h('p', { class: 'dcfoot' }, `Not now leaves the request pending; the late projection stands.`));
}

// ------------------------------------------------------------ the sheet

export default {
  render(ctx) {
    const s = ctx.snap;
    const q = ctx.route.q;
    const flt = q.class || 'all';
    const sk = shotKeyFn(s.jobs);
    const shotF = q.shot || '';
    const inScope = s.jobs.filter((j) => matches(ctx, j, flt));
    const shots = shotsOf(s.jobs);
    const fShot = shots.find((x) => x.key === shotF);
    const jobs = shotF ? inScope.filter((j) => sk(j) === shotF || (fShot && fShot.deliveries.includes(j))) : inScope;
    const multiProject = s.viewer.projects.length > 1;
    const open = jobs.filter((j) => j.phase !== 'Succeeded');
    const fin = jobs.filter((j) => j.phase === 'Succeeded');
    const allOpen = s.jobs.filter((j) => j.phase !== 'Succeeded');
    const withDl = allOpen.filter((j) => j.objective && j.objective.deadline);
    const late = withDl.filter(isLate);
    const counts = Object.fromEntries(FILTERS.map(([k]) => [k, s.jobs.filter((j) => matches(ctx, j, k)).length]));
    const decisions = allOpen.map((j) => burstDecision(s, j)).filter(Boolean);
    const shown = decisions.filter((d) => !st.dismissed.has(keyOf(d.job)));
    const win = ((s.usage && s.usage.productionWindows) || []).find((w) => w.active);
    const mday = win ? Math.floor((f.now() - Date.parse(win.from)) / 864e5) + 1 : null;
    return h('section', { class: 'sheet qsheet', 'data-key': 'farm', 'aria-labelledby': 'qtitle' },
      h('div', { class: 'sheethead' },
        h('div', null,
          h('div', { class: 'crumbs' }, `Farm / ${s.viewer.allProjects ? 'all projects' : s.viewer.projects.join(' · ')} / dispatch order`),
          h('h1', { id: 'qtitle' }, 'The farm'),
          h('div', { class: 'meta' }, `${allOpen.length} jobs working or waiting, in dispatch order. Each job's deadline and estimate set the order; nobody sets priorities.`)),
        h('div', { class: 'titleblock' },
          h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Datum'), h('span', { class: 'val' }, f.clockSec(s.now)), h('span', { class: 'sub' }, `${f.dateLine(s.now)} · ${f.offsetLabel()}`)),
          h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'On time'), h('span', { class: 'val' }, String(withDl.length - late.length), h('small', null, `of ${withDl.length}`)), h('span', { class: 'sub' }, 'with a deadline')),
          h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Projected late'), h('span', { class: 'val' + (late.length ? ' late' : '') }, String(late.length)), h('span', { class: 'sub' }, late.length ? `worst ${f.signed(Math.min(...late.map((j) => j.estimate.slackSeconds)))}` : 'every deadline holds')),
          h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Awaiting you'), h('span', { class: 'val' }, String(decisions.length)), h('span', { class: 'sub' }, decisions.length ? `burst, est. ${f.money(decisions.reduce((a, d) => a + d.cost, 0))}` : 'no decisions')),
          win ? h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Mission day'), h('span', { class: 'val' }, `D${mday}`), h('span', { class: 'sub' }, `${win.name.toLowerCase()} · to ${f.dateLine(win.to)}`)) : null)),
      h('div', { class: 'qcollar' + (shown.length ? ' has-dc' : '') },
        shown.length ? h('div', { class: 'qdecide' }, shown.map((d, i) => decisionCard(ctx, d, i, shown.length))) : null,
        shotsBand(ctx, shots, shotF)),
      h('div', { class: 'qtools' },
        h('div', { class: 'filters', role: 'group', 'aria-label': 'Filter by work class' },
          h('span', { class: 'lab' }, 'Show'),
          h('span', { class: 'seg' }, FILTERS.map(([k, label]) => h('a', {
            class: 'btn small', href: qhref(ctx, { ...q, class: k }), 'aria-pressed': flt === k ? 'true' : 'false',
          }, `${label} ${counts[k]}`)))),
        shotF ? h('a', { class: 'btn small', href: qhref(ctx, { ...q, shot: '' }), 'aria-pressed': 'true', title: 'Show every shot' }, `Sheet ${shotF} ×`) : null,
        h('span', { class: 'scale mono' }, 'scale · 1 row = 1 job · batches of 24 frames (render) · slack on a log datum, −6h to +3d')),
      h('div', { class: 'neat qneat' },
        h('div', { class: 'edge w' }, '◂ ', h('a', { href: ctx.href('pools') }, 'pools'), ' · capacity feeding the queue'),
        h('div', { class: 'edge e' }, h('a', { href: ctx.href('graph') }, 'graph'), ' · deliveries to the client ▸'),
        h('div', { class: 'edge s' }, '▾ ', h('a', { href: ctx.href('wall') }, 'the wall'), ' · ', h('a', { href: ctx.href('cost') }, 'cost')),
        h('div', { class: 'qtable', style: { '--datum': `${(DATUM_X * 100).toFixed(2)}%` } },
          head(),
          jobs.length ? h('div', { class: 'qlist', role: 'list', 'aria-label': 'Jobs in dispatch order' }, open.map((j) => row(ctx, j, multiProject, sk))) : empty('No jobs match this filter.'),
          fin.length ? h('div', { class: 'qdiv' }, h('span', { class: 'lab' }, `Finished · ${fin.length}`), h('span', { class: 'sub' }, 'settled; kept while they still feed the sheet')) : null,
          fin.length ? h('div', { class: 'qlist fin', role: 'list', 'aria-label': 'Finished jobs' }, fin.map((j) => row(ctx, j, multiProject, sk))) : null)));
  },

  after(ctx, root) {
    for (const b of root.querySelectorAll('[data-qdismiss]')) {
      if (b.dataset.wired) continue;
      b.dataset.wired = '1';
      b.addEventListener('click', () => { st.dismissed.add(b.dataset.qdismiss); ctx.refresh(); });
    }
  },
};
