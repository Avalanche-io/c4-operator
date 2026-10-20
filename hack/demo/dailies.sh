#!/usr/bin/env bash
# The Flow dailies demo, live: a fake
# Flow Production Tracking site beside the real operator and c4-sync on the
# e2e kind cluster, a render from the oiio job image, and HUD captures of
# each step. `make demo-dailies` runs it; hack/demo/lib.sh says what it
# adds to the cluster and how it cleans up.
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
DEMO_NAME=dailies FAKE=fakeflow FAKE_ARGS='[--script-key=demo-script-key]' FAKE_PORT=${FF_PORT:-18080}
source "$(dirname "$0")/lib.sh"
demo_start "$@"

demo_hud
demo_images
demo_project
demo_scene render.sh sh040-scene
demo_fake
DUE=$(TZ=America/Los_Angeles date -v+4d +%F 2>/dev/null || TZ=America/Los_Angeles date -d '+4 days' +%F)
SEED=$(fkpost /demo/seed "{\"due\":\"$DUE\"}")
TASK=$(echo "$SEED" | jq_py 'print(d["task"])')
PROJ=$(echo "$SEED" | jq_py 'print(d["project"])')
demo_sync flow-script key demo-script-key
k apply -f - >/dev/null <<EOF
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
      pools: [$DEMO_NAME]
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

# --- run ------------------------------------------------------------------

state_json() { fk /demo/state; }
versions_are() { [ "$(state_json | jq_py 'print(len(d["versions"] or []))')" = "$1" ]; }
version_id() { state_json | jq_py "print(d['versions'][$1]['id'])"; }
task_status_is() { [ "$(state_json | jq_py 'print(d["task"]["sg_status_list"])')" = "$1" ]; }

# The job is named by the shot and the step (flow-sh040-lgt-<6 hex>); the
# link says which.
JOB=
has_link() { JOB=$(k -n "$NS" get trackerlinks -o jsonpath='{.items[0].spec.target.workJob}' 2>/dev/null) && [ -n "$JOB" ]; }
wait_until "c4-sync to make a job from Task $TASK" 120 has_link
case "$JOB" in flow-sh040-lgt-*) ;; *) fail "the job is named $JOB, not by its shot and step" ;; esac
wait_until "job $JOB" 60 has_job "$JOB"
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

fkpost /demo/set "{\"type\":\"Version\",\"id\":$V1,\"fields\":{\"sg_status_list\":\"fix\"},\"by\":\"dana\"}" >/dev/null
fkpost /demo/note "{\"about\":{\"type\":\"Version\",\"id\":$V1},\"subject\":\"sh040 lighting\",\"content\":\"Fill light too hot, 1001 to 1012. Bring it down a stop.\",\"by\":\"dana\",\"replies\":[\"Keep the rim as is.\"]}" >/dev/null
JOB2=$JOB-v002
wait_until "the note relayed as a rework, and $JOB2 submitted" 120 has_job "$JOB2"
mark "4 note to rework" "Dana's needs-fix and note on $V1 became a rework; $JOB2 follows $JOB with the note mounted"
capture 4-rework "$JOB2"

wait_until "v002 published to Flow" 600 versions_are 2
V2=$(version_id 1)
mark "5 v002 in review" "Version $V2 pending review"
capture 5-v002-review "$JOB2"

fkpost /demo/set "{\"type\":\"Version\",\"id\":$V2,\"fields\":{\"sg_status_list\":\"apr\"},\"by\":\"dana\"}" >/dev/null
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
lineage "$JOB" "$JOB2" "flow/Task/$TASK"
mark "checks" "Versions v001 fix, v002 apr; C4 IDs distinct; $JOB2 follows $JOB, both carry the Task; nothing C4 wrote approved anything"
log "demo done: $SHOTS"
