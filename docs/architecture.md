# How C4 works

The architecture of c4-operator: what it is made of, how the parts fit,
its commands and its code layout. The [README](../README.md) is the short
version.

## Overview

The render queue, reimagined around content identity, deadlines, provenance
and agents. c4-operator turns a Kubernetes cluster into one farm for
rendering, CI and machine learning, where every tree a pod reads or writes
is a C4 view: a directory tree in which each file is named by its C4 ID
(standardized as SMPTE ST 2114, and in production use since before the
standard), the SHA-512 of its bytes. Each object leaves the store
about once however many pods read it, work is ordered by deadlines instead
of priorities, every output is a version, every step is signed by whoever
did it, and agents have the same controls as people.
C4 is the short form of The Cinema Content Creation Cloud
([cccc.io](https://cccc.io)).

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
  state as sheets: each shot's work tree in the order work flows, progress
  by work, every action signed by whoever took it; a wall for the machine
  room, a widget for an artist's second monitor, a phone for the producer.
  An MCP server gives agents every HUD capability as the calling user.
- **Provenance by construction.** Every task leaves a signed record of what
  it mounted, where it ran, from which images, and what it wrote. Every
  entity signs as itself: the node that ran the unit, the controller that
  resolved its inputs, the queue that scheduled it, the HUD for each action
  a person or agent took, and the importer that brought the content in. A
  file traces back to its sources, with its declared AI use and a C2PA
  manifest definition.

## The tiers and the parts

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

[DESIGN.md](../DESIGN.md) is the full design, including what is designed and
not yet built.

## Commands

```
c4-operator manager      controllers, the work queue, the HUD, the pod webhook, the certificate issuer
c4-operator node         CSI node plugin, node cache, peer tier, capture, task records
c4-operator regional     regional cache tier
c4-operator ingest       hash a directory into a project's store; print its view ID
c4-operator import-git   import a git history (with c4git assets) as views, changesets and chains
c4-operator mcp          the farm's MCP server, over stdio or Streamable HTTP
c4-operator tree-id      the view ID of a directory
c4-operator egress-proxy the farm's egress proxy: a job reaches only the networks its tool declares

c4-farm                  `c4 farm`: submit work, watch it, move its deadline, take its output home; up makes a farm on this machine
c4-agent                 the host runner and unit runner, for agent work and tool units on a machine outside the cluster
c4-unit                  the helper every job image carries: progress, frame previews, yielding at a checkpoint, resuming
c4-sync                  keeps a tracker (Flow, GitHub, Kitsu, MLflow) and the farm in step
c4-plan                  turns a plan into a job's expansion document
c4-edges                 runs an edge source over a tree and writes the edges it finds
c4-sandbox-github        the local farm's stand-in GitHub, for the tracker demo
```

## Layout

```
api/v1alpha1/          Materialization, Project, WorkJob, Pool, CapacityRequest, Tool, Tracker,
                       TrackerPerson, TrackerLink, SubmitGrant, AgentBundle, CredentialLease,
                       DeploymentBinding, Milestone
cmd/                   c4-operator and the commands above
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
internal/farmcli/      `c4 farm`, the user command
internal/localfarm/    `c4 farm up`'s farm on this machine
internal/authoring/    submissions and plans, the two authoring formats
internal/agent/        the harness that makes a Claude Code session farm work
internal/tracker/      what c4-sync knows about any tracker
internal/egress/       the egress proxy
internal/telemetry/    what the farm keeps about how it behaved, and diagnostic bundles
config/                kustomize: default (local images), release (GHCR), samples
hack/e2e/              the end-to-end suite: cluster setup and phase scripts
hack/sim/              the scheduling simulation
```

## Installing on a cluster

To install on a cluster of your own, see [docs/install.md](install.md).
Releases attach one `install.yaml`, and the image is
`ghcr.io/avalanche-io/c4-operator` for `linux/amd64` and `linux/arm64`.

## All documentation

| document                                      | what it covers                                                            |
| --------------------------------------------- | ------------------------------------------------------------------------- |
| [docs/quickstart.md](quickstart.md)           | a farm on your machine, a render, and your first tool                     |
| [docs/install.md](install.md)                 | requirements, install, projects, pools, the HUD, hardening for production |
| [DESIGN.md](../DESIGN.md)                     | the design of record                                                      |
| [REPORT-v2.md](../REPORT-v2.md)               | what each phase proved, the numbers, the Scion comparison, open decisions |
| [docs/agents/tools.md](agents/tools.md)       | the MCP tools, generated from the server                                  |
| [docs/agents/c4-agent.md](agents/c4-agent.md) | agent work and tool units on a machine outside the cluster                |
| [docs/c4-sync.md](c4-sync.md)                 | keeping a tracker and the farm in step                                    |
| [docs/diagnose.md](diagnose.md)               | diagnostic bundles and the farm's telemetry                               |
| [skills/c4-farm](../skills/c4-farm/SKILL.md)  | a skill for agents that operate the farm                                  |
| [PROGRESS.md](../PROGRESS.md)                 | branches, release blockers, proposed changes to the c4 suite              |

## Relation to C4, SMPTE ST 2114 and Scion

A C4 ID is a SHA-512 digest encoded as a 90-character string that starts with
`c4`, standardized as SMPTE ST 2114:2017, and in production use since before
the standard. Because the ID is derived from the
bytes, any holder of the bytes can check them. The operator imports the
[c4 Go module](https://github.com/Avalanche-io/c4) for IDs, c4m and its
stores, and reads and writes stores the c4 CLI uses.

Anthropic's [Scion](https://www.anthropic.com/engineering/redesigning-source-control)
showed what a content-addressed tree service does for a monorepo's CI, and
the operator's tier design follows its architecture. This project takes the
same structure to media pipelines, with trees named by C4 IDs, and one
difference in the monorepo case: Scion made source control server-side, while here git
stays where it is and its history is mirrored into C4 views (`import-git`,
push by push).

## Versioning

c4-operator follows semantic versioning; before 1.0, a minor version may
change the API, the manifests and the record formats. It builds against c4
master (`v1.0.17-0.20261021011131-b3a6f2dd9280`, commit b3a6f2d) and moves
to the c4 release that carries those APIs when it ships.

C4 IDs are SMPTE ST 2114, authored by Joshua Kolden and public since 2015.
This release continues a design that began as a 2011 cloud queue and a 2017
Kubernetes system.
