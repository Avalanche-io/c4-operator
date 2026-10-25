# Releasing c4-operator

A release is a pushed tag `vX.Y.Z`. The `release` workflow then tests,
builds the job images on a native runner per architecture and joins them
into multi-arch tags, runs the prebuilt tools' own tests against those
images (`make tool-tests`; a tool that fails stops the release), writes each
prebuilt tool's `tool.lock` pinning those images by digest (`make
tool-locks`), and runs GoReleaser (`.goreleaser.yaml`): the CLI for Linux
and macOS (amd64 and arm64), with the locked tools embedded in `c4-farm`,
the image `ghcr.io/avalanche-io/c4-operator:vX.Y.Z` for linux/amd64 and
linux/arm64, `install.yaml`, checksums, the Homebrew formulas (generated
for reference, not pushed), and a draft GitHub release to review and
publish by hand. Last it builds the sample set with the release's images
and attaches it. If that step fails, the `samples` workflow (Actions,
samples, Run workflow, with the tag) builds the tag's tree with main's
build script and replaces the set's assets, so it can run again safely.
`make release-snapshot` runs GoReleaser locally and publishes nothing; its
`c4-farm` has no tool locks, so its farm pins the images when it publishes.

## Upgrade notes

What a farm running an earlier build must do, or will notice, on upgrade.

- **Units have no network unless a tool declares one.** The queue now
  writes `c4-units-deny-egress` in every namespace with units: every unit
  pod, egress, no rule. Jobs that name no tool lose all network, DNS
  included: a plain `POST /jobs` job, MCP's raw image form, a tracker's job
  and any unit not naming a tool in `reasoning.tools`. Such a job says "this
  job names no tool, so it has no network: name a tool that declares one".
  Whatever a unit must reach goes in a tool's `needs.network`, as a network
  the farm admin defines in `c4-networks` (internal/hud/API.md, Networks).
- **Re-apply the egress proxy's manifest**, `config/egress/egress.yaml`
  (part of `install.yaml`): its ClusterRole now lists nodes and
  `servicecidrs`, which the proxy reads to refuse the cluster's own
  addresses. Without it the proxy keeps only its fixed list.
- **Apply the CRDs.** A WorkJob's `spec.tool` now carries the version's
  tree (`view`) and cannot change after the job is made.
- **Tool jobs made before the upgrade** that claim a network name no
  version, so their units not yet released are held ("names no version of
  it"). Submit them again through the tool (`c4 farm submit`).

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
4. Agent: follow `docs/quickstart.md` word for word with `c4 farm up` on a
   machine with only Docker (`--image-archive` for the locally built
   operator image), run `hack/e2e/p7.sh` (provenance with a signer per entity),
   and `make release-snapshot` (builds the images; publishes nothing).

### On GitHub

5. The tap is `github.com/mrjoshuak/homebrew-tap` (`mrjoshuak/tap`), which
   exists already, until Avalanche-io is part of the service offering. The
   C4 lead runs the Homebrew release from its own machine: it writes
   `Formula/c4-farm.rb` and `Formula/c4-agent.rb` from the release's
   checksums (c4toolkit `scripts/farm-formulas.sh`) and the suite's
   `Formula/c4.rb`, which depends on both, so `brew install
   mrjoshuak/tap/c4` brings c4, `c4 farm`, c4-agent and c4-unit. The
   release pushes nothing to the tap and needs no token for it; GoReleaser
   generates the two formulas into `dist/homebrew` only, as a reference.
6. Done already: `github.com/Avalanche-io/c4-operator` is public, with
   `main` pushed.
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
10. **Joshua: check the GHCR packages are public**: Packages, `c4-operator`,
    each `c4-job-*` and each `c4-runtime-*`. GHCR gives a new package its
    own visibility, private unless the organization's default is public,
    even when the repository is public; set any private one Public under
    Package settings, Change visibility, and connect each to the repository
    if GitHub has not. This is needed once, for the first release.
11. **Joshua: review and publish the draft release** (notes, `install.yaml`,
    archives, checksums).
12. **The C4 lead: the tap.** Once the draft is published, it writes
    `c4-farm.rb` and `c4-agent.rb` to the tap from the release's checksums,
    then the suite's `c4.rb`; `brew install` works from then on (the
    repository is public already).
13. Smoke test from a clean machine with the published images: `brew
    install mrjoshuak/tap/c4-farm`, then `c4-farm` (help, exit 0),
    `c4-agent help` and `c4-unit` (its usage, exit 2); once the C4 suite
    release has written `c4.rb`, `brew install mrjoshuak/tap/c4` and
    `c4 farm agents`; the quickstart; and `kubectl apply -f
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
