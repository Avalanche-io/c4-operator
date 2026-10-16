
## P1: overlay views, capture, CSI (2026-10-16T08:48Z)

Show: files=134 bytes=742393856 unique=108 uniqueBytes=637536256. Ingested in 0.24 s as view `c41jet912oABEHhftvchVhYqhp3Y54tcmjCYkhMZxRymPgBxWVgtth8vXeGvfZQ5kxoojNpMCkLJaNq4mhBhCCiFmF`. Objects per shard (files and records): s0=135 s1=158 s2=154 

Materialization sh010: 52 files, 402655232 unique bytes. Its view ID equals an independent scan of the same subset on the host.
A pod with the MVP's hostPath volume is refused by Pod Security restricted (`violates PodSecurity "restricted:latest"`); every pod below uses the csi volume and is admitted.

### Layers and uppers while 16 pods are mounted

| node                    | layers in the view's stack | uppers mounted | links on one plate's inode | objects (bytes) | objects and layers together (bytes) |
| ----------------------- | -------------------------- | -------------- | -------------------------- | --------------- | ----------------------------------- |
| c4-operator-e2e-worker  | 1                          | 2              | 2                          | 402662076       | 402663381                           |
| c4-operator-e2e-worker2 | 1                          | 2              | 2                          | 402662076       | 402663381                           |
| c4-operator-e2e-worker3 | 1                          | 2              | 2                          | 402662076       | 402663380                           |
| c4-operator-e2e-worker4 | 1                          | 2              | 2                          | 402662076       | 402663381                           |
| c4-operator-e2e-worker5 | 1                          | 2              | 2                          | 402662076       | 402663380                           |
| c4-operator-e2e-worker6 | 1                          | 2              | 2                          | 402662076       | 402663381                           |
| c4-operator-e2e-worker7 | 1                          | 2              | 2                          | 402662076       | 402663381                           |
| c4-operator-e2e-worker8 | 1                          | 2              | 2                          | 402662076       | 402663381                           |

Each node holds one layer for the view, shared by both of its pods, and one upper per pod. A plate's inode has two links, the cache's and the layer's; the pods reach it only through overlay. Measured in one `du` run, which counts an inode once, objects and layers together take only a few kilobytes more than the objects alone: the layer costs directory entries, not file bytes.

### Per pod

| pod       | node                    | publish | publish s | layers | added | modified | deleted | uploaded | captured tree equals the pod's own scan |
| --------- | ----------------------- | ------- | --------- | ------ | ----- | -------- | ------- | -------- | --------------------------------------- |
| render-1a | c4-operator-e2e-worker  | cold    | 0.815     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-1b | c4-operator-e2e-worker  | warm    | 0.002     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-2a | c4-operator-e2e-worker2 | cold    | 0.081     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-2b | c4-operator-e2e-worker2 | warm    | 0.002     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-3a | c4-operator-e2e-worker3 | cold    | 0.079     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-3b | c4-operator-e2e-worker3 | warm    | 0.002     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-4a | c4-operator-e2e-worker4 | cold    | 0.078     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-4b | c4-operator-e2e-worker4 | warm    | 0.002     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-5a | c4-operator-e2e-worker5 | cold    | 1.000     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-5b | c4-operator-e2e-worker5 | warm    | 0.001     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-6a | c4-operator-e2e-worker6 | cold    | 0.081     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-6b | c4-operator-e2e-worker6 | warm    | 0.001     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-7a | c4-operator-e2e-worker7 | cold    | 0.080     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-7b | c4-operator-e2e-worker7 | warm    | 0.002     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-8a | c4-operator-e2e-worker8 | cold    | 0.063     | 1      | 6     | 1        | 2       | 5        | yes                                     |
| render-8b | c4-operator-e2e-worker8 | warm    | 0.002     | 1      | 6     | 1        | 2       | 5        | yes                                     |

- Cold publishes (first pod on a node): 8, median 0.0805 s. Warm publishes (second pod): 8, median 0.002 s.
- Bytes hashed on the read path across all nodes: 0. Content is verified where it enters (ingest, capture), never when it is read.
- Every captured output tree equals the tree the pod scanned after writing, and render-1a's equals a recomputation on the host from the source files.

### v12 = v11 + 3 frames

| pod   | node                    | publish | publish s | layers |
| ----- | ----------------------- | ------- | --------- | ------ |
| v12-1 | c4-operator-e2e-worker  | delta   | 0.003     | 2      |
| v12-2 | c4-operator-e2e-worker2 | delta   | 0.016     | 2      |
| v12-3 | c4-operator-e2e-worker3 | delta   | 0.011     | 2      |
| v12-4 | c4-operator-e2e-worker4 | delta   | 0.009     | 2      |
| v12-5 | c4-operator-e2e-worker5 | delta   | 0.020     | 2      |
| v12-6 | c4-operator-e2e-worker6 | delta   | 0.007     | 2      |
| v12-7 | c4-operator-e2e-worker7 | delta   | 0.005     | 2      |
| v12-8 | c4-operator-e2e-worker8 | delta   | 0.009     | 2      |

The changeset resolves to a chain of 2 views. Every node that held v11 added one delta layer for v12 and fetched 17 objects in total across 8 nodes (three new frames per node, minus any a node already had), while v12 has 55 files. Read-only mounts of v12 on all eight nodes match the host's recomputation.


## P2: tiers and thundering herds (2026-10-16T09:10Z)

Each herd reads a shot of 24 frames of 8 MiB, unique content per run, so every run starts cold for that shot. Eight workers in two localities; three regional replicas; three MinIO shards. Store GETs are counted by MinIO itself (`minio_s3_requests_total{api="getobject"}`). Objects per shot include the directory records nodes fetch to resolve the view.

| path   | pods | objects | store GETs | store GETs per object | regional requests per object | peer requests per object | wall s |
| ------ | ---- | ------- | ---------- | --------------------- | ---------------------------- | ------------------------ | ------ |
| tiered | 8    | 28      | 52         | 1.86                  | 2.00                         | 6.00                     | 3.83   |
| tiered | 32   | 28      | 46         | 1.64                  | 2.00                         | 6.00                     | 6.69   |
| tiered | 64   | 28      | 42         | 1.50                  | 2.00                         | 6.00                     | 11.15  |
| direct | 8    | 28      | 224        | 8.00                  | 0.00                         | 0.00                     | 3.81   |
| direct | 32   | 28      | 224        | 8.00                  | 0.00                         | 0.00                     | 6.56   |
| direct | 64   | 28      | 224        | 8.00                  | 0.00                         | 0.00                     | 10.30  |
| naive  | 8    | 28      | 224        | 8.00                  | 0.00                         | 0.00                     | 3.86   |
| naive  | 32   | 28      | 896        | 32.00                 | 0.00                         | 0.00                     | 6.13   |
| naive  | 64   | 28      | 1792       | 64.00                 | 0.00                         | 0.00                     | 29.56  |

Store GETs per object stay flat on the tiered path as pods grow: each object is fetched from the store by at most its two regional homes (r = 2), and each locality's home fetches it once from the regional tier. On the Direct path every node reads the store once per object (node-level coalescing only), and per-pod fetching reads it once per pod.

### A regional replica killed mid-herd

32 pods read a shot of 8 frames of 64 MiB (12 objects with records) through the regional tier, paced at 8 MiB/s per response so transfers last seconds. At 1.30 s, once it had streamed more than 32 MiB, the busiest regional replica, `c4-regional-57bc8cfd95-l6s7v`, was force-deleted (SIGKILL, no grace).

| measure                                                | value                               |
| ------------------------------------------------------ | ----------------------------------- |
| pods that completed                                    | 32 of 32, in 14.76 s                |
| store GETs                                             | 20 for 12 objects (1.67 per object) |
| transfers resumed from their offset on another replica | 7                                   |
| node requests that went to the store directly          | 0                                   |


## P3: isolation (2026-10-16T09:26Z)

Projects `iso-a` and `iso-b` are Strict, each with its own bucket and its own MinIO user whose policy allows only that bucket. `dept-1` and `dept-2` are Shared in sharing group `show-x`, also with their own buckets and users. The same show (134 files) is in all four buckets and has the same view ID in each, c42aH8EBP5cjxARq5hfMbubcxqcGRpowrDxrGnQ1cF2K9N3NDmS4njVZPtf9T6yjh7UUkZYHUrAGG1itUzMVU47zDp. Shot sh900 is only in `iso-a`'s bucket (A's unreleased work); shot sh500 is only in `dept-1`'s. Every attempt below is made from project B's side.

### Attempts to read another project's data

| attempt                                                                          | made as                       | outcome                                                                                                            |
| -------------------------------------------------------------------------------- | ----------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| GET A's objects from A's bucket                                                  | B's MinIO user, from the host | refused by MinIO: 403: Access Denied.                                                                              |
| Materialization naming A's unreleased view by ID                                 | namespace `iso-b`             | Failed ResolveFailed: description is incomplete: c41nzrCtvK... not in store: s3 get c41nzrCtvK...: not in store    |
| ConfigMap c4m listing A's object IDs                                             | namespace `iso-b`             | Failed ContentMissing: 4 of 4 objects are not in the store, e.g. shots/sh900/plates/sh900.1001.exr (c464GRpeZe...) |
| GET an object of A from the regional tier, no token                              | pod in `iso-b`                | 401 bearer token required                                                                                          |
| the same with the pod's own token for audience `c4-tier`                         | pod in `iso-b`                | 403 system:serviceaccount:iso-b:default may not use the tier API                                                   |
| GET it from the node daemon's peer port, no token                                | pod in `iso-b`                | 401 bearer token required                                                                                          |
| the same with the pod's token                                                    | pod in `iso-b`                | 403 system:serviceaccount:iso-b:default may not use the tier API                                                   |
| the same, asking as project `iso-b`                                              | pod in `iso-b`                | 403 system:serviceaccount:iso-b:default may not use the tier API                                                   |
| CSI volume attributes claiming A's pod (namespace, name, UID) and a forged token | pod in `iso-b`                | pod stays Pending; FailedMount: materialization iso-b/show: materializations.c4.avalanche.io "show" not found      |

### Partitions on one node (c4-operator-e2e-worker3)

|                                                          | Strict: `iso-a` then `iso-b`                                       | Shared: `dept-1` then `dept-2`                                                                                    |
| -------------------------------------------------------- | ------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------- |
| view ID in both projects                                 | c42aH8EBP5...                                                      | the same                                                                                                          |
| copies of plate sh010.1001 in the node cache             | 2 (inodes 2228733 and 2229345, partitions `p-iso-a` and `p-iso-b`) | 1 (inode 2401368, 2 links, partition `g-show-x`)                                                                  |
| second project's publish                                 | cold, 0.056 s, 1 layer                                             | warm, 0.002 s, 1 layer                                                                                            |
| node upstream requests during the second project's mount | 120                                                                | 0                                                                                                                 |
| second project's fills at the regional tier              | 120, each from its own store                                       | 0 requests                                                                                                        |
| second project's bucket during its turn                  | 132 GETs, 109 HEADs                                                | 12 GETs (the manager reading the view's directory records), 109 HEADs (preflight: every object, in its own store) |

Proof of possession: shot sh500 is only in `dept-1`'s bucket. After a `dept-1` pod used it on c4-operator-e2e-worker3, its bytes sat in the shared partition (frame 1001 at inode 2402528). `dept-2` asking for it by ID: Failed ResolveFailed: description is incomplete: c44hV64NgU... not in store: s3 get c44hV64NgU...: not in store. By a c4m listing its IDs: Failed ContentMissing: 4 of 4 objects are not in the store, e.g. shots/sh500/plates/sh500.1001.exr (c41EJBPzZa...).

### Grants: a vendor project on the show's store

Project `vendor` reads `iso-a`'s bucket through its own MinIO user, with scope Grants and one grant: the show's view, paths `shots/sh010/`.

| Materialization in `vendor`                                      | outcome                                                                                                                                          |
| ---------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| the granted view                                                 | Ready with 49 files; a pod verified its mount is the sh010 subset, c44oVCvP5L...                                                                 |
| the granted view with include `shots/sh020/`                     | Failed NotGranted: nothing in granted tree c42aH8EBP5... under paths [shots/sh010/] matches include [shots/sh020/]                               |
| sh900, another tree in the same bucket                           | Failed NotGranted: project vendor may materialize only granted trees, and neither source c41nzrCtvK... nor its base c41nzrCtvK... is one of them |
| a ConfigMap c4m listing exactly the granted files                | Failed NotGranted: project vendor may materialize only granted trees, and neither source c464JBzt9S... nor its base c44oVCvP5L... is one of them |
| the granted view after the grant is removed (scope stays Grants) | Failed NotGranted: project vendor has scope Grants and no grants, 0.12 s after the Project change                                                |
| a new pod mounting it after the revocation                       | Pending; FailedMount: materialization vendor/show is not ready (phase "Failed")                                                                  |
| the pod that mounted it before the revocation                    | still running, still reads its 48 plates: revocation takes effect at the next mount                                                              |


## P5: the work queue on the e2e farm (2026-10-16T10:22Z, policy objective)

Pools: cpu (4 nodes, 4 slots each), gpu (2 nodes, 4 slots each), and spot (2 nodes, 4 slots each, unprovisioned until a CapacityRequest asks for them; the kind-sim provider untaints them after a 20 s provisioning delay). Each show has a quota of 8 nominal cpu slots plus 8 borrowable, 4 + 4 gpu slots, and 0 + 8 spot slots, a burst budget of $5 a day, and lateness priced at $200 an hour. Times are seconds after the first submission; the run took 316 s.

| job                       | class  | submitted | deadline | finished | deadline met  | units done | preemptions |
| ------------------------- | ------ | --------- | -------- | -------- | ------------- | ---------- | ----------- |
| show-a/sh010-comp-v003    | render | +0 s      | +420 s   | +274 s   | yes, +146 s   | 96 of 96   | 0           |
| show-b/sh020-light-v002   | render | +0 s      | +540 s   | +168 s   | yes, +372 s   | 72 of 72   | 0           |
| show-a/ci-change-4521     | ci     | +0 s      | +240 s   | +76 s    | yes, +164 s   | 16 of 16   | 0           |
| show-b/train-denoise      | ml     | +0 s      | +86400 s | +314 s   | yes, +86086 s | 1 of 1     | 1           |
| show-b/train-upres        | ml     | +0 s      | +86400 s | +276 s   | yes, +86124 s | 1 of 1     | 1           |
| show-a/sh030-fx-v001      | render | +60 s     | +264 s   | +136 s   | yes, +128 s   | 24 of 24   | 0           |
| show-b/sh040-lookdev-v001 | render | +90 s     | +241 s   | +252 s   | no, -11 s     | 144 of 144 | 0           |

Deadlines met: 6 of 7. The CI change's deadline is its latency target (4 minutes) from when it was submitted.

Reclaim: 2 yields of training runs for the urgent GPU render.
- train-denoise yielded (reclaim-nominal) at 2026-10-16T10:23:32Z, its checkpoint captured as view c41AgYJRC7...
- train-upres yielded (restore-feasibility) at 2026-10-16T10:24:14Z, its checkpoint captured as view c42EFFmk9N...
- train-denoise resumed: "training from step 315 of 1200", reading the checkpoint from that view.
- train-upres resumed: "training from step 523 of 1200", reading the checkpoint from that view.

Burst: the queue first asked for spot capacity at 120 s (peak request 8 slots); slots were provisioned by 142 s and the first unit ran on spot at 142 s. Burst spend for the run: $0.03.

Estimate of sh010-comp-v003 (binary order; frames cost 6 s, and 24 s from 1040 to 1052). W is the estimator's remaining work in slot-seconds; the truth is what the units not yet finished went on to take (a running unit counts half). The render-manager estimate is the mean of finished frames times the frames left:

| point in the run | frames done | W (estimator) | W90 | truth | estimator error | render-manager error |
| ---------------- | ----------- | ------------- | --- | ----- | --------------- | -------------------- |
| 10%              | 16 of 96    | 592           | 851 | 656   | 10%             | 16%                  |
| 25%              | 21 of 96    | 596           | 683 | 590   | 1%              | 8%                   |
| 50%              | 45 of 96    | 388           | 419 | 388   | 0%              | 3%                   |
| 75%              | 77 of 96    | 166           | 181 | 163   | 2%              | 10%                  |

Output: sh010-comp-v003 folded 96 unit changesets into its output view c435ue2eEo...; the stored chain c43PVtxxnV... has one section per fold, each sealed by the output view after it.

## P7: provenance (2026-10-16T10:32Z)

|                        | sh050-render-v001, frame 1004                                 | sh050-denoise-v001, frame 1004                                          |
| ---------------------- | ------------------------------------------------------------- | ----------------------------------------------------------------------- |
| record                 | c426nmvP19...                                                 | c44E8hcKWB...                                                           |
| signature              | verified, key c4-23906723b314e8a4                             | verified, key c4-23906723b314e8a4                                       |
| unit, attempt, node    | 3, 1, c4-operator-e2e-worker                                  | 3, 1, c4-operator-e2e-worker5                                           |
| image                  | docker.io/library/c4-e2e:dev @sha256:385c13d9ed12...          | docker.io/library/c4-e2e:dev @sha256:385c13d9ed12...                    |
| input                  | p5-show c42aH8EBP5...                                         | the render's output c43LdExVdn..., produced by show-a/sh050-render-v001 |
| file                   | c44f7BwBRD... (subject digest: c4 and the SHA-512 it encodes) | c41Lo1hzwS...                                                           |
| declared AI use        | none                                                          | assisted, denoise-v3, human_validated, consent plates-release-2026-031  |
| C2PA digitalSourceType | algorithmicMedia                                              | compositeWithTrainedAlgorithmicMedia                                    |
| C2PA AI disclosure     | no                                                            | yes (c2pa.ai-disclosure)                                                |

The signing key (Secret `c4-provenance-key`) is mounted only by `c4-system/node` pods; a task's service account may read it: no.

Checked outside the cluster with the public key only: the stored record says "verified: signed by c4-23906723b314e8a4 for unit 3 of sh050-denoise-v001 on c4-operator-e2e-worker5". The same record with its AI use changed to "none" and the signature kept says "rejected: no trusted key signed this record".

An agent asking the MCP server (c4_trace_file, over stdio, as the wrangler) got the same record in 5 ms: verified True, input produced by show-a/sh050-render-v001.

