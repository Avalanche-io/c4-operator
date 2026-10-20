// Catalogue: the published designs for agent work (c4 proposals/agent-
// catalog, CATALOG-HUD 2), read from the export the farm serves
// (/api/v1/catalog, catalog.json). A design is a structure with its
// sources; what runs is a version of it; what the catalogue learns from is
// runs (ratings come only from signed run records, and there are none yet).
//
// By level (level.js):
//   1 what a person needs to pick one: what it is for, whether it runs on
//     this farm today, where a person decides, where it can stop safely
//   2 its shape and its graph, drawn as a plan job is drawn (designsheet.js,
//     the plan as a render), its roles and what each reasons with, its
//     meters (model share, parallelism, the human role), cost, failure modes
//   3 sources with their checks, claims measured or asserted,
//     implementations and their licenses, the C4 mapping and its gaps, the
//     taxonomy, the entry's own record
//
// No new hues: C4 fit is ink ("runs today"), pale ("with a workaround": true,
// with context) or grey ("not yet": it waits on something C4 lacks).

import { h, s } from '../dom.js';
import * as f from '../fmt.js';
import { get } from '../api.js';
import { own } from '../../design/components/live.js';
import { buildSheet } from '../../design/components/stack.js';
import { designSheet, ADDED } from '../../design/components/designsheet.js';
import { markEl } from '../../design/components/marks.js';

const st = { data: null, err: null, busy: false, plans: new Map(), sel: null, selFor: null };

const FIT = {
  direct: { word: 'Runs today', cls: 'cf-direct', why: 'C4 runs it as written.' },
  workaround: { word: 'Runs with a workaround', cls: 'cf-work', why: 'C4 runs it, with something lost; the C4 mapping says what.' },
  'missing-primitive': { word: 'Not yet', cls: 'cf-missing', why: 'It waits on something C4 does not have yet.' },
};
const fitOf = (E) => FIT[(E.c4 && E.c4.maps) || ''] || FIT['missing-primitive'];
// a model share in words; every other value is read as written
const SHARE = { 'very-high': 'nearly all model', high: 'mostly model', medium: 'half model', low: 'mostly code', 'very-low': 'nearly all code', none: 'no model' };
const said = (x) => (x == null || x === '' ? 'not said' : String(x).replace(/-/g, ' '));
const share = (x) => SHARE[x] || said(x);

function load(ctx) {
  if (st.data || st.busy || st.err) return;
  st.busy = true;
  get('/catalog').then((d) => {
    if (!String(d.schema || '').startsWith('c4-agent-catalog/export/v1')) throw new Error(`a catalogue this HUD cannot read (${d.schema || 'no schema'})`);
    st.data = d;
  }).catch((e) => { st.err = e.message; }).finally(() => { st.busy = false; ctx.refresh(); });
}

function plan(ctx, id) {
  if (st.plans.has(id)) return st.plans.get(id);
  st.plans.set(id, null);
  get(`/catalog/${encodeURIComponent(id)}/plan`).then((d) => st.plans.set(id, d)).catch((e) => st.plans.set(id, { err: e.message })).finally(() => ctx.refresh());
  return null;
}

const chref = (ctx, q = {}, id = '') => {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(q)) if (v) p.set(k, v);
  const t = p.toString();
  return ctx.href(`catalog${id ? '/' + encodeURIComponent(id) : ''}${t ? '?' + t : ''}`);
};

// ------------------------------------------------------------ the shape glyph

// A small drawing of the shape in grey blocks, as the plan as a render
// would draw it: columns are steps in flow order, a stippled block is added
// at run time, a loop arc is a bounded pass back.
const GLYPH = {
  chain: [[1], [1], [1], [1]], 'pipeline-roles': [[1], [1], [1], [1]], 'handoff-chain': [[1], [1], [1], [1]],
  'fan-out-fan-in': [[1], [1, 1, 1], [1]], 'ensemble-vote': [[1, 1, 1], [1]], 'best-of-n': [[1], [1, 1, 1, 1], [1]],
  'orchestrator-workers': [[1], [2, 2, 2], [1]], 'plan-then-execute': [[1], [2], [2], [2]], 'dag-compiled': [[1], [1, 1], [1, 1], [1]],
  router: [[1], [1, 0, 0], [1]], hierarchy: [[1], [1, 1], [2, 2, 2, 2]], 'lane-map': [[1, 1], [1, 1, 1], [1, 1], [1]],
  'graph-search': [[1], [1, 1], [1, 1, 1]], evolutionary: [[1, 1, 1], [1, 1, 1], [2, 2, 2]],
  'debate-rounds': [[1, 1], [1, 1], [1]], 'group-chat': [[1, 1, 1], [1, 1, 1]], blackboard: [[1, 1], [1], [1, 1]],
  'review-loop': [[1], [1]], 'single-loop': [[1]],
};
const LOOPS = new Set(['review-loop', 'single-loop', 'debate-rounds', 'evolutionary']);
function glyph(shape) {
  const cols = GLYPH[shape] || [[1], [1]];
  const bw = 9, bh = 5, gx = 5, gy = 2;
  const H = Math.max(...cols.map((c) => c.length)) * (bh + gy) - gy;
  const W = cols.length * (bw + gx) - gx;
  const kids = [];
  cols.forEach((c, x) => {
    const y0 = (H - (c.length * (bh + gy) - gy)) / 2;
    c.forEach((b, y) => kids.push(s('rect', { x: x * (bw + gx), y: y0 + y * (bh + gy), width: bw, height: bh, class: b === 2 ? 'g-exp' : b === 0 ? 'g-off' : 'g-on' })));
  });
  if (LOOPS.has(shape)) kids.push(s('path', { d: `M${W - 2} ${H / 2 + 3} C${W - 2} ${H + 6} 2 ${H + 6} 2 ${H / 2 + 3}`, class: 'g-loop' }));
  return s('svg', { class: 'cglyph', width: W + 2, height: H + (LOOPS.has(shape) ? 8 : 0), viewBox: `-1 -1 ${W + 2} ${H + (LOOPS.has(shape) ? 9 : 1)}`, 'aria-hidden': 'true' }, kids);
}

// ------------------------------------------------------------ the list

function fitBadge(E) {
  const F = fitOf(E);
  const need = (E.c4 && E.c4.gaps) || [];
  return h('span', { class: 'cfit ' + F.cls, title: F.why + (need.length ? ` Needs: ${need.join(', ')}.` : '') }, F.word);
}

function card(ctx, x) {
  const E = x.entry, D = x.derived || {};
  const L = ctx.level;
  const ev = D.evidence || {};
  const impl = D.implementations || {};
  const stale = impl.count > 0 && impl.active === 0 && impl.unknown === 0;
  return h('a', { class: 'ccard', href: chref(ctx, {}, E.id), 'data-key': 'c-' + E.id },
    h('div', { class: 'ch' },
      L >= 2 ? glyph(E.taxonomy && E.taxonomy.shape) : null,
      h('span', { class: 'cn' }, E.name), fitBadge(E)),
    L >= 2 ? h('div', { class: 'cfam' }, `${said(E.family)} · ${said(E.taxonomy && E.taxonomy.shape)}`) : null,
    h('p', { class: 'cs' }, D.summaryLine || E.summary),
    (E.fit && E.fit.suits || []).length ? h('div', { class: 'cline' }, h('b', null, 'Good for'), E.fit.suits[0]) : null,
    (E.structure && E.structure.personDecides || []).length ? h('div', { class: 'cline' }, h('b', null, 'You decide'), E.structure.personDecides[0]) : null,
    L >= 2 ? h('div', { class: 'cmeters' },
      h('span', null, h('i', null, 'model'), share(E.cost && E.cost.modelShare)),
      h('span', null, h('i', null, 'parallel'), said(E.cost && E.cost.parallelism)),
      h('span', null, h('i', null, 'a person'), said(E.taxonomy && E.taxonomy.humanRole)),
      h('span', { title: `${f.num(ev.sourcesVerified || 0)} of ${f.num(ev.sources || 0)} sources checked` }, h('i', null, 'claims'), ev.claims ? `${f.num(ev.measured || 0)} measured of ${f.num(ev.claims)}` : 'none')) : null,
    L >= 2 ? h('div', { class: 'cdom' }, (E.fit && E.fit.domains || []).map((d) => h('span', { class: 'chip' }, d))) : null,
    L >= 3 ? h('div', { class: 'cfoot mono' }, E.id,
      stale ? h('span', { class: 'cpale' }, ' · every implementation archived or superseded') : null,
      (E.c4 && E.c4.gaps || []).length ? ` · needs ${E.c4.gaps.join(', ')}` : '') : null);
}

function filters(ctx, d) {
  const q = ctx.route.q, L = ctx.level;
  const doms = [...new Set(d.entries.flatMap((x) => (x.entry.fit && x.entry.fit.domains) || []))].sort();
  const chipRow = (key, label, vals) => h('div', { class: 'cfrow' }, h('span', { class: 'lab' }, label),
    h('a', { class: 'cf' + (!q[key] ? ' on' : ''), href: chref(ctx, { ...q, [key]: '' }) }, 'all'),
    vals.map(([v, w]) => h('a', { class: 'cf' + (q[key] === v ? ' on' : ''), href: chref(ctx, { ...q, [key]: q[key] === v ? '' : v }) }, w)));
  const axes = L >= 3 ? ['shape', 'control', 'verdictSource', 'humanRole', 'expansion'].map((ax) => chipRow(ax, ax.replace(/[A-Z]/g, (m) => ' ' + m.toLowerCase()), [...new Set(d.entries.map((x) => x.entry.taxonomy && x.entry.taxonomy[ax]).filter(Boolean))].sort().map((v) => [v, v.replace(/-/g, ' ')]))) : [];
  return h('div', { class: 'cfilters' },
    h('label', { class: 'csearch' }, h('span', { class: 'lab' }, 'Find by problem'),
      h('input', { type: 'search', 'data-cat': 'q', value: q.q || '', placeholder: 'multi-file code change, literature review, …', 'aria-label': 'Find a design by the problem it suits' })),
    chipRow('fit', 'On this farm', Object.entries(FIT).map(([k, v]) => [k, v.word])),
    chipRow('domain', 'Domain', doms.map((x) => [x, x])),
    L >= 2 ? chipRow('group', 'Group', d.vocab.groups.map((g) => [g.id, g.label])) : null,
    axes);
}

function matches(x, q) {
  const E = x.entry;
  if (q.fit && (E.c4 && E.c4.maps) !== q.fit) return false;
  if (q.domain && !((E.fit && E.fit.domains) || []).includes(q.domain)) return false;
  if (q.group && (x.derived && x.derived.group) !== q.group) return false;
  for (const ax of ['shape', 'control', 'verdictSource', 'humanRole', 'expansion']) if (q[ax] && (E.taxonomy && E.taxonomy[ax]) !== q[ax]) return false;
  if (q.q) {
    const words = q.q.toLowerCase().split(/\s+/).filter(Boolean);
    const hay = [E.name, ...(E.aliases || []), E.summary, ...((E.fit && E.fit.suits) || []), ...((E.fit && E.fit.domains) || []), ...(E.tags || [])].join(' ').toLowerCase();
    if (!words.every((w) => hay.includes(w))) return false;
  }
  return true;
}

function list(ctx, d) {
  const q = ctx.route.q;
  const filtered = Object.keys(q).some((k) => q[k]);
  const byId = new Map(d.entries.map((x) => [x.entry.id, x]));
  const shown = d.entries.filter((x) => matches(x, q));
  const runs = d.counts && d.counts.maps ? d.counts.maps : {};
  return h('section', { class: 'sheet catalog' },
    h('div', { class: 'sheethead' },
      h('div', null,
        h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ' / catalogue'),
        h('h1', null, 'Catalogue'),
        h('div', { class: 'meta vmeta' }, `Published designs for agent work: how the agents are arranged, who checks whom and where a person decides. Pick one to see what it is for and whether it runs on this farm. As of ${d.asOf}.`)),
      h('div', { class: 'titleblock' },
        h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Designs'), h('span', { class: 'val' }, f.num(d.entries.length))),
        h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Run today'), h('span', { class: 'val' }, f.num(runs.direct || 0)), h('span', { class: 'sub' }, `${f.num(runs.workaround || 0)} more with a workaround`)),
        h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Rated from runs'), h('span', { class: 'val' }, '0'), h('span', { class: 'sub' }, 'ratings come only from signed runs')))),
    filters(ctx, d),
    !filtered && (d.launch || []).length ? h('div', { class: 'cgroup', 'data-key': 'launch' },
      h('h2', null, 'Start with these', h('span', { class: 'dim' }, ` · ${f.num(d.launch.length)} designs that show what C4 does, each runnable or nearly`)),
      h('div', { class: 'cgrid' }, d.launch.map((b) => byId.get(b.entries[0])).filter(Boolean).map((x) => card(ctx, x)))) : null,
    filtered
      ? h('div', { class: 'cgroup', 'data-key': 'found' }, h('h2', null, `${f.num(shown.length)} of ${f.num(d.entries.length)}`, h('a', { class: 'dim', href: chref(ctx), style: { marginLeft: '12px', fontSize: '12px' } }, 'clear')),
        shown.length ? h('div', { class: 'cgrid' }, shown.map((x) => card(ctx, x))) : h('p', { class: 'dim' }, 'Nothing in the catalogue matches. Try fewer words, or another domain.'))
      : d.vocab.groups.map((g) => {
        const xs = d.entries.filter((x) => x.derived && x.derived.group === g.id);
        return xs.length ? h('div', { class: 'cgroup', 'data-key': 'g-' + g.id },
          h('h2', null, g.label, h('span', { class: 'dim' }, ` · ${f.num(xs.length)} · ${g.needs}`)),
          h('div', { class: 'cgrid' }, xs.map((x) => card(ctx, x)))) : null;
      }));
}

// ------------------------------------------------------------ the detail page

const sect = (key, title, ...kids) => h('div', { class: 'panel cpanel', 'data-key': key }, h('header', null, h('h2', null, title)), h('div', { class: 'body' }, kids));
const bullets = (xs) => (xs && xs.length ? h('ul', { class: 'clist' }, xs.map((x) => h('li', null, x))) : h('p', { class: 'dim' }, 'Not said.'));
const doesMark = (does) => (does === 'model' || does === 'procedure' ? h('span', { class: 'cdoes', title: does === 'model' ? 'reasons with a model' : 'a procedure: a command, no model' }, markEl(does, { size: 11 })) : null);

function blockCard(ctx, u) {
  if (!u) return h('p', { class: 'dim' }, 'Hover a block for what it is; select one for why it waits where it does.');
  const D = u.design || {};
  const who = (x) => String(x || 'someone').replace(/^person:/, 'a person, ').replace(/^party:/, 'an outside party, ');
  const what = D.kind === 'added' ? `added at run time by ${D.generator}: ${D.fanOut}` : D.kind === 'check' ? `a check${D.bound ? `: ${D.bound}` : ''}` : D.kind === 'wait' ? `waits for ${who(D.server)}${D.ask ? `: “${D.ask}”` : ''}` : D.fanOut ? `a generator: it adds ${D.fanOut}` : 'work';
  return [
    h('div', { class: 'cbname' }, u.reasoning ? doesMark(u.reasoning.does) : null, h('b', null, u.label), h('span', { class: 'dim' }, ` · ${u.group === ADDED ? 'added' : u.group}`)),
    h('p', null, what + (D.role && !/^(setup|verdict)$/.test(D.role) ? ` · ${D.role}` : '')),
    D.note ? h('p', { class: 'dim' }, D.note) : null,
    D.holds && D.holds.length && ctx.level >= 3 ? h('p', { class: 'dim mono' }, `holds ${D.holds.join(', ')}`) : null,
    D.why && D.why.length ? h('ul', { class: 'clist' }, D.why.map((w) => h('li', null, h('span', { class: 'mono' }, w.after), `: ${w.text}`))) : null,
  ];
}

function graph(ctx, x) {
  const E = x.entry, D = x.derived || {};
  if (!D.plan) {
    return sect('graph', 'The graph',
      h('p', null, E.structure.graph),
      h('p', { class: 'dim' }, 'There is no plan written as data for this design yet, so it is not drawn. Its roles are below.'),
      ctx.level >= 3 && E.structure.mermaid ? h('details', null, h('summary', null, 'The graph as the entry writes it (mermaid)'), h('pre', { class: 'mono cmer' }, E.structure.mermaid)) : null);
  }
  const doc = plan(ctx, E.id);
  if (!doc) return sect('graph', 'The graph', h('p', { class: 'dim' }, 'Reading its plan…'));
  if (doc.err) return sect('graph', 'The graph', h('p', { class: 'dim' }, `Its plan could not be read: ${doc.err}.`), h('p', null, E.structure.graph));
  const det = designSheet(doc, { name: E.id });
  const sheet = buildSheet({ key: `design:${E.id}`, title: E.name, details: [det], anchor: `catalog/${E.id}`, graph: null, jobs: [] });
  if (st.selFor !== E.id) { st.sel = null; st.selFor = E.id; }
  const sel = st.sel != null ? det.unitList.find((u) => u.i === st.sel) : null;
  return h('div', { class: 'panel cpanel', 'data-key': 'graph' },
    h('header', null, h('h2', null, 'The graph'), h('span', { class: 'dim mono', style: { fontSize: '10px' } }, `${f.num(det.unitList.length)} blocks · ${f.num(det.laneEdges.length)} dependencies · drawn from ${String(D.plan).split('/').pop()}, before anything runs`)),
    h('div', { class: 'cgraph' },
      h('div', { class: 'wellbox', 'data-key': 'stackbox' },
        own('stack', 'stack:design:' + E.id, { sheet, view: { flat: false, arcs: 'thin', reasoning: true, effort: ctx.level >= 3, sel: sel ? { job: `catalog/${E.id}`, i: sel.i } : null, box: 12, lineA: false, lineB: new Set(), contours: false } })),
      h('div', { class: 'cbcard' }, blockCard(ctx, sel))),
    h('p', { class: 'dim', style: { padding: '0 14px 12px' } }, 'Grey blocks are queued, as on a farm before anything runs. A stippled row is what a generator adds at run time; the blocks that follow it wait for all of it. Arcs are dependencies; the selected block’s are in ink.'));
}

function detail(ctx, d, x) {
  const E = x.entry, D = x.derived || {}, S = E.structure || {}, L = ctx.level;
  const F = fitOf(E);
  const gaps = new Map((Array.isArray(d.vocab.gaps) ? d.vocab.gaps.map((g) => [g.id, g]) : Object.entries(d.vocab.gaps || {})));
  const sc = (E.fit && E.fit.scale) || {};
  const srcs = new Map((E.sources || []).map((s0) => [s0.id, s0]));
  return h('section', { class: 'sheet catalog', 'data-key': 'cd-' + E.id },
    h('div', { class: 'sheethead chead' },
      h('div', null,
        h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ' / ', h('a', { href: chref(ctx) }, 'catalogue'), ` / ${E.id}`),
        h('h1', null, L >= 2 ? glyph(E.taxonomy && E.taxonomy.shape) : null, E.name),
        h('div', { class: 'meta' }, fitBadge(E), ' ', L >= 2 ? `${said(E.family)} · ${said(E.taxonomy && E.taxonomy.shape)}` : '', (E.aliases || []).length && L >= 2 ? ` · also ${E.aliases.slice(0, 3).join(', ')}` : '')),
      h('div', { class: 'titleblock' },
        h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Agents'), h('span', { class: 'val small' }, sc.agents || 'not said')),
        h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'Wall time'), h('span', { class: 'val small' }, sc.wallTime || 'not said'), h('span', { class: 'sub' }, sc.basis ? `${sc.basis === 'estimate' ? 'the catalogue’s estimate' : sc.basis}` : '')),
        h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'On this farm'), h('span', { class: 'val small' }, F.word), h('span', { class: 'sub' }, D.plan ? 'a plan is written for it' : 'no plan written yet')),
        h('div', { class: 'cell' }, h('span', { class: 'lab' }, 'From runs'), h('span', { class: 'val small' }, 'none yet'), h('span', { class: 'sub' }, 'only signed runs rate it')))),
    h('p', { class: 'csum' }, E.summary),
    h('div', { class: 'cdetail' },
      h('div', { class: 'ccol' },
        L >= 2 ? graph(ctx, x) : null,
        sect('fit', 'What it is for', h('h3', null, 'Good for'), bullets(E.fit && E.fit.suits), h('h3', null, 'Avoid it for'), bullets(E.fit && E.fit.avoid)),
        L >= 2 ? sect('roles', 'Roles',
          h('table', { class: 'ctable' }, h('thead', null, h('tr', null, ['role', 'in C4', 'reasons with', 'how many', ''].map((t) => h('th', null, t)))),
            h('tbody', null, (S.roles || []).map((r) => h('tr', null, h('td', null, r.name), h('td', { class: 'mono' }, r.c4), h('td', null, doesMark(r.does), ' ', r.does), h('td', null, r.count), h('td', { class: 'dim' }, r.notes))))),
          S.expansion ? h('p', { class: 'dim' }, S.expansion) : null) : null,
        L >= 2 ? sect('fail', 'How it fails', (E.failureModes || []).length ? h('ul', { class: 'clist' }, E.failureModes.map((m) => h('li', null, m.text, h('span', { class: 'dim' }, ` · ${m.status === 'measured' ? 'measured' : 'a reading'}${m.source && srcs.has(m.source) ? `, ${srcs.get(m.source).title}` : m.source === 'catalogue' ? ', the catalogue' : ''}`)))) : h('p', { class: 'dim' }, 'Not said.')) : null,
        L >= 3 ? sect('sources', 'Sources and claims',
          h('ul', { class: 'clist' }, (E.sources || []).map((s0) => h('li', null, h('span', { class: 'cver', title: s0.verified ? `opened and checked ${s0.checkedAt || ''}` : 'not checked' }, s0.verified ? markEl('check', { size: 10 }) : '·'), ' ', h('a', { href: s0.url, target: '_blank', rel: 'noopener' }, s0.title), h('span', { class: 'dim' }, ` · ${s0.by || ''}${s0.date ? ', ' + s0.date : ''} · ${s0.kind}`)))),
          h('h3', null, 'Claims'),
          h('ul', { class: 'clist' }, (E.claims || []).map((c) => h('li', null, h('span', { class: 'cst cst-' + c.status }, c.status), ' ', c.text, c.source && srcs.has(c.source) ? h('span', { class: 'dim' }, ` · ${srcs.get(c.source).title}`) : null))),
          h('p', { class: 'dim' }, 'Claims are what the sources say; ratings would be what runs on a C4 farm measured. They are kept apart, never merged.')) : null),
      h('div', { class: 'ccol side' },
        sect('person', 'Where a person decides', bullets(S.personDecides)),
        sect('safe', 'Where it can stop safely', bullets(S.safePoints)),
        L >= 2 ? sect('cost', 'What it costs',
          h('dl', { class: 'cdl' },
            h('dt', null, 'Model share'), h('dd', null, share(E.cost && E.cost.modelShare), E.cost && E.cost.modelShareNote ? h('span', { class: 'dim' }, ` · ${E.cost.modelShareNote}`) : null),
            h('dt', null, 'Parallelism'), h('dd', null, said(E.cost && E.cost.parallelism)),
            h('dt', null, 'Rework'), h('dd', null, said(E.cost && E.cost.reworkRate)),
            E.cost && E.cost.tokens ? [h('dt', null, 'Tokens'), h('dd', null, typeof E.cost.tokens === 'string' ? E.cost.tokens : JSON.stringify(E.cost.tokens))] : null,
            h('dt', null, 'A person'), h('dd', null, said(E.taxonomy && E.taxonomy.humanRole)))) : null,
        L >= 3 ? sect('c4', 'C4 mapping',
          h('p', null, fitBadge(E), ' ', h('span', { class: 'mono dim' }, (E.c4.uses || []).join(' · '))),
          (E.c4.gaps || []).map((g) => h('p', null, h('b', { class: 'mono' }, g), ': ', (gaps.get(g) || {}).definition || '', h('span', { class: 'dim' }, (gaps.get(g) || {}).designStatus ? ` (${gaps.get(g).designStatus})` : ''))),
          E.c4.notes ? h('p', { class: 'dim' }, E.c4.notes) : null,
          D.plan ? h('p', { class: 'mono dim' }, `plan ${D.plan}`) : null) : null,
        L >= 3 ? sect('impl', 'Implementations', (E.implementations || []).length ? h('ul', { class: 'clist' }, E.implementations.map((m) => h('li', { class: m.status === 'archived' || m.status === 'superseded' ? 'cpale' : '' }, h('a', { href: m.url, target: '_blank', rel: 'noopener' }, m.name), h('span', { class: 'dim' }, ` · ${m.license || 'no license named'} · ${m.official ? 'official' : 'community'} · ${m.status}`)))) : h('p', { class: 'dim' }, 'None listed.')) : null,
        L >= 3 ? sect('entry', 'The entry',
          h('dl', { class: 'cdl' },
            h('dt', null, 'Version'), h('dd', null, `${E.entry.version} · ${E.entry.status}`, h('span', { class: 'dim' }, ' · a design version is the C4 ID of its bundle; this one is not published as a bundle yet')),
            h('dt', null, 'Taxonomy'), h('dd', { class: 'mono' }, Object.entries(E.taxonomy || {}).map(([k, v]) => `${k} ${v}`).join(' · ')),
            h('dt', null, 'Updated'), h('dd', null, `${E.entry.updated} by ${E.entry.author}`),
            (E.entry.related || []).length ? [h('dt', null, 'Related'), h('dd', null, E.entry.related.map((r, k) => [k ? ', ' : '', d.entries.some((y) => y.entry.id === r) ? h('a', { href: chref(ctx, {}, r) }, r) : r]))] : null)) : null)));
}

export default {
  render(ctx) {
    load(ctx);
    if (!st.data) {
      return h('section', { class: 'sheet catalog' }, h('div', { class: 'sheethead' }, h('div', null,
        h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Farm'), ' / catalogue'),
        h('h1', null, 'Catalogue'),
        h('p', { class: 'dim' }, st.err ? (/no catalogue/.test(st.err) ? 'This farm has no catalogue of designs.' : `The catalogue could not be read: ${st.err}.`) : 'Reading the catalogue…'))));
    }
    const id = ctx.route.parts[0];
    if (!id) return list(ctx, st.data);
    const x = st.data.entries.find((y) => y.entry.id === id);
    if (!x) return h('section', { class: 'sheet catalog' }, h('div', { class: 'sheethead' }, h('div', null, h('div', { class: 'crumbs' }, h('a', { href: chref(ctx) }, 'Catalogue')), h('h1', null, id), h('p', { class: 'dim' }, 'No design by that name in the catalogue.'))));
    return detail(ctx, st.data, x);
  },

  after(ctx, root) {
    const inp = root.querySelector('[data-cat="q"]');
    if (inp && !inp.dataset.wired) {
      inp.dataset.wired = '1';
      inp.addEventListener('change', () => { location.hash = chref(ctx, { ...ctx.route.q, q: inp.value.trim() }).split('#')[1]; });
    }
    const box = root.querySelector('.cgraph');
    if (box && !box.dataset.wired) {
      box.dataset.wired = '1';
      box.addEventListener('stack:select', (e) => { st.sel = e.detail.i; ctx.refresh(); });
    }
  },
};
