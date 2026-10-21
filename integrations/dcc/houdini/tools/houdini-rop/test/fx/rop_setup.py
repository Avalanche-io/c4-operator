# The houdini-rop tool's test scene, built in hython because a binary hip
# file does not belong in git: a box that rises a little each frame, and a
# Geometry ROP /out/cache writing it to $HIP/geo/box.$F4.bgeo.sc over
# frames 1001 to 1002. houdini_rop.py runs this with the hip file to save
# as its one argument, then opens that file and renders /out/cache.
import sys

import hou

hip = sys.argv[1]
hou.hipFile.clear(suppress_save_prompt=True)
geo = hou.node("/obj").createNode("geo", "box")
box = geo.createNode("box", "box1")
move = geo.createNode("xform", "rise")
move.setInput(0, box)
move.parm("ty").setExpression("($F - 1001) * 0.1")
move.setDisplayFlag(True)
move.setRenderFlag(True)
rop = hou.node("/out").createNode("geometry", "cache")
rop.parm("soppath").set(move.path())
rop.parm("sopoutput").set("$HIP/geo/box.$F4.bgeo.sc")
rop.parm("trange").set(1)
rop.parmTuple("f").set((1001, 1002, 1))
hou.hipFile.save(hip)
