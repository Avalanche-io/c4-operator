# Shared by the e2e scripts. Source it; do not run it.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
CLUSTER=${CLUSTER:-c4-operator-e2e}
CTX=kind-$CLUSTER
# E2E_PORT_OFFSET moves every port the farm publishes on this machine (the
# HUD, the signer, the three MinIO shards), so a second e2e cluster can run
# beside another. A multiple of 10; 0 keeps 30080, 30081 and 30900-30902.
PO=${E2E_PORT_OFFSET:-0}
if [ $((PO % 10)) -ne 0 ]; then echo "E2E_PORT_OFFSET must be a multiple of 10" >&2; exit 2; fi
HUD_PORT=$((30080 + PO))
SIGNER_PORT=$((30081 + PO))
s3port() { echo $((30900 + $1 + PO)); }
# an endpoint pattern for tools that add the shard index (0, 1, 2)
S3_ENDPOINT_FMT="http://127.0.0.1:$(((30900 + PO) / 10))%s"
OUT=$ROOT/hack/e2e/out
# The commit the binaries and images were built from, for the results.
BUILD=$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)
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

# built_from: the commit the checkout would build, -dirty with changes to
# what is built (hack/e2e/redeploy.sh stamps the manager with it).
built_from() {
  local v
  v=$(git -C "$ROOT" rev-parse --short=12 HEAD)
  [ -n "$(git -C "$ROOT" status --porcelain -- . ":!hack/demo/out" ":!hack/e2e/out")" ] && v=$v-dirty
  echo "$v"
}

record() { printf '%s\n' "$*" | tee -a "$RESULTS"; }
since() { perl -e "printf '%.2f', $(now) - $1"; }

# shard_args PROJECT HOST|CLUSTER: --shard flags for a project's bucket.
shard_args() {
  local bucket=$1 where=$2 i
  for i in 0 1 2; do
    if [ "$where" = host ]; then
      printf -- '--shard s%d=s3://%s/c4/?endpoint=http://127.0.0.1:%d ' "$i" "$bucket" "$(s3port "$i")"
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
    n=$(curl -sf "http://127.0.0.1:$(s3port "$i")/minio/v2/metrics/cluster" |
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
  for ns in show-a show-b herd-tiered herd-direct mono iso-a iso-b dept-1 dept-2 vendor; do
    k -n "$ns" delete pods --all --wait=true >/dev/null 2>&1 || true
  done
  for n in $WORKERS; do
    docker exec "$n" sh -c 'rm -rf /var/lib/c4/p /var/lib/c4/vols'
  done
  k -n c4-system rollout restart ds/c4-node deploy/c4-regional >/dev/null
  k -n c4-system rollout status deploy/c4-regional --timeout=180s >/dev/null
  k -n c4-system rollout status ds/c4-node --timeout=300s >/dev/null
}

# settled CMD...: CMD's output once two readings six seconds apart agree.
# MinIO caches its metrics for about ten seconds.
settled() {
  local a b
  sleep 12
  a=$("$@")
  while :; do
    sleep 6
    b=$("$@")
    [ "$a" = "$b" ] && break
    a=$b
  done
  echo "$b"
}

store_gets_settled() { settled store_gets; }

# bucket_requests BUCKET: "GETS HEADS" MinIO served for one bucket, summed
# over the three shards.
bucket_requests() {
  local i g=0 h=0 line
  for i in 0 1 2; do
    line=$(curl -sf "http://127.0.0.1:$(s3port "$i")/minio/v2/metrics/bucket" |
      awk -v b="bucket=\"$1\"" '
        /^minio_bucket_requests_total\{/ && index($0, b) && /api="getobject"/ { g += $NF }
        /^minio_bucket_requests_total\{/ && index($0, b) && /api="headobject"/ { h += $NF }
        END { printf "%d %d", g, h }')
    line=${line:-0 0}
    g=$((g + ${line% *})) h=$((h + ${line#* }))
  done
  echo "$g $h"
}

# mat_outcome NS NAME: once the Materialization is Ready or Failed,
# "PHASE REASON: MESSAGE".
mat_outcome() {
  local i ph
  for i in $(seq 1 120); do
    ph=$(k -n "$1" get materialization "$2" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    case "$ph" in Ready | Failed) break ;; esac
    sleep 1
  done
  k -n "$1" get materialization "$2" -o jsonpath='{.status.phase} {.status.conditions[?(@.type=="Ready")].reason}: {.status.conditions[?(@.type=="Ready")].message}'
}

# mount_error NS POD: the pod's latest FailedMount message, from the CSI
# driver's answer onward.
mount_error() {
  local i msg
  for i in $(seq 1 90); do
    msg=$(k -n "$1" get events --field-selector "involvedObject.name=$2,reason=FailedMount" -o jsonpath='{.items[-1:].message}' 2>/dev/null || true)
    if [ -n "$msg" ]; then
      echo "${msg#*desc = }"
      return 0
    fi
    sleep 1
  done
  fail "pod $1/$2 never reported FailedMount"
}

# regional_pod_metric POD NAME LABEL...: a series on one regional replica.
regional_pod_metric() {
  local p=$1
  shift
  k get --raw "/api/v1/namespaces/c4-system/pods/$p:7443/proxy/metrics" 2>/dev/null | sum_series "$@"
}

# farm_manager [POLICY]: run the manager with the e2e farm's settings: the
# kind-sim capacity provider, development HUD viewers, the farm's time zone.
farm_manager() {
  local policy=${1:-objective} want
  want="[\"manager\",\"--capacity-provider=kind-sim\",\"--work-policy=$policy\",\"--hud-dev-viewers=wrangler=*;artist=show-a\",\"--tz=America/Los_Angeles\"]"
  if [ "$(k -n c4-system get deploy c4-manager -o jsonpath='{.spec.template.spec.containers[0].args}' | tr -d ' ')" != "$want" ]; then
    k -n c4-system patch deploy c4-manager --type=json -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":$want}]" >/dev/null
  fi
  k -n c4-system rollout status deploy/c4-manager --timeout=180s >/dev/null
  local end=$((SECONDS + 180))
  until curl -sf "http://127.0.0.1:$HUD_PORT/api/v1/snapshot?as=wrangler" >/dev/null; do
    [ $SECONDS -lt $end ] || { echo "the HUD did not answer on 127.0.0.1:$HUD_PORT in 180s (is hud.yaml applied?)" >&2; return 1; }
    sleep 1
  done
}
