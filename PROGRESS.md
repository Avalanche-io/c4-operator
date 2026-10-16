# PROGRESS

## Branches

| Branch          | Opened     | Closed | Purpose                                                                                      |
| --------------- | ---------- | ------ | -------------------------------------------------------------------------------------------- |
| `main`          | 2026-10-15 |        | Empty until the work is reviewed.                                                            |
| `mvp`           | 2026-10-15 |        | Working MVP: CRD, controller, node agent, webhook, envtest, single-node kind e2e. Not merged. |
| `v2`            | 2026-10-15 |        | The v2 redesign (DESIGN.md): views and records, tiers, overlay and CSI, isolation, work system, provenance. |
| `v2-sim`        | 2026-10-16 |        | Worktree `.worktrees/sim`: scheduling policy core and the simulation harness. Merges into `v2`. |
| `v2-hud`        | 2026-10-16 |        | Worktree `.worktrees/hud`: the HUD frontend and its mock backend. Merges into `v2`.         |
| `v2-gitimport`  | 2026-10-16 |        | Worktree `.worktrees/gitimport`: the c4git-aware importer (P4). Merges into `v2`.           |

Worktrees live under `.worktrees/` (ignored by git). `.worktrees/c4` is a symlink
to `../c4` so the `replace => ../c4` in each worktree's `go.mod` resolves.

Nothing has been pushed anywhere. There is no remote. No image has been pushed to
any registry; `c4-operator:dev` and `c4-e2e:dev` exist only in the local Docker
daemon and the local kind cluster.

## Open questions for Joshua

1. **Name collisions before any launch.** C4 stands for the Cinema Content
   Creation Cloud (cccc.io). Before a public launch, check collisions:
   Cinema 4D (commonly "C4D") in the same industry, the C4 model for software
   architecture diagrams, and existing trademarks. The module path and the repo
   name are unchanged; those decisions are Joshua's.

## Release blockers

1. **Remove the `replace` in go.mod before release.**
   `go.mod` has `replace github.com/Avalanche-io/c4 => ../c4`. It builds here and
   nowhere else; a public CI runner has no `../c4`. This is the mistake recorded
   in `../../RELEASE.md` ("Lessons from v1.0.13", item 1).

   It cannot simply be dropped today. Building against the published
   `github.com/Avalanche-io/c4 v1.0.18` fails, because the operator uses APIs that
   exist on c4 `master` but in no release (for example `c4m.IsJournal`).

   Order of operations: ship the next c4 suite release (through `oss/c4toolkit`),
   bump the `require` to that version, delete the `replace`, and run
   `go build ./... && make test` with `GOFLAGS=-mod=mod` from a clean checkout.

2. **Dockerfile builds from a host binary.** Because of the `replace`, `make image`
   cross-compiles on the host and copies the binary in. Once the `replace` is gone,
   switch to a multi-stage build that compiles inside the image.

3. **Image name.** Manifests use `c4-operator:dev`. A release needs a registry path
   (for example `ghcr.io/avalanche-io/c4-operator`), which is Joshua's decision.

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

### c4-containers

1. The k8s examples run `c4 patch <c4m> <dir>`, which on c4 `master` is text
   algebra; the tree writer is `c4 restore`.
2. The README names `S3_ENDPOINT`; the store reads `C4_S3_ENDPOINT` or the
   `endpoint=` URL query.

## Status

See `REPORT-v2.md` for what each phase proved and what is open.
