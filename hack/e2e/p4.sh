#!/usr/bin/env bash
# P4: monorepo CI on the farm (Scion's case, DESIGN.md section 8.2). The
# synthetic monorepo's history (hack/e2e/monorepo) is imported into project
# mono's store, three MinIO shards. CI shards mount commit views instead of
# checking out; an API-only diff answers which files a change touched, from
# records alone; a sparse view mounts one service.
#
#   hack/e2e/p4.sh        (after hack/e2e/up.sh and P5's pools)
source "$(dirname "$0")/lib.sh"

REPO=${REPO:-/tmp/c4-monorepo-e2e/repo}
HUD=http://127.0.0.1:30080/api/v1
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
[ -x "$ROOT/bin/c4git" ] || GOBIN=$ROOT/bin go install github.com/Avalanche-io/c4git/cmd/c4git@latest
if [ ! -d "$REPO" ]; then
  go build -o "$ROOT/bin/monorepo" "$ROOT/hack/e2e/monorepo"
  "$ROOT/bin/monorepo" -out "$REPO" -c4git "$ROOT/bin/c4git" -seed 1 -services 150 -commits 400 >/dev/null
fi
HEAD=$(git -C "$REPO" rev-parse HEAD)

record ""
record "## P4: monorepo CI ($(date -u +%Y-%m-%dT%H:%MZ), build $BUILD)"
record ""

farm_manager
log "import the history into project mono's three MinIO shards"
mc_run "for i in 0 1 2; do mc rm --recursive --force m\$i/mono >/dev/null 2>&1 || true; done"
for ns in mono; do
  k -n "$ns" delete workjobs,materializations --all --wait=true >/dev/null 2>&1 || true
  k -n "$ns" delete pods --all --wait=true >/dev/null 2>&1 || true
done
rm -f "$OUT/p4-map.jsonl"
rm -rf "$OUT/p4-map.jsonl.chains"
t0=$(now)
# shellcheck disable=SC2046
"$OP" import-git -repo "$REPO" $(shard_args mono host) -out "$OUT/p4-map.jsonl" -quiet >"$OUT/p4-import.log"
IMPORT_S=$(since "$t0")
# field_of COMMIT FIELD: a field of the commit's line in the mapping (the
# mapping also holds chain lines, which name no commit).
field_of() { python3 -I -c 'import json,sys
for l in open(sys.argv[1]):
    d=json.loads(l)
    if d.get("commit") and d["commit"].startswith(sys.argv[2]): print(d[sys.argv[3]])' "$OUT/p4-map.jsonl" "$1" "$2"; }
view_of() { field_of "$1" view; }
VIEW=$(view_of "$HEAD")
grep -E '^(commits|per commit|objects uploaded|git blobs|c4git objects)' "$OUT/p4-import.log" >"$OUT/p4-import-summary.txt"

log "CI on two consecutive commits: the parent cold, the head as a delta, the head again warm"
# Start from empty node and regional caches, so the first mounts are cold.
cold_start
PARENT=$(git -C "$REPO" rev-parse HEAD~1)
PVIEW=$(view_of "$PARENT")
HEAD_CS=$(field_of "$HEAD" changeset)
k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: parent, namespace: mono}
spec:
  source: {id: "$PVIEW"}
---
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: head, namespace: mono}
spec:
  source: {id: "$HEAD_CS"}
---
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: svc-002, namespace: mono}
spec:
  source: {id: "$VIEW"}
  include: [services/svc-002-tracer/, libs/common/]
EOF
t0=$(now)
mat_view mono parent >/dev/null
RESOLVE_S=$(since "$t0")
[ "$(mat_view mono head)" = "$VIEW" ] || fail "the head's changeset does not resolve to the head view"
LAYERS=$(k -n mono get materialization head -o jsonpath='{.status.layers}' | python3 -I -c 'import json,sys; print(len(json.load(sys.stdin)))')
mat_view mono svc-002 >/dev/null
SPARSE_FILES=$(k -n mono get materialization svc-002 -o jsonpath='{.status.files}')
FULL_FILES=$(k -n mono get materialization head -o jsonpath='{.status.files}')
FULL_BYTES=$(k -n mono get materialization head -o jsonpath='{.status.bytes}')
SPARSE_VIEW=$(k -n mono get materialization svc-002 -o jsonpath='{.status.viewID}')

# ci NAME MATERIALIZATION EXPECT: 16 shards that verify their mount is
# exactly EXPECT, then test; prints a table row with the time to green and
# the mounts.
ci() {
  local t0 secs since
  t0=$(now)
  since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: WorkJob
metadata: {name: $1, namespace: mono}
spec:
  class: ci
  pools: [cpu]
  params: {shards: "16", latencyTarget: "5m", estimateSeconds: "4", change: "$1"}
  inputs: [{materialization: $2, mountPath: /src, readOnly: true}]
  template:
    spec:
      securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
      - name: test
        image: c4-e2e:dev
        imagePullPolicy: IfNotPresent
        command: [sh, -c, "c4e2e verify --expect $3 /src && c4e2e shard --mean 3s"]
        securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
EOF
  k -n mono wait --for=jsonpath='{.status.phase}'=Succeeded "workjob/$1" --timeout=600s >/dev/null ||
    fail "$1: $(k -n mono get workjob "$1" -o jsonpath='{.status.phase} {.status.units}')"
  secs=$(since "$t0")
  for n in $WORKERS; do
    k -n c4-system logs "$(node_pod "$n")" -c node --since-time="$since" 2>/dev/null | grep '"msg":"published"' | grep "\"pod\":\"mono/$1-u" || true
  done | perl -MJSON::PP -ne '$j=decode_json($_); printf "%s %.4f\n", $j->{outcome}, $j->{seconds}' >"$OUT/p4-publish-$1.txt"
  python3 -I "$ROOT/hack/e2e/p4mounts.py" "$OUT/p4-publish-$1.txt" "$secs" "$1"
}
CI_ROWS="$(ci change-parent parent "$PVIEW")
$(ci change-head head "$VIEW")
$(ci change-head-rerun head "$VIEW")"

log "the same commit checked out by git with c4git"
# Outside the module, so Go tooling never sees the synthetic sources.
CLONE=$(mktemp -d)/clone
t0=$(now)
git clone -q --no-checkout "$REPO" "$CLONE"
git -C "$CLONE" config filter.c4.smudge "$ROOT/bin/c4git smudge %f"
git -C "$CLONE" config filter.c4.clean "$ROOT/bin/c4git clean %f"
C4_STORE="$REPO/.c4/store" git -C "$CLONE" checkout -q "$HEAD"
rm -rf "$CLONE/.git"
CLONE_S=$(since "$t0")
CLONE_ID=$("$TOOL" tree-id "$CLONE" 2>/dev/null || echo "")
rm -rf "$(dirname "$CLONE")"

log "API-only diffs between commits"
DIFFS=""
for back in 1 10 100; do
  FROM=$(git -C "$REPO" rev-parse "HEAD~$back")
  FV=$(view_of "$FROM")
  curl -sf "$HUD/views/mono/diff?from=$FV&to=$VIEW&limit=100000&as=wrangler" >"$OUT/p4-diff-$back.json"
  git -C "$REPO" diff --no-renames --name-status "$FROM" "$HEAD" >"$OUT/p4-git-diff-$back.txt"
  DIFFS="$DIFFS$(python3 -I - "$OUT/p4-diff-$back.json" "$OUT/p4-git-diff-$back.txt" "$back" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
ops = {"A": "added", "M": "modified", "D": "removed", "T": "modified"}
git = {}
for line in open(sys.argv[2]):
    st, path = line.rstrip("\n").split("\t", 1)
    git[path] = ops[st[0]]
ours = {c["path"]: c["op"] for c in d["changes"]}
same = ours == git
c = d["counts"]
print(f"| HEAD~{sys.argv[3]} to HEAD | {c['added']} added, {c['modified']} modified, {c['removed']} removed | {d['seconds']*1000:.0f} ms | {'yes' if same else 'NO'} |")
if not same:
    print(f"| | ours only: {sorted(set(ours.items()) - set(git.items()))[:5]} | git only: {sorted(set(git.items()) - set(ours.items()))[:5]} | |")
PY
)
"
done
grep -q '| NO |' <<<"$DIFFS" && fail "an API diff disagreed with git diff: $DIFFS"

record "The synthetic monorepo (seed 1: 150 services, 400 commits after the initial one, 62 assets through the real c4git filter; head \`${HEAD:0:12}\`) imported into project \`mono\`'s three MinIO shards in ${IMPORT_S} s:"
record ""
while IFS= read -r line; do record "- $line"; done <"$OUT/p4-import-summary.txt"
record ""
record "| measure | value |"
record "|---|---|"
record "| head view | ${VIEW:0:14}..., $FULL_FILES files, $((FULL_BYTES / 1048576)) MiB |"
record "| the parent's Materialization resolved and preflighted (every object HEADed in the store) | ${RESOLVE_S} s |"
record "| the head's Materialization, by its commit changeset | a chain of $LAYERS views: the parent's, then the head's |"
record "| git clone and c4git checkout of the head, from a local remote | ${CLONE_S} s; the tree equals the view: $([ "$CLONE_ID" = "$VIEW" ] && echo yes || echo "no ($CLONE_ID)") |"
record "| a sparse view: services/svc-002-tracer/ and libs/common/ | $SPARSE_FILES of $FULL_FILES files, view ${SPARSE_VIEW:0:10}... |"
record ""
record "CI on the farm: 16 test shards per change on the four cpu nodes, each shard verifying that its mount is exactly the commit's view before testing. A node's first mount of a view builds its layer once, and the other shards on that node wait on that build; publishes are counted per pod:"
record ""
record "| change | result | mounts |"
record "|---|---|---|"
printf '%s\n' "$CI_ROWS" | while IFS= read -r line; do [ -n "$line" ] && record "$line"; done
record ""
record "API-only diffs: the HUD's \`GET /views/mono/diff\` (and the MCP tool \`c4_diff_views\`) between a commit's view and the head's, read from directory records, against \`git diff --name-status\` of the same commits:"
record ""
record "| change | files | answered in | same files and kinds as git |"
record "|---|---|---|---|"
printf '%s' "$DIFFS" | while IFS= read -r line; do [ -n "$line" ] && record "$line"; done
record ""
echo "P4 PASS"
