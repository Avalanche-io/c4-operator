"""Blender files for the tests, built in the fake bpy over real files."""

import copy
import os

import fakebpy as bf
from c4submit.testing import NUKE_COMP, SALTMARSH, FakeFarm, write

#: A tool that renders one view layer of a Blender file over frames, as a
#: studio would publish it.
BLENDER_RENDER = {
    "c4": "tool/1", "name": "blender-render", "title": "Blender render",
    "summary": "Renders one view layer of a Blender file over frames.",
    "kind": "frames",
    "runs": {"image": "registry.petrelfinch.example/blender:4.2", "exe": "blender",
             "command": ["{{Run.Exe}}", "-b", "{{Param.blend}}", "--python-expr", "...", "-s", "{{Unit.FrameStart}}",
                         "-e", "{{Unit.FrameEnd}}", "-a"]},
    "frames": {"chunk": 4, "order": "binary"},
    "params": [
        {"name": "blend", "type": "PATH", "objectType": "FILE", "dataFlow": "IN", "from": "scene.file",
         "userInterface": {"control": "CHOOSE_INPUT_FILE", "label": "Blender file",
                           "fileFilters": [{"label": "Blender", "patterns": ["*.blend"]}]}},
        {"name": "scene", "type": "STRING", "default": "Scene", "from": "scene.renderables",
         "description": "The scene to render.", "userInterface": {"label": "Scene"}},
        {"name": "view_layer", "type": "STRING", "default": "ViewLayer", "from": "scene.renderables",
         "description": "The view layer of the scene to render.", "userInterface": {"label": "View layer"}},
        {"name": "engine", "type": "STRING", "default": "CYCLES",
         "allowedValues": ["CYCLES", "BLENDER_EEVEE_NEXT", "BLENDER_WORKBENCH"],
         "userInterface": {"control": "DROPDOWN_LIST", "label": "Render engine"}},
        {"name": "camera", "type": "STRING", "default": "", "from": "scene.camera", "show": "advanced"},
        {"name": "samples", "type": "INT", "default": 0, "minValue": 0, "maxValue": 65536, "show": "advanced",
         "userInterface": {"label": "Samples (0 keeps the file's)"}},
        {"name": "denoise", "type": "BOOL", "default": True, "show": "advanced"},
    ],
    "needs": {"cpu": 16, "memory": "32G", "gpu": 1, "sizes": {"heavy": {"memory": "96G"}}},
    "reuse": "exact",
}


def farm(tools=None) -> FakeFarm:
    return FakeFarm(tools=tools if tools is not None else [BLENDER_RENDER, NUKE_COMP],
                    projects={"saltmarsh": dict(copy.deepcopy(SALTMARSH), root="/show")})


def env(monkeypatch, show: str, shot: str = "sh010") -> None:
    monkeypatch.setenv("C4_PROJECT", "saltmarsh")
    monkeypatch.setenv("C4_SHOT", shot)
    monkeypatch.setenv("C4_ROOT", show)


def _material(name, image, user=None, node="ShaderNodeTexImage"):
    n = bf.Node(node, image=image, image_user=user or bf.image_user())
    return bf.ID(name, use_nodes=True, node_tree=bf.NodeTree(name, nodes=[n]))


def lighting(root, modified: bool = False):
    """sh010's lighting file: scene sh010 with view layers bg and fg (and a
    disabled one, old), and scene sh010_comp whose compositor reads sh010's
    render. It reads a UDIM texture set, a texture sequence, a texture by a
    ``//`` path, a linked library and a texture inside it, an Alembic cache,
    an OpenVDB sequence, a font, a sound, a reference image no render node
    uses, and a fluid cache folder; a packed texture, a generated one and an
    unused one are not read from disk. Returns (show folder, bpy.data)."""
    show = str(root)
    light = os.path.join(show, "sh010", "lighting")
    blend = write(os.path.join(light, "sh010_lighting_v003.blend"), b"BLENDER-fake\n")
    for tile in (1001, 1002, 1011):
        write(os.path.join(show, "assets", "tex", f"wood.{tile}.exr"), b"wood %d" % tile)
    for f in range(1, 61):
        write(os.path.join(show, "fx", "smoke", f"smoke_{f:04d}.png"), b"smoke %d" % f)
    write(os.path.join(light, "tex", "sh010_matte.png"), b"matte")
    write(os.path.join(light, "ref", "layout_ref.jpg"), b"ref")
    write(os.path.join(show, "assets", "props", "props.blend"), b"BLENDER-props")
    write(os.path.join(show, "assets", "props", "tex", "props_diffuse.png"), b"props")
    write(os.path.join(show, "assets", "cache", "sh010_anim.abc"), b"abc")
    for f in range(1, 13):
        write(os.path.join(show, "fx", "vdb", f"explosion_{f:04d}.vdb"), b"vdb %d" % f)
    write(os.path.join(show, "assets", "fonts", "title.ttf"), b"font")
    write(os.path.join(show, "audio", "sh010_temp.wav"), b"wav")
    for f in range(1, 4):
        write(os.path.join(light, "cache_fluid", "data", f"fluid_data_{f:04d}.uni"), b"fluid %d" % f)

    sh010 = bf.Scene("sh010", frame_start=1001, frame_end=1048)
    sh010.camera = bf.Struct(name="CAM_sh010")
    sh010.layer("bg")
    sh010.layer("fg")
    sh010.layer("old", use=False)
    sh010.render.filepath = "//render/sh010_"
    sh010.render.image_settings.file_format = "OPEN_EXR"
    sh010.use_nodes = True
    sh010.node_tree = bf.NodeTree("Compositing", "CompositorNodeTree", nodes=[
        bf.Node("CompositorNodeOutputFile", base_path="//render/passes/", format=bf.Struct(file_format="OPEN_EXR"),
                file_slots=[bf.Struct(path="crypto_", use_node_format=True)]),
    ])

    comp = bf.Scene("sh010_comp", frame_start=1001, frame_end=1048)
    comp.camera = bf.Struct(name="CAM_comp")
    comp.layer("comp")
    comp.render.filepath = "//render/sh010_comp_"
    comp.use_nodes = True
    sh010_render = bf.Image("sh010_render", "//render/sh010_0001.exr", "SEQUENCE")
    comp.node_tree = bf.NodeTree("Compositing.001", "CompositorNodeTree", nodes=[
        bf.Node("CompositorNodeImage", image=sh010_render, frame_start=1001, frame_duration=48, frame_offset=0,
                use_cyclic=False),
    ])

    data = bf.reset(blend, scenes=[sh010, comp])
    data.is_dirty = modified
    props = bf.ID("props.blend", filepath="//../../assets/props/props.blend")
    data.libraries.append(props)

    wood = bf.Image("wood", "//../../assets/tex/wood.<UDIM>.exr", "TILED")
    smoke = bf.Image("smoke", "//../../fx/smoke/smoke_0001.png", "SEQUENCE")
    matte = bf.Image("sh010_matte", "//tex/sh010_matte.png")
    logo = bf.Image("logo", "//tex/logo.png", packed_file=bf.Struct(size=10))
    scratch = bf.Image("scratch", "", "GENERATED")
    unused = bf.Image("old_plate", "//../../plates/old_plate.exr", users=0)
    ref = bf.Image("reference image", "//ref/layout_ref.jpg")
    props_diffuse = bf.Image("props_diffuse", "//tex/props_diffuse.png", library=props)
    data.images.extend([wood, smoke, matte, logo, scratch, unused, ref, props_diffuse, sh010_render])

    data.materials.append(_material("M_wood", wood))
    data.materials.append(_material("M_smoke", smoke, bf.image_user(frame_start=1001, frame_duration=48)))
    data.materials.append(_material("M_logo", logo))
    m = _material("M_props", props_diffuse)
    m.library = props
    data.materials.append(m)
    data.worlds.append(_material("World", matte, node="ShaderNodeTexEnvironment"))

    data.cache_files.append(bf.ID("sh010_anim.abc", filepath="//../../assets/cache/sh010_anim.abc", is_sequence=False))
    data.volumes.append(bf.ID("explosion", filepath="//../../fx/vdb/explosion_0001.vdb", is_sequence=True,
                              frame_start=1001, frame_duration=10, frame_offset=0, sequence_mode="CLIP"))
    data.fonts.append(bf.ID("Bfont Regular", filepath="<builtin>"))
    data.fonts.append(bf.ID("title", filepath="//../../assets/fonts/title.ttf"))
    data.sounds.append(bf.ID("sh010_temp.wav", filepath="//../../audio/sh010_temp.wav"))
    data.extra_paths.append("//cache_fluid")
    return show, data


#: What lighting() collects for bg and fg: the file, 3 wood tiles, 48 smoke
#: frames, the matte, the reference image, the library and its texture, the
#: Alembic cache, 10 OpenVDB frames, the font, the sound, 3 fluid cache files.
LIGHTING_FILES = 1 + 3 + 48 + 1 + 1 + 2 + 1 + 10 + 1 + 1 + 3
