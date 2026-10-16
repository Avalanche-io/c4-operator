// The job view: the same for every class. One box per unit, grouped by
// pass, stage or rank, with slack and deadline risk on the same screen, the
// class's lens around it, and the selected unit's detail.

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get } from '../api.js';
import { JobGrid } from '../grid.js';
import { lensFor } from '../lenses/index.js';
import { lane, classChip, phaseText, orderText, burstButton, failedUnits } from './common.js';

const st = {
  key: null, detail: null, err: null, lastNow: null, fetching: false,
  sel: -1, unit: null, unitFor: null, unitNow: null,
  unit0: null, unit0Now: null, focus: null, grid: null, lens: null,
};

document.addEventListener('hud:changed', () => { st.lastNow = null; st.unitNow = null; });

function reset(k) {
  if (st.grid) st.grid.destroy();
  Object.assign(st, { key: k, detail: null, err: null, lastNow: null, sel: -1, unit: null, unitFor: null, unitNow: null, unit0: null, unit0Now: null, focus: null, grid: null });
}

function ensure(ctx, k, ns, name, lens) {
  const now = ctx.snap.now;
  if (!st.fetching && st.lastNow !== now) {
    st.lastNow = now;
    st.fetching = true;
    get(`/jobs/${ns}/${name}`)
      .then((d) => { st.detail = d; st.err = null; })
      .catch((e) => { st.err = e.message; })
      .finally(() => { st.fetching = false; if (st.key === k) ctx.refresh(); });
  }
  if (st.sel >= 0 && (st.unitFor !== st.sel || st.unitNow !== now)) {
    const want = st.sel;
    const stale = st.unitFor !== want;
    st.unitFor = want;
    st.unitNow = now;
    if (stale) st.unit = null;
    get(`/jobs/${ns}/${name}/units/${want}`)
      .then((u) => { if (st.sel === want) { st.unit = u; ctx.refresh(); } })
      .catch((e) => { st.unit = { error: e.message }; ctx.refresh(); });
  }
  if (lens.needs && lens.needs().unit0 && st.unit0Now !== now) {
    st.unit0Now = now;
    get(`/jobs/${ns}/${name}/units/0`).then((u) => { st.unit0 = u; ctx.refresh(); }, () => {});
  }
}

function cell(label, val, sub, cls = '') {
  return h('div', { class: 'cell' }, h('span', { class: 'lab' }, label), h('span', { class: 'val ' + cls }, val), sub ? h('span', { class: 'sub' }, sub) : null);
}

function controls(j, k) {
  const held = j.nudge.hold;
  const dl = j.objective.deadline ? j.objective.deadline.slice(0, 16) : '';
  const boosts = [1, 1.25, 1.5, 2];
  return h('div', { class: 'controls', role: 'group', 'aria-label': 'Job controls' },
    held ? h('button', { class: 'btn', 'data-action': 'release', 'data-job': k }, 'Release job')
      : h('button', { class: 'btn', 'data-action': 'hold', 'data-job': k, title: 'Release no new units; running units finish' }, 'Hold job'),
    !held && j.units.running + j.units.retrying > 0 ? h('button', { class: 'btn', 'data-action': 'holdstop', 'data-job': k, title: 'Hold, and stop the units that are running now' }, 'Hold and stop running units') : null,
    h('span', { class: 'lab' }, 'Boost'),
    h('span', { class: 'seg' }, boosts.map((b) => h('button', {
      class: 'btn small', 'data-action': 'boost', 'data-job': k, 'data-value': String(b), 'aria-pressed': Math.abs(j.nudge.boost - b) < 0.01 ? 'true' : 'false',
      title: b === 1 ? 'No boost' : `Ask for ${b}× the slots the deadline needs, from this project's quota first`,
    }, `×${b}`))),
    h('label', { class: 'lab', for: 'deadline-input' }, 'Deadline'),
    h('input', { type: 'datetime-local', id: 'deadline-input', value: dl, 'data-key': 'dl-' + k }),
    h('button', { class: 'btn', 'data-action': 'deadline', 'data-job': k, 'data-input': 'deadline-input' }, 'Set deadline'),
    j.units.failed ? h('button', { class: 'btn', 'data-action': 'requeue', 'data-job': k, 'data-units': failedUnits(j) }, `Requeue failed units (${j.units.failed})`) : null);
}

function warning(j) {
  const e = j.estimate;
  if (!e.message) return null;
  const tone = j.phase === 'Held' ? ' mag' : j.phase === 'Failed' ? ' red' : '';
  return h('div', { class: 'jwarn' + tone, role: 'status' }, h('span', null, e.message), burstButton(j));
}

function legend(j) {
  const items = [
    ['lg-uneval', 'not yet evaluated; may expand'], ['lg-queued', 'waiting'], ['lg-blocked', 'blocked by a dependency'],
    ['lg-running', 'running, measured'], ['lg-est', 'running, estimated'], ['lg-done', 'done'], ['lg-retry', 'retrying'],
    ['lg-failed', 'failed'], ['lg-held', 'held'], ['lg-risk', 'projected after the deadline'], ['lg-sel', 'selected'],
  ];
  return h('div', { class: 'legend', 'aria-label': 'Legend' }, items.map(([c, t]) => h('span', null, h('i', { class: c }), t)));
}

// provenanceBlock shows the unit's signed task record: who signed it,
// whether it verifies against the farm's keys, where it ran and on what.
function provenanceBlock(p) {
  const imgs = (p.images || []).map((i) => `${i.image}${i.digest ? ' @' + i.digest.slice(0, 19) : ''}`);
  const ins = (p.inputs || []).map((i) => `${i.materialization} ${f.shortId(i.view)}`);
  return h('div', { class: 'prov', 'data-key': 'prov' },
    h('div', { class: 'lab', style: { marginTop: '10px' } }, 'Task record'),
    h('dl', { class: 'kv' },
      h('dt', null, 'Signature'), h('dd', null,
        p.verified ? h('span', { class: 'chip green' }, 'verified') : h('span', { class: 'chip red' }, 'does not verify'),
        h('span', { class: 'mono dim', style: { marginLeft: '6px' } }, p.signer || p.problem || '')),
      h('dt', null, 'Record'), h('dd', { class: 'mono', title: p.record }, f.shortId(p.record)),
      h('dt', null, 'Ran on'), h('dd', { class: 'mono' }, p.node || '–'),
      h('dt', null, 'Images'), h('dd', { class: 'mono', style: { fontSize: '11.5px' } }, imgs.join(', ') || '–'),
      h('dt', null, 'Inputs'), h('dd', { class: 'mono', style: { fontSize: '11.5px' } }, ins.join(', ') || '–'),
      p.ai ? [h('dt', null, 'AI use'), h('dd', null, `${p.ai.generated || 'declared'}${p.ai.model ? ' · ' + p.ai.model : ''}`)] : null));
}

function unitPanel(ctx, j, k) {
  if (st.sel < 0) {
    return h('div', { class: 'panel', 'data-key': 'unit-hint' },
      h('header', null, h('h2', null, 'Unit')),
      h('p', { class: 'dim', style: { margin: 0 } }, 'Select a box to see its attempts, node, duration against its estimate, log tail, events and output. In the grid, arrow keys move and Enter opens.'));
  }
  const u = st.unit;
  if (!u) return h('div', { class: 'unit', 'data-key': 'unit' }, h('p', { class: 'dim' }, 'Loading unit…'));
  if (u.error) return h('div', { class: 'unit', 'data-key': 'unit' }, h('p', { class: 't-red' }, u.error));
  const last = u.attempts[u.attempts.length - 1];
  const node = last && last.node;
  const blk = (j.blacklist || []).includes(node);
  const durFrac = u.durationSeconds && u.estimateSeconds ? u.durationSeconds / Math.max(u.estimateSeconds, u.durationSeconds) : 0;
  const estFrac = u.durationSeconds && u.estimateSeconds ? u.estimateSeconds / Math.max(u.estimateSeconds, u.durationSeconds) : 1;
  const stateChip = { failed: 'red', retrying: 'red', held: 'mag', succeeded: 'green' }[u.state] || '';
  return h('div', { class: 'unit', 'data-key': 'unit', role: 'region', 'aria-label': `Unit ${u.label}` },
    h('h3', null, h('span', { class: 'mono' }, u.label), h('span', { class: 'chip ' + stateChip }, u.state)),
    h('dl', { class: 'kv' },
      h('dt', null, 'Node'), h('dd', { class: 'mono' }, node || '–', blk ? h('span', { class: 'chip flag', style: { marginLeft: '6px' } }, 'blacklisted') : null),
      h('dt', null, 'Attempts'), h('dd', { class: 'num' }, String(u.attempts.length)),
      h('dt', null, 'Duration'), h('dd', null,
        h('span', { class: 'num' }, u.durationSeconds !== undefined ? `${f.dur(u.durationSeconds)} of an estimated ${f.dur(u.estimateSeconds)}` : `estimated ${f.dur(u.estimateSeconds)}`),
        u.durationSeconds !== undefined ? h('div', { class: 'durbar', 'aria-hidden': 'true' }, h('i', { style: { width: `${durFrac * 100}%` } }), h('b', { style: { left: `${estFrac * 100}%` } })) : null),
      u.outputChangeset ? [h('dt', null, 'Output'), h('dd', { class: 'mono', title: u.outputChangeset }, f.shortId(u.outputChangeset))] : null,
      u.expandedInto ? [h('dt', null, 'Expanded into'), h('dd', { class: 'mono' }, `units ${u.expandedInto}`)] : null),
    u.attempts.length ? h('table', { class: 't' },
      h('thead', null, h('tr', null, h('th', null, '#'), h('th', null, 'Node'), h('th', null, 'Start'), h('th', null, 'End'), h('th', null, 'Exit'))),
      h('tbody', null, u.attempts.map((a) => [
        h('tr', { 'data-key': 'a' + a.n },
          h('td', { class: 'num' }, String(a.n)), h('td', { class: 'mono' }, a.node),
          h('td', { class: 'num' }, f.clockSec(a.start)), h('td', { class: 'num' }, a.end ? f.clockSec(a.end) : 'running'),
          h('td', { class: 'num ' + (a.exitCode ? 't-red' : '') }, a.exitCode !== undefined ? String(a.exitCode) : '')),
        a.reason ? h('tr', { 'data-key': 'r' + a.n }, h('td'), h('td', { colspan: 4, class: a.exitCode ? 't-red' : 'dim', style: { fontSize: '11.5px' } }, a.reason)) : null,
      ]))) : null,
    u.provenance ? provenanceBlock(u.provenance) : null,
    h('div', { class: 'lab', style: { marginTop: '10px' } }, 'Log tail'),
    h('pre', { class: 'log' }, u.logTail.join('\n') || '(no output yet)'),
    h('div', { class: 'lab' }, 'Events'),
    h('table', { class: 't', style: { marginBottom: '10px' } }, h('tbody', null, u.events.slice(-6).map((e, i) => h('tr', { 'data-key': 'e' + i },
      h('td', { class: 'num dim', style: { width: '64px' } }, f.clockSec(e.at)),
      h('td', { class: e.type === 'Warning' ? 't-red' : '' }, e.reason),
      h('td', { class: 'dim', style: { fontSize: '11.5px' } }, e.message))))),
    h('div', { class: 'controls', style: { justifyContent: 'flex-start' } },
      ['failed', 'succeeded'].includes(u.state) ? h('button', { class: 'btn', 'data-action': 'requeue', 'data-job': k, 'data-units': String(u.i) }, u.state === 'failed' ? 'Requeue unit' : 'Render again') : null,
      node && !blk ? h('button', { class: 'btn', 'data-action': 'blacklist', 'data-job': k, 'data-node': node }, `Blacklist ${node} for this job`) : null,
      h('a', { class: 'btn', href: ctx.href(`job/${j.namespace}/${j.name}`) }, 'Close')));
}

export default {
  render(ctx) {
    const [ns, name, sub, idx] = ctx.route.parts;
    const k = `${ns}/${name}`;
    if (st.key !== k) reset(k);
    st.sel = sub === 'unit' ? Number(idx) : -1;
    const summary = ctx.job(k);
    const cls = summary && ctx.cls(summary.class);
    const lens = lensFor(cls ? cls.lens : 'frames');
    st.lens = lens;
    ensure(ctx, k, ns, name, lens);
    st.pickFrame = (frame) => {
      const d = st.detail;
      if (!d || !d.unitList) return;
      const last = (d.classSummary.passes || []).slice(-1)[0];
      const u = d.unitList.find((x) => x.group === last && x.label.startsWith(String(frame)));
      if (u) location.replace(ctx.href(`job/${ns}/${name}/unit/${u.i}`));
    };
    if (!summary && !st.detail) {
      return h('div', null, h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Queue')),
        h('h1', null, name || 'Job'), h('p', { class: 'dim' }, st.err ? `No job named ${name} in your projects.` : 'Loading…'));
    }
    // The summary from the stream is newest; the detail adds the unit table.
    const j = st.detail ? { ...st.detail, ...(summary || {}) } : summary;
    if (st.detail && summary) j.unitStates = st.detail.unitStates;
    const e = j.estimate;
    const u = j.units;
    const main = st.detail ? lens.main(ctx, j, st) : [];
    const side = st.detail ? lens.side(ctx, j, st) : [];
    const wideLens = lens !== lensFor('render') && lens !== lensFor('frames');
    const obj = j.objective;
    const dlSub = obj.from ? `inherited from ${f.jobName(obj.from)}` : obj.type === 'latency' ? `ready + ${f.dur(obj.latencyTargetSeconds)} target` : 'set on the job';
    const groups = st.detail ? st.detail.groups : [];
    const focusLabel = st.focus && groups.find((g) => g.key === st.focus.group);
    return h('article', { 'data-key': 'job-' + k },
      h('div', { class: 'crumbs' }, h('a', { href: ctx.href('queue') }, 'Queue'), ' / ', j.project, ' / ', j.namespace),
      h('div', { class: 'jhead' },
        h('div', null,
          h('h1', null, j.name),
          h('div', { class: 'jmeta' }, classChip(j, cls), phaseText(j), h('span', null, j.rank > 0 ? `#${j.rank} in dispatch order` : 'not in the queue'),
            h('span', null, orderText(j.order)), h('span', { class: 'num' }, j.pools.join(' + ')),
            j.owner ? h('span', null, `owner ${j.owner}`) : null,
            h('span', { class: 'dimmer' }, `created ${f.clock(j.createdAt)}`),
            (j.dependsOn || []).length ? h('span', null, 'after ', j.dependsOn.map((d, i) => [i ? ', ' : '', h('a', { href: ctx.href(`job/${d}`) }, f.jobName(d))])) : null)),
        controls(j, k)),
      h('div', { class: 'jslate' },
        cell('Deadline', obj.deadline ? f.clock(obj.deadline) : 'none', obj.deadline ? dlSub : 'no objective'),
        cell('ETA, p50 to p90', j.phase === 'Succeeded' ? f.clock(e.etaP90) : `${f.clock(e.etaP50, false)} – ${f.clock(e.etaP90, false)}`,
          u.unevaluated ? `${u.unevaluated} generators still to expand; the band narrows as they do` : `band ${f.dur((f.ms(e.etaP90) - f.ms(e.etaP50)) / 1000)} wide`),
        cell('Slack', obj.deadline && j.phase !== 'Succeeded' ? f.signed(e.slackSeconds) : '–', e.feasible ? 'feasible' : 'will miss at current capacity', e.slackSeconds < 0 && j.phase !== 'Succeeded' ? 't-yellow' : ''),
        cell('Done', `${f.num(u.succeeded)}`, `of ${f.num(u.total)} units` + (u.failed ? `, ${u.failed} failed` : '') + (u.retrying ? `, ${u.retrying} retrying` : '')),
        cell('Slots', `${e.runningSlots}`, `running; deadline needs ${e.requiredSlots}, can get ${e.attainableSlots}`),
        j.output ? cell('Output view', f.shortId(j.output.view), `${f.num(j.output.changesets)} changesets folded in`, 'mono-sm') : null),
      h('div', { class: 'jlane' }, lane(j, { tall: true })),
      warning(j),
      (j.blacklist || []).length ? h('div', { class: 'controls', style: { justifyContent: 'flex-start', marginTop: '8px' } },
        h('span', { class: 'lab' }, 'Blacklisted for this job'),
        j.blacklist.map((n) => h('span', { class: 'chip flag', 'data-key': 'bl-' + n }, n)),
        j.blacklist.map((n) => h('button', { class: 'btn small', 'data-action': 'unblacklist', 'data-job': k, 'data-node': n }, `Allow ${n} again`))) : null,
      h('div', { class: 'jbody' },
        h('div', null,
          h('div', { class: 'panel', 'data-key': 'gridpanel' },
            h('header', null, h('h2', null, 'Units'),
              h('span', { class: 'dim' }, st.detail ? gridCaption(st.detail, u.total) : '')),
            focusLabel ? h('div', { class: 'focusbar' }, h('span', null, `Showing ${focusLabel.label} only.`),
              h('button', { class: 'btn small', 'data-focus-clear': '1' }, 'Show all groups')) : null,
            h('div', { class: 'well' },
              h('canvas', { class: 'grid', 'data-key': 'grid-' + k, tabindex: '0', role: 'img', 'aria-label': `Job view of ${j.name}. ${u.succeeded} of ${u.total} units done.` }),
              h('div', { class: 'tip', hidden: true, 'data-gridtip': '1', 'data-own': '1' })),
            legend(j)),
          wideLens ? null : main),
        h('div', null, side, unitPanel(ctx, j, k))),
      wideLens ? h('div', { style: { marginTop: '8px' } }, main) : null);
  },

  after(ctx, root) {
    const cv = root.querySelector('canvas.grid');
    if (!cv || !st.detail) return;
    if (!st.grid || st.grid.cv !== cv) {
      if (st.grid) st.grid.destroy();
      const [ns, name] = ctx.route.parts;
      st.grid = new JobGrid(cv, {
        tip: root.querySelector('[data-gridtip]'),
        onSelect: (i) => location.replace(ctx.href(i < 0 ? `job/${ns}/${name}` : `job/${ns}/${name}/unit/${i}`)),
        onFocus: (fo) => { st.focus = fo; ctx.refresh(); },
      });
      const clear = root.querySelector('[data-focus-clear]');
      void clear;
    }
    st.grid.set(st.detail, { selected: st.sel, focus: st.focus });
    const fc = root.querySelector('[data-focus-clear]');
    if (fc && !fc.dataset.wired) {
      fc.dataset.wired = '1';
      fc.addEventListener('click', () => { st.focus = null; ctx.refresh(); });
    }
    if (st.lens && st.lens.after) st.lens.after(ctx, root, { ...st.detail, ...(ctx.job(st.key) || {}), unitStates: st.detail.unitStates }, st);
  },

  leave() {
    if (st.lens && st.lens.leave) st.lens.leave();
  },
};

function gridCaption(d, total) {
  const kind = d.axis && d.axis.kind;
  const groups = d.groups.filter((g) => !g.generator).map((g) => g.label);
  if (st.focus) return 'Drilled down to one group. Select a box for its detail.';
  if (total > 20000) return `${f.num(total)} units. Each cell stacks the states of a run of frames; select a row to show only that ${groups.length > 1 ? 'group' : 'range'}.`;
  if (total > 2000) return `${f.num(total)} units as compact cells. Select a pass name to show only that pass, box by box.`;
  if (kind === 'frame') {
    const gen = d.groups.find((g) => g.generator);
    return gen ? `Each column is a frame, 24 to a row (one second). A light gray frame has not expanded yet; it splits into ${groups.join(', ')}.` :
      `24 frames to a row, one second of screen time${groups.length > 1 ? ', grouped by pass' : ''}.`;
  }
  if (kind === 'rank') return 'One row per rank. The ranks start and stop together; the fill is training progress in steps.';
  if (kind === 'shard') return 'Rows by stage. Test discovery is a generator that expands into the shards this change needs.';
  return `Rows by ${groups.join(', ')}.`;
}
