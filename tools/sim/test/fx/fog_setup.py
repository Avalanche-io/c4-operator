# The sim tool's test scene, from Saltmarsh sm0220's fog_setup.py made small: low
# fog over the water, a Mantaflow smoke domain at resolution 16 over three
# frames. The sim tool runs it with the .blend to write after --.
import sys
import bpy

out = sys.argv[sys.argv.index("--") + 1]
bpy.ops.wm.read_factory_settings(use_empty=True)
s = bpy.context.scene
s.frame_start, s.frame_end = 1001, 1003
bpy.ops.mesh.primitive_cube_add(size=1, location=(0, 0, 1.5))
dom = bpy.context.object
dom.name = "FogDomain"
dom.scale = (12, 6, 1.5)
m = dom.modifiers.new("Fluid", "FLUID")
m.fluid_type = "DOMAIN"
ds = m.domain_settings
ds.domain_type = "GAS"
ds.resolution_max = 16
ds.cache_data_format = "OPENVDB"
ds.cache_type = "ALL"
ds.cache_frame_start, ds.cache_frame_end = 1001, 1003
bpy.ops.mesh.primitive_plane_add(size=1, location=(0, 0, 1.0))  # inside the domain (z 0.75 to 2.25)
src = bpy.context.object
src.name = "FogSource"
src.scale = (10, 4, 1)
f = src.modifiers.new("Fluid", "FLUID")
f.fluid_type = "FLOW"
f.flow_settings.flow_type = "SMOKE"
f.flow_settings.flow_behavior = "INFLOW"
f.flow_settings.smoke_color = (0.8, 0.8, 0.85)
f.flow_settings.temperature = -0.2
bpy.context.view_layer.objects.active = dom  # baking works on the active domain
bpy.ops.wm.save_as_mainfile(filepath=out)
