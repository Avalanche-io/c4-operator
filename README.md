# c4-operator

A Kubernetes operator that gives each pod exactly the files it needs, from a
content-addressed store, through a cache on each node. Jobs declare their inputs
as a c4m file, a plain-text description of a directory tree in which every file
is named by its C4 ID. The node fetches only the content it does not already
hold and checks every byte against its ID. It then hands pods a read-only tree
built from hardlinks. Identical bytes are fetched once per node and shared by
every pod on that node.

It is built for heavy media: EXR sequences, ProRes, USD, and model weights.

The operator is rebuilt from a 2017 design by the author, Joshua Kolden, who
also wrote SMPTE ST 2114, the C4 ID standard.

Status: a working MVP, unreleased. See [What it is not yet](#what-it-is-not-yet).

## The problem

A render, training, or conform job usually needs a small, precise slice of a
large tree: one shot's plates out of a show, one checkpoint out of a model
directory. Pipelines tend to move far more than that slice:

- A shared filesystem mount gives every pod everything. Each pod pulls whatever
  it touches over the network, and every pod on a node pulls the same frames
  again.
- Copying a job's inputs into each pod (init containers running `aws s3 sync`,
  for example) makes N pods on a node fetch and store the same bytes N times.
- Git and Git LFS are built for source code. A clone carries history the job
  does not need. LFS addresses content by SHA-256 but materializes per working
  copy. Neither has a node-level cache shared by many pods, and neither suits a
  200 GB plate set.

Identity by content changes the cost model. If a job's inputs are a list of
content IDs, a node can tell exactly which bytes it already has. It fetches the
rest once and serves every pod from the same copy. It can also prove that what
it served is what was asked for.

## How it works

```
  ConfigMap (c4m text)  or  C4 ID of a c4m in the store
                 |
                 v
   +----------------------------- manager (Deployment) -----------------------------+
   | controller: parse c4m -> resolve directory records -> apply include filter      |
   |             -> view ID + totals -> ask the store whether every object exists    |
   |             -> Materialization status (Ready / Failed, missing paths)           |
   | webhook:    pod annotated c4.avalanche.io/materialize -> add init container     |
   |             + read-only mount of the view                                       |
   +----------------------------------------------------------------------------------+

   node
   +----------------------------------------------------------------------------------+
   |  pod: init container --- unix socket ---> agent (DaemonSet)                     |
   |                                             | preflight: store has every       |
   |                                             |   object this node lacks?        |
   |                                             | fetch missing IDs only --------------> store
   |                                             |   (S3, c4d, or a local path)     |
   |                                             | hash on write -> objects/<C4 ID> |
   |                                             | hardlink -> views/<view ID>/     |
   |  pod: workload containers <-- read-only bind of /var/lib/c4/views/<view ID>     |
   +----------------------------------------------------------------------------------+
```

**Materialization** (namespaced CRD, `c4.avalanche.io/v1alpha1`) names a c4m
source, a store, and optionally a list of `include` patterns. The include list
is the sparse part: only those paths are materialized. When the source is a
snapshot stored as per-directory records (as `c4 id -s` writes them), the
resolver never fetches the records of directories the filter cannot reach.

**Controller.** The controller resolves the source into a *view*: the exact
directories and files a pod will see. It computes the view's ID, counts files
and unique bytes, and checks the store for every unique object. A
Materialization whose content is missing goes `Failed` and lists sample paths.
It is rechecked every minute, so uploading the content heals it.

**View ID.** `status.viewID` is the content-level C4 ID of the tree pods
receive. It uses c4's content projection: names, sizes, and IDs are real, and
modes and timestamps are null. The controller computes it from the description,
and anyone can recompute it from the materialized directory:
`c4-operator tree-id /assets` scans with the c4 library in content mode. The
e2e test makes every pod do this.

**Webhook.** The webhook acts only on pods that carry the annotation (a CEL
`matchCondition`), so failing closed cannot block unrelated pods. It admits a
pod only once its Materialization has a view. It then adds an init container,
two hostPath volumes, and a read-only mount at the requested path in every other
container. The workload's image and command are not touched.

**Agent.** The agent owns `/var/lib/c4` on its node:

```
/var/lib/c4/objects/            c4 TreeStore, one file per C4 ID, mode 0444
/var/lib/c4/views/<view ID>/    the tree, every file a hardlink into objects/
/var/lib/c4/meta/<view ID>.c4m  the view's description, rechecked on load
/var/lib/c4/agent/agent.sock    what init containers talk to
```

For each request it:

1. Loads the view (from `meta/`, or by resolving the Materialization), and
   refuses if the view no longer matches what the pod was admitted for.
2. Asks the store about every object the node lacks, and fails before fetching
   anything if one is missing.
3. Fetches missing objects in parallel. A fetched object is named by the hash of
   the bytes actually written (c4 `TreeStore.Put`), so a wrong or truncated
   response can never appear under the requested ID.
4. Checks cached objects. Every cached object carries a fixed mtime (c4m's null
   timestamp), so any write to it, including a write through a view, moves the
   mtime and fails the check. A failed object is rehashed and refetched.
   `verify: Full` rehashes every object on every request.
5. Confirms each file in the existing tree is the same inode as the cached
   object, and otherwise builds a new tree in a temporary directory and renames
   it into place.
6. Reports hits, misses, refetches, bytes, and timing into
   `status.nodes[<node>]`.

The filesystem is the cache index. An object's link count is its reference
count, its atime is its LRU position, and its mtime is the verification
sentinel. Eviction removes unlinked objects in LRU order. Under pressure it also
removes trees that no pod on the node uses and whose `retainOnNode` has passed.
A tree replaced by a rebuild is renamed aside rather than deleted, because
running pods still mount it. It is removed once no pod uses that view.

**Why the tree is built inside the cache volume.** `link(2)` returns `EXDEV`
across two bind mounts, even when both sit on one filesystem. An init container
therefore cannot hardlink from a hostPath cache into a pod's `emptyDir`. The
agent builds each tree once per node next to the objects, and pods mount it
read-only. Fifty pods reading one plate set read one set of inodes. If the cache
and the views are ever on different filesystems, the builder falls back to
copying and reports the copies.

## Quickstart (kind)

Requirements: Go 1.26+, Docker, kubectl, and the `c4` CLI (`brew install
mrjoshuak/tap/c4`) for the end-to-end run. The tools install silently:

```bash
make tools          # controller-gen, setup-envtest, kind (go install)
make envtest        # downloads kube-apiserver and etcd for envtest
make test           # unit tests and envtest
make e2e            # kind cluster + MinIO + the full scenario below
make e2e-down       # delete the cluster
```

`make e2e` (`hack/e2e/run.sh`) does the following:

1. Creates a single-node kind cluster, builds `c4-operator:dev` locally, loads it
   into kind, and deploys the operator with `kubectl apply -k config/default`.
2. Runs MinIO in the cluster as the store.
3. Generates a synthetic show (131 files, 2.77 GiB) of deterministic
   pseudo-random EXR-sized frames, a reference movie, and a model file. Some files
   are byte-identical within a shot and across shots.
4. Snapshots the show into MinIO with the released `c4` CLI
   (`c4 id -s show > show.c4m`).
5. Creates three Materializations:
   - `sh010`: from a ConfigMap holding `show.c4m`, with include
     `shots/sh010/`, `editorial/`, `weights/`.
   - `sh020`: from the stored c4m's C4 ID, with include `shots/sh020/`,
     `weights/`.
   - `missing`: names a file that was never uploaded.
6. Starts render pods that run `tree-id --expect <viewID> /assets` as a
   non-root user: two pods on `sh010`, one on `sh020`, a fourth `sh010` pod
   after corrupting a cached frame, and one on `missing`.
7. Asserts the results and writes the measurements to
   `hack/e2e/out/results.md`.

To use it by hand after `make e2e`:

```bash
kubectl -n render get materializations -o wide
kubectl -n render logs render-sh010-a -c c4-materialize
kubectl -n c4-system logs ds/c4-operator-agent
```

A pod asks for a Materialization with two annotations:

```yaml
metadata:
  annotations:
    c4.avalanche.io/materialize: sh010          # Materialization in the pod's namespace
    c4.avalanche.io/mount-path: /assets         # default /c4
```

See [`config/samples/materialization.yaml`](config/samples/materialization.yaml)
for a full Materialization and a 50-pod Job.

## Measured results

Environment: kind v0.33.0 (Kubernetes v1.37.0), one node, on Docker Desktop
(linuxkit 7.0.14) on an Apple Silicon Mac with 16 cores. The store is MinIO in
the same cluster, so "fetch" here means a local network hop and disk, not a WAN.
Frames are 16 MiB. Run of 2026-10-16:

| pod            | Materialization | wall to Ready (s) | agent ms | objects | cached | fetched | refetched | bytes fetched | bytes in tree |
| -------------- | --------------- | ----------------- | -------- | ------- | ------ | ------- | --------- | ------------- | ------------- |
| render-sh010-a | sh010           | 1.74              | 877      | 50      | 0      | 50      | 0         | 1,610,612,736 | 1,610,612,736 |
| render-sh010-b | sh010           | 0.93              | 3        | 50      | 50     | 0       | 0         | 0             | 1,610,612,736 |
| render-sh020-a | sh020           | 1.02              | 181      | 49      | 25     | 24      | 0         | 402,653,184   | 1,358,954,496 |
| render-sh010-c | sh010           | 1.23              | 29       | 50      | 49     | 1       | 1         | 16,777,216    | 1,610,612,736 |

- *wall to Ready*: from `kubectl apply` of the pod to its Ready condition,
  including scheduling, both containers starting, and the workload scanning its
  tree.
- *agent ms*: the agent's own time for the request: preflight, fetch and verify,
  and the tree check or build.
- `render-sh010-c` started after one cached frame was overwritten through the
  view on the node. The agent caught the moved mtime, refetched that one frame,
  and rebuilt the tree. `render-sh010-a`, still running, kept its original tree.

Totals for the four pods on the node:

| measure                                      | value                           |
| -------------------------------------------- | ------------------------------- |
| unique objects in the store (whole show)     | 106                             |
| unique objects fetched to the node           | 74, each once (+1 refetch)      |
| bytes fetched from the store                 | 2,030,043,136 (1.89 GiB)        |
| bytes materialized into pods                 | 6,190,792,704 (5.77 GiB)        |
| bytes on the node's disk                     | 2,013,265,920 (1.88 GiB)        |
| fetched / materialized                       | 0.33                            |
| `shots/sh030/` (in the store, never requested) | 0 bytes fetched               |

Every pod scanned its mount and got its Materialization's `viewID`. The e2e
also builds each expected subset on the host from hardlinks and scans it with
the c4 scanner. The controller's `viewID` matched both.

Cold versus warm: in this setup a cold request moved 1.5 GiB in 0.88 s, about
1.8 GB/s, which is local disk and SHA-512 throughput. Against a real object
store, cold time is bytes fetched divided by store bandwidth, while warm time
stays a stat per file. The warm `sh010` request took 3 ms for 50 files. `sh020`
reused the shared weights and 24 shared frames and fetched only its 24 unique
frames.

Failure cases, from the same run:

- Missing content. The `missing` Materialization went `Failed` with
  `missing: 1`. Its pod's init container exited with
  `content missing from store: 1 of 2 objects, e.g. plates/never-uploaded.exr (c418Yf...)`
  before fetching anything, and the workload container never started.
- Unknown Materialization. Admission refused the pod: `admission webhook
  "pods.c4.avalanche.io" denied the request: materialization
  "no-such-materialization": ... not found`.

Status after the run (abridged):

```yaml
status:
  phase: Ready
  sourceID: c42RKtpjnbycDqTnuRfBpM8j7oBGE3RgLthFtHvdMhtJkqdWmrTb2ht6omQhQ8ZJaHM38Fg8CBMM5aCS9uNnH7HqwC
  rootID: c45NhuUT8z7Q7dDJB2ocpK1HWZPDnFTtAuiNtAZq5ig2QuBiSmEWwcrT8seqG2mZij2VCkRLAU98JeKFaGTgLnXDLo
  viewID: c44SYdn8GXJmDnyttxEyo1oDS68wzSmXweGyJ4e6n1mvL4d2XEjm8VtMERTmPLZNb28Diz2dNGqbvCZm3HAUtp21Q4
  files: 50
  bytes: 1610612736
  uniqueObjects: 50
  uniqueBytes: 1610612736
  nodes:
  - node: c4-operator-e2e-control-plane
    requests: 3
    hits: 99
    misses: 51
    refetched: 1
    bytesFetched: 1627389952
    bytesMaterialized: 4831838208
    lastReadyMillis: 29
```

The `sh010` (ConfigMap) and `sh020` (stored ID) Materializations report the
same `rootID`, because they describe the same show. The ConfigMap's `sourceID`
equals the ID the `c4` CLI printed when it stored the c4m.

## Relation to C4 and SMPTE ST 2114

A C4 ID is a SHA-512 digest encoded as a 90-character string that starts with
`c4`, as standardized in SMPTE ST 2114:2017. Because the ID is derived from the
bytes, any holder of the bytes can check them. That is what lets the node trust
its cache: it never trusts a name, only a recomputation.

A c4m file describes a filesystem: one line per entry with mode, timestamp,
size, name, and C4 ID, nested by indentation. It is small enough for a
ConfigMap. The `show.c4m` for the 2.77 GiB e2e show is 21 KB.

This operator is a consumer of the C4 ecosystem, not a fork of it. It imports
the [c4 Go module](https://github.com/Avalanche-io/c4) for IDs, c4m parsing,
base-chain resolution, the content-mode scanner, and the `TreeStore` and
`S3Store` backends. It reads stores written by the `c4` CLI and the
`avalancheio/c4` images, with the same `C4_STORE` URL forms
(`s3://bucket/prefix?region=..&endpoint=..`). It follows c4's "push intent, pull
content" model: the c4m (intent) travels through the Kubernetes API, and each
node pulls content by ID.

## What it is not yet

- **Not released.** `go.mod` uses a local `replace` for the c4 module, because
  the operator needs c4 APIs that are on `master` but in no release yet. See
  `PROGRESS.md`.
- **hostPath, not CSI.** Pods mount the view through a hostPath volume, which
  namespaces under the Pod Security `baseline` or `restricted` levels reject. A
  CSI node plugin publishing the view as an inline ephemeral volume is the right
  long-term interface. It would also remove the init container.
- **Trust boundary.** Any process that can reach the agent socket can ask the
  node to materialize any Materialization in any namespace. Only pods with the
  injected hostPath can reach it, but the agent does not yet check that the
  caller's namespace matches the request. The agent and controller read store
  credentials from Secrets in the Materialization's namespace, so the agent's
  ClusterRole can get Secrets cluster-wide.
- **Read-only trees.** Views are mounted read-only and every file is mode 0444.
  Executable bits and per-file timestamps from the c4m are not applied, because
  hardlinked files share one inode and the views are content-level. There is no
  writable overlay for jobs that edit their inputs in place.
- **No output capture.** The `render-pipeline.yaml` pattern (scan an output
  directory, store new content, write `output.c4m`) is not implemented.
- **No cache-aware scheduling.** Pods are not steered toward nodes that already
  hold most of a view. The per-node numbers in `status.nodes` are the input such
  a scorer would need.
- **No peer or regional tier.** Each node fetches from the store. Nodes do not
  fetch from each other, and there is no shared regional cache between the
  nodes and the store.
- **Corruption detection is cheap by default.** `verify: Stat` catches writes,
  which move the mtime. It does not catch bit rot, or a write followed by
  resetting the mtime. `verify: Full` catches both, at the cost of rehashing on
  every request. There is no background scrubber.
- **Changing a Materialization under running pods.** A pod is admitted for one
  view. If the spec or ConfigMap changes before the pod's init container runs on
  a node that has never seen the old view, the init container fails and asks for
  the pod to be recreated.
- **Unreachable stores look like missing content** (c4's `S3Store.Has` returns
  false on network errors).
- **Symlinks** are materialized as recorded, but trees with symlinks are not
  covered by the `tree-id` equality check.
- **Scale untested.** The e2e uses one node, 131 files, and 2.8 GiB. Trees with
  millions of files, many nodes, and eviction under real pressure have only unit
  test coverage.

## Layout

```
api/v1alpha1/          Materialization types
cmd/c4-operator/       one binary: manager, agent, ensure (init), tree-id
internal/manifest/     c4m source -> view: records, bases, sequences, include filter, view ID
internal/storeurl/     C4_STORE URL forms -> read-only store
internal/cache/        node cache: fetch, verify, refetch, LRU eviction, view metadata
internal/tree/         hardlinked tree builder and checker
internal/agent/        node agent: socket API, preflight, status reporting, GC loop
internal/controller/   reconciler (envtest)
internal/webhook/      pod mutation, self-managed webhook certificate
config/                CRD, RBAC, manager, agent, webhook, samples (kustomize)
hack/e2e/              kind config, MinIO, seed tool, run.sh
```

## License

Apache License 2.0, the same as c4. See [LICENSE](LICENSE).
