# The blender-render tool's test scene: scene sh010 with view layers bg and
# fg, a camera on a cube over a plane, 64x36, Cycles at 4 samples on the CPU,
# output //render/sh010_ as PNG. blender-render.sh runs it with the .blend
# to write after --.
import sys

import bpy

out = sys.argv[sys.argv.index("--") + 1]
bpy.ops.wm.read_factory_settings(use_empty=True)
s = bpy.context.scene
s.name = "sh010"
s.frame_start, s.frame_end = 1001, 1002
s.view_layers[0].name = "bg"
s.view_layers.new("fg")
bpy.ops.mesh.primitive_plane_add(size=8)
bpy.ops.mesh.primitive_cube_add(size=1, location=(0, 0, 0.5))
cube = bpy.context.object
cube.keyframe_insert("location", frame=1001)
cube.location.x = 1
cube.keyframe_insert("location", frame=1002)
bpy.ops.object.light_add(type="SUN", rotation=(0.6, 0.2, 0.4))
bpy.ops.object.camera_add(location=(6, -6, 4), rotation=(1.1, 0, 0.785))
cam = bpy.context.object
cam.name = "CAM_sh010"
s.camera = cam
r = s.render
r.engine = "CYCLES"
s.cycles.device = "CPU"
s.cycles.samples = 16
r.resolution_x, r.resolution_y, r.resolution_percentage = 64, 36, 100
r.image_settings.file_format = "PNG"
r.filepath = "//render/sh010_"
bpy.ops.wm.save_as_mainfile(filepath=out, relative_remap=False)
