"""c4submit's fake farm over HTTP on localhost, with the karma, houdini-rop
and pdg-cook tools, for trying the C4 nodes in a real Houdini without a
farm (CHECKLIST.md). Runs until interrupted:

    python3 integrations/dcc/houdini/tests/serve_fake_farm.py [--token TOKEN]

Then start Houdini with C4_HUD set to the address it prints.
"""

import argparse
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.normpath(os.path.join(HERE, "..", "..", "c4submit", "src")))
sys.path.insert(0, HERE)

from c4submit import yamlio  # noqa: E402
from c4submit.testing import KARMA, SALTMARSH, FakeFarm, serve  # noqa: E402
from houdini_scenes import HOUDINI_ROP  # noqa: E402


def main() -> int:
    ap = argparse.ArgumentParser(description="c4submit's fake farm over HTTP on localhost")
    ap.add_argument("--token", default="", help="the bearer token it asks for (none by default)")
    ap.add_argument("--project", default="saltmarsh")
    args = ap.parse_args()
    with open(os.path.join(HERE, "..", "tools", "pdg-cook", "tool.yaml"), encoding="utf-8") as f:
        pdg_cook = yamlio.load(f.read())
    farm = FakeFarm(tools=[KARMA, HOUDINI_ROP, pdg_cook], projects={args.project: dict(SALTMARSH, name=args.project)})
    served = serve(farm, token=args.token)
    print(f"fake farm at {served.url} (project {args.project}); Ctrl-C to stop", flush=True)
    try:
        while True:
            time.sleep(3600)
    except KeyboardInterrupt:
        pass
    finally:
        served.close()
        print(f"{len(farm.jobs)} submission(s), {len(farm.store)} file(s) stored")
    return 0


if __name__ == "__main__":
    sys.exit(main())
