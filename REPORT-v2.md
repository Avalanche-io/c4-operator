# c4-operator v2: report

This report covers the v2 redesign of c4-operator: what was built, what each phase proved on an eight-worker kind cluster, what is designed but not built, and the decisions that are Joshua's. The design is in [`DESIGN.md`](DESIGN.md); every number below comes from [`hack/e2e/out/results.md`](hack/e2e/out/results.md), [`hack/e2e/monorepo/RESULTS.md`](hack/e2e/monorepo/RESULTS.md) or [`hack/sim/out/results.md`](hack/sim/out/results.md), each reproducible by the script named there.

Everything ran on one machine: kind v0.33 with Kubernetes 1.36.1, one control plane and eight workers in two zones, three MinIO shards as the store, on Docker Desktop (16 CPUs, 47 GB) on an Apple M3 Max. Nothing was pushed to a remote or a registry.

## Summary

The operator gives each pod a C4 view of exactly the files it needs, mounted through a CSI driver as an overlay of shared, hardlinked layers, filled through node, peer and regional caches that read each object from the store about once however many pods need it. What a pod writes is captured as a changeset when it exits, signed in a task record, and folded into its job's output view. A deadline-driven queue runs render, CI and ML work on the same farm without priorities; a HUD shows the farm from live cluster state; an MCP server gives agents every HUD capability as the calling user.

Headline numbers:

- **Store load stays flat as pods grow.** 64 pods reading a 28-object shot cost the store 42 GETs through the tiers (1.5 per object), 224 with node-level caching only, and 1,792 with per-pod copies.
- **Deadlines without priorities.** On a contended farm, 6 of 7 jobs met their deadlines while a training run yielded its GPU slots at a checkpoint and resumed from its captured view, and burst capacity arrived within the provider's delay. In a five-week simulation the policy missed 43% fewer render deadlines than manual priorities.

## Phases

| phase | status | evidence |
| --- | --- | --- |
| P1 views, capture, CSI | done | `hack/e2e/p1.sh` |
| P2 tiers | done | `hack/e2e/p2.sh` |
| P3 isolation | done | `hack/e2e/p3.sh`, `internal/controller` envtest |
| P4 monorepo CI | done | `hack/e2e/p4.sh`, `hack/e2e/monorepo/run.sh` |
| P5 work system and HUD | done, with gaps listed below | `hack/e2e/p5.sh`, `go run ./hack/sim`, `internal/work/queue` envtest, `docs/hud/live` |
| P6 agent interface | done, with gaps listed below | `hack/e2e/p6.sh`, `internal/mcpserver` tests |
| P7 provenance | done, with gaps listed below | `hack/e2e/p7.sh`, `internal/provenance` tests |

### P1: views, overlay layers, capture, CSI

Pods under Pod Security `restricted` mount views through the CSI driver (inline ephemeral volumes; the node daemon checks the pod's projected token against the volume context). On eight workers with two pods each:

- One layer per node serves both pods (a plate's inode has two links: the cache's and the layer's); each pod has its own upper. Objects and layers together take a few kilobytes more than the objects alone.
- Cold publish (the first pod on a node) median 80 ms; warm 2 ms. Bytes hashed on the read path: 0. Content is verified where it enters (ingest, capture), not when it is read (DESIGN.md section 1).
- Every captured output equals the tree the pod scanned, and one equals a recomputation on the host from the source files.
- A changeset (v12 = v11 + 3 frames) mounts as a delta layer on all eight nodes, 17 objects fetched in total.

### P2: tiers and thundering herds

| path | pods | store GETs per object |
| --- | --- | --- |
| tiered (node, peer homes, regional r = 2) | 8, 32, 64 | 1.86, 1.64, 1.50 |
| Direct (each node reads the store) | 8, 32, 64 | 8.00 each |
| per-pod copies | 8, 32, 64 | 8, 32, 64 |

Store GETs are counted by MinIO itself. On kind every link runs at memory speed, so wall time is not where the tiers win here (64 pods: tiered 11.2 s, Direct 10.3 s, per-pod 29.6 s); store requests, egress and per-prefix request limits are. Killing the busiest regional replica with SIGKILL while it streamed: 32 of 32 pods finished, 7 transfers resumed from their offsets at the other home, no node fell through to the store, and store GETs stayed at 1.67 per object.

### P3: isolation

Every attempt by project B to read project A's data was refused: B's MinIO user against A's bucket (403), a Materialization naming A's view by ID (unresolvable from B's store), a hand-written c4m listing A's object IDs (preflight: every object missing), the regional tier and a node's peer port without a token (401) and with the pod's own token (403), and a CSI volume context claiming A's pod identity with a forged token (resolved in B's own namespace from the verified token). Strict projects get separate copies of identical bytes on a shared node; Shared projects share one copy, and a Shared project still cannot mount content its own store lacks (the preflight is the proof of possession). A vendor project with one grant sees exactly the granted shot; removing the grant fails its Materialization 0.1 s later, new pods cannot mount it, and pods already running keep their mount until they end. Records of a refused description are never written to the store.

### P4: monorepo CI

The importer (`import-git`) turns git's first-parent history into views, changesets and one chain per branch, copying c4git-managed assets by ID without rehashing them. On the synthetic monorepo (2,219 files, 401 commits, 62 c4git assets):

- Import into MinIO: 401 commits in 5.4 s, 7.5 ms per commit at p50. Cost follows the change: 1.1 ms per commit for 8,555 files grouped by team, 8.4 ms with 600 services in one directory (local stores, `hack/e2e/monorepo/RESULTS.md`).
- Push-to-view (local stores): 173 ms at p50 from `git push` to a stored view through a post-receive hook, 232 ms through a webhook listener.
- CI with 16 shards per change on four nodes: the parent commit mounts cold in 1.7 s per node, the next commit as a 3-file delta layer in 16 ms, a rerun in 2 ms. A git clone and c4git checkout of the same commit takes 1.1 s on the host's SSD.
- Which files changed between two commits, from directory records alone: 13 ms for one commit, 34 ms for 10, 221 ms for 100, each identical to `git diff --name-status`.
- `c4 log` on a branch chain lists exactly the commits' views. Getting there found a bug in the view records (a directory holding a symlink was recorded with size 0 instead of null), fixed in `internal/view`.

The one difference from Scion: Scion made source control server-side; this mirrors git into C4 views, push by push, and leaves git where it is.

### P5: the work system and the HUD

The queue (`internal/work/queue`) runs the scheduling policy (`internal/work/sched`, a pure function) every two seconds over WorkJobs, Pools, Projects and its unit pods. Slots are an extended resource on pool nodes, so kube-scheduler places units as the queue counts them.

On the kind farm (16 cpu slots, 8 GPU slots, up to 8 burst slots on spot nodes that the kind-sim provider untaints after a 20 s delay), seven jobs from two shows:

- 6 of 7 deadlines met. The late one, a lookdev render submitted at 90 s with a 150 s deadline, finished 11 s late: the queue asked for burst capacity once its first frames were measured, and capacity took the provider's delay to arrive.
- A training run yielded its GPU slots to reclaim show-a's nominal quota for an urgent render. It saved a checkpoint at its next step, capture stored it as a view, and the next attempt mounted that view and resumed from step 316 of 1,200. (In earlier runs of the same scenario a second run also yielded, to restore the urgent render's feasibility.)
- Burst: the request was seen at 125 s, 8 spot slots were provisioned by 138 s, units ran there from 143 s, and the run spent $0.03.
- The remaining-work estimate of a binary-order render was within 10% of the truth a tenth of the way in and within 3% from a quarter on; a render manager's mean-of-finished-frames estimate was off by 15%, 7%, 4% and 6%.
- Each job's output view is folded from its units' changesets as they finish, into a chain sealed after every fold. The HUD's preview player shows the frames from that view, served from the store by C4 ID.

In simulation (`go run ./hack/sim`, five seeded weeks of a three-show farm, mean over seeds):

| policy | render deadlines missed per week | late hours | CI p95 (min) |
| --- | --- | --- | --- |
| manual priorities | 43.0 | 107.8 | 17.3 |
| FIFO | 46.6 | 140.3 | 17.1 |
| EDF | 24.4 | 60.1 | 17.2 |
| objective (this policy) | 24.4 | 61.0 | 14.4 |
| objective with burst | 23.2 | 51.7 | 14.4 |

Against EDF on the same weeks the policy matches deadline performance and cuts the CI tail (p95 17.2 to 14.4 minutes, CI runs over 15 minutes from 9.1% to 4.2%); with burst it has fewer late hours on 4 of 5 weeks, for about $578 a week. With half the GPUs, burst cuts late hours from 382 (EDF) to 52. Binary order roughly halves the error of the remaining-time estimate when a shot has a heavy region (whole-render error 0.24 against 0.46 at 3x) and flags the region at a median 15% of the render instead of 46%. It costs makespan when heavy frames land in the last wave (median 5% longer at 3x, 12% at 8x), and W90 under-covers heavy shots (83% at 3x, 68% at 8x, against a target of 90%). The simulation also showed that the allocation formula as first written (`R = W90 / T`) misses more deadlines than EDF; the built policy reserves room for the last wave (`R = W90 / (T - t_path)`).

The HUD (`internal/hud`) is served by the manager from live cluster state: queue, job view with the binary fill and the cost curve, workers, cache tiers, dependencies, cost and capacity, an artist's scoped view, and controls (hold, boost, deadline, requeue, node avoidance, node disable, burst approval), each checked against the user's Kubernetes permissions and recorded as an Event. Screenshots: `docs/hud/live` (the kind farm) and `docs/hud` (a synthetic farm, `hack/hudmock`).

Gaps: generators (dynamic expansion) run in the policy and the simulator, not in the queue's classes; a comp job does not yet start on a render's partial output chain (downstream units start when the upstream job succeeds, through `fromJob`); ML gangs larger than a node need Kueue and JobSet, which are designed and not installed; the ML lens shows checkpoints and yields but not steps, which the workload does not report.

### P6: the agent interface

`c4-operator mcp` serves 17 tools over stdio and stateless Streamable HTTP, on the official Go SDK (MCP 2026-07-28). Each tool is a call to the HUD API as the caller, so an agent sees and changes exactly what its user could. Read tools are annotated read-only; requeue is destructive; approving burst capacity is open-world. Errors say what to do next. The tool reference (`docs/agents/tools.md`) is generated from the tool list; the `c4-farm` skill (`skills/c4-farm`) and `llms.txt` point agents at it. On the live farm, an MCP client answered all 10 questions of the evaluation set (`docs/agents/eval.xml`), each call in 0 to 9 ms including the HUD round trip, and a hold and release made through MCP were recorded as Events naming the user.

Gaps: no recorded session of an LLM agent driving the farm (the evaluation is answered by a reference client); no tools for the HUD's byte and stream endpoints (preview frames, objects, events); the designed explain, migrate, provider and vendor tools are not built.

### P7: provenance

At every capture the node daemon signs a task record: an in-toto Statement (v1) with the predicate `https://cccc.io/provenance/task/v1`, in a DSSE envelope signed with Ed25519, stored as a C4 object and indexed per job. A record names the job, unit, attempt, pod, node, container images by digest, the input views, the output view and changeset, every changed file by C4 ID (and the SHA-512 it encodes), and the job's declared AI use. On the kind cluster:

- A frame of a model-assisted denoise traces to the render it read (through its input view), with the declared model, oversight level and consent record; every record verifies against the farm's public key.
- The C2PA manifest definition for the render's frame says `algorithmicMedia`; the denoised frame's says `compositeWithTrainedAlgorithmicMedia`, with a `c2pa.ai-disclosure` assertion (C2PA 2.4). An OMC v2.8 Asset with the C4 ID as an identifier is emitted for each file.
- The signing key is mounted only by node daemons; task pods' service accounts cannot read it.
- Changing the declared AI use in a stored record, keeping its signature, makes it fail verification.
- The same trace through the MCP server answers in a few milliseconds.

Gaps: one farm key (per-node keys or a KMS are stronger); C2PA manifests are emitted as definitions for c2patool, not signed (a "Trusted" C2PA credential needs a certificate from the C2PA Conformance Program); the disclosure report and the forward trace are designed, not built.

## Designed, not built

- Lazy lower layers through FUSE (DESIGN.md section 4.6).
- Encryption with just-in-time keys from a project KMS (section 6.6).
- The signed, hash-chained usage ledger and marketplace exporters (section 12). Usage is metered in memory and shown in the HUD (cluster-hours, container-hours, GPU-hours); bytes avoided is shown as savings, never billed.
- The licensing broker for per-hour software licenses (section 16.2), cloud provider adapters beyond kind-sim (Cluster Autoscaler ProvisioningRequest, Karpenter), the Deadline bridge (section 14), DCC and production-tracking integrations (section 15), and a Helm chart (kustomize only today).
- Pre-warming new nodes for a capacity request, the chain service for live capture streams, and re-basing long output chains.

## Engineering notes

- Tests: the controllers and the queue run against a real API server (envtest); overlay, layer and capture tests run privileged in a Linux container (`make test-linux`); tier tests pass under `-race`; `go vet ./...` and `go test ./...` are clean at fcc0578.
- The MVP's weaknesses are gone: no hostPath for pods, no cluster-wide Secret read (credentials are Secrets in `c4-system`, read through a namespaced Role), node daemons may change only their own pods' output annotations (a ValidatingAdmissionPolicy), and tiers authenticate every request.
- Found and fixed along the way: yields were lost when the queue restarted (now kept in status), records were written for refused descriptions (now only after the preflight), and the view record size of directories holding symlinks.
- P1 to P3 ran on earlier builds of the branch; P4 to P7 on one build (d259c38). The symlink fix between them changes the view IDs of trees with symlinks in subdirectories and no measured quantity.
- Once, after the P7 run, the Docker Desktop VM stalled (load average above 500, API server unreachable, VM disk at 91%); restarting Docker Desktop brought the kind cluster back intact. Keep e2e data modest on the shared Docker disk.
- Changes proposed for c4, c4d, c4git and c4-containers are listed in `PROGRESS.md`; none of those repositories was modified.

## Decisions for Joshua

1. Name collisions before a launch: Cinema 4D ("C4D") and the C4 software architecture model.
2. The release path: a c4 suite release so `go.mod` can drop its `replace`, and the image registry path.
3. The billing model: cluster-hours with container-hours, GPU-hours, or fixed production windows, and prices.
4. Transport security between cache tiers (mTLS through a mesh, or the tiers' own certificates).
5. Whether Direct mode (node daemons holding store credentials) stays a supported mode.
6. Task-record key custody (one farm key, per-node keys, a KMS, or keyless signing), and whether to pursue C2PA conformance for trusted credentials.
7. The HUD's authorization model: per-write checks with an audit Event (as built), or impersonation.
8. Which proposed upstream changes (c4 library APIs, c4d range reads, the c4git smudge truncation) go into the next c4 suite release.

## Where things are

| what | where |
| --- | --- |
| design | `DESIGN.md` |
| measured results | `hack/e2e/out/results.md`, `hack/e2e/monorepo/RESULTS.md`, `hack/sim/out/results.md` |
| phase scripts | `hack/e2e/p1.sh` to `p7.sh` (after `hack/e2e/up.sh`) |
| HUD screenshots | `docs/hud/live`, `docs/hud` |
| agent docs | `docs/agents/tools.md`, `docs/agents/eval.xml`, `skills/c4-farm/SKILL.md`, `llms.txt` |
| research briefs | `docs/research/provenance.md`, `docs/research/ecosystem.md` |
| branches, blockers, proposals, open questions | `PROGRESS.md` |
