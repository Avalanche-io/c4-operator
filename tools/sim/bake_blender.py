"""Bake a Mantaflow domain to OpenVDB, run by Blender in sim.sh:

    blender -b SCENE.blend --python-exit-code 1 --python bake_blender.py -- DOMAIN CACHE START END
"""

import sys

import bpy

domain, cache, start, end = sys.argv[sys.argv.index("--") + 1:]
s = bpy.context.scene
s.frame_start, s.frame_end = int(start), int(end)
dom = bpy.data.objects.get(domain)
fluid = dom.modifiers.get("Fluid") if dom else None
if fluid is None or fluid.fluid_type != "DOMAIN":
    print(f"c4_fail: permanent the scene has no object {domain} with a Fluid domain modifier", flush=True)
    raise SystemExit(2)
bpy.context.view_layer.objects.active = dom  # baking works on the active domain
d = fluid.domain_settings
d.cache_directory, d.cache_data_format = cache, "OPENVDB"
d.cache_frame_start, d.cache_frame_end = s.frame_start, s.frame_end
if "FINISHED" not in bpy.ops.fluid.bake_all():
    print(f"c4_fail: retry Blender did not finish baking {domain}", flush=True)
    raise SystemExit(1)
