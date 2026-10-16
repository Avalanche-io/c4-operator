# c4-operator v2: report

This report covers the v2 redesign of c4-operator: what was built, what each phase proved on an eight-worker kind cluster, what is designed but not built, and the decisions that are Joshua's. The design is in [`DESIGN.md`](DESIGN.md); every number below comes from [`hack/e2e/out/results.md`](hack/e2e/out/results.md), [`hack/e2e/monorepo/RESULTS.md`](hack/e2e/monorepo/RESULTS.md) or [`hack/sim/out/results.md`](hack/sim/out/results.md), each reproducible by the script named there.

Everything ran on one machine: kind v0.33 with Kubernetes 1.36.1, one control plane and eight workers in two zones, three MinIO shards as the store, on Docker Desktop (16 CPUs, 47 GB) on an Apple M3 Max. Nothing was pushed to a remote or a registry.

## Summary

The operator gives each pod a C4 view of exactly the files it needs, mounted through a CSI driver as an overlay of shared, hardlinked layers, filled through node, peer and regional caches that read each object from the store about once however many pods need it. What a pod writes is captured as a changeset when it exits, signed in a task record, and folded into its job's output view. A deadline-driven queue runs render, CI and ML work on the same farm without priorities; a HUD shows the farm from live cluster state; an MCP server gives agents every HUD capability as the calling user.

Headline numbers:

- **Store load stays flat as pods grow.** 64 pods reading a 28-object shot cost the store 42 GETs through the tiers (1.5 per object), 224 with node-level caching only, and 1,792 with per-pod copies.
- **Deadlines without priorities.** On a contended farm, 6 of 7 jobs met their deadlines while a training run yielded its GPU slots at a checkpoint and resumed from its captured view, and burst capacity arrived within the provider's delay. In a five-week simulation the policy missed 43% fewer render deadlines than manual priorities.

## Phases

| phase                  | status                       | evidence                                                                              |
| ---------------------- | ---------------------------- | ------------------------------------------------------------------------------------- |
| P1 views, capture, CSI | done                         | `hack/e2e/p1.sh`                                                                      |
| P2 tiers               | done                         | `hack/e2e/p2.sh`                                                                      |
| P3 isolation           | done                         | `hack/e2e/p3.sh`, `internal/controller` envtest                                       |
| P4 monorepo CI         | done                         | `hack/e2e/p4.sh`, `hack/e2e/monorepo/run.sh`                                          |
| P5 work system and HUD | done, with gaps listed below | `hack/e2e/p5.sh`, `go run ./hack/sim`, `internal/work/queue` envtest, `docs/hud/live` |
| P6 agent interface     | done, with gaps listed below | `hack/e2e/p6.sh`, `internal/mcpserver` tests                                          |
| P7 provenance          | done, with gaps listed below | `hack/e2e/p7.sh`, `internal/provenance` tests                                         |

### P1: views, overlay layers, capture, CSI

Pods under Pod Security `restricted` mount views through the CSI driver (inline ephemeral volumes; the node daemon checks the pod's projected token against the volume context). On eight workers with two pods each:

- One layer per node serves both pods (a plate's inode has two links: the cache's and the layer's); each pod has its own upper. Objects and layers together take a few kilobytes more than the objects alone.
- Cold publish (the first pod on a node, 52 files, 384 MiB) median 80 ms; warm 2 ms. The median is mostly peer copies: the earliest node in each zone, which had to read beyond its zone, took 0.82 s and 1.00 s, and the other six, whose zone peers already held the bytes, took 63 to 81 ms. Bytes hashed on the read path: 0. Content is verified where it enters (ingest, capture), not when it is read (DESIGN.md section 1).
- Every captured output equals the tree the pod scanned, and one equals a recomputation on the host from the source files.
- A changeset (v12 = v11 + 3 frames) mounts as a delta layer on all eight nodes, 17 objects fetched in total.

### P2: tiers and thundering herds

| path                                      | pods      | store GETs per object |
| ----------------------------------------- | --------- | --------------------- |
| tiered (node, peer homes, regional r = 2) | 8, 32, 64 | 1.86, 1.64, 1.50      |
| Direct (each node reads the store)        | 8, 32, 64 | 8.00 each             |
| per-pod copies                            | 8, 32, 64 | 8, 32, 64             |

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

| policy                  | render deadlines missed per week | late hours | CI p95 (min) |
| ----------------------- | -------------------------------- | ---------- | ------------ |
| manual priorities       | 43.0                             | 107.8      | 17.3         |
| FIFO                    | 46.6                             | 140.3      | 17.1         |
| EDF                     | 24.4                             | 60.1       | 17.2         |
| objective (this policy) | 24.4                             | 61.0       | 14.4         |
| objective with burst    | 23.2                             | 51.7       | 14.4         |

Against EDF on the same weeks the policy matches deadline performance and cuts the CI tail (p95 17.2 to 14.4 minutes, CI runs over 15 minutes from 9.1% to 4.2%); with burst it has fewer late hours on 4 of 5 weeks, for about $578 a week. With half the GPUs, burst cuts late hours from 382 (EDF) to 52. Binary order roughly halves the error of the remaining-time estimate when a shot has a heavy region (whole-render error 0.24 against 0.46 at 3x) and flags the region at a median 15% of the render instead of 46%. It costs makespan when heavy frames land in the last wave (median 5% longer at 3x, 12% at 8x), and W90 under-covers heavy shots (83% at 3x, 68% at 8x, against a target of 90%). The simulation also showed that the allocation formula as first written (`R = W90 / T`) misses more deadlines than EDF; the built policy reserves room for the last wave (`R = W90 / (T - t_path)`).

The HUD (`internal/hud`) is served by the manager from live cluster state: queue, job view with the binary fill and the cost curve, workers, cache tiers, dependencies, cost and capacity, an artist's scoped view, and controls (hold, boost, deadline, requeue, node avoidance, node disable, burst approval), each checked against the user's Kubernetes permissions and recorded as an Event. Screenshots: `docs/hud/live` (the kind farm) and `docs/hud` (a synthetic farm, `hack/hudmock`).

Gaps: generators (dynamic expansion) run in the policy and the simulator, not in the queue's classes; a comp job does not yet start on a render's partial output chain (downstream units start when the upstream job succeeds, through `fromJob`); ML gangs larger than a node need Kueue and JobSet, which are designed and not installed; the ML lens shows checkpoints and yields but not steps, which the workload does not report.

### P6: the agent interface

`c4-operator mcp` serves 18 tools (17 in the P6 run, plus `c4_verify_custody` since) over stdio and stateless Streamable HTTP, on the official Go SDK (MCP 2026-07-28). Each tool is a call to the HUD API as the caller, so an agent sees and changes exactly what its user could. Read tools are annotated read-only; requeue is destructive; approving burst capacity is open-world. Errors say what to do next. The tool reference (`docs/agents/tools.md`) is generated from the tool list; the `c4-farm` skill (`skills/c4-farm`) and `llms.txt` point agents at it. On the live farm, an MCP client answered all 10 questions of the evaluation set (`docs/agents/eval.xml`), each call in 0 to 9 ms including the HUD round trip, and a hold and release made through MCP were recorded as Events naming the user.

Gaps: no recorded session of an LLM agent driving the farm (the evaluation is answered by a reference client); no tools for the HUD's byte and stream endpoints (preview frames, objects, events); the designed explain, migrate, provider and vendor tools are not built.

### P7: provenance

At every capture the node daemon writes a task record: an in-toto Statement (v1) with the predicate `https://cccc.io/provenance/task/v1`, signed in a DSSE envelope, stored as a C4 object and indexed per job. A record names the job, unit, attempt, pod, node, container images by digest, the input views, the output view and changeset, every changed file by C4 ID (and the SHA-512 it encodes), and the job's declared AI use. On the kind cluster, with the records signed by one farm key:

- A frame of a model-assisted denoise traces to the render it read (through its input view), with the declared model, oversight level and consent record; every record verifies.
- The C2PA manifest definition for the render's frame says `algorithmicMedia`; the denoised frame's says `compositeWithTrainedAlgorithmicMedia`, with a `c2pa.ai-disclosure` assertion (C2PA 2.4). An OMC v2.8 Asset with the C4 ID as an identifier is emitted for each file.
- Changing the declared AI use in a stored record, keeping its signature, makes it fail verification.
- The same trace through the MCP server answers in a few milliseconds.

**Signers per entity (built after the run above).** A provenance record is now the set of records each entity that did or decided part of the work signed itself (DESIGN.md section 9.2):

| record     | signed by                                                                                 |
| ---------- | ----------------------------------------------------------------------------------------- |
| task       | the node daemon that ran the unit, as `node/<node>`                                       |
| resolution | the controller: which view a Materialization resolved to, under which grant, all present |
| decision   | the queue: each preemption or yield, and for whom                                         |
| job        | the queue: a finished job's output, chain, index, decisions and controls                 |
| control    | the HUD: each HUD or MCP action, naming the actor, the sign-in, the client and the check |
| import     | the importer's own service account, per git commit                                        |
| ingest     | the ingesting workload's own service account                                              |

Each signer holds its own Ed25519 key and a short-lived certificate from a farm signing CA, issued by the manager against a projected service-account token (TokenReview, audience `c4-signer`). A node daemon's token names its node, so it can sign only as that node. The CA key sits in its own namespace (`c4-signing`) that only the manager can read; the root certificate is published in the trust bundle (`c4-provenance`, `ca.crt`). The policy checks who signed each kind of record, so a node cannot sign for another node or for the queue. A Materialization can name its source's record, which the controller verifies (and refuses from another project's workload); the queue links an upstream job's record into its downstream inputs; the HUD writes each control record onto the job in the same write as the change. `GET /api/v1/custody/{ns}/{name}?path=` and the MCP tool `c4_verify_custody` walk from a file to its ingest across every signer.

Tested in unit tests and envtest: identities and the signer policy, tampered bundles, a foreign root, an expired certificate, issuer refusals and legacy farm-key records (`internal/provenance`); signed resolutions and four kinds of refused source record against a real API server (`internal/controller`); decision and job records (`internal/work/queue`); a custody walk across six signers, a control record made through the MCP client header, and a forged job record caught by the policy (`internal/hud/server`). **Not yet run on the cluster:** `hack/e2e/p7.sh` is rewritten for the signers (signed ingest, a held denoise released through MCP, the custody walk, the issuer's refusals, the CA's reach), but the kind cluster was stopped for a power emergency before it could run. Until it runs, the node daemon's certificate round trip and the deployment manifests (`config/signing`) are checked only by unit tests and `kubectl kustomize`.

Gaps: `signedAt` is the signer's own claim (a timestamp authority or a private Rekor would make it independent); revocation is by short certificate lifetimes only; people act through the HUD rather than signing with their own keys; C2PA manifests are emitted as definitions for c2patool, not signed (a "Trusted" C2PA credential needs a certificate from the C2PA Conformance Program); the disclosure report and the forward trace are designed, not built.

## Comparison with Scion

Scion's numbers come from one source: Anthropic's post [Redesigning source control](https://www.anthropic.com/engineering/redesigning-source-control) (published 2026-10-13, updated 2026-10-15), its text and its six figures, read in full. The only other material from Anthropic is 25 replies on [Hacker News](https://news.ycombinator.com/item?id=50073633) by `nickg-anthropic`, which matches the post's byline (HN does not verify identity). No Scion talks, slides or papers were found as of 2026-10-16, and `docs/research/ecosystem.md` has nothing on Scion. Every Scion latency below is a production median of a client command from the post's Figure 6 ("Observed performance improvements on native Scion repos"), against Anthropic's own git setup.

**No number here can be set against Scion's as a speedup or a ratio.** Only CI checkout is the same operation in both, and it is only partly comparable.

| metric        | Scion (published)                                                                                                       | c4-operator (kind, one laptop)                                                                                                                                         | comparable                              | why                                                                                                                                                                                                                                                          |
| ------------- | ----------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| store fan-in  | each tier keeps a cache hit rate "of over 99%"; no request counts (post, "Scaling Scion")                               | 1.50 store GETs per object at 64 pods (1.86 at 8, 1.64 at 32); 8.00 with node caches only; 64 with per-pod copies                                                      | no                                      | Their figure is a steady-state hit rate on caches seeded from snapshots; ours is a cold herd on new 8 MiB objects, where the regional tier itself answered only 7 to 25% of its requests, by design                                                         |
| cold mount    | none published; daemons start "from a periodically refreshed snapshot"                                                  | 52 files, 384 MiB: 0.82 s and 1.00 s for the earliest node in each zone, 63 to 81 ms for nodes whose zone peers held the bytes; P4 CI parent commit 1.7 s per node        | no                                      | Scion daemons never start empty; our median is mostly a peer copy at memory speed                                                                                                                                                                            |
| warm mount    | developer "checkout" 1.1 s (git 15 s), cache state not stated                                                           | 2 ms                                                                                                                                                                   | no                                      | We mount a layer the node already built; `scn` updates a working copy                                                                                                                                                                                        |
| CI checkout   | 3.5 s per CI job (git 31 s); 22 s at the start of a compute job (git 175 s)                                             | 16 shards on 4 nodes, empty caches: 1.7 s median per pod, after 0.50 s of view resolution; git clone and c4git checkout of the same commit on the host's SSD: 1.1 s    | partly                                  | The same step, but our tree is over 170 times smaller in bytes, our links run at memory speed, we measured cold only, and theirs may be sparse or virtual. Our git baseline is a local clone; theirs is a mirror fleet                                       |
| next commit   | "pull latest" 3.5 s (developer), 9.0 s (agent)                                                                          | a 3-file delta layer in 16 ms; a rerun in 2 ms                                                                                                                         | no                                      | One known 3-file commit against everything merged since the last pull (about 21 merges a minute at 30,000 a day)                                                                                                                                            |
| commit, import | "commit" 1.1 s (developer), 761 ms (agent); ingestion only relative: the rewrite used "20x less memory" and was "4x faster" | `git push` to a stored view 173 ms at p50 (hook), 232 ms (webhook); import 7.5 ms per commit at p50 into MinIO                                                       | partly (end state only); no (import)    | Both end with the change durable on the server side, but ours starts after `git commit` and pushes to a bare remote on the same SSD                                                                                                                         |
| diff          | developer "diff" 67 ms (git 407 ms); which diff and how large is not stated                                             | 13, 34 and 221 ms for commits 1, 10 and 100 apart (3, 24 and 248 files), server compute only, identical to `git diff --name-status`                                   | no                                      | Their operation is unstated; our timer excludes the HTTP round trip                                                                                                                                                                                          |
| repository    | main-branch history "almost 100 GB", a compressed working copy "over 25 GB", all text                                   | 2,219 files, 136 MiB at head, 401 commits; over 90% of the bytes are binary c4git assets, about 9.4 MiB of text in all history                                       | no                                      | Our tree is over 170 times smaller in bytes and our text history about 10,000 times smaller                                                                                                                                                                 |
| load          | "on the order of 25 million code checkouts per day for CI" (about 290 a second); over 30,000 merges a day                | 8 workers, at most 64 pods at once, 16 shards per change, 3 CI runs                                                                                                    | no                                      | Orders of magnitude apart                                                                                                                                                                                                                                    |

Scion also reports infrastructure "200x smaller than our previous git mirroring infrastructure in terms of CPU and memory" and "a 25% CPU capacity win" for CI; we measured neither. Its text says checkouts became "two orders of magnitude faster", while Figure 6 shows 8.8 times for CI checkout and 8.0 times for compute jobs; only an agent's "new worktree" (7.1 min to 5.1 s, 84 times) is near a hundredfold. The figure's numbers are the ones used here.

**What is not comparable.** Our numbers come from kind on one Apple M3 Max: nine Docker containers and three MinIO shards sharing 16 CPUs, 47 GB and one VM disk, plain HTTP between tiers, every link at memory speed, a synthetic repository, and 8 or 16 samples per cell from one run. Scion's are medians of production traffic. The confounders:

- Network: we had no latency, no bandwidth limit and no TLS, so every fetch-bound number of ours is a floor a real network will not reach.
- Object sizes: our P1 and P2 objects are 8 MiB frames and P4's bytes are mostly 1 to 4 MB assets; Scion's repository is size-capped text, so request overhead dominates for them and bytes dominate for us.
- Repository size and history: 136 MiB and 401 commits hide any cost that grows with tree width, directory fan-out or history. Our own import already shows fan-out matters (1.1 ms against 8.4 ms per commit by layout).
- Concurrency: at most 64 pods, against about 290 CI checkouts a second before agents and developers.
- Cache state: we wipe every cache and P2 uses new content each run; Scion runs warm and seeds nodes from snapshots.
- Hardware: one laptop against an unspecified fleet on Spanner and cloud storage.
- What is timed: our mount timer runs from the CSI publish call to the overlay mount and excludes view resolution and pod start; our diff timer covers server compute only. Scion states no timer bounds, cache-state mix, sparse share or measurement window, and no percentile but the median.
- Baselines: their speedups are against their own git mirror fleet; our 1.1 s git baseline is a local clone with no network.

What our results do show is mechanism: store load stays flat as pods grow, cost follows the change, and delta layers work. They show nothing yet about performance at Scion's scale.

**What a fair benchmark needs.**

- Workload: a real code repository with deep history (or a generator fitted to one) with text under a hard size cap, reporting the file-size histogram, file count, directory fan-out and history depth; a separate media track of 8 to 100 MiB frames in the same harness; three sizes spanning 100 times (for example 0.25, 2.5 and 25 GB working copies); commits replayed at a stated merge rate with sizes sampled from the real history; 16, 64 and 256 shards per change with several changes in flight, as synchronized herds and as a steady Poisson stream.
- Environment: 64 or more nodes in three zones and two regions, each tier on its own machines, the store in S3 or GCS in the same region, TLS between tiers, link latency and bandwidth measured before each run, and baselines in the same environment: git with a mirror per zone (partial clone and sparse checkout), git from one central remote, c4-operator Direct, and c4-operator tiered.
- Timers with stated bounds: mount ready (from the CSI call, and separately from pod creation, until the tree is listable), read set done (until the shard has read its last byte, which keeps lazy and eager checkouts honest), CI end to end (from push accepted until the last shard's tree is verified), update(k) for k = 1, 10, 100 and 1,000 commits, push to visible in another zone, and diff(k) at the client with server compute reported separately.
- Per tier: requests in, requests sent upstream, bytes in and out, hit rate as 1 - upstream/in, store GETs per unique object per herd, CPU-seconds and peak memory per 1,000 checkouts, and store requests and egress at list price.
- Statistics and controls: p50, p90, p99, max and sample count with bootstrap intervals and 1,000 or more samples per cell; named cache states reached by a scripted reset and confirmed from tier metrics; identical content and commit sequence for every system, each sample checking its tree ID; randomized, interleaved run order with five or more repetitions; pinned versions and no other tenants.

Sources, accessed 2026-10-16: the post and its Figure 6 ([image](https://www-cdn.anthropic.com/images/4zrzovbb/website/c706fa6efb4b9e5cd2566cb080acbfce7218042d-3840x4684.png)); HN replies [50079191](https://news.ycombinator.com/item?id=50079191) (the 600 PB figure is the 25 GB copy times the job count), [50079663](https://news.ycombinator.com/item?id=50079663) (a low file size cap; "This is really all just text"), [50078883](https://news.ycombinator.com/item?id=50078883) (every test shard checks out the code), [50079857](https://news.ycombinator.com/item?id=50079857) and [50076230](https://news.ycombinator.com/item?id=50076230) (ingestion). Our timers are defined in `internal/node/publish.go` (`Publish`) and `internal/hud/server/detail.go` (`handleDiff`).

## Designed, not built

- Lazy lower layers through FUSE (DESIGN.md section 4.6).
- Encryption with just-in-time keys from a project KMS (section 6.6).
- The signed, hash-chained usage ledger and marketplace exporters (section 12). Usage is metered in memory and shown in the HUD (cluster-hours, container-hours, GPU-hours); bytes avoided is shown as savings, never billed.
- The licensing broker for per-hour software licenses (section 16.2), cloud provider adapters beyond kind-sim (Cluster Autoscaler ProvisioningRequest, Karpenter), the Deadline bridge (section 14), DCC and production-tracking integrations (section 15), and a Helm chart (kustomize only today).
- Pre-warming new nodes for a capacity request, the chain service for live capture streams, and re-basing long output chains.
- For signing (DESIGN.md section 9.2): an independent timestamp (an RFC 3161 authority or a private Rekor), certificates for people from the studio's OIDC provider so artists and agents sign submissions and approvals with their own keys, and a farm-level audit store for actions that belong to no project (node disable).

Two follow-ups Joshua asked for were not started when the open-source release became the priority: a `NEXT.md` giving each item above its purpose, the need it serves, its design reference, what done means and how it is proven, and its order; and HUD views for each persona the design names (producer, wrangler, farm admin, security, finance, vendor, ISV and the others), built where the HUD API already has the data.

## Engineering notes

- Tests: the controllers and the queue run against a real API server (envtest); overlay, layer and capture tests run privileged in a Linux container (`make test-linux`); tier tests pass under `-race`; `go vet ./...` and `go test ./...` are clean at the merge of `v2-followup`, built against the pinned c4 pseudo-version with no `replace`.
- The MVP's weaknesses are gone: no hostPath for pods, no cluster-wide Secret read (credentials are Secrets in `c4-system`, read through a namespaced Role), node daemons may change only their own pods' output annotations (a ValidatingAdmissionPolicy), and tiers authenticate every request.
- Found and fixed along the way: yields were lost when the queue restarted (now kept in status), records were written for refused descriptions (now only after the preflight), and the view record size of directories holding symlinks.
- P1 to P3 ran on earlier builds of the branch; P4 to P7 on one build (83ca7f2). The symlink fix between them changes the view IDs of trees with symlinks in subdirectories and no measured quantity.
- Once, after the P7 run, the Docker Desktop VM stalled (load average above 500, API server unreachable, VM disk at 91%); restarting Docker Desktop brought the kind cluster back intact. Keep e2e data modest on the shared Docker disk.
- Changes proposed for c4, c4d, c4git and c4-containers are listed in `PROGRESS.md`; none of those repositories was modified.

## Decisions for Joshua

1. The release path: c4 is pinned to a master pseudo-version until a c4 release carries what the operator uses (the table below); the proposed image path is `ghcr.io/avalanche-io/c4-operator`, and `RELEASING.md` lists the steps that are Joshua's, including how the history is published.
2. The billing model: cluster-hours with container-hours, GPU-hours, or fixed production windows, and prices.
3. Transport security between cache tiers (mTLS through a mesh, or the tiers' own certificates).
4. Whether Direct mode (node daemons holding store credentials) stays a supported mode.
5. Signing key custody. Built and recommended: each entity signs as itself with a short-lived certificate from a farm signing CA, issued against its Kubernetes service-account token (Sigstore's keyless model, self-hosted, so nothing goes to a public log). Open: where the root key lives in production (a KMS or HSM signing an intermediate the manager holds), whether to run a private Sigstore (Fulcio and Rekor) for independent timestamps and an append-only log, certificates for people from the studio's OIDC provider, and whether to pursue C2PA conformance for trusted credentials.
6. The HUD's authorization model: per-write checks with an audit Event (as built), or impersonation.
7. Which proposed upstream changes (c4 library APIs, c4d range reads, the c4git smudge truncation) go into the next c4 suite release; the table below says which are planned.

### The release decision: c4 dependencies

None of the needed c4 changes is in a release: v1.0.18 lacks all three the operator uses, and 1.0.19 is a patch release with no new exported API. So `go.mod` now requires the c4 master pseudo-version `v1.0.17-0.20261016134402-db4c7bec2376` (commit db4c7be on `origin/master`, which contains every c4 commit named below) instead of `replace => ../c4`, and the operator builds and passes its tests from a clean checkout. Two caveats: it must be re-pinned after the v1.1 re-scope, because packages may move; and the pseudo-version sorts below v1.0.18, so a build that also required c4 v1.0.18 would select v1.0.18 and fail. A pre-release tag on c4 master (for example `v1.1.0-alpha.1`) would end both. The status of every c4-side item the operator needs or found:

| item                                                                                                     | repo           | status                                                       | evidence                                                                                          |
| -------------------------------------------------------------------------------------------------------- | -------------- | ------------------------------------------------------------ | ------------------------------------------------------------------------------------------------- |
| `c4m.IsJournal`                                                                                          | c4             | on master, in no tag                                         | fc7ca48; `git tag --contains` is empty                                                            |
| `scan.ModeContent`                                                                                       | c4             | on master, in no tag                                         | 31f3c20 (used by one test)                                                                        |
| the parser accepting a closing checkpoint line, which ends every changeset the operator writes          | c4             | on master, in no tag                                         | 6c2b82d, CHANGELOG.md "Unreleased"; on v1.0.18 three operator tests fail ("empty patch section") |
| `c4m.ResolveBase`                                                                                        | c4             | on master, in no tag; v2 no longer calls it                  | 72c79cf, external bases in 5f5a5d2                                                                |
| `c4 patch` no longer writes directories                                                                  | c4             | on master, in no tag                                         | f15c1c2, CHANGELOG.md                                                                             |
| record expansion as a library                                                                            | c4             | not planned                                                  | `cmd/c4/cat.go` (package main); a finding only in `design/ash-brief.md`:152-153                   |
| opening a store from a URL                                                                               | c4             | not planned                                                  | `store/config.go`:70 is unexported                                                                |
| S3 ranged reads, and a `Has` that reports errors                                                         | c4             | not planned                                                  | `store/s3store.go`:118-158                                                                        |
| S3 virtual-hosted addressing with a custom endpoint                                                      | c4             | not planned                                                  | `store/s3store.go`:83-103                                                                         |
| a separator between the S3 prefix and the ID                                                            | c4             | not planned; the store-layout draft keeps prefix + ID        | `design/store-layout-finish-results.json`:116                                                     |
| a hardlink mode for reconcile                                                                            | c4             | not planned (v1.1 plans clonefile only)                      | `design/ash-brief.md`:39-40                                                                       |
| a scan projection that keeps the executable bit                                                          | c4             | not planned; decision D1 left it open                        | `plans/handoff-2026-07/03-DESIGN-DECISIONS.md`:9-18                                               |
| `c4 log` resolving an external base reference                                                            | c4             | not planned; base fetching is given to cat, patch, restore   | `design/chain-resolution.md`:79-82                                                                |
| k8s examples that run `c4 patch <c4m> <dir>`                                                             | c4-containers  | planned for v1.1                                             | `design/ash-brief.md`:149-151                                                                     |
| five more `patch`-on-a-directory uses (`render-pipeline.yaml`:94, three compose files, `scan-bucket.sh`) | c4-containers  | not planned                                                  | found by this audit                                                                               |
| the README names `S3_ENDPOINT`                                                                           | c4-containers  | planned ("fits 1.0.19") but missing from the 1.0.19 list     | `design/ash-brief.md`:215-216 against :74-98                                                      |
| `Range` on `GET /{id}`                                                                                   | c4d            | not planned (an archived design only)                        | `design/archive/HTTP_API.md`:410-420                                                              |
| project or partition authorization of content reads                                                      | c4d            | not planned (namespace RBAC flagged, subtree ACLs designed)  | `dev-plans/open-source-release/02-DESIGN.md`:239-243                                              |
| whole objects held in memory on peer misses                                                              | c4d            | not planned                                                  | `internal/server/content.go`:60-75                                                                |
| `checkout` ignores depth                                                                                 | c4d            | not planned; the unmerged open-source branch deletes it      | f74c26c                                                                                           |
| a release path for c4d                                                                                   | c4d            | none: c4d is not a suite component (last tag v0.1.0)         | `RELEASE.md`:31-42                                                                                |
| smudge writes only 256 bytes of a non-pointer blob                                                       | c4git          | not planned; a plain fix that fits 1.0.19                    | `filter/filter.go`:53-65                                                                          |
| the v1.1 suite release from master                                                                       | c4, c4toolkit  | planned, on hold for a re-scope                              | `design/ash-brief.md`:25-29, 105-149                                                              |
| porting the v1.0.17 and v1.0.18 fixes to master                                                          | c4             | planned                                                      | `design/ash-brief.md`:138-143                                                                     |

To add to the plan: (1) in the v1.1 re-scope, keep `c4m.IsJournal` exported (or name its replacement), keep `scan.ModeContent` and the 6c2b82d parsing, and correct the operator's list in `design/ash-brief.md`:145-149, which names `ResolveBase` (unused by v2) and omits the parser change; (2) extend c4-containers' move from `patch` to `restore` to the five other uses; (3) put the c4git smudge fix and the `S3_ENDPOINT` README fix into the 1.0.19 content list; (4) schedule or reject the not-planned c4 items for v1.1 (each adds API or changes CLI output, so none fits a patch release); (5) give c4d a release path, or record that the operator keeps its own workarounds (it does today).

## Where things are

| what                                          | where                                                                                 |
| --------------------------------------------- | ------------------------------------------------------------------------------------- |
| design                                        | `DESIGN.md`                                                                           |
| measured results                              | `hack/e2e/out/results.md`, `hack/e2e/monorepo/RESULTS.md`, `hack/sim/out/results.md`  |
| phase scripts                                 | `hack/e2e/p1.sh` to `p7.sh` (after `hack/e2e/up.sh`)                                  |
| HUD screenshots                               | `docs/hud/live`, `docs/hud`                                                           |
| agent docs                                    | `docs/agents/tools.md`, `docs/agents/eval.xml`, `skills/c4-farm/SKILL.md`, `llms.txt` |
| research briefs                               | `docs/research/provenance.md`, `docs/research/ecosystem.md`                           |
| branches, blockers, proposals, open questions | `PROGRESS.md`                                                                         |
| install, quickstart, release steps            | `docs/install.md`, `docs/quickstart.md`, `RELEASING.md`                               |
