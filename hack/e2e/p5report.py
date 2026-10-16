"""Summarize a P5 run as markdown, from the files hack/e2e/p5.sh leaves in
hack/e2e/out: p5-jobs.csv, p5-timeline.csv, p5-capacity.csv, p5-ml.json,
p5-resume.txt, p5-sh010.json, p5-snapshot.json.

    python3 -I hack/e2e/p5report.py OUT T0 WALL
"""

import csv
import datetime
import json
import os
import sys

out, t0, wall = sys.argv[1], float(sys.argv[2]), float(sys.argv[3])
SUBMIT = {"sh030-fx-v001": 60, "sh040-lookdev-v001": 90}


def ts(s):
    if not s:
        return None
    return datetime.datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc).timestamp()


def rel(x):
    return "-" if x is None else f"{x - t0:+.0f} s"


print(
    "Pools: cpu (4 nodes, 4 slots each), gpu (2 nodes, 4 slots each), and spot "
    "(2 nodes, 4 slots each, unprovisioned until a CapacityRequest asks for them; "
    "a simulated capacity provider brings them up after a 20 s provisioning delay). Each show "
    "has a quota of 8 nominal cpu slots plus 8 borrowable, 4 + 4 gpu slots, and 0 + 8 "
    "spot slots, a burst budget of $5 a day, and lateness priced at $200 an hour. "
    f"Times are seconds after the first submission; the run took {wall:.0f} s."
)
print()
print("| job | class | submitted | deadline | finished | deadline met | units done | preemptions |")
print("|---|---|---|---|---|---|---|---|")
order = ["sh010-comp-v003", "sh020-light-v002", "ci-change-4521", "train-denoise", "train-upres", "sh030-fx-v001", "sh040-lookdev-v001"]
# Only this scenario's jobs: other phases leave theirs in other namespaces.
rows = [r for r in csv.reader(open(os.path.join(out, "p5-jobs.csv"))) if r[0] in ("show-a", "show-b") and r[1] in order]
rows.sort(key=lambda r: order.index(r[1]))
met = missed = 0
for ns, name, cls, phase, done, total, dl, started, fin, pre, slack in rows:
    d, f = ts(dl), ts(fin)
    if d and f:
        ok = f <= d
        met += ok
        missed += not ok
        verdict = f"{'yes' if ok else 'no'}, {d - f:+.0f} s"
    elif d:
        verdict = "no (unfinished)"
        missed += 1
    else:
        verdict = "no deadline"
    print(f"| {ns}/{name} | {cls} | +{SUBMIT.get(name, 0)} s | {rel(d)} | {rel(f)} | {verdict} | {done} of {total} | {pre} |")
print()
print(f"Deadlines met: {met} of {met + missed}. The CI change's deadline is its latency target (4 minutes) from when it was submitted.")
print()

# Reclaim by yield and resume from the captured view.
ml = json.load(open(os.path.join(out, "p5-ml.json")))
yields = [(j["metadata"]["name"], y) for j in ml["items"] for y in j["status"].get("yields", [])]
resume = [l.strip() for l in open(os.path.join(out, "p5-resume.txt")) if l.strip()]
if yields:
    print(f"Reclaim: {len(yields)} yield{'s' if len(yields) != 1 else ''} of training runs for the urgent GPU render.")
    for name, y in yields:
        print(f"- {name} yielded ({y['reason']}) at {y['at']}, its checkpoint captured as view {y['view'][:10]}...")
    for l in resume:
        print(f"- {l.split(':', 1)[0]} resumed: \"{l.split(':', 1)[1].strip()}\", reading the checkpoint from that view.")
else:
    print("Reclaim: no training run had to yield.")
print()

# Burst.
cap = list(csv.DictReader(open(os.path.join(out, "p5-capacity.csv"))))
first_req = next((float(r["t"]) for r in cap if r["requested"] not in ("", "0")), None)
first_prov = next((float(r["t"]) for r in cap if r["provisioned"] not in ("", "0")), None)
first_unit = next((float(r["t"]) for r in cap if r["spot_units"] not in ("", "0")), None)
peak = max((int(r["requested"] or 0) for r in cap), default=0)
snap = json.load(open(os.path.join(out, "p5-snapshot.json")))
spend = sum(p["burstSpendToday"] for p in snap["projects"])
if first_req is not None:
    print(
        f"Burst: the queue first asked for spot capacity at {first_req:.0f} s (peak request {peak} slots); "
        f"slots were provisioned by {first_prov if first_prov is None else round(first_prov)} s and the first unit ran on spot "
        f"at {first_unit if first_unit is None else round(first_unit)} s. Burst spend for the run: ${spend:.2f}."
    )
else:
    print("Burst: no CapacityRequest was needed.")
print()

# The binary-order estimate on sh010: the estimator's remaining work W
# (slot-seconds, before allocation) against the work the units turned out
# to need, and the estimate a render manager shows (the mean of finished
# frames times the frames left), at the same moments.
def decode(s):
    out = []
    for part in (s or "").split(","):
        if not part:
            continue
        a, _, b = part.partition("-")
        out.extend(range(int(a), int(b or a) + 1))
    return out

final = json.load(open(os.path.join(out, "p5-sh010.json")))["status"]
millis = final.get("unitMillis") or []
samples = [json.loads(l) for l in open(os.path.join(out, "p5-sh010-samples.jsonl")) if l.strip()]
running_samples = [x for x in samples if x["unitStates"].get("succeeded") and x["estimate"].get("remainingWork")]
if millis and running_samples:
    first, last = running_samples[0]["t"], running_samples[-1]["t"]
    print("Estimate of sh010-comp-v003 (binary order; frames cost 6 s, and 24 s from 1040 to 1052). W is the estimator's remaining work in slot-seconds; the truth is what the units not yet finished went on to take (a running unit counts half). The render-manager estimate is the mean of finished frames times the frames left:")
    print()
    print("| point in the run | frames done | W (estimator) | W90 | truth | estimator error | render-manager error |")
    print("|---|---|---|---|---|---|---|")
    for share in (0.1, 0.25, 0.5, 0.75):
        t = first + share * (last - first)
        x = min(running_samples, key=lambda r: abs(r["t"] - t))
        done = set(decode(x["unitStates"].get("succeeded")))
        running = set(decode(x["unitStates"].get("running")))
        truth = sum(m for i, m in enumerate(millis) if i not in done) / 1000 - sum(millis[i] for i in running if i < len(millis)) / 2000
        w, w90 = x["estimate"]["remainingWork"], x["estimate"].get("remainingWorkP90", 0)
        mean = sum(millis[i] for i in done if i < len(millis)) / 1000 / max(1, len(done))
        naive = mean * (len(millis) - len(done) - len(running) / 2)
        err = lambda e: f"{abs(e - truth) / max(1.0, truth):.0%}"
        print(f"| {share:.0%} | {len(done)} of {len(millis)} | {w:.0f} | {w90:.0f} | {truth:.0f} | {err(w)} | {err(naive)} |")
    print()

o = json.load(open(os.path.join(out, "p5-sh010.json")))["status"].get("output") or {}
if o:
    print(
        f"Output: sh010-comp-v003 folded {o.get('changesets', 0)} unit changesets into its output view {o.get('view', '')[:10]}...; "
        f"the stored chain {o.get('chain', '')[:10]}... has one section per fold, each sealed by the output view after it."
    )
    if o.get("conflicts"):
        print(f"Conflicts reported: {len(o['conflicts'])}.")
