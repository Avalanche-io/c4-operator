#!/usr/bin/env bash
# End-to-end run on a local kind cluster. Everything stays on this
# machine: images are built locally and loaded into kind, the store is
# MinIO inside the cluster.
#
#   hack/e2e/run.sh            # FRAME_MIB=16 by default (2.8 GiB show)
#   FRAME_MIB=4 hack/e2e/run.sh
#
# Results land in hack/e2e/out/results.md.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CLUSTER=${CLUSTER:-c4-operator-e2e}
CTX=kind-$CLUSTER
NODE=$CLUSTER-control-plane
FRAME_MIB=${FRAME_MIB:-16}
OUT=$ROOT/hack/e2e/out
GOBIN=$(go env GOBIN); GOBIN=${GOBIN:-$(go env GOPATH)/bin}
KIND=${KIND:-$(command -v kind || echo "$GOBIN/kind")}
C4=${C4:-$(command -v c4)}
NS=render
STORE_HOST="s3://c4-store/show/?endpoint=http://127.0.0.1:30900"
STORE_CLUSTER="s3://c4-store/show/?endpoint=http://minio.c4-store:9000"
export AWS_ACCESS_KEY_ID=c4e2eaccess AWS_SECRET_ACCESS_KEY=c4e2esecretkey

k() { kubectl --context "$CTX" "$@"; }
now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
log() { printf '\n== %s\n' "$*" >&2; }
fail() { echo "FAIL: $*" >&2; exit 1; }
jget() { k -n "$NS" get "$1" "$2" -o jsonpath="$3"; }

mkdir -p "$OUT"
RESULTS=$OUT/results.md
: > "$RESULTS"
record() { printf '%s\n' "$*" | tee -a "$RESULTS"; }

log "cluster"
if ! "$KIND" get clusters | grep -qx "$CLUSTER"; then
  "$KIND" create cluster --name "$CLUSTER" --config "$ROOT/hack/e2e/kind.yaml"
fi

log "images"
make -C "$ROOT" image >/dev/null
"$KIND" load docker-image c4-operator:dev --name "$CLUSTER" >/dev/null
for img in minio/minio:latest minio/mc:latest; do
  docker image inspect "$img" >/dev/null 2>&1 || docker pull "$img"
  "$KIND" load docker-image "$img" --name "$CLUSTER" >/dev/null
done

log "operator"
k apply -k "$ROOT/config/default" >/dev/null
# Start cold: no render pods, an empty node cache, a fresh agent.
k delete ns "$NS" --ignore-not-found --wait=true >/dev/null
docker exec "$NODE" rm -rf /var/lib/c4/objects /var/lib/c4/views /var/lib/c4/meta
k -n c4-system rollout restart deploy/c4-operator-manager ds/c4-operator-agent >/dev/null
k -n c4-system rollout status deploy/c4-operator-manager --timeout=180s
k -n c4-system rollout status ds/c4-operator-agent --timeout=180s

log "store (MinIO)"
k apply -f "$ROOT/hack/e2e/minio.yaml" >/dev/null
k -n c4-store rollout status deploy/minio --timeout=180s
until curl -sf http://127.0.0.1:30900/minio/health/ready >/dev/null; do sleep 1; done
k -n c4-store delete pod mc --ignore-not-found >/dev/null
k -n c4-store run mc --restart=Never --image=minio/mc:latest --image-pull-policy=IfNotPresent --command -- \
  sh -c 'until mc alias set e2e http://minio.c4-store:9000 c4e2eaccess c4e2esecretkey >/dev/null 2>&1; do sleep 1; done; mc mb --ignore-existing e2e/c4-store' >/dev/null
k -n c4-store wait --for=jsonpath='{.status.phase}'=Succeeded pod/mc --timeout=120s >/dev/null

log "show: generate ${FRAME_MIB} MiB frames, snapshot into the store with the c4 CLI"
go build -o "$ROOT/bin/seed" "$ROOT/hack/e2e/seed"
rm -rf "$OUT/show" "$OUT/missing"
"$ROOT/bin/seed" gen --out "$OUT/show" --frame-mib "$FRAME_MIB" | tee "$OUT/gen.txt"
(cd "$OUT" && C4_STORE=$STORE_HOST "$C4" id -s show > show.c4m 2> store.log)
SHOW_ID=$(awk '/^stored:/{print $2}' "$OUT/store.log" | tail -1)
[ -n "$SHOW_ID" ] || fail "c4 id -s did not report a stored ID"
echo "stored show c4m: $SHOW_ID"

# A description that names content the store has never seen.
mkdir -p "$OUT/missing/plates"
head -c 1048576 /dev/urandom > "$OUT/missing/plates/never-uploaded.exr"
ln "$OUT/show/weights/denoise-v3.safetensors" "$OUT/missing/plates/weights.safetensors"
(cd "$OUT" && "$C4" id missing > missing.c4m)

EXPECT_SH010=$("$ROOT/bin/seed" expect --src "$OUT/show" --include shots/sh010/,editorial/,weights/)
EXPECT_SH020=$("$ROOT/bin/seed" expect --src "$OUT/show" --include shots/sh020/,weights/)

log "namespace, credentials, sources, Materializations"
k create ns "$NS" >/dev/null
k -n "$NS" create secret generic store-credentials \
  --from-literal=AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" --from-literal=AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" >/dev/null
k -n "$NS" create configmap show-c4m --from-file=show.c4m="$OUT/show.c4m" >/dev/null
k -n "$NS" create configmap missing-c4m --from-file=missing.c4m="$OUT/missing.c4m" >/dev/null
k -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: sh010}
spec:
  source: {configMapKeyRef: {name: show-c4m, key: show.c4m}}
  store: "$STORE_CLUSTER"
  storeSecretRef: {name: store-credentials}
  include: [shots/sh010/, editorial/, weights/]
  retainOnNode: 1h
---
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: sh020}
spec:
  source: {id: "$SHOW_ID"}
  store: "$STORE_CLUSTER"
  storeSecretRef: {name: store-credentials}
  include: [shots/sh020/, weights/]
---
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: missing}
spec:
  source: {configMapKeyRef: {name: missing-c4m, key: missing.c4m}}
  store: "$STORE_CLUSTER"
  storeSecretRef: {name: store-credentials}
EOF
k -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready materialization/sh010 materialization/sh020 --timeout=180s >/dev/null
k -n "$NS" wait --for=jsonpath='{.status.phase}'=Failed materialization/missing --timeout=180s >/dev/null
k -n "$NS" get materializations -o wide

VIEW_SH010=$(jget materialization sh010 '{.status.viewID}')
VIEW_SH020=$(jget materialization sh020 '{.status.viewID}')
[ "$VIEW_SH010" = "$EXPECT_SH010" ] || fail "sh010 viewID $VIEW_SH010 != host scan $EXPECT_SH010"
[ "$VIEW_SH020" = "$EXPECT_SH020" ] || fail "sh020 viewID $VIEW_SH020 != host scan $EXPECT_SH020"
[ "$(jget materialization sh010 '{.status.rootID}')" = "$(jget materialization sh020 '{.status.rootID}')" ] \
  || fail "the same show resolved to different root IDs from a ConfigMap and from the store"

# render_pod NAME MATERIALIZATION EXPECTED_VIEW
render_pod() {
  k -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
  annotations:
    c4.avalanche.io/materialize: $2
    c4.avalanche.io/mount-path: /assets
spec:
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, runAsUser: 65532}
  containers:
  - name: render
    image: c4-operator:dev
    args: [tree-id, --expect, "$3", --hold, 30m, /assets]
EOF
}

# run_pod NAME MAT VIEW -> prints "wall_seconds"
run_pod() {
  local t0 t1
  t0=$(now)
  render_pod "$1" "$2" "$3"
  k -n "$NS" wait --for=condition=Ready "pod/$1" --timeout=900s >/dev/null
  t1=$(now)
  perl -e "printf '%.2f', $t1 - $t0"
}

ensure_json() { k -n "$NS" logs "$1" -c c4-materialize | grep '^{' | tail -1; }
field() { perl -MJSON::PP -e '$j=decode_json(<STDIN>); print $j->{$ARGV[0]} // 0' "$1"; }
tree_seen() {
  local id=""
  for _ in $(seq 1 30); do
    id=$(k -n "$NS" logs "$1" -c render 2>/dev/null | head -1)
    [ -n "$id" ] && break
    sleep 1
  done
  echo "$id"
}

log "pods: cold, warm, and a second shot sharing content"
WALL1=$(run_pod render-sh010-a sh010 "$VIEW_SH010")
WALL2=$(run_pod render-sh010-b sh010 "$VIEW_SH010")
WALL3=$(run_pod render-sh020-a sh020 "$VIEW_SH020")

record "## e2e run $(date -u +%Y-%m-%dT%H:%MZ), frame size ${FRAME_MIB} MiB"
record ""
record "Show: $(cat "$OUT/gen.txt")"
record ""
record "| pod | Materialization | wall to Ready (s) | agent ms | objects | cached | fetched | refetched | bytes fetched | bytes materialized | tree ID matches |"
record "|---|---|---|---|---|---|---|---|---|---|---|"
row() {
  local pod=$1 mat=$2 wall=$3 view=$4 j seen
  j=$(ensure_json "$pod")
  seen=$(tree_seen "$pod")
  [ "$seen" = "$view" ] || fail "$pod sees tree $seen, expected $view"
  record "| $pod | $mat | $wall | $(echo "$j" | field millis) | $(echo "$j" | field objects) | $(echo "$j" | field hits) | $(echo "$j" | field misses) | $(echo "$j" | field refetched) | $(echo "$j" | field bytesFetched) | $(echo "$j" | field bytes) | yes |"
}
row render-sh010-a sh010 "$WALL1" "$VIEW_SH010"
row render-sh010-b sh010 "$WALL2" "$VIEW_SH010"
row render-sh020-a sh020 "$WALL3" "$VIEW_SH020"

log "corrupt one cached object through the view, then start another sh010 pod"
VICTIM=/var/lib/c4/views/$VIEW_SH010/shots/sh010/plates/sh010.1010.exr
docker exec "$NODE" sh -c "printf CORRUPT | dd of=$VICTIM bs=1 seek=4096 conv=notrunc 2>/dev/null"
WALL4=$(run_pod render-sh010-c sh010 "$VIEW_SH010")
row render-sh010-c sh010 "$WALL4" "$VIEW_SH010"
j=$(ensure_json render-sh010-c)
[ "$(echo "$j" | field refetched)" = 1 ] || fail "corruption was not detected and refetched: $j"
[ "$(k -n "$NS" get pod render-sh010-a -o jsonpath='{.status.phase}')" = Running ] || fail "the first pod died during the rebuild"

log "missing content: the workload must never start"
render_pod render-missing missing "$(jget materialization missing '{.status.viewID}')"
k -n "$NS" wait --for=jsonpath='{.status.initContainerStatuses[0].state.terminated.reason}'=Error pod/render-missing --timeout=300s >/dev/null
MISSING_MSG=$(k -n "$NS" logs render-missing -c c4-materialize | tail -1)
STARTED=$(k -n "$NS" get pod render-missing -o jsonpath='{.status.containerStatuses[0].state.running}{.status.containerStatuses[0].state.terminated}')
[ -z "$STARTED" ] || fail "the workload container ran despite missing content"
[ "$(jget materialization missing '{.status.missing}')" = 1 ] || fail "controller did not report exactly one missing object"

log "unknown Materialization: admission refuses the pod"
if DENY=$(render_pod render-unknown no-such-materialization x 2>&1); then
  fail "a pod naming an unknown Materialization was admitted"
fi

log "cache accounting on the node"
NODE_OBJECTS=$(docker exec "$NODE" sh -c 'find /var/lib/c4/objects -type f ! -name ".*" | wc -l' | tr -d ' ')
NODE_BYTES=$(docker exec "$NODE" sh -c 'find /var/lib/c4/objects -type f ! -name ".*" -exec stat -c %s {} + | awk "{s+=\$1} END {print s}"')
sum_status() { k -n "$NS" get materialization sh010 sh020 -o jsonpath="{range .items[*]}{.status.nodes[0].$1}{\"\\n\"}{end}" | awk '{s+=$1} END {print s}'; }
FETCHED=$(sum_status bytesFetched)
MATERIALIZED=$(sum_status bytesMaterialized)
MISSES=$(sum_status misses)
REFETCHED=$(sum_status refetched)
SH030=$(docker exec "$NODE" sh -c 'ls /var/lib/c4/views/*/shots 2>/dev/null | grep -c sh030 || true')
FRAME_BYTES=$((FRAME_MIB * 1048576))

[ "$((MISSES - REFETCHED))" = "$NODE_OBJECTS" ] || fail "fetches ($MISSES - $REFETCHED refetched) != unique objects on node ($NODE_OBJECTS)"
[ "$((FETCHED - REFETCHED * FRAME_BYTES))" = "$NODE_BYTES" ] || fail "bytes fetched ($FETCHED) != cached bytes ($NODE_BYTES) + refetch"
[ "$SH030" = 0 ] || fail "sh030 was materialized although nobody asked for it"

record ""
record "| measure | value |"
record "|---|---|"
record "| unique objects in the store (whole show) | $(sed -E 's/.*unique=([0-9]+).*/\1/' "$OUT/gen.txt") |"
record "| unique objects fetched to the node | $NODE_OBJECTS (each once; plus $REFETCHED refetch after corruption) |"
record "| bytes fetched from the store | $FETCHED |"
record "| bytes materialized into pods (4 pods) | $MATERIALIZED |"
record "| bytes on the node's disk (cache) | $NODE_BYTES |"
record "| fetched / materialized | $(perl -e "printf '%.2f', $FETCHED / $MATERIALIZED") |"
record ""
record "Missing content: Materialization \`missing\` phase $(jget materialization missing '{.status.phase}'), missing=$(jget materialization missing '{.status.missing}'); pod render-missing init container failed with:"
record ""
record "    $MISSING_MSG"
record ""
record "and its workload container never started."
record ""
record "Unknown Materialization: admission refused the pod: \`$(echo "$DENY" | tail -1)\`"

log "status"
k -n "$NS" get materialization sh010 -o jsonpath='{.status.nodes}' ; echo
k -n "$NS" get materialization sh020 -o jsonpath='{.status.nodes}' ; echo
echo
echo "PASS. Results in $RESULTS"
