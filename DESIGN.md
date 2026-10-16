# c4-operator v2 design

Status: design of record for branch `v2`. The MVP on branch `mvp` is the
baseline; this document says what changes and why.

## Goals

1. **Grab objects, do not re-check them.** As in a c4 store, a reader asks
   for an ID and uses the bytes it gets. Bytes are verified once, when they
   enter the system.
2. **Track what a job changes.** A job's writes become a changeset against
   the tree it was given: a c4m patch over a parent, with content-addressed
   pointers to new objects.
3. **Very large render clusters.** Tens of thousands of nodes, and a render
   start in which thousands of nodes ask for the same plates in the same
   second.
4. **No single point of failure or overload.** No single NFS server, S3
   endpoint, cache replica, or node may be on the critical path for everyone.
5. **Minimal duplication on disk.** Containers share read-only overlay
   lowers. Each container's private writes live in its own upper. A new
   version of a tree costs only its changes.
6. **Strong per-project isolation.** Knowing a C4 ID grants nothing.

Non-goals for v2: a version-control system (history, refs, merges, blame),
cross-region replication of the authoritative store, and Windows nodes.

## Prior art: Scion

Anthropic described Scion, its replacement for Git, in
[Redesigning source control](https://www.anthropic.com/engineering/redesigning-source-control)
(2026-10-13). The parts this design borrows:

| Scion                                                                                                       | Here                                                                                            |
| ----------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------- |
| Changesets record the files changed relative to the first parent, with content-addressed blob pointers.     | A changeset is a c4m patch: a base tree ID on the first line, then the changed entries.         |
| Fetch is top-down (root tree, then missing trees, then blobs), and every request names an immutable object. | The same. Directory records, then file objects, each fetched by C4 ID.                          |
| Each node runs one daemon on a local socket, shared by all jobs on the node.                                | One node daemon per node, which is also the CSI node plugin.                                    |
| Tiers are strict (daemon, then regional cache, then server), and all tiers answer identical RPCs.           | The same three tiers plus peer fetch inside a pool. One content API at every tier.              |
| Sparse and virtual checkouts: the client does not request what it does not want.                            | `include` filters stop the fetch at directory records. A lazy lower is designed in section 5.5. |
| Directory-level ACLs.                                                                                       | Project grants on (tree, path prefix).                                                          |
| Commits are server-side, with cost proportional to the change.                                              | Capture uploads only the changed objects, the new directory records, and one patch.             |

Scion is built for one code monorepo. This design applies the same
structure to media trees, where single objects are 10 MB to 10 GB rather
than 10 KB, and where outputs (rendered frames) dominate writes.

## 1. Trust model

**Bytes are verified once, when they enter the system.** Entry happens in
exactly two places:

- **Ingest**: `c4-operator ingest` (or the c4 CLI with `c4 id -s`) hashes
  a file as it uploads it, and the store key is the computed ID.
- **Capture**: when a container's writes are captured (section 3.4), each
  new file is hashed once as it is uploaded.

Everything after that trusts the ID-to-bytes binding. The node daemon,
peers, and regional caches do not rehash on fill or on read. The c4 store
works the same way: `TreeStore.Create(id)` writes under a caller-supplied
ID without hashing.

Why this is safe enough:

1. **Immutability is structural.** Containers never see cache objects
   writable. Lowers are read-only overlay layers. A container's writes go to
   its own upperdir, and overlayfs copies up before the first write. The MVP
   needed an mtime sentinel and a per-request inode check because pods
   mounted hardlinks directly. With overlay, a pod cannot reach the object
   inode, so there is nothing to check per read.
2. **Transport integrity.** Tier-to-tier traffic is TLS in production.
   Length is checked on every fill: a short read is retried with a ranged
   resume, never accepted.
3. **Scrub.** An optional background scrubber on each tier rehashes cached
   objects at a configurable byte rate, oldest-verified first, and evicts any
   that do not match. A mismatch is a disk or software fault, reported as a
   metric and an event. It is off by default on nodes and on by default on
   the regional tier, whose objects are served many times.

What this gives up: a corrupt object in a regional cache is served to every
node that asks for it until a scrub finds it. The mitigations are TLS, scrub,
and the fact that a cache entry can be dropped and refetched at any time.
`verifyFill: true` (per project) restores hash-on-fill at the node for
projects that want it. Hashing while writing costs about 1 to 2 GB/s per core
and almost nothing when the network is slower than that.

## 2. Data path

### 2.1 One content API at every tier

Every tier answers the same HTTP API. It is the c4d content plane, extended
in a compatible way:

```
GET  /{c4id}            object bytes; honors Range (206) for resume
HEAD /{c4id}            200 if this tier can serve the object, 404 if not
PUT  /                  body is content; reply is its C4 ID (writes only)

Request headers:
  Authorization: Bearer <projected service-account token>
  X-C4-Project:  <project>      cache partition and credential scope
  X-C4-Hop:      peer           marks a peer request (see 2.3)
Response headers:
  Content-Length, Accept-Ranges: bytes
  Retry-After                   with 503 when shedding load
```

**Does c4d's API fit?** The path shape and semantics do: `GET/HEAD /{c4id}`
and `PUT /` are exactly c4d's content plane, so a c4d can be the
authoritative store tier (`c4d://host:7433`) unchanged. c4d is not suitable as
a cache tier as it stands, for two reasons:

- It does not honor `Range`, so a transfer interrupted at 9 GB of a 10 GB
  object restarts at zero. The client handles that: a 200 in reply to a
  ranged GET means "start over".
- Its content plane authorizes any mTLS peer to read any ID, so it has no
  notion of a project. Project isolation needs the partition header and a
  per-request authorization decision.

Both are additive changes to c4d, written up in PROGRESS.md.

### 2.2 Tiers

```
 pod --CSI--> node daemon ----> peer daemons (same pool) ----> regional cache ----> authoritative store
              (one per node)    rendezvous home(s)             (Deployment,          (sharded: several
                                                                stateless, sharded    buckets, endpoints,
                                                                by rendezvous)        or regions)
```

The tiers are strict: a node never talks to the store when a regional tier
is configured. That keeps the store's load bounded by the number of regional
replicas, not the number of nodes. A small installation can run with
`tiers: direct`, in which nodes fetch from the store. The kind e2e uses that
mode for P1.

**Node daemon** (DaemonSet). It holds the node's object cache, builds
lowers, mounts overlays for pods (it is the CSI node plugin), captures
changesets, and serves its cache to peers.

**Peer fetch** (inside a pool). A pool is a set of nodes with cheap
bandwidth between them: a rack, a node pool, or a zone. The node's
`topology.kubernetes.io/zone` label, or a configured label, defines it. For
each object, rendezvous hashing over the pool's daemons, `score(peer, id) =
hash(peer.name || id)`, picks the top `k` (default 1) as the object's
**homes**. A daemon that needs an object:

1. serves it from its own cache, or
2. if it is not a home, asks the homes in rank order, or
3. if it is a home (or every home failed), asks the regional tier.

A home that receives a peer request for an object it lacks fetches it from
the regional tier itself, coalesced with any other request for that object.
A non-home that receives a peer request for an object it lacks answers 404
and does not fetch. Requests never cascade from peer to peer.

**Regional cache** (Deployment). It is stateless: its cache is local disk
that can be lost at any time. It scales horizontally. Clients pick a replica
by rendezvous over the replicas for (project, id). The top `r` (default 2)
are the object's regional homes, and a client spreads its requests over them
by hashing its own node name. Each object is therefore fetched from the store
at most `r` times per region, and a hot object is served by `r` replicas, not
one.

**Authoritative store.** A project's store is a list of shards: S3 buckets,
S3 endpoints, c4d servers, or regions. An object lives on the shard with the
highest rendezvous score for its ID. Rendezvous is used rather than an ID
prefix modulo N, because adding a shard then moves only 1/(N+1) of the
objects. No single endpoint carries all reads.

### 2.3 Fan-in arithmetic

A render start puts `P` pods on `N` nodes in `G` pools, all reading the same
`M` objects:

| tier                   | requests per object                        |
| ---------------------- | ------------------------------------------ |
| pods to node daemons   | `P` (`P/N` per node, coalesced to 1)       |
| node daemons to homes  | `N - G·k` (each node asks its pool's home) |
| homes to regional tier | `G·k`                                      |
| regional tier to store | at most `r`                                |

With 5,000 nodes in 50 pools, `k = 1`, and `r = 2`, the store sees 2 GETs
per plate frame instead of 5,000. The regional tier sees 50.

### 2.4 Every tier coalesces, sheds, and resumes

- **Singleflight.** Concurrent requests for the same (project, id) at a tier
  share one upstream fetch. Followers wait for the leader's file and are then
  served from it.
- **Admission control.** Each tier caps concurrent upstream fetches and
  queued waiters. Beyond the queue it answers `503` with `Retry-After`. It
  never forwards the excess upstream.
- **Backpressure, not stampede.** A client that gets 503 or a connection
  error tries the next candidate at the same tier (the next home, or the next
  regional replica). If every candidate refuses, it backs off with jitter and
  retries. It does not skip a tier: a node never falls through to the store
  because the regional tier is busy. A regional outage slows nodes down but
  cannot multiply load on the store.
- **Resumable transfers.** Fills write to a temporary file. On failure the
  client retries with `Range: bytes=<written>-`, against the same or a
  different replica. Every replica serves identical bytes for an ID, so
  stitching across replicas is correct.
- **Bounded memory.** Objects stream to disk, never through memory, at
  every tier.

### 2.5 Fetch order

Resolution is top-down: the root record or c4m, then the directory records
the include filter can reach, then file objects. Directory records are
small, are fetched on the request path, and are also cached in every tier.
File objects are fetched in parallel, largest first, so the long transfers
start early.

## 3. Overlay filesystems

### 3.1 Layout on a node

```
/var/lib/c4/
  p/<project>/objects/               object cache (strict); shared mode uses /var/lib/c4/objects
  p/<project>/lowers/<layer>/        immutable layer trees, hardlinked from objects/
  p/<project>/vols/<volume>/upper    one per mounted volume
  p/<project>/vols/<volume>/work     overlay workdir
  p/<project>/meta/                  layer descriptions, retention
```

Everything is on one filesystem, so lowers are hardlinks (`link(2)` returns
`EXDEV` across filesystems, and even across bind mounts). A file in a lower
costs an inode, not its bytes.

### 3.2 Lower layers

A **full layer** is the tree of one view (a c4m after the include filter),
hardlinked from the object cache. It is built once per (project, view ID) on
a node and shared read-only by every container on the node that mounts that
view.

A **delta layer** holds what one changeset changes, relative to its parent
view:

- added or modified files, hardlinked;
- deleted files, as overlay whiteouts (character device 0/0);
- directories whose contents were replaced, marked opaque
  (`trusted.overlay.opaque=y`).

### 3.3 Version stacking

A Materialization whose source is a changeset resolves to a chain:
`changeset_n -> changeset_(n-1) -> ... -> base tree`. The mount's lowerdir is
the stack `delta_n : delta_(n-1) : ... : full_base`. Shot v12 that is v11 plus
three changed frames costs three hardlinks and a directory on a node that
already has v11.

Stacks are bounded. Past 16 layers (overlay's per-mount option length is a
page), or once the deltas hold more than half the base's entries, the node
builds a full layer for the newest view instead. That is compaction, and it
is local to the node.

### 3.4 Upper layers and changeset capture

Each mounted volume gets its own upperdir and workdir. The container sees
the merged tree, writable. Lowers are untouched.

Capture runs when the volume is unpublished (the pod has terminated, and
kubelet tears down volumes before the pod object is deleted). It can also be
requested on demand. Capture:

1. unmounts the overlay, so the upper is quiescent;
2. walks the upper:
   - regular file: added or modified. It is hashed (the verify-once point),
     put into the node cache, and uploaded to the project's store. If its ID
     equals the lower's, it was a metadata-only copy-up and is not a change.
   - character device 0/0: a whiteout, so the path was deleted.
   - directory with `trusted.overlay.opaque=y`: the lower's subtree was
     replaced.
   - new directory or symlink: added.
3. applies those changes to the lower's view to get the output tree, at
   content level, and computes its ID;
4. stores the directory records the output tree needs (those along changed
   paths; the rest are the parent's), and a changeset c4m: the parent view ID
   on a bare first line, then `c4m.PatchDiff` entries. `c4m.ResolveBase` and
   `c4 cat` can read it;
5. records the result on the pod as annotations:
   `c4.avalanche.io/output-tree`, `c4.avalanche.io/output-changeset`, and
   `c4.avalanche.io/output-changes` (added, modified, deleted counts), and as
   metrics.

The output is a tree ID. A downstream Materialization can mount it, or mount
the changeset to get a stacked lower. Capture is at least once: if it fails,
the upper is kept and retried. An empty upper produces no changeset.

Cost is proportional to the change. A render that writes 200 new EXRs
uploads 200 objects and a few directory records, whatever the size of the
input tree.

### 3.5 Lazy lower (designed, not built)

For trees where jobs read a small fraction (a 25 GB source tree in which a
test touches 300 files), materializing every object up front wastes the
fetch. The lazy option is a FUSE filesystem served by the node daemon. It
presents the view from its directory records, and fetches an object on the
first `open` through the same tiered client. Overlay mounts it as the bottom
lower, so writes still go to the per-container upper and capture works
unchanged. Costs: one FUSE round trip per `open` (reads of cached objects go
through passthrough or a kernel page-cache bypass), a privileged daemon
holding the FUSE session, and failure handling for a daemon restart under
live mounts. A Materialization chooses `materialize: Eager | Lazy`. Eager
remains the default for render inputs, which are read whole.

## 4. Mounting: a CSI node driver

The MVP's init container and hostPath volumes are replaced by a CSI driver,
`c4.avalanche.io`, with ephemeral inline volumes:

```yaml
volumes:
- name: assets
  csi:
    driver: c4.avalanche.io
    volumeAttributes:
      materialization: sh010
      capture: "true"
```

- Pods under Pod Security `restricted` may use `csi` volumes. hostPath is
  gone from user pods.
- All privileged work (mounting overlays, creating whiteouts, writing the
  cache) happens in the driver, which runs privileged in `c4-system`.
- `NodePublishVolume` resolves the Materialization, makes the layers present,
  and mounts the overlay at the target path. It is idempotent. Fetching runs
  detached from the RPC, so if kubelet's call times out on a 200 GB cold
  fetch, the retry joins the work in flight instead of restarting it. The pod
  stays `ContainerCreating` until the tree is complete. Missing content fails
  the publish, and the workload never starts.
- `NodeUnpublishVolume` captures (section 3.4) and unmounts.
- The `CSIDriver` object requests a projected service-account token
  (`tokenRequests`, audience `c4.avalanche.io`), used for identity
  (section 5.2).
- The mutating webhook stays as a convenience. It turns the MVP's
  annotations into the `csi` volume and mounts, and nothing more.

## 5. Per-project isolation

### 5.1 Projects

A `Project` is cluster-scoped and owned by cluster administrators:

```yaml
apiVersion: c4.avalanche.io/v1alpha1
kind: Project
metadata: {name: show-a}
spec:
  namespaces: [show-a-render, show-a-comp]
  isolation: Strict                 # Strict | Shared
  store:
    shards: ["s3://a-store-1/c4/?endpoint=...", "s3://a-store-2/c4/?endpoint=..."]
    credentialsSecretRef: {name: show-a-store}   # in c4-system, not in user namespaces
    kmsKeyRef: ...                  # optional, see 5.5
  grants:                           # optional; empty means any tree in the project's own store
  - tree: c4...                     # a root tree, source c4m, or changeset ID
    paths: [shots/sh010/, editorial/]
```

A Materialization belongs to the project that lists its namespace. Users
choose neither the store nor the credentials. Those come from the Project.

### 5.2 Identity and authorization

- **Pods.** `NodePublishVolume` receives the pod's projected token (audience
  `c4.avalanche.io`). The driver validates it with TokenReview, and checks
  that the token's namespace and pod UID match the volume context. The
  namespace maps to a project, and the Materialization must be in that
  namespace. The driver does not rely on the caller being kubelet.
- **Daemons and caches.** Tier-to-tier requests carry the caller's own
  projected token (audience `c4-tier`). The server validates it with
  TokenReview, caches the result for the token's lifetime, and accepts only
  the `c4-system` service accounts of the node daemon and the regional tier.
  Pods never reach the tier API with valid credentials.
- **Knowing an ID is not permission.** Pods cannot fetch by ID at all; they
  can only mount Materializations in their own namespace. A Materialization
  resolves through the project's store with the project's credentials, and
  through the project's grants. An object is served to a project only if it
  is reachable from a tree the project may use:
  - with grants: from a granted tree, under granted paths (the include filter
    is intersected with the grant paths);
  - without grants: from a description in the project's own store. A
    hand-written ConfigMap c4m that lists another project's IDs resolves
    against this project's store, where those objects do not exist.
- **Directory-level grants** are the `paths` of a grant, applied as a second
  filter that the user cannot widen.

### 5.3 Strict mode: no shared caches

In `Strict` mode (the default), each project has its own object cache, its own
lowers on every node, and its own partition at every cache tier (the
`X-C4-Project` header is part of the cache key, and upstream fetches use the
project's credentials). No byte is shared across projects, even when the
content is identical.

**Why not deduplicate across projects?** A shared content-addressed cache is
an oracle. If project B can make the node fetch ID X and time the result, a
fast answer reveals that someone on that node (or region) recently used X.
Because IDs are hashes of content, B can test guesses: render a candidate
frame of a competitor's unreleased shot, compute its ID, and probe. That is
a confirmation-of-file attack, and it needs no read access to A's data. Even
without timing, a cache that answers B's request from A's fill leaks A's
bytes if B's store lacks them. Partitioning removes both channels.

`Shared` mode trades this away deliberately for projects that trust each
other, such as departments of one show. It shares objects and lowers across
the projects listed in a `sharingGroup`. Before serving a shared cached
object to project B, the tier checks `HEAD` on B's own store (a proof of
possession), so B can never read bytes it could not have read anyway. The
timing channel remains, and is documented.

### 5.4 Credentials

Store credentials live in Secrets in `c4-system`, referenced by Projects. The
node daemon and the regional tier get them through a Role in `c4-system`
only. That fixes the MVP's cluster-wide Secret read. In strict tier mode only
the regional tier needs read credentials, and nodes need write credentials
only for capture.

### 5.5 Encryption (designed, not built)

With `kmsKeyRef`, objects are encrypted at rest in the store under a
per-project data key wrapped by KMS. The C4 ID stays the ID of the plaintext,
so the store key does not reveal anything that the ID would not. Encryption
uses a convergent-free scheme: a random nonce per object, stored with the
object, so equal plaintexts in two projects are different ciphertexts.
Decryption happens at the regional tier (strict) or the node (direct). Caches
hold plaintext on local disk, so per-project encryption protects the store and
not a compromised node.

### 5.6 Trust boundary

Pods are untrusted. Node daemons and regional caches are trusted
infrastructure: they run privileged or hold credentials, and anyone who owns a
node already owns what is mounted on it. Tier traffic is authenticated so
that pods and other workloads in the cluster cannot use the tiers.

## 6. Very large clusters

- **The control plane is not in the data path.** The controller resolves a
  Materialization once per generation (IDs, totals, store preflight) and
  writes one status. Nodes and caches talk to each other and to the store,
  never through the API server.
- **No per-node or per-pod status in the CR.** At 10,000 nodes, per-node
  status entries would make every Materialization a hot, megabyte-sized etcd
  object. The MVP's `status.nodes` is removed. Per-tier and per-node behavior
  is reported as Prometheus metrics (hits, misses, coalesced waits, shed
  requests, upstream fetches and bytes, publish latency, capture counts),
  aggregated by the cluster's metrics stack. The only per-pod write is the
  capture annotation on the pod itself, which the pod already owns.
- **Bounded watches.** A node daemon watches:
  - only the daemon pods in its own pool (label selector
    `c4.avalanche.io/pool=<pool>`), for peer membership;
  - the regional tier's EndpointSlices, which hold tens of endpoints;
  - Projects and Materializations by name, on demand, with a short-lived
    cache. There is no cluster-wide pod watch.

  A Materialization change wakes the controller, not 10,000 nodes.
- **Eviction under pressure.** Each node has a byte budget. Unlinked objects
  go first, in LRU order by atime. Then layers that no mounted volume uses
  and whose `retainOnNode` has passed. A layer under a live mount is never
  removed. The regional tier evicts by LRU only, since it has no mounts.
- **Node churn.** When a node leaves a pool, rendezvous moves only the objects
  homed on it (1/n of the pool's objects) to new homes. Those homes refetch
  them from the regional tier on demand, which coalesces. A node that joins
  starts empty and becomes home for 1/n of objects. Peers that ask it early
  get 404s or coalesced waits, never cascades.
- **Regional churn.** Losing a regional replica moves its objects to the next
  replica in rendezvous order. That replica refetches each one at most once,
  so the store sees at most one extra GET per affected object. Clients resume
  interrupted transfers there.

## 7. Use cases

### 7.1 Render farms

Plates (EXR sequences), USD stages and layers, caches (Alembic, VDB),
textures, and model weights. The pattern is many pods reading one tree
whole, at the same moment, and writing new frames.

- Inputs: a Materialization per shot (`include: shots/sh010/`, `assets/`),
  eager lowers, homed per pool, served from one lower per node.
- Outputs: written into the mounted tree (for example `shots/sh010/render/`),
  captured per pod as a changeset with the pod's frames. A downstream comp job
  mounts the output tree, or the input stacked with every frame changeset.
- Versioning: "v12 = v11 + 3 changed frames" is a changeset. Nodes that had
  v11 add a three-file delta layer.

### 7.2 Monorepo CI (Scion's case)

A large repository imported into C4. Each commit is a tree, and each
commit's diff against its first parent is a changeset c4m. Many CI pods
check out sparse, stacked views:

- the import walks `git ls-tree`/`git cat-file` and ingests blobs once.
  Files already managed by c4git are 90-byte C4 IDs in git, which the
  importer uses directly, so large files are never re-hashed;
- a CI job's Materialization is (commit tree or changeset, `include:
  services/foo/, libs/common/`);
- a node that ran CI for the parent commit adds one thin delta layer for the
  child commit;
- build outputs captured from the upper become the cache key for the next
  stage.

The lazy lower (section 3.5) matters most here, because tests read a small
fraction of a large tree.

## 8. What it is not yet, and the plan

### Phases

- **P1:** no per-read verification; Project CRD; overlay views with a shared
  lower per (project, view) and an upper per volume; changeset capture with
  store ingest; the CSI driver; status without per-node entries; metrics.
- **P2:** the tier API server and client with singleflight, admission control
  and resume; peer fetch with rendezvous homes; the regional cache
  Deployment; sharded stores (several MinIO instances); a thundering-herd e2e
  on a multi-node kind cluster, including a regional replica killed mid-run.
- **P3:** project isolation: TokenReview at publish and between tiers; strict
  partitions; grants; credentials scoped to `c4-system`. Tests show project B
  cannot read project A's objects even with their IDs.
- **P4:** the monorepo CI case through a git importer that understands c4git,
  if time allows.

### Not in this plan

- The lazy FUSE lower (section 3.5).
- KMS encryption (section 5.5).
- Cache-aware scheduling, which places pods on nodes or pools that already
  hold most of a tree. The node metrics are its input.
- On-demand capture through an API (capture happens at unpublish only).
- Cross-region store replication, and garbage collection of unreferenced
  objects in the store.
- Streaming to coalesced waiters while the leader is still downloading. They
  wait for the whole object.
