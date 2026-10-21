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
  if (st.at !== ctx.snap.now) {
    st.at = ctx.snap.now;
    get('/observed').then((d) => { st.data = d; st.err = null; ctx.refresh(); }, (e) => { st.data = null; st.err = e.message; ctx.refresh(); });
  }
  return st.data;
}

const tokens = (n) => (n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${Math.round(n / 1e3)}k` : String(n));
// used: the tokens a session has used since its process started, every model
function used(s) {
  const ms = (s.usage && s.usage.models) || {};
  const n = Object.values(ms).reduce((a, m) => a + (m.input || 0) + (m.output || 0), 0);
  return n ? `${tokens(n)} tokens` : '';
}
// the HUD's state for c4-agent observe's word
const STATE = { 'needs you': 'waiting', working: 'working', idle: 'idle', done: 'done', stopped: 'stopped', 'at its limit': 'limited' };
const stateOf = (s) => STATE[s.word] || s.word;
const ago = (iso) => f.dur(Math.max(0, (f.now() - f.ms(iso)) / 1000));
const where = (s) => String(s.cwd || '').replace(/^.*\//, '');

// word: a session's state in the vocabulary, its class, and the line under it
function word(s) {
  switch (stateOf(s)) {
    case 'waiting': return ['needs you', 't-risk', `asked ${ago(s.lastActive)} ago`];
    case 'limited': return ['at its limit', 't-temp', s.limit ? `${s.limit.text || 'a limit'}; resets ${f.when(s.limit.resets)}` : 'resets later'];
    case 'working': return ['working', 't-run', `for ${ago(s.startedAt)}`];
    case 'done': return ['done', 't-done', `its task ended ${ago(s.lastActive)} ago`];
    case 'stopped': return ['stopped', '', `stopped ${ago(s.lastActive)} ago; it resumes where it left off`];
    default: return ['at its prompt', '', `idle ${ago(s.lastActive)}`];
  }
}

function row(ctx, s) {
  const [w, cls, line] = word(s);
  const open = st.open.has(s.name);
  return h('div', { class: 'd1-job ses', 'data-key': 'ses-' + s.name },
    h('div', null, h('div', { class: 'nm' }, s.name), h('div', { class: 'st ' + cls }, w),
      h('div', { class: 'see' }, `${s.kind} · ${where(s)}`)),
    h('div', null,
      s.needsYou ? h('div', { class: 'ses-ask' }, `“${s.needsYou}”`) : null,
      h('div', { class: 'd1-pct' }, [line, used(s)].filter(Boolean).join(' · '))),
    h('div', { class: 'ses-act' },
      s.onFarm ? h('span', { class: 'dim' }, 'on the farm')
        : [h('button', { type: 'button', class: 'btn small', 'data-adopt': s.name, 'aria-expanded': open ? 'true' : 'false' }, 'Run it on the farm…'),
          open ? h('div', { class: 'ses-cmd' }, h('code', null, `c4 farm agents add ${s.name}`), h('span', { class: 'dim' }, 'Run this when you want it. The farm then runs it, yields it for deadlines, and asks you here.')) : null]));
}

export default {
  render(ctx) {
    const d = observed(ctx);
    if (!d) {
      return h('section', { class: 'd1', 'data-key': 'sessions' }, h('h1', null, 'Your sessions'),
        h('div', { class: 'd1-quiet' }, st.err ? 'Nothing is watching the sessions on this machine. Run c4 farm agents to see them here.' : 'Reading…'));
    }
    const ss = d.sessions || [];
    const waiting = ss.filter((s) => stateOf(s) === 'waiting');
    const limited = ss.filter((s) => stateOf(s) === 'limited');
    const working = ss.filter((s) => stateOf(s) === 'working');
    const rest = ss.filter((s) => !['waiting', 'limited', 'working'].includes(stateOf(s)));
    return h('section', { class: 'd1 sessions', 'data-key': 'sessions' },
      h('h1', null, 'Your sessions'),
      h('p', { class: 'd1-sub' }, `${f.num(ss.length)} Claude Code ${ss.length === 1 ? 'session' : 'sessions'} on ${d.machine || 'this machine'} · ${f.num(working.length)} working · read only: nothing is moved or stopped`),
      h('h2', null, waiting.length ? `Needs you · ${waiting.length}` : 'Needs you'),
      waiting.length ? waiting.map((s) => row(ctx, s)) : h('div', { class: 'd1-quiet' }, 'Nothing needs you. Your sessions are working or at rest.'),
      limited.length ? [h('h2', null, `Hit a limit · ${limited.length}`), limited.map((s) => row(ctx, s))] : null,
      working.length ? [h('h2', null, `Working · ${working.length}`), working.map((s) => row(ctx, s))] : null,
      rest.length ? [h('h2', null, `At rest · ${rest.length}`), rest.map((s) => row(ctx, s))] : null,
      h('div', { class: 'd1-foot' }, 'Read with claude agents --json and the sessions’ own transcripts. Answer a session that needs you in Claude Code, where it runs.'));
  },
};
