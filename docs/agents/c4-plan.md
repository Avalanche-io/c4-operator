# c4-plan: a development plan as a job

`c4-plan import` reads a written development plan and writes the queue's
expansion document (DESIGN 10.6, version 1): one **planned block** per
lane, the edges between them, and, for every edge, the sentence of the
plan it came from. It is the first step of running agent work as a render
job: the plan rolled out as grey blocks in dependency order, each expanded
later by its charter step into builders, reviews and a merge.

```sh
c4-plan import [--milestone M1] [--extra EDGES.yaml] [-o plan.json] [--notes notes.json] PLAN_DIR
```

It only reads `PLAN_DIR`.

## The lane-map layout

The importer reads a plan written as a lane map: `LANES.md`, a milestone
file (`M1.md`), and `charters/`.

| from                                         | it reads                                                                                                                                       |
| -------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `LANES.md`, the lane tables                  | each lane, its chapter (the block's row), its rule count, status (merged, chartered), charter, the rule families it owns, the M2 column's `after` lists |
| `LANES.md`, prose and the hard orderings     | "X follows Y", "X cannot start before Y merges", "X precedes Y", "X lands before Y starts", "X starts after Y merges", "X starts when Y … landed", families that "come after" other families; ranges (`C4.1 … C4.6`) and chapter references (`C3`) |
| `LANES.md`, the run order block              | each lane's `runOrder`                                                                                                                         |
| `LANES.md`, "… as fillers"                   | `filler: true`                                                                                                                                 |
| the milestone file, "Order and parallelism"  | the waves, "(after X)", "X, then Y", "Z … once Y lands"                                                                                        |
| each charter                                 | `- **Model:**`, `- **Refutation:**`, and the paths under `- **Grants, by file:**`, which become the block's `holds`                            |

A lane whose row says **DEFERRED TO M2** is left out of the milestone.

## The document

Each block is a unit of the expansion document: `name` (the lane),
`label`, `key` (the estimator key: `lane`, `infra-lane`, `metal-lane`, …),
`after`, `exists: 1`, `planned: true`, `slots: 1`, and four optional fields
the queue may ignore:

- `group`: the row it belongs to (`chapter 2`, `phase A`);
- `holds`: the paths it writes;
- `params`: plan facts (`rules`, `status`, `sha`, `charter`, `model`, `refute`, `filler`, `runOrder`, `wave`, `target`, and `origin`: `plan` for a lane in the plan from the start, `follow-on` for one chartered later from another lane's need);
- `cites`: for each edge, `{after, file, line, text}`.

## What it could not read

Anything with ordering words and lane names that matches no pattern, an
edge to a lane the plan does not list, a deferred lane, and every bare
chapter reference it expanded become **notes** (`--notes`, else stderr),
for a person to check. A plan states some orders in words no pattern reads
("no lane from C2.1 onward starts until the foundation lane is merged"); a
person adds those with `--extra`, a YAML list citing the plan the same way:

```yaml
- {block: C2.1, after: [F1], file: LANES.md, line: 193, text: "no lane from C2.1 onward starts until the foundation lane is chartered and merged"}
```

A cycle in the edges is an error that names the blocks on it.

## Rulings and reviews as waits

A plan also holds up lanes for decisions: an owner's ruling, an oracle's
review. An `--extra` entry with `wait` adds one as a **wait block** (DESIGN
10.6, waits): no slot, completed by a signed decision. The lanes in `before`
run after it, and it takes the row and origin of the first of them:

```yaml
- {block: R-owner-item-85, before: [C7.6], file: LANES.md, line: 104, text: "ERR-17's refusal for three facts of the program's environment (L290, owner item 85)",
   wait: {server: "person:owner", ask: "owner item 85", expect: 46080}}
```

`server` is `person:<id>`, `party:<name>` or `clock:<time>`. `expect` is
the expected wait in seconds, used until the job has measured that
server's own decisions.
