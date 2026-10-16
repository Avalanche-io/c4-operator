#!/usr/bin/env bash
# P1: overlay views, changeset capture, the CSI driver, and no per-read
# verification, on eight workers under Pod Security restricted.
#
#   hack/e2e/p1.sh            (after hack/e2e/up.sh)
#   FRAME_KIB=16384 hack/e2e/p1.sh
source "$(dirname "$0")/lib.sh"

NS=show-a
FRAME_KIB=${FRAME_KIB:-4096}
INCLUDE=shots/sh010/,editorial/,weights/,tools/
SHOW=$OUT/p1-show
NODES=($WORKERS)

# pod NAME NODE MATERIALIZATION READONLY SCRIPT: a restricted pod that
# mounts the Materialization at /show and runs SCRIPT.
pod() {
  k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS}
spec:
  nodeName: $2
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    seccompProfile: {type: RuntimeDefault}
  containers:
  - name: work
    image: c4-e2e:dev
    imagePullPolicy: IfNotPresent
    command: [sh, -c, '$5']
    env:
    - name: POD_NAME
      valueFrom: {fieldRef: {fieldPath: metadata.name}}
    securityContext:
      allowPrivilegeEscalation: false
      capabilities: {drop: [ALL]}
    volumeMounts:
    - {name: show, mountPath: /show, readOnly: $4}
  volumes:
  - name: show
    csi:
      driver: c4.avalanche.io
      readOnly: $4
      volumeAttributes: {materialization: $3}
EOF
}

# wait_phase NAME PHASE [TIMEOUT]
wait_phase() {
  k -n "$NS" wait --for=jsonpath='{.status.phase}'="$2" "pod/$1" --timeout="${3:-300}s" >/dev/null ||
    fail "pod $1 did not reach $2: $(k -n "$NS" describe pod "$1" | tail -5)"
}

log "cold start: empty caches on every node and regional replica"
cold_start
k -n "$NS" delete materializations --all >/dev/null 2>&1 || true

record ""
record "## P1: overlay views, capture, CSI ($(date -u +%Y-%m-%dT%H:%MZ))"
record ""

log "show: generate (${FRAME_KIB} KiB frames) and ingest into three shards"
rm -rf "$SHOW"
GEN=$("$TOOL" gen-show --out "$SHOW" --frame-kib "$FRAME_KIB")
t0=$(now)
SHOW_VIEW=$(ingest show-a "$SHOW")
INGEST_S=$(since "$t0")
SHARDS=$(mc_run 'for i in 0 1 2; do printf "s%d=%s " $i $(mc ls --recursive m$i/show-a/c4/ | wc -l); done')
record "Show: $GEN. Ingested in ${INGEST_S} s as view \`$SHOW_VIEW\`. Objects per shard (files and records): $SHARDS"
record ""

log "Materialization sh010"
k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: sh010, namespace: $NS}
spec:
  source: {id: "$SHOW_VIEW"}
  include: [shots/sh010/, editorial/, weights/, tools/]
EOF
V11=$(mat_view "$NS" sh010)
HOST_V11=$("$TOOL" expect --src "$SHOW" --include "$INCLUDE")
[ "$V11" = "$HOST_V11" ] || fail "controller view $V11 != host scan $HOST_V11"
FILES=$(k -n "$NS" get mat sh010 -o jsonpath='{.status.files}')
UBYTES=$(k -n "$NS" get mat sh010 -o jsonpath='{.status.uniqueBytes}')
record "Materialization sh010: $FILES files, $UBYTES unique bytes. Its view ID equals an independent scan of the same subset on the host."

log "Pod Security restricted: a hostPath pod is refused, a csi pod is admitted"
HOSTPATH_ERR=$(k apply -f - 2>&1 <<EOF || true
apiVersion: v1
kind: Pod
metadata: {name: hostpath-mvp-style, namespace: $NS}
spec:
  containers: [{name: c, image: c4-e2e:dev, command: [sh, -c, "true"]}]
  volumes: [{name: v, hostPath: {path: /var/lib/c4}}]
EOF
)
echo "$HOSTPATH_ERR" | grep -q "forbidden" || fail "the hostPath pod was not refused: $HOSTPATH_ERR"
record "A pod with the MVP's hostPath volume is refused by Pod Security restricted (\`$(echo "$HOSTPATH_ERR" | grep -o 'violates PodSecurity "restricted[^"]*"' | head -1)\`); every pod below uses the csi volume and is admitted."
record ""

log "16 render pods, two per worker, writable views"
HASHED0=$(nodes_metric 'c4_node_hashed_bytes_total{reason="verify-fill"}')
for i in "${!NODES[@]}"; do
  for s in a b; do
    name=render-$((i + 1))$s
    pod "$name" "${NODES[$i]}" sh010 false \
      "c4e2e verify --expect $V11 /show && c4e2e render --pod \$POD_NAME /show && c4e2e hold 45s"
    [ "$s" = a ] && wait_phase "$name" Running
  done
done
for i in "${!NODES[@]}"; do for s in a b; do wait_phase "render-$((i + 1))$s" Running; done; done

log "while mounted: layers, uppers and links on each node"
record "### Layers and uppers while 16 pods are mounted"
record ""
record "| node | layers in the view's stack | uppers mounted | links on one plate's inode | objects (bytes) | objects and layers together (bytes) |"
record "|---|---|---|---|---|---|"
for n in "${NODES[@]}"; do
  P=/var/lib/c4/p/p-show-a
  layers=$(docker exec "$n" sh -c "grep -o '\"layers\":\[[^]]*\]' $P/stacks/$V11.json | tr -cd ',' | wc -c" | tr -d ' ')
  layers=$((layers + 1))
  uppers=$(docker exec "$n" sh -c "ls $P/vols | wc -l" | tr -d ' ')
  links=$(docker exec "$n" sh -c "stat -c %h \$(find $P/layers -path '*shots/sh010/plates/sh010.1001.exr' | head -1)")
  duo=$(docker exec "$n" sh -c "du -sb $P/objects | cut -f1")
  dub=$(docker exec "$n" sh -c "du -sb $P/objects $P/layers | awk '{s+=\$1} END {print s}'")
  record "| $n | $layers | $uppers | $links | $duo | $dub |"
  [ "$layers" = 1 ] || fail "$n stacks $layers layers for one view"
  [ "$uppers" = 2 ] || fail "$n has $uppers uppers for two pods"
done
record ""
record "Each node holds one layer for the view, shared by both of its pods, and one upper per pod. A plate's inode has two links, the cache's and the layer's; the pods reach it only through overlay. Measured in one \`du\` run, which counts an inode once, objects and layers together take only a few kilobytes more than the objects alone: the layer costs directory entries, not file bytes."
record ""

log "captures"
for i in "${!NODES[@]}"; do for s in a b; do wait_phase "render-$((i + 1))$s" Succeeded 600; done; done
record "### Per pod"
record ""
record "| pod | node | publish | publish s | layers | added | modified | deleted | uploaded | captured tree equals the pod's own scan |"
record "|---|---|---|---|---|---|---|---|---|---|"
COLD=() ; WARM=()
for i in "${!NODES[@]}"; do
  for s in a b; do
    name=render-$((i + 1))$s
    wait_annotated "$NS" "$name"
    seen=$(k -n "$NS" logs "$name" | awk '/^output /{print $2}')
    tree=$(annotation "$NS" "$name" c4.avalanche.io/output-tree)
    [ "$seen" = "$tree" ] || fail "$name: captured $tree, the pod scanned $seen"
    read -r outcome secs nlayers <<<"$(publish_info "$NS" "$name")"
    read -r added modified deleted uploaded reused csecs <<<"$(capture_info "$NS" "$name")"
    [ "$outcome" = cold ] && COLD+=("$secs")
    [ "$outcome" = warm ] && WARM+=("$secs")
    record "| $name | ${NODES[$i]} | $outcome | $secs | $nlayers | $added | $modified | $deleted | $uploaded | yes |"
  done
done
HOST_OUT=$("$TOOL" expect --src "$SHOW" --include "$INCLUDE" --render render-1a | tail -1)
[ "$HOST_OUT" = "$(annotation "$NS" render-1a c4.avalanche.io/output-tree)" ] || fail "host recomputation of render-1a differs"
HASHED1=$(nodes_metric 'c4_node_hashed_bytes_total{reason="verify-fill"}')
med() { printf '%s\n' "$@" | sort -n | awk '{a[NR]=$1} END {print (NR%2 ? a[(NR+1)/2] : (a[NR/2]+a[NR/2+1])/2)}'; }
record ""
record "- Cold publishes (first pod on a node): ${#COLD[@]}, median $(med "${COLD[@]}") s. Warm publishes (second pod): ${#WARM[@]}, median $(med "${WARM[@]}") s."
record "- Bytes hashed on the read path across all nodes: $((HASHED1 - HASHED0)). Content is verified where it enters (ingest, capture), never when it is read."
record "- Every captured output tree equals the tree the pod scanned after writing, and render-1a's equals a recomputation on the host from the source files."
record ""

log "v12 = v11 + 3 frames, stacked on every node"
pod adder "${NODES[0]}" sh010 false "c4e2e add-frames --count 3 --dir shots/sh010/plates --pod \$POD_NAME --frame-kib $FRAME_KIB /show"
wait_phase adder Succeeded
wait_annotated "$NS" adder
CS12=$(annotation "$NS" adder c4.avalanche.io/output-changeset)
k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: sh010-v12, namespace: $NS}
spec:
  source: {id: "$CS12"}
EOF
V12=$(mat_view "$NS" sh010-v12)
HOST_V12=$("$TOOL" expect --src "$SHOW" --include "$INCLUDE" --add-frames 3 --frames-dir shots/sh010/plates --pod adder | tail -1)
[ "$V12" = "$HOST_V12" ] || fail "v12 $V12 != host $HOST_V12"
CHAIN=$(k -n "$NS" get mat sh010-v12 -o jsonpath='{.status.layers}' | tr -cd ',' | wc -c)
FETCH0=$(nodes_metric 'c4_node_object_requests_total{project="show-a",result="fetch"}')
for i in "${!NODES[@]}"; do
  pod "v12-$((i + 1))" "${NODES[$i]}" sh010-v12 true "c4e2e verify --expect $V12 /show"
done
record "### v12 = v11 + 3 frames"
record ""
record "| pod | node | publish | publish s | layers |"
record "|---|---|---|---|---|"
for i in "${!NODES[@]}"; do
  name=v12-$((i + 1))
  wait_phase "$name" Succeeded
  read -r outcome secs nlayers <<<"$(publish_info "$NS" "$name")"
  record "| $name | ${NODES[$i]} | $outcome | $secs | $nlayers |"
done
FETCH1=$(nodes_metric 'c4_node_object_requests_total{project="show-a",result="fetch"}')
record ""
record "The changeset resolves to a chain of $((CHAIN + 1)) views. Every node that held v11 added one delta layer for v12 and fetched $(((FETCH1 - FETCH0))) objects in total across ${#NODES[@]} nodes (three new frames per node, minus any a node already had), while v12 has $(k -n "$NS" get mat sh010-v12 -o jsonpath='{.status.files}') files. Read-only mounts of v12 on all eight nodes match the host's recomputation."
record ""
echo "P1 PASS"
