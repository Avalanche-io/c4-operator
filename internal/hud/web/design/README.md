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

**The rule (Joshua).** A hue is a state family. A pale (dim) tint means
that state is true, but there is mitigating context worth showing. Pale
green: the frames are done, but not yet in your output folder because they
are still copying. Pale blue: the rendering family, still in preflight. Pale
orange: a retry is scheduled, nothing is failing right now. Pale red: a
failure a person has acknowledged, or accepted (bad frames accepted so the
job is not blocked), so nobody has to act now. Pale orchid: at risk, not late
yet. A new edge case takes the pale of its family's hue, never a new hue.
Every state also has a shape, so none rests on color alone. Approved by
Joshua (see
[docs/intro/palette/state-colors.html](../../docs/intro/palette/state-colors.html)).
The table lists today's cases; they are examples of the rule, not its
definition.

**A pale is more transparent, not darker.** It is the state's hue at 48%
opacity over the ground (`color-mix(in srgb, var(--done) 48%,
var(--ground))`, exactly what a 48% alpha composites to), so the same rule
reads on black and on white. On the dark field it lands within ΔE 1.1 of
the dim tints Joshua approved (#276639, #294b80, #824e22, #77251e, #593652).

**The translation rule: one set of rules, two schemes** (light: *pending
Joshua's approval*). The light scheme inverts fills and lines: white
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

| family          | phase          | token           | shape                                                      |
| --------------- | -------------- | --------------- | ---------------------------------------------------------- |
| not started     | ready          | `--g09`         | flat                                                       |
|                 | blocked        | `--g09` `--g12` | hatched                                                    |
|                 | held           | `--g09` `--g16` | two bars                                                   |
| working         | preflight      | `--run-pale`    | blue outline, nothing rising                               |
|                 | rendering      | `--run`         | bright blue rising as it renders, grey above, blue outline |
| done            | moving         | `--done-pale`   | solid green rising inside as bytes commit                  |
|                 | committed      | `--done`        | solid; a finished upstream job is the same green           |
| temporary error | backoff        | `--temp-pale`   | dashed orange outline, a notch per earlier attempt         |
|                 | retrying       | `--temp`        | rendering, with an orange outline and notches              |
| needs a person  | unacknowledged | `--fail`        | solid, a white corner flag                                 |
|                 | acknowledged   | `--fail-pale`   | the seal of whoever took it                                |
|                 | accepted       | `--fail-pale`   | a check: bad frames accepted, nothing blocked              |
| deadline        | at risk        | `--risk-pale`   | a dashed orchid edge or tick                               |
|                 | will miss      | `--risk`        | hatched                                                    |
|                 | missed         | `--risk`        | solid                                                      |

Red is only for what needs a person: a failure after retries, a stuck job,
an approval waiting, a quota or permission block. Lateness is orchid.
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
