"""Scenes and tools for the tests, built in the fake maya over real files."""

import os

import fakemaya as fm
from c4submit.testing import write

from c4_maya.host import MayaHost

MAYA_RENDER = {
    "c4": "tool/1", "name": "maya-render", "title": "Maya render",
    "summary": "Renders a Maya scene's render layer over frames.",
    "kind": "frames",
    "runs": {"exe": "Render",
             "command": ["{{Run.Exe}}", "-r", "{{Param.renderer}}", "-rl", "{{Param.layer}}", "-cam", "{{Param.camera}}",
                         "-s", "{{Unit.FrameStart}}", "-e", "{{Unit.FrameEnd}}", "{{Param.scene}}"]},
    "frames": {"chunk": 4, "order": "binary"},
    "params": [
        {"name": "scene", "type": "PATH", "objectType": "FILE", "dataFlow": "IN", "from": "scene.file",
         "userInterface": {"control": "CHOOSE_INPUT_FILE", "label": "Scene",
                           "fileFilters": [{"label": "Maya scenes", "patterns": ["*.ma", "*.mb"]}]}},
        {"name": "layer", "type": "STRING", "default": "defaultRenderLayer", "from": "scene.renderables",
         "userInterface": {"label": "Render layer"}},
        {"name": "camera", "type": "STRING", "from": "scene.camera", "userInterface": {"label": "Camera"}},
        {"name": "renderer", "type": "STRING", "default": "arnold", "allowedValues": ["arnold", "mayaSoftware"],
         "userInterface": {"control": "DROPDOWN_LIST", "label": "Renderer"}},
        {"name": "threads", "type": "INT", "default": 0, "minValue": 0, "maxValue": 128, "show": "advanced",
         "affectsOutput": False},
        {"name": "verbose", "type": "BOOL", "default": False, "show": "advanced",
         "userInterface": {"label": "Verbose log"}},
    ],
    "needs": {"cpu": 16, "memory": "64G"},   # no licences: the farm refuses tools that need them for now
    "reuse": "exact",
}


def light_scene(root, modified: bool = False, master: bool = False):
    """A lighting scene: the heron (UDIM textures, a rig reference, an
    Alembic cache), water with a caustics sequence, a splash standin
    sequence, fog from a VDB sequence and a sky. Render Setup layers
    beauty (heron, water, splash, sky) and fg (heron, fog, sky, its own
    range 1001-1010); an old layer not renderable; the master layer
    renderable only when ``master``."""
    show = str(root)
    ws = os.path.join(show, "seq", "sm0140", "maya")
    scene = write(os.path.join(ws, "scenes", "sm0140_light_v003.ma"), b"//Maya ASCII 2025 scene\n")
    tex = os.path.join(show, "assets", "heron", "tex")
    for tile in (1001, 1002, 1011):
        write(os.path.join(tex, f"heron_diffuse.{tile}.exr"), b"tile %d" % tile)
    for f in range(1, 11):
        write(os.path.join(ws, "sourceimages", "caustics", f"caustic.{f:04d}.exr"), b"caustic %d" % f)
    rig = write(os.path.join(show, "assets", "heron", "heron_rig.ma"), b"//Maya ASCII rig\n")
    for f in range(1001, 1049):
        write(os.path.join(show, "fx", "splash", f"splash.{f}.ass"), b"splash %d" % f)
    for f in range(1001, 1011):
        write(os.path.join(show, "fx", "fog", f"fog.{f}.vdb"), b"fog %d" % f)
    abc = write(os.path.join(show, "anim", "sm0140_heron_anim.abc"), b"abc")
    sky = write(os.path.join(show, "assets", "hdri", "sky.exr"), b"sky")
    ocio = write(os.path.join(show, "color", "config.ocio"), b"ocio_profile_version: 2\n")

    fm.reset(scene, 1001, 1048, ws=ws, modified=modified)
    fm.state().nodes["defaultRenderLayer"].attrs["renderable"] = master
    fm.camera("shotCam")
    fm.mesh("heron")
    fm.mesh("water")
    fm.standin("splash", os.path.join(show, "fx", "splash", "splash.1001.ass"), sequence=True)
    fm.volume("fog", os.path.join(show, "fx", "fog", "fog.####.vdb"), sequence=True)
    fm.node("transform", "skyDome")
    fm.node("aiSkyDomeLight", "skyDomeShape", "skyDome")
    fm.file_texture("heron_diffuse", os.path.join(tex, "heron_diffuse.1001.exr"), ["heronShape"], tiling=3,
                    pattern=os.path.join(tex, "heron_diffuse.<UDIM>.exr"))
    fm.file_texture("caustics", "sourceimages/caustics/caustic.0001.exr", ["waterShape"], sequence=True)
    fm.ai_image("skyImage", sky, feeds="skyDomeShape")
    fm.alembic("heron_anim_AlembicNode", abc, feeds="heronShape")
    fm.reference("heronRN", rig)
    fm.listed(ocio)
    fm.layer("rs_beauty", members=["heron", "water", "splash", "skyDome"])
    fm.layer("rs_fg", members=["heron", "fog", "skyDome"], startFrame=1001, endFrame=1010)
    fm.layer("rs_old", renderable=False, members=["water"])
    fm.state().modified = modified
    env = {"C4_PROJECT": "saltmarsh", "C4_SHOT": "sm0140", "C4_ROOT": show}
    return MayaHost(env=env), scene
