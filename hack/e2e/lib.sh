# Shared by the e2e scripts. Source it; do not run it.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
CLUSTER=${CLUSTER:-c4-operator-e2e}
CTX=kind-$CLUSTER
# E2E_PORT_OFFSET moves every port the farm publishes on this machine (the
# HUD, the signer, the Garage instances' S3 and admin APIs), so a second e2e
# cluster can run beside another. A multiple of 10. The default, 1000, puts
# the HUD on 31080, the signer on 31081 and the shards' S3 APIs on
# 31900-31902 (their admin APIs on 31910-31912, P3's instances on
# 31920-31923 and 31930-31933), outside the ports a person's `c4 farm up`
# takes (30080 and 30900), so the two never collide; 0 keeps 30080, 30081
# and 30900-30902.
PO=${E2E_PORT_OFFSET:-1000}
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
# The store's root key, on every instance and every bucket (Garage has no
# superuser: each bucket grants it). Key IDs are GK and 24 hex digits,
# secrets 64 hex digits. hack/e2e/migrate.sh reads this line from the
# checkout whose harness made the farm.
export AWS_ACCESS_KEY_ID=GKe2e2e2e2e2e2e2e2e2e2e2e2 AWS_SECRET_ACCESS_KEY=e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2
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

# The store is Garage (the image `c4 farm up` runs). garage-0, garage-1 and
# garage-2 are the three shards every project but P3's uses, each holding
# one bucket per project. P3's projects each have their own instance
# (garage-iso-a, ...), one shard: Garage counts requests per instance, not
# per bucket, so a project's own store counting what it served is the
# store itself counting. Each instance is one node (sqlite, replication 1)
# on the control plane, with its S3 and admin APIs on NodePorts the kind
# config maps to this machine.
GARAGE_IMAGE=dxflrs/garage:v2.4.1
GARAGE_TOKEN=c4-e2e-admin
P3_STORES="iso-a iso-b dept-1 dept-2"

# garage_nodeports NAME: the instance's S3 and admin NodePorts.
garage_nodeports() {
  case $1 in
    garage-[0-2]) echo "$((30900 + ${1#garage-})) $((30910 + ${1#garage-}))" ;;
    garage-iso-a) echo "30920 30930" ;;
    garage-iso-b) echo "30921 30931" ;;
    garage-dept-1) echo "30922 30932" ;;
    garage-dept-2) echo "30923 30933" ;;
    *) fail "no Garage instance $1" ;;
  esac
}
# garage_port NAME s3|admin: where this machine reaches it.
garage_port() {
  local p
  read -r -a p <<<"$(garage_nodeports "$1")"
  if [ "$2" = s3 ]; then echo $((p[0] + PO)); else echo $((p[1] + PO)); fi
}
# garage_url NAME: where the cluster reaches its S3 API.
garage_url() { echo "http://$1.c4-store:3900"; }

# garage_yaml NAME: the instance's Deployment and NodePort Service.
garage_yaml() {
  local np
  read -r -a np <<<"$(garage_nodeports "$1")"
  cat <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: $1, namespace: c4-store}
spec:
  replicas: 1
  selector: {matchLabels: {app: garage, instance: $1}}
  template:
    metadata: {labels: {app: garage, instance: $1}}
    spec:
      nodeSelector: {node-role.kubernetes.io/control-plane: ""}
      tolerations: [{operator: Exists}]
      containers:
      - name: garage
        image: $GARAGE_IMAGE
        imagePullPolicy: IfNotPresent
        ports: [{containerPort: 3900}, {containerPort: 3903}]
        volumeMounts:
        - {name: config, mountPath: /etc/garage.toml, subPath: garage.toml}
        - {name: data, mountPath: /var/lib/garage}
      volumes:
      - {name: config, configMap: {name: garage}}
      - {name: data, emptyDir: {}}
---
apiVersion: v1
kind: Service
metadata: {name: $1, namespace: c4-store}
spec:
  type: NodePort
  selector: {app: garage, instance: $1}
  ports:
  - {name: s3, port: 3900, nodePort: ${np[0]}}
  - {name: admin, port: 3903, nodePort: ${np[1]}}
EOF
}

# garage_admin NAME CALL [JSON]: one admin API v2 call, from this machine.
garage_admin() {
  curl -sf -m 30 -H "Authorization: Bearer $GARAGE_TOKEN" -H 'Content-Type: application/json' \
    "http://127.0.0.1:$(garage_port "$1" admin)/v2/$2" ${3:+-d "$3"}
}

# garage_up NAME...: each instance running, its layout applied (one node,
# one zone), and the root key on it. Running it again changes nothing.
garage_up() {
  local n node ver
  k apply -f "$ROOT/hack/e2e/garage.yaml" >/dev/null
  for n in "$@"; do garage_yaml "$n" | k apply -f - >/dev/null; done
  for n in "$@"; do
    k -n c4-store rollout status "deploy/$n" --timeout=180s >/dev/null || fail "$n did not start"
    until garage_admin "$n" GetClusterStatus >/dev/null; do sleep 1; done
    if [ "$(garage_admin "$n" GetClusterLayout | jq '.roles | length')" = 0 ]; then
      node=$(garage_admin "$n" GetClusterStatus | jq -r '.nodes[0].id')
      garage_admin "$n" UpdateClusterLayout "{\"roles\":[{\"id\":\"$node\",\"zone\":\"e2e\",\"capacity\":100000000000,\"tags\":[]}]}" >/dev/null
      ver=$(garage_admin "$n" GetClusterLayout | jq '.version + 1')
      garage_admin "$n" ApplyClusterLayout "{\"version\":$ver}" >/dev/null || fail "$n: the layout"
    fi
    until curl -sf -m 5 -o /dev/null "http://127.0.0.1:$(garage_port "$n" admin)/health"; do sleep 1; done
    garage_key "$n" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" root
  done
}

# garage_key NAME ID SECRET LABEL: the key on the instance.
garage_key() {
  garage_admin "$1" "GetKeyInfo?id=$2" >/dev/null ||
    garage_admin "$1" ImportKey "{\"accessKeyId\":\"$2\",\"secretAccessKey\":\"$3\",\"name\":\"$4\"}" >/dev/null ||
    fail "$1: key $4"
}

# garage_bucket NAME BUCKET [KEY...]: the bucket on the instance, the root
# key allowed everything on it, and each KEY reading and writing it.
garage_bucket() {
  local n=$1 b=$2 id key
  shift 2
  if ! id=$(garage_admin "$n" "GetBucketInfo?globalAlias=$b" | jq -r .id); then
    id=$(garage_admin "$n" CreateBucket "{\"globalAlias\":\"$b\"}" | jq -r .id)
  fi
  [ -n "$id" ] && [ "$id" != null ] || fail "$n: bucket $b"
  garage_admin "$n" AllowBucketKey "{\"bucketId\":\"$id\",\"accessKeyId\":\"$AWS_ACCESS_KEY_ID\",\"permissions\":{\"read\":true,\"write\":true,\"owner\":true}}" >/dev/null ||
    fail "$n: bucket $b for the root key"
  for key in "$@"; do
    garage_admin "$n" AllowBucketKey "{\"bucketId\":\"$id\",\"accessKeyId\":\"$key\",\"permissions\":{\"read\":true,\"write\":true,\"owner\":false}}" >/dev/null ||
      fail "$n: bucket $b for $key"
  done
}

# wipe BUCKET [NAME...]: every object out of the bucket on each instance
# (the three shards by default). Garage has no call that empties one.
wipe() {
  local b=$1 n
  shift
  for n in ${@:-garage-0 garage-1 garage-2}; do
    "$TOOL" s3keys --endpoint "http://127.0.0.1:$(garage_port "$n" s3)" --bucket "$b" --delete >/dev/null || fail "wipe $b on $n"
  done
}

# count_keys NAME BUCKET: the objects the bucket holds on the instance.
count_keys() { "$TOOL" s3keys --endpoint "http://127.0.0.1:$(garage_port "$1" s3)" --bucket "$2"; }

# shard_args PROJECT HOST|CLUSTER: --shard flags for a project's bucket.
shard_args() {
  local bucket=$1 where=$2 i
  for i in 0 1 2; do
    if [ "$where" = host ]; then
      printf -- '--shard s%d=s3://%s/c4/?endpoint=http://127.0.0.1:%d ' "$i" "$bucket" "$(garage_port "garage-$i" s3)"
    else
      printf -- '--shard s%d=s3://%s/c4/?endpoint=%s ' "$i" "$bucket" "$(garage_url "garage-$i")"
    fi
  done
}

# shards_yaml BUCKET [NAME]: the Project's shard list: the three shards, or
# one, the instance NAME.
shards_yaml() {
  local bucket=$1 i
  if [ -n "${2:-}" ]; then
    printf '    - {name: s0, url: "s3://%s/c4/?endpoint=%s"}\n' "$bucket" "$(garage_url "$2")"
    return
  fi
  for i in 0 1 2; do
    printf '    - {name: s%d, url: "s3://%s/c4/?endpoint=%s"}\n' "$i" "$bucket" "$(garage_url "garage-$i")"
  done
}

# project_host_shards PROJECT: --shard flags for a Project's store as this
# machine reaches it, from its spec (hack/e2e/migrate.sh calls it).
project_host_shards() {
  local line name url g
  k get project "$1" -o jsonpath='{range .spec.store.shards[*]}{.name}={.url}{"\n"}{end}' | while IFS= read -r line; do
    name=${line%%=*} url=${line#*=}
    for g in garage-0 garage-1 garage-2 garage-iso-a garage-iso-b garage-dept-1 garage-dept-2; do
      url=${url/$(garage_url "$g")/http://127.0.0.1:$(garage_port "$g" s3)}
    done
    printf -- '--shard %s=%s ' "$name" "$url"
  done
}

# ingest BUCKET DIR [NAME]: hash DIR into the bucket's three shards, or the
# one on instance NAME; prints the view ID.
ingest() {
  if [ -n "${3:-}" ]; then
    "$OP" ingest -q --shard "s0=s3://$1/c4/?endpoint=http://127.0.0.1:$(garage_port "$3" s3)" "$2"
    return
  fi
  # shellcheck disable=SC2046
  "$OP" ingest -q $(shard_args "$1" host) "$2"
}

# instance_requests NAME: "GETS HEADS" the instance's S3 API has served,
# refused ones included (Garage counts every request it answers).
instance_requests() {
  curl -sf -m 30 "http://127.0.0.1:$(garage_port "$1" admin)/metrics" | awk '
    /^api_s3_request_counter\{/ && /api_endpoint="GetObject"/ { g += $NF }
    /^api_s3_request_counter\{/ && /api_endpoint="HeadObject"/ { h += $NF }
    END { printf "%d %d", g, h }'
}

# store_gets: GetObject requests served by the three shards so far.
store_gets() {
  local total=0 i line
  for i in 0 1 2; do
    line=$(instance_requests "garage-$i")
    total=$((total + ${line% *}))
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

# metrics_token: an API token that may get /metrics (the manager's
# service account is bound to c4-metrics-reader).
metrics_token() {
  k -n c4-system create token c4-manager --duration 10m
}

# node_metric NODE NAME LABEL...: a series on one node daemon.
node_metric() {
  local n=$1
  shift
  docker exec "$n" curl -sf -H "Authorization: Bearer $(metrics_token)" localhost:7444/metrics | sum_series "$@"
}

# regional_metrics POD: a regional replica's /metrics, read from a worker
# (the API server's pod proxy carries no token).
regional_metrics() {
  local ip
  ip=$(k -n c4-system get pod "$1" -o jsonpath='{.status.podIP}')
  docker exec "${WORKERS%% *}" curl -sf -H "Authorization: Bearer $(metrics_token)" "http://$ip:7443/metrics"
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
    v=$(regional_metrics "$p" 2>/dev/null | sum_series "$@")
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

# settled CMD...: CMD's output once two readings three seconds apart
# agree, so requests still in flight are counted.
settled() {
  local a b
  sleep 3
  a=$("$@")
  while :; do
    sleep 3
    b=$("$@")
    [ "$a" = "$b" ] && break
    a=$b
  done
  echo "$b"
}

store_gets_settled() { settled store_gets; }

# bucket_requests BUCKET: "GETS HEADS" P3's project BUCKET's own store has
# served (its instance holds only its bucket).
bucket_requests() { instance_requests "garage-$1"; }

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
  regional_metrics "$p" 2>/dev/null | sum_series "$@"
}

# farm_manager [POLICY]: run the manager with the e2e farm's settings: the
# kind-sim capacity provider, development HUD viewers, the farm's time zone.
farm_manager() {
  local policy=${1:-objective} want
  # a test harness: open development viewers, anyone who reaches the HUD is any viewer
  want="[\"manager\",\"--capacity-provider=kind-sim\",\"--work-policy=$policy\",\"--hud-dev-viewers=wrangler=*;artist=show-a\",\"--hud-dev-viewers-open\",\"--tz=America/Los_Angeles\"]"
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
