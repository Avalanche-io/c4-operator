#!/usr/bin/env bash
# Builds the synthetic monorepos and measures `c4-operator import-git` on
# them. Everything is local: the project store is three TreeStore shards
# on the local disk. Output goes to $OUT (default /tmp/c4-monorepo-e2e).
#
#   hack/e2e/monorepo/run.sh
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
OUT=${OUT:-/tmp/c4-monorepo-e2e}
BIN=$ROOT/bin
cd "$ROOT"

if [ ! -x "$BIN/c4git" ]; then
	GOBIN=$BIN go install github.com/Avalanche-io/c4git/cmd/c4git@latest
fi
go build -o "$BIN/c4-operator" ./cmd/c4-operator
go build -o "$BIN/monorepo" ./hack/e2e/monorepo
go build -o "$BIN/measure" ./hack/e2e/monorepo/measure
go build -o "$BIN/pushlat" ./hack/e2e/monorepo/pushlat

rm -rf "$OUT"
mkdir -p "$OUT"
# The importer's git processes must not pick up a user's global config.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

shards() {
	local d=$1
	mkdir -p "$d/a" "$d/b" "$d/c"
	echo "-store $d/a -store $d/b -store $d/c"
}

echo "== generate (seed 1, 150 services, 400 commits)"
"$BIN/monorepo" -out "$OUT/repo" -c4git "$BIN/c4git" -seed 1 -services 150 -commits 400
du -sh "$OUT/repo/.git" "$OUT/repo/.c4/store"

echo "== import into three local shards"
S=$(shards "$OUT/shards")
/usr/bin/time -p "$BIN/c4-operator" import-git -repo "$OUT/repo" $S -out "$OUT/map.jsonl" >"$OUT/import.log"
tail -9 "$OUT/import.log"

echo "== checkouts (smudged by c4git) of sampled commits against the imported views"
mkdir -p "$OUT/wt"
for sha in $(git -C "$OUT/repo" rev-list --first-parent HEAD | awk 'NR % 40 == 1'); do
	C4_STORE="$OUT/repo/.c4/store" git -C "$OUT/repo" worktree add -q --detach "$OUT/wt/$sha" "$sha"
	rm "$OUT/wt/$sha/.git"
	want=$(grep "\"commit\":\"$sha\"" "$OUT/map.jsonl" | sed 's/.*"view":"\([^"]*\)".*/\1/')
	got=$("$BIN/c4-operator" tree-id "$OUT/wt/$sha")
	[ "$got" = "$want" ] || { echo "MISMATCH $sha: checkout $got, import $want"; exit 1; }
	echo "ok ${sha:0:10} $got"
	rm -rf "$OUT/wt/$sha"
done
git -C "$OUT/repo" worktree prune

echo "== push-to-view latency (post-receive hook, and webhook to a listener with a mirror)"
"$BIN/pushlat" -repo "$OUT/repo" -out "$OUT/pushlat" -bin "$BIN" -small 30 -assets 10 -asset-mb 3

echo "== import again into the same shards (everything present)"
"$BIN/c4-operator" import-git -repo "$OUT/repo" $S -out "$OUT/map-again.jsonl" -quiet

echo "== importer alone (a sink that stores nothing)"
"$BIN/measure" -repo "$OUT/repo" -runs 3

echo "== continue an import: the oldest 301 commits, then the rest"
S2=$(shards "$OUT/shards-inc")
"$BIN/c4-operator" import-git -repo "$OUT/repo" -rev HEAD~100 $S2 -out "$OUT/map-inc.jsonl" -quiet | sed -n 1p
"$BIN/c4-operator" import-git -repo "$OUT/repo" $S2 -out "$OUT/map-inc.jsonl" -quiet
echo "full import head view:        $(tail -1 "$OUT/map.jsonl" | sed 's/.*"view":"\([^"]*\)".*/\1/')"
echo "continued import head view:   $(tail -1 "$OUT/map-inc.jsonl" | sed 's/.*"view":"\([^"]*\)".*/\1/')"

echo "== newest 50 commits only"
S3=$(shards "$OUT/shards-50")
"$BIN/c4-operator" import-git -repo "$OUT/repo" -max-commits 50 $S3 -quiet

echo "== generate a repository four times larger (600 services, 1 MB assets)"
"$BIN/monorepo" -out "$OUT/repo-large" -c4git "$BIN/c4git" -seed 1 -services 600 -commits 400 -asset-min-mb 1 -asset-max-mb 1
S4=$(shards "$OUT/shards-large")
/usr/bin/time -p "$BIN/c4-operator" import-git -repo "$OUT/repo-large" $S4 -out "$OUT/map-large.jsonl" -quiet
"$BIN/measure" -repo "$OUT/repo-large" -runs 3

echo "== the same 600 services grouped under 24 team directories"
"$BIN/monorepo" -out "$OUT/repo-teams" -c4git "$BIN/c4git" -seed 1 -services 600 -teams 24 -commits 400 -asset-min-mb 1 -asset-max-mb 1
S5=$(shards "$OUT/shards-teams")
/usr/bin/time -p "$BIN/c4-operator" import-git -repo "$OUT/repo-teams" $S5 -out "$OUT/map-teams.jsonl" -quiet
"$BIN/measure" -repo "$OUT/repo-teams" -runs 3
