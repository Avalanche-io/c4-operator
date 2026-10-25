# Shared by the live tracker demos (dailies.sh: Flow; github.sh: GitHub;
# kitsu.sh: Kitsu, a real server rather than a fake). Source it after setting:
#
#   DEMO_NAME   the demo's namespace, Project, bucket and Pool (dailies, widgets)
#   FAKE        the fake tracker's command and image name (fakeflow, fakegithub)
#   FAKE_ARGS   its arguments, as a YAML flow list
#   FAKE_PORT   the local port its service is forwarded to
#
# A demo adds to the running e2e cluster (hack/e2e/up.sh) and changes nothing
# that is there except what it records and restores: the manager's arguments
# (development viewers, for the captures) and the HUD's NodePort. What it
# creates it deletes at the end (KEEP=1 keeps it; `<demo>.sh down` removes
# it), on one node (worker5).
source "$(dirname "${BASH_SOURCE[0]}")/../e2e/lib.sh"

NS=$DEMO_NAME
NODE=$CLUSTER-worker5
DEMO=$ROOT/hack/demo
SHOTS=${DEMO_OUT:-$DEMO/out/$DEMO_NAME}
STATE=$SHOTS/.state
FAKE_URL=http://127.0.0.1:$FAKE_PORT
HUD=http://127.0.0.1:30080
ORACLE=${ORACLE:-c4-job-oiio:dev}
# Development viewers for every demo's project, so any demo's run keeps
# the manager's arguments the same.
DEMO_VIEWERS="wrangler=*;artist=show-a;dana@supervisor=dailies|widgets|kitsu;c4-sync@wrangler=dailies|widgets|kitsu;noor@artist=dailies|widgets|kitsu"
# DEMO_BLOCK=1 (the GitHub demo): the agent's work is a real block, Claude
# Code in the block image, on the sign-in Noor (the demo's person) lends
# with their own holder-transfer lease, for jobs the Tracker submits for
# them under their grant. The farm uses holder-transfer for the run only.
BLOCK_IMG=${BLOCK_IMG:-c4-block:dev}
LEASE=noor-claude
mkdir -p "$SHOTS"
touch "$STATE"
note() { printf '%s\n' "$*" >>"$STATE"; }
had() { grep -qx "$1" "$STATE"; }
t0=$(now)
mark() { printf '%-28s %6ss  %s\n' "$1" "$(since "$t0")" "$2" | tee -a "$SHOTS/timeline.txt" >&2; }
jq_py() { python3 -c "import sys,json; d=json.load(sys.stdin); $1"; }
fk() { curl -sf "$FAKE_URL$1"; }
fkpost() { curl -sf -X POST -H 'Content-Type: application/json' "$FAKE_URL$1" -d "$2"; }

power_ok() {
  [ "$(cat /tmp/c4-power-mode 2>/dev/null || echo RUN)" != STOP ] || fail "power mode STOP: not starting (or continuing) the demo"
}

# wait_until DESC SECONDS CMD...: poll CMD every 2 s until it succeeds, or
# fail after SECONDS with DESC. Never a fixed sleep as synchronization.
wait_until() {
  local desc=$1 limit=$2 start
  shift 2
  start=$(date +%s)
  until "$@" >/dev/null 2>&1; do
    [ $(($(date +%s) - start)) -lt "$limit" ] || fail "timed out after ${limit}s: $desc"
    power_ok
    sleep 2
  done
}

job_phase() { k -n "$NS" get workjob "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
has_job() { k -n "$NS" get workjob "$1" >/dev/null 2>&1; }
phase_is() { [ "$(job_phase "$1")" = "$2" ]; }
units_running() { [ "$(k -n "$NS" get workjob "$1" -o jsonpath='{.status.units.running}' 2>/dev/null)" -ge 1 ] 2>/dev/null; }
wait_open() { # JOB UNIT: the job's wait UNIT is open
  [ "$(k -n "$NS" get workjob "$1" -o jsonpath="{.status.waits[?(@.name==\"$2\")].since}" 2>/dev/null)" != "" ]
}
# lineage JOB JOB2 REF: JOB2 follows JOB, and both carry the tracker entity.
lineage() {
  local f r
  f=$(k -n "$NS" get workjob "$2" -o jsonpath='{.spec.follows}')
  [ "$f" = "$1" ] || fail "$2 follows '$f', want $1"
  for j in "$1" "$2"; do
    r=$(k -n "$NS" get workjob "$j" -o jsonpath='{.metadata.annotations.c4\.avalanche\.io/tracker-ref}')
    [ "$r" = "$3" ] || fail "$j carries tracker-ref '$r', want $3"
  done
}
output_view() { k -n "$NS" get workjob "$1" -o jsonpath='{.status.output.view}' 2>/dev/null; }

# --- teardown -------------------------------------------------------------

diagnose() {
  local d=$SHOTS/diagnose
  mkdir -p "$d"
  k -n "$NS" get workjobs -o yaml >"$d/workjobs.yaml" 2>&1
  k -n "$NS" get trackerlinks,trackers -o yaml >"$d/trackers.yaml" 2>&1
  k -n "$NS" get pods,materializations -o wide >"$d/pods.txt" 2>&1
  k -n "$NS" get events --sort-by=.lastTimestamp >"$d/events.txt" 2>&1
  k -n c4-system logs deploy/c4-sync --tail=400 >"$d/c4-sync.log" 2>&1
  k -n c4-system logs deploy/c4-manager --tail=400 >"$d/manager.log" 2>&1
  fk /demo/state >"$d/tracker-state.json" 2>&1
  log "diagnostics in $d"
}

teardown() {
  local rc=$?
  set +e
  [ "$rc" = 0 ] || diagnose
  [ -n "${profile:-}" ] && rm -rf "$profile"
  if [ "${KEEP:-}" = 1 ]; then
    log "KEEP=1: leaving the demo up ($0 down removes it)"
    return
  fi
  down
  rm -f "$STATE"
  exit $rc
}

down() {
  set +e
  log "teardown"
  [ -n "${PF:-}" ] && kill "$PF" 2>/dev/null && wait "$PF" 2>/dev/null
  k delete ns "$NS" --wait=true --timeout=180s >/dev/null 2>&1
  k -n c4-system delete deploy c4-sync svc c4-sync sa c4-sync >/dev/null 2>&1
  k delete clusterrole c4-sync clusterrolebinding c4-sync >/dev/null 2>&1
  if [ -z "$(k get trackers -A -o name 2>/dev/null)" ]; then
    k delete crd trackers.c4.avalanche.io trackerlinks.c4.avalanche.io >/dev/null 2>&1
  fi
  k delete project "$DEMO_NAME" --wait=true --timeout=120s >/dev/null 2>&1
  k delete pool "$DEMO_NAME" >/dev/null 2>&1
  k label node "$NODE" c4.avalanche.io/demo- >/dev/null 2>&1
  mc_run "for i in 0 1 2; do mc rb --force m\$i/$DEMO_NAME >/dev/null 2>&1 || true; done" >/dev/null 2>&1
  if had hud-svc-created; then k -n c4-system delete svc c4-hud-e2e c4-signer-e2e >/dev/null 2>&1; fi
  if [ -f "$SHOTS/.manager-args" ]; then
    k -n c4-system patch deploy c4-manager --type=json \
      -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":$(cat "$SHOTS/.manager-args")}]" >/dev/null 2>&1
    k -n c4-system rollout status deploy/c4-manager --timeout=180s >/dev/null 2>&1
    rm -f "$SHOTS/.manager-args"
  fi
  for img in c4-sync:dev "c4-$FAKE:dev" ${DEMO_BLOCK:+"$BLOCK_IMG"}; do
    docker exec "$NODE" crictl rmi "docker.io/library/$img" >/dev/null 2>&1
  done
  [ -n "${DEMO_BLOCK:-}" ] && log "Noor: stop your c4-agent lease now (Ctrl-C); it deletes the lease's Secret"
  if had oracle-loaded; then docker exec "$NODE" crictl rmi "docker.io/library/$ORACLE" >/dev/null 2>&1; fi
  log "torn down"
}

# demo_start: handle `down`, refuse a second run over a kept one, arm the
# teardown, and check what the run needs.
demo_start() {
  if [ "${1:-}" = down ]; then
    down
    rm -f "$STATE"
    exit 0
  fi
  [ -s "$STATE" ] && fail "a demo is still up (KEEP=1?): $0 down first"
  : >"$SHOTS/timeline.txt"
  rm -rf "$SHOTS/diagnose"
  power_ok
  k cluster-info >/dev/null 2>&1 || fail "no cluster $CTX: hack/e2e/up.sh makes one"
  demo_current
  docker image inspect "$ORACLE" >/dev/null 2>&1 || fail "$ORACLE is not built here: make job-images builds it"
  trap teardown EXIT
}

# demo_current: the cluster's operator is built from this checkout
# (hack/e2e/redeploy.sh stamps it). A stale one lacks what the demo shows,
# such as the TrackerLink journal c4-sync keeps or the HUD's controls.
# REDEPLOY=1 redeploys it first. The cluster may be shared: make sure no
# one else is using it before either.
demo_current() {
  local want have
  want=$(built_from)
  have=$(k -n c4-system get deploy c4-manager -o jsonpath='{.metadata.annotations.c4\.avalanche\.io/built-from}' 2>/dev/null)
  [ "$have" = "$want" ] && return
  if [ "${REDEPLOY:-}" = 1 ]; then
    log "redeploying the operator from $want (the cluster ran ${have:-an unstamped build})"
    "$ROOT/hack/e2e/redeploy.sh" >/dev/null || fail "hack/e2e/redeploy.sh failed"
    return
  fi
  fail "the cluster's operator is ${have:-unstamped}, not this checkout ($want): REDEPLOY=1 $0 redeploys it first (book the cluster), or run hack/e2e/redeploy.sh"
}

# --- up -------------------------------------------------------------------

demo_hud() {
  log "the HUD, as the captures need it (recorded, restored at the end)"
  local want cur
  cur=$(k -n c4-system get deploy c4-manager -o jsonpath='{.spec.template.spec.containers[0].args}')
  # The demo's flags replace their own kind; flags it does not set (another
  # team's, on the shared cluster) stay. A demo harness: open development
  # viewers (--hud-dev-viewers-open), anyone who reaches the HUD is any viewer.
  want=$(python3 -c '
import json, sys
cur, ours = json.loads(sys.argv[1] or "[\"manager\"]"), sys.argv[2:]
own = {a.split("=")[0] for a in ours}
print(json.dumps(cur[:1] + [a for a in cur[1:] if a.split("=")[0] not in own] + ours, separators=(",", ":")))
' "$cur" --capacity-provider=kind-sim --work-policy=objective "--hud-dev-viewers=$DEMO_VIEWERS" --hud-dev-viewers-open --tz=America/Los_Angeles \
    ${DEMO_BLOCK:+--holder-transfer=allow})
  if [ "$(echo "$cur" | tr -d ' ')" != "$want" ]; then
    printf '%s' "$cur" >"$SHOTS/.manager-args"
    k -n c4-system patch deploy c4-manager --type=json -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":$want}]" >/dev/null
    k -n c4-system rollout status deploy/c4-manager --timeout=180s >/dev/null
  fi
  if ! k -n c4-system get svc c4-hud-e2e >/dev/null 2>&1; then
    k apply -f "$ROOT/hack/e2e/hud.yaml" >/dev/null
    note hud-svc-created
  fi
  wait_until "the HUD on $HUD" 120 curl -sf "$HUD/api/v1/snapshot?as=wrangler"
}

demo_images() {
  log "images: c4-sync, the fake tracker, $ORACLE onto $NODE only"
  make -C "$ROOT" image-sync >/dev/null
  mkdir -p "$ROOT/bin/demo-context/linux/$ARCH"
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -o "$ROOT/bin/demo-context/linux/$ARCH/$FAKE" "$DEMO/$FAKE"
  docker build -q -f "$DEMO/Dockerfile.fake" --build-arg TARGETPLATFORM="linux/$ARCH" --build-arg FAKE="$FAKE" \
    -t "c4-$FAKE:dev" "$ROOT/bin/demo-context" >/dev/null
  docker exec "$NODE" crictl images 2>/dev/null | grep -q "${ORACLE%%:*}" || note oracle-loaded
  local img
  for img in c4-sync:dev "c4-$FAKE:dev" "$ORACLE"; do
    "$KIND" load docker-image "$img" --name "$CLUSTER" --nodes "$NODE" >/dev/null
  done
  if [ -n "${DEMO_BLOCK:-}" ]; then
    log "the block image ($BLOCK_IMG): Claude Code, no credential"
    make -C "$ROOT" image-block BLOCK_IMG="$BLOCK_IMG" >/dev/null
    "$KIND" load docker-image "$BLOCK_IMG" --name "$CLUSTER" --nodes "$NODE" >/dev/null
  fi
}

# demo_lent TRACKER: wait for what only Noor gives, as themselves: their
# grant to the Tracker and their lease of their sign-in. The demo never gives
# either for them; it prints what to run and waits.
demo_lent() {
  local grant
  grant=$(python3 -c 'import hashlib,sys; print(sys.argv[1]+"-"+hashlib.sha256(sys.argv[2].encode()).hexdigest()[:10])' "$1" noor)
  granted() { [ "$(k -n "$NS" get submitgrant "$grant" -o jsonpath='{.spec.person}/{.spec.revoked.at}' 2>/dev/null)" = "noor/" ]; }
  leased() { [ "$(k -n "$NS" get credentiallease "$LEASE" -o jsonpath='{.status.state}' 2>/dev/null)" = active ]; }
  go build -o "$ROOT/bin/c4-agent" "$ROOT/cmd/c4-agent"
  go build -o "$ROOT/bin/c4-farm" "$ROOT/cmd/c4-farm"
  if ! granted || ! leased; then
    {
      echo
      echo "== Noor: two things only you can do, as yourself, in another terminal =="
      echo "  1. let tracker $1 submit work for you, for a day:"
      echo "       $ROOT/bin/c4-farm login $HUD --as noor --name demo   # once"
      echo "       $ROOT/bin/c4-farm grant $1 --farm demo --project $DEMO_NAME --pools $DEMO_NAME --until 1d"
      echo "  2. lend your sign-in to your own jobs in pool $DEMO_NAME (leave it running; Ctrl-C after the demo):"
      echo "       $ROOT/bin/c4-agent lease --context $CTX --namespace $NS --name $LEASE \\"
      echo "         --holder noor --owner noor --pool $DEMO_NAME --ttl 2h --renew-before 15m --config-dir \$HOME/.claude"
      echo "waiting for both (15 min) ..."
    } >&2
  fi
  wait_until "Noor's grant to tracker $1" 900 granted
  wait_until "Noor's lease $LEASE, active" 900 leased
}

demo_project() {
  log "project $DEMO_NAME: bucket, namespace, Project, Pool on $NODE"
  k label node "$NODE" c4.avalanche.io/demo="$DEMO_NAME" --overwrite >/dev/null
  mc_run "for i in 0 1 2; do mc mb --ignore-existing m\$i/$DEMO_NAME >/dev/null; done" >/dev/null
  k create ns "$NS" --dry-run=client -o yaml | k apply -f - >/dev/null
  k label ns "$NS" pod-security.kubernetes.io/enforce=restricted --overwrite >/dev/null
  k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Project
metadata: {name: $DEMO_NAME}
spec:
  namespaces: [$NS]
  isolation: Strict
  store:
    access: Tiered
    credentialsSecretRef: {name: store-root}
    shards:
$(shards_yaml "$DEMO_NAME")
---
apiVersion: c4.avalanche.io/v1alpha1
kind: Pool
metadata: {name: $DEMO_NAME}
spec:
  nodeSelector: {c4.avalanche.io/demo: $DEMO_NAME}
  slotsPerNode: 4
  costPerSlotHour: "0.40"
  attributes: {class: cpu, capacityType: on-demand}
EOF
  k wait --for=condition=Ready "project/$DEMO_NAME" --timeout=120s >/dev/null
}

# demo_scene SCRIPT MATERIALIZATION: the script as a view in the project's
# store, mounted by the jobs as /show.
demo_scene() {
  log "the scene: $1 as a view in the project's store"
  go build -o "$OP" "$ROOT/cmd/c4-operator"
  local scene view
  scene=$(mktemp -d)
  cp "$DEMO/scene/$1" "$scene/$1"
  view=$(ingest "$DEMO_NAME" "$scene")
  rm -rf "$scene"
  k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: $2, namespace: $NS}
spec:
  source: {id: "$view"}
EOF
  mat_view "$NS" "$2" >/dev/null
}

demo_fake() {
  log "the fake tracker ($FAKE)"
  k apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: $FAKE, namespace: $NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: $FAKE}}
  template:
    metadata: {labels: {app: $FAKE}}
    spec:
      nodeSelector: {kubernetes.io/hostname: $NODE}
      securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
      - name: $FAKE
        image: c4-$FAKE:dev
        imagePullPolicy: IfNotPresent
        args: $FAKE_ARGS
        ports: [{containerPort: 8080}]
        readinessProbe: {httpGet: {path: /healthz, port: 8080}}
        securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
---
apiVersion: v1
kind: Service
metadata: {name: $FAKE, namespace: $NS}
spec:
  selector: {app: $FAKE}
  ports: [{port: 8080, targetPort: 8080}]
EOF
  k -n "$NS" rollout status "deploy/$FAKE" --timeout=120s >/dev/null
  k -n "$NS" port-forward "svc/$FAKE" "$FAKE_PORT:8080" >/dev/null 2>&1 &
  PF=$!
  wait_until "the fake tracker on $FAKE_URL" 60 fk /healthz
}

# demo_sync SECRET KEY VALUE: c4-sync, and a Secret its Tracker reads.
demo_sync() {
  log "c4-sync"
  k apply -f "$ROOT/config/crd/c4.avalanche.io_trackers.yaml" -f "$ROOT/config/crd/c4.avalanche.io_trackerlinks.yaml" >/dev/null
  k apply -k "$ROOT/config/sync" >/dev/null
  k -n c4-system patch deploy c4-sync --type=json -p="[
 {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":[\"--hud=http://c4-hud.c4-system.svc\",\"--hud-dev-viewer=c4-sync\",\"--hud-public-url=$HUD\",\"--interval=10s\"]},
 {\"op\":\"add\",\"path\":\"/spec/template/spec/nodeSelector\",\"value\":{\"kubernetes.io/hostname\":\"$NODE\"}},
 {\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/imagePullPolicy\",\"value\":\"IfNotPresent\"}]" >/dev/null
  k -n c4-system rollout status deploy/c4-sync --timeout=120s >/dev/null
  k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata: {name: $1, namespace: $NS}
stringData: {$2: $3}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: c4-sync-tracker, namespace: $NS}
rules:
- {apiGroups: [""], resources: [secrets], resourceNames: [$1], verbs: [get]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: c4-sync-tracker, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: c4-sync-tracker}
subjects: [{kind: ServiceAccount, name: c4-sync, namespace: c4-system}]
EOF
}

# --- captures -------------------------------------------------------------

chrome="${CHROME:-}"
if [ -z "$chrome" ]; then
  dir="$(ls -d "$HOME"/.cache/puppeteer/chrome-headless-shell/mac_arm-*/ 2>/dev/null | sort -V | tail -1)"
  chrome="${dir}chrome-headless-shell-mac-arm64/chrome-headless-shell"
fi
profile=$(mktemp -d)
shot() { # NAME URL WIDTH HEIGHT: one capture; the browser exits by itself
  [ -x "$chrome" ] || { echo "no chrome-headless-shell; skipping $1" >&2; return 0; }
  "$chrome" --headless --disable-gpu --hide-scrollbars --no-first-run --no-default-browser-check \
    --user-data-dir="$profile/$1" --force-device-scale-factor=2 --window-size="$3,$4" \
    --virtual-time-budget=9000 --run-all-compositor-stages-before-draw \
    --screenshot="$SHOTS/$1.png" "$2" >/dev/null 2>&1
}
# capture STEP JOB: the HUD queue as the supervisor (her projects only), the
# job as the wrangler and as the supervisor, and the fake tracker's page.
capture() {
  shot "$1-hud-queue" "$HUD/?as=dana&static=1#/queue" 1600 1000
  shot "$1-hud-job" "$HUD/?as=wrangler&static=1#/job/$NS/$2" 1600 1300
  shot "$1-hud-supervisor" "$HUD/?as=dana&static=1#/job/$NS/$2" 1600 1300
  shot "$1-tracker" "$FAKE_URL/demo/" 1280 900
}
