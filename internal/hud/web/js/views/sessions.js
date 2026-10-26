// Your sessions (#/sessions): the Claude Code sessions already running on
// your machine, read only (PLAN 3a, 1a.9: the beta's first touch). Nothing
// is moved, stopped or signed in to; the HUD reads the rows `c4-agent
// observe` writes, as `c4 farm agents` or a farm's runner serves them
// (GET /api/v1/observed: {now, machine, sessions}). It answers, at level 1, the questions a
// person running several agents has: which one waits on me, which are
// working, which hit a limit, and what each has used. Moving one onto the
// farm stays one opt-in step, a command the person runs.
//
//   waits on you   a session that asked its person something
//   hit a limit    a usage limit, and when it resets
//   working        at work, and since when
//   at rest        an interactive session at its prompt, a background task
//                  that ended, or a stopped background session

import { h } from '../dom.js';
import * as f from '../fmt.js';
import { get } from '../api.js';

const st = { at: null, data: null, err: null, open: new Set() };
document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-adopt]');
  if (!b) return;
  if (st.open.has(b.dataset.adopt)) st.open.delete(b.dataset.adopt); else st.open.add(b.dataset.adopt);
  document.dispatchEvent(new CustomEvent('hud:refresh'));
});

export { stateOf };

export function observed(ctx) {
  // a farm with nothing observing answers once, and is not asked again
  if (st.none) return null;
  if (st.at !== ctx.snap.now) {
    st.at = ctx.snap.now;
    get('/observed').then((d) => { st.data = d; st.err = null; ctx.refresh(); }, (e) => { st.data = null; st.err = e.message; st.none = true; ctx.refresh(); });
  }
  return st.data;
}


// tokens: two figures at most, so a column of them reads at a glance
const tokens = (n) => {
  const [d, u] = n >= 1e9 ? [1e9, 'B'] : n >= 1e6 ? [1e6, 'M'] : n >= 1e3 ? [1e3, 'k'] : [1, ''];
  const x = n / d;
  return `${x < 10 && d > 1 ? x.toFixed(1) : Math.round(x)}${u}`;
};
// cacheReadWeight: a model's cache-read price against its base input, as
// the operator weighs it (work/tokens): 0.025 on Fable 5.1 and Mythos 5.1,
// 0.05 on Opus 5.5, else 0.1
function cacheReadWeight(model) {
  const m = String(model || '').toLowerCase();
  if (m.startsWith('claude-fable-5-1') || m.startsWith('claude-mythos-5-1')) return 0.025;
  if (m.startsWith('claude-opus-5-5')) return 0.05;
  return 0.1;
}
// usage: what a session has used since its process started, in the farm's
// weighted tokens, the count `c4 farm agents` shows: input + 5 × output +
// 1.25 × cache writes + the model's cache-read weight × cache reads. The
// raw classes are on hover.
function usage(s) {
  const list = ((s.usage && s.usage.models) || []).filter((m) => m.model && !m.model.startsWith('<'));
  let used = 0, wrote = 0, read = 0;
  for (const m of list) {
    const inp = m.inputTokens || 0, out = m.outputTokens || 0, cr = m.cacheReadTokens || 0, cw = m.cacheWriteTokens || 0;
    used += inp + 5 * out + 1.25 * cw + cacheReadWeight(m.model) * cr;
    wrote += out;
    read += inp + cr + cw;
  }
  // the farm's own figure when observe sends it, so the CLI and the HUD agree
  if (Number.isFinite(s.weighted)) used = s.weighted;
  if (!used) return null;
  const models = list.filter((m) => (m.inputTokens || 0) + (m.outputTokens || 0) > 0).map((m) => m.model);
  return { text: `used ${tokens(used)}`, title: `${tokens(used)} weighted tokens since it started: ${tokens(wrote)} written, ${tokens(read)} read (cache reads included)${models.length ? ` · ${models.join(', ')}` : ''}` };
}
// the HUD's state for c4-agent observe's word
const STATE = { 'needs you': 'waiting', working: 'working', idle: 'idle', done: 'done', stopped: 'stopped', 'at its limit': 'limited', failed: 'failed' };
const stateOf = (s) => STATE[s.word] || s.word;
const ago = (iso) => f.dur(Math.max(0, (f.now() - f.ms(iso)) / 1000));
const where = (s) => String(s.cwd || '').replace(/^.*\//, '');
// a failure is news for a day; a session that ended a day ago or more is
// history, folded into one line, as `c4 farm agents` folds it
const FRESH = 24 * 3600e3;
const lastAt = (s) => f.ms(s.lastActive || s.startedAt);
const recent = (s) => !!(s.lastActive || s.startedAt) && f.now() - lastAt(s) < FRESH;
const ended = (s) => ['failed', 'done', 'stopped'].includes(stateOf(s)) || s.running === false;

// word: a session's state in the vocabulary, its class, and the line beside it
function word(s) {
  const last = s.lastActive ? ago(s.lastActive) : null;
  switch (stateOf(s)) {
    case 'waiting': return ['needs you', 't-risk', last ? `asked ${last} ago` : ''];
    case 'limited': return ['at its limit', 't-temp', s.limit ? `${s.limit.text || 'a limit'}; resets ${f.when(s.limit.resets)}` : 'resets later'];
    case 'working': return ['working', 't-run', [last ? `last wrote ${last} ago` : '', s.backgroundOpen ? `${s.backgroundOpen} background ${s.backgroundOpen === 1 ? 'task' : 'tasks'}` : ''].filter(Boolean).join(' · ')];
    case 'done': return ['done', 't-done', last ? `its task ended ${last} ago` : ''];
    case 'failed': return recent(s) ? ['stopped on an error', 't-fail', last ? `${last} ago` : ''] : ['stopped on an error', '', last ? `${last} ago` : ''];
    case 'stopped': return ['stopped', '', last ? `${last} ago; it resumes where it left off` : ''];
    default: return ['at its prompt', '', last ? `idle ${last}` : 'no turns yet'];
  }
}

// card: a session that may need you, in full: what it asked, or its error
function card(ctx, s) {
  const [w, cls, line] = word(s);
  const u = usage(s);
  return h('div', { class: 'd1-job ses', 'data-key': 'ses-' + s.session },
    h('div', null, h('div', { class: 'nm' }, s.name), h('div', { class: 'st ' + cls }, w),
      h('div', { class: 'see' }, `${s.kind} · ${where(s)}${s.machine ? ` · on ${s.machine}` : ''}`)),
    h('div', null,
      s.needsYou ? h('div', { class: 'ses-ask' }, `“${s.needsYou}”`) : null,
      h('div', { class: 'd1-pct', title: u ? u.title : null }, [line, u && u.text].filter(Boolean).join(' · '))),
    h('div', { class: 'ses-act' }, s.onFarm ? h('span', { class: 'dim' }, 'on the farm') : null));
}

// workers: the rows a session started (LEAD.md stage 1), by parent
let KIDS = new Map();
const kidsOf = (s) => KIDS.get(s.session) || [];
// crew: a lead's workers in words: "4 workers, 1 waits on it"
function crew(s) {
  const ks = kidsOf(s);
  if (!ks.length && !s.workersDone) return '';
  const live = ks.filter((k) => !ended(k));
  const asks = ks.filter((k) => stateOf(k) === 'waiting' && k.waitsOn !== 'person');
  // workersDone: finished workers older than an hour, counted, not listed
  const done = ks.length - live.length + (s.workersDone || 0);
  // none at work: say what is true, "11 workers done", not "0 workers"
  if (!live.length) return `${done} ${done === 1 ? 'worker' : 'workers'} done`;
  return [`${live.length} ${live.length === 1 ? 'worker' : 'workers'}`, asks.length ? `${asks.length} ${asks.length === 1 ? 'waits' : 'wait'} on it` : '',
    done ? `${done} done` : ''].filter(Boolean).join(', ');
}

// modelName: claude-opus-5-5 as Opus 5.5; anything else as it is
const modelName = (m) => {
  const x = /^claude-([a-z]+)-(\d+)-(\d+)/.exec(String(m || ''));
  return x ? `${x[1][0].toUpperCase()}${x[1].slice(1)} ${x[2]}.${x[3]}` : m || '';
};
// about: a worker's line to hover: what it was asked, its agent type and
// the model it was asked for
const about = (s) => [s.description, s.agentType ? `agent type: ${s.agentType}` : '', s.model ? `model: ${modelName(s.model)}` : ''].filter(Boolean).join('\n') || null;
// unseen: a lead that ran workflow agents, which observing does not read yet
const unseen = (s) => (s.workflowsNotShown ? h('div', { class: 'ses-note', 'data-key': 'wf-' + s.session }, 'It ran workflow agents; those are not shown here yet.') : null);

// line: a session that needs nothing, in one quiet line, with the workers
// it started nested under it
function line(ctx, s, depth = 0) {
  const [w, cls, rest] = word(s);
  const u = usage(s);
  const c = crew(s);
  const own = h('div', { class: 'ses-line' + (depth ? ' ses-child' : '') + (s.parentClaimed ? ' ses-guess' : ''), 'data-key': 'ses-' + s.session },
    h('span', { class: 'nm', title: about(s) }, s.name),
    h('span', { class: 'st ' + (stateOf(s) === 'waiting' && s.waitsOn === 'lead' ? '' : cls) }, stateOf(s) === 'waiting' && s.waitsOn === 'lead' ? 'waits on its lead' : w),
    h('span', { class: 'dt' }, [depth && s.description ? s.description : rest, c, s.onFarm ? 'on the farm' : ''].filter(Boolean).join(' · ')),
    h('span', { class: 'us', title: u ? u.title : null }, u ? u.text : ''),
    h('span', { class: 'wh' }, depth ? (s.parentClaimed ? `${s.kind} · started from it, by its command` : s.kind) : `${s.kind} · ${where(s)}${s.machine ? ` · ${s.machine}` : ''}`));
  const ks = kidsOf(s);
  // nothing nested unless a worker is at work: a lead whose workers all
  // finished is summed up on its own line, and opened from Your sessions
  if (!ks.some((k) => !ended(k)) && !st.open.has('kids:' + s.session)) return unseen(s) ? [own, unseen(s)] : own;
  return [own, unseen(s), ctx.level >= 2 ? boxes(ctx, s, ks) : nested(ctx, s, ks, depth)];
}

// nested: a lead's workers at level 1, one line each; finished ones and
// any past five fold into a line that opens them
function nested(ctx, s, ks, depth) {
  const live = ks.filter((k) => !ended(k)).sort((a, b) => (stateOf(a) === 'waiting') - (stateOf(b) === 'waiting') || lastAt(b) - lastAt(a));
  const done = ks.filter((k) => ended(k));
  const key = 'kids:' + s.session, open = st.open.has(key);
  const shown = open ? [...live, ...done] : live.slice(0, 5);
  const hidden = ks.length - shown.length;
  const older = s.workersDone || 0;
  return h('div', { class: 'ses-kids' },
    shown.map((k) => line(ctx, k, depth + 1)),
    hidden > 0 || open ? h('button', { type: 'button', class: 'ses-more', 'data-adopt': key, 'aria-expanded': open ? 'true' : 'false' },
      open ? 'fold the finished workers' : `${hidden} more: ${[live.length > 5 ? `${live.length - 5} working` : '', done.length ? `${done.length} done` : ''].filter(Boolean).join(', ')}`) : null,
    older ? h('div', { class: 'ses-more dim' }, `and ${older} finished more than an hour ago`) : null);
}

// boxes: a lead's workers at level 2, a box each: its description, its
// state, what it has used; finished ones folded to a count
function boxes(ctx, s, ks) {
  const live = ks.filter((k) => !ended(k));
  const done = ks.length - live.length + (s.workersDone || 0);
  return h('div', { class: 'ses-boxes', 'data-key': 'boxes-' + s.session },
    live.map((k) => {
      const [w, cls] = word(k);
      const u = usage(k);
      const lead = stateOf(k) === 'waiting' && k.waitsOn === 'lead';
      return h('div', { class: 'ses-box' + (lead ? ' asks-lead' : '') + (k.parentClaimed ? ' ses-guess' : ''), 'data-key': 'box-' + k.session, title: about(k) },
        h('div', { class: 'bn' }, k.name),
        h('div', { class: 'bd' }, k.description || k.kind),
        h('div', { class: 'bs ' + (lead ? '' : cls) }, lead ? 'waits on its lead' : w),
        k.needsYou ? h('div', { class: 'bq' }, `“${k.needsYou}”`) : null,
        h('div', { class: 'bu', title: u ? u.title : null }, [k.agentType || k.kind, modelName(k.model), u ? u.text : ''].filter(Boolean).join(' · ')));
    }),
    done ? h('div', { class: 'ses-box done' }, h('div', { class: 'bn' }, `${done} done`), h('div', { class: 'bd' }, 'finished workers')) : null);
}

// outsideQueue: the sessions on the machine that the queue does not run,
// as one group below a queue's run order (LEAD.md stage 1): each top-level
// session a line, a lead's workers nested; never ranked. Null when nothing
// observes the machine, or every session runs on the farm.
export function outsideQueue(ctx) {
  const d = observed(ctx);
  if (!d) return null;
  const me = (ctx.snap.viewer || {}).user;
  const ss = (d.sessions || []).filter((s) => (!s.owner || s.owner === me) && (!s.owners || !s.owners.length || s.owners.includes(me)));
  KIDS = byParent(ss);
  const tops = ss.filter((s) => !isChild(ss, s) && !s.onFarm && !(ended(s) && !recent(s)));
  if (!tops.length) return null;
  return h('div', { class: 'ses-outside', 'data-key': 'outside' },
    h('h2', null, `Outside the queue · ${tops.length}`),
    h('p', { class: 'd1-sub' }, 'Claude Code sessions on this machine the farm does not run: read, never moved.'),
    h('div', { class: 'ses-lines' }, tops.map((s) => line(ctx, s))),
    h('div', { class: 'd1-foot' }, h('a', { href: ctx.href('sessions') }, 'Your sessions'), ' has each in full.'));
}

// byParent: the rows a session started, keyed by the parent's session
function byParent(ss) {
  const m = new Map();
  for (const s of ss) {
    if (!s.parent || !ss.some((p) => p.session === s.parent)) continue;
    if (!m.has(s.parent)) m.set(s.parent, []);
    m.get(s.parent).push(s);
  }
  return m;
}
const isChild = (ss, s) => !!s.parent && ss.some((p) => p.session === s.parent);

export default {
  render(ctx) {
    const d = observed(ctx);
    if (!d) {
      return h('section', { class: 'd1', 'data-key': 'sessions' }, h('h1', null, 'Your sessions'),
        h('div', { class: 'd1-quiet' }, st.err ? 'Nothing is watching the sessions on this machine. Run c4 farm agents to see them here.' : 'Reading…'));
    }
    // only the viewer's own machines' sessions: the server sends no others
    // on a shared farm, and a row or report that names other owners is
    // never drawn
    const me = (ctx.snap.viewer || {}).user;
    const mine = (s) => (!s.owner || s.owner === me) && (!s.owners || !s.owners.length || s.owners.includes(me));
    const all = (d.owners && d.owners.length && !d.owners.includes(me)) ? [] : (d.sessions || []).filter(mine);
    KIDS = byParent(all);
    // the page groups top-level sessions; a lead's workers are drawn under
    // it, except one that waits on the person, which needs them too
    const ss = all.filter((s) => !isChild(all, s) || (stateOf(s) === 'waiting' && s.waitsOn === 'person'));
    const machines = [...new Set(ss.map((s) => s.machine).filter(Boolean))];
    const waiting = ss.filter((s) => stateOf(s) === 'waiting');
    const failed = ss.filter((s) => stateOf(s) === 'failed' && recent(s));
    const limited = ss.filter((s) => stateOf(s) === 'limited');
    const working = ss.filter((s) => stateOf(s) === 'working').sort((a, b) => f.ms(b.lastActive) - f.ms(a.lastActive));
    const others = ss.filter((s) => !waiting.includes(s) && !failed.includes(s) && !limited.includes(s) && !working.includes(s))
      .sort((a, b) => (lastAt(b) || 0) - (lastAt(a) || 0));
    const old = others.filter((s) => ended(s) && !recent(s));
    const rest = others.filter((s) => !old.includes(s));
    const off = ss.filter((s) => !s.onFarm);
    return h('section', { class: 'd1 sessions', 'data-key': 'sessions' },
      h('h1', null, 'Your sessions'),
      h('p', { class: 'd1-sub' }, `${f.num(ss.length)} Claude Code ${ss.length === 1 ? 'session' : 'sessions'} on ${machines.length > 1 ? `${machines.length} of your machines` : machines[0] || d.machine || 'this machine'} · ${f.num(working.length)} working · read only: nothing is moved or stopped`),
      h('h2', null, waiting.length ? `Needs you · ${waiting.length}` : 'Needs you'),
      waiting.length ? waiting.map((s) => card(ctx, s)) : h('div', { class: 'd1-quiet' }, 'Nothing needs you. Your sessions are working or at rest.'),
      failed.length ? [h('h2', null, `Stopped on an error today · ${failed.length}`), failed.map((s) => card(ctx, s))] : null,
      limited.length ? [h('h2', null, `Hit a limit · ${limited.length}`), limited.map((s) => card(ctx, s))] : null,
      working.length ? [h('h2', null, `Working · ${working.length}`), h('div', { class: 'ses-lines' }, working.map((s) => line(ctx, s)))] : null,
      rest.length ? [h('h2', null, `At rest · ${rest.length}`), h('div', { class: 'ses-lines' }, rest.map((s) => line(ctx, s)))] : null,
      old.length ? h('div', { class: 'ses-old' },
        h('button', { type: 'button', class: 'linkish', 'data-adopt': 'old', 'aria-expanded': st.open.has('old') ? 'true' : 'false' },
          `${old.length} older ${old.length === 1 ? 'session' : 'sessions'} ended ${old.length === 1 ? '' : 'from '}${ago(new Date(Math.max(...old.map(lastAt))).toISOString())} ago${old.length === 1 ? '' : ' back'}`),
        st.open.has('old') ? h('div', { class: 'ses-lines' }, old.map((s) => line(ctx, s))) : null) : null,
      off.length ? h('div', { class: 'ses-adopt' },
        h('button', { type: 'button', class: 'btn small', 'data-adopt': '*', 'aria-expanded': st.open.has('*') ? 'true' : 'false' }, 'Run one on the farm…'),
        st.open.has('*') ? h('div', { class: 'ses-cmd' }, h('code', null, 'c4 farm agents add NAME'),
          h('span', { class: 'dim' }, 'With the session’s name from the list. The farm then runs it, yields it for deadlines, and asks you here; the others stay as they are.')) : null) : null,
      h('div', { class: 'd1-foot' }, 'Read with claude agents --json and the sessions’ own transcripts. Answer a session that needs you in Claude Code, where it runs.'));
  },
};
