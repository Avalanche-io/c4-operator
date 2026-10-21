# The pdg-cook tool's test scene, built in hython because a binary hip file
# does not belong in git: a TOP network /obj/tops whose Generic Generator
# makes three work items and whose Python Script TOP (the output node)
# writes pdg/item.<index>.txt beside this file for each. pdg_cook.py runs
# this with the hip file to save as its one argument, then opens that file
# and cooks /obj/tops.
import os
import sys

import hou

hip = sys.argv[1]
out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "pdg")
hou.hipFile.clear(suppress_save_prompt=True)
tops = hou.node("/obj").createNode("topnet", "tops")
gen = tops.createNode("genericgenerator", "three")
gen.parm("itemcount").set(3)
write = tops.createNode("pythonscript", "write")
write.setInput(0, gen)
write.parm("script").set(
    "import os\n"
    f"out = {out!r}\n"
    "os.makedirs(out, exist_ok=True)\n"
    "with open(os.path.join(out, 'item.%d.txt' % work_item.index), 'w') as f:\n"
    "    f.write('cooked %d\\n' % work_item.index)\n")
write.setDisplayFlag(True)
hou.hipFile.save(hip)
