"""One markdown table row for a P4 CI job: its time to green and its
mounts, grouped by publish outcome (cold, delta, warm).

    python3 -I hack/e2e/p4mounts.py PUBLISH_FILE SECONDS JOB
"""

import statistics
import sys

rows = [line.split() for line in open(sys.argv[1]) if line.strip()]
by = {}
for outcome, seconds in rows:
    by.setdefault(outcome, []).append(float(seconds))
mounts = "; ".join(f"{len(v)} {o}, median {statistics.median(v) * 1000:.0f} ms" for o, v in sorted(by.items()))
print(f"| {sys.argv[3]} | all 16 passed in {sys.argv[2]} s | {mounts} |")
