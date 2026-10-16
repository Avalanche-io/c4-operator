# The Cinema Content Creation Cloud: c4-operator

c4-operator runs a render, CI and machine-learning farm on Kubernetes on top
of content identity. Every tree a pod reads or writes is a C4 view: a
directory tree in which each file is named by its C4 ID (SMPTE ST 2114), the
SHA-512 of its bytes. C4 is the short form of The Cinema Content Creation
Cloud ([cccc.io](https://cccc.io)).

- **Views, not volumes.** A pod mounts exactly the files it needs through a
  CSI driver, as an overlay of layers that every pod on the node shares. The
  bytes come through node, peer and regional caches that read each object
  from the store about once, however many pods ask for it.
- **Outputs are versions.** When a pod exits, what it wrote is captured as a
  changeset over the view it mounted, and a job's output is one view folded
  from its units' changesets.
- **Isolation by project.** A project's pods can mount only what its own
  store holds; vendors see exactly the paths they were granted, and a
  revoked grant stops new mounts at once.
- **Deadlines, not priorities.** One queue runs render, CI and ML work
  together, ordered by each job's deadline and an estimate of its remaining
  work. People move deadlines, boost or hold; renders can run frames in
  binary order so the estimate is good early; training runs yield their GPUs
  at a checkpoint and resume from it.
- **A HUD and an MCP server.** The HUD shows the farm from live cluster
  state, and an MCP server gives agents every HUD capability as the calling
  user.
- **Provenance by construction.** Every task leaves a signed record of what
  it mounted, where it ran, from which images, and what it wrote. Every
  entity signs as itself: the node that ran the unit, the controller that
  resolved its inputs, the queue that scheduled it, the HUD for each action
  a person or agent took, and the importer that brought the content in. A
  file traces back to its sources, with its declared AI use and a C2PA
  manifest definition.

**Status: alpha, v0.1 (pre-1.0).** The API (`c4.avalanche.io/v1alpha1`),
the manifests and the record formats will change. Every number below was
measured on kind on one laptop, not at production scale.

![The HUD's queue on the kind test farm, mid-run](docs/hud/live/mid-queue.png)

## Measured on kind, on one laptop

kind v0.33 (Kubernetes 1.36), eight workers in two zones, three MinIO
shards, all on one Apple M3 Max. Every link ran at memory speed, so these
numbers show mechanisms, not production performance.
[REPORT-v2.md](REPORT-v2.md) has every measurement, its method and its
limits.

| what                         | result                                                                                                                                                                                              |
| ---------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| store load as pods grow      | 64 pods reading one 28-object shot cost the store 42 GETs through the tiers (1.5 per object), against 224 when each node reads for itself and 1,792 when each pod does                             |
| mounts                       | a warm mount in 2 ms; a cold one in 80 ms at the median, mostly from a peer in the same zone (0.8 to 1.0 s when nothing in the zone held the bytes); the next version as a delta layer               |
| failure                      | a regional cache replica killed mid-transfer: all 32 pods finished, 7 transfers resumed from their offsets at another replica, none fell through to the store                                       |
| isolation                    | nine attempts by one project to read another's data refused; a revoked vendor grant took effect 0.1 s later                                                                                       |
| monorepo CI                  | 401 commits imported in 5.4 s; 16 test shards per change mount the next commit as a 3-file delta in 16 ms; which files changed between two commits in 13 to 221 ms, identical to `git diff`          |
| deadlines                    | 6 of 7 jobs met their deadlines on a contended farm while a training run yielded its GPUs at a checkpoint and resumed; in a five-week simulation, 43% fewer missed render deadlines than manual priorities |
| agents                       | an MCP client answered all 10 questions of the evaluation set from the live farm, each call in under 10 ms                                                                                          |

How this compares with Anthropic's Scion, and what a fair benchmark would
need, is in [REPORT-v2.md](REPORT-v2.md#comparison-with-scion).

<p>
<img src="docs/hud/live/mid-job-sh010-comp-v003.png" alt="A render's job view: frames in binary order and the remaining-work estimate" width="49%">
<img src="docs/hud/live/mid-tiers.png" alt="The cache tiers: hit rates and requests at the node, peer, regional and store tiers" width="49%">
</p>

More screenshots: [docs/hud/live](docs/hud/live) (the kind farm) and
[docs/hud](docs/hud) (a synthetic farm).

## Quickstart

On a laptop with Docker, kind, kubectl and Go:

```sh
git clone https://github.com/Avalanche-io/c4-operator && cd c4-operator && git checkout v0.1.0
go install ./cmd/c4-operator
kind create cluster --name c4 --config config/samples/quickstart/kind.yaml
kubectl apply -k config/release
```

[docs/quickstart.md](docs/quickstart.md) continues from there: a store, a
project, a tree ingested and mounted in a pod, its output captured, and a
render queued and watched in the HUD, in about five minutes.

To install on a cluster of your own, see [docs/install.md](docs/install.md).
Releases attach one `install.yaml`, and the image is
`ghcr.io/avalanche-io/c4-operator` for `linux/amd64` and `linux/arm64`.

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
  kube-scheduler places units as the queue counts them.
- **HUD** (`internal/hud`) and **MCP** (`internal/mcpserver`): one API, two
  faces. **Provenance** (`internal/provenance`): records signed by each
  entity with short-lived certificates from a farm signing CA, the chain of
  custody, C2PA and OMC exports.

[DESIGN.md](DESIGN.md) is the full design, including what is designed and
not yet built.

## Documentation

| document                                         | what it covers                                                            |
| ------------------------------------------------ | ------------------------------------------------------------------------- |
| [docs/quickstart.md](docs/quickstart.md)         | five minutes on kind                                                      |
| [docs/install.md](docs/install.md)               | requirements, install, projects, pools, the HUD, limits before production |
| [DESIGN.md](DESIGN.md)                           | the design of record                                                      |
| [REPORT-v2.md](REPORT-v2.md)                     | what each phase proved, the numbers, the Scion comparison, open decisions |
| [docs/agents/tools.md](docs/agents/tools.md)     | the MCP tools, generated from the server                                  |
| [skills/c4-farm](skills/c4-farm/SKILL.md)        | a skill for agents that operate the farm                                  |
| [PROGRESS.md](PROGRESS.md)                       | branches, release blockers, proposed changes to the c4 suite              |

## Development

Go 1.26, Docker, kind 0.33 and kubectl.

```sh
make tools          # controller-gen, setup-envtest, kind (pinned)
make test           # unit tests, and controllers and the queue against a real API server (envtest)
make test-linux     # overlay, layer and capture tests in a privileged Linux container
make verify         # gofmt, vet, generated code and CRDs in step, manifests render
go run ./hack/sim   # the scheduling simulation
go run ./hack/hudmock   # the HUD against a synthetic farm, no cluster
```

The end-to-end phases run on an eight-worker kind cluster:

```sh
hack/e2e/up.sh      # kind (8 workers, 2 zones), MinIO, the operator, projects
hack/e2e/p1.sh      # views, capture, CSI
hack/e2e/p2.sh      # tiers and herds
hack/e2e/p3.sh      # isolation
hack/e2e/p5.sh      # the work queue (sets up pools; run before p4, p6, p7)
hack/e2e/p4.sh      # monorepo CI
hack/e2e/p7.sh      # provenance
hack/e2e/p6.sh      # the MCP evaluation
```

See [CONTRIBUTING.md](CONTRIBUTING.md). Security reports go through
[SECURITY.md](SECURITY.md).

## Commands

```
c4-operator manager      controllers, the work queue, the HUD, the pod webhook, the certificate issuer
c4-operator node         CSI node plugin, node cache, peer tier, capture, task records
c4-operator regional     regional cache tier
c4-operator ingest       hash a directory into a project's store; print its view ID
c4-operator import-git   import a git history (with c4git assets) as views, changesets and chains
c4-operator mcp          the farm's MCP server, over stdio or Streamable HTTP
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
internal/provenance/   signers, certificates, records, verification, C2PA and OMC exports
internal/gitimport/    git history -> views, changesets, chains
config/                kustomize: default (local images), release (GHCR), samples
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
project takes the same structure to media pipelines, with one difference in
the monorepo case: Scion made source control server-side, while here git
stays where it is and its history is mirrored into C4 views (`import-git`,
push by push).

## Versioning

c4-operator follows semantic versioning; before 1.0, minor versions may
break the API, the manifests and the record formats. It depends on c4
master at a pseudo-version (`v1.0.17-0.20261016134402-db4c7bec2376`, commit
db4c7be) because no c4 release carries the APIs it uses yet; expect a
re-pin after c4's v1.1 re-scope.

The operator is rebuilt from a 2017 design by Joshua Kolden, who also wrote
SMPTE ST 2114, the C4 ID standard. It was designed and built with Claude,
working as his engineering agent.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
