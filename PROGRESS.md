# PROGRESS

## Branches

| Branch             | Opened     | Closed     | Purpose                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| ------------------ | ---------- | ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `main`             | 2026-10-15 |            | Not present in this repository; `RELEASING.md` publishes `v2` as `main`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `mvp`              | 2026-10-15 |            | Working MVP: CRD, controller, node agent, webhook, envtest, single-node kind e2e. Not merged.                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `v2`               | 2026-10-15 |            | The v2 redesign (DESIGN.md): views and records, tiers, overlay and CSI, isolation, work system, provenance.                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `v2-sim`           | 2026-10-16 | 2026-10-16 | Scheduling policy core (`internal/work`) and the simulation harness (`hack/sim`). Rebased onto `v2` and fast-forwarded; worktree removed.                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `v2-hud`           | 2026-10-16 | 2026-10-16 | The HUD frontend (`internal/hud/web`), its API contract and the mock backend (`hack/hudmock`). Rebased onto `v2` and fast-forwarded; worktree removed.                                                                                                                                                                                                                                                                                                                                                                                                       |
| `v2-gitimport`     | 2026-10-16 | 2026-10-16 | The c4git-aware importer (`internal/gitimport`, `import-git`), push-driven import, per-branch chains, the synthetic monorepo harness (`hack/e2e/monorepo`). Rebased onto `v2` and fast-forwarded; worktree removed.                                                                                                                                                                                                                                                                                                                                          |
| `v2-followup`      | 2026-10-16 | 2026-10-16 | The c4 release audit and the c4 pin (no `replace`), a signer per entity for provenance, the Scion comparison, the name question closed. `NEXT.md` and the persona views were deferred for the open-source release. Fast-forwarded into `v2`; worktree removed.                                                                                                                                                                                                                                                                                               |
| `release-v0.1`     | 2026-10-16 | 2026-10-16 | Open-source release readiness for v0.1: the public-repo audit, the install path (images, GoReleaser, install.yaml, CI), the quickstart, the README, the community files and `RELEASING.md`. Fast-forwarded into `v2`; worktree removed.                                                                                                                                                                                                                                                                                                                      |
| `docs-positioning` | 2026-10-16 | 2026-10-16 | Public docs lead with the project's scope and present results plainly: development-machine context removed, results under "Results (end-to-end suite)", the Scion comparison operation by operation with what C4 adds. Fast-forwarded into `main`; worktree removed.                                                                                                                                                                                                                                                                                         |
| `hud-graticule`    | 2026-10-16 | 2026-10-17 | The HUD redesigned as GRATICULE: a design system (`internal/hud/web/design`: tokens, components, previews), the job view as a shot's sheet (STACK with containment by adjacency and collapse to aggregate), the farm sheet, pools, the map, the wall, the side widget, the phone; API additions (work in slot-seconds, shot, expansion levels, cross-row reads, the book, sites, unit ETAs) in the contract, the backend, the mock and MCP. Fast-forwarded into `main`; worktree removed.                                                                    |
| `scenarios`        | 2026-10-16 | 2026-10-17 | The scenario ladder: one farm and scenario spec format (`examples/farms`), farms driven by the production policy (`sched.Decide`) through an event-driven engine in `hack/hudmock` (nodes indexed by pool and free slots, burst, spot reclaims, approvals, cache-tier fan-in from what units read), five farms from a home lab to a global facility, named moments with actions through the HUD's controls, and `make tutorials` writing fixtures, screenshots and a walkthrough per farm to `docs/tutorials`. Fast-forwarded into `main`; worktree removed. |
| `scenarios-2`      | 2026-10-17 | 2026-10-17 | sched fixes the scenarios exposed: jobs waiting on upstream work rank after every job that can run, and a job the plan makes late is priced at its lateness under its planned share (no more $0 lateness, so auto-burst works); hack/sim rerun. Modern pipeline examples throughout (USD exports instead of RIB, Hydra renderers to OpenEXR, Nuke, ACES ProRes and DNxHR, gen-AI inference); design fixtures (`hudmock fixtures`), docs/hud and docs/tutorials regenerated. Fast-forwarded into `main`; worktree removed.                                    |
| `sched-stress` | 2026-10-17 | | The scheduler stress grid (`hack/stress`): a Latin hypercube over farm scale, load, deadlines, durations, DAG shape, quotas, arrivals, burst economics, faults, nudges and training gangs, plus corners; invariant and metamorphic checks on every decision; FIFO, EDF, fair-share and static-priority baselines; a report with heatmaps (`make stress`, `make stress-quick`, reference report in `hack/stress/reference/`). The sched fixes it led to, one commit each with a regression test: blocked and held jobs rank last, a late job is never priced at $0, plan capacity respects borrowing limits, a job no pool can run is at risk, preemption no longer trades units back and forth, the starvation guard lets the deadline tier go first for jobs with slack (`StarvationDeferMax`), nominal quota and guaranteed demand are served before borrowing, and a burst can carry a whole gang. AWS, GCP and Azure spot models and a completion-aware spot policy (`hack/stress/spot.go`, `spot.md`). |

Worktrees live under `.worktrees/` (ignored by git). `go.mod` requires a published
c4 pseudo-version, so worktrees need nothing else; to build against a local c4
checkout, use an uncommitted `go.work` (`go work init . ../c4`).

The repository is public at
[github.com/Avalanche-io/c4-operator](https://github.com/Avalanche-io/c4-operator);
`main` is the published branch. The release workflow pushes images to
`ghcr.io/avalanche-io/c4-operator` when a tag is pushed.

## Pending end-to-end runs

1. Run `hack/e2e/p7.sh`, rewritten for a signer per entity: a signed ingest,
   a source record from another project refused, a held denoise released
   through MCP, the custody walk across node, controller, queue, HUD and
   workload signers, the issuer's refusals, and the CA's reach. It needs
   `hack/e2e/redeploy.sh` first (new manifests: `config/signing`, the node
   daemon's `c4-signer` token, the manager's issuer port).
2. Validate `docs/quickstart.md` on a fresh kind cluster with locally built
   images, then delete that cluster.
3. `make release-snapshot` and `make images`: the release images, built
   locally and published nowhere.

## Open questions for Joshua

1. **Release path.** c4 is pinned to the master pseudo-version
   `v1.0.17-0.20261016134402-db4c7bec2376` (db4c7be) until a c4 release carries
   what the operator uses; expect to re-pin after the v1.1 re-scope, because
   packages may move. See "c4 dependencies and the release plan" below. The
   image registry path is also open (blocker 3).
2. **Billing model.** Usage is metered as cluster-hours, container-hours and
   GPU-hours (the HUD shows them; a signed ledger is designed, not built), and
   DESIGN.md section 12 allows fixed production windows instead. Which to offer,
   and at what prices, is a business decision.
3. **Transport security between tiers.** Tier requests are authenticated with
   projected service-account tokens (TokenReview) but travel as plain HTTP
   inside the cluster. mTLS through a mesh, or the tiers' own certificates, is
   a deployment choice to make before a multi-tenant install.
4. **Direct mode.** In Direct mode node daemons hold a project's store
   credentials. It is the baseline P2 measures against and suits small
   installs; keeping it as a supported mode is a choice.
5. **Signing key custody.** Built: each entity signs its own records with a
   short-lived certificate from a farm signing CA (DESIGN.md section 9.2); the
   CA key is a Secret in `c4-signing` that only the manager reads. Open: a KMS
   or HSM for the root in production, a private Sigstore (Fulcio and Rekor) for
   independent timestamps and an append-only log, certificates for people from
   the studio's OIDC provider, and C2PA conformance: credentials that
   validators show as "Trusted" need a certificate on the C2PA Trust List; the
   farm emits manifest definitions, not signed C2PA manifests.
6. **HUD authorization model.** Controls are checked per write
   (SubjectAccessReview) and made by the HUD's service account, with an Event
   naming the user. Impersonation would let RBAC check the write itself but
   needs the `impersonate` verb. The current choice is reversible.
7. **Proposed upstream changes.** The c4, c4d, c4-containers and c4git changes
   below were found while building; none was made. Which to take into the next
   c4 suite release is Joshua's call.

## Release blockers

1. **The `replace` is gone; the c4 pin is a pseudo-version.** `go.mod` requires
   `github.com/Avalanche-io/c4 v1.0.17-0.20261016134402-db4c7bec2376` (c4 master
   at db4c7be, public), which has every API the operator uses; v1.0.18 does not.
   Re-pin after the v1.1 re-scope. The pseudo-version sorts below v1.0.18, so a
   build that also requires c4 v1.0.18 would select it and fail; a pre-release
   tag on c4 master would end that.

2. **Images.** The Dockerfiles only copy a cross-compiled binary, so
   multi-arch images need no emulation: `make images` (buildx, linux/amd64 and
   linux/arm64; an OCI archive unless `PUSH=1`) and GoReleaser
   (`Dockerfile.release`, run by the release workflow on a tag). Neither has
   built an image yet, because Docker is stopped; `make release-snapshot` will
   check both.

3. **Image name.** Proposed: `ghcr.io/avalanche-io/c4-operator`
   (`config/release`, `.goreleaser.yaml`). Joshua's decision.

4. **History.** Published with option B of `RELEASING.md` (2026-10-16): the
   internal working note was dropped from the history with `git filter-repo`,
   and the commit IDs the docs cite were updated to the rewritten ones.

## c4 dependencies and the release plan

Audited 2026-10-16 from the c4 suite's plans (`../../RELEASE.md`,
`../ROADMAP.md`, `../../PLAN.md`, `../c4/design/ash-brief.md` and the other
design notes, `../c4-design`, `../c4toolkit`, the c4d dev plans) and the git
history, branches and tags of c4, c4d, c4git, c4-containers and c4toolkit.
Paths are relative to the repository named in each row.

None of the needed c4 changes is in a release: v1.0.18 lacks all three the operator uses, and 1.0.19 is a patch release with no new exported API. So `go.mod` now requires the c4 master pseudo-version `v1.0.17-0.20261016134402-db4c7bec2376` (commit db4c7be on `origin/master`, which contains every c4 commit named below) instead of `replace => ../c4`, and the operator builds and passes its tests from a clean checkout. Two caveats: it must be re-pinned after the v1.1 re-scope, because packages may move; and the pseudo-version sorts below v1.0.18, so a build that also required c4 v1.0.18 would select v1.0.18 and fail. A pre-release tag on c4 master (for example `v1.1.0-alpha.1`) would end both. The status of every c4-side item the operator needs or found:

| item                                                                                                     | repo          | status                                                      | evidence                                                                                         |
| -------------------------------------------------------------------------------------------------------- | ------------- | ----------------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| `c4m.IsJournal`                                                                                          | c4            | on master, in no tag                                        | fc7ca48; `git tag --contains` is empty                                                           |
| `scan.ModeContent`                                                                                       | c4            | on master, in no tag                                        | 31f3c20 (used by one test)                                                                       |
| the parser accepting a closing checkpoint line, which ends every changeset the operator writes           | c4            | on master, in no tag                                        | 6c2b82d, CHANGELOG.md "Unreleased"; on v1.0.18 three operator tests fail ("empty patch section") |
| `c4m.ResolveBase`                                                                                        | c4            | on master, in no tag; v2 no longer calls it                 | 72c79cf, external bases in 5f5a5d2                                                               |
| `c4 patch` no longer writes directories                                                                  | c4            | on master, in no tag                                        | f15c1c2, CHANGELOG.md                                                                            |
| record expansion as a library                                                                            | c4            | not planned                                                 | `cmd/c4/cat.go` (package main); a finding only in `design/ash-brief.md`:152-153                  |
| opening a store from a URL                                                                               | c4            | not planned                                                 | `store/config.go`:70 is unexported                                                               |
| S3 ranged reads, and a `Has` that reports errors                                                         | c4            | not planned                                                 | `store/s3store.go`:118-158                                                                       |
| S3 virtual-hosted addressing with a custom endpoint                                                      | c4            | not planned                                                 | `store/s3store.go`:83-103                                                                        |
| a separator between the S3 prefix and the ID                                                             | c4            | not planned; the store-layout draft keeps prefix + ID       | `design/store-layout-finish-results.json`:116                                                    |
| a hardlink mode for reconcile                                                                            | c4            | not planned (v1.1 plans clonefile only)                     | `design/ash-brief.md`:39-40                                                                      |
| a scan projection that keeps the executable bit                                                          | c4            | not planned; decision D1 left it open                       | `plans/handoff-2026-07/03-DESIGN-DECISIONS.md`:9-18                                              |
| `c4 log` resolving an external base reference                                                            | c4            | not planned; base fetching is given to cat, patch, restore  | `design/chain-resolution.md`:79-82                                                               |
| k8s examples that run `c4 patch <c4m> <dir>`                                                             | c4-containers | planned for v1.1                                            | `design/ash-brief.md`:149-151                                                                    |
| five more `patch`-on-a-directory uses (`render-pipeline.yaml`:94, three compose files, `scan-bucket.sh`) | c4-containers | not planned                                                 | found by this audit                                                                              |
| the README names `S3_ENDPOINT`                                                                           | c4-containers | planned ("fits 1.0.19") but missing from the 1.0.19 list    | `design/ash-brief.md`:215-216 against :74-98                                                     |
| `Range` on `GET /{id}`                                                                                   | c4d           | not planned (an archived design only)                       | `design/archive/HTTP_API.md`:410-420                                                             |
| project or partition authorization of content reads                                                      | c4d           | not planned (namespace RBAC flagged, subtree ACLs designed) | `dev-plans/open-source-release/02-DESIGN.md`:239-243                                             |
| whole objects held in memory on peer misses                                                              | c4d           | not planned                                                 | `internal/server/content.go`:60-75                                                               |
| `checkout` ignores depth                                                                                 | c4d           | not planned; the unmerged open-source branch deletes it     | f74c26c                                                                                          |
| a release path for c4d                                                                                   | c4d           | none: c4d is not a suite component (last tag v0.1.0)        | `RELEASE.md`:31-42                                                                               |
| smudge writes only 256 bytes of a non-pointer blob                                                       | c4git         | not planned; a plain fix that fits 1.0.19                   | `filter/filter.go`:53-65                                                                         |
| the v1.1 suite release from master                                                                       | c4, c4toolkit | planned, on hold for a re-scope                             | `design/ash-brief.md`:25-29, 105-149                                                             |
| porting the v1.0.17 and v1.0.18 fixes to master                                                          | c4            | planned                                                     | `design/ash-brief.md`:138-143                                                                    |

To add to the plan: (1) in the v1.1 re-scope, keep `c4m.IsJournal` exported (or name its replacement), keep `scan.ModeContent` and the 6c2b82d parsing, and correct the operator's list in `design/ash-brief.md`:145-149, which names `ResolveBase` (unused by v2) and omits the parser change; (2) extend c4-containers' move from `patch` to `restore` to the five other uses; (3) put the c4git smudge fix and the `S3_ENDPOINT` README fix into the 1.0.19 content list; (4) schedule or reject the not-planned c4 items for v1.1 (each adds API or changes CLI output, so none fits a patch release); (5) give c4d a release path, or record that the operator keeps its own workarounds (it does today).

## Changes proposed for oss/c4, oss/c4d, oss/c4-containers (not made)

Those repos were read, not modified.

### c4

1. **Record expansion is not importable.** `expandRecursive`, `expandIfRecord` and
   `storeDescend` live in `cmd/c4` (package `main`). The operator implements its
   own (`internal/view`), generalized so a directory known only by ID is expanded
   only when the include filter can reach it. A library `c4m.Expand(m, src,
   mayContain)` would let `c4 restore` do sparse restores too.
2. **Store opening by URL is not importable.** `store.openS3Configured` is
   unexported and reads credentials only from the environment.
3. **S3 reads cannot resume, and `Has` hides errors.** `S3Store.Open` sends no
   `Range`, and `S3Store.Has` returns false on network errors, so "absent" and
   "unreachable" read the same. The operator signs its own ranged GET and HEAD
   (`internal/storeurl/sigv4.go`, checked against the AWS SigV4 example) and uses
   `S3Store.Create` only for multipart uploads. A ranged `OpenAt(id, offset)` and a
   `Head(id) (size, error)` in the library would remove that.
4. **S3 addressing.** With a custom endpoint, `S3Store` always uses path-style
   URLs. CoreWeave AI Object Storage requires virtual-hosted addressing.
5. **S3 prefix concatenation.** `S3Store.objectKey` is `prefix + id` with no
   separator, so `s3://datasets/c4` writes keys like `c4c4...`. The e2e uses a
   trailing slash.
6. **`reconcile` has no link mode.** A hardlink option (copy fallback on `EXDEV`)
   would let `c4 restore` materialize from a local store the way the node does.
7. **The content projection drops the executable bit.** Views record mode 0755 on
   executable files and null elsewhere, so a tree without executables has the same
   ID as `c4 id` in content mode. A scan mode with that projection in the c4
   scanner would let the CLI compute view IDs directly.

8. **`c4 log` on a chain with an external base.** `c4 log` treats the first
   section as the base state and computes version IDs from the chain text, so
   on a chain whose first line is an external base reference its IDs are those
   of the patches alone, not of the versions. With the base written out in full
   (the operator's chain export) it lists exactly the checkpoints. Resolving a
   first-line external base from the configured store, when there is one,
   would make `c4 log` list the true version IDs of any stored chain.

### c4d

1. **No `Range` support** on `GET /{id}`, so an interrupted 10 GB transfer from a
   c4d shard restarts at zero (the operator detects the 200 and restarts cleanly).
2. **No notion of a project.** The content plane authorizes any mTLS peer to read
   any ID; a cache tier needs a partition header and a per-request decision.
3. **Whole-object buffering on peer misses.** `handleGetContent` tees a peer
   fetch into a `bytes.Buffer` before caching it: a 10 GB object needs 10 GB of
   RAM. Streaming to a temporary file fixes it.
4. **`checkout` ignores depth** (from reading `checkout.go`; not executed): nested
   files land flat in the target directory.

### c4git

1. **The smudge filter truncates non-pointer blobs to 256 bytes.** c4git
   1.0.18 reads 256 bytes to look for an ID and, when the blob is not a
   pointer, writes only what it read. A file committed before its path became
   c4git-managed is therefore truncated in a c4git checkout. Found by the
   monorepo importer (`hack/e2e/monorepo/RESULTS.md`); the importer records
   the blob's real content. Copying the rest of the stream after the probe
   fixes it.

### c4-containers

1. The k8s examples run `c4 patch <c4m> <dir>`, which on c4 `master` is text
   algebra; the tree writer is `c4 restore`.
2. The README names `S3_ENDPOINT`; the store reads `C4_S3_ENDPOINT` or the
   `endpoint=` URL query.

## Status

See `REPORT-v2.md` for what each phase proved and what is open.
