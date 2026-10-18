# Releasing c4-operator

A release is a pushed tag `vX.Y.Z`. The `release` workflow then runs
GoReleaser (`.goreleaser.yaml`): the CLI for Linux and macOS (amd64 and
arm64), the image `ghcr.io/avalanche-io/c4-operator:vX.Y.Z` for linux/amd64
and linux/arm64, `install.yaml`, checksums, and a draft GitHub release to
review and publish by hand. `make release-snapshot` runs the same locally
and publishes nothing.

## v0.1.0 checklist

Agents prepare the repository; they do not create repositories, push, or
change anything on GitHub. Everything marked **Joshua** is his to do.

### Before publishing

1. **Joshua: approve how the history is published** (see
   [History](#history)). Recommended: option B.
2. **Joshua: approve the image path** `ghcr.io/avalanche-io/c4-operator`. A
   different path means editing `config/release/kustomization.yaml`,
   `.goreleaser.yaml`, `Makefile` (`REGISTRY`) and the docs.
3. **Joshua: confirm the license and contacts**: Apache-2.0, "Copyright 2026
   Joshua Ian Kolden" (as in c4 and c4git), and `c4dev@avalanche.io` for
   security reports and conduct (as in c4).
4. Agent: validate `docs/quickstart.md` on a fresh kind cluster with locally
   built images, run `hack/e2e/p7.sh` (provenance with a signer per entity),
   and `make release-snapshot` (builds the images; publishes nothing).

### On GitHub

5. **Joshua: create the repository** `github.com/Avalanche-io/c4-operator`,
   private, empty (no README, license or .gitignore), default branch `main`.
6. **Joshua: push `main`** from the prepared clone (history option B below,
   or `git push origin v2:main` for option A).
7. **Joshua: settings.**
   - Security: enable private vulnerability reporting (SECURITY.md points
     to it).
   - Actions: allow GitHub Actions; under Workflow permissions, allow
     workflows to write packages (the release workflow pushes to GHCR with
     `GITHUB_TOKEN`).
   - Branches: protect `main`, requiring the `ci` checks.
8. Wait for the `ci` workflow on `main` to pass: verify, unit and envtest,
   privileged Linux tests, build.

### The release

9. **Joshua: tag and push.**
   ```sh
   git tag -a v0.1.0 -m "c4-operator v0.1.0"
   git push origin v0.1.0
   ```
   The `release` workflow builds and pushes the images and drafts the
   release.
10. **Joshua: make the GHCR package public**: Packages, `c4-operator`,
    Package settings, Change visibility, Public; and connect it to the
    repository if GitHub has not.
11. **Joshua: review and publish the draft release** (notes, `install.yaml`,
    archives, checksums).
12. **Joshua: flip the repository public**: Settings, General, Change
    visibility.
13. Smoke test from a clean machine with the published images: the
    quickstart, and `kubectl apply -f
    https://github.com/Avalanche-io/c4-operator/releases/download/v0.1.0/install.yaml`.
14. **Joshua, optional:** the repository description and topics (kubernetes,
    operator, csi, vfx, render-farm, content-addressed-storage, provenance,
    mcp), the cccc.io link, and pinning it on his profile.

## History

The history was audited on 2026-10-16 (60 commits, branches `mvp` and `v2`;
`mvp` is an ancestor of `v2`):

- gitleaks over every commit and the working tree: no secrets. The MinIO
  credentials in `hack/e2e` and `config/samples/quickstart` are for local
  kind clusters only.
- No personal paths, machine names, network addresses, Sphere references or
  client names. Studios named in the docs are public references (Netflix's
  published guidance for partners, Sony's C2PA cameras, Pixar's render queue
  as a UI reference).
- No large files: the largest blob is 1.4 MiB; the packed repository is
  26 MiB.
- Every commit is by "Joshua Kolden <joshua@avalanche.io>".
- One internal working note from the development sessions,
  `HANDOFF-v2.md`, is in the history from its introduction (be24446) to
  157703c. It was not written for publication. The public history was made
  with option B on 2026-10-16, so the note is in no published commit.

Options:

- **A. Publish as is.** The handoff note stays readable in the history.
- **B. Drop the note from the history (recommended).** In a fresh clone,
  so the local repository keeps everything:
  ```sh
  git clone --no-local --branch v2 /path/to/c4-operator /tmp/c4-operator-publish
  cd /tmp/c4-operator-publish
  git branch -m v2 main
  git filter-repo --invert-paths --path HANDOFF-v2.md --refs main
  ```
  Commits from be24446 on get new IDs. Three IDs that the docs cite
  (`hack/e2e/out/results.md`, `REPORT-v2.md`: 1f0a287, afd7741, 46ea151) are
  then updated from `.git/filter-repo/commit-map` in one more commit. The
  story of the work, 60 one-line commits, is kept.
- **C. One squashed commit.** The simplest public history; the commit
  history is lost, and the docs' commit IDs must be reworded.
