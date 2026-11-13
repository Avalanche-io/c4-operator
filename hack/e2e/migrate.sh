#!/usr/bin/env bash
# The c4 pin move on a real farm (DESIGN.md section 2.1, re-identifying
# views): the e2e phases at the old pin populate a kind farm; `migrate-pin`
# runs dry over every project (reports kept); jobs are drained; the
# operator is stopped and the new CRDs applied (the old Project CRD would
# drop status.renames, the run's commit point); `migrate-pin --write`; the
# new operator; hack/e2e/migratecheck per project; a second `migrate-pin
# --write`, which must do nothing (run on the state the migration left: the
# new pin's phases wipe stores, P4 mono's); the phases at the new pin on
# the same farm. The cluster is deleted on every exit.
#
#   hack/e2e/migrate.sh OLD NEW OUT
#
# OLD and NEW are checkouts of the two pins (NEW is the one with
# migrate-pin); OUT gets every log and report. It exits 1 if anything
# failed: a phase, the CRDs, a migrate, a check, or a second migrate that
# would change something.
set -u
OLD=$(cd "$1" && pwd)
NEW=$(cd "$2" && pwd)
mkdir -p "$3"
R=$(cd "$3" && pwd)
exec > >(tee -a "$R/run.log") 2>&1
CLUSTER=${CLUSTER:-c4-operator-e2e}
export CLUSTER
PO=${E2E_PORT_OFFSET:-1000}
export AWS_ACCESS_KEY_ID=c4e2eaccess AWS_SECRET_ACCESS_KEY=c4e2esecretkey
CTX=kind-$CLUSTER
BIN=$R/bin
mkdir -p "$BIN"
failed=0
k() { kubectl --context "$CTX" "$@"; }
stamp() { date '+%H:%M:%S'; }
note() { printf '\n=== %s %s\n' "$(stamp)" "$*"; }
fail() {
  echo "FAILED: $*"
  failed=1
}

down() {
  note "teardown"
  kind delete cluster --name "$CLUSTER" >>"$R/kind-delete.log" 2>&1 || echo "kind delete failed: see kind-delete.log"
  # The phases append to the committed results; neither checkout keeps it.
  for wt in "$OLD" "$NEW"; do git -C "$wt" checkout -- hack/e2e/out 2>/dev/null; done
  echo "done $(stamp), failed=$failed"
}
trap 'down; exit $failed' EXIT
trap 'failed=1; exit' INT TERM HUP

PHASES=${PHASES:-"p1 p2 p3 p5 p4 p7 p6"}
# health TAG: whether the API server answers, and the nodes' memory.
health() {
  local ready mem
  ready=$(timeout 20 kubectl --context "$CTX" get --raw /readyz 2>&1 | tail -1)
  # shellcheck disable=SC2046 # one container ID per argument
  mem=$(timeout 30 docker stats --no-stream --format '{{.MemUsage}}' $(docker ps -q --filter "name=$CLUSTER") 2>/dev/null |
    awk '{v=$1; u=v; gsub(/[0-9.]/,"",u); gsub(/[A-Za-z]/,"",v); if (u=="GiB") v*=1024; if (u=="KiB") v/=1024; s+=v} END {printf "%.0f MiB in the nodes", s}')
  echo "health $1: readyz=${ready:-none}; $mem"
}
phases() { # CHECKOUT TAG
  local wt=$1 tag=$2 p s
  for p in $PHASES; do
    note "$tag $p"
    (cd "$wt" && hack/e2e/$p.sh) >"$R/$tag-$p.log" 2>&1
    s=$?
    echo "$tag $p: exit $s"
    [ $s -eq 0 ] || { fail "$tag $p"; tail -5 "$R/$tag-$p.log"; }
    health "after $tag $p"
  done
}

# up CHECKOUT TAG: the farm at a checkout. p5 waits for the HUD's NodePort
# before it applies hud.yaml, so the HUD and the pools are applied here.
up() {
  note "$2: up"
  (cd "$1" && hack/e2e/up.sh) >"$R/$2-up.log" 2>&1 || { fail "$2 up"; tail -20 "$R/$2-up.log"; exit 1; }
  k apply -f "$1/hack/e2e/work.yaml" -f "$1/hack/e2e/hud.yaml" >/dev/null || { fail "$2: the HUD and pools"; exit 1; }
  health "after $2 up"
}

# host_shards PROJECT: --shard flags for the project's store, reached from
# this machine (the spec's URLs are the cluster's).
host_shards() {
  k get project "$1" -o json | PO=$PO python3 -I -c '
import json, os, sys
po = int(os.environ["PO"])
for s in json.load(sys.stdin)["spec"]["store"]["shards"]:
    u = s["url"]
    for i in range(3):
        u = u.replace("http://minio-%d.c4-store:9000" % i, "http://127.0.0.1:%d" % (30900 + i + po))
    print("--shard", "%s=%s" % (s["name"], u))'
}
# P4's git-import mapping is the old checkout's, on this machine.
mapping_of() { [ "$1" = mono ] && [ -f "$OLD/hack/e2e/out/p4-map.jsonl" ] && echo "--mapping $OLD/hack/e2e/out/p4-map.jsonl"; }
projects() { k get projects -o jsonpath='{.items[*].metadata.name}'; }

migrate() { # TAG [--write]
  local tag=$1 p s
  shift
  for p in $(projects); do
    # shellcheck disable=SC2046
    "$BIN/c4-operator" migrate-pin --project "$p" --namespace c4-system $(host_shards "$p") $(mapping_of "$p") "$@" \
      >"$R/$tag-$p.out" 2>"$R/$tag-$p.err"
    s=$?
    echo "$tag $p: exit $s; $(grep -c '^rename' "$R/$tag-$p.out") rename, $(grep -c '^unfinished' "$R/$tag-$p.out") unfinished, $(grep -c '^missing' "$R/$tag-$p.out") missing; $(tail -1 "$R/$tag-$p.err")"
    # A dry run ends with a refusal while a job runs; that is its report.
    [ $s -eq 0 ] || [ "$tag" = dry ] || fail "migrate $tag $p"
  done
}

drain() { # TAG
  note "$1: drain"
  local deadline=$(($(date +%s) + 900)) live
  while :; do
    live=$(k get workjobs -A -o json | python3 -I -c '
import json, sys
for j in json.load(sys.stdin)["items"]:
    if j.get("status", {}).get("phase") not in ("Succeeded", "Failed", "Forecast"):
        print(j["metadata"]["namespace"] + "/" + j["metadata"]["name"] + " " + str(j.get("status", {}).get("phase")))')
    [ -z "$live" ] && { echo "every WorkJob finished"; return; }
    [ "$(date +%s)" -gt "$deadline" ] && break
    sleep 20
  done
  echo "unfinished after 15 minutes, deleted:"
  echo "$live"
  echo "$live" | while read -r nsn _; do k -n "${nsn%%/*}" delete workjob "${nsn#*/}" --wait=true; done
}

quiesce() {
  note "quiesce: the manager stopped"
  k -n c4-system scale deploy/c4-manager --replicas=0
  k -n c4-system wait --for=delete pod -l app.kubernetes.io/name=c4-operator,app.kubernetes.io/component=manager --timeout=180s 2>/dev/null ||
    k -n c4-system get pods
}

note "builds: migrate-pin and migratecheck from $(git -C "$NEW" rev-parse --short=8 HEAD), the old farm from $(git -C "$OLD" rev-parse --short=8 HEAD)"
(cd "$NEW" && go build -o "$BIN/c4-operator" ./cmd/c4-operator && go build -o "$BIN/migratecheck" ./hack/e2e/migratecheck) || { fail "builds"; exit 1; }

up "$OLD" old
phases "$OLD" old

note "before: the state the migration starts from"
k get materializations -A -o json >"$R/mats-before.json"
k get projects -o json >"$R/projects-before.json"
k get tools -A -o json >"$R/tools-before.json"
k get workjobs -A -o json >"$R/jobs-before.json"
# The dry run is before the new CRDs, so each one reports what it would do
# and ends with the refusal to write under the old Project CRD.
note "dry run"
migrate dry
drain drained
quiesce
note "the new CRDs"
for f in "$NEW"/config/crd/c4.avalanche.io_*.yaml; do
  k apply --server-side --force-conflicts -f "$f" >>"$R/crd.log" 2>&1 || fail "crd $f"
done
note "migrate --write"
migrate write --write
up "$NEW" new
note "checks"
for p in $(projects); do
  # shellcheck disable=SC2046
  "$BIN/migratecheck" -project "$p" $(host_shards "$p") -dry "$R/dry-$p.out" -pre "$R/mats-before.json" $(mapping_of "$p") >"$R/check-$p.out" 2>&1 ||
    fail "check $p"
  grep -E '^(ok|FAIL)' "$R/check-$p.out"
done
drain drained2
quiesce
note "a second migrate"
[ -n "$(projects)" ] || fail "no projects listed: the API server did not answer, so the second migrate did not run"
migrate again --write
for f in "$R"/again-*.out; do
  [ -e "$f" ] || continue
  if grep -qE '^(rename|table|grant|rematerialize|tool|mapping) ' "$f"; then
    grep -E '^(rename|table|grant|rematerialize|tool|mapping) ' "$f" | head -5
    fail "the second migrate is not a no-op: $(basename "$f")"
  fi
done
k -n c4-system scale deploy/c4-manager --replicas=1 >/dev/null
k -n c4-system rollout status deploy/c4-manager --timeout=180s >/dev/null || fail "the manager after the second migrate"
phases "$NEW" new
echo "end $(stamp)"
