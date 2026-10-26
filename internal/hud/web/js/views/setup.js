// Farm setup: the farm's own tool tests (<tool>-farmtest-<hex>), which a
// farm runs when it publishes a tool and before it offers it. They are the
// farm's business, not anyone's work: app.js keeps them out of every job
// list and count (snap.checks), and this one quiet section says how they
// went. A failed one is the tool's to fix, in the tool's own words, with
// the command that shows it; never a red item, never Needs you.

import { h } from '../dom.js';
import * as f from '../fmt.js';

// why: the tool's own words for a failed test, from the farm's message
// ("1 unit is out of retries (0): <the tool's words>. Open them, …")
function why(j) {
  const m = ((j.needsPerson || {}).message || '').replace(/\s*Open them, requeue, assign or accept\.?$/, '');
  // the farm's own sentence ("1 unit failed after its retries; fix the tool
  // X, then c4 farm tool publish again …") carries no words of the tool's:
  // the line says it failed, and the command shows why
  if (/failed after (its|their) retries/.test(m)) return '';
  const i = m.indexOf(': ');
  // (the tool's own label, "the transcode tool's test: …", is said already)
  return (i >= 0 ? m.slice(i + 2) : m).replace(/^(the )?[\w-]+( tool['’]s)? test( failed)?:\s*/i, '').replace(/\.$/, '');
}

export function farmSetup(ctx) {
  const cs = (ctx.snap && ctx.snap.checks) || [];
  if (!cs.length) return null;
  const failed = cs.filter((j) => j.phase === 'Failed');
  const passed = cs.filter((j) => j.phase === 'Succeeded');
  const testing = cs.length - failed.length - passed.length;
  const words = [
    passed.length ? `${f.count(passed.length, 'tool')} ready` : '',
    testing ? `${f.count(testing, 'tool')} testing` : '',
    failed.length ? `${f.num(failed.length)} failed ${failed.length === 1 ? 'its test' : 'their tests'}` : '',
  ].filter(Boolean).join(' · ');
  const shown = ctx.route && ctx.route.q && ctx.route.q.checks === '1';
  return h('div', { class: 'setup', 'data-key': 'setup' },
    h('div', { class: 'setup-h' }, h('span', { class: 'lab' }, 'Farm setup'),
      h('span', null, `The farm tests each tool before it offers it: ${words}.`),
      ctx.level >= 2 ? h('a', { href: ctx.href(shown ? 'queue' : 'queue?checks=1') }, shown ? 'hide the tests' : 'show the tests') : null),
    failed.map((j) => h('div', { class: 'setup-f', 'data-key': 'setup-' + j.name },
      h('span', null, `The farm’s test of ${j.toolTest} failed${why(j) ? `: ${why(j)}` : ''}. Until it passes, ${j.toolTest} is not offered.`),
      h('code', null, `c4 farm tool show ${j.toolTest}`))));
}
