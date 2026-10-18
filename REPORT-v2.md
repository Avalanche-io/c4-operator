# c4-operator v2: report

This report covers the v2 redesign of c4-operator: what was built, what each phase proved in the end-to-end suite, what is designed and comes next, and the decisions that are Joshua's. The design is in [`DESIGN.md`](DESIGN.md); every number below comes from [`hack/e2e/out/results.md`](hack/e2e/out/results.md), [`hack/e2e/monorepo/RESULTS.md`](hack/e2e/monorepo/RESULTS.md) or [`hack/sim/out/results.md`](hack/sim/out/results.md), each reproducible by the script named there.

Method: an eight-worker, two-zone Kubernetes test cluster with a three-shard object store; each phase script builds its scenario, measures it and appends the results.

## Summary

c4-operator reimagines the render queue around content identity, deadlines, provenance and agents. It gives each pod a C4 view of exactly the files it needs, mounted through a CSI driver as an overlay of shared, hardlinked layers, filled through node, peer and regional caches that read each object from the store about once however many pods need it. What a pod writes is captured as a changeset when it exits, signed in a task record, and folded into its job's output view. A deadline-driven queue runs render, CI and ML work on the same farm without priorities; a HUD shows the farm from live cluster state; an MCP server gives agents every HUD capability as the calling user.

Headline results:

- **Store load stays flat as pods grow.** 64 pods reading a 28-object shot cost the store 42 GETs through the tiers (1.5 per object), 224 with node-level caching only, and 1,792 with per-pod copies.
- **Deadlines without priorities.** On a contended farm, 6 of 7 jobs met their deadlines while a training run yielded its GPU slots at a checkpoint and resumed from its captured view, and burst capacity arrived within the provider's delay. In a five-week simulation, ordering by deadline missed about 44% fewer render deadlines than manual priorities, as plain earliest-deadline-first does too; the policy adds a shorter CI tail, and with burst capacity it missed 52% fewer than manual for about $487 a week.
- **Cost follows the change.** The next commit of a monorepo mounts as a 3-file delta layer in 16 ms at the median across 16 CI shards, and which files changed between two commits is answered from records alone, identical to `git diff`.
- **Every step vouched for by whoever did it.** Nodes, the controller, the queue, the HUD and importers each sign their own records, and a chain of custody runs from any output file back to its ingest.

## Phases

| phase                  | status                       | evidence                                                                              |
| ---------------------- | ---------------------------- | ------------------------------------------------------------------------------------- |
| P1 views, capture, CSI | done                         | `hack/e2e/p1.sh`                                                                      |
| P2 tiers               | done                         | `hack/e2e/p2.sh`                                                                      |
| P3 isolation           | done                         | `hack/e2e/p3.sh`, `internal/controller` envtest                                       |
| P4 monorepo CI         | done                         | `hack/e2e/p4.sh`, `hack/e2e/monorepo/run.sh`                                          |
| P5 work system and HUD | done, next steps below       | `hack/e2e/p5.sh`, `go run ./hack/sim`, `internal/work/queue` envtest, `docs/hud/live` |
| P6 agent interface     | done, next steps below       | `hack/e2e/p6.sh`, `internal/mcpserver` tests                                          |
| P7 provenance          | done, next steps below       | `hack/e2e/p7.sh`, `internal/provenance` tests                                         |

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

Store GETs are counted by the object store itself. Wall time for 64 pods: tiered 11.2 s, Direct 10.3 s, per-pod copies 29.6 s; what the tiers take off the store is requests, egress and pressure on per-prefix request limits. Killing the busiest regional replica with SIGKILL while it streamed: 32 of 32 pods finished, 7 transfers resumed from their offsets at the other home, no node fell through to the store, and store GETs stayed at 1.67 per object.

### P3: isolation

Every attempt by project B to read project A's data was refused: B's MinIO user against A's bucket (403), a Materialization naming A's view by ID (unresolvable from B's store), a hand-written c4m listing A's object IDs (preflight: every object missing), the regional tier and a node's peer port without a token (401) and with the pod's own token (403), and a CSI volume context claiming A's pod identity with a forged token (resolved in B's own namespace from the verified token). Strict projects get separate copies of identical bytes on a shared node; Shared projects share one copy, and a Shared project still cannot mount content its own store lacks (the preflight is the proof of possession). A vendor project with one grant sees exactly the granted shot; removing the grant fails its Materialization 0.1 s later, new pods cannot mount it, and pods already running keep their mount until they end. Records of a refused description are never written to the store.

### P4: monorepo CI

The importer (`import-git`) turns git's first-parent history into views, changesets and one chain per branch, copying c4git-managed assets by ID without rehashing them. On the synthetic monorepo (2,219 files, 401 commits, 62 c4git assets):

- Import into MinIO: 401 commits in 5.4 s, 7.5 ms per commit at p50. Cost follows the change: 1.1 ms per commit for 8,555 files grouped by team, 8.4 ms with 600 services in one directory (local stores, `hack/e2e/monorepo/RESULTS.md`).
- Push-to-view (local stores): 173 ms at p50 from `git push` to a stored view through a post-receive hook, 232 ms through a webhook listener.
- CI with 16 shards per change on four nodes: the parent commit mounts cold in 1.7 s per node, the next commit as a 3-file delta layer in 16 ms, a rerun in 2 ms. A git clone and c4git checkout of the same commit from a local remote takes 1.1 s.
- Which files changed between two commits, from directory records alone: 13 ms for one commit, 34 ms for 10, 221 ms for 100, each identical to `git diff --name-status`.
- `c4 log` on a branch chain lists exactly the commits' views. Getting there found a bug in the view records (a directory holding a symlink was recorded with size 0 instead of null), fixed in `internal/view`.

The one difference from Scion: Scion made source control server-side; this mirrors git into C4 views, push by push, and leaves git where it is.

### P5: the work system and the HUD

The queue (`internal/work/queue`) runs the scheduling policy (`internal/work/sched`, a pure function) every two seconds over WorkJobs, Pools, Projects and its unit pods. Slots are an extended resource on pool nodes, so kube-scheduler places units as the queue counts them.

On the test farm (16 cpu slots, 8 GPU slots, up to 8 burst slots on spot nodes that a simulated capacity provider brings up after a 20 s delay), seven jobs from two shows:

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
| objective (this policy) | 23.8                             | 61.8       | 15.4         |
| objective with burst    | 20.8                             | 50.5       | 15.4         |

Against EDF on the same weeks the policy matches deadline performance and cuts the CI tail (p95 17.2 to 15.4 minutes, CI runs over 15 minutes from 9.1% to 5.6%); with burst it misses 3.6 fewer deadlines a week on average and has fewer late hours on 4 of 5 weeks, for about $487 a week. With half the GPUs, burst cuts late hours from 382 (EDF) to 53. Binary order roughly halves the error of the remaining-time estimate when a shot has a heavy region (whole-render error 0.24 against 0.46 at 3x) and flags the region at a median 15% of the render instead of 46%. It costs makespan when heavy frames land in the last wave (median 5% longer at 3x, 12% at 8x), and W90 under-covers heavy shots (83% at 3x, 68% at 8x, against a target of 90%). The simulation also showed that the allocation formula as first written (`R = W90 / T`) misses more deadlines than EDF; the built policy reserves room for the last wave (`R = W90 / (T - t_path)`).

The HUD (`internal/hud`) is served by the manager from live cluster state: queue, job view with the binary fill and the cost curve, workers, cache tiers, dependencies, cost and capacity, an artist's scoped view, and controls (hold, boost, deadline, requeue, node avoidance, node disable, burst approval), each checked against the user's Kubernetes permissions and recorded as an Event. Screenshots: `docs/hud/live` (the end-to-end farm) and `docs/hud` (a synthetic farm, `hack/hudmock`).

Next: generators (dynamic expansion), which run in the policy and the simulator, move into the queue's classes; a comp job starts on a render's partial output chain (today downstream units start when the upstream job succeeds, through `fromJob`); ML gangs larger than a node run through Kueue and JobSet (designed); the ML lens adds steps, once workloads report them.

### P6: the agent interface

`c4-operator mcp` serves 18 tools (17 in the P6 run, plus `c4_verify_custody` since) over stdio and stateless Streamable HTTP, on the official Go SDK (MCP 2026-07-28). Each tool is a call to the HUD API as the caller, so an agent sees and changes exactly what its user could. Read tools are annotated read-only; requeue is destructive; approving burst capacity is open-world. Errors say what to do next. The tool reference (`docs/agents/tools.md`) is generated from the tool list; the `c4-farm` skill (`skills/c4-farm`) and `llms.txt` point agents at it. On the live farm, an MCP client answered all 10 questions of the evaluation set (`docs/agents/eval.xml`), each call in 0 to 9 ms including the HUD round trip, and a hold and release made through MCP were recorded as Events naming the user.

Next: a recorded session of an LLM agent driving the farm (the evaluation is answered by a reference client today); tools for the HUD's byte and stream endpoints (preview frames, objects, events); the designed explain, migrate, provider and vendor tools.

### P7: provenance

At every capture the node daemon writes a task record: an in-toto Statement (v1) with the predicate `https://cccc.io/provenance/task/v1`, signed in a DSSE envelope, stored as a C4 object and indexed per job. A record names the job, unit, attempt, pod, node, container images by digest, the input views, the output view and changeset, every changed file by C4 ID (and the SHA-512 it encodes), and the job's declared AI use. In the end-to-end suite, with the records signed by one farm key:

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

Tested in unit tests and envtest: identities and the signer policy, tampered bundles, a foreign root, an expired certificate, issuer refusals and legacy farm-key records (`internal/provenance`); signed resolutions and four kinds of refused source record against a real API server (`internal/controller`); decision and job records (`internal/work/queue`); a custody walk across six signers, a control record made through the MCP client header, and a forged job record caught by the policy (`internal/hud/server`). The end-to-end scenario for the signers, `hack/e2e/p7.sh` (a signed ingest, a held denoise released through MCP, the custody walk, the issuer's refusals, the reach of the signing CA), is written and runs next; until then the node daemon's certificate round trip and the deployment manifests (`config/signing`) are covered by the tests above and by rendering the manifests.

Next: an independent signing time (a timestamp authority or a private Rekor; `signedAt` is the signer's own claim today); revocation lists in addition to short certificate lifetimes; people signing with their own keys, not only through the HUD; signed C2PA manifests (they are emitted as definitions for c2patool today, and a "Trusted" C2PA credential needs a certificate from the C2PA Conformance Program); the disclosure report and the forward trace.

## Comparison with Scion

Anthropic's Scion ([Redesigning source control](https://www.anthropic.com/engineering/redesigning-source-control), 2026-10-13, updated 2026-10-15) and C4 share an architecture: content-addressed trees fetched top-down by immutable ID, one daemon per node shared by every job on it, strict cache tiers that answer the same requests, sparse checkouts, and commits whose cost follows the change (DESIGN.md, "Prior art: Scion"). Scion's figures are the medians in the post's Figure 6 and the numbers in its text; C4's are from the end-to-end suite and the monorepo harness.

| operation        | Scion (published)                                                                 | C4                                                                                                                                                       |
| ---------------- | --------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| store fan-in     | each tier keeps a cache hit rate "of over 99%"                                    | a new 28-object shot read by 64 pods: 1.50 store GETs per object (1.86 at 8 pods, 1.64 at 32), against 8.00 with node caches only and 64 with per-pod copies |
| cold mount       | daemons start "from a periodically refreshed snapshot"                           | 52 files, 384 MiB: 80 ms at the median, served by peers in the same zone; 0.82 s and 1.00 s for the earliest node in each zone                          |
| warm checkout    | developer "checkout" 1.1 s (git 15 s)                                             | 2 ms: the node's existing layer, mounted for the next pod                                                                                                |
| CI checkout      | 3.5 s per CI job (git 31 s); 22 s at the start of a compute job (git 175 s)       | 1.7 s per node at the median, cold, for a 2,219-file, 136 MiB commit, 16 test shards on 4 nodes                                                          |
| next commit      | "pull latest" 3.5 s (developer), 9.0 s (agent)                                    | the next commit as a 3-file delta layer in 16 ms; a rerun in 2 ms                                                                                        |
| commit to server | "commit" 1.1 s (developer), 761 ms (agent)                                        | `git push` to a stored view in 173 ms at p50 through a post-receive hook, 232 ms through a webhook; 7.5 ms per commit to import                           |
| diff             | developer "diff" 67 ms (git 407 ms)                                               | 13, 34 and 221 ms for commits 1, 10 and 100 apart (3, 24 and 248 files), from directory records, identical to `git diff --name-status`                   |
| a cache fails    | the strict tiers keep a regional failure from spreading                           | a regional replica killed mid-transfer: 32 of 32 pods finished, 7 transfers resumed from their offsets at the other home, none reached the store         |

Scion is source control for one code monorepo, a text repository with a low cap on file size (as described in the post's [Hacker News thread](https://news.ycombinator.com/item?id=50079663)). C4 applies the same structure to a whole production, and adds:

- **Media as well as code.** The same views carry plates, renders and caches whose objects run from kilobytes to gigabytes, and the git importer brings c4git-managed binary assets in by ID without rehashing them.
- **Outputs as versions.** Every pod's writes are captured as a changeset over the view it mounted, so render, CI and training outputs get the same IDs, diffs and chains as source.
- **A peer tier.** Nodes in a zone serve each other before the regional tier is asked.
- **Isolation by project.** Each project has its own stores and credentials; vendors get grants on a tree and a path prefix; shared caches require proof of possession; a revoked grant takes effect on the next mount.
- **Git stays.** Scion replaced git on the server; C4 mirrors git history into views push by push, and developers keep their tools.
- **Scheduling.** One deadline-driven queue runs render, CI and ML on the same data, with estimates that converge early through binary frame order, and training runs that yield at a checkpoint and resume.
- **Provenance and agents.** Each entity signs its own records, a chain of custody runs from any frame to its sources, and an MCP server gives agents every HUD capability as the calling user.

Sources, accessed 2026-10-16: the post and its Figure 6 ([image](https://www-cdn.anthropic.com/images/4zrzovbb/website/c706fa6efb4b9e5cd2566cb080acbfce7218042d-3840x4684.png)) and the author's replies on [Hacker News](https://news.ycombinator.com/item?id=50073633). C4's timers are defined in `internal/node/publish.go` (`Publish`) and `internal/hud/server/detail.go` (`handleDiff`).

## Designed and next

- Lazy lower layers through FUSE (DESIGN.md section 4.6).
- Encryption with just-in-time keys from a project KMS (section 6.6).
- The signed, hash-chained usage ledger and marketplace exporters (section 12). Usage is metered in memory and shown in the HUD (cluster-hours, container-hours, GPU-hours); bytes avoided is shown as savings, never billed.
- The licensing broker for per-hour software licenses (section 16.2), cloud provider adapters beyond the simulated provider (Cluster Autoscaler ProvisioningRequest, Karpenter), the Deadline bridge (section 14), DCC and production-tracking integrations (section 15), and a Helm chart (kustomize only today).
- Pre-warming new nodes for a capacity request, the chain service for live capture streams, and re-basing long output chains.
- For signing (DESIGN.md section 9.2): an independent timestamp (an RFC 3161 authority or a private Rekor), certificates for people from the studio's OIDC provider so artists and agents sign submissions and approvals with their own keys, and a farm-level audit store for actions that belong to no project (node disable).

Also next: a `NEXT.md` giving each item above its purpose, the need it serves, its design reference, what done means and how it is proven, and its order; and HUD views for each persona the design names (producer, wrangler, farm admin, security, finance, vendor, ISV and the others), built where the HUD API already has the data.

## Engineering notes

- Tests: the controllers and the queue run against a real API server (envtest); overlay, layer and capture tests run privileged in a Linux container (`make test-linux`); tier tests pass under `-race`; `go vet ./...` and `go test ./...` are clean at the merge of `v2-followup`, built against the pinned c4 pseudo-version with no `replace`.
- The MVP's weaknesses are gone: no hostPath for pods, no cluster-wide Secret read (credentials are Secrets in `c4-system`, read through a namespaced Role), node daemons may change only their own pods' output annotations (a ValidatingAdmissionPolicy), and tiers authenticate every request.
- Found and fixed along the way: yields were lost when the queue restarted (now kept in status), records were written for refused descriptions (now only after the preflight), and the view record size of directories holding symlinks.
- P1 to P3 ran on earlier builds of the branch; P4 to P7 on one build (83ca7f2). The symlink fix between them changes the view IDs of trees with symlinks in subdirectories and no measured quantity.
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
