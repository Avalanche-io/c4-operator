# import-git on a synthetic monorepo

Measured 2026-10-16 on branch `v2-gitimport`.

Machine: Apple M3 Max (16 cores, 128 GB), macOS 27.2, internal SSD (APFS).
Tools: git 2.55.0, go 1.26.7, c4git 1.0.18 (`go install
github.com/Avalanche-io/c4git/cmd/c4git@latest`). Store: three c4 TreeStore
shards on the local SSD, given as three `-store` flags, so objects spread by
rendezvous hashing exactly as they would over three S3 buckets. No network
store was used.

## Commands

Everything below comes from one script:

```
hack/e2e/monorepo/run.sh        # OUT=/tmp/c4-monorepo-e2e by default
```

It runs these steps:

```
GOBIN=$PWD/bin go install github.com/Avalanche-io/c4git/cmd/c4git@latest
go build -o bin/c4-operator ./cmd/c4-operator
go build -o bin/monorepo ./hack/e2e/monorepo
go build -o bin/measure ./hack/e2e/monorepo/measure
go build -o bin/pushlat ./hack/e2e/monorepo/pushlat
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

bin/monorepo -out $OUT/repo -c4git bin/c4git -seed 1 -services 150 -commits 400
bin/c4-operator import-git -repo $OUT/repo \
    -store $OUT/shards/a -store $OUT/shards/b -store $OUT/shards/c -out $OUT/map.jsonl
# sampled checkouts: git worktree add (c4git smudge), then c4-operator tree-id
bin/pushlat -repo $OUT/repo -out $OUT/pushlat -bin bin -small 30 -assets 10 -asset-mb 3
bin/c4-operator import-git -repo $OUT/repo -store ... -out $OUT/map-again.jsonl -quiet
bin/measure -repo $OUT/repo -runs 3
bin/c4-operator import-git -repo $OUT/repo -rev HEAD~100 -store ... -out $OUT/map-inc.jsonl -quiet
bin/c4-operator import-git -repo $OUT/repo -store ... -out $OUT/map-inc.jsonl -quiet
bin/c4-operator import-git -repo $OUT/repo -max-commits 50 -store ... -quiet
bin/monorepo -out $OUT/repo-large -c4git bin/c4git -seed 1 -services 600 -commits 400 \
    -asset-min-mb 1 -asset-max-mb 1
bin/monorepo -out $OUT/repo-teams -c4git bin/c4git -seed 1 -services 600 -teams 24 \
    -commits 400 -asset-min-mb 1 -asset-max-mb 1
```

`bin/measure` runs the same importer into a sink that stores nothing. That
isolates the importer's own cost (git, hashing, records) from the store's
write latency. `bin/pushlat` measures push-to-view latency (below).

## The repository

`hack/e2e/monorepo` builds it deterministically from the seed. Commit dates
are fixed, so two runs with seed 1 produce the same head,
`73c83663560010da8c47617e966c22030a3afcfb`.

- `services/svc-NNN-<word>/`: Go-like sources and tests in `internal/<pkg>/`,
  `main.go`, `BUILD`, `README.md`. About a quarter of the services have
  `testdata/plate_N.exr` assets of 1 to 4 MB of incompressible bytes,
  committed through the real c4git clean filter (`c4git init`, then
  `*.exr filter=c4`).
- `libs/common/`: 30 packages and a config file.
- `tools/`: six executable shell scripts and a symlink (`tools/b ->
  build.sh`); a service has a symlink into `libs/common/config/`.
- History after the initial commit: 400 commits. 78% change 1 to 4 files in
  one service (services picked with a skew, so a few are hot), with
  occasional adds, deletes and renames; 12% change the shared library; 4%
  change a tool script; 3.5% regenerate or add an asset; the rest add or
  remove a whole service.

| repository         | tracked files at head | .git   | c4git store |
| ------------------ | --------------------- | ------ | ----------- |
| `repo` (150 svcs)  | 2,218                 | 4.2 MB | 167 MB      |
| `repo-large` (600) | 8,555                 |        |             |
| `repo-teams` (600) | 8,555                 |        |             |

A changed commit touched 2 files at p50, 4 at p90, and 19 at most.

## Correctness on this repository

For every 40th commit and the head (11 commits), a `git worktree add`
checkout smudged by c4git, scanned by `c4-operator tree-id`, has exactly the
view ID the import recorded. Continuing an import (301 commits, then the
remaining 100 from the mapping file) ends at the same head view as one
full import:
`c45q8vJ413Lw8xuabUkon9XiG9jmskZVRRmyp3eG3DCsaxfaCKA7Q6fMcbtMk9wkoGv1rp2HemY9xFqayMiYD3mq6t`.
Every pushed commit in the latency run below resolved through
`view.FromID` on its changeset to its view, with the parent's view as the
chain base.

## Push-to-view latency

`bin/pushlat` clones the repository, then makes 40 commits in random order:
30 small ones (1 to 3 Go files of one service) and 10 that add a 3 MB c4git
asset. It pushes each commit to three bare remotes:

- **plain**: no hook. This is what the push alone costs.
- **hook**: a self-hosted remote whose `post-receive` hook runs
  `c4-operator import-git -post-receive`. It imports the ref's commits
  after the last one the mapping records for that ref.
- **webhook**: a remote whose `post-receive` hook POSTs `{"ref", "after"}`
  with curl to `c4-operator import-git -listen`. The listener runs `git
  fetch` into its mirror, then imports. This is a git host webhook without
  the host's delivery delay.

git waits for `post-receive`, so the push returns once the import is done.
"View stored" is the time from starting `git push` until the commit's
mapping line exists and its root record and changeset are in the store.
The developer's clone and the importer share one c4git store, which the
importer needs in order to find pushed assets.

| commit | path    | push returns (p50 / p90) | view stored (p50 / p90 / max) | import of the commit (p50) |
| ------ | ------- | ------------------------ | ----------------------------- | -------------------------- |
| small  | plain   | 83 / 104 ms              |                               |                            |
| small  | hook    | 168 / 218 ms             | 169 / 218 / 242 ms            | 46 ms                      |
| small  | webhook | 246 / 314 ms             | 247 / 315 / 357 ms            | 46 ms                      |
| asset  | plain   | 98 / 110 ms              |                               |                            |
| asset  | hook    | 206 / 243 ms             | 206 / 244 / 259 ms            | 58 ms                      |
| asset  | webhook | 305 / 337 ms             | 306 / 337 / 339 ms            | 56 ms                      |

So the hook adds about 85 ms to a small push: 46 ms imports the commit
(three fsync rounds into the local shards) and the rest is fixed cost.
That fixed cost is the importer's process start, reading the mapping, and
git's rev-parse and rev-list. A no-op hook run takes 25 to 40 ms. The
webhook path adds about another 80 ms, mostly the listener's `git fetch`
into its mirror: the listener's own time per request was 143 ms at p50 and
190 ms at p90, fetch included. The 3 MB asset adds 10 to 12 ms of import
time, which is the copy from the c4git store, and about 15 ms of push time,
which is c4git's clean filter at commit time and the larger push. A full
resolution of the new view through `view.FromID` (every record of the
2,231-file view) took 135 ms for small commits and 160 to 167 ms for asset
commits. A sparse CI checkout reads only its include set.

## Results: `repo`, 401 commits

| measure                                        | value                                  |
| ---------------------------------------------- | -------------------------------------- |
| total import, three local shards               | 37.8 s wall, 2.3 s user, 4.7 s sys     |
| base commit (2,134 files, full ls-tree import) | 18.1 s, 2,039 objects, 130 MiB         |
| per later commit, local shards                 | p50 37.0 ms, p90 55.1 ms, p99 162 ms   |
| per later commit, max                          | 1,755 ms (an fsync stall, see below)   |
| per later commit, importer alone               | p50 3.0 ms, p90 3.3 ms, p99 12.9 ms    |
| whole history, importer alone                  | 1.33 s (base 158 ms)                   |
| objects uploaded                               | 3,045 (176.1 MiB)                      |
| objects already present                        | 0 on a fresh store                     |
| re-import into the same shards                 | 1.37 s; 0 uploaded, 3,045 present      |
| git blobs read                                 | 3,047: 2,985 hashed, 62 c4git pointers |
| c4git objects used by ID, no rehash            | 62 (166.7 MiB), copied store to store  |
| parent records read / new records              | 2,128 / 3,301                          |
| records per later commit                       | read p50 5 (max 8), new p50 6 (max 12) |
| largest view                                   | 2,226 files, 2 symlinks, 1,132 dirs    |
| largest view, bytes                            | 139.9 MiB, 2,127 unique objects        |
| continue: 100 commits after 301 imported       | 4.8 s, 253 objects uploaded            |
| newest 50 commits only (`-max-commits 50`)     | 21.5 s, base plus 50, 2,239 objects    |

Each git blob is read once in the whole import. Of the 3,047 blobs read,
2,983 are file content (hashed and uploaded), 2 are symlink targets, and 62
are c4git pointers; the 3,045 objects uploaded are those 2,983 plus the 62
c4git objects. The 166.7 MiB of asset content was never hashed by the
importer; it was copied under the IDs c4git computed at `git add`.

The local TreeStore makes every write durable with a full fsync, which is
most of the per-commit wall time: the importer alone takes 3.0 ms at p50,
and three fsync rounds per commit take the rest. Those rounds are: file
objects and inner records in parallel, then the root record and the
changeset together. The outliers of one to two seconds are commits of one
or two small files whose summed store time was several seconds, which is
APFS flush latency, not importer work. Against S3 the same structure
applies with a network round trip (a Head, then a Put when absent) in place
of each fsync; the importer issues up to `-jobs` (default 8) writes at once.

## Cost follows the change, measured on three layouts

| repository                   | files | per commit, importer alone (run 1 of 3) | new record bytes per commit (p50) |
| ---------------------------- | ----- | --------------------------------------- | --------------------------------- |
| `repo`, 150 flat services    | 2,218 | p50 3.0 ms, p90 3.3 ms                  | 20,246                            |
| `repo-large`, 600 flat       | 8,555 | p50 10.3 ms, p90 13.1 ms                | 73,504                            |
| `repo-teams`, 600 in 24 dirs | 8,555 | p50 1.3 ms, p90 1.8 ms                  | 7,926                             |

The three histories make the same kinds of changes. Records read and
written per commit are the same small numbers in all three (read p50 5 or
6, max 9; new p50 6, max 13), because only directories along changed paths
are touched. What a commit costs is the size of those directories'
records, since a record is a one-level listing that is rewritten when any
child changes. With 600 services in one `services/` directory, every service
commit rewrites a 600-entry listing; with the same 8,555 files grouped under
24 team directories, the rewritten listings are small and a commit costs
1.3 ms, less than in the 2,218-file flat repository. Repository size alone
does not enter.

A CPU profile of the 600-flat case puts most of the importer's time in
`c4.ID.String` (base58 through `math/big`) while canonicalizing those
listings.

Full imports into local shards: `repo-large` 107.7 s, `repo-teams` 97.0 s;
per later commit p50 53 ms and 40 ms, again mostly fsync.

## Notes

- Submodules (mode 160000) are skipped and counted; none are in this
  repository.
- A c4git pointer whose object is in neither the c4git store nor the project
  store is recorded with a null size and reported; none were missing here.
- c4git 1.0.18's smudge filter writes only the leading 256 bytes of a blob
  that is not a pointer (it reads 256 bytes to look for an ID and writes
  what it read). A file committed before its path was c4git-managed is
  therefore truncated in a c4git checkout. The importer records the blob's
  real content. This repository commits every asset through the filter, so
  it is not affected.
