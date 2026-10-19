#!/usr/bin/env bash
# P7: provenance, with a signer per entity (DESIGN.md section 9.2). A plate
# set is ingested by a workload that signs an ingest record as its own
# service account. A render reads it and a model-assisted denoise reads the
# render's output; each unit's task record is signed by the node daemon that
# ran it, each input's resolution by the controller, each job's record and
# scheduling decisions by the queue, and the release of the held denoise,
# made by an agent through MCP, by the HUD on the agent's behalf. The run
# walks the chain of custody from a denoised frame back to the ingest,
# verifies every link against the farm's root, shows the signing CA is out
# of every other workload's reach, and shows that a changed byte breaks a
# record.
#
#   hack/e2e/p7.sh        (after hack/e2e/up.sh; P5 leaves the pools set up)
source "$(dirname "$0")/lib.sh"

HUD=http://127.0.0.1:$HUD_PORT/api/v1
SIGNER=http://127.0.0.1:$SIGNER_PORT
go build -o "$ROOT/bin/mcpcheck" "$ROOT/hack/e2e/mcpcheck"
get() { # PATH: GET from the HUD as the wrangler
  local sep='?'
  case "$1" in *\?*) sep='&' ;; esac
  curl -sf "$HUD/$1${sep}as=wrangler"
}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

record ""
record "## P7: provenance, a signer per entity ($(date -u +%Y-%m-%dT%H:%MZ), build $BUILD)"
record ""

farm_manager
k apply -f "$ROOT/hack/e2e/hud.yaml" >/dev/null
log "the certificate issuer"
until [ "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$SIGNER/v1/certificate")" = 401 ]; do sleep 1; done
k -n c4-system get configmap c4-provenance -o jsonpath='{.data.ca\.crt}' >"$OUT/p7-roots.pem"
k -n c4-system get configmap c4-provenance -o json | python3 -I -c '
import json,sys
for k,v in json.load(sys.stdin)["data"].items():
    if k != "ca.crt": print(f"{k}={v}")' >"$OUT/p7-legacy-keys.txt"
ROOT_FP=$(openssl x509 -in "$OUT/p7-roots.pem" -noout -fingerprint -sha256 | cut -d= -f2 | tr -d : | cut -c1-16)

log "the issuer refuses what it should"
for sa in show-a:ingest show-b:ingest; do
  k -n "${sa%%:*}" create serviceaccount "${sa##*:}" --dry-run=client -o yaml | k apply -f - >/dev/null
done
k -n show-a create token ingest --audience c4-signer --duration 10m >"$TMP/token-a"
k -n show-b create token ingest --audience c4-signer --duration 10m >"$TMP/token-b"
k -n show-a create token ingest --duration 10m >"$TMP/token-wrong-audience"
k -n c4-system create token c4-regional --audience c4-signer --duration 10m >"$TMP/token-regional"
issue() { # TOKENFILE: the issuer's status for a request with that token
  curl -s -o "$TMP/issue.out" -w '%{http_code}' -X POST "$SIGNER/v1/certificate" -H "Authorization: Bearer $(cat "$1")" \
    -H 'Content-Type: application/json' -d '{"publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}'
}
WRONG=$(issue "$TMP/token-wrong-audience")
REGIONAL=$(issue "$TMP/token-regional")
[ "$WRONG" = 403 ] || fail "a token for another audience got $WRONG"
[ "$REGIONAL" = 403 ] || fail "the regional tier's token got $REGIONAL"
record "The issuer refused a service account token minted for another audience ($WRONG) and the regional tier's own token ($REGIONAL: the regional tier signs nothing)."
record ""

log "a signed ingest"
rm -rf "$OUT/p7-plates"
"$TOOL" gen-shot --out "$OUT/p7-plates" --name sh050 --frames 8 --frame-kib 64 --seed 50 >/dev/null
# shellcheck disable=SC2046
"$OP" ingest $(shard_args show-a host) --signer "$SIGNER" --sign-token-file "$TMP/token-a" \
  --record-out "$TMP/ingest-record" "$OUT/p7-plates" >"$TMP/ingest.out"
PLATES=$(sed -n 's/^view //p' "$TMP/ingest.out")
INGEST=$(cat "$TMP/ingest-record")
# The same plates vouched for by show-b's workload, stored in show-a's store.
# shellcheck disable=SC2046
"$OP" ingest $(shard_args show-a host) --signer "$SIGNER" --sign-token-file "$TMP/token-b" \
  --record-out "$TMP/foreign-record" "$OUT/p7-plates" >/dev/null
FOREIGN=$(cat "$TMP/foreign-record")
k -n show-a delete materialization p7-plates p7-plates-foreign --ignore-not-found --wait=true >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata:
  name: p7-plates
  namespace: show-a
  annotations: {c4.avalanche.io/source-record: "$INGEST"}
spec:
  source: {id: "$PLATES"}
---
apiVersion: c4.avalanche.io/v1alpha1
kind: Materialization
metadata:
  name: p7-plates-foreign
  namespace: show-a
  annotations: {c4.avalanche.io/source-record: "$FOREIGN"}
spec:
  source: {id: "$PLATES"}
EOF
mat_view show-a p7-plates >/dev/null
k -n show-a wait --for=jsonpath='{.status.phase}'=Failed materialization/p7-plates-foreign --timeout=60s >/dev/null ||
  fail "a record from show-b's workload vouched for show-a's plates"
FOREIGN_WHY=$(k -n show-a get materialization p7-plates-foreign -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')
[ "$(k -n show-a get materialization p7-plates -o jsonpath='{.status.sourceRecord}')" = "$INGEST" ] || fail "p7-plates does not link its ingest record"
record "Plates ingested by \`sa/show-a/ingest\` resolve with the ingest record linked; the same plates vouched for by \`sa/show-b/ingest\` are refused ($FOREIGN_WHY)."
record ""

log "a render, then a held, model-assisted denoise of its output"
k -n show-a delete workjob sh050-render-v002 sh050-denoise-v002 --ignore-not-found --wait=true >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: WorkJob
metadata: {name: sh050-render-v002, namespace: show-a}
spec:
  class: render
  params: {frames: "1001-1008", order: binary, estimateSeconds: "3"}
  inputs: [{materialization: p7-plates, mountPath: /show}]
  template:
    spec:
      securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
      - name: render
        image: c4-e2e:dev
        imagePullPolicy: IfNotPresent
        command: [c4e2e, frame, --out, /show/renders/sh050, --cost, 2s]
        securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
---
apiVersion: c4.avalanche.io/v1alpha1
kind: WorkJob
metadata:
  name: sh050-denoise-v002
  namespace: show-a
  annotations:
    c4.avalanche.io/ai-usage: '{"generated": "assisted", "model": "denoise-v3", "oversight": "human_validated", "consent": ["plates-release-2026-031"]}'
spec:
  class: render
  dependsOn: [sh050-render-v002]
  nudge: {hold: true}
  params: {frames: "1001-1008", order: binary, estimateSeconds: "2"}
  inputs: [{fromJob: sh050-render-v002, mountPath: /show}]
  template:
    spec:
      securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
      containers:
      - name: denoise
        image: c4-e2e:dev
        imagePullPolicy: IfNotPresent
        command: [c4e2e, frame, --out, /show/renders/sh050-denoised, --cost, 1s]
        securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
EOF
k -n show-a wait --for=jsonpath='{.status.phase}'=Succeeded workjob/sh050-render-v002 --timeout=600s >/dev/null ||
  fail "render: $(k -n show-a get workjob sh050-render-v002 -o jsonpath='{.status.phase} {.status.conditions}')"
until [ -n "$(k -n show-a get workjob sh050-render-v002 -o jsonpath='{.status.output.record}')" ]; do sleep 1; done

log "an agent releases the denoise through MCP"
"$ROOT/bin/mcpcheck" --server "$OP mcp --hud http://127.0.0.1:$HUD_PORT --as wrangler" \
  'c4_hold_job {"namespace": "show-a", "name": "sh050-denoise-v002", "hold": false}' >"$OUT/p7-mcp-release.jsonl"
CONTROL=$(k -n show-a get workjob sh050-denoise-v002 -o jsonpath='{.metadata.annotations.c4\.avalanche\.io/control-records}')
[ -n "$CONTROL" ] || fail "the release through MCP left no control record"
k -n show-a wait --for=jsonpath='{.status.phase}'=Succeeded workjob/sh050-denoise-v002 --timeout=600s >/dev/null ||
  fail "denoise: $(k -n show-a get workjob sh050-denoise-v002 -o jsonpath='{.status.phase} {.status.conditions}')"
until [ -n "$(k -n show-a get workjob sh050-denoise-v002 -o jsonpath='{.status.output.record}')" ]; do sleep 1; done

log "the chain of custody"
FRAME=renders/sh050-denoised/sh050-denoised.1004.exr
for i in $(seq 1 30); do
  get "custody/show-a/sh050-denoise-v002?path=$FRAME" >"$OUT/p7-custody.json" && break
  sleep 1
done
get "trace/show-a/sh050-denoise-v002?path=$FRAME" >"$OUT/p7-trace-denoise.json"
get "trace/show-a/sh050-render-v002?path=renders/sh050/sh050.1004.exr" >"$OUT/p7-trace-render.json"
python3 -I - "$OUT" "$INGEST" <<'PY' >"$OUT/p7-custody.md"
import json, sys
out, ingest = sys.argv[1], sys.argv[2]
c = json.load(open(f"{out}/p7-custody.json"))
base = "https://cccc.io/id/"
signers = [s.removeprefix(base) for s in c["signers"]]
kinds = {}
for l in c["links"]:
    kinds.setdefault(l["kind"], []).append(l)
assert c["verified"] and c["complete"], (c["gaps"], [l for l in c["links"] if l.get("problem") or l.get("policy")])
assert any(s.startswith("node/") for s in signers) and "component/controller" in signers and "component/queue" in signers
assert "component/hud" in signers and "sa/show-a/ingest" in signers, signers
assert kinds["ingest"][0]["record"] == ingest
nodes = sorted({s for s in signers if s.startswith("node/")})
assert len(nodes) >= 2, nodes
print(f"Custody of `{c['file']['path']}`: verified {c['verified']}, complete {c['complete']}, {len(c['links'])} links, {len(signers)} signers.")
print()
print("| link | signed by | what it vouches for |")
print("|---|---|---|")
for l in c["links"]:
    who = (l.get("signer") or "").removeprefix(base) or "(summary)"
    print(f"| {l['kind']} | {who} | {l['about']} |")
print()
print(f"Task records were signed by {len(nodes)} node identities: {', '.join(nodes)}.")
r = json.load(open(f"{out}/p7-trace-render.json"))
d = json.load(open(f"{out}/p7-trace-denoise.json"))
def src(t):
    for a in t["c2pa"]["assertions"]:
        if a["label"] == "c2pa.actions.v2":
            return a["data"]["actions"][0]["digitalSourceType"].rsplit("/", 1)[1]
assert r["verified"] and d["verified"] and not r.get("policy") and not d.get("policy")
assert src(r) == "algorithmicMedia" and src(d) == "compositeWithTrainedAlgorithmicMedia"
print(f"C2PA digitalSourceType: the render's frame {src(r)}, the denoised frame {src(d)}.")
PY
while IFS= read -r line; do record "$line"; done <"$OUT/p7-custody.md"
record ""

log "the same verdict through MCP"
"$ROOT/bin/mcpcheck" --server "$OP mcp --hud http://127.0.0.1:$HUD_PORT --as wrangler" \
  "c4_verify_custody {\"namespace\": \"show-a\", \"name\": \"sh050-denoise-v002\", \"path\": \"$FRAME\"}" >"$OUT/p7-mcp.jsonl"
python3 -I - "$OUT/p7-mcp.jsonl" <<'PY2' | while IFS= read -r line; do record "$line"; done
import json, sys
call = [json.loads(l) for l in open(sys.argv[1])][1]
r = call["result"]
assert not call["isError"] and r["verified"] and r["complete"]
print("An agent asking the MCP server (c4_verify_custody, as the wrangler) got the same verdict in {} ms (one tool call on an open session, from the request to the answer, including the HUD reading and verifying the job's records from the store): verified {}, complete {}, {} signers.".format(
    call["ms"], r["verified"], r["complete"], len(r["signers"])))
PY2
record ""

log "the signing CA is out of every other workload's reach"
CAN_NODE=$(k auth can-i get secret/c4-signing-ca -n c4-signing --as=system:serviceaccount:c4-system:c4-node 2>/dev/null || true)
CAN_TASK=$(k auth can-i get secret/c4-signing-ca -n c4-signing --as=system:serviceaccount:show-a:default 2>/dev/null || true)
CAN_MGR=$(k auth can-i get secret/c4-signing-ca -n c4-signing --as=system:serviceaccount:c4-system:c4-manager 2>/dev/null || true)
MOUNTS=$(k get pods -A -o json | python3 -I -c '
import json,sys
pods=json.load(sys.stdin)["items"]
print(len([1 for p in pods for v in p["spec"].get("volumes",[]) if (v.get("secret") or {}).get("secretName") in ("c4-signing-ca","c4-provenance-key")]))')
[ "$CAN_NODE" = no ] && [ "$CAN_TASK" = no ] && [ "$CAN_MGR" = yes ] && [ "$MOUNTS" = 0 ] ||
  fail "CA access: node $CAN_NODE, task $CAN_TASK, manager $CAN_MGR, pods mounting a signing secret $MOUNTS"
record "The signing CA (Secret \`c4-signing-ca\` in \`c4-signing\`, root ${ROOT_FP}...) may be read by the manager: $CAN_MGR; by node daemons: $CAN_NODE; by a task's service account: $CAN_TASK. Pods mounting a signing secret: $MOUNTS. Each node daemon's key never leaves its process."
record ""

log "one changed byte"
REC=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["record"])' "$OUT/p7-trace-denoise.json")
get "objects/show-a/$REC" >"$OUT/p7-record.json"
GOOD=$("$TOOL" verify-record --roots "$OUT/p7-roots.pem" "$OUT/p7-record.json")
python3 -I - "$OUT/p7-record.json" "$OUT/p7-record-forged.json" <<'PY'
import base64, json, sys
b = json.load(open(sys.argv[1]))
st = json.loads(base64.b64decode(b["dsseEnvelope"]["payload"]))
st["predicate"]["ai"]["generated"] = "none"  # hide the model's part
b["dsseEnvelope"]["payload"] = base64.b64encode(json.dumps(st, separators=(",", ":")).encode()).decode()
json.dump(b, open(sys.argv[2], "w"))
PY
BAD=$("$TOOL" verify-record --roots "$OUT/p7-roots.pem" "$OUT/p7-record-forged.json")
case "$GOOD" in verified:*"policy ok"*) ;; *) fail "the stored record does not verify: $GOOD" ;; esac
case "$BAD" in rejected:*) ;; *) fail "a forged record verified: $BAD" ;; esac
record "Checked outside the cluster with the root certificate only: the stored record says \"$GOOD\". The same record with its AI use changed to \"none\" says \"$BAD\"."
record ""
echo "P7 PASS"
