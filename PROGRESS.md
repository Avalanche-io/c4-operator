# PROGRESS

## Branches

| Branch | Opened     | Closed | Purpose                                                                                     |
| ------ | ---------- | ------ | ------------------------------------------------------------------------------------------- |
| `main` | 2026-10-15 |        | Empty until the MVP is reviewed.                                                            |
| `mvp`  | 2026-10-15 |        | First working version: CRD, controller, node agent, webhook, envtest, kind e2e. Not merged. |

No worktrees are in use. If one is needed, it goes under `.worktrees/`.

Nothing has been pushed anywhere. There is no remote. No image has been pushed to
any registry; `c4-operator:dev` exists only in the local Docker daemon and the
local kind cluster.

## Release blockers

1. **Remove the `replace` in go.mod before release.**
   `go.mod` has `replace github.com/Avalanche-io/c4 => ../c4`. It builds here and
   nowhere else; a public CI runner has no `../c4`. This is the mistake recorded
   in `../../RELEASE.md` ("Lessons from v1.0.13", item 1).

   It cannot simply be dropped today. Building against the published
   `github.com/Avalanche-io/c4 v1.0.18` fails, because the operator uses three APIs
   that exist on c4 `master` but in no release:

   - `c4m.IsJournal`
   - `c4m.ResolveBase`
   - `scan.ModeContent`

   Order of operations: ship the next c4 suite release (through `oss/c4toolkit`),
   bump the `require` to that version, delete the `replace`, and run
   `go build ./... && make test` with `GOFLAGS=-mod=mod` from a clean checkout.

2. **Dockerfile builds from a host binary.** Because of the `replace`, `make image`
   cross-compiles on the host and copies the binary in. Once the `replace` is gone,
   switch to a multi-stage build that compiles inside the image.

3. **Image name.** Manifests use `c4-operator:dev`. A release needs a registry path
   (for example `ghcr.io/avalanche-io/c4-operator`), which is Joshua's decision.

## Changes proposed for oss/c4, oss/c4d, oss/c4-containers (not made)

Those repos were read, not modified. These are the places where the operator had
to work around something, or noticed something while reading.

1. **Record expansion is not importable.** `expandRecursive`, `expandIfRecord`, and
   `storeDescend` live in `cmd/c4` (package `main`). The operator re-implements
   them in `internal/manifest/resolve.go`, generalized so a directory known only by
   ID is expanded only when the include filter can reach it. A
   `c4m.Expand(m, src, mayContain func(dir string) bool)` in the library would
   remove the copy, and give `c4 restore` sparse restores for free.
2. **Store opening by URL is not importable.** `store.openS3Configured` is
   unexported and reads credentials only from the environment. The operator copies
   its URL parsing (`internal/storeurl`) so object keys match exactly. An exported
   `store.Open(uri string, creds ...)` would remove the copy.
3. **S3 prefix concatenation.** `S3Store.objectKey` is `prefix + id`, with no
   separator. `s3://datasets/c4` (the form in `c4-containers/k8s/*.yaml`) writes
   keys like `c4c4...`. That works, but it is surprising, and changing it now would
   orphan existing stores. At minimum it should be documented; the e2e uses a
   trailing slash (`s3://c4-store/show/`).
4. **`S3Store.Has` returns false on network errors.** Absent and unreachable read
   the same. The controller therefore reports an unreachable store as "content
   missing". A `Has(id) (bool, error)` variant would let it say which.
5. **`reconcile` has no link mode.** `Apply` copies from a `LocalSource` path. A
   hardlink option (with copy fallback on `EXDEV`) would let `c4 restore`
   materialize from a local store the way this operator does.
6. **c4-containers k8s examples will break on the next c4 release.** The examples
   `materializer-job.yaml` and `render-pipeline.yaml` run `c4 patch <c4m> <dir>`.
   That works with the released 1.0.18. On c4 `master`, `patch` is text algebra
   that "never touches directories" (ARCHITECTURAL_CATALOG.md), and the tree
   writer is `c4 restore`. This is from reading the catalog, not from running
   master's CLI.
7. **c4-containers README names `S3_ENDPOINT`.** The c4 store reads
   `C4_S3_ENDPOINT` or the `endpoint=` URL query, not `S3_ENDPOINT`.
8. **c4d `checkout` ignores depth.** `checkout.go` writes each entry to
   `filepath.Join(targetDir, entry.Name)`. c4m names are bare names with a depth,
   so nested files land flat in the target directory. This is from reading the
   code; it was not executed.

## Design decisions worth a second look

- **Views live inside the cache volume.** `link(2)` returns `EXDEV` across two bind
  mounts even when both are the same filesystem, so an init container cannot
  hardlink from a hostPath cache into a pod's `emptyDir`. The agent builds each view
  once per node under `/var/lib/c4/views/<viewID>`, and pods mount that directory
  read-only. One tree serves every pod on the node that wants the same view.
- **`viewID` is the content-level C4 ID of the tree.** The ID is computed from the
  description with c4's own directory identity (content projection: names, sizes,
  and IDs real; mode and timestamp null). A pod can scan its mount and compare, so
  "the pod sees the exact tree" is a recomputation, not an assertion. The e2e does
  exactly that, and also scans hardlinked subsets on the host.
- **The filesystem is the cache index.** An object's link count is its reference
  count, its atime is its LRU position, and its mtime is a fixed sentinel (c4m's
  null timestamp) that any write disturbs. The node has no database to drift.
- **Rebuilt trees are retired, not deleted.** Running pods bind-mount the old tree.
  It is removed only when no pod on the node uses that view.
- **Pods for a Materialization with missing content are admitted.** The init
  container's preflight fails before any byte is fetched, and the workload never
  starts. A pod that restarts on failure picks the content up once it is uploaded.
  Denying at admission is the alternative; it gives faster feedback but no
  self-healing.

## Status

See README "What it is not yet" for the scope boundary.
