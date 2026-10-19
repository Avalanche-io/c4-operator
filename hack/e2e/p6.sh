#!/usr/bin/env bash
# P6: the agent interface on the live farm. A client starts the MCP server
# (c4-operator mcp) over stdio, lists its tools, answers the evaluation set
# (docs/agents/eval.xml) through tool calls the way a reference agent
# would, and checks the answers. It also makes one change through MCP and
# reads it back through the HUD, and times the calls.
#
#   hack/e2e/p6.sh        (after hack/e2e/p5.sh and hack/e2e/p7.sh)
source "$(dirname "$0")/lib.sh"

farm_manager
go build -o "$ROOT/bin/mcpcheck" "$ROOT/hack/e2e/mcpcheck"
SERVER="$OP mcp --hud http://127.0.0.1:$HUD_PORT --as wrangler"

record ""
record "## P6: the agent interface ($(date -u +%Y-%m-%dT%H:%MZ), build $BUILD)"
record ""

log "answer the evaluation set through MCP"
"$ROOT/bin/mcpcheck" --server "$SERVER" \
  'c4_trace_file {"namespace": "show-a", "name": "sh050-denoise-v001", "path": "renders/sh050-denoised/sh050-denoised.1004.exr"}' \
  'c4_trace_file {"namespace": "show-a", "name": "sh050-render-v001", "path": "renders/sh050/sh050.1004.exr"}' \
  'c4_get_job {"namespace": "show-a", "name": "sh010-comp-v003"}' \
  'c4_list_nodes {"pool": "spot"}' \
  'c4_list_nodes {"pool": "gpu"}' \
  'c4_get_job {"namespace": "show-b", "name": "train-denoise"}' \
  'c4_get_job {"namespace": "show-b", "name": "train-upres"}' \
  'c4_capacity_and_cost {}' \
  'c4_farm_status {}' \
  'c4_list_jobs {"at_risk_only": true}' >"$OUT/p6-calls.jsonl"

python3 -I - "$OUT/p6-calls.jsonl" "$ROOT/docs/agents/eval.xml" >"$OUT/p6-eval.md" <<'PY'
import json, sys, xml.etree.ElementTree as ET
lines = [json.loads(l) for l in open(sys.argv[1])]
tools = lines[0]["tools"]
calls = lines[1:]
res = [c["result"] for c in calls]
denoise, render, sh010, spot, gpu, tden, tup, cost = res[:8]
def src(t):
    for a in t["c2pa"]["assertions"]:
        if a["label"] == "c2pa.actions.v2":
            return a["data"]["actions"][0]["digitalSourceType"].rsplit("/", 1)[1]
def reclaimed():
    for j, name in ((tden, "train-denoise"), (tup, "train-upres")):
        for y in (j.get("lens") or {}).get("yields") or []:
            if y["reason"] == "reclaim-nominal":
                return name
cpu = next(p for p in cost["pools"] if p["name"] == "cpu")
heavy = sh010.get("heavyRegions") or [{}]
answers = [
    denoise["lineage"][0].get("producedBy"),
    "{},{}".format(denoise["statement"]["predicate"]["ai"]["model"], denoise["statement"]["predicate"]["ai"]["oversight"]),
    src(render),
    render["file"]["id"],
    str(sh010["output"]["changesets"]),
    "{}-{}".format(heavy[0].get("from"), heavy[0].get("to")),
    ",".join(sorted(n["name"] for n in spot["nodes"])),
    str(gpu["nodes"][0]["slots"]),
    reclaimed(),
    "{:.2f}".format(cpu["costPerSlotHour"]),
]
qa = ET.parse(sys.argv[2]).getroot().findall("qa_pair")
ok = 0
print("| # | question | expected | answered through MCP | match |")
print("|---|---|---|---|---|")
for i, (q, got) in enumerate(zip(qa, answers), 1):
    want = q.find("answer").text.strip()
    good = str(got) == want
    ok += good
    short = q.find("question").text.strip().split("?")[0][:90]
    shown = want if len(want) < 40 else want[:12] + "..."
    gshown = str(got) if len(str(got)) < 40 else str(got)[:12] + "..."
    print(f"| {i} | {short}? | {shown} | {gshown} | {'yes' if good else 'NO'} |")
print()
ms = [c["ms"] for c in calls]
print(f"{ok} of {len(qa)} answers match. {len(tools)} tools listed; {len(calls)} calls answered in {min(ms)} to {max(ms)} ms each (median {sorted(ms)[len(ms)//2]} ms), including the HUD round trip.")
if ok != len(qa):
    sys.exit(1)
PY
while IFS= read -r line; do record "$line"; done <"$OUT/p6-eval.md"
record ""

log "a change through MCP, read back through the HUD"
"$ROOT/bin/mcpcheck" --server "$SERVER" \
  'c4_hold_job {"namespace": "show-a", "name": "sh010-comp-v003", "hold": true}' \
  'c4_hold_job {"namespace": "show-a", "name": "sh010-comp-v003", "hold": false}' >"$OUT/p6-control.jsonl"
EV=$(k -n show-a get events --field-selector involvedObject.name=sh010-comp-v003,reason=Nudged -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' | tail -2 | tr '\n' ';')
python3 -I -c '
import json,sys
for l in list(open(sys.argv[1]))[1:]:
    d=json.loads(l); print(d["tool"], d["result"]["message"])' "$OUT/p6-control.jsonl" | while IFS= read -r line; do record "- $line"; done
record "- Events recorded on the job: $EV"
record ""
echo "P6 PASS"
