#!/usr/bin/env bash
# P5: the work queue on the e2e farm. Two shows share 16 cpu slots, 8 GPU
# slots and up to 8 burst slots on spot nodes that start unprovisioned.
# Renders with deadlines, a CI change with a latency target, and two
# checkpointing training runs arrive together; an urgent GPU render and a
# late lookdev render arrive later. The run measures what the queue did:
# deadlines met, reclaim by yield and resume from the captured view, burst
# requested and provisioned, the binary-order estimate, and each job's
# output view folded from its units' changesets.
#
#   hack/e2e/p5.sh        (after hack/e2e/up.sh)
source "$(dirname "$0")/lib.sh"

POLICY=${POLICY:-objective}
LOG=$OUT/p5-timeline.csv
SHOTS=${SHOTS:-1}

# rfc3339 SECONDS: a time SECONDS from now, in UTC.
rfc3339() { perl -MPOSIX=strftime -e 'print strftime("%Y-%m-%dT%H:%M:%SZ", gmtime(time + $ARGV[0]))' "$1"; }

# workjob NS NAME CLASS DEADLINE_S POOLS PARAMS CMD...: PARAMS is a flow map
# body, POOLS a flow list body; DEADLINE_S 0 means none.
workjob() {
  local ns=$1 name=$2 cls=$3 dl=$4 pools=$5 params=$6
  shift 6
  local cmd="" a dline="" mount=/show
  for a in "$@"; do cmd="$cmd, \"$a\""; done
  [ "$dl" != 0 ] && dline="  deadline: \"$(rfc3339 "$dl")\""
  [ "$cls" = ml ] && mount=/data
  k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: WorkJob
metadata: {name: $name, namespace: $ns}
spec:
  class: $cls
  params: {$params}
$dline
  pools: [$pools]
  inputs: [{materialization: p5-show, mountPath: $mount}]
  template:
    spec:
      securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
      - name: work
        image: c4-e2e:dev
        imagePullPolicy: IfNotPresent
        command: [${cmd#, }]
        securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
EOF
}

jobs_csv() { # one line per job: ns,name,class,phase,done,total,deadline,started,finished,preemptions,slack
  k get workjobs -A -o json | python3 -I -c '
import json,sys
for j in json.load(sys.stdin)["items"]:
    m,s,st=j["metadata"],j["spec"],j.get("status",{})
    u=st.get("units",{})
    print(",".join(str(x) for x in [m["namespace"],m["name"],s["class"],st.get("phase",""),u.get("succeeded",0),u.get("total",0),
        st.get("deadline",""),st.get("started",""),st.get("finished",""),st.get("preemptions",0),(st.get("estimate") or {}).get("slackSeconds","")]))'
}

record ""
record "## P5: the work queue on the e2e farm ($(date -u +%Y-%m-%dT%H:%MZ), policy $POLICY)"
record ""

log "farm"
k -n c4-system patch deploy c4-manager --type=json -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":[\"manager\",\"--capacity-provider=kind-sim\",\"--work-policy=$POLICY\",\"--hud-dev-viewers=wrangler=*;artist=show-a\",\"--tz=America/Los_Angeles\"]}]" >/dev/null
for ns in show-a show-b; do k -n "$ns" delete workjobs --all --wait=true >/dev/null 2>&1 || true; done
k delete capacityrequests --all >/dev/null 2>&1 || true
for n in $CLUSTER-worker4 $CLUSTER-worker8; do
  k taint node "$n" c4.avalanche.io/unprovisioned=true:NoSchedule --overwrite >/dev/null
done
k apply -f "$ROOT/hack/e2e/work.yaml" -f "$ROOT/hack/e2e/hud.yaml" >/dev/null
cold_start
k -n c4-system rollout status deploy/c4-manager --timeout=180s >/dev/null
for p in show-a show-b; do
  k patch project "$p" --type=merge -p '{"spec":{"work":{"quotas":[{"pool":"cpu","nominal":8,"borrowingLimit":8},{"pool":"gpu","nominal":4,"borrowingLimit":4},{"pool":"spot","nominal":0,"borrowingLimit":8}],"burstBudgetPerDay":"5","lateCostPerHour":"200"}}}' >/dev/null
done
rm -rf "$OUT/p5-show"
"$TOOL" gen-show --out "$OUT/p5-show" --frame-kib 256 >/dev/null
SHOW=$(ingest show-a "$OUT/p5-show")
ingest show-b "$OUT/p5-show" >/dev/null
for ns in show-a show-b; do
  k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata: {name: p5-show, namespace: $ns}
spec:
  source: {id: "$SHOW"}
EOF
  mat_view "$ns" p5-show >/dev/null
done
until curl -sf http://127.0.0.1:30080/api/v1/snapshot >/dev/null; do sleep 1; done

log "t=0: renders, a CI change and two training runs"
T0=$(now)
workjob show-b train-denoise ml 0 gpu 'ranks: "4", steps: "1200", checkpointEvery: "50", estimateSeconds: "250", turnaround: "24h"' c4e2e train --dir /data/train --step 200ms
workjob show-b train-upres ml 0 gpu 'ranks: "4", steps: "1200", checkpointEvery: "50", estimateSeconds: "250", turnaround: "24h"' c4e2e train --dir /data/train --step 200ms
workjob show-a sh010-comp-v003 render 420 cpu 'frames: "1001-1096", order: binary, estimateSeconds: "8"' c4e2e frame --out /show/renders/sh010 --cost 6s --heavy 1040-1052:4
workjob show-b sh020-light-v002 render 540 cpu 'frames: "1001-1072", order: binary, estimateSeconds: "8"' c4e2e frame --out /show/renders/sh020 --cost 8s
workjob show-a ci-change-4521 ci 0 cpu 'shards: "16", latencyTarget: "4m", estimateSeconds: "20", change: "4521"' c4e2e shard --mean 20s --flaky 0.1

: >"$LOG"
echo "t,job,phase,succeeded,total,slack,eta50" >"$LOG"
echo "t,requested,state,provisioned,spot_units" >"$OUT/p5-capacity.csv"
: >"$OUT/p5-resume-raw.txt"
: >"$OUT/p5-sh010-samples.jsonl"
sample() {
  local t
  t=$(since "$T0")
  # The binary-order job's unit states and estimate, to score the
  # estimator against what the units turned out to cost.
  k -n show-a get workjob sh010-comp-v003 -o json 2>/dev/null |
    python3 -I -c 'import json,sys; j=json.load(sys.stdin); st=j.get("status",{}); print(json.dumps({"t":float(sys.argv[1]),"unitStates":st.get("unitStates",{}),"estimate":st.get("estimate") or {}}))' "$t" >>"$OUT/p5-sh010-samples.jsonl" || true
  # Resumed training attempts say where they started; their pods are gone
  # once they finish, so keep the line now.
  k -n show-b logs -l c4.avalanche.io/job --prefix --tail=3 2>/dev/null | grep 'training from step' | grep -v 'from step 0 ' >>"$OUT/p5-resume-raw.txt" || true
  echo "$t,$(k get capacityrequest spot -o jsonpath='{.spec.slots},{.status.state},{.status.provisionedSlots}' 2>/dev/null || echo ',,'),$(k get pods -A -l c4.avalanche.io/pool=spot --field-selector=status.phase=Running --no-headers 2>/dev/null | wc -l | tr -d ' ')" >>"$OUT/p5-capacity.csv"
  k get workjobs -A -o json | python3 -I -c '
import json,sys
t=sys.argv[1]
for j in json.load(sys.stdin)["items"]:
    st=j.get("status",{}); e=st.get("estimate") or {}; u=st.get("units",{})
    print(",".join(str(x) for x in [t,j["metadata"]["namespace"]+"/"+j["metadata"]["name"],st.get("phase",""),u.get("succeeded",0),u.get("total",0),e.get("slackSeconds",""),e.get("eta50","")]))' "$t" >>"$LOG"
}
wait_until() { # SECONDS: sample every 5 s until SECONDS after T0
  while [ "$(perl -e "print $(now) - $T0 < $1 ? 1 : 0")" = 1 ]; do
    sample
    sleep 5
  done
}

wait_until 60
log "t=60: an urgent GPU render for show-a"
workjob show-a sh030-fx-v001 render 200 gpu 'frames: "1001-1024", order: binary, estimateSeconds: "10"' c4e2e frame --out /show/renders/sh030 --cost 10s
wait_until 90
log "t=90: a late lookdev render for show-b that may burst to spot"
workjob show-b sh040-lookdev-v001 render 150 'cpu, spot' 'frames: "1001-1144", order: binary, estimateSeconds: "15"' c4e2e frame --out /show/renders/sh040 --cost 15s
wait_until 120
if [ "$SHOTS" = 1 ]; then
  log "HUD screenshots, mid-run"
  "$ROOT/hack/e2e/hud-shots.sh" mid queue workers tiers cost job:show-a/sh010-comp-v003 job:show-b/train-denoise job:show-a/ci-change-4521 artist >/dev/null
fi

log "until every job ends"
while :; do
  sample
  left=$(jobs_csv | awk -F, '$4 != "Succeeded" && $4 != "Failed"' | wc -l | tr -d ' ')
  [ "$left" = 0 ] && break
  [ "$(perl -e "print $(now) - $T0 > 1200 ? 1 : 0")" = 1 ] && fail "jobs still running after 20 minutes: $(jobs_csv | awk -F, '$4 != "Succeeded"')"
  sleep 5
done
WALL=$(since "$T0")
if [ "$SHOTS" = 1 ]; then
  "$ROOT/hack/e2e/hud-shots.sh" end queue cost job:show-a/sh010-comp-v003 job:show-b/sh040-lookdev-v001 graph >/dev/null
fi

log "results"
jobs_csv >"$OUT/p5-jobs.csv"
k -n show-b get workjob train-denoise train-upres -o json >"$OUT/p5-ml.json"
sort -u "$OUT/p5-resume-raw.txt" | sed -E 's#^\[pod/([a-z-]+)-u[0-9]+-[a-z0-9]+/[a-z]+\] #\1: #' >"$OUT/p5-resume.txt"
k -n show-a get workjob sh010-comp-v003 -o json >"$OUT/p5-sh010.json"
curl -s 'http://127.0.0.1:30080/api/v1/snapshot?as=wrangler' >"$OUT/p5-snapshot.json"
python3 -I "$ROOT/hack/e2e/p5report.py" "$OUT" "$T0" "$WALL" | while IFS= read -r line; do record "$line"; done
echo "P5 PASS"
