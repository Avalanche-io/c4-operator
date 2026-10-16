# Shared by the e2e scripts. Source it; do not run it.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
CLUSTER=${CLUSTER:-c4-operator-e2e}
CTX=kind-$CLUSTER
OUT=$ROOT/hack/e2e/out
RESULTS=$OUT/results.md
GOBIN=$(go env GOBIN); GOBIN=${GOBIN:-$(go env GOPATH)/bin}
KIND=${KIND:-$(command -v kind || echo "$GOBIN/kind")}
ARCH=$(go env GOARCH)
OP=$ROOT/bin/c4-operator
TOOL=$ROOT/bin/c4e2e
export AWS_ACCESS_KEY_ID=c4e2eaccess AWS_SECRET_ACCESS_KEY=c4e2esecretkey
WORKERS=$(printf "$CLUSTER-worker %s" "$CLUSTER-worker2 $CLUSTER-worker3 $CLUSTER-worker4 $CLUSTER-worker5 $CLUSTER-worker6 $CLUSTER-worker7 $CLUSTER-worker8")

mkdir -p "$OUT"

k() { kubectl --context "$CTX" "$@"; }
now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
log() { printf '\n== %s\n' "$*" >&2; }
fail() { echo "FAIL: $*" >&2; exit 1; }
record() { printf '%s\n' "$*" | tee -a "$RESULTS"; }
since() { perl -e "printf '%.2f', $(now) - $1"; }

# shard_args PROJECT HOST|CLUSTER: --shard flags for a project's bucket.
shard_args() {
  local bucket=$1 where=$2 i
  for i in 0 1 2; do
    if [ "$where" = host ]; then
      printf -- '--shard s%d=s3://%s/c4/?endpoint=http://127.0.0.1:3090%d ' "$i" "$bucket" "$i"
    else
      printf -- '--shard s%d=s3://%s/c4/?endpoint=http://minio-%d.c4-store:9000 ' "$i" "$bucket" "$i"
    fi
  done
}

# shards_yaml BUCKET: the Project's shard list.
shards_yaml() {
  local bucket=$1 i
  for i in 0 1 2; do
    printf '    - {name: s%d, url: "s3://%s/c4/?endpoint=http://minio-%d.c4-store:9000"}\n' "$i" "$bucket" "$i"
  done
}

# ingest BUCKET DIR: hash DIR into the bucket's three shards; prints the view ID.
ingest() {
  # shellcheck disable=SC2046
  "$OP" ingest -q $(shard_args "$1" host) "$2"
}

# mc ARGS...: run the MinIO client in the cluster against all shards.
mc_run() {
  local name=mc-$RANDOM
  k -n c4-store run "$name" --restart=Never --image=minio/mc:latest --image-pull-policy=IfNotPresent \
    --overrides='{"spec":{"tolerations":[{"operator":"Exists"}],"nodeSelector":{"kubernetes.io/hostname":"'"$CLUSTER"'-control-plane"}}}' \
    --command -- sh -c "for i in 0 1 2; do until mc alias set m\$i http://minio-\$i.c4-store:9000 c4e2eaccess c4e2esecretkey >/dev/null 2>&1; do sleep 1; done; done; $*" >/dev/null
  k -n c4-store wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$name" --timeout=180s >/dev/null || {
    k -n c4-store logs "$name" >&2 || true
    fail "mc: $*"
  }
  k -n c4-store logs "$name"
  k -n c4-store delete pod "$name" --wait=false >/dev/null
}

# store_gets: GetObject requests served by all three MinIO shards so far.
store_gets() {
  local total=0 i n
  for i in 0 1 2; do
    n=$(curl -sf "http://127.0.0.1:3090$i/minio/v2/metrics/cluster" |
      awk '/^minio_s3_requests_total\{/ && /api="getobject"/ {s+=$NF} END {printf "%d", s}')
    total=$((total + ${n:-0}))
  done
  echo "$total"
}

# sum_series NAME LABEL...: sum a Prometheus series from stdin over every
# line of that metric that carries all the given label pairs (in any order).
sum_series() {
  local name=$1
  shift
  awk -v name="$name" -v want="$*" '
    BEGIN { n = split(want, labels, " ") }
    index($0, name "{") == 1 || $1 == name {
      for (i = 1; i <= n; i++) if (index($0, labels[i]) == 0) next
      v += $NF
    }
    END { printf "%d", v }'
}

# node_metric NODE NAME LABEL...: a series on one node daemon.
node_metric() {
  local n=$1
  shift
  docker exec "$n" curl -sf localhost:7444/metrics | sum_series "$@"
}

# nodes_metric NAME LABEL...: the series summed over all workers.
nodes_metric() {
  local n total=0 v
  for n in $WORKERS; do
    v=$(node_metric "$n" "$@")
    total=$((total + ${v:-0}))
  done
  echo "$total"
}

# regional_metric NAME LABEL...: the series summed over the regional replicas.
regional_metric() {
  local p total=0 v
  for p in $(k -n c4-system get pods -l app.kubernetes.io/component=regional --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}'); do
    v=$(k get --raw "/api/v1/namespaces/c4-system/pods/$p:7443/proxy/metrics" 2>/dev/null | sum_series "$@")
    total=$((total + ${v:-0}))
  done
  echo "$total"
}

# node_pod NODE: the c4 node daemon pod on NODE.
node_pod() {
  k -n c4-system get pod -l app.kubernetes.io/component=node --field-selector "spec.nodeName=$1" -o jsonpath='{.items[0].metadata.name}'
}

# publish_info NS POD: "outcome seconds layers" from the node daemon's log.
publish_info() {
  local node
  node=$(k -n "$1" get pod "$2" -o jsonpath='{.spec.nodeName}')
  k -n c4-system logs "$(node_pod "$node")" -c node | grep '"msg":"published"' | grep "\"pod\":\"$1/$2\"" | tail -1 |
    perl -MJSON::PP -ne '$j=decode_json($_); printf "%s %.3f %d\n", $j->{outcome}, $j->{seconds}, $j->{layers}'
}

# capture_info NS POD: "added modified deleted uploaded reused seconds" from the log.
capture_info() {
  local node
  node=$(k -n "$1" get pod "$2" -o jsonpath='{.spec.nodeName}')
  k -n c4-system logs "$(node_pod "$node")" -c node | grep '"msg":"captured"' | grep "\"pod\":\"$1/$2\"" | tail -1 |
    perl -MJSON::PP -ne '$j=decode_json($_); printf "%d %d %d %d %d %.3f\n", $j->{added}, $j->{modified}, $j->{deleted}, $j->{uploaded}, $j->{reused}, $j->{seconds}'
}

# annotation NS POD KEY
annotation() {
  k -n "$1" get pod "$2" -o go-template="{{index .metadata.annotations \"$3\"}}" 2>/dev/null | sed 's/<no value>//'
}

# wait_annotated NS POD [TIMEOUT]: until the capture annotation appears.
wait_annotated() {
  local i
  for i in $(seq 1 "${3:-120}"); do
    [ -n "$(annotation "$1" "$2" c4.avalanche.io/output-tree)" ] && return 0
    sleep 1
  done
  fail "pod $1/$2 was never annotated with its capture"
}

# mat_view NS NAME: wait until Ready, print the view ID.
mat_view() {
  k -n "$1" wait --for=jsonpath='{.status.phase}'=Ready "materialization/$2" --timeout=180s >/dev/null ||
    fail "materialization $1/$2: $(k -n "$1" get materialization "$2" -o jsonpath='{.status.conditions[*].message}')"
  k -n "$1" get materialization "$2" -o jsonpath='{.status.viewID}'
}

# cold_start: no pods, empty node and regional caches, fresh daemons, so a
# phase measures cold behavior and not leftovers from an earlier one.
cold_start() {
  local ns n
  for ns in show-a show-b herd-tiered herd-direct mono; do
    k -n "$ns" delete pods --all --wait=true >/dev/null 2>&1 || true
  done
  for n in $WORKERS; do
    docker exec "$n" sh -c 'rm -rf /var/lib/c4/p /var/lib/c4/vols'
  done
  k -n c4-system rollout restart ds/c4-node deploy/c4-regional >/dev/null
  k -n c4-system rollout status deploy/c4-regional --timeout=180s >/dev/null
  k -n c4-system rollout status ds/c4-node --timeout=300s >/dev/null
}

# store_gets_settled: store_gets once MinIO's metrics (cached for about ten
# seconds) stop changing.
store_gets_settled() {
  local a b
  sleep 12
  a=$(store_gets)
  while :; do
    sleep 6
    b=$(store_gets)
    [ "$a" = "$b" ] && break
    a=$b
  done
  echo "$b"
}

# regional_pod_metric POD NAME LABEL...: a series on one regional replica.
regional_pod_metric() {
  local p=$1
  shift
  k get --raw "/api/v1/namespaces/c4-system/pods/$p:7443/proxy/metrics" 2>/dev/null | sum_series "$@"
}
