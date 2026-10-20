#!/usr/bin/env bash
# The GitHub agent-work demo, live (proposals/integrations LAUNCH 3.2): a
# fake GitHub repository beside the real operator and c4-sync on the e2e
# kind cluster, and HUD captures of each step. `make demo-github` runs it;
# hack/demo/lib.sh says what it adds to the cluster and how it cleans up.
#
# The agent is a script (hack/demo/scene/agent.sh, and this file for its
# GitHub actions): a real agent bundle needs model credentials, and this
# demo is about the tracker loop, not the model.
#
#   1. a maintainer's issue, labeled c4:agent in a milestone with a due
#      date, becomes an agent job whose deadline is the milestone's;
#   2. the agent works and opens a pull request that closes the issue:
#      C4's check run and commit status appear on its head, with the ETA;
#   3. the work is up for review: the check run says it waits for dana;
#   4. dana requests changes in GitHub, with an inline comment; the review
#      comes back (webhook, verified, with polling under it) as a rework,
#      and v002 follows v001 with the review mounted as an input;
#   5. the agent pushes v002 to the same pull request: a new check run on
#      the new head, waiting for review again;
#   6. dana approves in GitHub; the wait passes, the check run succeeds,
#      and the output's C4 ID is published on the pull request. C4 never
#      wrote a review or merged.
DEMO_NAME=widgets FAKE=fakegithub FAKE_PORT=${GH_PORT:-18081}
HOOK_SECRET=demo-hook-secret
FAKE_ARGS="[--token=demo-gh-token, --url=http://fakegithub.$DEMO_NAME.svc:8080, --hook=http://c4-sync.c4-system.svc/webhooks/$DEMO_NAME/github, --hook-secret=$HOOK_SECRET]"
source "$(dirname "$0")/lib.sh"
demo_start "$@"

demo_hud
demo_images
demo_project
demo_scene agent.sh agent-scene
demo_fake
DUE=$(python3 -c 'import datetime as d; t=d.datetime.now(d.timezone.utc)+d.timedelta(days=3); print(t.replace(minute=0, second=0, microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ"))')
fkpost /demo/seed "{\"due\":\"$DUE\"}" >/dev/null
demo_sync github token demo-gh-token
k -n "$NS" patch secret github --type=merge -p "{\"stringData\":{\"hook\":\"$HOOK_SECRET\"}}" >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Tracker
metadata:
  name: github
  namespace: $NS
  annotations: {c4.avalanche.io/tracker-self: "c4-sync[bot]"}
spec:
  kind: github
  endpoint: http://fakegithub.$NS.svc:8080
  scope: {repo: acme/widgets}
  credential: {secret: {name: github, key: token}}
  webhookSecret: {name: github, key: hook}
  events: {mode: webhook, pollSeconds: 10, resyncSeconds: 300}
  rate: {perSecond: "5", burst: 20}
  people: {dana: dana}
  intake:
  - match: {type: Issue, labels: [c4:agent]}
    as: job
    review: {wait: review}
    rework: nextVersion
    job:
      class: graph
      params: {root: plan}
      pools: [$DEMO_NAME]
      inputs: [{materialization: agent-scene, mountPath: /show}]
      template:
        spec:
          securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
          containers:
          - name: agent
            image: $ORACLE
            imagePullPolicy: IfNotPresent
            command: [sh, /show/agent.sh]
            securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
  statusOut:
    commitStatus: true
    fieldSeconds: 30
EOF
mark "up" "cluster, project, fake GitHub, c4-sync ready; issue #7 due $DUE"

# --- run ------------------------------------------------------------------

state_json() { fk /demo/state; }
check_title() { # SHA: the newest check run's title on SHA
  state_json | jq_py "p=[x for x in (d['Pulls'] or []) if x['head']['sha']=='$1']; c=(p[0]['Checks'] or []) if p else []; print(c[-1]['output']['title'] if c else '')"
}
check_says() { case "$(check_title "$1")" in "$2"*) return 0 ;; esac; return 1; }
has_check() { [ -n "$(check_title "$1")" ]; }
check_done() { # SHA CONCLUSION
  [ "$(state_json | jq_py "p=[x for x in (d['Pulls'] or []) if x['head']['sha']=='$1']; c=(p[0]['Checks'] or []) if p else []; print((c[-1]['conclusion'] or '') if c else '')")" = "$2" ]
}
sha() { printf 'acme/widgets %s' "$1" | shasum | cut -c1-40; }
PULL=12

JOB=github-7
wait_until "c4-sync to make job $JOB from issue #7" 120 has_job "$JOB"
deadline=$(k -n "$NS" get workjob "$JOB" -o jsonpath='{.spec.deadline} {.spec.deadlineMode}')
mark "1 intake" "issue #7 became $JOB, deadline $deadline (the milestone's)"
capture 1-intake "$JOB"

wait_until "the agent's build in $JOB running" 300 units_running "$JOB"
SHA1=$(sha v001)
fkpost /demo/pull "{\"number\":$PULL,\"title\":\"Port the tier fan-in test\",\"body\":\"Closes #7\\n\\nWork by the C4 agent bundle (a script in this demo).\",\"sha\":\"$SHA1\",\"by\":\"c4-agent\"}" >/dev/null
wait_until "C4's check run on the pull request's head" 120 has_check "$SHA1"
mark "2 working" "the agent opened PR #$PULL; C4's check run on ${SHA1:0:10}: $(check_title "$SHA1")"
capture 2-working "$JOB"

wait_until "the review wait of $JOB open" 300 wait_open "$JOB" review
wait_until "the check run to wait for dana" 60 check_says "$SHA1" "Waiting for review by dana"
mark "3 in review" "the check run waits for review by dana"
capture 3-review "$JOB"

fkpost /demo/review "{\"pull\":$PULL,\"by\":\"dana\",\"state\":\"CHANGES_REQUESTED\",\"body\":\"Close, one thing.\",\"comments\":[{\"path\":\"tier/fanin_test.go\",\"line\":42,\"body\":\"assert the order, not just the count\"}]}" >/dev/null
JOB2=$JOB-v002
wait_until "the review relayed as a rework, and $JOB2 submitted" 120 has_job "$JOB2"
mark "4 review to rework" "dana's changes-requested review became a rework; $JOB2 follows $JOB with the review mounted"
capture 4-rework "$JOB2"

wait_until "the review wait of $JOB2 open" 300 wait_open "$JOB2" review
SHA2=$(sha v002)
fkpost /demo/push "{\"number\":$PULL,\"sha\":\"$SHA2\"}" >/dev/null
wait_until "a new check run on the pushed head, waiting for dana" 120 check_says "$SHA2" "Waiting for review by dana"
mark "5 v002 in review" "the agent pushed ${SHA2:0:10} to PR #$PULL; a new check run waits for review"
capture 5-v002-review "$JOB2"

fkpost /demo/review "{\"pull\":$PULL,\"by\":\"dana\",\"state\":\"APPROVED\",\"body\":\"Ship it.\"}" >/dev/null
wait_until "$JOB2 to finish once the approval serves its wait" 180 phase_is "$JOB2" Succeeded
wait_until "the check run on ${SHA2:0:10} to succeed" 120 check_done "$SHA2" success
mark "6 approved" "dana approved in GitHub; the wait passed; $JOB2 Succeeded; the check run succeeded"
capture 6-approved "$JOB2"

log "checks"
state_json >"$SHOTS/github-state.json"
k -n "$NS" get trackerlinks -o yaml >"$SHOTS/trackerlinks.yaml"
k -n "$NS" get workjob "$JOB" "$JOB2" -o yaml >"$SHOTS/workjobs.yaml"
VIEW=$(output_view "$JOB2")
python3 - "$SHOTS/github-state.json" "$SHA2" "$VIEW" <<'PY'
import json, sys
d, sha2, view = json.load(open(sys.argv[1])), sys.argv[2], sys.argv[3]
p = d["Pulls"][0]
assert p["head"]["sha"] == sha2, p["head"]
assert [r["state"] for r in p["Reviews"] or []] == ["CHANGES_REQUESTED", "APPROVED"], "dana's reviews must stand"
c = (p["Checks"] or [{}])[-1]
assert c.get("status") == "completed" and c.get("conclusion") == "success", c
assert p["Statuses"] and p["Statuses"][-1]["state"] == "success", p["Statuses"]
assert view.startswith("c4") and any(view in x["body"] for x in p["Comments"] or []), "the output's C4 ID is published on the pull request"
assert d["Judgments"] == 0, "c4-sync wrote a review or a merge"
hooks = [x["status"] for x in d["Deliveries"] or []]
print("github state ok: changes requested then approved, check run success on the new head, output", view[:12], "published; webhooks:", hooks)
PY
lineage "$JOB" "$JOB2" "github/Issue/7"
mark "checks" "reviews stand as dana wrote them; check run and status success on ${SHA2:0:10}; output ${VIEW:0:12}… published; $JOB2 follows $JOB, both carry the issue; nothing C4 wrote was a review or a merge"
log "demo done: $SHOTS"
