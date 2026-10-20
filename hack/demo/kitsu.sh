#!/usr/bin/env bash
# The Kitsu dailies demo, live, against a real Kitsu: CGWire's open-source
# server (cgwire/cgwire) in a local container beside the e2e kind cluster,
# with the real operator and c4-sync, a render from the oiio job image, and
# HUD captures of each step. `make demo-kitsu` runs it; hack/demo/lib.sh
# says what it adds to the cluster and how it cleans up. The Kitsu server
# is a throwaway: the demo seeds a production of its own on it, with
# passwords and a bot token it makes.
#
#   1. a lighting task, due on a date, becomes a render job whose deadline
#      is 14:00 Los Angeles time that day (deadline mode Tracking);
#   2. the render runs: c4-sync comments the task into work in progress,
#      with C4's ETA in its data;
#   3. the frames are done and the job's review opens: c4-sync comments the
#      task into waiting for approval with the output view's C4 ID, and the
#      middle frame as the comment's preview, revision 1;
#   4. the supervisor, in Kitsu, retakes it with a note: the note comes
#      back as a rework, and v002 follows v001 with it mounted as an input;
#   5. v002 renders with the fill light down and is published as revision 2;
#   6. the supervisor sets it done in Kitsu; the review passes. c4-sync
#      never set a done status.
#
# KITSU_SITE (default http://127.0.0.1:8091) and KITSU_ADMIN (the site's
# admin, EMAIL:PASSWORD) name the server; a stopped container named
# kitsu-local is started and stopped again after; without one the demo
# runs one, and removes it at the end.
DEMO_NAME=kitsu FAKE=kitsuctl FAKE_ARGS='[]' FAKE_PORT=${KS_PORT:-18082}
source "$(dirname "$0")/lib.sh"
KSITE=${KITSU_SITE:-http://127.0.0.1:8091}
KADMIN=${KITSU_ADMIN:-admin@example.com:mysecretpassword}
KIMG=${KITSU_IMAGE:-cgwire/cgwire:1.0.73}
KS=$ROOT/bin/kitsuctl

eval "lib_$(declare -f down)"
down() {
  lib_down
  if had kitsu-connected; then docker network disconnect kind kitsu-local >/dev/null 2>&1; fi
  if had kitsu-started; then docker rm -f kitsu-local >/dev/null 2>&1; fi
  if had kitsu-woken; then docker stop kitsu-local >/dev/null 2>&1; fi
}

demo_start "$@"

# kitsu_site: the server, up and on the kind network, so c4-sync's pod
# reaches it; KIP is its address there.
kitsu_site() {
  log "Kitsu: $KSITE, on the kind network"
  if ! docker inspect kitsu-local >/dev/null 2>&1; then
    docker run -d --name kitsu-local -p 127.0.0.1:8091:80 -e MAIL_CHECK_DELIVERABILITY=false "$KIMG" >/dev/null
    note kitsu-started
  elif [ "$(docker inspect -f '{{.State.Running}}' kitsu-local)" != true ]; then
    docker start kitsu-local >/dev/null
    note kitsu-woken
  fi
  if [ -z "$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' kitsu-local)" ]; then
    docker network connect kind kitsu-local
    note kitsu-connected
  fi
  KIP=$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' kitsu-local)
  wait_until "Kitsu's API on $KSITE" 300 curl -sf "$KSITE/api"
  wait_until "Kitsu from the cluster's node, at $KIP" 60 docker exec "$NODE" curl -sf "http://$KIP/api"
}

kitsu_images() {
  log "images: c4-sync and $ORACLE onto $NODE only; kitsuctl here"
  make -C "$ROOT" image-sync >/dev/null
  go build -o "$KS" "$DEMO/kitsuctl"
  docker exec "$NODE" crictl images 2>/dev/null | grep -q "${ORACLE%%:*}" || note oracle-loaded
  local img
  for img in c4-sync:dev "$ORACLE"; do
    "$KIND" load docker-image "$img" --name "$CLUSTER" --nodes "$NODE" >/dev/null
  done
}

demo_hud
kitsu_images
demo_project
demo_scene frames.sh sh040-scene
kitsu_site
DUE=$(TZ=America/Los_Angeles date -v+4d +%F 2>/dev/null || TZ=America/Los_Angeles date -d '+4 days' +%F)
SEED=$("$KS" seed --site "$KSITE" --admin "$KADMIN" --due "$DUE") || fail "seeding Kitsu"
printf '%s\n' "$SEED" >"$SHOTS/seed.json"
get() { echo "$SEED" | jq_py "print(d$1)"; }
TASK=$(get '["Task"]')
PROD=$(get '["ProjectName"]')
DANA="$(get '["Supervisor"]["Email"]'):$(get '["Supervisor"]["Password"]')"
demo_sync kitsu-bot token "$(get '["BotToken"]')"
k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Tracker
metadata: {name: kitsu, namespace: $NS}
spec:
  kind: kitsu
  endpoint: http://$KIP
  scope: {project: "$PROD", eta_field: c4_eta, link_field: c4_link}
  credential: {secret: {name: kitsu-bot, key: token}}
  events: {mode: eventlog, pollSeconds: 10, resyncSeconds: 300}
  rate: {perSecond: "5", burst: 20}
  people: {"$(get '["Supervisor"]["Email"]')": dana}
  intake:
  - match: {type: Task, fields: {task_type: Lighting, task_status: ready, entity.type: Shot}}
    as: job
    deadline: {dateOnlyAt: "14:00", timeZone: America/Los_Angeles}
    rework: nextVersion
    job:
      class: render
      params: {frames: "1001-1012", chunk: "4", order: sequential}
      review: {server: "person:dana", ask: "Dailies: approve sh040 lighting?", choices: [approve, rework, accept], expectSeconds: 1800}
      pools: [$DEMO_NAME]
      inputs: [{materialization: sh040-scene, mountPath: /show}]
      template:
        spec:
          securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
          containers:
          - name: render
            image: $ORACLE
            imagePullPolicy: IfNotPresent
            command: [sh, /show/frames.sh]
            securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
  statusOut: {fieldSeconds: 30}
EOF
mark "up" "cluster, project, Kitsu at $KIP, c4-sync ready; production '$PROD', task $TASK due $DUE"

# --- run ------------------------------------------------------------------

state_json() { "$KS" state --site "$KSITE" --admin "$KADMIN" --task "$TASK"; }
task_status_is() { [ "$(state_json | jq_py 'print(d["Status"])')" = "$1" ]; }
revisions_ready() { [ "$(state_json | jq_py 'print(sum(1 for c in d["Comments"] for p in (c["Previews"] or []) if p.endswith(":ready")))')" = "$1" ]; }
# capture STEP JOB: the HUD as the wrangler and the supervisor, and the
# Kitsu task as JSON (its web app needs a sign-in a headless capture lacks).
capture() {
  shot "$1-hud-queue" "$HUD/?as=dana&static=1#/queue" 1600 1000
  shot "$1-hud-job" "$HUD/?as=wrangler&static=1#/job/$NS/$2" 1600 1300
  shot "$1-hud-supervisor" "$HUD/?as=dana&static=1#/job/$NS/$2" 1600 1300
  state_json >"$SHOTS/$1-kitsu.json"
}

JOB=$(k -n "$NS" get trackerlinks -o jsonpath='{.items[0].spec.target.workJob}' 2>/dev/null)
has_link() { JOB=$(k -n "$NS" get trackerlinks -o jsonpath='{.items[0].spec.target.workJob}' 2>/dev/null) && [ -n "$JOB" ]; }
wait_until "c4-sync to make a job from task $TASK" 120 has_link
wait_until "job $JOB" 60 has_job "$JOB"
deadline=$(k -n "$NS" get workjob "$JOB" -o jsonpath='{.spec.deadline} {.spec.deadlineMode}')
mark "1 intake" "task $TASK became $JOB, deadline $deadline"
capture 1-intake "$JOB"

wait_until "frames of $JOB running" 300 units_running "$JOB"
wait_until "the task in progress in Kitsu" 60 task_status_is wip
mark "2 rendering" "frames running; task wip by a C4 comment, ETA in its data"
capture 2-rendering "$JOB"

wait_until "v001 published to Kitsu with its preview" 600 revisions_ready 1
wait_until "the task waiting for approval in Kitsu" 60 task_status_is wfa
mark "3 v001 in review" "a comment in wfa with the output view's C4 ID; revision 1 is the middle frame"
capture 3-v001-review "$JOB"

"$KS" comment --site "$KSITE" --as "$DANA" --task "$TASK" --status retake \
  --text "Fill light too hot, 1001 to 1012. Bring it down a stop; keep the rim as is." >/dev/null
JOB2=$JOB-v002
wait_until "the retake relayed as a rework, and $JOB2 submitted" 120 has_job "$JOB2"
mark "4 retake to rework" "Dana's retake and note became a rework; $JOB2 follows $JOB with the note mounted"
capture 4-rework "$JOB2"

wait_until "v002 published to Kitsu with its preview" 600 revisions_ready 2
mark "5 v002 in review" "revision 2, fill light down"
capture 5-v002-review "$JOB2"

"$KS" comment --site "$KSITE" --as "$DANA" --task "$TASK" --status done --text "Approved." >/dev/null
wait_until "$JOB2 to finish once the done serves its review" 180 phase_is "$JOB2" Succeeded
mark "6 approved" "Dana set the task done in Kitsu; the review passed; $JOB2 Succeeded"
capture 6-approved "$JOB2"

log "checks"
state_json >"$SHOTS/kitsu-state.json"
k -n "$NS" get trackerlinks -o yaml >"$SHOTS/trackerlinks.yaml"
k -n "$NS" get workjob "$JOB" "$JOB2" -o yaml >"$SHOTS/workjobs.yaml"
python3 - "$SHOTS/kitsu-state.json" <<'PY'
import json, re, sys
d = json.load(open(sys.argv[1]))
cs = d["Comments"]
bot = [c for c in cs if c["By"] == "C4 Sync"]
pubs = [c for c in bot if "Output view: " in c["Text"]]
assert len(pubs) == 2, [c["Text"] for c in bot]
ids = [re.search(r"Output view: (c4\w+)", c["Text"]).group(1) for c in pubs]
assert all(len(i) == 90 for i in ids) and ids[0] != ids[1], ids
assert [c["Previews"] for c in pubs] == [["r1:ready"], ["r2:ready"]], [c["Previews"] for c in pubs]
assert not {c["Status"] for c in bot} & {"done", "retake"}, "c4-sync set a judgment"
assert d["Status"] == "done", "the supervisor's done must stand"
assert "c4_eta" in (d["Data"] or {}) and "c4_link" in (d["Data"] or {}), d["Data"]
print("kitsu state ok: two revisions ready, two distinct C4 IDs, Dana's retake and done stand, c4-sync wrote work states only")
PY
lineage "$JOB" "$JOB2" "kitsu/Task/$TASK"
mark "checks" "revisions 1 and 2 ready with distinct C4 IDs; $JOB2 follows $JOB, both carry the task; c4-sync never set retake or done"
log "demo done: $SHOTS"
