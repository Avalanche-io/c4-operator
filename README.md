# c4-operator

A Kubernetes operator for media and machine-learning work built on content
identity. Every tree a pod reads or writes is a C4 view: a directory tree in
which every file is named by its C4 ID (SMPTE ST 2114). Pods mount views
through a CSI driver as overlay filesystems; the bytes come through node,
peer and regional caches that fetch each object from the store about once,
whatever the number of pods. What a pod writes is captured as a changeset when
it exits. A deadline-driven queue runs render, CI and ML work on the same
farm, a HUD shows it, an MCP server gives agents the same controls as people,
and every task leaves a signed record of what it read, where it ran and what
it wrote.

C4 is the Cinema Content Creation Cloud ([cccc.io](https://cccc.io)). The
operator is rebuilt from a 2017 design by Joshua Kolden, who also wrote SMPTE
ST 2114, the C4 ID standard.

Status: the `v2` branch, unreleased (see [Release](#release)). Every phase is
measured on an eight-worker kind cluster; the numbers below come from
[`hack/e2e/out/results.md`](hack/e2e/out/results.md) and the design is in
[`DESIGN.md`](DESIGN.md). [`REPORT-v2.md`](REPORT-v2.md) summarizes both.

## What the evidence shows

| phase                  | what was shown on the kind cluster                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| P1 views, capture, CSI | 16 pods under Pod Security `restricted` mount one shot through the CSI driver: one shared layer per node, one writable upper per pod, a warm mount in 2 ms (cold 80 ms), no bytes hashed on the read path. Every captured output equals the tree the pod saw, recomputed on the host. The next version of the shot mounts as a delta layer: 17 objects fetched across 8 nodes.                                                                                                                                                                                                                                                                                   |
| P2 tiers               | 64 pods reading one 28-object shot cost the store 42 GETs through the tiers (1.5 per object), against 224 when each node fetches for itself and 1,792 when each pod does. A regional replica killed with SIGKILL mid-transfer: all 32 pods finished, 7 transfers resumed from their offsets elsewhere, none went to the store directly.                                                                                                                                                                                                                                                                                                                          |
| P3 isolation           | Nine attempts by one project to read another's data (by view ID, by a hand-written c4m, through both cache tiers' APIs with and without a token, through a forged CSI volume context) are refused. Strict projects get separate copies of identical bytes; Shared projects share one copy only after proving they hold it. A vendor sees exactly its granted shot; revoking the grant takes effect at the next mount, 0.1 s later.                                                                                                                                                                                                                               |
| P4 monorepo CI         | The synthetic monorepo (2,219 files, 401 commits, 62 c4git assets) imports into MinIO in 5.4 s (7.5 ms per commit at p50). For 16 CI shards per change, a node mounts the parent commit cold in 1.7 s, the next commit as a 3-file delta layer in 16 ms, and a rerun in 2 ms; a git clone and c4git checkout of the same commit takes 1.1 s on the host SSD. Which files changed between two commits is answered from records alone in 13 to 221 ms, identical to `git diff`.                                                                                                                                                                                    |
| P5 work queue          | Seven jobs from two shows on 16 cpu, 8 GPU and up to 8 burst slots: 6 of 7 deadlines met. A training run gave its GPU slots to an urgent render at a checkpoint and resumed from the step it saved, through the view its capture made. Burst capacity was requested, provisioned and in use within about 20 s, for $0.03. The remaining-work estimate of a binary-order render was within 10% from a tenth of the way in and within 3% from a quarter on (a render manager's mean-frame estimate: 15% and 7%). In simulation over five weeks, the policy misses 43% fewer render deadlines than manual priorities and cuts the CI p95 from 17.2 to 14.4 minutes. |
| P6 agents              | 17 MCP tools over stdio and stateless HTTP, each a call to the HUD API as the caller. An MCP client answers all 10 questions of the evaluation set from the live farm, and a hold made through MCP shows up as an Event naming the user.                                                                                                                                                                                                                                                                                                                                                                                                                         |
| P7 provenance          | Each capture is signed by the node daemon as an in-toto Statement in a DSSE envelope. A frame of a model-assisted denoise traces to its render through the input view, with the declared model, oversight and consent; its C2PA definition carries the AI disclosure. Changing one field of a record breaks its signature. The key is mounted only by node daemons.                                                                                                                                                                                                                                                                                              |

The HUD on the live farm (more in [`docs/hud/live`](docs/hud/live)):

![The queue on the live e2e farm, mid-run](docs/hud/live/mid-queue.png)

## How it works

```
                     project store (S3 shards, c4d)
                                  |  each object about once per regional home
            regional tier (Deployment, r = 2 homes per object, streams, resumes)
                                  |  one request per locality
      node daemons (DaemonSet): peer homes by rendezvous hashing within a zone
                                  |
   CSI NodePublishVolume: verify the pod's token, stack the view's layers
   (hardlinks into the node cache), overlay with a private upper -> pod
                                  |
   NodeUnpublishVolume: capture the upper as a changeset over the view,
   store it, sign a task record, annotate the pod
```

- **Views and records** (`internal/view`). A view's ID is the C4 ID of its
  root record; directory records are stored like any object, so a node loads
  only the parts of a tree its filter reaches. A changeset is a c4m patch
  sealed by the view it produces, and changesets append into chains that the
  c4 CLI reads (`c4 log`, `c4 split`).
- **Tiers** (`internal/tier`, `internal/fill`). One content API at every
  tier. Fills are coalesced (one upstream fetch per object, however many
  waiters), streamed to waiters while they arrive, shed with `503` and
  `Retry-After` past capacity, and resumed from their offset at another home
  when an upstream dies.
- **Isolation** (`internal/project`, `internal/auth`). A Project names its
  namespaces, its store shards and credentials (in a Secret only the operator
  reads), Strict or Shared caching, and optional grants. Pods never fetch by
  ID; they mount Materializations in their own namespace, resolved through
  their project's store.
- **Work** (`internal/work`). A queue in the manager releases units as pods
  only when they can run, in an order derived from each job's deadline and an
  estimate of its remaining work. Slots are an extended resource, so
  kube-scheduler places units as the queue counts them. There are no
  priorities: people move deadlines, boost or hold.
- **HUD** (`internal/hud`) and **MCP** (`internal/mcpserver`): one API, two
  faces. **Provenance** (`internal/provenance`): task records, lineage, C2PA
  and OMC exports.

## Try it

Requirements: Docker, kind 0.33, Go 1.26, kubectl. Everything runs on one
machine; nothing is pushed anywhere.

```sh
hack/e2e/up.sh          # kind cluster (8 workers, 2 zones), MinIO, the operator, projects
hack/e2e/p1.sh          # views, capture, CSI
hack/e2e/p2.sh          # tiers and herds
hack/e2e/p3.sh          # isolation
hack/e2e/p5.sh          # the work queue (sets up pools; run before p4, p6, p7)
hack/e2e/p4.sh          # monorepo CI
hack/e2e/p7.sh          # provenance
hack/e2e/p6.sh          # the MCP evaluation
go run ./hack/sim       # the scheduling simulation
```

The HUD is at <http://127.0.0.1:30080/?as=wrangler> (the e2e runs the HUD
with development viewers; `?as=artist` sees one show). To give Claude Code
the farm's tools:

```sh
go build -o bin/c4-operator ./cmd/c4-operator
claude mcp add c4-farm -- "$PWD/bin/c4-operator" mcp --hud http://127.0.0.1:30080 --as wrangler
```

and see [`skills/c4-farm`](skills/c4-farm/SKILL.md) and
[`docs/agents/tools.md`](docs/agents/tools.md). The HUD can also run against a
synthetic farm with no cluster: `go run ./hack/hudmock`.

## Commands

```
c4-operator manager      controllers, the work queue, the HUD, the pod webhook
c4-operator node         CSI node plugin, node cache, peer tier, capture, task records
c4-operator regional     regional cache tier
c4-operator ingest       hash a directory into a project's store; print its view ID
c4-operator import-git   import a git history (with c4git assets) as views, changesets and chains
c4-operator mcp          the farm's MCP server, over stdio or Streamable HTTP
c4-operator keygen       an Ed25519 key for signing task records
c4-operator tree-id      the view ID of a directory
```

## Layout

```
api/v1alpha1/          Materialization, Project, WorkJob, Pool, CapacityRequest
cmd/c4-operator/       the one binary
internal/view/         views, records, filters, changesets, chains, diffs
internal/tier/         the content API, fetcher, server, uploader
internal/fill/         coalesced, streamed fills with admission limits
internal/objcache/     partitioned node cache
internal/layer/        layers (full and delta), whiteouts, stacks
internal/overlay/      overlayfs mounts and upper walks
internal/capture/      upper -> changeset -> store
internal/node/         the node daemon: CSI, publish, capture, records, GC
internal/controller/   Materialization and Project controllers (envtest)
internal/project/      projects, stores, credentials
internal/auth/         TokenReview for pods and tiers
internal/work/         order, estimate, sched (policy), class, queue, kindsim
internal/hud/          web app, API types, backend
internal/mcpserver/    MCP tools over the HUD API
internal/provenance/   task records, DSSE, C2PA and OMC exports
internal/gitimport/    git history -> views, changesets, chains
hack/e2e/              the kind cluster and the phase scripts
hack/sim/              the scheduling simulation
hack/hudmock/          a synthetic farm behind the HUD API
```

## Relation to C4, SMPTE ST 2114 and Scion

A C4 ID is a SHA-512 digest encoded as a 90-character string that starts with
`c4`, standardized in SMPTE ST 2114:2017. Because the ID is derived from the
bytes, any holder of the bytes can check them. The operator imports the
[c4 Go module](https://github.com/Avalanche-io/c4) for IDs, c4m and its
stores, and reads and writes stores the c4 CLI uses.

Anthropic's [Scion](https://www.anthropic.com/engineering/redesigning-source-control)
showed what a content-addressed tree service does for a monorepo's CI. This
project takes the same idea to media pipelines, with one difference in the
monorepo case: Scion made source control server-side, while here git stays
where it is and its history is mirrored into C4 views (`import-git`, push by
push).

## Release

Not released. `go.mod` replaces the c4 module with a local checkout until a c4
release carries the APIs the operator uses; the image registry path is open.
See [`PROGRESS.md`](PROGRESS.md) for the release blockers, the open decisions,
and changes proposed for c4, c4d, c4git and c4-containers.

This system was designed and built with Claude, working as Joshua's
engineering agent.

## License

Apache License 2.0, the same as c4. See [LICENSE](LICENSE).
