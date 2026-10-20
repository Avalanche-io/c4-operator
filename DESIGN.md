# c4-operator v2 design

Status: design of record for branch `v2`. The MVP on branch `mvp` is the
baseline. This document says what changes, why, and how each claim is
proven. Measured results are in `hack/e2e/out/results.md` and the summary is
in `REPORT-v2.md`.

This is The Cinema Content Creation Cloud, C4 for short (cccc.io). Its
identifiers, C4 IDs (standardized as SMPTE ST 2114, and in production use
since before the standard), name every file and tree by its content, and
they are what everything here is built on. This operator is the cloud
part of the name: a production farm whose data, caches, work and records
are all addressed by those identifiers.

## Goals

1. **Grab objects, do not re-check them.** As in a c4 store, a reader asks
   for an ID and uses the bytes it gets. Bytes are verified once, when they
   enter the system.
2. **Track what a job changes.** A job's writes become a changeset against
   the tree it was given: a c4m patch over a parent, with content-addressed
   pointers to new objects. Cost is proportional to the change.
3. **Very large render clusters.** Tens of thousands of nodes, and a render
   start in which thousands of nodes ask for the same plates in the same
   second.
4. **No single point of failure or overload.** No single NFS server, S3
   endpoint, cache replica, or node is on the critical path for everyone.
5. **Minimal duplication on disk.** Containers share read-only overlay
   lowers. Each container's private writes live in its own upper. A new
   version of a tree costs only its changes.
6. **Strong per-project isolation.** Knowing a C4 ID grants nothing.
7. **Provenance by construction.** Every task's inputs, outputs, software,
   place, time and submitter are recorded as signed, content-addressed
   records that trace any delivered frame back to its sources and forward to
   everything derived from it, including generative AI use (section 9).
8. **One farm, many kinds of work, scheduled to their objectives** (P5).
   Render, CI and ML share pools, quotas and capacity hooks. Priorities are
   derived from deadlines, latency targets and measured work instead of set
   by hand. A HUD serves artists, wranglers, developers and platform
   engineers.

The niche is media and VFX studios and mid-size shops, where one shared
farm runs deadline-driven rendering, CI, media processing and modest ML
(fine-tuning, LoRAs, studio-data training, generative batch jobs) over heavy
content-addressed data. Section 19 lists the non-goals, including the
training infrastructure that large labs build.

## Prior art: Scion

Anthropic described Scion, its replacement for Git, in
[Redesigning source control](https://www.anthropic.com/engineering/redesigning-source-control)
(2026-10-13). The tier design here follows Scion's architecture: one
daemon per node, strict cache tiers that answer the same requests,
top-down fetch by immutable ID, sparse checkouts, and commits whose cost
follows the change. The parts this design borrows:

| Scion                                                                                                       | Here                                                                                                              |
| ----------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| Changesets record the files changed relative to the first parent, with content-addressed blob pointers.     | A changeset is a c4m patch: the parent tree ID on the first line, then the changed entries (section 2.3).         |
| Fetch is top-down (root tree, then missing trees, then blobs), and every request names an immutable object. | The same. Root record, then the directory records the filter reaches, then file objects, each fetched by C4 ID.   |
| Each node runs one daemon on a local socket, shared by all jobs on the node.                                | One node daemon per node. It is also the CSI node plugin, and kubelet reaches it on the CSI socket.               |
| Tiers are strict (daemon, then regional cache, then server), and all tiers answer identical RPCs.           | The same tiers plus peer fetch inside a locality (rack or zone). One content API at every tier.                   |
| Each tier keeps a hit rate above 99% and holds no long-lived state.                                         | Caches are disposable local disk. Hit rates are exported per tier and are a HUD panel (section 11).               |
| Sparse and virtual checkouts: the client never requests trees it does not need.                             | `include` filters stop resolution at directory records. A lazy FUSE lower is designed in section 4.6.             |
| Directory-level ACLs.                                                                                       | Project grants on (tree, path prefix).                                                                            |
| Commits are server-side, with cost proportional to the change.                                              | Capture uploads only the changed objects, the directory records along changed paths, and one patch (section 4.4). |

The post's update of 2026-10-15 credits Mononoke, Sapling's server, and its
Bonsai data model for many of Scion's techniques (its footnote 2); they are
prior art for this design as well. REPORT-v2.md compares the measurements.

Scion is built for one code monorepo. This design applies the same
structure to media trees, where single objects are 10 MB to 10 GB rather
than 10 KB, and where outputs (rendered frames) dominate writes. Section 8.2
applies it back to source code through `oss/c4git`.

What C4 brings to that architecture: the trees are C4 views, every file
and tree named by its C4 ID (standardized as SMPTE ST 2114, and in
production use since before the standard) and described as c4m, so the
same names hold in any c4 store and tool outside the farm; and peer fetch
by rendezvous hashing inside a locality, so a node's cache misses go to
its neighbours before the regional tier.

## 1. Trust model

### 1.1 Verified once, where bytes enter

Bytes enter the system at exactly three places, and each computes the IDs
of what it writes:

- **Ingest.** `c4-operator ingest`, `c4-operator import-git`, or the c4 CLI
  (`c4 id -s`) hash content as they upload it. The store key is the
  computed ID.
- **Record publishing.** The controller computes the records of each view
  (section 2.2) and stores them under their computed IDs.
- **Capture.** When a container's writes are captured (section 4.4), each
  new file is hashed once.

Everything after that trusts the ID-to-bytes binding. Node daemons, peers
and regional caches do not rehash on fill or on read. The c4 store works the
same way: `TreeStore.Create(id)` writes under a caller-supplied ID.

### 1.2 Writers never choose the key

Verify-once has a weak point that the MVP did not have: if a component that
holds store write credentials is compromised, it can write wrong bytes under
an existing ID, and readers that do not rehash will serve them. The design
therefore keeps write credentials away from the large, exposed population of
node daemons:

- In tiered mode a node daemon holds **no store credentials at all**. It
  uploads captured objects with `PUT /` to the regional tier. The regional
  tier hashes the body itself, rejects it if it does not match the
  `X-C4-ID` the client claimed, and writes it under the ID it computed,
  only if the store lacks that ID.
- A compromised node can therefore add new objects (under their true IDs,
  which is harmless) but cannot replace an existing one.
- In direct mode (no regional tier, section 3.2) the node daemon holds the
  project's credentials and is trusted to the same degree as the regional
  tier. Direct mode is for small installations.

### 1.3 Why no per-read check is needed

1. **Immutability is structural.** Containers never reach cache objects.
   Lowers are read-only overlay layers. A container's writes go to its own
   upperdir, and overlayfs copies a file up before the first write. The MVP
   needed an mtime sentinel and a per-request inode check because pods
   mounted the hardlinked tree itself. With overlay, a pod cannot reach the
   object inode, so there is nothing to check per read.
2. **Transport integrity.** Tier-to-tier traffic is TLS in production.
   Length is checked on every fill. A short read is resumed with a ranged
   request, never accepted.
3. **Scrub.** An optional background scrubber on each tier rehashes cached
   objects at a configured byte rate, oldest first, and evicts any that do
   not match. A mismatch is a disk or software fault, reported as a metric
   and an event. It is off by default on nodes and on by default on the
   regional tier, whose objects are served many times.

What this gives up: a corrupt object in a regional cache is served to every
node that asks for it until a scrub finds it. The mitigations are TLS,
scrub, and the fact that any cache entry can be dropped and refetched.
`spec.store.verifyFill: true` on a Project restores hash-on-fill at the node.
Hashing while writing costs about 1 to 2 GB/s per core, which is free when
the network is slower than that.

## 2. Data model

### 2.1 Views and their identity

A **view** is the exact tree a container sees: directories, regular files
and symlinks, after the include filter and any grant paths. Its ID is the
C4 ID of its root record (section 2.2) under one projection:

- names, sizes, content IDs and symlink targets are real;
- timestamps are null everywhere;
- mode is null everywhere except on executable files, which record
  `-rwxr-xr-x`.

The c4 scanner's content mode nulls every mode. This projection differs only
for executable files, so **a tree without executable files has the same ID
as `c4 id` in content mode**, which is what media trees are. Source trees
need the executable bit, or a checkout of a repository would lose it and
`./configure` would fail. `c4-operator tree-id DIR` computes the projection
from a directory, and every e2e pod runs it against its own mount.

### 2.2 Records

A **record** is the one-level canonical c4m listing of a directory's direct
children under the view projection. Its bytes hash to the directory's ID, so
a record is an immutable, content-addressed object like any other: every tier
caches records exactly as it caches file content.

The controller publishes each view's records to the project store when it
resolves a Materialization (deepest first, root last, so an existing root
record means the whole view is present). Nodes then need nothing but the
view ID: they fetch the root record, then the records of subdirectories,
then file objects, all through the tiers. This is Scion's fetch order, and it
means 5,000 nodes resolving the same view hit their caches for records too.

### 2.3 Changesets and chains

A **changeset** is a c4m patch: a bare C4 ID on the first line naming the
parent view, then the changed entries, then a bare C4 ID naming the view the
patch produces. Additions and modifications are emitted with their new IDs,
removals as exact duplicates of the parent's entries, and directories along
changed paths with their new IDs. `c4m.ApplyPatch`, `c4m.ResolveBase` and
`c4 cat` read it unchanged, and the closing ID lets any reader check the
result.

A changeset is the one-section case of a **chain**: c4m's append-only patch
chain, in which a base is followed by patch sections, each sealed by a bare
C4 ID line that must equal the ID of the accumulated state above it (the c4m
whitepaper's "Version History"). The ID of a full c4m manifest is the ID of
its root listing, so every checkpoint in a chain is a view ID. A chain is
therefore a write-ahead log of views: appending a section and its checkpoint
is a commit, and every sealed prefix is a view that can be verified by
applying the sections along changed paths and comparing IDs, at a cost
proportional to the change. Section 2.5 makes chains the system's streaming
primitive.

The c4 CLI reads these chains as they are. `c4 split` works on a stored
chain unchanged. `c4 log` computes each version's ID from the chain text
alone, so a chain exported with its base listing written out in full lists
exactly the checkpoint view IDs (checked with c4 1.0.18); a chain stored with
an external base needs `c4 log` to resolve that base from the store, a
proposed c4 change (PROGRESS.md).

A Materialization whose source is a changeset or a chain resolves to its
sequence of views, base first (`status.layers` keeps the most recent 16 as
stacking candidates). Chains are not bounded by length; they are re-based
(section 2.5).

### 2.4 Layers

A **layer** is an immutable directory on a node that overlay stacks as a
lower:

- a **full layer** is a whole view, hardlinked from the node's object cache;
- a **delta layer** holds what changes from one view to another: added and
  modified files (hardlinked), removed paths as overlay whiteouts (character
  device 0/0), and directories whose contents were mostly replaced marked
  opaque (`trusted.overlay.opaque=y`).

A delta is computed by diffing the two views' records top-down and skipping
any subtree whose ID is equal, so its cost is proportional to the change. Any
two views can be diffed. The changeset chain only tells a node which views
are likely to be close (section 4.3).

### 2.5 Chains as streams

A chain grows while work runs, and anything that follows its tail sees each
sealed checkpoint as it lands. One mechanism serves five uses:

1. **Live capture.** With `capture: live`, the node daemon seals a
   checkpoint of a running pod's upper on an interval and when the task
   says so (a line in `.c4/seal`), and appends it to the unit's output
   chain. A file is sealed only once the task has closed it: the task names
   it in `.c4/seal`, or its size and modification time have not changed for
   one interval. The final capture at unpublish seals the rest. A render's
   output view grows frame by frame instead of appearing at exit.
2. **Pipelining.** A job whose input is `fromJob` follows the upstream job's
   output chain. The work queue makes a downstream unit runnable as soon as
   the frames it reads exist at a sealed checkpoint, so comp starts on
   rendered frames before the render finishes. With binary order (section
   10.8) the first checkpoints already span the whole shot.
3. **Subscribers.** Chains are served by a chain service (in the manager,
   sharded by chain ID when one replica is not enough) that is the single
   writer of each chain: unit chains are appended by their node daemon, and
   a job's chain folds its units' sealed sections in arrival order. Readers
   long-poll the tail (`GET /chains/{id}?after=<checkpoint>&wait=30s`) rather
   than poll. Nodes pre-warm downstream inputs, the HUD updates job views
   and the preview player, and agents watch jobs, all from the same stream.
   A subscriber needs to trust nothing but the checkpoints: it verifies each
   sealed prefix by applying the section to the previous view and comparing
   IDs.
4. **Git import.** Each branch is one chain, appended per commit, with the
   commit's view as the checkpoint: Scion's write-ahead log of changesets,
   expressed natively in c4m, readable by `c4 log` and `c4 split` (section
   2.3).
5. **Provenance and metering.** Each checkpoint is a natural boundary to
   sign the batch of task records (section 9) and usage records (section 12)
   that produced it. The chain of checkpoints is itself the tamper-evident
   audit log.

**Storage.** A chain is stored as segments: each segment is a changeset
whose first line is the previous checkpoint, so segments concatenate into
one valid c4m chain (a repeated boundary ID is read as one), and a chain
grows without rewriting any object. The chain's index (segment IDs in order,
and the head checkpoint) is append-only and kept by the chain service.

**Compaction by re-basing.** A chain is never cut off at a length. When a
chain grows long, the service starts a new chain whose base is the last
checkpoint (an external base on its first line), and the old chain stays
retained by ID. Readers of the new chain resolve the base view from its
records, so re-basing costs nothing but a new index.

## 3. Data path

### 3.1 One content API at every tier

Every tier answers the same HTTP API. It is the c4d content plane, extended
in a compatible way:

```
GET  /{c4id}   object bytes; honors Range (206) for resume
HEAD /{c4id}   200 if this tier can serve the object, 404 if not
PUT  /         body is content; reply is its C4 ID (regional tier and stores)

Request headers:
  Authorization: Bearer <projected service-account token, audience c4-tier>
  X-C4-Project:  <project>    cache partition and credential scope
  X-C4-Hop:      peer         marks a peer request (section 3.2)
  X-C4-ID:       <c4id>       on PUT: the ID the client computed
Response headers:
  Content-Length, Accept-Ranges: bytes
  Retry-After                 with 503 when shedding load
  X-C4-Served:   hit | fill | join   how this tier answered (metrics, tests)
```

**Does c4d's API fit?** The path shape and semantics do: `GET/HEAD /{c4id}`
and `PUT /` are c4d's content plane, so a c4d server can be an authoritative
store shard (`c4d://host:7433`) unchanged. c4d is not suitable as a cache
tier as it stands:

- It does not honor `Range`, so a transfer interrupted at 9 GB of a 10 GB
  object restarts at zero. The client handles that: a 200 in reply to a
  ranged GET means "start over".
- Its content plane authorizes any mTLS peer to read any ID, so it has no
  notion of a project. Isolation needs the partition header and a
  per-request authorization decision.
- On a miss served from a peer, it buffers the whole object in memory
  (`bytes.Buffer` in `handleGetContent`) before caching it. A 10 GB plate
  would need 10 GB of RAM.

These are additive changes to c4d, written up in PROGRESS.md.

### 3.2 Tiers

```
 pod --CSI--> node daemon ----> peer daemons (same locality) ----> regional cache ----> authoritative store
              one per node,     rendezvous home(s) per object      Deployment,           sharded: buckets,
              hostNetwork                                           stateless, homed      endpoints, c4d
                                                                    by rendezvous         servers, regions
```

The tiers are strict for reads: a node never reads the store when a regional
tier is configured, and in that mode it holds no credentials that would let
it. The store's read load is bounded by the number of regional replicas, not
the number of nodes. A Project can choose `access: Direct`, in which its nodes
read the store themselves. The herd test uses Direct as the baseline.

**Node daemon** (DaemonSet). It holds the node's object cache, builds layers,
mounts overlays for pods (it is the CSI node plugin), captures changesets,
and serves its cache to peers.

**Peer fetch** (inside a locality). A locality is a set of nodes with cheap
bandwidth between them: a rack or a zone, taken from the node label
`topology.kubernetes.io/zone` or a configured label. For each object,
rendezvous hashing over the locality's nodes, `score(node, id) =
mix(hash(node || id))`, picks the top `k` (default 1) as the object's
**homes**. A daemon that needs an object:

1. serves it from its own cache, or
2. if it is not a home, asks the homes in rank order, or
3. if it is a home, or every home failed, asks the regional tier.

A home that receives a peer request for an object it lacks fetches it from
the regional tier itself, coalesced with any other request for that object,
and streams it to the requester as it arrives. A non-home that receives a
peer request for an object it lacks answers 404 and does not fetch. Requests
never cascade from peer to peer.

**Regional cache** (Deployment). Its cache is local disk that can be lost at
any time. It scales horizontally. Clients rank replicas by rendezvous on
(project, id). The top `r` (default 2) are the object's regional homes, and a
client starts at the one chosen by hashing its own node name, so a hot object
is served by `r` replicas, not one. Each object is fetched from the store at
most `r` times per region per eviction.

**Writes.** Captured objects, records and changesets go to the regional
tier with `PUT /`, addressed to the object's regional home, which keeps a
copy in its cache: the next stage usually reads what the last one wrote.

**Authoritative store.** A project's store is a list of shards: S3 buckets,
S3 endpoints, c4d servers, or regions. An object lives on the shard with the
highest rendezvous score for its ID. Rendezvous is used rather than an ID
prefix modulo N because adding a shard then moves only 1/(N+1) of the
objects. No single endpoint carries all reads.

### 3.3 Fan-in arithmetic

A render start puts `P` pods on `N` nodes in `G` localities, all reading the
same `M` objects:

| tier                   | requests per object                              |
| ---------------------- | ------------------------------------------------ |
| pods to node daemons   | `P` (`P/N` per node, coalesced to 1)             |
| node daemons to homes  | `N - G·k` (each non-home asks its locality home) |
| homes to regional tier | `G·k`                                            |
| regional tier to store | at most `r`                                      |

With 5,000 nodes in 50 localities, `k = 1`, and `r = 2`, the store sees 2
GETs per plate frame instead of 5,000, and the regional tier sees 50. The P2
herd test measures each row on the test cluster.

### 3.4 Every tier coalesces, streams, sheds, and resumes

- **Singleflight.** Concurrent requests for the same (partition, id) at a
  tier share one upstream fetch.
- **Streaming fills.** The fill writes to a temporary file. Waiters read
  that file as it grows and are not held until the leader finishes. A
  three-hop path (store, regional, home, node) therefore pipelines instead
  of costing three full transfer times.
- **Admission control.** Each tier caps concurrent upstream fills and
  queued waiters. Beyond the queue it answers `503` with `Retry-After`. It
  never forwards the excess upstream.
- **Backpressure, not stampede.** A client that gets a 503 or a connection
  error tries the next candidate at the same tier (the next home, or the
  next regional replica). If every candidate refuses, it backs off with
  jitter and retries. It never skips a tier: a node never falls through to
  the store because the regional tier is busy. A regional outage slows nodes
  down but cannot multiply load on the store.
- **Resumable transfers.** On failure the client retries with
  `Range: bytes=<written>-`, against the same or another replica. Every
  replica serves identical bytes for an ID, so stitching across replicas is
  correct. A 200 in reply to a ranged request restarts the object.
- **Circuit breaking.** A peer or replica that fails is skipped for a short,
  growing interval. Membership is not changed by failures, so rendezvous
  homes stay stable while a peer restarts.
- **Bounded memory.** Objects stream to disk, never through memory.

### 3.5 Fetch order

Resolution on a node is top-down: the root record, then the directory
records of each level in parallel, then file objects, largest first, so the
long transfers start early.

### 3.6 Membership and discovery

- Node daemons run with `hostNetwork` and serve the tier API on the node's
  address. A daemon learns its locality from its own Node object and watches
  only the Nodes in that locality (a label selector, so the watch is bounded
  by the locality size). Rendezvous members are node names.
- The regional tier is a Service. Daemons watch its EndpointSlices (tens of
  endpoints). Members are pod names.
- Every component reads the same objects, so every daemon computes the same
  homes without coordination.

## 4. Overlay filesystems

### 4.1 Layout on a node

```
/var/lib/c4/
  p/<partition>/objects/<id[3:5]>/<id>     object cache; partition = project (Strict) or sharing group (Shared)
  p/<partition>/xobjects/<id[3:5]>/<id>    executable variant of an object (separate inode, mode 0777)
  p/<partition>/layers/<key>/              immutable layer trees, files hardlinked from objects/
  p/<partition>/layers/<key>.json          layer description and last use
  p/<partition>/vols/<volume>/upper        one per mounted volume
  p/<partition>/vols/<volume>/work         overlay workdir
  vols/<volume>.json                       volume state (project, pod, view, stack, capture)
```

Everything is on one filesystem, so lowers are hardlinks (`link(2)` returns
`EXDEV` across filesystems, and even across bind mounts). A file in a layer
costs a directory entry, not its bytes, and directories cost one inode each.

### 4.2 Permissions in the merged view

A writable view behaves like an `emptyDir`: directories are mode 0777,
regular files 0666 (0777 if executable), all owned by root, and every
timestamp is the epoch (c4m's null timestamp). Any container user can
therefore create, replace and edit files, and overlay copies a file up before
the first write, so the cached object is never touched. Objects are 0666 on
disk, but `/var/lib/c4` is 0700 and only the node daemon can reach it. Pods
never see a path into the cache.

### 4.3 Stacking policy

A node stacks only on what it already holds:

1. Look for the newest view in `status.layers` (the chain) for which the
   node holds a stack, and also the last view this node built for the same
   Materialization.
2. If one is found and the delta to the target is small (fewer changed
   entries than half the base's), mount `delta(base -> target)` over that
   stack. Each version adds one layer.
3. Otherwise build the target as one full layer.
4. A stack never exceeds 16 layers (overlay's mount options fit in one page,
   and lookups walk every layer). At the bound, the node builds a full layer
   for the target. This is compaction, and it is local to the node.

Shot v12 that is v11 plus three changed frames costs three hardlinks and a
few directories on a node that already has v11.

### 4.4 Upper layers and changeset capture

Each writable volume gets its own upperdir and workdir. Lowers are never
modified.

Capture runs at `NodeUnpublishVolume`, which kubelet calls when the pod has
terminated (its containers have exited, and volumes of terminated pods are
unmounted by default) or is being deleted, before the pod object is removed.
It:

1. unmounts the overlay, so the upper is quiescent;
2. walks the upper:
   - regular file: added or modified. It is hashed (the verify-once point).
     If its ID and executable bit equal the lower's, it was a metadata-only
     copy-up and is not a change;
   - character device 0/0: a whiteout, so the path was deleted;
   - directory with `trusted.overlay.opaque=y`: the lower subtree was
     replaced by the upper's;
   - other directories: merged; symlinks: added or changed;
3. hardlinks each new file into the node's object cache, so a later stage on
   this node reads it without a fetch;
4. computes the output view by rewriting only the records along changed
   paths (the parent's records come from the node cache), and computes the
   changeset c4m;
5. uploads new objects, new records and the changeset with `PUT /`;
6. records the result on the pod as annotations,
   `c4.avalanche.io/output-tree`, `c4.avalanche.io/output-changeset` and
   `c4.avalanche.io/output-changes` (`added=N modified=N deleted=N
   bytes=N`), and as metrics.

With `capture: live`, the same walk runs while the pod runs and seals
checkpoints into the unit's output chain (section 2.5); the capture at
unpublish then seals only what is left.

Capture runs detached from the RPC and is keyed by volume. If kubelet's call
times out, its retry joins the capture in flight. If the daemon restarts, the
upper is still on disk and the retry starts over, which is safe because every
step is idempotent. The volume is not released until its outputs are stored,
so a pod's outputs are never lost to a slow upload. An empty upper produces
no changeset.

Cost is proportional to the change: a render that writes 200 new EXRs
uploads 200 objects, a few records and one patch, whatever the size of the
input tree.

### 4.5 Read-only volumes

A volume with `readOnly: true` gets no upper: a read-only overlay of its
stack, or a read-only bind mount when the stack is one layer. Nothing is
captured.

### 4.6 Lazy lower (designed, not built)

For trees where jobs read a small fraction (a 25 GB source tree in which a
test touches 300 files), materializing every object up front wastes the
fetch. The lazy option is a FUSE filesystem served by the node daemon. It
presents the view from its records and fetches an object through the tier
client on first `open`. Overlay mounts it as the bottom lower, so writes go to
the per-container upper and capture works unchanged. Costs: one FUSE round
trip per `open` (reads can use FUSE passthrough on kernels that have it), a
daemon that must hold the FUSE session, and recovery for a daemon restart
under live mounts. A Materialization would choose `materialize: Eager |
Lazy`. Eager stays the default for render inputs, which are read whole.

## 5. Mounting: a CSI node driver

The MVP's init container and hostPath volumes are replaced by a CSI driver,
`c4.avalanche.io`, with ephemeral inline volumes:

```yaml
volumes:
- name: assets
  csi:
    driver: c4.avalanche.io
    readOnly: false
    volumeAttributes:
      materialization: sh010
      capture: "true"
```

- Pods under Pod Security `restricted` may use `csi` volumes. hostPath is
  gone from user pods, and the e2e enforces `restricted` on its namespaces.
- All privileged work (mounting overlays, creating whiteouts, writing the
  cache) happens in the node daemon, which runs privileged in `c4-system`.
- The `CSIDriver` object sets `podInfoOnMount: true`,
  `volumeLifecycleModes: [Ephemeral]`, `fsGroupPolicy: None` (kubelet must
  never chown a view recursively, which would copy every file up), and
  `tokenRequests` with audience `c4.avalanche.io` (section 6.2).
- `NodePublishVolume` authenticates the pod, resolves its Materialization,
  makes the layers present, and mounts the overlay at the target path. It is
  idempotent. Fetching runs detached from the RPC, so if kubelet's call
  times out on a 200 GB cold fetch, the retry joins the work in flight. The
  pod stays `ContainerCreating` until the tree is complete. Missing content
  fails the publish, and the workload never starts.
- `NodeUnpublishVolume` captures (section 4.4) and unmounts.
- The mutating webhook stays as a convenience. It turns the MVP's
  annotations into the `csi` volume and mounts, and nothing more.

## 6. Per-project isolation

### 6.1 Projects

A `Project` is cluster-scoped and owned by cluster administrators:

```yaml
apiVersion: c4.avalanche.io/v1alpha1
kind: Project
metadata: {name: show-a}
spec:
  namespaces: [show-a-render, show-a-comp]
  isolation: Strict                   # Strict | Shared
  store:
    shards: ["s3://show-a/c4/?endpoint=http://minio-0...", "s3://show-a/c4/?endpoint=http://minio-1..."]
    credentialsSecretRef: {name: show-a-store}   # a Secret in c4-system
    access: Tiered                    # Tiered | Direct
  scope: Grants                       # Store (default): anything in the project's own store
  grants:                             # with scope Grants, only these; none means nothing
  - tree: c4...                       # a view, root record, source c4m, or changeset ID
    paths: [shots/sh010/, editorial/]
  work:                               # P5: quotas per pool, budgets
    quotas: [{pool: cpu, nominal: 40, borrowingLimit: 40}]
    burstBudgetPerDay: "800"
```

A namespace belongs to at most one Project. A Materialization belongs to the
Project that lists its namespace. Users choose neither the store nor the
credentials.

### 6.2 Identity and authorization

- **Pods.** `NodePublishVolume` receives the pod's projected token (audience
  `c4.avalanche.io`). The daemon validates it with TokenReview and checks
  that the token's namespace, pod name and pod UID match the volume context.
  kubelet already overrides any user-supplied `csi.storage.k8s.io/*` keys,
  and the token check makes that a verified fact rather than an assumption.
- **Tiers.** Tier requests carry the caller's own projected token (audience
  `c4-tier`). The server validates it with TokenReview, caches the result
  until the token expires, and accepts only the node daemon and regional
  service accounts in `c4-system`, plus the HUD's for reads (it fetches
  preview proxies, section 11.4, after checking the viewer's access to that
  project). A pod that calls a node daemon or a regional replica directly is
  refused, with or without a token.
- **Knowing an ID is not permission.** Pods cannot fetch by ID at all. They
  can only mount Materializations in their own namespace. A Materialization
  resolves through its project's store with its project's credentials, and
  the manager checks that every object of the resolved view is in that
  store (the preflight) before any node is involved:
  - with scope `Store`, any description that resolves from the project's
    own store is allowed. A hand-written ConfigMap c4m that lists another
    project's IDs resolves against this project's store, where those objects
    do not exist, and the Materialization fails;
  - with scope `Grants`, the source (or, for a changeset, its base) must be a
    granted tree, the include filter is intersected with the grant's paths,
    and an include that the grant's paths do not reach is refused rather
    than mounted empty. A grant names a tree, not files: a c4m that lists
    exactly the granted files is a different tree and is refused. Removing
    the last grant leaves the project able to read nothing, never
    everything; listing grants under scope `Store` is reported as invalid
    and the grants are still enforced.
  - Records of a view are written to the project's store only after its
    preflight passes, so a refused description leaves nothing behind.
  - A change to a Project (a revoked grant, new shards) re-resolves every
    Materialization of its namespaces, and node daemons refuse to mount a
    Materialization resolved under an older Project generation. Revocation
    therefore takes effect at the next mount; pods already running keep
    their mount until they end (P3 measures both).
- **Writes the daemons make to the API.** Node daemons annotate pods with
  capture results and label their own Node when its cache is warm (section
  9.8). RBAC cannot scope those verbs, so a ValidatingAdmissionPolicy does:
  the node daemon's service account may change only
  `c4.avalanche.io/output-*` annotations, only on pods bound to its own node
  (from the token's `authentication.kubernetes.io/node-name`), and only the
  `c4.avalanche.io/` labels of its own Node.

### 6.3 Strict mode: no shared caches

In `Strict` mode (the default), each project has its own object cache, its
own layers on every node, and its own partition at every cache tier (the
project is part of the cache key, and upstream fetches use the project's
credentials). No byte is shared across projects, even when the content is
identical.

**Why not deduplicate across projects?** A shared content-addressed cache is
an oracle. If project B can make a node fetch ID X and time the result, a
fast answer reveals that someone on that node or in that region recently used
X. Because IDs are hashes of content, B can test guesses: render a candidate
frame of a competitor's unreleased shot, compute its ID, and probe. That is
a confirmation-of-file attack, and it needs no read access to A's data. Even
without timing, a cache that answers B's request from A's fill leaks A's
bytes if B's store lacks them. Partitioning removes both channels.

### 6.4 Shared mode

`Shared` trades this away deliberately for projects that trust each other,
such as departments of one show. Projects in the same `sharingGroup` share
objects and layers. Project B is served a shared object only as part of
one of B's own Materializations, whose preflight has already found every
object of the view in B's own store (proof of possession), so B never reads
bytes it could not have read anyway. Node daemons, the only clients of the
tiers, fetch nothing else. The timing channel remains, and is documented.

### 6.5 Credentials

| component       | Tiered project           | Direct project           |
| --------------- | ------------------------ | ------------------------ |
| node daemon     | none                     | read and write           |
| regional tier   | read and write           | none                     |
| manager         | read and write (records) | read and write (records) |
| user namespaces | none                     | none                     |

Store credentials live in Secrets in `c4-system`, referenced by Projects, and
are read through a Role in `c4-system` only. That fixes the MVP's cluster-wide
Secret read. The MVP's agent socket is gone: kubelet reaches the driver on
the CSI socket, and every publish is authenticated with the pod's own token.

### 6.6 Encryption with just-in-time keys (designed)

A project with `spec.keys.kms` set is encrypted end to end. Plaintext exists
only on a node that is running one of the project's tasks, and only while it
runs.

**Format.** An object keeps its plaintext C4 ID, and its store key is still
that ID. Its bytes are a header (format version, key epoch, random salt)
followed by AES-256-GCM ciphertext in 1 MiB chunks. Each object has its own
key, `HKDF(epoch key, salt || ID)`, so chunk nonces are simple counters, a
ranged read decrypts from any chunk, and equal plaintexts in two projects are
different ciphertexts. Records and changesets are encrypted too: file names
reveal titles and shots.

**Key hierarchy.** Each project has a key-encryption key in the customer's
KMS (AWS KMS, Cloud KMS, or Key Vault, HSM-backed where the provider offers
it). Epoch keys are generated by KMS (`GenerateDataKey`), stored only in
wrapped form in the project's store, and rotated by schedule. No plaintext
key is ever written to disk or to the Kubernetes API.

**Just-in-time release.** A key broker (in the manager) releases keys to a
task, not to a machine:

1. At `NodePublishVolume`, the node daemon asks the broker for the epoch keys
   of the view it is mounting. It presents its own service-account token and
   the pod's token, and an ephemeral public key generated for this volume.
2. The broker validates both tokens with TokenReview, checks that the pod is
   bound to that node, is not finished, belongs to a namespace of the
   project, and mounts a Materialization the project's grants allow. It
   unwraps the epoch keys with KMS, passing an encryption context of
   project, epoch, node and pod, so the provider's own audit log (CloudTrail,
   Cloud Audit Logs, Azure Monitor) records every unwrap against that task.
3. It returns the keys sealed to the volume's ephemeral key, with a lease no
   longer than the pod's active deadline or the project's maximum. The
   daemon holds them in locked memory, renews the lease while the pod runs,
   and zeroes them when the volume is unpublished.
4. Every release, renewal and refusal is appended to an audit log that is
   hash-chained like the usage ledger (section 12): time, project, epoch,
   pod, node, service account, view, lease, decision and reason.

**Where bytes are plaintext.** The regional tier and peers carry ciphertext
and need no keys. The consuming node decrypts at fill, writes plaintext
objects for its layers, and verifies each object's plaintext hash while
decrypting, at no extra pass. That check replaces "writers never choose the
key" (section 1.2) for encrypted projects: a node that holds a task's keys
encrypts that task's captured outputs itself, so the regional tier cannot
hash them, and readers verify instead. With `keys.retainPlaintext: false`, a
node deletes the project's layers and plaintext objects as soon as no volume
of the project uses them, trading cache warmth for exposure time.

**Revocation.** Disabling the project's KMS key stops new releases at once.
The broker can refuse renewals for a project, a node or a pod. Node daemons
watch the broker's revocation feed, drop the keys of revoked leases, and
unmount and purge the affected volumes. Keys already in a running task's
memory are revoked by ending the task, which the broker can request.

**Why this strengthens isolation.** Sections 6.2 to 6.5 keep project B from
reaching A's bytes through this system. Encryption keeps A's bytes safe from
everything else: a misconfigured bucket policy, a stolen store credential, a
copy of a regional cache disk, or a peer that relays A's objects all yield
ciphertext. Keys follow task identity, so a node holds a project's keys only
while it runs that project's work.

### 6.7 Content security expectations (MPA, TPN)

Studios assess vendors against the MPA Content Security Best Practices
through the Trusted Partner Network (TPN). The design is meant to make those
assessments straightforward; it does not certify anything by itself.

| expectation                       | how the design meets it                                                                                                               |
| --------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| segregation of client content     | a store, cache partitions, layers and keys per project; strict mode shares nothing, not even identical bytes                          |
| encryption at rest and in transit | AES-256-GCM objects and records in the store and every cache tier; TLS between tiers; plaintext only on a running task's node         |
| key management                    | KMS-held key-encryption keys, envelope encryption, rotation by epoch, revocation, separation between KMS administrators and operators |
| least privilege and need to know  | keys released per task and per granted view, for the task's life; pods cannot fetch by ID; grants limit paths                         |
| logging and monitoring            | hash-chained key-release log, KMS provider audit trail joined by encryption context, usage ledger                                     |
| asset tracking                    | every release names the exact view, so "which task could read which frames, and when" has an exact answer                             |

### 6.8 Trust boundary

Pods are untrusted. Node daemons and regional caches are trusted
infrastructure: they run privileged or hold credentials, and whoever owns a
node already owns what is mounted on it. Within that, node daemons are the
large, exposed population, so they hold the least: no store credentials in
tiered mode, and API writes limited by admission policy.

## 7. Very large clusters

- **The control plane is not in the data path.** The controller resolves a
  Materialization once per generation (IDs, totals, records, store
  preflight) and writes one status. Nodes and caches talk to each other and
  to the store, never through the API server.
- **No per-node or per-pod status in custom resources.** At 10,000 nodes,
  per-node entries would make every Materialization a hot, megabyte-sized
  etcd object. The MVP's `status.nodes` is removed. Behavior is exported as
  Prometheus metrics (hits, misses, coalesced waits, shed requests, upstream
  fetches and bytes, publish latency, capture counts) and aggregated by the
  metrics stack. The only per-pod write is the capture annotation on the pod.
- **Bounded watches.** A node daemon watches the Nodes of its locality, the
  regional tier's EndpointSlices, and Projects (cluster-scoped and small).
  It reads a Materialization only when a pod mounts it, with
  `resourceVersion=0` so the API server answers from its watch cache, and
  caches it briefly. A Materialization change wakes the controller, not
  10,000 nodes.
- **Eviction under pressure.** Each node has a byte budget. Layers that no
  mounted volume uses and whose retention has passed go first, least recently
  used first, then objects no layer links (link count 1), least recently
  used first. A layer under a live mount is never removed. The regional tier
  evicts by LRU only.
- **Node churn.** When a node leaves a locality, rendezvous moves only the
  objects homed on it (1/n) to new homes, which refetch them from the
  regional tier on demand, coalesced. A joining node starts empty. Peers that
  ask it early get 404s or coalesced waits, never cascades.
- **Regional churn.** Losing a replica moves its objects to the next replica
  in rank order, which refetches each one at most once. Clients resume
  interrupted transfers there.

## 8. Use cases

### 8.1 Render farms

Plates (EXR sequences), USD stages and layers, caches (Alembic, VDB),
textures, and model weights. Many pods read one tree whole, at the same
moment, and write new frames.

- Inputs: a Materialization per shot (`include: shots/sh010/`, `assets/`),
  eager layers, homed per locality, one layer per node shared by every pod
  on it.
- Outputs: written into the mounted tree (for example
  `shots/sh010/render/`), captured per pod as a changeset. A downstream comp
  job mounts the output view, or the input stacked with the changeset.
- Versioning: "v12 = v11 + 3 changed frames" is a changeset. Nodes that had
  v11 add a three-file delta layer.

### 8.2 Monorepo CI (Scion's case)

A repository imported into C4. Each commit is a view, and each commit's diff
against its first parent is a changeset.

**The one place this differs from Scion.** Scion made source control itself
server-side: commits are created on the server, and git is gone. This system
keeps git as the system of record and mirrors it into C4 views. Studios and
their developers keep their tools, hosting, review and hooks; the price is an
import step, so import must be fast and push-driven.

- **Push-driven import.** A webhook from the git host (or a `post-receive`
  hook on a self-hosted remote) names the new commits. `c4-operator
  import-git` imports only those, applying each commit's `git diff-tree`
  changes to its parent's view with the same code capture uses, so the cost
  of a commit is proportional to its change, and publishes the view. Blob
  content is hashed once and remembered by git object ID. P4 measures
  push-to-view latency as well as per-commit cost. Each branch is one chain
  (section 2.5), appended per commit, so `c4 log` lists a branch's commits
  as views.
- **c4git files** are 90-byte C4 IDs in git. The importer uses the ID
  directly, copies the object from the c4git store without rehashing (c4git
  verified it at `git add`), and takes its size from the store.
- **API-only CI.** Many CI decisions need no checkout. A query on the tier
  API and the MCP server returns the changed paths, objects and sizes between
  any two views, and the merge-base view of a commit against a branch. It
  walks directory records top-down and skips every subtree whose ID is equal
  (the same diff nodes use for delta layers), so its cost is proportional to
  the change, not to the repository. A pipeline decides which test shards to
  run before any pod mounts anything; test discovery (section 10.6) is often
  just this query.
- **Sparse, stacked checkouts.** A CI job that does need files mounts (commit
  view or changeset, `include: services/foo/, libs/common/`). A node that ran
  CI for the parent commit adds one thin delta layer for the child commit.
- Build outputs captured from the upper become the input of the next stage.

The lazy lower (section 4.6) matters most here, because tests read a small
fraction of a large tree.

## 9. Provenance

A production tracker records what people say happened. This system can
record what ran, because it is the thing that ran it: it mounted each
task's inputs by tree ID, captured its outputs by tree ID, and knows the
container images, the node, the project and the submitter. Lineage is a
byproduct of execution, not a form someone fills in. That is the reason
provenance is a core capability here and not an add-on.

### 9.1 Task records

Every unit of work (section 10.4) that finishes produces one record:

| field                 | source                                                                                                                                                  |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| inputs                | the view IDs the unit mounted (from the node daemon's volume state)                                                                                     |
| outputs               | the output view and changeset IDs (from capture)                                                                                                        |
| software              | container image digests (from the pod's status), software versions and the class parameters                                                             |
| parameters            | the job's parameters and the unit's range, as submitted                                                                                                 |
| where and when        | pool, node class, node, start and finish times, exit status, attempts                                                                                   |
| who                   | the project, and the identity that submitted the job, recorded at admission from the API request and protected from later change by an admission policy |
| keys                  | the key releases (section 6.6) that let the unit read encrypted content                                                                                 |
| generative and ML use | model weights, LoRAs and training-data trees by view ID; prompts and parameters by hash (section 9.4)                                                   |

The node daemon gathers the execution facts at capture, outside the pod:
the views it mounted from its own volume state, the image digests from the
pod's status, the job and unit from the pod's labels. It signs the record
as its node (section 9.2), stores it in the project's store and names it
on the pod; the queue indexes it by output file.

**Format.** Records are in-toto Statement v1 in a DSSE envelope, the format
the software supply chain already verifies. The predicate type is
`https://cccc.io/provenance/task/v1`, modeled on SLSA build provenance
(`buildDefinition` with external and internal parameters and resolved
dependencies, `runDetails` with the builder and run metadata), with the
production context and generative fields added. Subjects name outputs by
digest: a file by `sha512` (the hex SHA-512 that is its C4 ID, which generic
in-toto tooling matches) and `c4`; a tree by `c4` only, since a tree ID is
not the SHA-512 of one byte stream. CI tasks that build software artifacts
also emit standard SLSA provenance (`https://slsa.dev/provenance/v1`), so
existing verifiers work unchanged. Records are generated by the platform
and signed with keys no task pod can reach, which is what SLSA Build level 3
asks of a build platform. The claim is SLSA Build L3-style
provenance for compute tasks; SLSA (v1.2) has no data or media track, so
there is no SLSA compliance claim for media.

### 9.2 Signers, storage and verification

The provenance of a piece of work is the set of records that each entity
which did or decided part of it signed itself. No farm key signs for
everyone, so every record names who vouches for it, and the chain of
custody crosses signers.

| record     | predicate (`https://cccc.io/provenance/...`) | signed by                                                                                                                             | identity (`https://cccc.io/id/...`) |
| ---------- | -------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------- |
| task       | `task/v1`                                    | the node daemon that ran and captured the unit                                                                                        | `node/<node>`                       |
| resolution | `resolution/v1`                              | the controller, for the view a Materialization resolved to, under which grant, with every object present                              | `component/controller`              |
| decision   | `decision/v1`                                | the queue, for each preemption or yield it decides, and for whom                                                                      | `component/queue`                   |
| job        | `job/v1`                                     | the queue, when a job ends: its output view, chain and record index, and the decision and control records that touched it             | `component/queue`                   |
| control    | `control/v1`                                 | the HUD, for each action a person or agent takes through the HUD or MCP: the actor, how they signed in, the client, the authorization | `component/hud`                     |
| import     | `import/v1`                                  | the importer's workload identity, one per git commit (the commit, its view and its changeset)                                         | `sa/<namespace>/<name>`             |
| ingest     | `ingest/v1`                                  | the ingesting workload's identity, for the view it hashed                                                                             | `sa/<namespace>/<name>`             |
| submission | designed                                     | the person or agent who submits or approves work, with their own key                                                                  | `user/<name>`                       |

**Identity: short-lived certificates from a farm signing CA.** Each signer
makes its own Ed25519 key, which never leaves its process, and obtains a
certificate binding the key to its identity (a URI in the certificate),
valid for a day for a node and an hour for a workload, renewed at half its
life. The issuer runs in the manager (`POST /v1/certificate` behind the
`c4-signer` Service). A signer proves who it is with a projected
service-account token for the audience `c4-signer`, checked with
TokenReview. A node daemon's token is bound to its pod and names its node,
so the issuer certifies the daemon's key as that node and no other; any
other service account is certified as itself; the manager's and the
regional tier's accounts are refused (the controller, the queue and the HUD
sign with certificates the manager issues to itself in-process). People
are not certified by this issuer.

The CA key is a Secret in its own namespace, `c4-signing`, which only the
manager may read; the Role that lets node daemons and the regional tier
read store credentials in `c4-system` does not reach it. The CA
certificate is published in the farm's trust bundle (ConfigMap
`c4-provenance`, key `ca.crt`), to which roots are added and never removed,
so records signed under an earlier root still verify.

This is the shape of Sigstore's keyless signing (Fulcio certificates
issued against an OIDC token), self-hosted and without a public log, so it
runs offline inside a studio's security boundary.

**Bundles.** A record is an attestation bundle
(`application/vnd.cccc.attestation.v1+json`): the DSSE envelope and the
signer's certificate chain, shaped like a Sigstore bundle without a
transparency-log entry. The predicate carries `signedAt`. Verification
checks the chain to the farm's roots at that time, then the signature, then
the signer policy, and stores nothing but the bundle: each record is a C4
object in the project's store, so its ID commits to its content.

**Policy.** A task record must be signed by the node it says it ran on;
resolutions by the controller; decisions and job records by the queue;
controls by the HUD; imports and ingests by a workload identity of the
project whose store holds the content. A record that verifies but fails the
policy is reported as such, so a node cannot speak for another node or for
the queue.

**Links.** A Materialization may name the record of its source (annotation
`c4.avalanche.io/source-record`): the controller verifies it before
resolving, refuses a mismatch or another project's workload, and links it
from its resolution record. The queue names the upstream job's record on
the Materializations of `fromJob` inputs, and waits for it. A job's record
lists its decision and control records, and the HUD writes each control
record's ID onto the object in the same write as the change. The chain of
custody (`GET /api/v1/custody/{ns}/{name}?path=`, MCP `c4_verify_custody`)
follows these links from a file back to its ingest or import and verifies
every record against the roots and the policy. A step without a record (a
source ingested with signing off) is reported as a gap, not a failure.

**Choices.**

- One farm key for every record (what P7 first built): any holder can sign
  as anyone, and a record names no signer. Records signed that way still
  verify against the legacy keys in the trust bundle and are reported as
  signed with a farm key.
- Per-node keys provisioned from a farm CA: long-lived keys on node disks,
  with manual rotation and revocation.
- A KMS key per component: strong custody, but a KMS call per record and no
  per-node identity without a key per node.
- Public Sigstore (Fulcio and Rekor at sigstore.dev): workload identities
  and content digests of unreleased productions would go to a public log,
  which TPN and MPA content security rules out.
- **Built:** per-entity short-lived certificates from a farm CA against
  Kubernetes service-account tokens. In production the root key lives in a
  KMS or HSM and signs only an intermediate the manager holds. A private
  Sigstore (Fulcio and Rekor inside the studio) is the upgrade path when an
  independent timestamp and an append-only log are wanted.

**Designed, not built.** `signedAt` is the signer's own claim; an RFC 3161
timestamp authority or a private Rekor makes it independent. Revocation is
by short lifetimes, with no revocation list. People act through the HUD
(the HUD signs that it authenticated and authorized them); certificates for
people from the studio's OIDC provider, so an artist or agent signs a
submission or approval with their own key, are designed. Node disable and
enable are recorded as Events only, for want of a farm-level audit store.
Records are not yet batched at chain checkpoints (section 2.5).

**Storage and use.**

- An index (by output file, output view, input view, job, model and dataset)
  is derived data, rebuildable from the records. Today the queue keeps one
  index per job (output file and unit to task record), and the transition
  log (`GET /book?since`, integrations GAPS 5): job transitions numbered
  farm-wide by the queue, sealed in each project's store as hash-linked
  segments, the head in ConfigMaps (`c4-work-book` for the number, one
  `c4-work-book-<project>` per project, so none grows with the farm); the
  farm-wide index,
  kept in the installation's system store as a hash-chained log like the
  usage ledger (section 12.2), is designed.
- Usage records (section 12) and license usage (section 16.2) reference the
  task records they come from, so the same signed records are the audit
  trail for content, for keys, for billing and for licenses.

### 9.3 The lineage graph

The graph has three kinds of node (trees, files, task records) and four
kinds of edge: a task reads a view, a task writes a changeset and output
view, a view contains a file, and a view derives from its parent view
through a changeset.

- **Backward**: from a delivered frame (its C4 ID, or a path in a delivery
  view) to the record whose output contains it, to that task's inputs, to
  the records that produced those, back to the ingested plates and assets:
  delivery, transcode, comp, render, sim, source.
- **Forward**: from a source (a plate, a texture, a performer scan) to every
  task that read a view containing it, and onward to everything derived:
  what must be redone if a plate is recalled or a license ends.
- Records are small and the index is keyed by ID, so a trace touches only
  the records on the path, never a scan of the farm's history.

### 9.4 AI usage disclosure and consent

- Generative and ML work classes declare their model inputs, and the
  records keep them: weights and LoRAs by view ID, training-data trees by
  view ID, prompts and parameters by hash, with the plaintext prompt stored
  under the project's access control (a prompt can be confidential script
  material). A training run's record links its training data to the
  checkpoint view it produced, so "trained on what" is a graph query.
- **One query** answers "which shots in this delivery used generative AI,
  which models, trained on what": a backward traversal from the delivery
  view, filtered to records with generative fields, joined to the training
  records of those models.
- The questions the records are built to answer come from current
  agreements and policies (sources and verification status in
  `docs/research/provenance.md`):
  - WGA 2023 MBA: was any material given to a writer generated by AI?
  - WGA 2026 MBA and DGA 2026: was a writer's or director's work licensed to
    train a commercial generative system (a notice trigger)?
  - SAG-AFTRA 2023 and 2026: does an output contain a digital replica of an
    identifiable performer, under which consent, and was a synthetic
    performer used?
  - IATSE Basic Agreement 2024: which AI systems did covered employees use
    on which tasks, and was the use disclosed to the producer?
  - DGA 2026: which AI-generated footage exists, and who approved it?
  - Streamer guidance (for example Netflix's 2025 guidance for partners):
    every generative use per title, and the approval behind each one.
- **Consent as an input.** A consent record (a performer's consent for a
  digital replica, scoped to a project) is a signed object like any other.
  A project can require that a generative task using a replica model
  reference a consent record, and admission refuses the task otherwise.
- **Human authorship.** The US Copyright Office's 2025 report on
  copyrightability holds that prompts alone do not give sufficient human
  control, and registration requires disclosing AI-generated material and
  describing the human contribution. The graph keeps human-authored inputs,
  human selection among generated candidates, and human modifications as
  distinct records from prompt-only generation. It supplies evidence for a
  case-by-case determination; it does not prove copyrightability.
- **Regulation.** EU AI Act Article 50 transparency duties apply from
  2 August 2026, and California's AI Transparency Act (SB 942 as amended)
  requires latent disclosures from covered providers. A studio that runs
  its own generative models may itself be a provider. The fields those
  disclosures need (provider, system and version, time, identifier) derive
  from the records; whether they apply is a question for counsel.

### 9.5 Standards

- **C2PA (2.4).** Deliverables get Content Credentials derived from the
  graph: `c2pa.created` with a `digitalSourceType` of
  `trainedAlgorithmicMedia` for fully generated media,
  `compositeWithTrainedAlgorithmicMedia` with `c2pa.placed` for comps with
  generated elements, the `c2pa.ai-disclosure` assertion, and the CAWG
  `cawg.training-mining` assertion for training permissions. Embedding is
  defined for MOV (ProRes in MOV) and MP4 through BMFF hashing, WAV and PDF.
  OpenEXR sequences and MXF have no defined embedding, so they get a sidecar
  `.c2pa` manifest whose `c2pa.hash.collection.data` binds every frame file.
  The system writes C2PA manifest definitions and leaves signing and
  embedding to the C2PA SDK and its `c2patool`; credentials show as
  "Trusted" only with a certificate on the C2PA Trust List, which needs the
  C2PA conformance program.
- **MovieLabs.** C4 references every media element by its content ID,
  independent of where the bytes live; MovieLabs' 2030 Vision asks for the
  same, that "individual media elements are referenced, accessed, tracked and
  interrelated using a universal linking system". Records export as Ontology
  for Media Creation (v2.8) entities: Task, Participant, Infrastructure and
  Asset, with `provenance.CreatedBy`, `createdOn`, `Role` and `Origin`,
  versions as
  `DerivationOf`, `RevisionOf` and `VariantOf`, and C4 IDs as identifiers
  with scope `c4`.
- **ASWF.** OpenAssetIO can resolve entity references to view IDs, and
  OpenTimelineIO media references can carry them. OpenEXR has no
  standardized C2PA carriage yet, hence the sidecar.
- **Trackers.** ShotGrid (Flow Production Tracking) published-file links and
  ftrack version links are fed from the graph (section 15.2). The trackers
  stay the system of record for production; this is the signed,
  content-addressed layer underneath them.
- **Prior art.** Tekton Chains signs SLSA provenance per task on
  Kubernetes; in-toto Witness and GUAC collect attestations into graphs;
  OpenLineage and Marquez track job and dataset lineage; cameras from Leica,
  Sony and Google sign at capture with C2PA. What is particular here is the
  combination: content-addressed trees as the subjects across render, CI,
  ML and generative work, production context in signed records, and
  disclosure queries shaped by the industry's agreements.

### 9.6 HUD and MCP

- The HUD's provenance view starts from any frame: a box in the job view, or
  a file in an output view. It draws the lineage graph back to sources and
  forward to derivatives, each node a record with its verification status,
  with generative steps marked, and produces the disclosure report for a
  delivery.
- The MCP server exposes the same queries: `c4_trace_file` and
  `c4_verify_custody` (built), and `c4_trace_forward`, `c4_verify_record`,
  `c4_disclosure_report`, `c4_export_c2pa` and `c4_export_omc` (designed,
  section 13.2).

## 10. Work system (P5)

### 10.1 Scope

One farm runs several kinds of work: deadline-driven rendering, CI over a
source monorepo, media processing, and modest ML. They share nodes, data and
budgets, and they compete for the same capacity on the same days. The work
system is one queue and one scheduler for all of them. What differs between
kinds of work lives in **work classes** (section 10.4). Render, CI and ML are
the classes built here.

### 10.2 The queue model

The queue is a controller in the manager. It holds **units** of work (a
chunk of frames, a test shard, a pipeline step, a training run) and
**releases them into the cluster only when they can run**, in an order
derived from each job's objective. kube-scheduler places each pod on a node.
This is admission-style queueing, the division of labor Kueue uses: the queue
decides what runs next and how much, the scheduler decides where.

Releasing units only when capacity is free matters for three reasons.
Changes of order take effect immediately, because nothing waits as a pending
pod with a frozen priority. The cluster never holds 100,000 pending pods.
Capacity hooks see a clean signal: pending pods mean real shortage, not queue
depth.

**As built** (`internal/work/queue`). The queue runs under leader election and
ticks every two seconds: it reads Pools, Projects, WorkJobs and its own unit
pods from the manager's cache, builds a snapshot, calls the policy
(`sched.Decide`, a pure function), and carries out the decisions: create
pods for released units, ask units to yield or stop them, write
CapacityRequests, write each job's status. Every pool node advertises its
slots as an extended resource, `c4.avalanche.io/slots` (as a device plugin
advertises devices), and every unit pod requests its unit's slots, so
kube-scheduler places units by slot exactly as the queue counts them. A unit
pod has a generated name, an owner reference to its job, the pool's node
selector and tolerations, the job's inputs as C4 CSI volumes, and the unit's
environment (`C4_JOB`, `C4_NAMESPACE`, `C4_UNIT`, `C4_FRAMES`, `C4_SHARD`, `C4_STEPS`..., and `C4_OUTPUT`, the mount path of the first writable input, where the unit writes what is captured). A unit pod holds no Kubernetes API token (`automountServiceAccountToken: false`, whatever its template says): it reaches the farm through its control directory, its pod's annotations, and the HUD with a token its tool profile grants. Finished pods are
deleted only after the status that records them is written, so a restart
loses no unit. The manager caches only unit pods (a label selector), not
every pod in the cluster.

### 10.3 Prior art and what this builds on

| system                    | what it offers                                                                                                                                                                                                                                                      | what it lacks for this                                                                                          |
| ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| Kueue                     | ResourceFlavors; ClusterQueues with quotas; cohorts with borrowing and lending; fair sharing; preemption; all-or-nothing admission; topology-aware scheduling; ProvisioningRequest admission checks; JobSet, LeaderWorkerSet, Kubeflow Trainer and Ray integrations | Admits whole workloads with a static priority that people set. No estimate of remaining work, no deadline model |
| Volcano                   | Gang scheduling, queues with weights, DRF, proportion, an SLA plugin on waiting time, reclaim and preempt                                                                                                                                                           | Replaces kube-scheduler for its pods. Orders by priority, fairness and wait time, not completion deadline       |
| YuniKorn                  | Hierarchical queues with guaranteed and max resources, fair or FIFO sorting, gang scheduling, preemption                                                                                                                                                            | A scheduler replacement with static ordering policies                                                           |
| JobSet, LeaderWorkerSet   | Multi-pod training and serving jobs with gang semantics, failure policies, exclusive topology placement                                                                                                                                                             | No queue: they rely on Kueue or a gang scheduler for admission                                                  |
| Kubeflow Trainer, KubeRay | Training jobs (TrainJob on JobSet) and Ray clusters as Kubernetes objects                                                                                                                                                                                           | Same: admission and quotas come from Kueue                                                                      |
| Deadline                  | Pools and groups, priority 0 to 100, machine and license limits, bad-worker detection, Spot Event Plugin bursting                                                                                                                                                   | Priorities are set by people. Bursting follows queued task counts, not deadline risk                            |
| OpenCue                   | Shows with subscriptions (guaranteed size and burst per allocation), layers with resource needs, host tags                                                                                                                                                          | Orders by subscription share and priority. No deadlines                                                         |
| Tractor (and Alfred)      | Job scripts of Tasks and Cmds, priorities, sharing limits, blade profiles, and `-expand` commands whose output becomes new subtasks                                                                                                                                 | Priorities are set by people. Expansion exists, but nothing estimates the work it will reveal                   |

Tractor's expand is the prior art for dynamic graphs. A `RemoteCmd` with
`-expand 1` "produces new Tractor task descriptions as its output", and
"those new tasks will become new subtasks of the current Task" (Tractor
scripting reference). Tractor 1.7 added a file form, and 2.1 added expand
chunks (`TR_EXPAND_CHUNK "filename"`) so a long-running command can deliver
tasks while it runs. Section 10.6 adopts both forms.

Decision:

- **Build on** kube-scheduler for placement, Pods for execution, the Cluster
  Autoscaler `ProvisioningRequest` API and Karpenter NodePools for capacity,
  Prometheus metrics for KEDA, and **Kueue with JobSet** for anything that
  needs gang or topology-aware scheduling (ML training). Kueue is chosen over
  Volcano and YuniKorn because it admits work and leaves placement to
  kube-scheduler, as this queue does, so the two compose instead of competing,
  and because JobSet, LeaderWorkerSet, Kubeflow Trainer and KubeRay all
  integrate with it. Project quotas use Kueue's semantics (nominal quota,
  borrowing limit, lending of unused quota, reclaim by preemption), so a farm
  that runs Kueue reads the same rules.
- **Build** the unit-level queue, dynamic expansion with estimates of
  unrevealed work, runtime estimation, objective-driven allocation, the
  feasibility check, and cost-aware burst decisions. None of the systems
  above estimate remaining work or order by deadline risk.

### 10.4 The shared core and work classes

Render, CI and ML have the same skeleton. The core owns it:

| concept    | core meaning                                                                                                             | render                              | CI                                      | ML (wrapped)                                      |
| ---------- | ------------------------------------------------------------------------------------------------------------------------ | ----------------------------------- | --------------------------------------- | ------------------------------------------------- |
| unit       | the smallest thing the queue releases; one pod, or one wrapped object                                                    | a chunk of frames of one pass       | a step or a test shard                  | a training run or a sweep trial                   |
| gang       | members that start and stop together; the queue accounts for them as one unit and never releases or preempts part of one | size 1                              | size 1                                  | the ranks of a run; placement by Kueue and JobSet |
| generator  | a unit whose evaluation adds units and edges (section 10.6)                                                              | USD export expanding into passes    | test discovery into shards              | a sweep planner into trials                       |
| objective  | an effective deadline or a required rate (section 10.7)                                                                  | deadline from job, shot or delivery | latency SLO: ready time plus p95 target | deadline, or a GPU-hour rate                      |
| estimator  | a duration distribution for every unit and every unexpanded generator (section 10.8)                                     | per frame-range bucket and pass     | per target and shard history            | steps left times measured step time               |
| pools      | where units may run (section 10.10)                                                                                      | cpu, gpu, high-mem, spot            | ci runners                              | gpu pools                                         |
| preemption | stop with or without a checkpoint; resume from captured state (section 10.13)                                            | checkpointed long frames            | rarely: shards are short                | yield at a checkpoint boundary                    |
| lens       | the class-specific HUD panel (section 11.4)                                                                              | passes by frame, exposure sheet     | pipelines per change, flaky tests       | ranks and trials, useful GPU hours                |

A **work class** is a driver plus its configuration. The driver is a Go
package that implements one interface; the configuration is a `WorkClass`
object. The core imports no class package except through a registry, and
nothing in the core mentions frames, tests or ranks.

```go
type Class interface {
    Name() string
    Plan(job *Job) (Graph, error)                        // units, edges, gangs, generators
    Expand(job *Job, gen UnitID, doc []byte) (Graph, error) // a generator's output becomes units and edges
    Objective(job *Job, wc *WorkClass, now time.Time) Objective
    Estimator(job *Job, h History) Estimator             // includes the fan-out model for generators
    Launch(job *Job, u *Unit) ([]client.Object, error)   // pods, or a suspended JobSet for wrapped work
    Observe(job *Job, u *Unit, r Result)                 // class signals: flaky tests, useful GPU hours
    Summary(job *Job) json.RawMessage                    // bounded, for the HUD lens
}
```

Adding a class (media transcode, simulation caches, generative batch,
interactive sessions, data movement) means writing a driver, a `WorkClass`
object and a lens. Section 10.18 sketches each.

### 10.5 Objects

| kind              | scope     | written by       | holds                                                                                                                                           |
| ----------------- | --------- | ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| `WorkClass`       | cluster   | administrators   | driver, SLO type and target, default pools, estimator settings, lens, fair-share weight, preemptibility                                         |
| `WorkJob`         | namespace | people and tools | class, class parameters, objective overrides, requirements, inputs, outputs, dependencies on other jobs, nudges, pod template                   |
| `Pool`            | cluster   | administrators   | node selector and tolerations, slot shape, attributes (class, GPU model, VRAM, capacity type), cost per node-hour, capacity bounds and provider |
| `Project`         | cluster   | administrators   | `spec.work.quotas`: nominal slots and borrowing limit per pool; burst budget; lateness cost                                                     |
| `Milestone`       | namespace | production       | a due time and a label selector of the jobs it covers (a shot, a delivery, a release)                                                           |
| `CapacityRequest` | cluster   | the scheduler    | per pool: slots wanted, until when, why, estimated cost, approval, views to pre-warm                                                            |

A WorkJob looks like this:

```yaml
apiVersion: c4.avalanche.io/v1alpha1
kind: WorkJob
metadata:
  name: sh040-light-v007
  namespace: show-a-render
  labels: {c4.avalanche.io/shot: sh040}
spec:
  class: render
  params: {frames: "1001-1240", chunk: 1, passes: [beauty], expand: true}
  deadline: "2026-10-23T18:00:00-07:00"     # optional; else from Milestones; else class default
  requirements: {pools: [gpu]}
  dependsOn: [sh040-fx-v003]
  inputs:
  - {materialization: sh040-plates, mountPath: /show}
  - {fromJob: sh040-fx-v003, mountPath: /fx}  # mounts the upstream job's output view
  output: {capture: true}
  nudge: {hold: false, boost: "1.0"}
  retries: 2
  nodeErrorLimit: 3
  checkpoint: {signal: SIGTERM, graceSeconds: 120}
  template: {spec: {containers: [...]}}
```

WorkJob status holds counts, per-unit state as run-length encoded index sets
(`succeeded: "0-99,102-180"`), the graph's size and expansion state, the
estimate (remaining slot-seconds with a 90th percentile, required and
attainable slots, ETA, slack, feasibility), a bounded blacklist, the output
view, and the class summary. Its size is bounded by state changes along the
unit range, not by the number of units. Very large graphs (over 50,000 units
or after deep expansion) keep their unit table as a c4m-style object in the
project store, referenced from status, so etcd never holds it.

**Data flows between jobs as views.** Each unit's captured changeset has the
job's input view as parent. As each unit finishes, the queue folds its
changeset into the job's output view and appends the fold to the job's
output chain: the input view on the first line, then one section per fold,
each sealed by the output view it produced, so every intermediate output is
a checkpoint that can be verified and mounted. The chain is stored in the
project's store and named in the job's status with the current output view
(`status.output`). Writes by two units to the same path are reported in
status, not merged silently. A unit's pod is deleted only after its fold is
recorded, so a restart repeats no fold and loses none. A downstream job's `fromJob` input
mounts that view. The dependency graph (sim to render to comp to transcode to
delivery) is therefore also a data-flow graph of content-addressed trees, and
each output is already cached on the nodes and regional replicas that
produced it.

Data also flows while jobs run. Units seal their outputs into the job's
output chain as they go (section 2.5), and a `fromJob` input can be
pipelined: a downstream unit becomes runnable as soon as the upstream frames
it reads exist at a sealed checkpoint, instead of when the whole upstream job
finishes.

### 10.6 Dynamic expansion

A unit can be a **generator**: a node of the graph whose evaluation adds
units and edges. A USD export that produces deep env and volumes layers
before the beauty layer that holds out against them, a CI step that
discovers which test shards a change needs,
and a sweep planner that spawns trials are all generators.

- **States.** A generator starts *unevaluated*. Evaluating it is itself a
  schedulable unit. When it finishes, its expansion document becomes new
  concrete units and edges attached under it, and the generator becomes
  *expanded*. Expansion may nest (a generator may emit generators), bounded
  in depth and in total units per job.
- **Strict documents.** Expansion documents, verdicts, usage documents and
  c4-plan files are read strictly: a field the format does not have is an
  error naming it (a renamed one says where it went, such as `does` now
  `reasoning.does`), so an old or misspelled document fails loudly instead
  of being misread. A refused expansion or verdict fails its unit; a
  refused usage document leaves the unit's tokens uncounted and says why in
  `status.modelUsage[i].usageError`.
- **Transport.** The evaluation writes its expansion document into its own
  C4 volume (`.c4/expand.json`, the Tractor 1.7 file form). Capture stores it
  like any output, and the controller reads it from the store by path. Long
  evaluations can write numbered chunks (`.c4/expand.0001.json`, the Tractor
  2.1 chunk form); the node daemon notices each complete chunk in the upper,
  ingests it, and reports its ID in a pod annotation, so children can start
  before the generator ends. No unit table travels through pod annotations or
  termination messages.
- **Edges.** Children inherit the generator's dependents: whatever waited on
  the generator now waits on its children (all of them, unless the document
  names a subset). A child may depend on its siblings, as beauty depends on
  the deep layers it holds out against.
- **Estimating work that does not exist yet.** Before evaluation, a
  generator's eventual work is a distribution taken from history for similar
  generators (same class and generator key, for example "usd-export, layer
  lighting, show A"): the number of children and their total work, with
  variance. As generators in the job are evaluated, the job's own observed
  fan-out replaces the prior (a running mean and variance per generator key),
  and expanded children carry their own estimates. The job's remaining-work
  band narrows as expansion happens, and the HUD draws that band (section
  10.3).
- **Order.** Unevaluated generators carry information the scheduler needs,
  so a job's generators are evaluated ahead of its concrete units of equal
  standing (section 10.9), the same reasoning as sampling frames early.
- **Built.** The controller carries out the file form; the chunk form is not
  built yet.
  - At capture the node daemon stores `.c4/expand.json` (the control
    directory is never part of the output) and names it on the pod
    (`c4.avalanche.io/output-expand`). When the generator succeeds, the queue
    reads the document, `class.Expand` turns it into units (a class may
    override the plain reading, `class.ExpandDocument`), and their indexes
    follow the table's last.
  - The document, version 1, gives each unit a `name`, a `label` and a
    `key`; `after` (names in the document or already in the job); a `work`
    prior with `workSD`; `exists`; `planned` (itself a generator); `env`;
    `inputs` (`fromUnit` mounts a unit's captured output read-only, and is
    also an edge); and `slots`. The document's `dependents` is `"all"` or a
    list of names. `holds` and `claims` are the unit's claims (below);
    `group`, `params` and `cites` are carried for the HUD and for the
    record. A document with a bad version,
    a name used twice, an edge to a name the job lacks, a cycle, or nesting
    deeper than `maxExpandDepth` (4) is refused whole: the generator fails
    and the refusal is listed.
  - Each expansion is signed (decision/v1, kind `expand`, naming the
    document) and listed in status `expansions`, so a restart replays the
    same documents from the store to the same table.
  - A unit runs once every unit in its `after` has succeeded or been
    accepted. Until then it is `blocked`, and a generator that has not run
    is `planned`. A unit waiting on a failed one is stuck, and counts with it
    when the job's phase is decided.
  - The estimate carries unevaluated generators as the fan-out model
    (section 10.8) and weights tentative work by `exists`. The critical
    path runs through the unit graph.
  - The table is bounded at 50,000 units; the store-backed table of
    section 10.5 is not built.
  - The `graph` class is a job whose plan is one generator (a plan
    importer, test discovery, a sweep planner); its units run the job's
    template, told apart by `$C4_UNIT_NAME` and their `env`.
- **Rounds** (agent-catalog G-rounds, G-expand-depth): iteration that is
  not spelled as nesting. A generator may be a bounded loop:
  `rounds: {max, until?}` on a planned unit of a document, or the job params
  `rounds` and `roundsUntil` on a graph job's root.
  - **Siblings, not children.** Round k's units are named `r<k>/<name>`. A
    round's document refers to its own units by the names it gave them, and
    to earlier rounds' by their full names; so may a review of the round in
    its verdict. A document that says
    `next: true` adds the next round's generator, `r<k+1>/<name>`, after the
    round's units, a sibling of the first. So `maxExpandDepth` limits
    nesting only, and a fifty-round loop is one level deep. Each round's
    generator carries `$C4_ROUND` and `$C4_ROUND_MAX`.
  - **Until.** With `until`, `next` is the default and the round must hold
    that review. When its verdict's summary word is `pass`, the next
    round's generator does not run: the loop ends there and the generator
    counts as done (signed, kind `rounds-stop`). At `max` no next round is
    added.
  - **Status.** `status.rounds` lists each loop: `{unit, name, round, max,
    until?, stoppedBy?, generators, expectMore?, expectSeconds?}`.
    `stoppedBy` is `until`, `generator` (it wrote no next round), `max` or
    `failed`.
  - **Pricing.** A loop's continuation is learned per project, class and
    generator key, across jobs, and saved with the project's calibrations,
    as rework is. It is the chance a round is followed by another, counting
    rounds that went on and loops that stopped on their own. Before any
    history a loop expects `max/2` rounds, or the generator's
    `expectRounds` param, else the job's. The rounds after the pending one
    are tentative units: round j is weighted by the continuation to the
    power j and costs what a round of the key costs. Rounds run one after
    another, so their weighted durations add to the critical path. The
    round in progress is priced from its own expansion, like any
    generator's. The ETA band widens with the expected rounds, never with
    `max`.
- **Holds and claims** (PRIMITIVES Q8, Q9). A unit's `holds` (paths it
  writes) and `claims` (`{on, mode}`) are what it claims.
  - **Overlap.** A path claim covers everything under it (`src/net/` and
    `src/net/conn.go` overlap, `src/net` and `src/netx` do not); a name
    with a scheme (`db:fixtures`) is a resource and overlaps only itself.
    Two `shared` claims never conflict; an `exclusive` one (the default,
    and every hold) conflicts with any claim it overlaps.
  - **Locking.** A unit whose claims conflict with a unit of its job that
    is running, or whose output is still being captured, is `locked` by
    it. Units released in one tick are checked against each other in the
    policy's order. This is Ra's P-21 rule, that lanes sharing a file run
    in sequence, as a scheduling fact.
  - **The visible queue.** Status `holds` lists each holder with its
    claims and the units locked behind it, in dispatch order. The HUD's
    unit list says `lockedBy` and each unit's `claims`. Units that must
    take turns form a chain on the critical path.
  - **No deadlock.** A unit takes its claims when it starts and waits for
    nothing while it holds them (its edges were met before it started), so
    claims cannot deadlock and need no cycle check.
  - **Scope.** Claims are scoped to the job. A gate is shared across a
    namespace's jobs.
- **Gates** (Q9; LLM-OR-PROCEDURE 7.2 item 4). A unit of kind gate
  (`gate: "merge"`) passes a resource of one slot shared by every job in its
  namespace: a project's merge gate, its signing step, its one DI suite.
  - **First come, first served.** Units ready to pass queue in the order
    they became ready. While the gate is held, or another unit is ahead,
    a unit is `locked`, with its place in line, the holder and when it is
    expected to pass (the holder's remaining work plus the predicted work
    of those ahead). Only the head may start when the gate is free, and
    only one unit passes per tick.
  - **Where the line shows.** Each job's status `gates` lists its units at
    gates. Join times survive a restart, so the order does too. The HUD
    snapshot's `gates` gathers each gate's holder and line across jobs,
    and a waiting unit's `lockedBy` names its gate and position.
  - **No new scheduling.** A gate unit still needs a slot in its pool; the
    gate only decides which waiting unit may take one.
- **Edge sources** (LLM-OR-PROCEDURE 5 and 7.2 item 2). Expansion v1's
  `edges: [{from, to, cite?}]` add orderings between units already in the
  job (or in the document): `to` runs after `from`.
  - **Edges alone.** A document of edges with no units is valid, so a
    procedural unit re-run after each merge keeps the job's graph in step
    with its code.
  - **Effect.** An edge takes effect on a unit that has not started; one
    that is running or done is not stopped.
  - **Refusal.** Edges that would close a cycle anywhere in the job are
    refused whole, like any bad document.
  - **Sources are plugins** (`internal/edges`, run by `c4-edges`). `marker`
    reads a tree with a regular expression whose `from` group names the
    unit waited on; the waiting unit is the pattern's `to` group, else the
    owner of the file (the unit claiming its path, from `-owners`), else
    `-to`. `json` reads a list a tool wrote. Each edge cites its file and
    line, fixtures are excluded by glob, a marker naming its own owner is
    no edge, and what cannot be read is listed. Ra's `Pending{…}` markers
    are the motivating case: the pattern is the job's, not c4's.
- **Procedural and model units** (LLM-OR-PROCEDURE 6.2 and 7.2). A unit
  declares what it reasons with in its expansion document or plan block:
  `reasoning: {does, model, effort, tools}`, `does` being `model` or
  `procedure`, `effort` one of low, medium, high, xhigh and max, `tools` a
  tool profile's name. `does` defaults to the class's (agent members are
  model units), else a procedure. A graph job's root, its generator, is a
  procedure unless the job's `rootDoes: model` says a model reads its plan
  from prose. What an attempt actually reasoned with, as its runner saw it
  (the model or models, the effort or efforts, thinking, the tool profile,
  the tools offered and their MCP servers), comes back in its usage
  document's `reasoning` and is kept for each unit's latest attempt
  (`status.modelUsage[i].reasoning`), so the HUD can show declared and
  observed side by side.
  - **Tool profiles** (agent-jobs 10.15, agent-catalog G-tool-config).
    `reasoning.tools` names a ToolProfile, a namespaced resource. The
    queue looks for it in the job's namespace, then in the operator's,
    where the farm's own profiles live (`none` ships with the operator).
    - **`view`** is the configuration (MCP server definitions, settings,
      skills, plugins), a view's C4 ID, mounted read-only at
      `/etc/c4/tools` (`$C4_TOOLS`). The unit's record names it, so a
      unit's record says exactly what it ran with.
    - **`egress`** is what the unit may reach: hosts (`{host, ports?}`,
      a name or `*.domain` for the names under it), address blocks
      (`{cidr, ports?}`) and cluster services (`{service:
      <namespace>/<name>, ports?}`). Hosts go through the farm's egress
      proxy (`c4-operator egress-proxy`, config/egress; the manager's
      `--egress-proxy`). The unit's `HTTPS_PROXY` and `HTTP_PROXY` name
      the proxy, and its policy allows the proxy rather than the hosts.
      The proxy knows the unit by its pod's address, lets a request
      through (CONNECT or plain HTTP) only to a host the unit's profile
      lists, and logs each one it passes or refuses against the job and
      the unit. It works on any CNI. The fields stay neutral, so a
      Cilium `toFQDNs` backend could serve them instead. The queue writes the profile's
      NetworkPolicy in the job's namespace (`c4-tools-<profile>`), which
      selects the unit pods labelled `c4.avalanche.io/tool-profile` and
      allows those, plus DNS when there is any rule. A profile with no
      egress allows nothing, so its units are sealed, which the derivation
      record counts. The owner writes the allow-list.
    - **`sidecars`** are servers that run beside the unit for its whole
      run: MCP servers, a headless browser (`{name, image, args?, env?,
      port}`). They are native sidecars, started before the unit and
      ended with it, with the view mounted too. The unit finds each at
      `$C4_TOOL_<NAME>` (`http://localhost:<port>`). They share the
      unit's network, so its egress is theirs.
    - **Reuse:** a unit's process key is its template, so a unit with a
      profile is never reused from an earlier run. Nor is one given
      supplied information or a decision.
    - **Release:** a unit is released once its profile exists, its view's
      Materialization is Ready, and its policy is written. Until then the
      job's `Tools` condition says what it waits for. Units that name no
      profile run as before.
    - **Tokens:** no unit pod holds a Kubernetes API token (10.2). A
      profile's `c4` says what its units may do with c4 itself, through
      the HUD and so MCP.
      - **Grants:** `read` sees their project; `job` may also act on
        their own job (decide its waits, steer, nudge); `project` may
        also submit jobs in their project.
      - **The token:** such a unit holds a token for its pod alone, a
        projected service-account token with the HUD's audience
        (`c4.avalanche.io/hud`), which the Kubernetes API does not
        accept. It sits at `$C4_HUD_TOKEN_FILE`, with the HUD at
        `$C4_HUD` (the manager's `--hud-service`), and its policy
        allows the HUD.
      - **At the HUD:** the HUD checks the token with a TokenReview for
        that audience, finds the pod it is bound to, and acts on the
        unit's grant. Every control the unit makes is in the book as
        `unit:<namespace>/<job>/<unit>`.
  - **Work share.** Each ended attempt also adds its slot time (hours
    times slots) to its unit, and status gives the model units' share of
    it (`spend.modelWorkShare`): how much of the work models did, which
    the HUD's job summary shows as `modelShare` ("models did N% of the
    work"). `spend.modelShare` stays the model units' share of the
    cost.
  - **Slot spend.** Each ended attempt, failed and interrupted ones
    included, adds its slot time at its pool's rate to its unit.
  - **Token spend.** A model unit also reports the tokens it used, per
    model, in `.c4/usage.json` (`c4-unit usage --model M --input N
    --output N [--cache-read N] [--cost USD] [--cost-source S]`, stored at
    capture as `c4.avalanche.io/output-usage`). The cost is optional, with
    its source named, because the price is known to a metering proxy or a
    price table rather than to the session.
  - **Token classes are weighed by what they cost** (`work/tokens`).
    Relative to a model's base input price, Anthropic's published rates
    (platform.claude.com/docs/en/about-claude/pricing and
    .../build-with-claude/prompt-caching, as of 2026-10-06) put output at 5,
    a cache write at 1.25 (5-minute lifetime; 2 for an hour), and a cache
    read at 0.1. The cache read is 0.05 on Claude Opus 5.5 and 0.025 on
    Claude Fable 5.1 and Claude Mythos 5.1.
    - **Weighted tokens.** They are the input-equivalent sum. Usage does not
      say a write's lifetime, so writes are weighed at the 5-minute rate.
    - **Why it matters.** It keeps an agent session honest, since its cache
      reads dwarf everything else: one measured session read 514M cached
      tokens against 1.06M output. Raw, that is 515M tokens; weighted it is
      56.7M input-equivalents, about $113 on Claude Sonnet 5.5 rather than
      $1,030.
    - **Estimated cost.** Tokens that came with no price are estimated at
      the model's published input price times their weight (a dated table
      of current Claude models). A model the table does not know is left
      out, and the share says so.
  - **What status shows.** `status.spend` has:
    - the units of each kind;
    - the raw token classes (input, output, cache reads, cache writes) and
      their weighted total;
    - the unpriced tokens, raw and weighted;
    - the reported token cost and the estimate for unpriced tokens;
    - each kind's slot cost;
    - the model share of spend, with its basis: `priced`, `estimated` (some
      at published rates) or `partial` (some of a model with no known
      price, left out). `status.modelUsage` has each model unit's tokens
    by model. The HUD's job summary shows `spend`, and its unit list
    `kind` and `usage`.
  - **Reuse** is C4-native memoization, which Argo's string-keyed one
    cannot be.
    - **What is reused.** A procedural unit ready to run reuses an earlier
      run's output when the same computation ran before in its project:
      the same process (images by digest, command, arguments, environment,
      platform) on the same input views. Its program must be exact
      (declared, sealed) and verified by re-runs that wrote the same bytes,
      and its job must itself declare exact.
    - **What happens.** The earlier changeset is folded into the job's
      output with that run's task record. The unit succeeds without a pod,
      the reuse is signed (decision/v1, kind `reuse`), and status and the
      HUD mark it `reused`.
    - **Verification.** A share of reuse opportunities (the book's
      `VerifyRate`, 5%) runs anyway, so the declaration stays falsifiable.
    - **Not reused:** model units, generators, gates, and units whose run
      the key does not fully describe (inputs from other units, a rework's
      changes, a resumed attempt).
  - **Model work a procedure could do** is reported, not decided. A model
    unit whose same computation (process and input views) ran at least
    three times and wrote the same bytes every time behaved as a pure
    function of its inputs. It is listed in status `procedural` (and the
    HUD's job detail) with the evidence, for a person to weigh: a
    procedure would do it cheaper and checkably. It needs the unit's runs
    in the book: pinned images and resolved input views.
- **The job's measure of done** (Q12; LLM-OR-PROCEDURE 7.2 item 3). A job
  declares what it counts: `spec.measure: {name: "rules sited", of: 379,
  required: true}`. A unit, usually a procedural one re-run after each
  merge that reads a manifest, reports a reading with `c4-unit measure
  --of 379 361` (`.c4/measure.json`, stored at capture as
  `c4.avalanche.io/output-measure`).
  - **Shown as progress.** The latest reading by finish time is the job's
    `status.measure` (`{name, value, of, done, at, unit, document,
    detail}`), and the HUD's job summary shows it as the job's progress.
  - **A completion predicate.** A required measure decides when the job is
    done: the job succeeds only once the value reaches the target. With its
    units finished short of it, the job fails and needs a person
    (NeedsPerson `Measure`: "its units finished at 361 of 379 rules
    sited").
- **Verdicts.** A review unit judges the units it runs after and writes its
  judgment as `.c4/verdict.json`. The node daemon stores it at capture, as
  `c4.avalanche.io/output-verdict`. When the review succeeds the queue
  carries the verdict out, once per pod, and signs it (decision/v1, kind
  `verdict`): the review's record, the document, and each judged unit's
  output and outcome. Render QC, CI flaky reruns and agent refutation are
  the same primitive with different checks.
  - **Naming units.** The verdict names units by name, label, a label
    range (`1101-1102`) or `#index`. Only units the review runs after may
    be judged, and `pass: rest` passes the others.
  - **rework** runs the named units again with `with`: nodes to avoid,
    the only pool to use, the first container's memory or image, env, and
    views to mount (a counterexample, a note). The next attempt starts from
    the unit's last output view, as a yield resumes, unless `fresh` is set.
    The review then runs again after them. A unit reworked `maxPasses`
    times (default 2) gets `onExhausted` instead: `escalate[:who]`,
    `accept` or `fail`.
  - **escalate** flags the units for a person (NeedsPerson `Review`,
    naming who). The queue treats them as failed units, so requeue, take
    and accept work on them unchanged. The review that flagged them does
    not wait on them when it runs again.
  - **accept** lets the units stand, with the note recorded.
  - **expand** adds units under the review, as a generator's expansion
    does.
  - **One format.** Scores, selection and supersede are optional fields
    of the same document (version 1, read strictly), not a second version
    (agent-catalog G-scored-verdict, G-supersede).
  - **score** is a number per judged unit, on its item. Higher is better
    unless the review unit's params say `scoreOrder: asc`. A later verdict
    may score a unit again (a backup in a tree search). The last score
    stands, and every one is in the signed record.
  - **select** chooses among the passing units with a score, this
    verdict's or the one standing: `{top: k}`, `{threshold: x}`, or
    `{ucb: c, top?: k}`. UCB ranks by the score plus c times sqrt(ln N / n),
    n being how many verdicts scored the unit and N their sum. The chosen
    pass; the rest are superseded.
  - **supersede** ends units because others were chosen. It may name the
    units the review judges, or its unfinished siblings, so alternatives
    still running stop: their pods are deleted, no retry is spent and
    nothing is failed. A superseded unit is done, not accepted (state
    `superseded`): nothing waits on it, it costs no more, and its
    changesets leave the job's output. The output is refolded from its
    fold log, kept in the provenance index, without them: a new chain,
    each section again sealed by the view it makes. The superseded
    output's chain stays in the store. Pricing what early superseding
    saves, per key, is not built: a superseded unit's remaining work
    simply leaves the estimate.
  - **Refusal.** A verdict that judges a unit the review does not run
    after, judges one twice, supersedes a unit it may not, or carries a
    bad override, selection or expansion is refused whole, and the review
    fails.
  - **Status.** `verdicts` lists the most recent verdicts with their
    counts (`superseded` among them) and selection rule. `reviews` holds
    each unit's last outcome beyond a plain pass, its passes, its standing
    override, and its score.
  - **Expected rework is priced from the start.** A review is a unit its
    document marks (`params.review`), or one whose key has written
    verdicts in the project. Its rework rate is learned per project, class
    and review key across jobs (saved with the project's calibrations):
    units reworked over units judged, shrunk toward a prior (the review
    unit's `reworkRate` param, else the job's, else 0.3) with a
    pseudo-count of 2, so the first job of a kind carries rework in its
    p50 and p90 before any verdict. A review that has not run yet adds,
    for each unit it will judge, the passes it may still send back (r, r²,
    up to maxPasses less the unit's passes so far) as tentative work, and
    its own rerun with the chance that anything is reworked; a review that
    has run judges again only what it sent back. The same chains lengthen
    the critical path.
  - **A rework costs what reruns cost.** A rework resumes from the unit's
    last output, so it is priced as a resumed unit: the share of a first
    pass that reruns of its key take, learned per project, class and key
    (a rerun's run time over the unit's first pass), shrunk toward one half.
    Expected reworks, reworked units waiting to run and reviews running
    again are all priced that way, and a unit's key learns only from its
    first pass. On agent-jobs' library-port run, replayed through the
    queue (TestReplayLibraryPort), the p90's mean error from five minutes in
    fell from 16 minutes to 7, its shortfall before the first verdict from
    20 minutes to 11, and its jump at that verdict from 89 minutes above
    the finish to 18.
- **Waits** (PRIMITIVES Q6; the HUD word is "waits for"). A wait is a
  unit served by something other than a slot. A document's unit carries
  `wait: {server, ask?, expect?, choices?, reply?}`, and `server` says what serves
  it: `person:<id>`, `party:<name>` or `clock:<RFC 3339 time>`.
  `predicate:<unit>` is designed and refused until it is built.
  - **No pod.** A wait creates no pod, holds no slot, and is never handed
    to the policy as runnable. Once every unit it runs after is done, it
    is open (`unitStates.waits`, `units.waits`).
  - **Decisions.** A person's or a party's wait completes on a decision:
    `POST /jobs/{ns}/{name}/decide` or MCP `c4_decide`, signed first,
    then written as a request in `c4.avalanche.io/decide` that the queue
    carries out once, in token order. `do` is `approve`, one of the
    wait's `choices`, or `fail`, which fails it. A clock's wait completes
    at its time.
  - **Replies.** A wait with `reply: true` asks for content with its
    decision: a text, a file, an edited plan. The decision carries it as
    a view (`reply.view`, or `reply.text`, which the HUD stores as a view
    of one file, `reply.md`), and anything but `fail` without one leaves
    the wait open. The reply is the wait unit's output, so the units it
    holds up mount it with a `fromUnit` input, as they would a unit's
    view; `waits[].view` shows it. A unit may read only a wait that
    replies, and a clock cannot reply.
  - **The job's review** (`spec.review: {server, ask?, choices?,
    expectSeconds?}`, integrations GAPS 7). A final wait, unit `@review`,
    after every other unit, including those an expansion adds later
    (`Plan.Apply` joins each to it, whatever the document's dependents
    say), so the job is Succeeded only once it is decided: done means
    approved, as a tracker means it. Any choice but `fail` completes it;
    `fail` fails the job. No document names it or waits on it. It counts as
    blocking, so the job needs the reviewer once its work is done. A
    requeue reopens it, so an earlier approval passes nothing new. It is
    set when the job is created, since the queue replays the plan's
    expansions against it.
  - **Opened by a unit.** A running unit that needs someone (an approval
    of a tool call, an answer, an edited plan) asks at a safe point:
    `c4-unit wait --server person:<id> [--ask] [--choices] [--reply]
    [--expect] [--checkpoint]` writes the wait into `.c4/safepoint`, and
    the node daemon (or the host runner) passes it on in the safepoint
    annotation. The queue checks it as a document's wait and answers
    `stop` ("waits for person:owner"), or `continue` with the reason when
    it cannot open it. The unit yields there (yield reason `wait`): its
    slot is freed, and it stays out of the runnable set, in
    `unitStates.waits`, while the wait is open, its expected wait plus its
    remaining work on the critical path. The wait is in `waits` with
    `block` and the safe point's number, decided like any other; `fail`
    fails the unit. Decided, the unit runs again and resumes from what it
    saved, with the decision in `$C4_DECISION` (`{safepoint, do, note,
    actor, at, view?, reply?}`, `c4-unit decision` prints it) and a reply
    mounted read-only at `$C4_REPLY` (`/etc/c4/reply`). A unit that ends
    without stopping withdraws its wait. Each ask is in the book
    (`safepoint-wait`).
  - **Missing information is an error, not a question** (Joshua, agent-jobs
    ASKS.md). A unit acts on its brief and decides what the brief leaves
    to it. When the brief lacks information only a person could give, the
    unit stops with `NeedsInformation`.
    - **Signal:** its container exits 12 with the information as its
      termination message (`c4-unit needs --what [--why]`), or a host
      runner sets the pod's status reason.
    - **The unit fails at once,** without a retry (the same brief would
      ask the same again) and without a mark on its node. It keeps what it
      saved, and the job says what it needs: `status.needs[i]` and
      NeedsPerson `NeedsInformation`, an incident of its own in the HUD.
      The rest of the job goes on where it does not depend on the unit.
    - **Supplying it** (`POST /jobs/{ns}/{name}/supply {unit, text}`, MCP
      `c4_supply`, signed) runs the unit again, resuming where it stopped,
      with the text in `$C4_SUPPLIED` and what it asked in `$C4_NEEDED`.
    - **A person answering on purpose** (a creative approval) is a step
      in the graph: a wait or a verdict, decided as above.
  - **Agents answer agents** (Joshua; agent-jobs ASKS.md). A unit that
    lacks a fact another unit of its job has asks it: `c4-unit ask --to
    unit:<name> --ask TEXT` prints the question's id (`<unit>#<n>`).
    - **Answering:** the answerer replies with `c4-unit answer --ask ID
      (--answer TEXT | --cannot WHY)`. Both commands write lists in the
      unit's `.c4/` (`asks`, `answers`), which the node daemon or host
      runner relays as pod annotations.
    - **Routing:** the queue routes each question, once, as a steering
      message marked `ask: {id, from}` to the answerer, and each answer
      back marked `answer: {id, from, cannot?}`. The answerer is the
      question's `to`, else the job's `answerer` param.
    - **Answered `cannot` at once:** a question with no answerer, one to a
      unit that failed, one that would wait in a cycle, or one past
      `maxAsks` (3) open questions.
    - **Answer-decided waits:** a wait may be served by a unit (`server:
      unit:<name>`). A wait unit of the graph asks its `ask` of that unit,
      and the answer completes it. A block that waited past its hold gives
      up its slot to such a wait and resumes with the answer
      (`C4_DECISION` with `do: answer`). The expected wait for a unit's
      answer is 10 minutes until one is measured.
    - **Status and book:** `status.asks` keeps the latest 64, and every
      question and answer is in the book.
    - **Waking a finished answerer:** a question to a unit that has already
      finished wakes it. The queue starts an extra attempt of that unit,
      marked `c4.avalanche.io/wake` with the question in
      `$C4_ANSWER_ONLY`. An agent block takes it as its own session
      resumed to answer, and nothing else. The attempt takes a slot like
      any unit. Its end, and anything it writes, leave the unit as it was.
      One that ends without answering answers `cannot`, and a question
      wakes its answerer once.
  - **Steering** (agent-catalog G-steer): a message to a unit while it
    runs, without ending it.
    - **Sending:** a person sends it with `POST /jobs/{ns}/{name}/steer
      {unit, text}` or MCP `c4_steer`. It is signed, then written as a
      request in `c4.avalanche.io/steer` that the queue carries out once,
      in token order.
    - **Delivery:** the queue numbers each message for its unit and puts
      every message the unit has not read on the unit's pod
      (`c4.avalanche.io/unit-steer`). The node daemon, or the host runner,
      writes them to the control directory as `steer`.
    - **Reading:** the unit reads them at its safe points with `c4-unit
      steer`, which records how far it read (`.c4/steer-seen`, relayed as
      `c4.avalanche.io/steer-seen`). The queue then drops what was read
      from the pod.
    - **A unit not running** gets its unread messages on its next pod. A
      unit that has ended takes none.
    - **Limits:** messages are text, up to 8 KiB, because a running pod
      cannot mount a new view.
    - **Status:** `status.steers` lists the latest 64, each delivered and
      seen.
  - **Estimate.** A wait adds its expected time to the critical path and
    nothing to slot-seconds. The expected time is the median of the
    job's decided waits for the same server, else the unit's `expect`,
    else 12.8 hours, the median of Ra's owner rulings. A person is never
    late in the queue's terms: the ETA moves, the slack shows it.
  - **Needs you.** An open wait for a person that holds up unfinished
    work sets NeedsPerson `Decision`, naming who. An open one that holds
    up nothing yet is not a block.
  - **Escalations are waits.** A unit a verdict escalated is a wait for
    the person in `to`, with no change to the plan. A decision on it is
    `accept` (it stands), `rework` (it runs again) or `fail`. Ra's third
    send-back, `onExhausted: escalate:lead`, is the lead's ruling,
    decided the same way.
  - **Status.** `waits` lists the job's waits, open and decided, and
    `decided` holds the last decision's token.
  - **Scheduling gates are not used.** c4 creates a unit's pod only when
    the unit may run (10.2), so a wait has no pod to gate. A gate is set
    only at creation, and anyone who can update the pod may lift it, so
    it would not be a lock either.

### 10.7 Objectives: every SLO becomes a deadline or a rate

| SLO type   | target                        | effective objective per job                                                                                            |
| ---------- | ----------------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| Deadline   | a time                        | the job's `deadline`, else the earliest Milestone that selects it, else submission plus the class's default turnaround |
| Latency    | p50 and p95 time to finish    | deadline = the time the job became ready plus the p95 target                                                           |
| Throughput | units or GPU-hours per period | a required rate in slots; slack is how far the job is ahead of or behind that rate                                     |

**Deadlines propagate up the dependency graph.** A job's effective deadline
is also no later than each dependent's deadline minus the dependent's
estimated duration at its attainable parallelism. Comp due Friday 18:00 that
needs 4 hours gives its render inputs Friday 14:00, and their sim inputs
earlier still. A job waiting on dependencies is not runnable, but its
upstream jobs see its pressure.

**Milestones.** A Milestone (namespaced: `spec.due`, a label `selector`
of the jobs it covers, and optionally `source: {tracker, ref}`) is a due
time owned by whoever plans the work: a producer, a tracker of intent. A
job with no deadline of its own takes the earliest due of the Milestones
in its namespace that select it; its deadline source is `milestone`
(`status.milestone` names it) and the farm suggests none over it. In
media the tracker does not create jobs, artists do, so the date reaches
every render submitted for a task this way. Each tick the queue writes on
every Milestone the jobs it covers (`status.jobs`, `count`, `done`) and
the worst risk among the unfinished ones (`status.risk`; a failed job
counts as `willMiss`), so a tracker can write one value back to its task.
Date-only due dates, which most trackers keep, mean the project's due time
(`spec.work.dueTime: {at, timeZone}`, 18:00 UTC without one) wherever the
HUD accepts a deadline. A job carries the tracker entity it was made for
(`c4.avalanche.io/tracker-ref: <tracker>/<type>/<id>`, label
`c4.avalanche.io/tracker`) and the version it follows (`spec.follows`),
and its record names both, so lineage links a version to its task and to
the version whose notes it answers.

**Suggested deadlines.** Nobody can pick a sensible deadline without some
sense of when a job can be done, so a job with no deadline of its own gets
one from the farm (`spec.deadlineMode: Suggested`, the default without
`spec.deadline`; `Pinned` and `Tracking` leave it to a person or to
production tracking). The deadline policy is the project's for the job's
class (`spec.work.deadlinePolicies`), else the class's:

- `achievable` (render and ML by default): the earliest time, rounded up to
  the quarter hour, by which the job finishes at the policy's quantile
  (95% by default) without making any job that is on time now late. The queue finds it by running the policy on copies of the
  farm with the job's deadline moved (a bisection, a dozen policy runs, at
  most two jobs a tick).
- `sessions`: the next of a calendar of review sessions (dailies at 08:00
  and 14:00). A job not expected to make it keeps it, and its status
  offers the following session (`status.suggestion.nextSession`), one
  click away.
- `turnaround`: submission plus a duration, the old default.

Suggestions are generous on purpose. A deadline met two days early is
still met, and the person can then decide it really needs to make
tomorrow's dailies. So padding is not treated as a cost, and the farm
never moves a suggestion earlier: tightening is the person's act, made with
the deadline-options readout below. The only move the farm makes is later,
and only when the suggestion itself is at risk. At binary order's
information points (1, 3, 5, 9, 17, 33 units finished), if the finish at
the policy's quantile is after the suggestion, the suggestion moves to the
new achievable time when that is later by more than max(10% of the time
left, 15 minutes). Such a move is recorded with reason `at-risk` and shown
(`status.suggestion.atRisk`, orchid in the HUD). There are at most two such
moves. The suggestion settles after the second, after 33 units, or once a
quarter of the time to it has passed; a settled suggestion never moves,
and if it falls at risk the job's warning says so. Every value is signed
into the book. `work/forecast` holds the rules; the queue applies them.

**What a person is shown.** `status.estimate.eta50` to `eta95` start from
the plan's finish for the calibrated percentiles of the remaining work, with
half the critical path as the last wave's tail, and never sooner than now
plus the whole critical path. The scheduler's own finish adds the whole
critical path, which is the right margin for deciding who is late and what
to buy. The plan assumes each job gets its planned share, and under
contention that is optimistic: in `hack/sim` the band covered 49/56/60/64%
of finishes at P50/P80/P90/P95 with the work calibrated. So the band is
also calibrated on the finish (`forecast.FinishCalibration`). While a job
runs, the queue keeps the uncalibrated median at up to 64 moments: each new
finished count, or hourly. When the job succeeds, each moment's log ratio
of actual remaining time to that median becomes a score, kept per project
and class and by stage (nothing finished, under a quarter, under three
quarters, the rest). With 100 scores, bands are shown at those quantiles;
in `hack/sim` that gave 50/79/89/94%. Sharing, and keeping the scores
across a restart, work as for the work calibration (section 10.8). The
suggestion search and the deadline-options readout use the same calibrated
band.

**Dry runs.** `Queue.DryRun` plans a job that does not exist yet against
the farm as the last tick left it: its class's plan, its estimate with the
project's calibrations, the deadline it would get (suggested, or its own),
and the band it would be shown. It writes and signs nothing, and serves a
submission dialog's preview.

**Deadline options.** Before a person sets a time, the queue says what it
would take (`GET /jobs/{ns}/{name}/deadline-options`, `c4_deadline_options`),
from what-if runs of the policy on a copy of its last snapshot with the
job's deadline at the candidate time: the band and which percentile makes
it; the smallest boost from 1.1 to 2.0 that makes it (a boost asks for the
slots of a deadline 1/boost as far away, and is served only after every
job's need, so it may not make an on-time job late); the burst the policy
would buy and whether it needs approval; the jobs on time with this job's
deadline far out that the time would make late, counted for everyone and
named only in the viewer's projects; and, when nothing makes it, the
earliest finish possible (the critical path, or the remaining work on every
slot the job can attain). The readout also gives the achievable deadline,
the earliest that makes no one late. At most eight times per request; it
never spends or changes anything. Asked for no particular time, it offers
the tighter choices itself, one click each: the project's upcoming review
sessions before the job's deadline (from its `sessions` policies) and the
earliest time the job makes at 80% without making another job late, each
with its odds and what it would displace in the viewer's own project.

**Latency SLOs feed back.** If a class's measured p95 over the last hour is
under target, its effective deadlines relax toward the target. If it is over,
they tighten. CI then takes no more of the farm than its SLO needs.

### 10.8 Dispatch order and estimating remaining work

**Order within a job** is a core option over the job's unit axis (frames
for render, grid points for a sweep):

- `Sequential` and `Reverse`;
- `Binary`: the first and last unit, then the midpoint, then the quarter
  points, then the eighths, and so on. Within each level the order is
  bit-reversed (van der Corput: 1/2, 1/4, 3/4, 1/8, 5/8, 3/8, 7/8, ...), so
  every prefix of the order is spread as evenly as the level allows. For a
  range that is not a power of two, each fraction maps to the nearest index
  of the range and indexes already taken are skipped, which still visits
  every index exactly once. Frame steps (`1001-1240x2`) and chunking apply
  first: the order runs over units, so a chunked job fills coarse to fine at
  chunk granularity.

`Binary` is the default for render preview and dailies classes, and
available to every job. After 33 units of a 240-frame shot, every 8th frame
exists: a preview plays the whole shot at low temporal resolution, and the
estimator has a spot check across the entire sequence. A class can declare a
different default where its axis has no locality (CI shards are ordered by
historical failure rate instead, so a broken change fails fast).

**Estimating** is stratified along the same axis:

- **Per-region cost.** Durations are kept per (job, class key), indexed by
  position on the axis. An unrendered frame's cost is interpolated between
  the nearest completed frames on each side, which binary order guarantees
  exist early. Its error bar combines the difference between those
  neighbors and the interpolation's measured error (each completed frame is
  also predicted from its own neighbors, leave-one-out, and the residuals
  give the spread). Errors are treated as correlated within a region and
  independent across regions, so the job's error bar is honest rather than
  shrunk by averaging.
- **Fallbacks.** With no neighbors on one side, the estimate is the nearest
  region's median, else the job's median, else a prior from the project's
  history for that class key, else the class default. Each class picks its
  key: pass for render, test target for CI, steps and step time for ML.
  For a job with no `estimateSeconds`, no shot profile and no derivation of
  its units, the project's history is the class's recent units in the
  project: the median of their last 256 run times, with a robust spread
  (the median absolute deviation as a standard deviation, at least a
  quarter of the median), else those of projects that share history in
  aggregate. The window is saved with the project's calibrations. The
  class default (render ten minutes a frame) is left for a farm that has
  run nothing of the class. Before this, a quickstart `c4 farm submit` of
  eight frames was estimated ready 25 minutes after it was done.
- **The previous version of the shot.** A job labeled with a shot
  (`c4.avalanche.io/shot`) is estimated from the durations of the last job
  of that shot in its project to have succeeded, frame by frame and pass by
  pass. That previous version's per-frame time is the prior, heavy regions
  included, with a shared spread of 30%. What is interpolated between
  measured frames is then the ratio of this version to the last one, so the
  estimate follows this version as frames land, locally too: a new heavy
  stretch shows as a ratio spike between its neighbors. The queue builds
  these profiles from the WorkJobs it already sees, so they survive a
  restart. They never cross projects. In the eta-deadlines evidence
  (synthetic), this prior put the estimate at submission within 12% of
  the actual run, against 65% for farm-wide history.
- **Heavy regions.** A region whose interpolated cost exceeds three times
  the job median is flagged ("frames 1100-1150 run 3.4x the median, hero
  FX") as soon as one of its frames is measured. Under sequential order the
  same region is found when the queue reaches it.
- Sequential order can only extrapolate from frames already rendered, so its
  estimate of the remainder is blind to whatever lies ahead. The simulation
  measures the difference (section 10.20).
- **Remaining work** `W` is the sum of unit estimates for queued units, the
  expected remainder of running ones (past their estimate, a running unit is
  assumed to need a quarter of its elapsed time again), and the expected work
  of unexpanded generators. A unit that reports its progress `f` (a fraction
  in `.c4/progress`, which the node daemon exports as `c4_task_progress` and
  the manager's scrape hands to the queue, never through the API server) is
  estimated from it. Its remainder is `(done + elapsed)(1 - f)/f`, with a
  relative spread of `0.2 sqrt((1 - f)/f)`, blended by precision with its
  prediction while it is within it, and on its own once the unit has
  outrun the prediction. That is what tracks a single long unit, which has
  nothing to sample. Its spread `σ` combines the variances, and the
  scheduler plans on `W90 = W + 1.28 σ`, so a job with few samples or many
  unexpanded generators is treated as riskier.
- **Calibration.** `W + 1.28 σ` under-covers wherever work has not been
  sampled yet (68 to 83% on shots with a 3x to 8x heavy region in
  `hack/sim`). So the percentiles W50, W80, W90 and W95 are calibrated
  against finished jobs (`estimate.Calibration`). The queue keeps a job's
  summaries at up to 80 moments: every finished-unit count up to 33, then
  about every 1/32 of the job. When the job succeeds, each moment's true
  remaining work is known, and the gap from W, scaled by the summary's
  spread, becomes a split-conformal score. Scores are kept per project and
  class and by what the estimator could see at that moment: the share of
  units finished, the heavy multiple measured on finished units, and
  running units that have outlasted it. A stratum with 200 scores supplies
  its quantiles; one with fewer falls back to coarser strata, then to
  `W + z σ`. A project with `spec.work.shareHistory: aggregate` also adds
  its scores to a farm-wide pool per class, which opted-in projects consult
  behind their own once three projects contribute. Scores carry no names,
  inputs or IDs. Calibrations survive a restart. Every five minutes, each
  project that has learned scores since its last save has its own
  calibrations (remaining work and finish, every class, at most 500 scores
  per stratum) written as one JSON object into that project's own store,
  never another's. The ConfigMap `c4-work-calibrations` in the operator's
  namespace points each project at its latest object. On its first tick,
  before any estimate, the queue restores every project from it. A missing
  or unreadable object starts that project empty; a store that cannot be
  reached is retried, and the project is not saved over until it answers.
  The farm-wide pools are not stored: they are rebuilt from the restored
  calibrations of the projects that share at that point, so a project that
  has stopped sharing drops out. In `hack/sim`, binary-order W90 covers 87
  to 93% for every heavy multiple once a quarter of the frames are done and
  the heavy region has been sampled. Before that, a 3x and an 8x region look alike to any
  estimator, so calibration covers 90% of the mix.
- **Critical path.** Within a job, the longest chain of estimated durations
  through the unit graph (`t_path`) bounds the finish time no matter how many
  slots the job gets. For a render job it is one frame's passes; for a CI
  pipeline it is build plus the longest shard.

### 10.9 Objective-driven allocation

For each runnable job, with `T` the time to its effective deadline:

- **required slots** `R = W90 / T`, the average parallelism that finishes
  exactly on time (a Throughput job's `R` is its required rate);
- **attainable slots** `A`: the job's parallel cap, its pools' size, and its
  project's quota plus borrowable slots, whichever is least;
- **slack** `S = T - max(W90 / A, t_path)`. This is the laxity in the brief:
  deadline minus now minus estimated remaining time over attainable
  parallelism, with the critical path as a floor.

Each time capacity frees up in a pool, the scheduler fills it in this order:

1. **Deadline need.** Jobs running below their required rate, most behind
   first by `running / R`. Every feasible job gets the parallelism its
   objective needs before anyone gets more. This is load-to-deadline
   balancing, and it applies to a render job due Friday and a CI pipeline due
   in twelve minutes alike.
2. **Information.** Unevaluated generators, and the coarse levels of jobs
   in binary order (up to every 8th unit), of jobs that have them. Both
   reveal what the rest of the job will cost.
3. **Fair share.** Remaining capacity goes to projects in order of usage
   divided by entitlement (their quota share, weighted by class), and within
   a project to the job with the least slack. Finishing early is how a farm
   builds margin for surprises.
4. **Ties** within 10% on the deadline ratio go to the project with the
   lower fair-share usage, then to the older job.

**Overload.** When the total `R` exceeds capacity even with burst, ordering
purely by urgency (EDF and least-laxity-first both do this) makes every job
late. The scheduler instead marks jobs whose objective is lost even with all
attainable capacity as infeasible, serves feasible jobs first, and serves
infeasible ones in order of smallest remaining work, which minimizes the
number of late jobs. The infeasible state is what the HUD and the capacity
hooks act on.

**Starvation guard.** A job with ready work and no running unit for 30
minutes gets one unit regardless of order, so far-deadline work keeps
progressing and keeps its estimate fresh. A job with more than 2 hours of
slack lets the deadline tier go first, and is served right after it, until
it has starved for 2 hours (`StarvationDeferMax`); after that it goes first
like any other starving job, so no job waits longer than that while
capacity frees. A guard start is a floor on progress, not a reservation: a
unit the guard started may be preempted, whatever work it would lose, by a
job that will miss its deadline without that slot (on time in the plan but
short of its required slots, or already late in it), and the job whose
guard unit was preempted waits a full 2 hours for its next guard start, so
the two cannot trade the slot. Ordinary units keep the protections of
section 10.13. The work lost to guard preemptions is reported separately
(`guard-unit` preemptions).

**Nudges, not priorities.** Manual priorities are gone. People get bounded
overrides:

- `hold`: release no new units (running ones finish, unless `hold: Stop`);
- `boost` (1 to 2): multiply the job's required slots, drawn from its own
  project's quota first, so a boost cannot take another project's
  guarantee;
- `deadline`: set or move the job's deadline, which is usually what people
  mean when they raise a priority.

### 10.10 Pools, placement, quotas

- A **pool** maps to a node pool by label selector and tolerations. A slot
  is the pool's unit of capacity (for example 8 cores and 32 GiB, or 1 GPU).
  A node's slots come from its allocatable resources divided by the slot
  shape, or from a node label.
- A job states **requirements**: pool names, or attributes (`class: gpu`,
  `gpu.vram >= 24Gi`, `capacityType: on-demand`). The scheduler releases
  units only into matching pools, prefers the cheapest matching pool with
  room, and adds the pool's node selector and tolerations to the pod.
- **Quotas** follow Kueue's semantics. A project's nominal slots in a pool
  are guaranteed when the pool is contended. Unused nominal slots are lent to
  others up to their borrowing limit. When the owner needs them back, the
  scheduler reclaims them by preempting the borrower's least valuable units
  (section 10.13). For a gang it reclaims only when the plan needs the
  gang now, and then room for all of it up to the quota left: a gang that
  can wait does not hold its pool, so part of its room taken back would be
  lent again. By the same rule, a borrower is not held back for another
  project's need that is only such gangs: the held slots would go to the
  next starving job while a job on time in its plan waited.
- **Gangs** are accounted as one unit with the sum of their members' slots.
  The queue releases a gang only when its pool has room for all of it. The
  co-placement itself (all members bound together, in one rack or fabric
  domain) is provided by the wrapped scheduler (Kueue with JobSet), not by
  this queue.

### 10.11 Feasibility check and warnings

The scheduler recomputes every job's estimate and slack on every unit
transition, coalesced to once per second. A job whose 90th-percentile finish
is after its effective deadline gets a `Feasible=False` condition with a
message in production's terms:

```
sh040-fx-v003 will miss Fri 18:00 by about 3h at current capacity.
40 more gpu-spot slots close it (about $410).
```

The added slots are the smallest burst that restores feasibility, taking
each pool's provisioning delay into account. The cost is the pool's price per
node-hour times node-hours. The same numbers drive capacity requests.

### 10.12 Capacity hooks

The scheduler expresses demand as **CapacityRequest** objects, one per pool.
Actuators turn them into the platform's own APIs:

| actuator             | mechanism                                                                                                                                                              |
| -------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cluster-autoscaler` | creates a `ProvisioningRequest` (`autoscaling.x-k8s.io/v1`, class `best-effort-atomic-scale-up.autoscaling.x-k8s.io`) with a PodTemplate of the slot shape and a count |
| `karpenter`          | sets the pool's NodePool `limits`. Karpenter provisions for the pods the queue then releases                                                                           |
| `keda`               | exports `c4_capacity_requested_slots{pool}` and `c4_work_required_slots{pool}` for a KEDA ScaledObject                                                                 |
| `kind-sim`           | the e2e stand-in: provisioning removes a `c4.avalanche.io/unprovisioned` taint from pre-created nodes after the pool's delay                                           |

The `cluster-autoscaler` actuator is built (`--capacity-provider
provisioning-request`, `internal/work/provreq`), for burst pools whose
provider is `provisioning-request`.

- **Shape.** A pool's slots are counted in nodes. Its PodTemplate is one
  token pod per node (required anti-affinity on the hostname), with the
  pool's node selector and tolerations, so `count` means nodes whatever
  their size.
- **Per decision.** Each burst decision (each generation of the pool's
  CapacityRequest) gets fresh requests, because the spec is immutable and a
  booking lasts ten minutes. First `check-capacity` asks whether existing
  nodes have the room; if they do, it is booked and the queue's units take
  it. If not, `best-effort-atomic-scale-up` adds them, with
  `ValidUntilSeconds` four provisioning delays.
- **Rounds.** A booking that expires, a scale-up that fails or capacity
  that is revoked while the pool is still short starts a new round after a
  minute. Requests the pool no longer needs are deleted. The
  CapacityRequest's status says where it stands.
- **kind-sim** runs alongside it unchanged; each actuator acts only on the
  pools that name it.

Burst decisions are cost-aware:

- A burst is considered when a job is infeasible on current capacity and the
  slots it needs exist in a burst pool it may use.
- Candidate pools are ranked by price per slot-hour, and the scheduler picks
  the cheapest set that restores feasibility after each pool's provisioning
  delay. A pool too slow to help before the deadline is skipped.
- The burst is requested only if its cost is within the project's daily
  burst budget and below the lateness it prevents, priced at the project's
  `lateCostPerHour`. Otherwise the HUD shows the option and its price, and a
  person approves it.
- Bursts are sized from a plan with a margin: the critical path's work is
  counted in W90 and again as the job's tail. What a job reports (at risk,
  late by, ETA) uses Graham's bound without the double count, so a burst the
  margin alone buys is labelled as margin against estimate uncertainty. The
  margin stays even when W90 is calibrated. With W90 calibrated to 90% coverage
  in the stress grid (`hack/stress -calibrated`), sizing bursts from Graham's
  bound instead saved $2.5k of burst against about $63k more lateness at the
  projects' own prices: the overrun tail a calibrated 90th percentile leaves
  still costs more than the margin. Every margin burst passes the same
  economic rule, priced from the lateness the margin plans.
- **Bursts are topped up from what is left.** A job confined to a subset of
  its group's pools takes the more pessimistic of the two plans: the riskier
  plan's shortfall, but the smaller share. At that smaller share it needs
  more, `W90 - share x horizon`.
  - Sizing every burst on the larger shortfall makes bursts close less
    often, so fewer are bought. Sizing on it in deadline order takes budget
    and burst slots from the jobs after.
  - So every job first gets the burst its plan sizes. Then each requested
    burst is topped up toward the larger shortfall, from the budget and
    slots left. The top-up is made only when it covers the rest of that
    shortfall and the whole burst still costs less than the lateness it
    prevents.
  - On the global tutorial farm, jobs late at London's 08:00 went 11 -> 3.
    The reduced stress grid was flat (the PROGRESS row has the numbers).
- Scale-down uses hysteresis: a request shrinks only after a pool's surplus
  has lasted ten minutes, and nodes are released by the autoscaler's own
  consolidation, never by the queue.

**Staging (experimental).** A pool at another site can serve a job once
the job's inputs are there. The caller lists, per pool, the ways to get
them there (`Job.Sources`): move the missing bytes over a link (same zone,
inter-region, internet, private interconnect, shipment), recompute them
from inputs the site has (exact processes, validated ones where the
consumer accepts it), or reuse an identical output. Each source carries its
bytes, link, latency and full price (egress, ingress or processing fees,
outputs back). Links are shared capacity (`Snapshot.Links` with the bytes
already committed), so a transfer waits behind the ones before it. A job at
risk where its inputs are gets the cheapest way that restores its
deadline, across staging (the other site's spare slots under its own
deadlines) and burst, under the same budget and lateness rules. A burst pool
at another site provisions while the inputs move. Units run only where the
inputs are, and a job a stage saves reports the plan with it. What a
subset of pools plans excludes the slots that jobs staged there hold.
Known limit: the spare is what the other site's current plan leaves, so a
site whose own work arrives after a stage can find it holding slots that
work needs.

The queue gets these from a Placement (`--staging`, off by default;
`internal/work/placement`):

- **Sites and links** come from the topology file (`--hud-sites`, also the
  HUD's map), one versioned schema (`work/topology`): sites with their
  region, regional replicas and the projects whose store is there; links
  with their channel, throughput, latency and price at each end. Its types
  are shaped to become Site and Link resources (link capacity is
  facilities' to declare in the long run). A pool's site is its `site`
  attribute or its nodes' region.
- **What a site lacks** is a C4 ID set difference: the objects of the views
  a job mounts, less those the site's regional replicas hold (`HEAD` with
  `X-C4-Cached: only`, which never answers from the store) and those
  already on their way. It is learned in the background and cached; a job
  whose inputs are not known yet runs where it may, as without staging.
  Objects shared by several jobs cross a link once per site, and an
  identical output elsewhere is the same IDs, so it needs no special case.
- **Moving** comes from the project's store site over each link, first come
  first served (the provider keeps the bytes committed to each link), and
  is carried out by `POST /warm` on the target site's regional replicas.
  Warming spends egress, so a replica admits it only for the operator's own
  identity (never anonymously), within a byte budget per day and a pace
  (`--warm-bytes-per-day`, `--warm-bytes-per-second`; off at 0), and
  records each request.
- **Recomputing** an input at the target site is offered from the
  derivation records' cost (`Queue.RecomputeCost`) for exact processes: a
  fresh run writes the same bytes, so the site then holds the input the job
  names. Staging a recompute moves what the site lacks of the
  computation's own inputs and the job's other inputs, then runs the unit
  that produced the input there: a WorkJob made from the producing job's
  spec (class, parameters, pod template), planned as that one unit
  (`c4.avalanche.io/recompute-unit`), reading the views it read pinned by
  ID, in the producing job's pools at the site, due when the stage needs
  it. The record names the unit that last ran each computation; the
  recompute takes that unit from the job's plan as the queue holds it,
  expansions included, and carries its definition. A unit's inputs in the
  record are the views it read: the job's, and other units' outputs at
  the mounts it read them at, so a unit downstream in a job's graph
  reruns with those pinned like the rest. A
  rerun that writes the same output verifies the process; one that writes
  something else demotes it to validated. A validated recompute is
  offered only to a job that accepts one
  (`c4.avalanche.io/accept-validated`), since it writes other bytes than
  the input the job names; volatile ones never are.
- **Input substitution.** At a site where a recompute rebuilt one of its
  inputs as another view, an accepting job reads that view instead: the
  queue keeps, per project, site and view, the outputs of the recompute
  jobs that succeeded, and a unit placed in a pool there mounts the
  substitute through a Materialization the job owns. The pod records what
  it read in place of what, the derivation record takes the inputs the
  unit actually read (the C4 way: what was read, not what was named), and
  status lists the substitutions. The job's spec, its Materializations and
  other jobs never change, and elsewhere it reads its inputs as named.
  While a recompute runs, the view it rebuilds counts as on its way, so
  the job reads as staging; its output's IDs are known only once it has
  run. An exact recompute that wrote other bytes substitutes the same
  way for an accepting job; for any other job the input is moved.

**Pre-warm.** A CapacityRequest lists the views its jobs will mount. The node
daemon on a node joining that pool fetches those layers before it labels its
Node `c4.avalanche.io/cache-warm=true`. The queue releases units to burst
nodes only once they are warm (or after a timeout), so the first frames on a
new node do not wait on a cold cache, and a fleet of fresh nodes fills
through the tiers instead of from the store.

### 10.13 Preemption and checkpoints

- The scheduler preempts only to reclaim a project's nominal quota, or to
  restore feasibility for a job that is infeasible without it, and only from
  jobs whose slack exceeds a threshold. It picks victims by lost work: time
  since the unit's last checkpoint, plus restore cost. It is rate-limited per
  pool, and a unit is preempted at most twice.
  - **What a victim keeps.** A victim job keeps its planned share
    rounded up, so two jobs with fractional shares do not trade a slot.
    Two cases are exempt. A borrower above its nominal gives up what it
    borrowed whatever its share, since that capacity was lent. And a job
    that checkpoints keeps its share rounded down when another job would
    be late waiting for its unit to end (time-slicing).
  - **Time-slicing.** A job planned for a fraction of a slot (an urgent
    job on a one-slot pool, say) that would miss waiting for the slot to
    free takes the slot from a checkpointing unit. That happens when the
    unit is expected to run past the job's latest start: its deadline,
    less what its remaining work needs on its share rounded up. The unit
    yields at its next safe point and loses only its restore cost, and it
    may not take the slot back within the hour. The queue tells the
    policy each running unit's expected time left
    (`RunningUnit.Remaining`). With no estimate, nothing is sliced. A
    job the plan already makes late slices too, up to its share rounded
    up: it misses by less with the slot than waiting for it.
- A unit of a class that checkpoints is asked to yield, not killed. The
  queue sets the annotation `c4.avalanche.io/yield` on its pod; the unit sees
  it through a downward API file (`C4_PODINFO`), saves a checkpoint into its
  working tree at its next step and exits. Capture stores the upper as a
  changeset like any other output, and the queue records the captured view
  as the unit's resume point. The next attempt mounts that view, through a
  Materialization the queue owns, in place of the job's input, so it
  resumes on any node from what it saved. A unit that has not yielded after
  ten minutes is stopped. A yield is not a failure and does not count
  toward retries. P5 measures one end to end: a training run yields its GPU
  slots to an urgent render and resumes from the step it saved.
- Units that do not checkpoint (render frames) are stopped and run again;
  victims are chosen to lose the least work.
- **Only c4 preempts units.** Unit pods run at the `c4-unit`
  PriorityClass (config/manager; `--unit-priority-class`), unless a job's
  template names its own: value 1000, `preemptionPolicy: Never`.
  kube-scheduler never preempts other pods to place a unit, ordinary
  workloads (priority 0) cannot preempt units, and system classes keep
  their precedence. The queue sets the class only while it exists, since a
  pod naming a missing class is refused.
- **Safe points** make the two directions meet. A unit announces each moment
  it could stop and lose nothing it wants kept, whatever its class: the end
  of an agent turn, a frame or chunk boundary, a training checkpoint, a CI
  test file. `c4-unit safepoint [--checkpoint REF]` writes `.c4/safepoint`
  (its number, the time, what was saved), the node daemon reports the last
  one with the unit's progress, and the queue learns how often each unit
  reaches them.
- **Arriving at a safe point is a decision point**, not an automatic stop.
  The command asks, and waits briefly (`--wait`, 20 s) for the answer: the
  node daemon sets the pod's `c4.avalanche.io/safepoint` annotation, the
  queue decides on its next tick and answers in
  `c4.avalanche.io/safepoint-decision`, and the daemon writes the answer
  into the pod's control directory (`$C4_CONTROL`, an emptyDir it reaches
  through kubelet's pods directory). `stop` (exit status 10): the unit
  exits there, capture keeps what it wrote, and the next attempt resumes
  from it, as for a yield. `continue` (0): it goes on.
  - A soft stop asked earlier (the yield annotation, with its reason, and
    `c4.avalanche.io/yield-by`, its deadline) is advisory. At the safe point
    the scheduler reconsiders it: a hold-stop holds while the job is on
    hold; a capacity shrink while the pool is still over its slots; a stop
    for another job while that job still waits for slots, is still at
    risk, and the pool has no free slots for it. A unit asked to stop for
    another job that is nearly done (90%) goes on: stopping it would lose
    more than it frees. A stop that no longer holds is cancelled.
  - With no stop pending, the queue may move the unit if its class allows
    it (render and ml): a pool the job may use costing at most three
    quarters as much per slot-hour, with room now, for a unit with at
    least 20 minutes left and a job not at risk. The answer is stop, and
    the next attempt is released into that pool.
  - Each stop, each cancelled stop and each move is signed in the book
    (decision/v1: `safepoint-stop`, `safepoint-continue`, `safepoint-move`,
    with the safe point, why, and the stop it answers); a plain continue is
    not.
  - **SIGTERM is a yield.** Every voluntary disruption (a Karpenter or
    `kubectl drain`, an eviction, a spot notice, a node shutdown) sends
    SIGTERM and waits the pod's grace period. The queue sets the grace
    when the template does not: the job's learned cadence between safe
    points, plus half again and 30 seconds for the decision; else the job's
    `checkpointSeconds`; else the class's checkpoint time (ml five minutes,
    agent three). It is bounded to 30 seconds to 15 minutes. A unit at a
    safe point while its pod is being deleted is told to stop there, and
    its pod is marked as yielded (`disruption`), so its exit resumes, with
    the decision signed. A unit that never reaches one is killed at the end
    of its grace and counted as interrupted (section 10.14).
  - **Units that cannot stop softly are protected instead.** A unit whose
    class does not checkpoint, in a job that has shown no safe points,
    with a predicted run of 15 minutes or more, carries Karpenter's
    `karpenter.sh/do-not-disrupt` annotation with a duration: its
    prediction plus two standard deviations, rounded up to the minute and
    at most a day. Karpenter then keeps its node out of consolidation and
    drift for that long after the pod starts. Expiry, spot interruption and
    deletion still happen; those attempts are interrupted.
  - If no answer arrives in time (the controller is unreachable), the unit
    falls back on the last request it sees through its downward API file:
    it stops only if a stop is still pending. The hard stop at the
    deadline remains the fallback for a unit that never reaches one.
  - A unit that reports safe points is therefore stopped softly, not
    killed, and what stopping it loses is the time since its last safe
    point, which the scheduler uses to choose victims. The HUD shows each
    unit's last safe point and cadence, what the queue would decide at the
    next one as things stand (`c4.avalanche.io/safepoint-next`: stop,
    continue, or undecided), its last decision, and for a unit asked to
    stop, when its next safe point is expected and its deadline. An
    agent's Claude Code session reaches one at each turn's end, from a
    Stop hook.
- A gang is preempted whole: the queue asks it to yield and suspends the
  wrapped object once it has checkpointed (section 10.17). Members are never
  evicted one by one.

**Derivations (experimental).** Every unit that succeeds cleanly is a
derivation: process + inputs → output, with what it cost. The process is
the unit's images by digest, command, arguments, environment (less what
names the run) and, on a GPU node, its platform (GPU model, driver,
compute stack). The inputs are the C4 IDs of every view it mounted; the
output is the view it wrote. Views are projected, so equal IDs mean equal
bytes. The queue keeps one record per project (`work/process`): input IDs
identify content, so a shared record would tell one project what another
holds.

The record answers three things. The first is what recomputing an output
costs: work and its 90th percentile in slot-seconds, plus the longest
unit, for the scheduler to weigh against moving the output. The second is
whether an identical output already exists to reuse in its place. The
third is what a unit that ran before with the same process and inputs
will take, which becomes its estimate.

Reuse needs trust, and trust is evidence, kept per program (the process
without what tells one unit from another, such as frames or shards):

- what the job declared, with the annotation
  `c4.avalanche.io/derivation-class` (`exact`, `validated` or `volatile`,
  the default);
- whether its pods were sealed: images pinned by digest, no host network or
  paths, no environment from secrets, and a network policy denying egress;
- how many re-runs of the same process on the same inputs wrote the same
  output, and how many wrote another.

A program is exact if it is declared exact, sealed and never contradicted,
and verified once two re-runs matched. Only exact and verified allows
reuse, and even then 5% of reuse opportunities are recomputed to keep the
declaration checked. A mismatch, or an unsealed pod, makes an exact
declaration read as validated, recomputed but never reused, and
`status.derivation` says why. An undeclared program stays volatile however
often it repeats; its matching re-runs are shown as evidence a person may
use to declare it. This follows C4's derivation-record design
(c4/design/derivation-records.md) and feeds its redesign
(proposals/eta-deadlines/DERIVATION-LESSONS.md); the annotation, the status
field and the record will change with it.

### 10.14 Failures, retries and blacklists

- A unit is retried up to `retries` times (2 by default; `0` is none),
  and its retries keep off the nodes it failed on. When the farm's own
  avoidance (the unit's failed nodes, then the job's blacklist) would leave
  it no node of the pool, that gives way, so a unit is never kept off every
  node by the farm's guesses. Only nodes a person named stay avoided
  whatever happens.
- When a node accumulates `nodeErrorLimit` failures for one job, the job
  blacklists it, and later pods carry a node anti-affinity rule. People can
  add a node to a job's blacklist, or disable a node farm-wide (a taint),
  from the HUD.
- A node that fails units across several jobs and projects is flagged as
  suspect, which usually means bad hardware rather than a bad job.
- **Causes** (integrations GAPS 4). A job's failures and flags are
  grouped by cause in `status.causes`: a unit's exit (its code, the
  container's reason such as OOMKilled, the first line of its message with
  numbers blurred), a node that failed units up to the job's limit, a
  review's reason, information a brief lacked. Each records the units it
  touched (a unit set), how many attempts, a sample, its first and last
  time, up to four nodes, and what the farm did: retried, failed,
  blacklisted, reworked, escalated (and to whom), accepted. A failed pod
  counts once. There are at most 16 causes; a new one drops the one seen
  least recently, counted in `causesDropped`. Status is their only record,
  loaded back after a restart. The HUD's incidents group failed jobs by
  cause, and a tracker gets one comment per cause.
- **Interrupted is not failed.** An attempt the farm stopped is
  interrupted. It spends no retry, marks no node, and runs again. That
  covers the pod's `DisruptionTarget` condition (scheduler preemption, an
  Eviction API call such as a drain, a NoExecute taint, the kubelet's
  node-pressure eviction or graceful shutdown, PodGC) and a pod deleted
  while it ran. Every unit pod carries the `c4.avalanche.io/unit`
  finalizer, so a deleted pod stays readable, holding its slot, until it
  ends and the queue has recorded it with what its node captured. A pod
  deleted before it was placed, or whose node never ends it, is let go.
  Status lists the latest interruptions with their reasons and counts them.

### 10.15 Render class

- Units are chunks of frames of one pass, rendered through a Hydra delegate
  (Karma XPU or CPU, RenderMan, Arnold, V-Ray) to OpenEXR. With
  `expand: true`, each frame (or chunk) starts as a generator that exports
  the frame's USD stage (from Solaris, or a layout or animation publish to
  USD layers) and emits its passes, for example deep env and volumes layers
  that the beauty layer holds out against.
- The objective is a deadline: the job's own, or a Milestone selecting its
  shot or delivery, or the class default turnaround.
- Order is per job: `Sequential`, `Reverse` or `Binary`. The `render-preview`
  and `render-dailies` classes default to `Binary`.
- The estimator is keyed by pass and interpolates along the frame axis
  (section 10.8).
- Outputs are captured per unit and folded into the job's output view as
  units finish (each fold costs only that unit's changes), so the output
  view in status is current to the second. A render task that writes
  proxies (`preview/<frame>.jpg`) makes them visible to the preview player
  through that view.
- The lens groups units by pass, 24 frames per row (one second of screen
  time per row), with the cost-by-frame strip and the preview player
  (section 11.4).

### 10.16 CI class

The Scion case (section 8.2) running on the same farm.

- **Pipelines per change.** A change (a commit view and its changeset from
  `import-git`) gets one WorkJob per pipeline stage group, labeled with the
  change. Units are steps (build) and test shards. Test discovery is a
  generator that emits the shards a change needs.
- **Order.** Shards run in order of historical failure rate, so a broken
  change fails fast. Shard indexes have no locality, so binary order has
  nothing to spread over.
- **Objective.** A latency SLO per WorkClass, for example `ci-presubmit`
  with p50 6 min and p95 15 min, and `ci-merge` with a tighter target.
  Effective deadline = ready time + p95 target (section 10.7).
- **Checkout.** Each unit mounts the change's view, sparse by the step's
  include patterns. A node that ran the parent commit adds one delta layer.
  Checkout time and outcome (reused stack, delta, or cold) are publish
  metrics labeled by class.
- **Flaky tests.** A view ID is the exact identity of a shard's inputs. A
  shard that fails and then passes on the same view ID, on a retry or in
  another pipeline, is flaky by definition, with no heuristics about
  timing. The class records (target, input view, result) and the lens ranks
  flaky targets.
- **Result reuse.** A shard whose input view (its include set at the change)
  already passed is marked passed without running, as Bazel does with input
  digests. This is opt-in, because it trusts the declared include set to
  cover every input.
- **Merge queue.** Changes queued to land are tested in order, each on a
  speculative base: the queue head with the changesets ahead of it applied.
  C4 changesets compose, so a speculative base is a view computed from
  records, and nodes mount it as a thin delta over the head. A failing change
  is ejected and the changes behind it are retested on a corrected base.
  Health is depth, time in queue (p50, p95), ejection rate and merges per
  hour.
- **Runner pools** are ordinary pools (for example `ci-cpu`), shared with
  render under quotas.

### 10.17 ML class

ML here means what studios run: fine-tuning, LoRAs, training on studio data,
and generative batch jobs. Large-scale training infrastructure is not built
(section 19).

- **Wrapping, not reimplementing.** An ML WorkJob wraps a JobSet (or a
  Kubeflow TrainJob, a LeaderWorkerSet, or a RayJob). The class creates it
  suspended, with Kueue's queue label, and unsuspends it when the work queue
  releases the unit. Kueue with JobSet provides what training needs and this
  system does not implement: all-or-nothing admission of the ranks,
  topology-aware placement in one rack or fabric domain, and failure
  policies. A gang member that fails is handled by JobSet, which restarts the
  whole set.
- **What this system owns:**
  1. **Data.** Datasets and checkpoints are versioned C4 trees. Training data
     is materialized through the same tiers and overlays as render inputs.
     Checkpoints are captured from the run's working tree, so the store
     deduplicates every tensor file that did not change between checkpoints.
     A run is reproducible from its tree IDs: dataset view, code view, base
     checkpoint view, and output checkpoint view, all recorded in status.
  2. **Sharing capacity.** Training competes with render and CI under the
     same scheduler. Its objective is a deadline if it has one, otherwise a
     GPU-hour rate. When a render deadline is at risk and GPU capacity is the
     shortage, the queue asks the training run to yield at its next
     checkpoint boundary (annotation `c4.avalanche.io/yield`, which the run's
     training loop or a small sidecar reads), waits for the checkpoint to be
     captured, then suspends the wrapped object. Kueue releases the quota.
     When capacity returns, the run resumes from its latest checkpoint view.
  3. **Visibility.** The lens shows one box per rank (or per trial in a
     sweep, where the sweep planner is a generator), and **useful GPU
     hours**: GPU time whose work survived in a checkpoint or a finished run,
     against GPU time lost to preemption and failure.
- Sweeps are generators: the planner emits trials, each a wrapped run, and a
  successive-halving planner is a generator that expands again after each
  rung. A grid sweep emits its trials along each axis of the grid, so binary
  order covers the grid coarse to fine with no extra mechanism: the corners
  and center first, then finer points.

### 10.18 Adding a class

| class                                 | unit                      | SLO                    | pools             | estimator                                | lens                        |
| ------------------------------------- | ------------------------- | ---------------------- | ----------------- | ---------------------------------------- | --------------------------- |
| media ingest, transcode, QC, delivery | a clip or segment         | deadline (delivery)    | cpu, gpu encoders | real-time factor times media duration    | clips by reel, QC flags     |
| simulation caches                     | a frame range, sequential | deadline               | high-memory       | per-frame time, growing with the sim     | the sim's frames in order   |
| generative batch                      | a batch of prompts        | throughput or deadline | gpu               | items per second by model and resolution | batches and samples         |
| interactive sessions                  | a session                 | latency to start       | gpu workstations  | none (a reservation)                     | sessions and idle time      |
| data movement                         | a tree or object batch    | throughput or deadline | io nodes          | bytes over measured bandwidth            | transfers and tier counters |

Each is a driver implementing the interface in section 10.4, a WorkClass
object, and a lens module. A sequential simulation is a chain of units, and
the critical-path floor in the slack formula (section 10.9) already handles
it.

### 10.19 Scale

- One controller holds the queue in memory and is the only component that
  watches task pods (label-selected). It rebuilds its state from WorkJobs,
  their stored unit tables, and task pods after a restart.
- Status writes are coalesced to at most one per job per second.
- Pods are created only for runnable units, so the number of pending pods is
  bounded by free slots, not by queue depth.
- Task pods are kept until the job is deleted (logs live with them) unless
  the job sets `keepSucceededUnits: false`.

### 10.20 Simulation harness

The policy is a pure Go package. The controller calls it, and so does a
discrete-event simulator in `hack/sim`, so **the simulation runs the
production policy code**. The simulated farm has CPU, GPU and CI pools, spot
and on-demand burst pools with provisioning delays and prices, and a week of
synthetic work:

- render jobs with dailies due at 08:00 and client deliveries on Fridays,
  evening submission surges, lognormal frame times with heavy stretches
  inside a shot, generators that expand into passes, and task failures;
- CI pipelines arriving during working hours with a p95 latency SLO;
- ML fine-tuning runs that checkpoint every few minutes and yield on
  request.

It replays the same workload, over several seeds, under:

- manual priority (static priorities by show and task type, plus a wrangler
  who bumps late jobs every two hours);
- FIFO;
- EDF;
- the objective-driven policy, with and without burst.

It reports missed deadlines, total late hours, CI p50 and p95 time to green,
utilization, useful GPU hours, and burst cost.

A second experiment isolates the estimator: synthetic sequences with heavy
regions (a hero FX section at a random place, at several cost multiples) are
rendered in sequential and in binary order, and the error of the estimated
remaining time is recorded against elapsed time, together with how soon the
heavy region is flagged and how soon the job's slack estimate settles within
10% of the truth.

A smaller version runs on the test cluster with real pods and the `kind-sim`
actuator (a simulated capacity provider).

## 11. HUD (P5)

### 11.1 Information architecture

- **Farm.** Slots in use and free per pool, queue depth by class, units per
  hour, failures in the last hour, burst in flight and its cost, tier health,
  and **bytes avoided** (section 12.1): what content addressing, the cache
  tiers and the overlays saved, today and to date.
- **Projects.** A scope, not a page. Every view is filtered to the projects
  the viewer may see.
- **Jobs.** In dispatch order across classes, each with its job view
  (section 11.3), objective, ETA band, slack and feasibility, owner, pools,
  inputs and output view.
- **Units.** For each unit: state, attempts, node, duration against
  estimate, exit reason, log tail, output changeset, and for generators what
  they expanded into.
- **Workers.** Nodes grouped by pool and locality: state (idle, busy,
  disabled, not ready, unprovisioned, warming), slots used, recent failures,
  cache warmth.
- **Cache tiers.** Node, peer, regional and store: hit rate, bytes served,
  upstream fetches, fan-in (requests in per request out), shed rate,
  in-flight fills, publish latency.
- **Dependency graph.** Jobs across classes as a graph (sim to render to comp
  to transcode to delivery, or build to test to merge), with the critical
  path, propagated deadlines, and the views flowing along edges.
- **Cost and capacity.** Per pool: capacity, use, requested burst and its
  state, spend against budget per project, and the required-slots curve over
  the next 48 hours against capacity, with what-if: "+40 gpu-spot slots
  makes these three jobs feasible for $410".
- **Provenance.** Any frame's lineage graph, back to its sources and forward
  to its derivatives, with record verification and generative steps marked,
  and the disclosure report for a delivery (section 9.6).

### 11.2 At a glance

| who               | needs at a glance                                                                                                                                                              |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| artist            | my jobs; which frames are done, running, failed; ETA against deadline; why a frame failed (log lines, node, exit reason); the output view to hand to comp                      |
| render wrangler   | every job in scope in dispatch order; feasibility warnings with the burst that fixes them and its price; stuck jobs; failure hot spots by node; slots by project against quota |
| CI wrangler       | queue wait against run time; checkout time and cache hit; pipelines per change; time to green against the p50 and p95 targets; flaky tests; merge-queue health; runner pools   |
| ML user           | my runs as boxes per rank or trial; checkpoints as views; when and why a run yielded; useful GPU hours                                                                         |
| platform engineer | tier hit rates and fan-in; shed rates; regional replica health; store request rate; node cache pressure; publish and capture latency; capacity requests and their state        |
| production        | cost against budget per project and pool; what a burst would buy; which deliveries are at risk; bytes avoided and worker-hours per project                                     |

### 11.3 The job view

The job view is the signature of the HUD, after the job view of Pixar's
render queue, and it is the same for every class. It is a sheet: the job's
shot (every job carrying the same `c4.avalanche.io/shot`, and the deliveries
they feed) laid out as one STACK, in the order work flows.

- **One row per frame, dependencies to the right.** The shot is one block;
  it spans its batches (the job script's batch generators, or 24 frames,
  one second of screen time, when it has none); a batch spans exactly the
  rows of its frames; a frame's stages sit side by side in its row:
  upstream work (sim caches, USD export), its passes, render, then comp,
  transcode and delivery, which span the rows they read. Containment is
  shown by adjacency, never by lines.
- **Expansion is visible.** An unevaluated generator is a stippled block
  with its expected children drawn as dashed outlines; when it expands, its
  row opens to the width of its widest stage (shadow and reflection side by
  side, then beauty).
- **Progress fills from the bottom.** A running unit has a blue body with a
  breathing leading edge, and green rises inside it by measured progress
  (a line in `.c4/progress` inside its volume, read by the node daemon), or
  by elapsed time over the estimate, striped so an estimate never passes for
  a measurement. Done is green; a finished upstream job is a settled green
  one step down. Earlier failed attempts are orange notches while the new
  attempt fills; failed is red. Progress never touches the API server.
- **Any container collapses to its aggregate.** A collapsed shot, batch or
  folded run of upstream stages is one block: green to the share done of
  everything inside and upstream of it, by work (slot-seconds) where the
  estimate exists, a blue sliver for the share running, an orange tick if
  anything inside is retrying, a red tick if anything failed, a dashed red
  outline if any of it is projected late. By default a batch opens only
  where something needs attention (a failure, a retry, a heavy region).
- **Lines only where grouping cannot say it.** A: a dependency on a job
  outside the sheet (delivery wk43 also needs the other shot's transcode),
  on by default. B: a unit that reads other rows (a temporal comp reading
  frames n-1 to n+1), off by default, per job.
- **Slack and risk on the same view.** The collar's title block carries the
  deadline in ink, the ETA band (p50 to p90) and the slack in pencil; waiting
  units the scheduler projects after the deadline carry a red pencil tick;
  time lines (projected finish contours) cross the rows, with the deadline
  as a red pencil datum. The cost per frame runs beside the rows, measured
  in ink, estimated as a band.
- **Scale.** The sheet is drawn on a canvas; very large jobs batch more
  frames per row so the sheet stays a few hundred rows tall.

### 11.4 Lenses and views

- **Render lens:** the job view by pass and seconds; the **cost-by-frame
  strip** under the grid, measured frame times as bars and interpolated
  estimates as a band with error bars, heavy regions marked; the **preview
  player**, which plays the shot at its frame rate as soon as the coarse
  frames exist, holding each rendered frame until the next rendered one, and
  sharpens in time as more frames land (it reads proxies from the job's
  output view through the regional tier, with the HUD's own tier identity,
  only for viewers of that project); the output view and its changesets.
- **CI lens:** changes as rows of pipelines; queue wait and run time as
  paired bars per stage; checkout time with its cache outcome; time to green
  against the p50 and p95 lines; flaky targets ranked; the merge queue with
  its speculative bases, depth and ejections; runner pool use.
- **ML lens:** ranks and trials in the job view; checkpoints as views on a
  timeline; yields and resumes; useful against lost GPU hours.
- **Dependency graph view** and **cost and capacity view** as in section
  10.1.

A lens is a JavaScript module registered by name. It receives the class
summary (section 10.4) and the unit table, and it draws into the panel the
HUD gives it. Adding a class's lens does not change the HUD shell.

### 11.5 Controls

Every control is one Kubernetes write, and RBAC alone decides what a person
may do: the HUD asks the API server whether the user may make that write (a
SubjectAccessReview for the verb and the object), makes it with its own
service account, and records an Event on the object naming the user. That
is the as-built form. Impersonation, the alternative, makes RBAC check the
write itself, but it needs the `impersonate` verb on users and groups,
which lets the HUD act as anyone; checking first and recording the actor
keeps the HUD's own permission to the few objects it writes. Each control
has an MCP tool with the same effect (section 13.2).

| control             | effect                                                                                                                |
| ------------------- | --------------------------------------------------------------------------------------------------------------------- |
| hold / release      | `spec.nudge.hold` on the WorkJob (`holdStop` also stops running units)                                                |
| boost               | `spec.nudge.boost` (1 to 2)                                                                                           |
| set deadline        | `spec.deadline`                                                                                                       |
| requeue units       | annotation `c4.avalanche.io/requeue: <token>:<units>`, carried out once by the queue                                  |
| avoid node (job)    | append to `spec.avoidNodes`; allowing a node the queue blacklisted itself is annotation `c4.avalanche.io/allow-nodes` |
| disable node (farm) | cordon the node (`spec.unschedulable`); running units finish                                                          |
| approve burst       | `spec.approved` on the CapacityRequest                                                                                |

### 11.6 Real-time updates at 10,000 nodes

- Unit state reaches the HUD through WorkJob status (run-length encoded,
  written at most once a second per job by the scheduler, which watches task
  pods anyway). The HUD never watches pods.
- The HUD backend holds a constant number of watches: WorkJobs, Nodes (with
  a transform that keeps only labels, taints, conditions and allocatable),
  Projects, Pools, WorkClasses and CapacityRequests. Browsers add no
  API-server load: they receive server-sent events, coalesced to at most one
  update per second per scope.
- Tier, node and progress metrics come from Prometheus with aggregate
  queries (sum by tier, pool, locality, class and project; the top-N worst
  nodes; progress only for the job on screen). The HUD never scrapes 10,000
  nodes. Without Prometheus (the kind e2e) it scrapes the regional replicas
  and node daemons directly, which is fine for small clusters.
- Events and logs are read on demand: opening a unit fetches that pod, its
  log tail and its Events (field-selected by involved object), as the user.

### 11.7 Per-project isolation

- Authentication is a bearer token (an OIDC ID token or a service-account
  token) validated with TokenReview, or the user and groups an SSO proxy in
  front of the HUD sets (`X-Forwarded-User`, `X-Forwarded-Groups`, trusted
  only with `--hud-trust-proxy`). A development mode (`--hud-dev-viewers`)
  takes the viewer from `?as=`; the e2e cluster uses it and nothing else
  should.
- A viewer's scope is the set of projects whose namespaces the viewer may
  list WorkJobs in, checked with SubjectAccessReview and cached per user for
  a minute.
- The event stream a browser receives is filtered on the server. Job names,
  units, logs, views and per-project tier series of other projects never
  leave the server.
- Farm totals are shown as aggregates ("other projects: 290 slots").
  Workers are shared infrastructure, so node names and health are visible,
  but what a node is running is shown only for the viewer's projects. Cost
  is shown per project only to viewers of that project.

### 11.8 Graphic design direction

GRATICULE: designed for the wall of a machine room and for an artist's
second monitor, not as an admin console. The design system lives in
`internal/hud/web/design` (tokens, components, previews and a README on the
thesis and motifs).

- **Every screen is a sheet in a series.** A neatline bounds the data; the
  collar says what, when and at what scale; the neighbouring sheets are
  named at the edges (upstream jobs, downstream jobs, sibling shots).
- **Value carries depth.** A 20-step grey ramp, packed densely below
  L*20, layers the room, the sheet, recessed wells and raised plates.
- **Four colors carry state, and nothing else is colored.** Green is
  completed work, blue is work running now, orange is temporary trouble the
  farm is handling (a retry, a backoff, a suspect node), red is failure and
  lateness (dashed red pencil when it is projected, not yet true). Held is a
  grey hold mark; selection a paper outline; burst capacity ordinary slots.
- **Ink and pencil.** Measured values are ink and never move; estimates are
  pencil, lighter and dash-underlined. Every action is an entry in the book,
  signed by whoever acted, with that signer's seal: a node, the queue, the
  controller, an importer, or the HUD for a person or an agent.
- **Type.** IBM Plex Sans Condensed for labels and the glance numbers, IBM
  Plex Mono with tabular figures for every value, frame and ID; C4 IDs are
  printed as grid references (`c43L 2MDG ... UypQ`), the full ID on hover.
- **Motion is meaning.** Fills tween between snapshots, a running unit's
  edge breathes, a finished unit glows and settles over a minute, a
  collapse slides; the wall's sweep advances between snapshots and its
  history fades over fifteen minutes. All of it stops under
  `prefers-reduced-motion`.
- **Scales.** The desk; the wall (`#/wall`: every job a channel sweeping
  toward its deadline, mission clocks, the strobe strip, the shots
  collapsed, the book); the side widget (`#/widget/ns/name`, 240 to 420 px);
  the phone (one column, one decision per screen).

## 12. Usage metering

The open-source project is the path to market. Revenue is expected to come
from metered deployments sold through the AWS, GCP and Azure marketplaces,
which bill the customer. The queue is not what is sold. The final pricing
model is not decided, so the accounting is general enough to support the
candidates without new measurement:

- **cluster-hours plus container-hours**: hours an installation runs on a
  cluster, plus hours of containers it manages (task pods and pods that
  mount its volumes);
- **fixed production windows**: a declared period (a show's production
  months) billed as a static, much higher fee, during which usage is still
  recorded and attributed but not priced per unit.

Usage is recorded per project, attributable to the component that measured
it, tamper-evident, and exportable in the shape the marketplaces accept.

### 12.1 What is measured, and by whom

| quantity                                | measured by                   | used for                                                                                                                                      |
| --------------------------------------- | ----------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| cluster-hours                           | the meter (manager)           | billing candidate: hours the installation is running                                                                                          |
| container-hours                         | node daemons, work controller | billing candidate: hours of managed containers, per project and pool                                                                          |
| production windows                      | the meter                     | billing candidate: declared periods and the usage inside them                                                                                 |
| GPU-hours, slot-hours by pool and class | work controller               | reporting, capacity planning, cost attribution                                                                                                |
| bytes materialized                      | node daemon                   | proof metric                                                                                                                                  |
| bytes fetched                           | node daemon, regional tier    | proof metric, per tier                                                                                                                        |
| bytes avoided                           | derived                       | proof metric: materialized minus fetched from the store, by source (node cache, layer reuse, duplicates within a view, peers, regional cache) |
| bytes stored and deduplicated           | regional tier (ingest)        | proof metric: new bytes written against bytes already present                                                                                 |

**Bytes avoided is a proof metric, never a billing unit.** It shows what
content addressing, the cache tiers and the overlays save, and the HUD shows
it per project, per job and farm-wide (section 11.1). Pricing by bytes saved
would be commodity pricing that falls as the system gets better at its job.

### 12.2 Tamper-evident accounting

- Each producer (a node daemon, a regional replica, the work controller)
  keeps counters per project and dimension and, every hour, signs a report
  with an Ed25519 key it generated at startup. It registers the public key
  with the meter once, authenticated by its service-account token, so a
  report is attributable to one node or replica and a compromised node cannot
  write another's reports.
- The meter (in the manager) verifies each report, sums them per project,
  dimension and hour, and appends one ledger entry per hour: the totals, the
  open production windows, the IDs of the reports it included, and the ID of
  the previous entry. Entries are content-addressed objects in the
  installation's system store, so changing any past entry changes its ID and
  breaks the chain, and a missing hour is a visible gap. The chain head is
  kept in a `UsageLedger` status.
- The key-release audit log (section 6.6) uses the same chained format.

### 12.3 Export

```go
type Exporter interface {
    // Export reports one hour's usage. It must be idempotent per
    // (hour, dimension, allocation), because the meter retries.
    Export(ctx context.Context, hour time.Time, records []UsageRecord) error
}
```

| exporter | interface                                                                                                  |
| -------- | ---------------------------------------------------------------------------------------------------------- |
| AWS      | Marketplace Metering Service `MeterUsage`, hourly per dimension, with `UsageAllocations` tagged by project |
| GCP      | usage reports to the Service Control API, as the Marketplace usage-based billing agent (ubbagent) does     |
| Azure    | Marketplace Metering Service `usageEvent` and `batchUsageEvent`, hourly per dimension                      |
| open     | Prometheus series and JSON lines, for self-hosted installs and for reconciling with marketplace invoices   |

A production window is exported as its own dimension (one unit per window
hour, or one fixed charge, depending on the listing), so either pricing
model maps onto the same records.

## 13. Agent-native interface (P6)

Agents are first-class users of the farm, from day one. This system is
itself being designed and built with Claude, working as Joshua's engineering
agent, and the same holds for the people who will run it: a wrangler's
assistant, a CI bot, or a migration agent should be able to do anything a
person can do in the HUD, under the same rules.

### 13.1 One API, three faces

The HUD backend's REST API is the single implementation. The HUD, the MCP
server (`c4-operator mcp`) and the CLI are faces of it, so **every HUD
capability has an agent-callable equivalent** by construction. All three act
as the caller: the caller's own bearer token, passed through to the HUD
(from a token file over stdio; from each request's own `Authorization`
header over HTTP), so the viewer scopes of section 11.7 and the per-write
checks of section 11.5 bind agents exactly as they bind people, and the
Events record which user an agent acted for.

### 13.2 MCP server

**As built** (`internal/mcpserver`, on the official Go SDK v1.8.0, which
speaks MCP 2026-07-28): thirty tools, every one a call to the HUD API.

| group   | built                                                                                                                                                                                                                                                                                                   | annotations                           |
| ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------- |
| inspect | `c4_farm_status`, `c4_list_jobs` (filters, `limit`, `offset`, `nextOffset`), `c4_get_job` (units on request), `c4_get_unit` (log tail, events), `c4_list_nodes`, `c4_cache_tiers`, `c4_capacity_and_cost`, `c4_dependency_graph`, `c4_diff_views`, `c4_read_book`, `c4_trace_file`, `c4_verify_custody` | read-only, closed-world               |
| plan    | `c4_suggest_deadline` (the suggestion, its source and band), `c4_deadline_options` (what candidate deadlines would take, section 10.7)                                                                                                                                                                  | read-only, closed-world               |
| act     | `c4_hold_job`, `c4_boost_job`, `c4_set_deadline` (with `mode`: pin, suggested, tracking), `c4_move_to_next_session`, `c4_avoid_node`, `c4_disable_node`                                                                                                                                                 | idempotent, not destructive           |
| act     | `c4_requeue_units`                                                                                                                                                                                                                                                                                      | destructive (discards results)        |
| grant   | `c4_grant`, `c4_list_grants` (read-only), `c4_revoke_grant` (destructive): a person's leave for a tracker to submit work for them (integrations GAPS 17)                                                                                                                                                |                                       |
| spend   | `c4_approve_capacity`                                                                                                                                                                                                                                                                                   | open-world (provisions paid capacity) |

Errors come back as tool results that say what to do next ("Job names are
namespace/name; call c4_list_jobs to see the jobs you can see"). The tool
reference (`docs/agents/tools.md`) is generated by `c4-operator mcp
--tools-md` from the server's own tool list, so docs and schemas cannot
drift. The rest of this section is the full design; the tools it names
beyond the table above are not built.

- **Transports.** stdio for a local agent (Claude Code on a wrangler's
  machine), and Streamable HTTP, stateless JSON, for a shared in-cluster
  server behind the cluster's auth proxy. Both are built.
- **Tools.** Named `c4_<verb>_<noun>`, with input and output schemas, a
  `response_format` of JSON or Markdown, pagination (`limit`, `offset`,
  `has_more`) on every list, and annotations (`readOnlyHint`,
  `destructiveHint`, `idempotentHint`, `openWorldHint`) that match what the
  tool does. Errors come back as tool results with a next step ("job sh040 is
  held; release it with c4_release_job or ask its owner"), never as stack
  traces.

| group      | tools                                                                                                                                                                                                                                                                                                                                               | annotations                                                      |
| ---------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| inspect    | `c4_get_farm`, `c4_list_jobs`, `c4_get_job`, `c4_get_unit`, `c4_list_nodes`, `c4_get_tiers`, `c4_get_graph`, `c4_list_capacity_requests`, `c4_browse_view`, `c4_diff_views` (changed paths, objects and sizes between two views, or a commit and its merge base), `c4_follow_chain` (a chain's checkpoints after a given one, waiting for new ones) | read-only                                                        |
| explain    | `c4_explain_job` (rank, slack decomposition, the limiting factor, the burst that would fix it), `c4_diagnose_failures` (failed units grouped by cause, suspect nodes, flaky shards, suggested actions), `c4_what_if_capacity`, `c4_get_cost`                                                                                                        | read-only                                                        |
| lenses     | `c4_list_ci_changes` (pipelines, time to green, flaky targets, merge queue), `c4_list_ml_runs` (ranks, checkpoints, yields, useful GPU hours), `c4_get_preview` (frame list)                                                                                                                                                                        | read-only                                                        |
| act        | `c4_submit_job` (with `dry_run`, which returns the estimate and feasibility first), `c4_hold_job`, `c4_release_job`, `c4_boost_job`, `c4_set_deadline`, `c4_requeue_units`, `c4_blacklist_node`                                                                                                                                                     | writes; idempotent with a name or nonce                          |
| farm       | `c4_disable_node`, `c4_approve_burst`                                                                                                                                                                                                                                                                                                               | writes; `c4_approve_burst` spends money and is marked open-world |
| migrate    | `c4_import_deadline_jobs` (dry run produces the translation report), `c4_set_deadline_routing`                                                                                                                                                                                                                                                      | writes                                                           |
| provenance | `c4_trace_file`, `c4_trace_forward`, `c4_verify_record`, `c4_disclosure_report`, `c4_export_c2pa`, `c4_export_omc`                                                                                                                                                                                                                                  | read-only                                                        |
| providers  | `c4_get_capacity_forecast`, `c4_get_ingress` (section 16.1)                                                                                                                                                                                                                                                                                         | read-only                                                        |
| vendors    | `c4_get_license_usage`, `c4_export_license_usage` (section 16.2)                                                                                                                                                                                                                                                                                    | read-only                                                        |

- **Within policy.** Reprioritizing is the nudges of section 10.9 and nothing
  else: hold, boost between 1 and 2 from the caller's own project quota, and
  deadlines. An agent has no priority knob a person does not have.
- **Resources and prompts.** Jobs, units and views are also resources
  (`c4://jobs/{namespace}/{name}`, `c4://views/{project}/{id}/{path}`), and
  the server offers prompts for the common playbooks: triage a failing job,
  make a deadline, migrate a Deadline job, explain the farm's cost.

### 13.3 Claude skill

A skill, `c4-farm` (`skills/c4-farm/SKILL.md`, built), teaches an agent to
use the server well. Its
`description` says what it does and when to use it (render jobs, frames,
deadlines, CI pipelines, training runs on a c4 farm, and Deadline
migration). Its body holds the playbooks, and its reference files hold the
concepts an agent needs and does not know (views and changesets, slack and
required slots, binary order, generators, nudges) and a tool reference.

### 13.4 Documentation for humans and agents

- The tool reference is generated from the tool registry, so the docs, the
  MCP schemas and the skill's reference cannot drift apart.
- Every concept page leads with a short definition, then the details, and
  each page is plain Markdown with stable anchors, indexed by an `llms.txt`
  at the docs root.
- An evaluation set of questions with verifiable answers, run against the
  deterministic demo farm, checks that an agent using only the MCP server can
  answer what a wrangler would ask ("which node is failing sh040's frames,
  and what would it cost to make Friday 18:00?").

## 14. Deadline migration bridge

Deadline 10 is in maintenance mode, and studios have years of jobs, plugins
and submitters built on it. The bridge lets them move gradually, and agents
can drive it through the MCP tools of section 13.2.

- **Submission compatibility.** An endpoint that accepts the Deadline Web
  Service's job submission (`POST /api/jobs` with `JobInfo` and
  `PluginInfo`), and a `deadlinecommand`-style CLI that accepts job and
  plugin info files. Existing submitters work by pointing them at the bridge.
- **Job translation.** `Frames` and `ChunkSize` become units, `Pool`,
  `SecondaryPool` and `Group` become pool requirements, `Whitelist` and
  `Blacklist` become node rules, `JobDependencies` become `dependsOn`,
  `MachineLimit` becomes the parallel cap, `OutputDirectory` becomes capture,
  and `Priority` becomes a bounded boost or, better, a deadline taken from
  production tracking. Each import writes a report of what translated
  cleanly and what needs a person.
- **Plugins.** A plugin host runs a Deadline plugin's command construction
  (render executable, arguments, pre- and post-render steps) through a
  compatibility layer for the parts of the plugin API those steps use, inside
  the task pod. Plugins that depend on deeper Deadline behavior (event
  plugins, stdout handlers that drive task status) are listed in the import
  report with what to replace them with.
- **Side by side.** In shadow mode the bridge reads Deadline's queue through
  its Web Service and shows it in the HUD next to native jobs, with the
  scheduler's estimates for both. Routing rules (by project, pool or plugin)
  send each new submission to Deadline or to this queue. During the
  transition, Deadline Workers can run as pods of a wrapped work class, so
  the remaining Deadline jobs draw from the same farm under the same
  objective-driven allocation, and a studio retires Deadline pool by pool.

## 15. Integrations

### 15.1 DCC submitters

Every submitter follows the same steps:

1. collect the scene's dependencies (references, caches, textures);
2. compute their C4 IDs and upload only objects the project's store lacks, so
   a resubmission after a small change uploads only that change;
3. create a Materialization of the dependency view and a WorkJob for the
   frame range and passes;
4. show the scheduler's estimate and slack before the artist confirms.

| tool    | specifics                                                                                                       |
| ------- | --------------------------------------------------------------------------------------------------------------- |
| Houdini | a ROP for renders and a PDG (TOPs) scheduler; PDG work items map to units, and dynamic work items to generators |
| Maya    | render layers become passes; renderer settings for Arnold, V-Ray, Redshift and RenderMan                        |
| Nuke    | Write nodes become jobs, with dependencies on the renders they read                                             |
| Blender | an add-on for Cycles and Eevee frame ranges                                                                     |
| Unreal  | a Movie Render Queue executor; each shot is a job and its frames are units                                      |

### 15.2 Production tracking

ShotGrid (Autodesk Flow Production Tracking) and ftrack connect through their
APIs and event streams:

- delivery and task due dates become Milestones, which is where most jobs get
  their deadlines;
- a finished job creates a Version with its output view ID and preview;
- task statuses follow the jobs that serve them.

## 16. Studios, cloud providers and software vendors

The system has three audiences, and each gets its own documentation, its
own agent tools and its own HUD lens. Each sees only its own data: a vendor
sees the usage of its products, not studio content; a provider sees
capacity and ingress in aggregate, not project data. Facts about programs,
prices and license servers below are sourced in `docs/research/ecosystem.md`.

### 16.1 Cloud providers

- **Neutral by design.** AWS, Google Cloud, Azure, CoreWeave, OCI and
  on-premises clusters are equal citizens. Nothing in the data path depends
  on a provider: stores are S3-compatible endpoints or c4d servers,
  capacity goes through a capacity-request interface (section 10.12), and
  keys through a KMS interface.
- **An onboarding ramp, not a lock-in.** A studio's store can stay where it
  is. A regional tier in a provider's region fills on demand, by content ID,
  from wherever the store lives, so burst compute can start in any provider
  without a bulk migration, and only the objects a job reads ever cross.
  The same mechanism makes leaving cheap, which is what makes arriving
  easy.
- **Provider adapters.**

| concern      | AWS                                | Google Cloud                               | Azure                                     | CoreWeave                                                                   | OCI                                                   |
| ------------ | ---------------------------------- | ------------------------------------------ | ----------------------------------------- | --------------------------------------------------------------------------- | ----------------------------------------------------- |
| capacity     | Karpenter NodePools, EKS Auto Mode | node auto-provisioning with ComputeClasses | node auto-provisioning (Karpenter-based)  | Cluster Autoscaler on node pools; bare-metal scale-up takes 5 to 15 minutes | Cluster Autoscaler, or the Karpenter provider for OCI |
| spot         | Karpenter `capacity-type: spot`    | Spot VMs in a ComputeClass                 | Spot node pools                           | none documented                                                             | preemptible instances                                 |
| object store | S3                                 | Cloud Storage XML API with HMAC keys       | Blob has no S3 API: an adapter or gateway | AI Object Storage; virtual-hosted addressing only                           | S3 compatibility API with customer secret keys        |
| KMS          | AWS KMS                            | Cloud KMS                                  | Key Vault                                 | no KMS product: Vault or KMIP                                               | OCI Vault                                             |

  The S3 client therefore supports virtual-hosted addressing with custom
  endpoints, and the KMS interface has a Vault backend.
- **Marketplaces and co-sell.** The packaging of section 17 fits AWS
  Marketplace container products (hourly per pod with `RegisterUsage`, or
  custom units with `MeterUsage`, on EKS), Google Cloud Marketplace
  Kubernetes apps (usage through the billing agent, on GKE), and Azure
  Kubernetes application offers (custom meters on AKS). The co-sell
  programs are AWS ISV Accelerate, the Google Cloud Partner Network
  (successor to Partner Advantage), and Microsoft's AI Cloud Partner
  Program with Frontier Accelerate for Marketplace, Azure IP co-sell
  eligibility and MACC. Oracle's marketplace allows only free or BYOL Helm
  listings; CoreWeave has a partner network but no marketplace. Co-sell
  status requires transactable, paid listings and sales thresholds, so the
  realistic early step is a listing and co-sell-ready status.
- **The provider lens** shows capacity and ingress: bytes that entered the
  provider's regional tier, burst node-hours by pool, cache warmth by
  region, and the transfer avoided.

### 16.2 Software vendors and per-hour licensing

The industry's per-hour licensing channel has moved. Thinkbox's
usage-based licensing marketplace was retired on 30 September 2025, and
Deadline 10 has been in maintenance mode since 7 November 2025.
Usage-based licensing now runs through AWS Deadline Cloud license
endpoints (for example Arnold at $0.66 an hour), which serve workers in AWS
only. Vendors who sold through Deadline have a question about their
channel, and studios on other clouds have none at all.

**Licensing broker.**

- Vendors register products: the license server type (RLM, FlexNet,
  SideFX hserver, Chaos licset), the features, and the per-hour rates used
  for reporting and settlement.
- Jobs declare the licenses each unit needs. The scheduler treats licenses
  as a pool with a count, like Deadline's Limits: a unit is released only
  when a license is free, so a farm never runs more renders than it has
  licenses for, and overflow can go to a per-hour endpoint where one
  exists.
- At task start the broker grants the pod a short-lived lease tied to its
  identity, with the same TokenReview and lease mechanism as the
  just-in-time keys (section 6.6): network access to the license server
  (a NetworkPolicy) and the `port@host` variable the application reads. At
  task end the lease closes and becomes a usage record in the signed
  metering pipeline.
- Adapters:
  - customer-owned RLM and FlexNet servers: gate access, enforce counts, and
    reconcile leases with the servers' report logs;
  - AWS Deadline Cloud license endpoints for AWS customers: a
    vendor-sanctioned per-hour pool for Arnold, Cinema 4D, Nuke, Red Giant,
    Redshift, Houdini, Mantra, Karma and V-Ray on customer-managed workers;
  - vendor-hosted services (RLM Cloud, FlexNet Operations metered
    licensing) where a vendor offers them.
- **What the broker does not do.** It does not mint or sign licenses, issue
  grants that vendors' clients honor natively, resell vendor hours, set
  prices, or host license servers outside a vendor's terms. Each of those
  needs an agreement with the vendor.
- **Proof of use.** Every lease is part of a task record (section 9), so
  license usage is signed, attributable to a task, a project and a node, and
  verifiable by both the vendor and the studio. The provenance records are
  the license-usage proof.
- **The vendor lens** shows usage and revenue by product, customer
  installation and hour, with export for settlement.

### 16.3 A face for each side

| audience         | documentation                                                     | agent tools                                  | HUD lens                         |
| ---------------- | ----------------------------------------------------------------- | -------------------------------------------- | -------------------------------- |
| studios          | production guides: submitting, deadlines, previews, provenance    | the tools of section 13.2                    | render, CI, ML, provenance, cost |
| cloud providers  | hosting a regional tier, capacity adapters, marketplace packaging | capacity forecast, ingress and burst queries | capacity and ingress             |
| software vendors | registering products, license adapters, usage export              | usage queries and exports for their products | usage and revenue                |

## 17. Packaging and deployment

- One Helm chart, published as an OCI artifact, installs everything: CRDs,
  manager (controllers, scheduler, key broker, meter), node daemon,
  regional tier, HUD, and MCP server, with the skill alongside. Values select stores, projects, pools,
  KMS, and the metering exporter.
- It runs in the customer's own cluster and cloud account. Data, keys,
  scheduling and metering all stay there. There is no vendor-hosted control
  path; metering reports go from the customer's cluster to the marketplace
  API using the cluster's own cloud identity (EKS Pod Identity or IRSA, GKE
  Workload Identity, AKS workload identity).
- Marketplace listings wrap the chart: AWS Marketplace Helm-based container
  products, Google Cloud Marketplace Kubernetes apps, and Azure Kubernetes
  application offers. Images are signed and ship with SBOMs.

## 18. Phases and evidence

Each phase is proven by tests and by a scenario on a multi-node test cluster
with 8 workers in two localities. Results land in
`hack/e2e/out/results.md`.

- **P1: overlay views, capture, CSI, no per-read verification.** Pods under
  Pod Security `restricted` mount views through the CSI driver. Evidence: one
  layer per node serves every pod on it (link counts), one upper per pod, a
  warm publish hashes nothing, captured output views equal views computed
  independently on the host, and v12 = v11 + 3 frames fetches 3 objects and
  adds one delta layer.
- **P2: tiers.** Three MinIO shards, three regional replicas, peer homes in
  two localities. Evidence: store GETs per object counted by MinIO itself,
  against the number of requesting pods, for the tiered path, the Direct
  path, and per-pod fetching; and a regional replica killed mid-run while
  the job completes, with the extra store GETs bounded.
- **P3: isolation.** Two projects with separate buckets and MinIO users.
  Evidence: project B cannot read A's objects by ID through any path
  (Materialization, ConfigMap c4m, tier API, peer port, forged volume
  context), strict partitions on a shared node, and grant paths.
- **P4: c4git monorepo.** A repository with c4git-managed assets is imported.
  Evidence: import cost per commit, CI pods doing sparse checkouts, and the
  bytes a node fetches for the next commit.
- **P5: work system and HUD.** A comp job pipelined on a render job's output
  chain, starting before the render finishes. Simulation results for each policy across
  render, CI and ML, and estimate error over time for binary against
  sequential order; the kind run with deadlines, binary order, a generator
  expanding, a CI pipeline under a latency SLO, a training run yielding at a
  checkpoint, and a burst; and HUD screenshots for an artist (with the
  preview player), a render wrangler, a CI wrangler and a platform engineer.
- **P6: agent-native interface.** The MCP server (stdio and Streamable
  HTTP), the `c4-farm` skill, and the generated tool reference. Evidence: a
  test that every HUD endpoint has a tool; the evaluation set answered by an
  agent that has only the MCP server; and a recorded session in which an
  agent triages a failing job, makes a deadline with a burst, and migrates a
  Deadline job on the test cluster.
- **P7: provenance.** Task records signed and stored as C4 objects, the
  lineage index, trace queries, the disclosure report, C2PA manifest
  definitions and OMC export, the HUD view and the MCP tools. Evidence: a
  delivered frame traced through comp, render and sim to its plates on the
  test cluster; a tampered record detected; and the generative-use query
  answered for a delivery that contains a generated element.

### 18.1 As run

What the phase scripts (`hack/e2e/p1.sh` to `p7.sh`) show, against the plan
above. `REPORT-v2.md` has the numbers.

- **P1 to P3** ran as planned.
- **P4** imports the synthetic monorepo into MinIO and runs CI shards on
  consecutive commits (cold, delta, warm mounts) and API-only diffs checked
  against `git diff`. Push-to-view latency was measured on local stores by
  `hack/e2e/monorepo/pushlat` (`hack/e2e/monorepo/RESULTS.md`), not against
  MinIO.
- **P5** ran deadlines, binary order with its estimate scored against what
  the units cost, a CI change under a latency target, training runs
  yielding at checkpoints and resuming from their captured views, and a burst
  through the simulated capacity provider, with live HUD screenshots. Not shown on the
  cluster: a comp job pipelined on a render's output chain (each job's
  output is a sealed chain, but downstream units start when the upstream job
  succeeds, through `fromJob`), and generators expanding (the policy and the
  simulator handle them; the queue's classes do not create generators yet).
  Wrapped ML (Kueue and JobSet) is designed; the ML class runs a gang as one
  pod holding all its slots.
- **P6** answers the evaluation set through the MCP server with a reference
  client (`hack/e2e/mcpcheck`) and checks the answers. A recorded session
  of an agent using the server is not included, and the HUD's preview,
  object and event-stream endpoints have no tools (they serve bytes and
  streams, not answers).
- **P7** traces a model-assisted denoise to the render it read, verifies
  every record, and rejects a forged one. The disclosure report, the
  forward trace (`c4_trace_forward`) and signed C2PA manifests are not built;
  the farm emits C2PA manifest definitions and OMC assets per file.

## 19. Non-goals and not in this plan

Non-goals:

- **Training infrastructure.** This system does not implement gang
  scheduling, topology-aware placement, NCCL or other collective tuning, or a
  training framework. Training jobs that need strict network sync get it
  from the scheduler they are wrapped in (Kueue with JobSet,
  LeaderWorkerSet, Kubeflow Trainer or KubeRay). The ML class owns data,
  capacity sharing with render and CI, and visibility (section 10.17).
- A version-control system (history, refs, merges, blame).
- Cross-region replication of the authoritative store, and Windows nodes.

Not in this plan:

- The lazy FUSE lower (section 4.6).
- Encryption with just-in-time keys (section 6.6), the Deadline bridge
  (section 14), DCC submitters and tracking integrations (section 15), and
  the marketplace exporters (section 12.3) are designed here. What of them
  was built is listed in `REPORT-v2.md`.
- TLS between tiers in the kind e2e (bearer tokens are sent in clear inside
  the cluster network there). Production needs it.
- Cache-aware placement of individual pods by kube-scheduler (the queue
  pre-warms burst nodes, but does not score nodes by cache contents).
- On-demand capture through an API, beyond expansion chunks and
  checkpoints.
- Garbage collection of unreferenced objects in the store.
