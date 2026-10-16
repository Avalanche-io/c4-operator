#!/usr/bin/env bash
# P3: isolation between projects (DESIGN.md section 6). Two Strict projects
# with their own buckets and MinIO users, two Shared projects in one sharing
# group, and a vendor project that shares the show's store but may
# materialize one shot of one version. Each way section 6.2 says project B
# cannot read project A's data is attempted for real, from B's namespace.
#
#   hack/e2e/p3.sh        (after hack/e2e/up.sh)
source "$(dirname "$0")/lib.sh"

NODE=$CLUSTER-worker3
POL=$OUT/p3-policies
P3NS="iso-a iso-b dept-1 dept-2 vendor"

# p3_project NAME BUCKET ISOLATION [GROUP]: a Tiered project for namespace
# NAME with its own MinIO user.
p3_project() {
  k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Project
metadata: {name: $1}
spec:
  namespaces: [$1]
  isolation: $3
  sharingGroup: "${4:-}"
  store:
    access: Tiered
    credentialsSecretRef: {name: store-$1}
    shards:
$(shards_yaml "$2")
EOF
}

# mat NS NAME SOURCE [INCLUDE]: SOURCE is a view ID or cm:CONFIGMAP.
mat() {
  local src
  case "$3" in
    cm:*) src="configMapKeyRef: {name: ${3#cm:}, key: tree.c4m}" ;;
    *) src="id: \"$3\"" ;;
  esac
  k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: $2, namespace: $1}
spec:
  source: {$src}
  include: [${4:-}]
EOF
}

# reader NS NAME MAT EXPECT [HOLD]: a pod on $NODE that verifies its mount
# is the view EXPECT, then, with HOLD, stays up that long.
reader() {
  local cmd="c4e2e verify --expect $4 /show"
  if [ -n "${5:-}" ]; then
    cmd="$cmd && echo held && exec c4e2e hold $5"
  fi
  k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $2, namespace: $1}
spec:
  nodeName: $NODE
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: read
    image: c4-e2e:dev
    imagePullPolicy: IfNotPresent
    command: [sh, -c, "$cmd"]
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
    volumeMounts: [{name: show, mountPath: /show, readOnly: true}]
  volumes:
  - name: show
    csi: {driver: c4.avalanche.io, readOnly: true, volumeAttributes: {materialization: $3}}
EOF
}

done_ok() {
  k -n "$1" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$2" --timeout=180s >/dev/null ||
    fail "pod $1/$2: $(k -n "$1" get pod "$2" --no-headers) $(k -n "$1" logs "$2" 2>&1 | tail -2)"
}

held() {
  local i
  for i in $(seq 1 180); do
    k -n "$1" logs "$2" 2>/dev/null | grep -q '^held' && return 0
    sleep 1
  done
  fail "pod $1/$2 did not verify its mount: $(k -n "$1" get pod "$2" --no-headers)"
}

# expect_outcome NS NAME PREFIX: the Materialization's outcome must start
# with PREFIX; prints the outcome.
expect_outcome() {
  local out
  out=$(mat_outcome "$1" "$2")
  case "$out" in
    "$3"*) echo "$out" ;;
    *) fail "materialization $1/$2: want $3, have $out" ;;
  esac
}

# inode NODE PARTITION ID: the inode of an object in a node's cache
# partition, or "none".
inode() {
  docker exec "$1" sh -c "stat -c %i /var/lib/c4/p/$2/objects/*/$3 2>/dev/null || echo none"
}

# cut_msg TEXT: one table cell: C4 IDs shortened to their first ten
# characters, at most 220 characters, no pipes.
cut_msg() {
  printf '%s' "$1" | sed -E 's/(c4[1-9A-HJ-NP-Za-km-z]{8})[1-9A-HJ-NP-Za-km-z]{80}/\1.../g' | cut -c1-220 | tr '|' '/'
}
short() { printf '%s...' "$(printf '%s' "$1" | cut -c1-10)"; }
# pub NS POD: the publish outcome as a phrase.
pub() { publish_info "$1" "$2" | awk '{printf "%s, %.3f s, %d layer%s", $1, $2, $3, ($3 == 1 ? "" : "s")}'; }

record ""
record "## P3: isolation ($(date -u +%Y-%m-%dT%H:%MZ), build $BUILD)"
record ""
cold_start
for ns in $P3NS; do
  k -n "$ns" delete materializations --all >/dev/null 2>&1 || true
  k -n "$ns" delete configmaps -l c4e2e=p3 >/dev/null 2>&1 || true
done

log "stores: one bucket and one MinIO user per project"
rm -rf "$POL"
mkdir -p "$POL"
for b in iso-a iso-b dept-1 dept-2; do
  printf '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::%s","arn:aws:s3:::%s/*"]}]}\n' "$b" "$b" >"$POL/$b.json"
done
docker run --rm --network kind -v "$POL:/p:ro" --entrypoint sh minio/mc:latest -c '
set -e
for i in 0 1 2; do
  mc alias set m$i http://'"$CLUSTER"'-control-plane:3090$i '"$AWS_ACCESS_KEY_ID $AWS_SECRET_ACCESS_KEY"' >/dev/null
  for b in iso-a iso-b dept-1 dept-2; do
    mc mb --ignore-existing m$i/$b >/dev/null
    mc rm --recursive --force m$i/$b >/dev/null 2>&1 || true
    mc admin policy create m$i rw-$b /p/$b.json >/dev/null
    mc admin user add m$i $b $b-e2e-secret >/dev/null
    mc admin policy attach m$i rw-$b --user $b >/dev/null 2>&1 || true
  done
  mc admin user add m$i vendor vendor-e2e-secret >/dev/null
  mc admin policy attach m$i rw-iso-a --user vendor >/dev/null 2>&1 || true
done' >/dev/null
for u in iso-a iso-b dept-1 dept-2 vendor; do
  k -n c4-system create secret generic "store-$u" --from-literal=AWS_ACCESS_KEY_ID="$u" \
    --from-literal=AWS_SECRET_ACCESS_KEY="$u-e2e-secret" --dry-run=client -o yaml | k apply -f - >/dev/null
done
for ns in $P3NS; do
  k create ns "$ns" --dry-run=client -o yaml | k apply -f - >/dev/null
  k label ns "$ns" pod-security.kubernetes.io/enforce=restricted --overwrite >/dev/null
done
p3_project iso-a iso-a Strict
p3_project iso-b iso-b Strict
p3_project dept-1 dept-1 Shared show-x
p3_project dept-2 dept-2 Shared show-x

log "content"
SHOW_DIR=$OUT/p3-show SECRET_DIR=$OUT/p3-sh900 D1_DIR=$OUT/p3-sh500
rm -rf "$SHOW_DIR" "$SECRET_DIR" "$D1_DIR"
"$TOOL" gen-show --out "$SHOW_DIR" --frame-kib 256 >/dev/null
"$TOOL" gen-shot --out "$SECRET_DIR" --name sh900 --frames 4 --frame-kib 256 --seed p3-unreleased >/dev/null
"$TOOL" gen-shot --out "$D1_DIR" --name sh500 --frames 4 --frame-kib 256 --seed p3-dept-1 >/dev/null
SHOW=$(ingest iso-a "$SHOW_DIR")
for b in iso-b dept-1 dept-2; do
  [ "$(ingest "$b" "$SHOW_DIR")" = "$SHOW" ] || fail "the show has a different ID in $b"
done
SECRET=$(ingest iso-a "$SECRET_DIR")
D1ONLY=$(ingest dept-1 "$D1_DIR")
"$TOOL" objects --src "$SECRET_DIR" >"$OUT/p3-sh900.objects"
"$TOOL" expect --src "$SECRET_DIR" --c4m "$OUT/p3-sh900.c4m" >/dev/null
"$TOOL" expect --src "$D1_DIR" --c4m "$OUT/p3-sh500.c4m" >/dev/null
SUB=$("$TOOL" expect --src "$SHOW_DIR" --include shots/sh010/ --c4m "$OUT/p3-sh010.c4m")
SECRET_OBJ=$("$TOOL" tree-id "$(find "$SECRET_DIR" -name '*.exr' | sort | head -1)")
PLATE=$("$TOOL" tree-id "$SHOW_DIR/shots/sh010/plates/sh010.1001.exr")
k wait --for=condition=Ready project iso-a iso-b dept-1 dept-2 --timeout=60s >/dev/null

record "Projects \`iso-a\` and \`iso-b\` are Strict, each with its own bucket and its own MinIO user whose policy allows only that bucket. \`dept-1\` and \`dept-2\` are Shared in sharing group \`show-x\`, also with their own buckets and users. The same show (134 files) is in all four buckets and has the same view ID in each, $SHOW. Shot sh900 is only in \`iso-a\`'s bucket (A's unreleased work); shot sh500 is only in \`dept-1\`'s. Every attempt below is made from project B's side."
record ""

log "1. the stores"
out_b=$(AWS_ACCESS_KEY_ID=iso-b AWS_SECRET_ACCESS_KEY=iso-b-e2e-secret "$TOOL" s3get --list "$OUT/p3-sh900.objects" --bucket iso-a --endpoint-fmt 'http://127.0.0.1:3090%s' 2>&1 || true)
case "$out_b" in
  *403* | *AccessDenied* | *"Access Denied"*) ;;
  *) fail "iso-b's credentials read iso-a's bucket: $out_b" ;;
esac
AWS_ACCESS_KEY_ID=iso-a AWS_SECRET_ACCESS_KEY=iso-a-e2e-secret "$TOOL" s3get --list "$OUT/p3-sh900.objects" --bucket iso-a --endpoint-fmt 'http://127.0.0.1:3090%s' >/dev/null ||
  fail "iso-a cannot read its own bucket"

log "2. by ID and by hand-written c4m"
mat iso-b steal-by-id "$SECRET"
r_id=$(expect_outcome iso-b steal-by-id "Failed ResolveFailed")
k -n iso-b create configmap stolen --from-file=tree.c4m="$OUT/p3-sh900.c4m" --dry-run=client -o yaml |
  k label --local -f - c4e2e=p3 -o yaml | k apply -f - >/dev/null
mat iso-b steal-by-c4m cm:stolen
r_c4m=$(expect_outcome iso-b steal-by-c4m "Failed ContentMissing")

log "3. the cache tiers' APIs"
k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: prober, namespace: iso-b}
spec:
  nodeName: $NODE
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: probe
    image: c4-e2e:dev
    imagePullPolicy: IfNotPresent
    command: [c4e2e, hold, 30m]
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
    volumeMounts: [{name: tier-token, mountPath: /var/run/tier, readOnly: true}]
  volumes:
  - name: tier-token
    projected:
      sources: [{serviceAccountToken: {audience: c4-tier, path: token, expirationSeconds: 3600}}]
EOF
k -n iso-b wait --for=condition=Ready pod/prober --timeout=120s >/dev/null
HOST_IP=$(k -n iso-b get pod prober -o jsonpath='{.status.hostIP}')
probe() { k -n iso-b exec prober -- c4e2e probe "$@" 2>&1; }
REG=http://c4-regional.c4-system.svc:7443/$SECRET_OBJ
PEER=http://$HOST_IP:7444/$SECRET_OBJ
p_reg_none=$(probe --url "$REG" --project iso-a)
p_reg_tok=$(probe --url "$REG" --project iso-a --token /var/run/tier/token)
p_peer_none=$(probe --url "$PEER" --project iso-a)
p_peer_tok=$(probe --url "$PEER" --project iso-a --token /var/run/tier/token)
p_peer_own=$(probe --url "$PEER" --project iso-b --token /var/run/tier/token)
for r in "$p_reg_none" "$p_peer_none"; do
  case "$r" in 401*) ;; *) fail "a tier answered a request without a token: $r" ;; esac
done
for r in "$p_reg_tok" "$p_peer_tok" "$p_peer_own"; do
  case "$r" in 403* | 401*) ;; *) fail "a tier served a pod's token: $r" ;; esac
done

log "4. a forged CSI volume context"
mat iso-a show "$SHOW"
expect_outcome iso-a show "Ready" >/dev/null
reader iso-a a-reader show "$SHOW"
done_ok iso-a a-reader
A_UID=$(k -n iso-a get pod a-reader -o jsonpath='{.metadata.uid}')
k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: forger, namespace: iso-b}
spec:
  nodeName: $NODE
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: read
    image: c4-e2e:dev
    imagePullPolicy: IfNotPresent
    command: [c4e2e, read, /show]
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
    volumeMounts: [{name: show, mountPath: /show, readOnly: true}]
  volumes:
  - name: show
    csi:
      driver: c4.avalanche.io
      readOnly: true
      volumeAttributes:
        materialization: show
        csi.storage.k8s.io/pod.namespace: iso-a
        csi.storage.k8s.io/pod.name: a-reader
        csi.storage.k8s.io/pod.uid: "$A_UID"
        csi.storage.k8s.io/serviceAccount.tokens: '{"c4.avalanche.io":{"token":"forged"}}'
EOF
forged=$(mount_error iso-b forger)
case "$forged" in
  *"iso-b/show"*) ;;
  *) fail "the forged volume context was not resolved in iso-b: $forged" ;;
esac
[ "$(k -n iso-b get pod forger -o jsonpath='{.status.phase}')" = Pending ] || fail "the forger's pod started"

record "### Attempts to read another project's data"
record ""
record "| attempt | made as | outcome |"
record "|---|---|---|"
record "| GET A's objects from A's bucket | B's MinIO user, from the host | refused by MinIO: $(cut_msg "$(printf '%s' "$out_b" | grep -o 'Access Denied[^,]*\|AccessDenied[^,]*\|40[0-9][^,]*' | head -1)") |"
record "| Materialization naming A's unreleased view by ID | namespace \`iso-b\` | $(cut_msg "$r_id") |"
record "| ConfigMap c4m listing A's object IDs | namespace \`iso-b\` | $(cut_msg "$r_c4m") |"
record "| GET an object of A from the regional tier, no token | pod in \`iso-b\` | $(cut_msg "$p_reg_none") |"
record "| the same with the pod's own token for audience \`c4-tier\` | pod in \`iso-b\` | $(cut_msg "$p_reg_tok") |"
record "| GET it from the node daemon's peer port, no token | pod in \`iso-b\` | $(cut_msg "$p_peer_none") |"
record "| the same with the pod's token | pod in \`iso-b\` | $(cut_msg "$p_peer_tok") |"
record "| the same, asking as project \`iso-b\` | pod in \`iso-b\` | $(cut_msg "$p_peer_own") |"
record "| CSI volume attributes claiming A's pod (namespace, name, UID) and a forged token | pod in \`iso-b\` | pod stays Pending; FailedMount: $(cut_msg "$forged") |"
record ""

log "5. Strict partitions"
bup0=$(node_metric "$NODE" c4_tier_upstream_requests_total 'tier="node"')
fill_b0=$(regional_metric c4_tier_requests_total 'tier="regional"' 'project="iso-b"' 'result="fill"')
breq0=$(settled bucket_requests iso-b)
mat iso-b show "$SHOW"
expect_outcome iso-b show "Ready" >/dev/null
reader iso-b b-reader show "$SHOW"
done_ok iso-b b-reader
bup1=$(node_metric "$NODE" c4_tier_upstream_requests_total 'tier="node"')
fill_b1=$(regional_metric c4_tier_requests_total 'tier="regional"' 'project="iso-b"' 'result="fill"')
breq1=$(settled bucket_requests iso-b)
ia=$(inode "$NODE" p-iso-a "$PLATE")
ib=$(inode "$NODE" p-iso-b "$PLATE")
[ "$ia" != none ] && [ "$ib" != none ] && [ "$ia" != "$ib" ] || fail "strict partitions: inodes $ia and $ib"
b_pub=$(pub iso-b b-reader)
b_gets=$((${breq1% *} - ${breq0% *})) b_heads=$((${breq1#* } - ${breq0#* }))

log "6. Shared partitions and proof of possession"
mat dept-1 show "$SHOW"
expect_outcome dept-1 show "Ready" >/dev/null
reader dept-1 d1-reader show "$SHOW"
done_ok dept-1 d1-reader
up0=$(node_metric "$NODE" c4_tier_upstream_requests_total 'tier="node"')
reg0=$(regional_metric c4_tier_requests_total 'tier="regional"' 'project="dept-2"')
req0=$(settled bucket_requests dept-2)
mat dept-2 show "$SHOW"
expect_outcome dept-2 show "Ready" >/dev/null
reader dept-2 d2-reader show "$SHOW"
done_ok dept-2 d2-reader
up1=$(node_metric "$NODE" c4_tier_upstream_requests_total 'tier="node"')
reg1=$(regional_metric c4_tier_requests_total 'tier="regional"' 'project="dept-2"')
req1=$(settled bucket_requests dept-2)
d2_pub=$(pub dept-2 d2-reader)
ig=$(inode "$NODE" g-show-x "$PLATE")
links=$(docker exec "$NODE" sh -c "stat -c %h /var/lib/c4/p/g-show-x/objects/*/$PLATE")
[ "$ig" != none ] || fail "the shared partition does not hold the plate"
[ "$(docker exec "$NODE" sh -c 'ls -d /var/lib/c4/p/p-dept-* 2>/dev/null | wc -l' | tr -d ' ')" = 0 ] || fail "a Shared project got its own partition"
d2_gets=$((${req1% *} - ${req0% *})) d2_heads=$((${req1#* } - ${req0#* }))

# sh500 is in dept-1's bucket only. Once dept-1 has used it on this node,
# the bytes are in the shared cache; dept-2 still cannot have them.
mat dept-1 sh500 "$D1ONLY"
expect_outcome dept-1 sh500 "Ready" >/dev/null
reader dept-1 d1-sh500 sh500 "$D1ONLY"
done_ok dept-1 d1-sh500
D1_OBJ=$("$TOOL" tree-id "$(find "$D1_DIR" -name '*.exr' | sort | head -1)")
mat dept-2 sh500-by-id "$D1ONLY"
s_id=$(expect_outcome dept-2 sh500-by-id "Failed ResolveFailed")
k -n dept-2 create configmap sh500 --from-file=tree.c4m="$OUT/p3-sh500.c4m" --dry-run=client -o yaml |
  k label --local -f - c4e2e=p3 -o yaml | k apply -f - >/dev/null
mat dept-2 sh500-by-c4m cm:sh500
s_c4m=$(expect_outcome dept-2 sh500-by-c4m "Failed ContentMissing")

record "### Partitions on one node ($NODE)"
record ""
record "| | Strict: \`iso-a\` then \`iso-b\` | Shared: \`dept-1\` then \`dept-2\` |"
record "|---|---|---|"
record "| view ID in both projects | $(short "$SHOW") | the same |"
record "| copies of plate sh010.1001 in the node cache | 2 (inodes $ia and $ib, partitions \`p-iso-a\` and \`p-iso-b\`) | 1 (inode $ig, $links links, partition \`g-show-x\`) |"
record "| second project's publish | $b_pub | $d2_pub |"
record "| node upstream requests during the second project's mount | $((bup1 - bup0)) | $((up1 - up0)) |"
record "| second project's fills at the regional tier | $((fill_b1 - fill_b0)), each from its own store | $((reg1 - reg0)) requests |"
record "| second project's bucket during its turn | $b_gets GETs, $b_heads HEADs | $d2_gets GETs (the manager reading the view's directory records), $d2_heads HEADs (preflight: every object, in its own store) |"
record ""
record "Proof of possession: shot sh500 is only in \`dept-1\`'s bucket. After a \`dept-1\` pod used it on $NODE, its bytes sat in the shared partition (frame 1001 at inode $(inode "$NODE" g-show-x "$D1_OBJ")). \`dept-2\` asking for it by ID: $(cut_msg "$s_id"). By a c4m listing its IDs: $(cut_msg "$s_c4m")."
record ""

log "7. grants: a vendor that may use one shot"
k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Project
metadata: {name: vendor}
spec:
  namespaces: [vendor]
  isolation: Strict
  scope: Grants
  grants:
  - tree: "$SHOW"
    paths: [shots/sh010/]
  store:
    access: Tiered
    credentialsSecretRef: {name: store-vendor}
    shards:
$(shards_yaml iso-a)
EOF
k wait --for=condition=Ready project vendor --timeout=60s >/dev/null
mat vendor show "$SHOW"
g_ok=$(expect_outcome vendor show "Ready")
g_files=$(k -n vendor get materialization show -o jsonpath='{.status.files}')
reader vendor v-reader show "$SUB" 30m
held vendor v-reader
mat vendor sh020 "$SHOW" shots/sh020/
g_out=$(expect_outcome vendor sh020 "Failed NotGranted")
mat vendor sh900 "$SECRET"
g_sec=$(expect_outcome vendor sh900 "Failed NotGranted")
k -n vendor create configmap handmade --from-file=tree.c4m="$OUT/p3-sh010.c4m" --dry-run=client -o yaml |
  k label --local -f - c4e2e=p3 -o yaml | k apply -f - >/dev/null
mat vendor handmade cm:handmade
g_hand=$(expect_outcome vendor handmade "Failed NotGranted")

t0=$(now)
k patch project vendor --type=merge -p '{"spec":{"grants":null}}' >/dev/null
until [ "$(k -n vendor get materialization show -o jsonpath='{.status.phase}')" = Failed ]; do
  [ "$(perl -e "print $(now) - $t0 > 60 ? 1 : 0")" = 1 ] && fail "revoking the grant did not fail the Materialization"
  sleep 0.2
done
revoke_s=$(since "$t0")
g_rev=$(mat_outcome vendor show)
reader vendor v-late show "$SUB"
late=$(mount_error vendor v-late)
still=$(k -n vendor exec v-reader -- sh -c 'ls /show/shots/sh010/plates | wc -l' | tr -d ' ')

record "### Grants: a vendor project on the show's store"
record ""
record "Project \`vendor\` reads \`iso-a\`'s bucket through its own MinIO user, with scope Grants and one grant: the show's view, paths \`shots/sh010/\`."
record ""
record "| Materialization in \`vendor\` | outcome |"
record "|---|---|"
record "| the granted view | Ready with $g_files files; a pod verified its mount is the sh010 subset, $(short "$SUB") |"
record "| the granted view with include \`shots/sh020/\` | $(cut_msg "$g_out") |"
record "| sh900, another tree in the same bucket | $(cut_msg "$g_sec") |"
record "| a ConfigMap c4m listing exactly the granted files | $(cut_msg "$g_hand") |"
record "| the granted view after the grant is removed (scope stays Grants) | $(cut_msg "$g_rev"), ${revoke_s} s after the Project change |"
record "| a new pod mounting it after the revocation | Pending; FailedMount: $(cut_msg "$late") |"
record "| the pod that mounted it before the revocation | still running, still reads its $still plates: revocation takes effect at the next mount |"
record ""

log "cleanup"
for ns in $P3NS; do k -n "$ns" delete pods --all --wait=false >/dev/null 2>&1 || true; done
rm -rf "$SHOW_DIR" "$SECRET_DIR" "$D1_DIR"
echo "P3 PASS"
