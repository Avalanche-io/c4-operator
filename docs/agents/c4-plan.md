# c4-plan: a plan as a job

`c4-plan import` turns a plan into the queue's expansion document (DESIGN
10.6, version 1): one block per piece of work, the edges between them, and,
for every edge, where it came from. It is the first step of running agent
work as a render job: the plan rolled out as grey blocks in dependency
order, each expanded later into its own steps (builders, checks, reviews, a
merge).

```sh
c4-plan import [--format file|lanemap] [--milestone M1] [--extra EXTRA.yaml] [-o plan.json] [--notes notes.json] [--owners owners.json] PLAN
c4-plan edges --since plan.json [--since edges-1.json …] [--format …] [--extra …] [-o edges.json] [--notes notes.json] PLAN
```

It only reads `PLAN`. The output is the queue's own format, and the command
reads it back the way the queue would before it writes it: a document the
queue would refuse is an error here, not at run time.

## The core and its adapters

The core (`internal/plan`) knows blocks, edges, citations, waits and notes,
and nothing about any one project. A plan reaches it through an adapter:

| format    | `PLAN` is   | for                                                                                         |
| --------- | ----------- | ------------------------------------------------------------------------------------------- |
| `file`    | a file      | a plan written as data: the format to keep a plan in                                        |
| `lanemap` | a directory | a plan written as prose in the lane-map layout, read by sentence patterns |

Without `--format`, a file is read as `file` and a directory as `lanemap`.
Another layout is another adapter: it calls `plan.New`, `Block`, `Edge` and
`Note`, and the core writes and checks the document.

## A plan written as data

A YAML or JSON list of blocks. Each is a unit of the expansion document with
the defaults a person would otherwise repeat: a block is planned (a
generator), exists for certain and takes one slot, unless it says otherwise
or is a wait.

```yaml
version: 1
blocks:
  - {name: text/scan, group: leaves, key: module, holds: [text/scan], params: {size: "14"}}
  - name: text/tokens
    group: core
    key: module
    reasoning: {does: model, effort: high}
    after: [text/scan]
    why: {text/scan: "tokens imports scan"}
  - name: api-break
    wait: {server: "person:owner", ask: "keep the old open() signature?", expect: 7200}
    before: [text/tokens]
```

- `reasoning` says what the block reasons with: `does` (`model` or
  `procedure`), and optionally `model`, `effort` (low, medium, high, xhigh,
  max) and `tools` (a tool profile's name). `holds` are paths
  or keys the block writes, exclusive claims; `claims`
  are `{on, mode}` with mode `exclusive` (the default) or `shared`; `gate`
  names the one-slot gate the block passes (a merge). The queue enforces
  all three.
- `before` lists blocks that run after this one, written where the reason is
  (a wait before what it holds up). A wait with no `group` takes the row of
  the first block in its `before`.
- `why` gives an edge's reason, by the other block's name; it becomes the
  edge's citation. Without one the citation says the file lists it.
- An unknown field, a name used twice or another version is an error. An
  edge to a block the file does not list is a note.

`internal/plan/testdata/port.yaml` is a whole one: a library ported module
by module in the order of its import graph.

## The document

Each block is a unit of the expansion document: `name`, `label`, `key` (the
estimator key; blocks of one kind share history; `block` when the plan
names none), `after`, `exists`, `planned`, `slots`, and four optional fields
the queue may ignore:

- `group`: the row it belongs to;
- `holds`, `claims` and `gate`: what it writes, what it shares, the gate it passes;
- `params`: plan facts, carried as they are;
- `cites`: for each edge, `{after, file, line, text}`.

## After a merge: edges alone

A merge can change the plan: a new sentence, a new line in a plan file.
`c4-plan edges` re-reads it for the job already running it and writes a
document of **edges alone** (expansion v1 `edges: [{from, to, cite}]`;
DESIGN 10.6, edge sources): every ordering between the job's blocks that
the job does not have yet, each citing where it was read. `--since` is the
document the job was created from, then each edges document written since,
in order. A procedural unit re-run after each merge writes the result to
`.c4/expand.json`, and the job's graph follows its plan.

Edges are only added. A block the plan now lists that the job does not
have needs an expansion of its own (a new plan version), and an ordering
the job has that the plan no longer states needs a person: both are notes.
An ordering that would close a cycle is an error.

`import --owners` also writes the paths blocks hold, each to its block (a
path two blocks hold is a note). `c4-edges -owners` takes that file, so a
marker in code (a pending edge naming the block it waits on) is an edge
from that block to the block that owns the file.

## What it could not read

An adapter that reads prose records what it could not read as **notes**
(`--notes`, else stderr), for a person to check. Their count is how much of
a plan is still only prose: a plan written as data has none. A person adds
the orders no pattern reads with `--extra`, a YAML list citing the plan:

```yaml
- {block: C2.1, after: [F1], file: LANES.md, line: 193, text: "no lane from C2.1 onward starts until the foundation lane is chartered and merged"}
```

An entry may say who read it (`by`), and `--extra-by NAME` gives that for
every entry that does not. It goes into each edge's citation, so an edge a
person or an agent read from prose stays marked as a reading, for someone to
check, apart from the edges the adapter read itself.

A cycle in the edges is an error that names the blocks on it.

## Waits

A plan also holds up work for decisions: an owner's ruling, an outside
party's review. In a plan file a block with `wait` is one; in `--extra` an
entry with `wait` adds one. Either way it is a **wait block** (DESIGN 10.6,
waits): no slot, completed by a signed decision, and the blocks in `before`
run after it:

```yaml
- {block: R-owner-item-85, before: [C7.6], file: LANES.md, line: 104, text: "ERR-17's refusal for three facts of the program's environment (L290, owner item 85)",
   wait: {server: "person:owner", ask: "owner item 85", expect: 46080}}
```

`server` is `person:<id>`, `party:<name>` or `clock:<time>`. `expect` is
the expected wait in seconds, used until the job has measured that
server's own decisions.

## The lane-map layout

The `lanemap` adapter (`internal/plan/lanemap`) reads a plan written as a
lane map: `LANES.md`, a milestone file (`M1.md`), and `charters/`.

| from                                        | it reads                                                                                                                                                                                                                                    |
| ------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `LANES.md`, the lane tables                 | each lane, its chapter (the block's row), its rule count, status (merged, chartered), charter, the rule families it owns, the M2 column's `after` lists                                                                                     |
| `LANES.md`, prose and the hard orderings    | "X follows Y", "X cannot start before Y merges", "X precedes Y", "X lands before Y starts", "X starts after Y merges", "X starts when Y … landed", families that "come after" other families; ranges (`C4.1 … C4.6`) and chapter references (`C3`) |
| `LANES.md`, the run order block             | each lane's `runOrder`                                                                                                                                                                                                                      |
| `LANES.md`, "… as fillers"                  | `filler: true`                                                                                                                                                                                                                              |
| the milestone file, "Order and parallelism" | the waves, "(after X)", "X, then Y", "Z … once Y lands"                                                                                                                                                                                     |
| each charter                                | `- **Model:**`, `- **Refutation:**`, and the paths under `- **Grants, by file:**`, which become the block's `holds`                                                                                                                          |

A lane whose row says **DEFERRED TO M2** is left out of the milestone. Its
keys name the lane's kind (`lane`, `infra-lane`, `frontend-lane`,
`foundation-lane`, `metal-lane`); its params are `rules`, `status`, `sha`,
`charter`, `model`, `refute`, `filler`, `runOrder`, `wave`, `target`, and
`origin` (`plan` for a lane in the plan from the start, `follow-on` for one
chartered later from another lane's need). Every bare chapter reference it
expanded is also a note.
