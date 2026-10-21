"""c4submit end to end against a real farm: what every DCC panel does, with
a small comp scene in place of an application.

    python integrations/dcc/c4submit/e2e_submit.py --hud http://127.0.0.1:30180 --as artist

It writes a four-frame scene to a temporary folder (an OpenImageIO comp
script and four render frames), checks it with the farm (a dry run, with
the estimate and slack), submits it (uploading what the farm lacks,
recording the tree and refusing if the farm names it differently from the
ID computed here), waits for the job, and lists the comp frames in its
output. The farm needs its prebuilt `comp` tool offered (`c4 farm up`
publishes it and offers it once its test passes). Exit 0 when the job
succeeded with its four frames.

Run it against a farm of your own (`c4 farm up --name ...`): it creates a
job and stores objects.
"""

import argparse
import json
import os
import shutil
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "src"))

from c4submit import summary  # noqa: E402
from c4submit.collect import NoCache  # noqa: E402
from c4submit.farm import Farm, FarmError  # noqa: E402
from c4submit.host import Dependency  # noqa: E402
from c4submit.submitter import Draft, Submitter, prepare  # noqa: E402
from c4submit.testing import FakeHost, write  # noqa: E402

# A render frame the comp tool's own test reads.
PLATE = os.path.join(HERE, "..", "..", "..", "tools", "comp", "test", "render", "test.1001.exr")
COMP = b"""#!/bin/sh
# A small OpenImageIO comp: each render frame, graded a little warmer.
set -eu
mkdir -p "$C4_TREE/comp"
for f in $(seq "$C4_FRAME_START" "$C4_FRAME_END"); do
  oiiotool "$C4_TREE/render/sm0140.$f.exr" --mulc 1.08,1.04,0.98 -d half -o "$C4_TREE/comp/sm0140.$f.exr"
done
"""


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--hud", required=True, help="the farm's HUD URL")
    ap.add_argument("--as", dest="viewer", default="", help="a development viewer, for a local farm")
    ap.add_argument("--project", default="", help="the project (default the viewer's first)")
    ap.add_argument("--wait", type=int, default=900, help="seconds to wait for the job")
    a = ap.parse_args()

    farm = Farm(base=a.hud, viewer=a.viewer)
    project = a.project or json.loads(farm.request("GET", "/snapshot"))["viewer"]["projects"][0]
    show = tempfile.mkdtemp(prefix="c4submit-e2e-")
    try:
        script = write(os.path.join(show, "comp", "sm0140_comp.sh"), COMP)
        for f in range(1001, 1005):
            os.makedirs(os.path.join(show, "render"), exist_ok=True)
            shutil.copy(PLATE, os.path.join(show, "render", f"sm0140.{f}.exr"))
        host = FakeHost(scene=script, frames="1001-1004",
                        deps=[Dependency(os.path.join(show, "render", "sm0140.####.exr"), "1001-1004", node="Read1")],
                        env={"C4_PROJECT": project, "C4_SHOT": "sm0140", "C4_ROOT": show})
        s = Submitter(farm, cache=NoCache())
        comp = next((t for t in s.tools_for(project, script) if t.name == "comp"), None)
        if comp is None:
            print(f"{project} offers no comp tool yet (c4 farm tools)")
            return 1
        d = Draft(host, comp, project=farm.project(project))
        d.due = "+2h"
        c = s.check(prepare(d))
        print(summary.text(c))
        if not c.ok:
            return 1
        out = s.submit(c)
        job = out.answer["jobs"][0]
        ns, name = job["namespace"], job["name"]
        print(f"submitted {ns}/{name}; tree {c.tree_id[:12]}... named the same by the farm; "
              f"{out.uploaded} file(s) sent, the farm had the rest")
        end, j = time.time() + a.wait, {}
        while time.time() < end:
            try:
                j = json.loads(farm.request("GET", f"/jobs/{ns}/{name}"))
            except FarmError as e:
                if e.status != 404:   # a job just made can take a moment to be readable
                    raise
            if j.get("phase") in ("Succeeded", "Failed"):
                break
            time.sleep(5)
        print("phase:", j.get("phase"))
        view = (j.get("output") or {}).get("view")
        if j.get("phase") != "Succeeded" or not view:
            return 2
        files = farm.json("GET", f"/views/{project}/{view}/files", query={"path": "comp"})["files"]
        frames = sorted(f["path"] for f in files if f["path"].endswith(".exr"))
        print("output:", ", ".join(frames))
        return 0 if len(frames) == 4 else 2
    finally:
        shutil.rmtree(show, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
