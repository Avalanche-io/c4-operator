#!/usr/bin/env bash
# P7: provenance. A render job and a model-assisted denoise job that mounts
# the render's output view run on the farm. Every unit's capture gets a
# task record signed by the node daemon. The run traces files back through
# both jobs, checks the signatures against the farm's public key, shows the
# key is out of every task pod's reach, and shows that changing one byte of
# a record breaks it.
#
#   hack/e2e/p7.sh        (after hack/e2e/up.sh; P5 leaves the pools set up)
source "$(dirname "$0")/lib.sh"

HUD=http://127.0.0.1:30080/api/v1
go build -o "$ROOT/bin/mcpcheck" "$ROOT/hack/e2e/mcpcheck"
get() { # PATH: GET from the HUD as the wrangler
  local sep='?'
  case "$1" in *\?*) sep='&' ;; esac
  curl -sf "$HUD/$1${sep}as=wrangler"
}

record ""
record "## P7: provenance ($(date -u +%Y-%m-%dT%H:%MZ))"
record ""

farm_manager
log "the farm's signing key"
if ! k -n c4-system get secret c4-provenance-key >/dev/null 2>&1; then
  keys=$("$OP" keygen)
  k -n c4-system create secret generic c4-provenance-key --from-literal=key="$(echo "$keys" | sed -n 's/^key=//p')" >/dev/null
  k -n c4-system create configmap c4-provenance --from-literal="$(echo "$keys" | sed -n 's/^keyid=//p')=$(echo "$keys" | sed -n 's/^public=//p')" >/dev/null
  k -n c4-system rollout restart ds/c4-node >/dev/null
  k -n c4-system rollout status ds/c4-node --timeout=300s >/dev/null
fi
KID=$(k -n c4-system get configmap c4-provenance -o jsonpath='{.data}' | python3 -I -c 'import json,sys; print(next(iter(json.load(sys.stdin))))')
k -n c4-system get configmap c4-provenance -o json | python3 -I -c 'import json,sys; [print(f"{k}={v}") for k,v in json.load(sys.stdin)["data"].items()]' >"$OUT/p7-keys.txt"

log "a render, then a model-assisted denoise of its output"
k -n show-a delete workjob sh050-render-v001 sh050-denoise-v001 --ignore-not-found --wait=true >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: c4.avalanche.io/v1alpha1
kind: WorkJob
metadata: {name: sh050-render-v001, namespace: show-a}
spec:
  class: render
  params: {frames: "1001-1008", order: binary, estimateSeconds: "3"}
  inputs: [{materialization: p5-show, mountPath: /show}]
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
  name: sh050-denoise-v001
  namespace: show-a
  annotations:
    c4.avalanche.io/ai-usage: '{"generated": "assisted", "model": "denoise-v3", "oversight": "human_validated", "consent": ["plates-release-2026-031"]}'
spec:
  class: render
  dependsOn: [sh050-render-v001]
  params: {frames: "1001-1008", order: binary, estimateSeconds: "2"}
  inputs: [{fromJob: sh050-render-v001, mountPath: /show}]
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
for j in sh050-render-v001 sh050-denoise-v001; do
  k -n show-a wait --for=jsonpath='{.status.phase}'=Succeeded "workjob/$j" --timeout=600s >/dev/null ||
    fail "$j: $(k -n show-a get workjob "$j" -o jsonpath='{.status.phase} {.status.conditions}')"
done
until [ -n "$(k -n show-a get workjob sh050-denoise-v001 -o jsonpath='{.status.output.provenance}')" ]; do sleep 1; done

log "trace"
# The HUD reads the farm once a second; ask until it has the jobs' indexes.
trace() { # JOB PATH OUTFILE
  local i
  for i in $(seq 1 30); do
    get "trace/show-a/$1?path=$2" >"$3" && return 0
    sleep 1
  done
  fail "no trace for $1 $2: $(curl -s "$HUD/trace/show-a/$1?path=$2&as=wrangler")"
}
trace sh050-render-v001 renders/sh050/sh050.1004.exr "$OUT/p7-trace-render.json"
trace sh050-denoise-v001 renders/sh050-denoised/sh050-denoised.1004.exr "$OUT/p7-trace-denoise.json"
RENDER_OUT=$(k -n show-a get workjob sh050-render-v001 -o jsonpath='{.status.output.view}')
python3 -I - "$OUT" "$KID" "$RENDER_OUT" <<'PY' >"$OUT/p7-trace.md"
import json, sys
out, kid, render_out = sys.argv[1], sys.argv[2], sys.argv[3]
r = json.load(open(f"{out}/p7-trace-render.json"))
d = json.load(open(f"{out}/p7-trace-denoise.json"))
def src(t):
    for a in t["c2pa"]["assertions"]:
        if a["label"] == "c2pa.actions.v2":
            return a["data"]["actions"][0]["digitalSourceType"].rsplit("/", 1)[1]
def has(t, label):
    return any(a["label"] == label for a in t["c2pa"]["assertions"])
assert r["verified"] and d["verified"], (r.get("problem"), d.get("problem"))
assert r["signer"] == kid and d["signer"] == kid
assert d["lineage"][0]["view"] == render_out and d["lineage"][0].get("producedBy") == "show-a/sh050-render-v001", d["lineage"]
assert src(r) == "algorithmicMedia" and not has(r, "c2pa.ai-disclosure")
assert src(d) == "compositeWithTrainedAlgorithmicMedia" and has(d, "c2pa.ai-disclosure")
p = d["statement"]["predicate"]
img = p["runtime"]["images"][0]
print("| | sh050-render-v001, frame 1004 | sh050-denoise-v001, frame 1004 |")
print("|---|---|---|")
rows = [
  ("record", r["record"][:10] + "...", d["record"][:10] + "..."),
  ("signature", f"verified, key {r['signer']}", f"verified, key {d['signer']}"),
  ("unit, attempt, node", f"{r['statement']['predicate']['invocation']['unit']}, {r['statement']['predicate']['invocation']['attempt']}, {r['statement']['predicate']['runtime']['node']}",
                          f"{p['invocation']['unit']}, {p['invocation']['attempt']}, {p['runtime']['node']}"),
  ("image", r['statement']['predicate']['runtime']['images'][0]['image'] + " @" + (r['statement']['predicate']['runtime']['images'][0].get('digest') or '')[:19] + "...",
            img['image'] + " @" + (img.get('digest') or '')[:19] + "..."),
  ("input", f"p5-show {r['lineage'][0]['view'][:10]}...", f"the render's output {d['lineage'][0]['view'][:10]}..., produced by {d['lineage'][0]['producedBy']}"),
  ("file", f"{r['file']['id'][:10]}... (subject digest: c4 and the SHA-512 it encodes)", f"{d['file']['id'][:10]}..."),
  ("declared AI use", "none", f"{p['ai']['generated']}, {p['ai']['model']}, {p['ai']['oversight']}, consent {', '.join(p['ai']['consent'])}"),
  ("C2PA digitalSourceType", src(r), src(d)),
  ("C2PA AI disclosure", "no", "yes (c2pa.ai-disclosure)"),
]
for name, a, b in rows:
    print(f"| {name} | {a} | {b} |")
PY
while IFS= read -r line; do record "$line"; done <"$OUT/p7-trace.md"
record ""

log "the key is out of every task's reach"
MOUNTS=$(k get pods -A -o json | python3 -I -c '
import json,sys
pods=json.load(sys.stdin)["items"]
users=sorted({p["metadata"]["namespace"]+"/"+p["metadata"]["labels"].get("app.kubernetes.io/component","?") for p in pods
  for v in p["spec"].get("volumes",[]) if (v.get("secret") or {}).get("secretName")=="c4-provenance-key"})
print(", ".join(users))')
[ "$MOUNTS" = "c4-system/node" ] || fail "the signing key is mounted by $MOUNTS"
WHO=$(k auth can-i get secret/c4-provenance-key -n c4-system --as=system:serviceaccount:show-a:default 2>/dev/null || true)
record "The signing key (Secret \`c4-provenance-key\`) is mounted only by \`$MOUNTS\` pods; a task's service account may read it: $WHO."
record ""

log "one changed byte"
REC=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["record"])' "$OUT/p7-trace-denoise.json")
get "objects/show-a/$REC" >"$OUT/p7-record.dsse.json"
GOOD=$("$TOOL" verify-record --keys "$OUT/p7-keys.txt" "$OUT/p7-record.dsse.json")
python3 -I - "$OUT/p7-record.dsse.json" "$OUT/p7-record-forged.dsse.json" <<'PY'
import base64, json, sys
e = json.load(open(sys.argv[1]))
p = base64.b64decode(e["payload"]).decode()
st = json.loads(p)
st["predicate"]["ai"]["generated"] = "none"  # hide the model's part
e["payload"] = base64.b64encode(json.dumps(st, separators=(",", ":")).encode()).decode()
json.dump(e, open(sys.argv[2], "w"))
PY
BAD=$("$TOOL" verify-record --keys "$OUT/p7-keys.txt" "$OUT/p7-record-forged.dsse.json")
case "$GOOD" in verified:*) ;; *) fail "the stored record does not verify: $GOOD" ;; esac
case "$BAD" in rejected:*) ;; *) fail "a forged record verified: $BAD" ;; esac
record "Checked outside the cluster with the public key only: the stored record says \"$GOOD\". The same record with its AI use changed to \"none\" and the signature kept says \"$BAD\"."
record ""

log "the same trace through the MCP server"
"$ROOT/bin/mcpcheck" --server "$OP mcp --hud http://127.0.0.1:30080 --as wrangler" \
  'c4_trace_file {"namespace": "show-a", "name": "sh050-denoise-v001", "path": "renders/sh050-denoised/sh050-denoised.1004.exr"}' >"$OUT/p7-mcp.jsonl"
python3 -I - "$OUT/p7-mcp.jsonl" <<'PY2' | while IFS= read -r line; do record "$line"; done
import json, sys
lines = [json.loads(l) for l in open(sys.argv[1])]
call = lines[1]
r = call["result"]
assert not call["isError"] and r["verified"] and r["lineage"][0]["producedBy"] == "show-a/sh050-render-v001"
print("An agent asking the MCP server (c4_trace_file, over stdio, as the wrangler) got the same record in {} ms: verified {}, input produced by {}.".format(
    call["ms"], r["verified"], r["lineage"][0]["producedBy"]))
PY2
record ""
echo "P7 PASS"
