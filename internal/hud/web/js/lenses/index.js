// Lens registry. A lens is registered by name (a WorkClass names its lens)
// and draws class-specific panels around the job view. Adding a class's
// lens does not change the HUD shell.
//
// A lens may provide:
//   main(ctx, job, st)  panels under the job view
//   side(ctx, job, st)  panels in the side column
//   after(ctx, root, job, st)  wiring after the DOM is in place
//   needs(job)  extra data to fetch: { unit0: true }

import { costStrip, wireCostStrip, updateCostStrip, Player, playerPanel } from './render.js';
import { ciLens } from './ci.js';
import { mlLens } from './ml.js';
import { agentLens } from './agent.js';
import { planLens } from './plan.js';

const lenses = new Map();
export function register(name, lens) { lenses.set(name, lens); }
export function lensFor(name) { return lenses.get(name) || lenses.get('frames'); }

const player = new Player();

register('render', {
  wideGrid: false,
  main: (ctx, job) => [costStrip(job)],
  side: (ctx, job) => [playerPanel(player, job)],
  after(ctx, root, job, st) {
    wireCostStrip(root, job, (frame) => st.pickFrame(frame));
    updateCostStrip(root, job);
    player.load(job);
    player.attach(root);
  },
  leave: () => player.stop(),
});

register('frames', {
  main: (ctx, job) => [job.axis && job.axis.kind === 'frame' ? costStrip(job) : null],
  side: () => [],
  after(ctx, root, job, st) {
    wireCostStrip(root, job, (frame) => st.pickFrame(frame));
    updateCostStrip(root, job);
  },
});

register('ci', {
  main: (ctx, job) => ciLens(ctx, job),
  side: () => [],
});

register('ml', {
  needs: () => ({ unit0: true }),
  main: (ctx, job, st) => mlLens(ctx, job, st.unit0),
  side: () => [],
});

register('agent', {
  main: (ctx, job) => agentLens(ctx, job),
  side: () => [],
});

register('plan', {
  main: (ctx, job) => planLens(ctx, job),
  side: () => [],
});
