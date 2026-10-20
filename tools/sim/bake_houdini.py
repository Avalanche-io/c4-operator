"""Render a Houdini cache node over a frame range, run by hython in sim.sh:

    hython bake_houdini.py SCENE.hip NODE START END
"""

import sys

import hou

scene, node, start, end = sys.argv[1:5]
hou.hipFile.load(scene)
n = hou.node(node)
if n is None:
    print(f"c4_fail: permanent the scene has no node {node}", flush=True)
    raise SystemExit(2)
n.render(frame_range=(int(start), int(end)))
