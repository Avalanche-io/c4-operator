
## P1: overlay views, capture, CSI (2026-10-16T08:48Z)

Show: files=134 bytes=742393856 unique=108 uniqueBytes=637536256. Ingested in 0.24 s as view `c41jet912oABEHhftvchVhYqhp3Y54tcmjCYkhMZxRymPgBxWVgtth8vXeGvfZQ5kxoojNpMCkLJaNq4mhBhCCiFmF`. Objects per shard (files and records): s0=135 s1=158 s2=154 

Materialization sh010: 52 files, 402655232 unique bytes. Its view ID equals an independent scan of the same subset on the host.
A pod with the MVP's hostPath volume is refused by Pod Security restricted (`violates PodSecurity "restricted:latest"`); every pod below uses the csi volume and is admitted.

### Layers and uppers while 16 pods are mounted

| node | layers in the view's stack | uppers mounted | links on one plate's inode | objects (bytes) | objects and layers together (bytes) |
|---|---|---|---|---|---|
| c4-operator-e2e-worker | 1 | 2 | 2 | 402662076 | 402663381 |
| c4-operator-e2e-worker2 | 1 | 2 | 2 | 402662076 | 402663381 |
| c4-operator-e2e-worker3 | 1 | 2 | 2 | 402662076 | 402663380 |
| c4-operator-e2e-worker4 | 1 | 2 | 2 | 402662076 | 402663381 |
| c4-operator-e2e-worker5 | 1 | 2 | 2 | 402662076 | 402663380 |
| c4-operator-e2e-worker6 | 1 | 2 | 2 | 402662076 | 402663381 |
| c4-operator-e2e-worker7 | 1 | 2 | 2 | 402662076 | 402663381 |
| c4-operator-e2e-worker8 | 1 | 2 | 2 | 402662076 | 402663381 |

Each node holds one layer for the view, shared by both of its pods, and one upper per pod. A plate's inode has two links, the cache's and the layer's; the pods reach it only through overlay. Measured in one `du` run, which counts an inode once, objects and layers together take only a few kilobytes more than the objects alone: the layer costs directory entries, not file bytes.

### Per pod

| pod | node | publish | publish s | layers | added | modified | deleted | uploaded | captured tree equals the pod's own scan |
|---|---|---|---|---|---|---|---|---|---|
| render-1a | c4-operator-e2e-worker | cold | 0.815 | 1 | 6 | 1 | 2 | 5 | yes |
| render-1b | c4-operator-e2e-worker | warm | 0.002 | 1 | 6 | 1 | 2 | 5 | yes |
| render-2a | c4-operator-e2e-worker2 | cold | 0.081 | 1 | 6 | 1 | 2 | 5 | yes |
| render-2b | c4-operator-e2e-worker2 | warm | 0.002 | 1 | 6 | 1 | 2 | 5 | yes |
| render-3a | c4-operator-e2e-worker3 | cold | 0.079 | 1 | 6 | 1 | 2 | 5 | yes |
| render-3b | c4-operator-e2e-worker3 | warm | 0.002 | 1 | 6 | 1 | 2 | 5 | yes |
| render-4a | c4-operator-e2e-worker4 | cold | 0.078 | 1 | 6 | 1 | 2 | 5 | yes |
| render-4b | c4-operator-e2e-worker4 | warm | 0.002 | 1 | 6 | 1 | 2 | 5 | yes |
| render-5a | c4-operator-e2e-worker5 | cold | 1.000 | 1 | 6 | 1 | 2 | 5 | yes |
| render-5b | c4-operator-e2e-worker5 | warm | 0.001 | 1 | 6 | 1 | 2 | 5 | yes |
| render-6a | c4-operator-e2e-worker6 | cold | 0.081 | 1 | 6 | 1 | 2 | 5 | yes |
| render-6b | c4-operator-e2e-worker6 | warm | 0.001 | 1 | 6 | 1 | 2 | 5 | yes |
| render-7a | c4-operator-e2e-worker7 | cold | 0.080 | 1 | 6 | 1 | 2 | 5 | yes |
| render-7b | c4-operator-e2e-worker7 | warm | 0.002 | 1 | 6 | 1 | 2 | 5 | yes |
| render-8a | c4-operator-e2e-worker8 | cold | 0.063 | 1 | 6 | 1 | 2 | 5 | yes |
| render-8b | c4-operator-e2e-worker8 | warm | 0.002 | 1 | 6 | 1 | 2 | 5 | yes |

- Cold publishes (first pod on a node): 8, median 0.0805 s. Warm publishes (second pod): 8, median 0.002 s.
- Bytes hashed on the read path across all nodes: 0. Content is verified where it enters (ingest, capture), never when it is read.
- Every captured output tree equals the tree the pod scanned after writing, and render-1a's equals a recomputation on the host from the source files.

### v12 = v11 + 3 frames

| pod | node | publish | publish s | layers |
|---|---|---|---|---|
| v12-1 | c4-operator-e2e-worker | delta | 0.003 | 2 |
| v12-2 | c4-operator-e2e-worker2 | delta | 0.016 | 2 |
| v12-3 | c4-operator-e2e-worker3 | delta | 0.011 | 2 |
| v12-4 | c4-operator-e2e-worker4 | delta | 0.009 | 2 |
| v12-5 | c4-operator-e2e-worker5 | delta | 0.020 | 2 |
| v12-6 | c4-operator-e2e-worker6 | delta | 0.007 | 2 |
| v12-7 | c4-operator-e2e-worker7 | delta | 0.005 | 2 |
| v12-8 | c4-operator-e2e-worker8 | delta | 0.009 | 2 |

The changeset resolves to a chain of 2 views. Every node that held v11 added one delta layer for v12 and fetched 17 objects in total across 8 nodes (three new frames per node, minus any a node already had), while v12 has 55 files. Read-only mounts of v12 on all eight nodes match the host's recomputation.

