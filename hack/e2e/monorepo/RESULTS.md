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
c4 log $OUT/map.jsonl.chains/refs_heads_main.c4m   # compared with the mapping's views
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
`c44GBH6esR5FjoZTX2kggfotFjvnLUEg2ekxR1SAaypfYbiCsgTefX9YYiu49GR7JJzq6ZA1yaVQJwQfNNuUTkRPzG`.
Every pushed commit in the latency run below resolved through
`view.FromID` on its changeset to its view, with the parent's view as the
chain base.

## The branch chain

Each ref is one c4m chain (DESIGN.md sections 2.3 and 2.5): the base view
ID, then each commit's patch section followed by its checkpoint, the
commit's view ID. The importer appends each commit's changeset to the ref's
chain file (`OUT.chains/refs_heads_main.c4m`, next to the mapping), at a
cost of the changeset's size. The mapping is the chain's index: each commit
line holds the changeset, the checkpoint (`view`), the chain's base and the
section number. Stored, the chain is its per-commit changesets, which
concatenate into it. The whole chain is stored as one object when it is
sealed: at the end of a one-shot import, when it reaches `-chain-max`
sections (1,024 by default), or when a force push leaves its head. A
sealed chain gets a mapping line with its object ID, and the next chain
starts from the last checkpoint.

On `repo`: the chain of `refs/heads/main` has 401 sections in 748,353
bytes, and was stored once at the end of the import. `c4 log` (c4 1.0.18)
on the chain file, unchanged, takes 5.6 s and lists 401 versions whose IDs
are exactly the mapping's 401 commit views, in order.

What the c4 CLI does with these chains:

- `c4 log` and `c4 split` read a chain from the root commit as it is. Its
  base is the empty view, and section 1 is the whole tree.
- A chain over a non-empty external base (a branch from an imported
  commit, or a chain after re-basing) is misread by c4 1.0.18: it takes
  section 1 as the base. `c4-operator import-git -export-chain REF` (and
  the listener's `GET /chains/REF?full=1`) writes the chain with its base
  listing in full (`view.ExportChain`), and `c4 log` and `c4 split` then
  read it correctly. The tests check both cases with the c4 binary on PATH.
- `c4 split` drops the closing checkpoint of the last section, so its
  `after` file ends in an unsealed section. Its sections verify, and the
  last patch still produces the head view.

Getting `c4 log` to list the view IDs needed a fix in `internal/view`. A
directory holding a symlink has a null size, because the symlink's size is
null. The directory's own entry, and so the changeset, said null. But its
parent's record said 0, because the record was canonicalized with c4m's
`PropagateMetadata`, which reads a child directory with a null size and no
children in scope as empty. The records still hashed to their IDs, so
nothing inside the operator noticed. `c4 log` computes each version's ID
from the nested tree, which keeps the null, so on this repository (two
symlinks in subdirectories) every listed ID differed from the view ID. Now
`dirEntry` hashes the listing exactly as written and `listingOf` no longer
canonicalizes it. View IDs change only for trees with a null-size directory
(one holding a symlink, at any depth). A new test checks that a view's ID
equals c4m's ID of the same tree as one nested manifest, both for a
scanned tree and for one built by `Apply`. As a side effect the importer
is about 20% faster, since each listing is no longer copied and propagated
before hashing.

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
| small  | plain   | 75 / 81 ms               |                               |                            |
| small  | hook    | 172 / 192 ms             | 173 / 192 / 198 ms            | 48 ms                      |
| small  | webhook | 232 / 247 ms             | 232 / 247 / 328 ms            | 50 ms                      |
| asset  | plain   | 73 / 75 ms               |                               |                            |
| asset  | hook    | 175 / 183 ms             | 176 / 184 / 185 ms            | 53 ms                      |
| asset  | webhook | 238 / 256 ms             | 239 / 256 / 262 ms            | 54 ms                      |

So the hook adds about 100 ms to a small push. About 50 ms imports the
commit: three fsync rounds into the local shards, plus appending the commit
to the branch chain. The rest is fixed cost: the importer's process start,
reading the mapping, and git's rev-parse and rev-list. A no-op hook run
takes 25 to 40 ms. The webhook path adds about another 60 ms, mostly the
listener's `git fetch` into its mirror; the listener's own time per
request was 142 ms at p50 and 151 ms at p90, fetch included. The 3 MB
asset adds about 5 ms of import time, which is the copy from the c4git
store. A full resolution of the new view through `view.FromID` (every
record of the 2,231-file view) took about 100 ms. A sparse CI checkout
reads only its include set.

## Results: `repo`, 401 commits

| measure                                        | value                                  |
| ---------------------------------------------- | -------------------------------------- |
| total import, three local shards               | 36.0 s wall, 2.3 s user, 5.2 s sys     |
| base commit (2,134 files, full ls-tree import) | 15.7 s, 2,039 objects, 130 MiB         |
| per later commit, local shards                 | p50 38.8 ms, p90 56.0 ms, p99 136 ms   |
| per later commit, max                          | 2,464 ms (an fsync stall, see below)   |
| per later commit, importer alone               | p50 2.4 ms, p90 2.7 ms, p99 10.7 ms    |
| whole history, importer alone                  | 1.09 s (base 140 ms)                   |
| objects uploaded                               | 3,045 (176.1 MiB)                      |
| objects already present                        | 0 on a fresh store                     |
| re-import into the same shards                 | 1.20 s; 0 uploaded, 3,045 present      |
| git blobs read                                 | 3,047: 2,985 hashed, 62 c4git pointers |
| c4git objects used by ID, no rehash            | 62 (166.7 MiB), copied store to store  |
| parent records read / new records              | 2,128 / 3,301                          |
| records per later commit                       | read p50 5 (max 8), new p50 6 (max 12) |
| largest view                                   | 2,226 files, 2 symlinks, 1,132 dirs    |
| largest view, bytes                            | 139.9 MiB, 2,127 unique objects        |
| continue: 100 commits after 301 imported       | 7.9 s, 253 objects uploaded            |
| newest 50 commits only (`-max-commits 50`)     | 17.6 s, base plus 50, 2,239 objects    |

Each git blob is read once in the whole import. Of the 3,047 blobs read,
2,983 are file content (hashed and uploaded), 2 are symlink targets, and 62
are c4git pointers; the 3,045 objects uploaded are those 2,983 plus the 62
c4git objects. The 166.7 MiB of asset content was never hashed by the
importer; it was copied under the IDs c4git computed at `git add`.

The local TreeStore makes every write durable with a full fsync, which is
most of the per-commit wall time: the importer alone takes 2.4 ms at p50,
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
| `repo`, 150 flat services    | 2,218 | p50 2.4 ms, p90 2.7 ms                  | 20,246                            |
| `repo-large`, 600 flat       | 8,555 | p50 8.4 ms, p90 9.1 ms                  | 73,504                            |
| `repo-teams`, 600 in 24 dirs | 8,555 | p50 1.1 ms, p90 1.4 ms                  | 7,926                             |

The three histories make the same kinds of changes. Records read and
written per commit are the same small numbers in all three (read p50 5 or
6, max 9; new p50 6, max 13), because only directories along changed paths
are touched. What a commit costs is the size of those directories'
records, since a record is a one-level listing that is rewritten when any
child changes. With 600 services in one `services/` directory, every service
commit rewrites a 600-entry listing; with the same 8,555 files grouped under
24 team directories, the rewritten listings are small and a commit costs
1.1 ms, less than in the 2,218-file flat repository. Repository size alone
does not enter.

A CPU profile of the 600-flat case puts most of the importer's time in
`c4.ID.String` (base58 through `math/big`) while canonicalizing those
listings.

Full imports into local shards: `repo-large` 90.6 s, `repo-teams` 104.4 s;
per later commit p50 39 ms and 49 ms, again mostly fsync.

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
