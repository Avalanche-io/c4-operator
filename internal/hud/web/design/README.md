# GRATICULE: the HUD's design system

The HUD shows a farm that names every file by its content (SMPTE ST 2114),
never overwrites a version, has every step signed by whoever did it, orders
work by deadline instead of priority, and lets agents and people use the same
controls. The design follows from that.

## The thesis

**Every screen is a sheet in a series** (SURVEY). A render farm is a
territory being made, frame by frame, and the HUD is its survey: it locates
rather than decorates. A neatline bounds the data; the collar outside it says
what, when and at what scale; the neighbouring sheets are named at the edges,
so the world is always larger than the sheet in front of you. A C4 ID is a
coordinate in content space, printed as a grid reference.

**Facts are ink, estimates are pencil** (LEDGER). What happened is measured
and never moves; what is expected is redrawn every second. Every action is an
entry in the book, signed by the node, the queue, the controller, an importer,
or the HUD on behalf of a person or an agent, and nothing is erased.

**The wall is a time base** (TIMEBASE). Every job is a channel sweeping toward
its deadline; history persists and fades; the work resolves coarse to fine,
in the binary order the operator renders it.

## Motifs

| motif                           | means                                                                                | data                                               |
| ------------------------------- | ------------------------------------------------------------------------------------ | -------------------------------------------------- |
| neatline and collar             | the sheet's bounds; the title block (deadline, ETA band, slack, done, slots, output) | `JobSummary`, `estimate`, `work`, `output`         |
| edge references                 | upstream jobs west, downstream east, sibling shots north and south                   | `/graph`, `dependsOn`, `shot`                      |
| containment by adjacency        | the shot spans its batches, a batch its frames, a frame's stages sit side by side    | `groups`, `unitList[].pos`, `parent`, `axis.batch` |
| collapse to aggregate           | a container becomes one block: green done, blue sliver running, orange and red ticks | `unitList[].work`, `JobSummary.work`               |
| fill from the bottom            | progress; striped when it is elapsed over estimate                                   | `progress[i].fraction`, `measured`                 |
| lines only where grouping fails | A: another job's input (default on); B: a unit reading other rows (per job, off)     | `/graph`, `unitList[].reads`                       |
| time lines                      | projected finish contours across the rows; the deadline as a red pencil datum        | `unitList[].eta`, `objective.deadline`             |
| ink and pencil                  | measured values solid; estimates lighter, dash-underlined                            | every number                                       |
| seals                           | who signed an entry; dashed until signed                                             | `/book`, task, decision and control records        |
| edge code                       | a job's output view and the changesets folded into its chain                         | `output.view`, `output.changesets`                 |
| the sweep                       | the wall's now line; persistence for what just happened                              | snapshots over time                                |

## Tokens

`tokens.css` is the single source: a 20-step grey ramp (`--g00` to `--g19`,
even in CIELAB lightness, packed densely below L*20 where layering lives),
the state colors, type (IBM Plex Sans Condensed, IBM Plex Mono), line
weights, spacing (a 4 px step; the collar and gutter) and motion
(`--fill-ms`, `--collapse-ms`, zero under reduced motion). The wall scale
(`[data-scale="wall"]`) doubles lines and type and drops three steps
toward the ground.

The colors are generated: `scheme-rules.js` holds the anchors (the
approved dark ramp and hues) and the rules, `make tokens` writes them into
`tokens.css` (and the palette page's data), and `web/test/contrast.test.mjs`
checks the result against WCAG and the ΔE rules. Edit the rules, not the
generated block. `scheme.js` chooses the scheme: `?scheme=light|dark` in
the URL, else the stored preference, else the system's; a change fires
`c4:scheme` and the canvases redraw. Captures name the scheme, since
headless Chrome prefers light.

**The rule.** A hue is a state family. A pale (dim) tint means
that state is true, but there is mitigating context worth showing. Pale
green: the frames are done, but not yet in your output folder because they
are still copying. Pale blue: the rendering family, still in preflight. Pale
orange: a retry is scheduled, nothing is failing right now. Pale red: a
failure a person has acknowledged, or accepted (bad frames accepted so the
job is not blocked), so nobody has to act now. Pale orchid: at risk, not late
yet. A new edge case takes the pale of its family's hue, never a new hue.
Every state also has a shape, so none rests on color alone. Approved by
the maintainer (see
[docs/intro/palette/state-colors.html](../../docs/intro/palette/state-colors.html)).
The table lists today's cases; they are examples of the rule, not its
definition.

**Orange: something is wrong, and it may still resolve itself.**
A retry may fix a frame; another agent's work may answer a question. Either
may also turn permanent: the frame fails (red), or the question must be
answered. So a question a unit stopped for is drawn in the hue of a
deadline at risk while it is raised, and turns orange once it is pressing
(an answer now costs the deadline, or it has waited too long), with the
reason as text. That is the same meaning as a retry's orange, not a second
one. What still sets it apart from a retry is that it is a question to a
person: it keeps the needs-you flag, it sits under "Needs you", and it may
be sent as a notification (Preferences, Notifications; policy's
`escalation.notify`). Supplied or withdrawn, it is shown for a few minutes
as resolved, with who and why, under what the farm handled, and then
leaves.

**A pale is more transparent, not darker.** It is the state's hue at 48%
opacity over the ground (`color-mix(in srgb, var(--done) 48%,
var(--ground))`, exactly what a 48% alpha composites to), so the same rule
reads on black and on white. On the dark field it lands within ΔE 1.1 of
the dim tints the maintainer approved (#276639, #294b80, #824e22, #77251e, #593652).

**The translation rule: one set of rules, two schemes** (light: approved by the maintainer on
2026-10-18, next to the dark scheme's approval). The light scheme inverts fills and lines: white
ground, black ink, the same hues.

- *Ground, ink and lines.* The light ramp mirrors the dark one in CIELAB
  lightness (L* becomes 100 − L*, the cool tint kept): `--g00` becomes the
  white ground, `--g19` the black ink, and every pair of steps keeps its
  perceptual distance. So "a step up the ramp" means "farther from the
  ground", never "lighter": emphasis, rules and neatlines translate
  without edits. A step that carries a text role moves toward the ink only
  as far as its floor needs (in light, the pencil step moves 1.6 L*).
- *Hues.* A hue keeps its OKLCH hue and chroma in both schemes. Its
  lightness moves toward the ink only where a cell needs 3:1 (WCAG
  non-text) against the surfaces it sits on: on white, green, blue and
  orange darken; red and orchid already pass. A hue set as text uses its
  text form (`--done-text` and so on), moved the same way to 4.5:1. In the
  dark scheme every hue and text form is the approved one, unchanged.
- *Pales.* The opacity rule above, over the scheme's own ground.
- *Shadows, scrims, glows.* Made from the void at an opacity, so on white a
  card lifts on a white halo and a scrim whitens what is behind it; a glow
  (the wall's sweep) is the paper mark at an opacity.
- *Images* stay as rendered: a frame in the preview plays on black in both.

Measured (`node web/design/gen-tokens.mjs --report`): body text 4.5:1 and
secondary labels 3:1 on every surface, every cell 3:1, in both schemes. In
light, one opacity cannot hold every pale ΔE 18 from its full strength
under every vision (2.11); the worst are blue under tritanopia (14.4) and
green (15.2), and the test holds light to ΔE 14.

**Verification is not a state.** Some facts are verification metadata, not
state: a unit's derivation class (exact, validated, volatile: how its output
can be checked, see c4 `design/derivation-records.md`; "class is
verification, never worth"), a record's signer, whether a record verifies.
They never take a state hue and never render as a quality tier: no green
for exact, nothing ranked. Show them as a neutral label or a seal in the job
and provenance views, in ink, with a glossary entry. The planner's reuse and
recompute decisions are shown as the choice it made ("reused output, exact
verified", "recomputed locally"), never as good or bad. Red stays for a
record that does not verify: that one needs a person.

### One vocabulary

A wrangler learns one word per idea, and every surface uses it: the HUD's
cells, legends, cards and book, the glossary, the intro, the tutorials and
the site. The words are
render words: an agent, CI or ML job's units are frames too, and rendering
is a unit at work in any class. Native names (Kubernetes, Kueue, the
operator's own enum values such as `preflight` or `PreemptionByScheduler`)
appear only in Full mode (5), in the unit card's "how" line.

| family          | term                | what it means                                                                                               | token           | drawing                                                                         | from the API                                            | mode |
| --------------- | ------------------- | ----------------------------------------------------------------------------------------------------------- | --------------- | ------------------------------------------------------------------------------- | ------------------------------------------------------- | ---- |
| not started     | queued              | ready to run; waits for a slot                                                                              | `--g09`         | flat                                                                            | `queued`                                                | 1    |
|                 | blocked on *x*      | waits for other work or an input to exist                                                                   | `--g09` `--g12` | hatched; *x* as text                                                            | `blocked`, `edges`, the job's `dependsOn`               | 1    |
|                 | waits for *a thing* | a check or decision outside the job: capacity, a license, a party, a time                                   | `--g09` `--g12` | hatched, with the clock mark; what and when as text                             | `waits`, `waits[]` with `party:` or `clock:`            | 1    |
|                 | held                | a person paused it                                                                                          | `--g09` `--g16` | two bars                                                                        | `held`, `nudge.hold`                                    | 1    |
|                 | locked by *x*       | another unit holds something this one needs to write                                                        | `--g09` `--g12` | hatched, with the lock mark; the line as text                                   | the plan gate (no operator field yet)                   | 1    |
|                 | together            | these units start and stop as one                                                                           | their one state | one outline around the gang's cells                                             | `order: gang`                                           | 4    |
|                 | reserved            | slots set aside for work that will start soon                                                               | `--g15`         | a dotted outline on the slots, pools view only                                  | (no operator field yet)                                 | 4    |
|                 | expected            | work or capacity that will exist but does not yet                                                           | `--g08` `--g12` | dashed outline, stippled                                                        | `planned`, `exists` < 1, generators, provisioning nodes | 3    |
| working         | preparing           | placed, getting ready: inputs, cache, image, license                                                        | `--run-pale`    | blue outline, nothing rising; the step on hover                                 | `preflight`                                             | 1    |
|                 | rendering           | at work                                                                                                     | `--run`         | bright blue rising, grey above, blue outline                                    | `running`                                               | 1    |
|                 | yielding            | asked to stop at its next safe point; it resumes where it left off                                          | `--run`         | rendering, with the pause mark; the safe point and the reason as text           | `stopping`, `safepoint.next`                            | 1    |
| done            | moving              | rendered, output still moving                                                                               | `--done-pale`   | solid green rising inside as bytes commit                                       | `egress`                                                | 1    |
|                 | done                | committed and named                                                                                         | `--done`        | solid; a finished upstream job is the same green                                | `succeeded`                                             | 1    |
|                 | checked             | a check passed                                                                                              | its state's     | the tick mark, in ink; never a hue                                              | `review.do: pass`                                       | 1    |
|                 | reused              | the same inputs ran before; the output was reused                                                           | `--done`        | the reuse mark, in ink                                                          | (derivation records; no operator field yet)             | 1    |
| the farm's      | interrupted         | stopped by the farm, not by its own fault; it runs again                                                    | `--g09`         | queued, with a grey notch per interruption; the minutes lost as text            | `interruptions[]`                                       | 1    |
| temporary error | backoff             | it failed; a retry is scheduled                                                                             | `--temp-pale`   | dashed orange outline, an orange notch per earlier failed attempt               | `backoff`                                               | 1    |
|                 | retrying            | it failed and runs again                                                                                    | `--temp`        | rendering, with an orange outline and notches                                   | `retrying`                                              | 1    |
|                 | rework              | a check found a problem; it runs again with a change                                                        | `--temp`        | as backoff or retrying, with the rework mark; the check's reason as text        | `review.do: rework`                                     | 1    |
|                 | asks you, pressing  | a question a unit stopped for now costs the deadline or has waited too long; other work may still answer it | `--temp`        | the needs-you flag in orange; what it blocks and the time to answer by, as text | `needs[].state: pressing`; incident `severity: urgent`  | 0    |
| needs a person  | needs you           | a person must act                                                                                           | `--fail`        | solid, a white corner flag; who and why as text                                 | `failed`, `flagged`, `waits[].needsYou`, `needsPerson`  | 0    |
|                 | waits for *someone* | a person's decision that holds up nothing yet                                                               | `--fail-pale`   | the person's name as text                                                       | `waits[]` with `person:`, not blocking                  | 1    |
|                 | acknowledged        | someone has it                                                                                              | `--fail-pale`   | the seal of whoever took it                                                     | `ack`                                                   | 1    |
|                 | accepted            | the bad frames were accepted; no one needs to act                                                           | `--fail-pale`   | a check; the note on hover                                                      | `accepted`, `review.do: accept`                         | 1    |
| deadline        | at risk             | projected after the deadline, still fixable                                                                 | `--risk-pale`   | a dashed orchid edge or tick                                                    | `estimate.risk: atRisk`                                 | 0    |
|                 | will miss           | late even with every slot it may use                                                                        | `--risk`        | hatched                                                                         | `willMiss`                                              | 0    |
|                 | missed              | the deadline passed                                                                                         | `--risk`        | solid                                                                           | `missed`                                                | 0    |
|                 | asks you            | a unit stopped for a question only a person can answer; the rest of the job runs on                         | `--risk`        | the needs-you flag in orchid; the time to answer by, as text                    | `needs[].state: raised`; incident `severity: warning`   | 0    |
| the farm's      | answered            | the question was supplied or withdrawn; shown a few minutes, then gone                                      | no hue          | the check mark; who answered or withdrew it, and why, as text                   | `needs[].state: supplied` or `withdrawn`                | 0    |

Capacity takes no hue: **guaranteed**, **borrowed** and **lent** are a
project's slots (its own, taken from another, given to another), a **pool**
is a kind of slot, and **may use** is the order a job falls back through
pools; all appear in Farm mode (4), in the pools view and on the unit card. A
budget, a token meter or a power cap is capacity too, shown like slots with
its reason as text, never as a state: a capped node's lost slots are
hatched, "capped: battery 21%", and the word is capped, not held or blocked.

The marks are ink, drawn by `marks.js` in one shape everywhere (cells,
legends, glossary swatches, lenses): **clock** (waits for a thing),
**lock** (locked by), **pause** (yielding), **notch** (an earlier attempt:
orange when it failed, grey when interrupted), **rework** (a check sent it
back), **tick** (checked), **reuse** (reused), **flag** (needs you),
**seal** (acknowledged), **check** (accepted). A mark never changes the
cell's hue: interrupted is never orange (the unit did nothing wrong), a
machine's wait is never red (no person must act), a check is never green.

**Gates** (Shots mode, 3; INTEGRATE 4.4): a stage of waits or of checks is a gate,
a narrow column between the stages it sits between. Each of its blocks spans
the rows it holds: a wait the units blocked on it (`edges`, `blockedOn`), a
check the frames it read or judged (`edges`, `review.by`); blocks that hold
the same rows sit side by side. A block takes its unit's drawing (a wait for
a thing hatched with the clock, a person's wait red or pale red, a check its
state). While a rework is under way, an orange arrow runs from the check
that sent the frame back to the frame (stack.js, paintOverlays).

**A plan as a render** (from level 2; plansheet.js): a plan job's sheet has
a row per block, named by its charter, and its step kinds as stages in the
order work flows (charter, build, check, review, refute, merge). A block
not expanded yet has expected cells in the stages to come; a block's
phases in one stage share its cell, left to right. A plan's own checks and
reviews are stages of each block; only its waits are a gate. From Shots
mode, a block's dependencies on other blocks are arcs in a gutter between
the names and the first stage, pointing at the block that waits: every arc
thin and `--g12`, the selected or hovered block's in `--ink`; at level 3,
"all dependencies" draws the full set at full weight. Arcs are ink: a
dependency is structure, not state.

**Agent bundle members** take the same words: rendering is `--run`;
queued is `--g09` flat; a member the queue stopped for capacity waits for
capacity: hatched with the clock mark and the checkpoint mark (a small ink
square at the turn it resumes from); held by a person is the two-bar hold
mark; done is `--done`, and a failed member needs you. A member stopping at
the end of its turn is yielding: `--run` with the pause mark, the reason as
text. A stopped interval is a gap on the bundle-time ruler and a `--g06`
band across the rows: neutral grey, one step off the well, drawn under the
member strips. A capped node's lost slots are hatched, with the reason as
text. Right after a cap drops, units still rendering past it stay `--run`
until they stop at the end of their turns; the text counts them, and the
pool keeps its full shape in cells.

**Plan lanes** (a plan job's lens, one cell per lane, under the render in
Full mode at level 3) take the same words:
a lane in the plan not yet chartered is expected (dashed outline,
stippled); a chartered lane waiting for a slot is queued, `--g09` flat; one
blocked on the lanes it follows is hatched, with those lanes as text; a
lane at work (its charter, phases, refutation or merge) is rendering, with
its steps as a strip inside the cell; merged is done. A lane a refuter sent
back is rework: rendering with an orange outline and a rework mark per
pass, the reason as text. A wait (a ruling, a review) is a cell in its row:
a person's open wait that holds up work needs you; one that holds up
nothing yet is pale red with the person's name; a wait for an outside
party is hatched with the clock mark, never red; a decided one is done. A
lane whose merge waits for the gate (one merge at a time) is locked by it:
hatched with the lock mark, the line's length as text.

Red is only for what needs a person: a failure after its retries, a check
that escalated, a stuck job, a decision or approval that holds up work, a
permission block. Lateness is orchid.
Everything else is grey: selection is a paper outline, and burst capacity
is ordinary slots (blue while running). The percent done never goes down
(`core.js` `steady`): a growing estimate shows in the time to done and the
finish band, not by pulling the green back.

## Components

ES modules with no build step, shared by the HUD (`../js`) and the previews.

| module                        | what                                                                                    |
| ----------------------------- | --------------------------------------------------------------------------------------- |
| `components/core.js`          | tokens for drawing, work-weighted aggregation with a unit fallback, the glance numbers  |
| `components/pylon.js`         | PYLON, the glance gauge: full, compact, mini; tweens between snapshots                  |
| `components/stack.js`         | STACK: a shot's sheet as merged cells; collapse, fold upstream, overlays, hover, select |
| `components/ledger.js`        | seals, ink and pencil, the book                                                         |
| `components/lattice.js`       | LATTICE: pools as identical cells, quotas, borrowing, burst                             |
| `components/atlas.js`         | ATLAS: sites as pool rings, regions and burst, the tiers' fan-in                        |
| `components/timebase.js`      | TIMEBASE: channels sweeping toward deadlines, mission clocks, the strobe strip          |
| `components/dispatch.js`      | the farm sheet: batch strips, the slack datum, shot grouping, the burst decision        |
| `components/live.js`          | live components: hosts the views do not morph, tweening                                 |
| `components/fmt.js`, `rle.js` | the farm's clock, durations, grid-reference IDs; unit sets                              |

## Previews

Each file in `previews/` is one card: its opening line is
`<!-- @dsCard group="…" -->`, so the set can be synced to a claude.ai/design
design-system project with `/design-sync`. The previews read
`fixtures/` (frozen from `hack/hudmock`) when present, else the live API of
whatever serves them. With the mock running:

```sh
go run ./hack/hudmock --freeze 50m
open http://127.0.0.1:8740/design/previews/stack-sheet.html
```

ES modules do not load from `file://`; serve the folder (the mock, the
manager, or any static server at `internal/hud/web`).
