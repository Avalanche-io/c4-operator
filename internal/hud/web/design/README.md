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

| motif                         | means                                                                                   | data                                                    |
| ----------------------------- | --------------------------------------------------------------------------------------- | ------------------------------------------------------- |
| neatline and collar           | the sheet's bounds; the title block (deadline, ETA band, slack, done, slots, output)    | `JobSummary`, `estimate`, `work`, `output`              |
| edge references               | upstream jobs west, downstream east, sibling shots north and south                      | `/graph`, `dependsOn`, `shot`                           |
| containment by adjacency      | the shot spans its batches, a batch its frames, a frame's stages sit side by side       | `groups`, `unitList[].pos`, `parent`, `axis.batch`      |
| collapse to aggregate         | a container becomes one block: green done, blue sliver running, orange and red ticks    | `unitList[].work`, `JobSummary.work`                    |
| fill from the bottom          | progress; striped when it is elapsed over estimate                                      | `progress[i].fraction`, `measured`                      |
| lines only where grouping fails | A: another job's input (default on); B: a unit reading other rows (per job, off)     | `/graph`, `unitList[].reads`                            |
| time lines                    | projected finish contours across the rows; the deadline as a red pencil datum           | `unitList[].eta`, `objective.deadline`                  |
| ink and pencil                | measured values solid; estimates lighter, dash-underlined                               | every number                                            |
| seals                         | who signed an entry; dashed until signed                                                | `/book`, task, decision and control records             |
| edge code                     | a job's output view and the changesets folded into its chain                            | `output.view`, `output.changesets`                      |
| the sweep                     | the wall's now line; persistence for what just happened                                 | snapshots over time                                     |

## Tokens

`tokens.css` is the single source: a 20-step grey ramp (`--g00` to `--g19`,
even in CIELAB lightness, packed densely below L*20 where layering lives),
four state colors, type (IBM Plex Sans Condensed, IBM Plex Mono), line
weights, spacing (a 4 px step; the collar and gutter) and motion
(`--fill-ms`, `--collapse-ms`, zero under reduced motion). The wall scale
(`[data-scale="wall"]`) doubles lines and type and drops three dark steps.

| state       | token                    | means                                                        |
| ----------- | ------------------------ | ------------------------------------------------------------ |
| done        | `--done`                 | completed progress; the fill                                 |
| settled     | `--done-settled`         | a finished upstream job, so the brightest green is current   |
| running     | `--run`, `--run-body`    | working now: a blue body, a breathing leading edge           |
| temporary   | `--temp`                 | retrying, an earlier failed attempt, backoff, a suspect node |
| failure     | `--fail`                 | failed, missed deadline; dashed: projected late              |

Everything else is grey: held is a grey hold mark, selection a paper outline,
burst capacity ordinary slots (blue only while running).

## Components

ES modules with no build step, shared by the HUD (`../js`) and the previews.

| module                  | what                                                                                       |
| ----------------------- | ------------------------------------------------------------------------------------------ |
| `components/core.js`    | tokens for drawing, work-weighted aggregation with a unit fallback, the glance numbers     |
| `components/pylon.js`   | PYLON, the glance gauge: full, compact, mini; tweens between snapshots                     |
| `components/stack.js`   | STACK: a shot's sheet as merged cells; collapse, fold upstream, overlays, hover, select     |
| `components/ledger.js`  | seals, ink and pencil, the book                                                            |
| `components/lattice.js` | LATTICE: pools as identical cells, quotas, borrowing, burst                                |
| `components/atlas.js`   | ATLAS: sites as pool rings, regions and burst, the tiers' fan-in                           |
| `components/timebase.js`| TIMEBASE: channels sweeping toward deadlines, mission clocks, the strobe strip            |
| `components/dispatch.js`| the farm sheet: batch strips, the slack datum, shot grouping, the burst decision          |
| `components/live.js`    | live components: hosts the views do not morph, tweening                                    |
| `components/fmt.js`, `rle.js` | the farm's clock, durations, grid-reference IDs; unit sets                          |

## Previews

Each file in `previews/` is one card: its first line is
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
