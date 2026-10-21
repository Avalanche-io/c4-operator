"""The file as c4submit reads it: view layers, outputs and every file read."""

import os

import fakebpy as bf
import pytest
from blender_scenes import lighting
from c4submit.testing import write

from c4_blender.host import BlenderHost, sequence_pattern, tile_pattern, user_frames, with_frame


def deps_by_node(host):
    return {d.node: d for d in host.dependencies()}


def test_view_layers_become_renderables_with_keys(tmp_path):
    show, _ = lighting(tmp_path)
    rs = BlenderHost().renderables()
    assert [(r.name, r.key, r.enabled) for r in rs] == [
        ("sh010/bg", "bg", True), ("sh010/fg", "fg", True), ("sh010_comp/comp", "comp", False)]
    assert all(r.frames == "1001-1048" for r in rs)
    light = os.path.join(show, "sh010", "lighting")
    assert rs[0].outputs == [os.path.join(light, "render", "sh010_####.exr"),
                             os.path.join(light, "render", "passes", "crypto_####.exr")]
    assert rs[2].outputs == [os.path.join(light, "render", "sh010_comp_####.png")]
    assert rs[2].reads == ["sh010/bg", "sh010/fg"], "the comp scene's compositor reads sh010's render"
    assert rs[2].label == "sh010_comp / comp" and rs[2].note == "scene sh010_comp · after bg, fg"
    assert rs[0].label == "bg" and rs[0].note == ""


def test_each_layer_gives_its_scene_layer_and_camera(tmp_path):
    lighting(tmp_path)
    rs = BlenderHost().renderables()
    assert rs[0].values == {"scene": "sh010", "view_layer": "bg", "scene.camera": "CAM_sh010"}
    assert rs[2].values == {"scene": "sh010_comp", "view_layer": "comp", "scene.camera": "CAM_comp"}
    bf.data.scenes["sh010_comp"].camera = None
    assert "scene.camera" not in BlenderHost().renderables()[2].values, "no camera: the panel's scene's stands"
    assert "|False" in BlenderHost().signature(), "the file's path and its unsaved state"


def test_disabled_layers_are_left_out(tmp_path):
    lighting(tmp_path)
    assert "sh010/old" not in [r.name for r in BlenderHost().renderables()]
    bf.data.scenes["sh010"].view_layers["fg"].use = False
    assert [r.name for r in BlenderHost().renderables()] == ["sh010/bg", "sh010_comp/comp"]


def test_the_same_layer_name_in_two_scenes(tmp_path):
    lighting(tmp_path)
    bf.data.scenes["sh010_comp"].view_layers["comp"].name = "bg"
    keys = [r.key for r in BlenderHost().renderables()]
    assert keys == ["sh010-bg", "fg", "sh010_comp-bg"]


def test_the_panels_scene_comes_first(tmp_path):
    lighting(tmp_path)
    rs = BlenderHost(scene="sh010_comp").renderables()
    assert [(r.name, r.enabled) for r in rs] == [("sh010_comp/comp", True), ("sh010/bg", False), ("sh010/fg", False)]
    assert BlenderHost(scene="gone").scene().name == "sh010", "a scene that is gone: the active one"


def test_file_frames_camera_and_saving(tmp_path):
    show, data = lighting(tmp_path, modified=True)
    h = BlenderHost()
    assert h.scene_file() == os.path.join(show, "sh010", "lighting", "sh010_lighting_v003.blend")
    assert h.is_modified() and h.frames() == "1001-1048" and h.camera() == "CAM_sh010"
    bf.data.scenes["sh010"].frame_step = 2
    assert h.frames() == "1001-1048:2"
    h.save()
    assert bf.state.saves == 1 and not h.is_modified()
    bf.data.scenes["sh010"].camera = None
    assert h.camera() is None
    bf.reset()
    with pytest.raises(RuntimeError, match="never been saved"):
        BlenderHost().save()
    assert BlenderHost().scene_file() == ""


def test_dependencies(tmp_path):
    show, _ = lighting(tmp_path)
    d = deps_by_node(BlenderHost())
    assert d["wood"].path == os.path.join(show, "assets", "tex", "wood.<UDIM>.exr") and d["wood"].frames is None
    assert d["smoke"].path == os.path.join(show, "fx", "smoke", "smoke_####.png")
    assert d["smoke"].frames == "1-48", "the frames its texture node reads over 1001-1048, not all 60 files"
    assert d["sh010_matte"].path == os.path.join(show, "sh010", "lighting", "tex", "sh010_matte.png"), "a // path"
    assert d["props.blend"].path == os.path.join(show, "assets", "props", "props.blend")
    assert d["props_diffuse"].path == os.path.join(show, "assets", "props", "tex", "props_diffuse.png"), \
        "a linked texture's // is its library's folder"
    assert d["explosion"].path == os.path.join(show, "fx", "vdb", "explosion_####.vdb") and d["explosion"].frames == "1-10"
    assert d["sh010_anim.abc"].path.endswith("assets/cache/sh010_anim.abc")
    assert d["title"].path.endswith("assets/fonts/title.ttf")
    assert d["sh010_temp.wav"].optional, "frames rendered to files play no sound"
    assert d["reference image"].optional, "no render node reads it"
    assert not d["wood"].optional and d["wood"].renderables is None
    assert d["sh010_render"].renderables == ["sh010_comp/comp"], "the comp scene's compositor reads it"
    assert d["sh010_render"].frames == "1-48"
    for gone in ("logo", "scratch", "old_plate", "Bfont Regular"):
        assert gone not in d, f"{gone}: packed, generated, unused or built in"
    fluid = [x for x in BlenderHost().dependencies() if x.node == "cache_fluid"]
    assert [os.path.basename(x.path) for x in fluid] == ["fluid_data_0001.uni", "fluid_data_0002.uni",
                                                        "fluid_data_0003.uni"]
    assert all(x.optional for x in fluid)
    paths = [x.path for x in BlenderHost().dependencies()]
    assert len(paths) == len(set(paths)), "blend_paths adds only what nothing else lists"


def test_a_cyclic_sequence_and_a_compositor_image_node(tmp_path):
    show, data = lighting(tmp_path)
    m = data.materials["M_smoke"]
    m.node_tree.nodes[0].image_user = bf.image_user(frame_start=1001, frame_duration=10, use_cyclic=True,
                                                    frame_offset=100)
    assert deps_by_node(BlenderHost())["smoke"].frames == "101-110"
    m.node_tree.nodes[0].image_user = None   # a node that holds no frames: every file of the sequence
    assert deps_by_node(BlenderHost())["smoke"].frames is None


def test_unused_materials_and_muted_nodes_read_nothing_required(tmp_path):
    show, data = lighting(tmp_path)
    data.materials["M_wood"].users = 0
    assert deps_by_node(BlenderHost())["wood"].optional
    data.materials["M_wood"].users = 1
    data.materials["M_wood"].node_tree.nodes[0].mute = True
    assert deps_by_node(BlenderHost())["wood"].optional


def test_the_compositor_off_reads_nothing_for_its_scene(tmp_path):
    lighting(tmp_path)
    comp = bf.data.scenes["sh010_comp"]
    comp.use_nodes = False
    d = deps_by_node(BlenderHost())
    assert d["sh010_render"].optional and d["sh010_render"].renderables is None
    assert [r.reads for r in BlenderHost().renderables()][2] == []


def test_sequencer_strips_movie_clips_and_libraries(tmp_path):
    show, data = lighting(tmp_path)
    light = os.path.join(show, "sh010", "lighting")
    for f in (1, 2):
        write(os.path.join(light, "edit", f"board_{f}.jpg"))
    write(os.path.join(show, "plates", "sh010_plate.mov"))
    bf.data.scenes["sh010"].sequence_editor = bf.Struct(sequences_all=[
        bf.Struct(name="boards", type="IMAGE", directory="//edit/", mute=False,
                  elements=[bf.Struct(filename="board_1.jpg"), bf.Struct(filename="board_2.jpg")]),
        bf.Struct(name="plate", type="MOVIE", filepath="//../../plates/sh010_plate.mov", mute=False),
        bf.Struct(name="muted", type="MOVIE", filepath="//gone.mov", mute=True),
    ])
    data.extra_paths.append("//edit/")
    data.movieclips.append(bf.ID("track", filepath="//../../plates/track_0001.exr", source="SEQUENCE"))
    nested = bf.ID("nested.blend", filepath="//sub/nested.blend", parent=data.libraries[0])
    packed = bf.ID("packed.blend", filepath="//packed.blend", packed_file=bf.Struct(size=1))
    data.libraries.extend([nested, packed])
    deps = BlenderHost().dependencies()
    by = {}
    for x in deps:
        by.setdefault(x.node, []).append(x)
    assert [os.path.basename(x.path) for x in by["boards"]] == ["board_1.jpg", "board_2.jpg"]
    assert by["boards"][0].renderables == ["sh010/bg", "sh010/fg"]
    assert by["plate"][0].path == os.path.join(show, "plates", "sh010_plate.mov") and "muted" not in by
    assert not any(x.path.endswith("/edit") or x.node == "edit" for x in deps), "the strips' folder is listed already"
    assert by["track"][0].path.endswith("plates/track_####.exr") and by["track"][0].optional
    assert by["nested.blend"][0].path == os.path.join(show, "assets", "props", "sub", "nested.blend"), \
        "a library inside a library is found from its parent's folder"
    assert "packed.blend" not in by


def test_patterns():
    assert sequence_pattern("/show/fx/smoke_0001.png") == "/show/fx/smoke_####.png"
    assert sequence_pattern("/show/plates/sh010.1001.exr") == "/show/plates/sh010.####.exr"
    assert sequence_pattern("/show/v2/plate.exr") == "/show/v2/plate.exr", "digits in a folder are not frames"
    assert sequence_pattern("/show/a_####.exr") == "/show/a_####.exr"
    assert tile_pattern("/show/tex/wood.1001.exr") == "/show/tex/wood.<UDIM>.exr"
    assert tile_pattern("/show/tex/wood.<UDIM>.exr") == "/show/tex/wood.<UDIM>.exr"
    assert tile_pattern("/show/tex/wood_v3.exr") == "/show/tex/wood_v3.exr"
    assert user_frames(1001, 1010, 1, 1001, 0, 5) == [1, 2, 3, 4, 5], "held at the last file"
    assert user_frames(1, 4, 1, 3, 0, 100) == [1, 2], "held at the first file before it starts"
    assert user_frames(1001, 1006, 1, 1001, 0, 3, cyclic=True) == [1, 2, 3]
    assert user_frames(1001, 1009, 4, 1001, 10, 100) == [11, 15, 19]
    assert with_frame("/r/sh010_", ".exr") == "/r/sh010_####.exr"
    assert with_frame("/r/sh010_##.exr", ".exr") == "/r/sh010_##.exr"
    assert with_frame("/r/", ".png") == "/r/####.png"
    assert with_frame("/r/beauty", "") == "/r/beauty####"


def test_a_movie_output_writes_no_frame_files(tmp_path):
    lighting(tmp_path)
    s = bf.data.scenes["sh010"]
    s.render.image_settings.file_format = "FFMPEG"
    s.use_nodes = False
    r = BlenderHost().renderables()[0]
    assert r.outputs == [] and r.note == "writes a movie, not a file per frame"
