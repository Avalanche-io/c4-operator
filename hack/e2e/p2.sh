#!/usr/bin/env bash
# P2: tiers. Thundering herds of pods reading one shot at once, measured
# where it counts (GetObject requests the MinIO shards themselves served),
# for the tiered path, the Direct path (nodes read the store, each node
# coalescing its own pods), and per-pod fetching (what init containers that
# copy from S3 do). Then a regional replica is killed in the middle of a
# herd.
#
#   hack/e2e/p2.sh                       (after hack/e2e/up.sh)
#   SIZES="8 32 64" FRAMES=24 FRAME_KIB=8192 hack/e2e/p2.sh
source "$(dirname "$0")/lib.sh"

SIZES=${SIZES:-"8 32 64"}
FRAMES=${FRAMES:-24}
FRAME_KIB=${FRAME_KIB:-8192}
NODES=($WORKERS)
CSV=$OUT/p2-herd.csv

# herd NS MODE P VIEW NAME: P pods spread evenly over the workers, created in
# one apply, each reading the whole shot; waits for all of them.
herd() {
  local ns=$1 mode=$2 p=$3 view=$4 name=$5 i spec=$OUT/p2-pods.yaml
  : >"$spec"
  for i in $(seq 1 "$p"); do
    local node=${NODES[$(((i - 1) % ${#NODES[@]}))]}
    if [ "$mode" = naive ]; then
      cat >>"$spec" <<EOF
---
apiVersion: v1
kind: Pod
metadata: {name: $name-$i, namespace: $ns, labels: {herd: "$name"}}
spec:
  nodeName: $node
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: copy
    image: c4-e2e:dev
    imagePullPolicy: IfNotPresent
    command: [c4e2e, s3get, --list, /objects/list, --bucket, herd-direct]
    envFrom: [{secretRef: {name: naive-s3}}]
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
    volumeMounts: [{name: objects, mountPath: /objects}]
  volumes: [{name: objects, configMap: {name: $name-objects}}]
EOF
      continue
    fi
    cat >>"$spec" <<EOF
---
apiVersion: v1
kind: Pod
metadata: {name: $name-$i, namespace: $ns, labels: {herd: "$name"}}
spec:
  nodeName: $node
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: read
    image: c4-e2e:dev
    imagePullPolicy: IfNotPresent
    command: [c4e2e, verify, --expect, "$view", /show]
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
    volumeMounts: [{name: show, mountPath: /show, readOnly: true}]
  volumes:
  - name: show
    csi: {driver: c4.avalanche.io, readOnly: true, volumeAttributes: {materialization: $name}}
EOF
  done
  k apply -f "$spec" >/dev/null
  k -n "$ns" wait --for=jsonpath='{.status.phase}'=Succeeded pod -l "herd=$name" --timeout=900s >/dev/null ||
    fail "herd $name did not finish: $(k -n "$ns" get pods -l herd=$name --no-headers | grep -v Completed | head -3)"
}

# shot NAME SEED BUCKET: generate and ingest a shot with unique content;
# prints the view ID and leaves the object list in $OUT/NAME.objects.
shot() {
  local dir=$OUT/p2-$1
  rm -rf "$dir"
  "$TOOL" gen-shot --out "$dir" --name "$1" --frames "$FRAMES" --frame-kib "$FRAME_KIB" --seed "$2" >/dev/null
  "$TOOL" objects --src "$dir" >"$OUT/$1.objects"
  ingest "$3" "$dir"
}

materialize() { # NS NAME VIEW
  k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: $2, namespace: $1}
spec:
  source: {id: "$3"}
EOF
  mat_view "$1" "$2"
}

record ""
record "## P2: tiers and thundering herds ($(date -u +%Y-%m-%dT%H:%MZ))"
record ""
k -n herd-direct create secret generic naive-s3 \
  --from-literal=AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" --from-literal=AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" \
  --dry-run=client -o yaml | k apply -f - >/dev/null
for ns in herd-tiered herd-direct; do k -n "$ns" delete materializations --all >/dev/null 2>&1 || true; done

record "Each herd reads a shot of $FRAMES frames of $((FRAME_KIB / 1024)) MiB, unique content per run, so every run starts cold for that shot. Eight workers in two localities; three regional replicas; three MinIO shards. Store GETs are counted by MinIO itself (\`minio_s3_requests_total{api=\"getobject\"}\`). Objects per shot include the directory records nodes fetch to resolve the view."
record ""
record "| path | pods | objects | store GETs | store GETs per object | regional requests per object | peer requests per object | wall s |"
record "|---|---|---|---|---|---|---|---|"
echo "path,pods,objects,store_gets,store_gets_per_object,regional_requests,peer_requests,wall_s" >"$CSV"

run() { # MODE P
  local mode=$1 p=$2 ns bucket name view m g0 g1 r0 r1 q0 q1 t0 wall
  case "$mode" in
    tiered) ns=herd-tiered bucket=herd-tiered ;;
    *) ns=herd-direct bucket=herd-direct ;;
  esac
  name=$mode$p
  log "herd: $mode, $p pods"
  # Every herd starts cold: node caches wiped, regional caches emptied.
  cold_start
  view=$(shot "$name" "$name-$RANDOM" "$bucket")
  m=$(wc -l <"$OUT/$name.objects" | tr -d ' ')
  if [ "$mode" = naive ]; then
    k -n "$ns" create configmap "$name-objects" --from-file=list="$OUT/$name.objects" --dry-run=client -o yaml | k apply -f - >/dev/null
  else
    materialize "$ns" "$name" "$view" >/dev/null
  fi
  g0=$(store_gets_settled)
  r0=$(regional_metric c4_tier_requests_total 'tier="regional"')
  q0=$(nodes_metric c4_tier_requests_total 'tier="node"')
  t0=$(now)
  herd "$ns" "$mode" "$p" "$view" "$name"
  wall=$(since "$t0")
  g1=$(store_gets_settled)
  r1=$(regional_metric c4_tier_requests_total 'tier="regional"')
  q1=$(nodes_metric c4_tier_requests_total 'tier="node"')
  local g=$((g1 - g0)) r=$((r1 - r0)) q=$((q1 - q0))
  record "| $mode | $p | $m | $g | $(perl -e "printf '%.2f', $g/$m") | $(perl -e "printf '%.2f', $r/$m") | $(perl -e "printf '%.2f', $q/$m") | $wall |"
  echo "$mode,$p,$m,$g,$(perl -e "printf '%.3f', $g/$m"),$r,$q,$wall" >>"$CSV"
  k -n "$ns" delete pods -l "herd=$name" --wait=true >/dev/null
  k -n "$ns" delete materialization "$name" --ignore-not-found >/dev/null
  rm -rf "$OUT/p2-$name"
}

for p in $SIZES; do run tiered "$p"; done
for p in $SIZES; do run direct "$p"; done
for p in $SIZES; do run naive "$p"; done
record ""
record "Store GETs per object stay flat on the tiered path as pods grow: each object is fetched from the store by at most its two regional homes (r = 2), and each locality's home fetches it once from the regional tier. On the Direct path every node reads the store once per object (node-level coalescing only), and per-pod fetching reads it once per pod."
record ""

log "kill a regional replica in the middle of a herd"
cold_start
k -n c4-system patch deploy c4-regional --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--max-rate=8Mi"}]' >/dev/null
k -n c4-system rollout status deploy/c4-regional --timeout=180s >/dev/null
sleep 5
OLD_FRAMES=$FRAMES OLD_KIB=$FRAME_KIB
FRAMES=8 FRAME_KIB=65536
name=kill32
view=$(shot "$name" "$name-$RANDOM" herd-tiered)
m=$(wc -l <"$OUT/$name.objects" | tr -d ' ')
materialize herd-tiered "$name" "$view" >/dev/null
g0=$(store_gets_settled)
res0=$(nodes_metric c4_tier_resumes_total 'tier="node"')
direct0=$(nodes_metric c4_tier_upstream_requests_total 'tier="node"' 'upstream="store"')
REPLICAS=$(k -n c4-system get pods -l app.kubernetes.io/component=regional --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}')
declare_served() { for rp in $REPLICAS; do printf '%s %s\n' "$rp" "$(regional_pod_metric "$rp" c4_tier_served_bytes_total 'tier="regional"')"; done; }
BASE_SERVED=$(declare_served)
t0=$(now)
herd herd-tiered tiered 32 "$view" "$name" &
HERD=$!
# Wait until the replicas are streaming objects, then kill the busiest.
VICTIM=""
for i in $(seq 1 120); do
  sleep 0.5
  NOW_SERVED=$(declare_served)
  VICTIM=$(join <(echo "$BASE_SERVED" | sort) <(echo "$NOW_SERVED" | sort) | awk '{d=$3-$2; if (d>best) {best=d; v=$1}} END {if (best > 32*1048576) print v}')
  [ -n "$VICTIM" ] && break
done
[ -n "$VICTIM" ] || fail "the regional replicas never started streaming"
KILLED_AT=$(since "$t0")
k -n c4-system delete pod "$VICTIM" --grace-period=0 --force >/dev/null 2>&1
wait "$HERD" || fail "the herd did not complete after the regional replica was killed"
wall=$(since "$t0")
g1=$(store_gets_settled)
res1=$(nodes_metric c4_tier_resumes_total 'tier="node"')
direct1=$(nodes_metric c4_tier_upstream_requests_total 'tier="node"' 'upstream="store"')
FRAMES=$OLD_FRAMES FRAME_KIB=$OLD_KIB
record "### A regional replica killed mid-herd"
record ""
record "32 pods read a shot of 8 frames of 64 MiB ($m objects with records) through the regional tier, paced at 8 MiB/s per response so transfers last seconds. At ${KILLED_AT} s, once it had streamed more than 32 MiB, the busiest regional replica, \`$VICTIM\`, was force-deleted (SIGKILL, no grace)."
record ""
record "| measure | value |"
record "|---|---|"
record "| pods that completed | 32 of 32, in ${wall} s |"
record "| store GETs | $((g1 - g0)) for $m objects ($(perl -e "printf '%.2f', ($g1-$g0)/$m") per object) |"
record "| transfers resumed from their offset on another replica | $((res1 - res0)) |"
record "| node requests that went to the store directly | $((direct1 - direct0)) |"
record ""
k -n c4-system patch deploy c4-regional --type=json -p='[{"op":"remove","path":"/spec/template/spec/containers/0/args/4"}]' >/dev/null || true
k -n c4-system rollout status deploy/c4-regional --timeout=180s >/dev/null
k -n herd-tiered delete pods --all --wait=true >/dev/null
k -n herd-tiered delete materialization "$name" --ignore-not-found >/dev/null
rm -rf "$OUT/p2-$name"
# Free the space the herds used on the shared Docker disk.
cold_start
mc_run "for i in 0 1 2; do mc rm --recursive --force m\$i/herd-tiered m\$i/herd-direct >/dev/null 2>&1 || true; done"
echo "P2 PASS"
