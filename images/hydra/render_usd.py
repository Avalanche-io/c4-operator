"""Blender script for c4-render-usd: import a USD stage, render frames with
Cycles on the CPU to single-layer 32-bit OpenEXR, one file per frame.

    blender -b --factory-startup --python render_usd.py -- \
        --stage S.usda --output out/S.####.exr --start 1001 --end 1004 --width 1920
"""

import argparse
import sys

import bpy  # provided by Blender


def main(argv):
    p = argparse.ArgumentParser()
    p.add_argument("--stage", required=True)
    p.add_argument("--output", required=True)
    p.add_argument("--start", type=int, required=True)
    p.add_argument("--end", type=int, required=True)
    p.add_argument("--width", type=int, default=1920)
    a = p.parse_args(argv)

    bpy.ops.wm.read_factory_settings(use_empty=True)
    bpy.ops.wm.usd_import(filepath=a.stage, import_cameras=True, import_lights=True, import_materials=True)

    scene = bpy.context.scene
    cams = [o for o in scene.objects if o.type == "CAMERA"]
    if not cams:
        sys.exit("c4-render-usd: the stage has no camera")
    scene.camera = cams[0]

    scene.render.engine = "CYCLES"
    scene.cycles.device = "CPU"
    aspect = scene.render.resolution_y / max(1, scene.render.resolution_x)
    scene.render.resolution_x = a.width
    scene.render.resolution_y = max(1, round(a.width * aspect))
    scene.render.resolution_percentage = 100
    scene.render.image_settings.file_format = "OPEN_EXR"
    scene.render.image_settings.color_depth = "32"
    scene.render.image_settings.exr_codec = "ZIP"
    scene.render.use_file_extension = False
    # Blender pads the frame where the path holds #, like usdrecord
    scene.render.filepath = a.output
    scene.frame_start, scene.frame_end = a.start, a.end
    bpy.ops.render.render(animation=True)


if __name__ == "__main__":
    main(sys.argv[sys.argv.index("--") + 1:] if "--" in sys.argv else [])
