#!/usr/bin/env bash
# The Flow dailies demo, live (proposals/integrations LAUNCH 3.1): a fake
# Flow Production Tracking site beside the real operator and c4-sync on the
# e2e kind cluster, a render from the oiio job image, and HUD captures of
# each step. `make demo-dailies` runs it.
#
#   1. a producer's Task, due on a date, becomes a render job whose
#      deadline is 14:00 Los Angeles time that day (deadline mode Tracking);
#   2. the render runs: the Task goes in progress in Flow, with C4's ETA;
#   3. the frames are done and the dailies wait opens: c4-sync publishes
#      v001 in pending review with its output view's C4 ID;
#   4. the supervisor, in Flow, sets v001 to needs-fix and writes a note:
#      the note comes back as a rework, and v002 follows v001 with the
#      note mounted as an input;
#   5. v002 renders with the fill light down and is published for review;
#   6. the supervisor approves v002 in Flow; the wait passes. C4 never
#      wrote an approval.
#
# It adds to the running e2e cluster (hack/e2e/up.sh) and changes nothing
# that is there, except what it records and restores: the manager's
# arguments (development viewers, for the captures) and the HUD's NodePort.
# Everything it creates it deletes at the end (KEEP=1 keeps it), on one
# node (worker5), in one namespace (dailies), with its own Project, bucket
# and Pool. Captures go to hack/demo/out/dailies (DEMO_OUT overrides).
source "$(dirname "$0")/../e2e/lib.sh"

NS=dailies
NODE=$CLUSTER-worker5
DEMO=$ROOT/hack/demo
SHOTS=${DEMO_OUT:-$DEMO/out/dailies}
STATE=$SHOTS/.state
FF_PORT=${FF_PORT:-18080}
FF=http://127.0.0.1:$FF_PORT
HUD=http://127.0.0.1:30080
ORACLE=${ORACLE:-c4-job-oiio:dev}
mkdir -p "$SHOTS"
touch "$STATE"
note() { printf '%s\n' "$*" >>"$STATE"; }
had() { grep -qx "$1" "$STATE"; }
t0=$(now)
mark() { printf '%-28s %6ss  %s\n' "$1" "$(since "$t0")" "$2" | tee -a "$SHOTS/timeline.txt" >&2; }

power_ok() {
  [ "$(cat /tmp/c4-power-mode 2>/dev/null || echo RUN)" != STOP ] || fail "power mode STOP: not starting (or continuing) the demo"
}
power_ok
k cluster-info >/dev/null 2>&1 || fail "no cluster $CTX: hack/e2e/up.sh makes one"
docker image inspect "$ORACLE" >/dev/null 2>&1 || fail "$ORACLE is not built here: make job-images (c4-onboarding) builds it"

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

ff() { curl -sf "$FF$1"; }
ffpost() { curl -sf -X POST -H 'Content-Type: application/json' "$FF$1" -d "$2"; }
state_json() { ff /demo/state; }
jq_py() { python3 -c "import sys,json; d=json.load(sys.stdin); $1"; }

# --- teardown, recorded as we go ---------------------------------------

# diagnose: what the farm, c4-sync and the fake Flow say, for a failed run.
diagnose() {
  local d=$SHOTS/diagnose
  mkdir -p "$d"
  k -n "$NS" get workjobs -o yaml >"$d/workjobs.yaml" 2>&1
  k -n "$NS" get trackerlinks,trackers -o yaml >"$d/trackers.yaml" 2>&1
  k -n "$NS" get pods,materializations -o wide >"$d/pods.txt" 2>&1
  k -n "$NS" get events --sort-by=.lastTimestamp >"$d/events.txt" 2>&1
  k -n c4-system logs deploy/c4-sync --tail=400 >"$d/c4-sync.log" 2>&1
  k -n c4-system logs deploy/c4-manager --tail=400 >"$d/manager.log" 2>&1
  ff /demo/state >"$d/flow-state.json" 2>&1
  log "diagnostics in $d"
}

teardown() {
  local rc=$?
  set +e
  [ "$rc" = 0 ] || diagnose
  if [ "${KEEP:-}" = 1 ]; then
    log "KEEP=1: leaving the demo up (hack/demo/dailies.sh down removes it)"
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
  k delete project dailies --wait=true --timeout=120s >/dev/null 2>&1
  k delete pool dailies >/dev/null 2>&1
  k label node "$NODE" c4.avalanche.io/demo- >/dev/null 2>&1
  mc_run "for i in 0 1 2; do mc rb --force m\$i/dailies >/dev/null 2>&1 || true; done" >/dev/null 2>&1
  if had hud-svc-created; then k -n c4-system delete svc c4-hud-e2e c4-signer-e2e >/dev/null 2>&1; fi
  if [ -f "$SHOTS/.manager-args" ]; then
    k -n c4-system patch deploy c4-manager --type=json \
      -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":$(cat "$SHOTS/.manager-args")}]" >/dev/null 2>&1
    k -n c4-system rollout status deploy/c4-manager --timeout=180s >/dev/null 2>&1
    rm -f "$SHOTS/.manager-args"
  fi
  for img in c4-sync:dev c4-fakeflow:dev; do
    docker exec "$NODE" crictl rmi "docker.io/library/$img" >/dev/null 2>&1
  done
  if had oracle-loaded; then docker exec "$NODE" crictl rmi "docker.io/library/$ORACLE" >/dev/null 2>&1; fi
  log "torn down"
}

if [ "${1:-}" = down ]; then
  down
  rm -f "$STATE"
  exit 0
fi
[ -s "$STATE" ] && fail "a demo is still up (KEEP=1?): hack/demo/dailies.sh down first"
: >"$SHOTS/timeline.txt"
rm -rf "$SHOTS/diagnose"
trap teardown EXIT

# --- up -----------------------------------------------------------------

log "the HUD, as the captures need it (recorded, restored at the end)"
want="[\"manager\",\"--capacity-provider=kind-sim\",\"--work-policy=objective\",\"--hud-dev-viewers=wrangler=*;artist=show-a;dana@supervisor=dailies;c4-sync@wrangler=dailies\",\"--tz=America/Los_Angeles\"]"
cur=$(k -n c4-system get deploy c4-manager -o jsonpath='{.spec.template.spec.containers[0].args}')
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

log "images: c4-sync, the fake Flow, $ORACLE onto $NODE only"
make -C "$ROOT" image-sync >/dev/null
mkdir -p "$ROOT/bin/demo-context/linux/$ARCH"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -o "$ROOT/bin/demo-context/linux/$ARCH/fakeflow" "$DEMO/fakeflow"
docker build -q -f "$DEMO/Dockerfile.fakeflow" --build-arg TARGETPLATFORM="linux/$ARCH" -t c4-fakeflow:dev "$ROOT/bin/demo-context" >/dev/null
docker exec "$NODE" crictl images 2>/dev/null | grep -q "${ORACLE%%:*}" || note oracle-loaded
for img in c4-sync:dev c4-fakeflow:dev "$ORACLE"; do
  "$KIND" load docker-image "$img" --name "$CLUSTER" --nodes "$NODE" >/dev/null
done

log "project dailies: bucket, namespace, Project, Pool on $NODE"
k label node "$NODE" c4.avalanche.io/demo=dailies --overwrite >/dev/null
mc_run "for i in 0 1 2; do mc mb --ignore-existing m\$i/dailies >/dev/null; done" >/dev/null
k create ns "$NS" --dry-run=client -o yaml | k apply -f - >/dev/null
k label ns "$NS" pod-security.kubernetes.io/enforce=restricted --overwrite >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Project
metadata: {name: dailies}
spec:
  namespaces: [$NS]
  isolation: Strict
  store:
    access: Tiered
    credentialsSecretRef: {name: store-root}
    shards:
$(shards_yaml dailies)
---
apiVersion: c4.avalanche.io/v1alpha1
kind: Pool
metadata: {name: dailies}
spec:
  nodeSelector: {c4.avalanche.io/demo: dailies}
  slotsPerNode: 4
  costPerSlotHour: "0.40"
  attributes: {class: cpu, capacityType: on-demand}
EOF
k wait --for=condition=Ready project/dailies --timeout=120s >/dev/null

log "the scene: render.sh as a view in the project's store"
go build -o "$OP" "$ROOT/cmd/c4-operator"
scene=$(mktemp -d)
cp "$DEMO/scene/render.sh" "$scene/render.sh"
SCENE=$(ingest dailies "$scene")
rm -rf "$scene"
k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: sh040-scene, namespace: $NS}
spec:
  source: {id: "$SCENE"}
EOF
mat_view "$NS" sh040-scene >/dev/null

log "the fake Flow site"
k apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: fakeflow, namespace: $NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: fakeflow}}
  template:
    metadata: {labels: {app: fakeflow}}
    spec:
      nodeSelector: {kubernetes.io/hostname: $NODE}
      securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
      - name: fakeflow
        image: c4-fakeflow:dev
        imagePullPolicy: IfNotPresent
        args: [--script-key=demo-script-key]
        ports: [{containerPort: 8080}]
        readinessProbe: {httpGet: {path: /healthz, port: 8080}}
        securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
---
apiVersion: v1
kind: Service
metadata: {name: fakeflow, namespace: $NS}
spec:
  selector: {app: fakeflow}
  ports: [{port: 8080, targetPort: 8080}]
EOF
k -n "$NS" rollout status deploy/fakeflow --timeout=120s >/dev/null
k -n "$NS" port-forward svc/fakeflow "$FF_PORT:8080" >/dev/null 2>&1 &
PF=$!
wait_until "the fake Flow on $FF" 60 ff /healthz
DUE=$(TZ=America/Los_Angeles date -v+4d +%F 2>/dev/null || TZ=America/Los_Angeles date -d '+4 days' +%F)
SEED=$(ffpost /demo/seed "{\"due\":\"$DUE\"}")
TASK=$(echo "$SEED" | jq_py 'print(d["task"])')
PROJ=$(echo "$SEED" | jq_py 'print(d["project"])')

log "c4-sync, and its Tracker for the show"
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
metadata: {name: flow-script, namespace: $NS}
stringData: {key: demo-script-key}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: c4-sync-flow, namespace: $NS}
rules:
- {apiGroups: [""], resources: [secrets], resourceNames: [flow-script], verbs: [get]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: c4-sync-flow, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: c4-sync-flow}
subjects: [{kind: ServiceAccount, name: c4-sync, namespace: c4-system}]
---
apiVersion: c4.avalanche.io/v1alpha1
kind: Tracker
metadata: {name: flow, namespace: $NS}
spec:
  kind: flow
  endpoint: http://fakeflow.$NS.svc:8080
  scope: {project: "$PROJ", script: c4-sync, eta_field: sg_c4_eta, link_field: sg_c4_link}
  credential: {secret: {name: flow-script, key: key}}
  events: {mode: eventlog, pollSeconds: 10, resyncSeconds: 300}
  rate: {perSecond: "5", burst: 20}
  people: {dana: dana}
  intake:
  - match: {type: Task, fields: {sg_status_list: rdy, step.Step.short_name: LGT}}
    as: job
    deadline: {dateOnlyAt: "14:00", timeZone: America/Los_Angeles}
    review: {wait: dailies}
    rework: nextVersion
    job:
      class: graph
      params: {root: plan}
      pools: [dailies]
      inputs: [{materialization: sh040-scene, mountPath: /show}]
      template:
        spec:
          securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
          containers:
          - name: render
            image: $ORACLE
            imagePullPolicy: IfNotPresent
            command: [sh, /show/render.sh]
            securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
  statusOut:
    states: {running: ip, rework: ip, in-review: rev, needs-person: hld}
    fieldSeconds: 30
  verdictsIn: {"status:apr": approve, "status:fix": rework, "status:cbb": accept}
EOF
mark "up" "cluster, project, fake Flow, c4-sync ready; Task $TASK due $DUE"

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
capture() { # STEP JOB: the HUD queue (the supervisor sees the show only), the job as the wrangler and the supervisor, and Flow
  shot "$1-hud-queue" "$HUD/?as=dana&static=1#/queue" 1600 1000
  shot "$1-hud-job" "$HUD/?as=wrangler&static=1#/job/$NS/$2" 1600 1300
  shot "$1-hud-supervisor" "$HUD/?as=dana&static=1#/job/$NS/$2" 1600 1300
  shot "$1-flow" "$FF/demo/" 1280 900
}

# --- run ------------------------------------------------------------------

JOB=flow-$TASK
job_phase() { k -n "$NS" get workjob "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
has_job() { k -n "$NS" get workjob "$1" >/dev/null 2>&1; }
phase_is() { [ "$(job_phase "$1")" = "$2" ]; }
units_running() { [ "$(k -n "$NS" get workjob "$1" -o jsonpath='{.status.units.running}' 2>/dev/null)" -ge 1 ] 2>/dev/null; }
versions() { state_json | jq_py 'print(len(d["versions"] or []))'; }
versions_are() { [ "$(versions)" = "$1" ]; }
version_id() { state_json | jq_py "print(d['versions'][$1]['id'])"; }
task_status_is() { [ "$(state_json | jq_py 'print(d["task"]["sg_status_list"])')" = "$1" ]; }

wait_until "c4-sync to make job $JOB from Task $TASK" 120 has_job "$JOB"
deadline=$(k -n "$NS" get workjob "$JOB" -o jsonpath='{.spec.deadline} {.spec.deadlineMode}')
mark "1 intake" "Task $TASK became $JOB, deadline $deadline"
capture 1-intake "$JOB"

wait_until "frames of $JOB running" 300 units_running "$JOB"
wait_until "the Task in progress in Flow" 60 task_status_is ip
mark "2 rendering" "frames running; Task ip with C4's ETA"
capture 2-rendering "$JOB"

wait_until "v001 published to Flow" 600 versions_are 1
wait_until "the Task in review in Flow" 60 task_status_is rev
V1=$(version_id 0)
mark "3 v001 in review" "Version $V1 pending review, C4 ID of the output view on it"
capture 3-v001-review "$JOB"

ffpost /demo/set "{\"type\":\"Version\",\"id\":$V1,\"fields\":{\"sg_status_list\":\"fix\"},\"by\":\"dana\"}" >/dev/null
ffpost /demo/note "{\"about\":{\"type\":\"Version\",\"id\":$V1},\"subject\":\"sh040 lighting\",\"content\":\"Fill light too hot, 1001 to 1012. Bring it down a stop.\",\"by\":\"dana\",\"replies\":[\"Keep the rim as is.\"]}" >/dev/null
JOB2=$JOB-v002
wait_until "the note relayed as a rework, and $JOB2 submitted" 120 has_job "$JOB2"
mark "4 note to rework" "Dana's needs-fix and note on $V1 became a rework; $JOB2 follows $JOB with the note mounted"
capture 4-rework "$JOB2"

wait_until "v002 published to Flow" 600 versions_are 2
V2=$(version_id 1)
mark "5 v002 in review" "Version $V2 pending review"
capture 5-v002-review "$JOB2"

ffpost /demo/set "{\"type\":\"Version\",\"id\":$V2,\"fields\":{\"sg_status_list\":\"apr\"},\"by\":\"dana\"}" >/dev/null
wait_until "$JOB2 to finish once the approval serves its wait" 180 phase_is "$JOB2" Succeeded
mark "6 approved" "Dana approved $V2 in Flow; the wait passed; $JOB2 Succeeded"
capture 6-approved "$JOB2"

log "checks"
state_json >"$SHOTS/flow-state.json"
k -n "$NS" get trackerlinks -o yaml >"$SHOTS/trackerlinks.yaml"
k -n "$NS" get workjob "$JOB" "$JOB2" -o yaml >"$SHOTS/workjobs.yaml"
python3 - "$SHOTS/flow-state.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
v = d["versions"]
assert [x["code"] for x in v] == ["sh040_lgt_v001", "sh040_lgt_v002"], [x["code"] for x in v]
assert [x["sg_status_list"] for x in v] == ["fix", "apr"], "the supervisor's statuses must stand"
assert all(x["sg_c4_id"].startswith("c4") and len(x["sg_c4_id"]) == 90 for x in v), "each Version carries its output view's C4 ID"
assert v[0]["sg_c4_id"] != v[1]["sg_c4_id"], "v002 is different content"
assert d["scriptVersionChanges"] == 0, "c4-sync changed a Version after creating it"
print("flow state ok: v001 fix, v002 apr, two distinct C4 IDs")
PY
mark "checks" "Versions v001 fix, v002 apr; C4 IDs distinct; nothing C4 wrote approved anything"
rm -rf "$profile"
log "demo done: $SHOTS"
