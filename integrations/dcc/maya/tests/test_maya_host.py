"""MayaHost: what the open scene tells c4submit."""

import os

import fakemaya as fm
from c4submit import seq
from c4submit.collect import gather
from c4submit.testing import write
from maya_scenes import light_scene

from c4_maya.host import MayaHost, frame_path, layer_key, udim_path, uv_pattern


def by_name(items):
    return {r.name: r for r in items}


def test_renderable_layers_become_renderables_with_their_ranges(tmp_path):
    host, scene = light_scene(tmp_path)
    rs = by_name(host.renderables())
    assert list(rs) == ["rs_beauty", "rs_fg"], "rs_old and the master layer are not renderable"
    assert rs["rs_beauty"].frames is None, "without its own range a layer renders the scene's"
    assert rs["rs_fg"].frames == "1001-1010" and rs["rs_fg"].note == "its own range"
    assert rs["rs_beauty"].key == "beauty" and rs["rs_beauty"].label == "beauty"
    assert rs["rs_fg"].reads == [], "layers do not read each other's renders"
    ws = os.path.join(str(tmp_path), "seq", "sm0140", "maya")
    assert rs["rs_fg"].outputs == [os.path.join(ws, "images", "fg", "sm0140_light_v003_fg.####.exr")]
    assert host.frames() == "1001-1048"
    assert host.scene_file() == scene


def test_reading_the_layers_leaves_the_artist_where_they_were(tmp_path):
    host, _ = light_scene(tmp_path)
    fm.state().current_layer = "rs_fg"
    fm.state().modified = False
    assert host.frames() == "1001-1048", "the master layer's range, not the current layer's"
    host.renderables()
    assert fm.state().current_layer == "rs_fg"
    assert fm.state().modified is False, "switching layers to read them is not an edit"
    fm.state().modified = True
    host.renderables()
    assert fm.state().modified is True, "the artist's own unsaved edits stay unsaved"


def test_one_read_switches_each_layer_once(tmp_path):
    host, _ = light_scene(tmp_path)
    fm.state().switches = 0
    frames, rs = host.read()
    assert frames == "1001-1048" and [r.name for r in rs] == ["rs_beauty", "rs_fg"]
    assert fm.state().switches == 3, "beauty, fg, then the master layer, current again"


def test_each_layer_has_its_own_camera(tmp_path):
    host, _ = light_scene(tmp_path)
    fm.camera("closeCam", renderable=False)
    fm.state().overrides["rs_fg"].update({"closeCamShape.renderable": True, "shotCamShape.renderable": False})
    rs = by_name(host.renderables())
    assert rs["rs_beauty"].values == {"scene.camera": "shotCam"}
    assert rs["rs_fg"].values == {"scene.camera": "closeCam"}, "fg overrides which camera renders"
    assert host.camera() == "shotCam", "the scene's, as the master layer has it"


def test_the_signature_holds_the_workspace(tmp_path):
    host, _ = light_scene(tmp_path)
    before = host.signature()
    fm.state().workspace = str(tmp_path / "elsewhere")
    assert host.signature() != before, "relative paths now resolve elsewhere"


def test_the_master_layer_when_it_is_renderable(tmp_path):
    host, _ = light_scene(tmp_path, master=True)
    rs = host.renderables()
    assert [r.name for r in rs] == ["defaultRenderLayer", "rs_beauty", "rs_fg"], "master first, as Maya lists it"
    assert rs[0].key == "master" and rs[0].label == "masterLayer" and rs[0].frames is None
    assert layer_key("legacyLayer") == "legacyLayer", "a legacy layer keeps its name"


def test_referenced_scenes_layers_are_not_this_scenes(tmp_path):
    host, _ = light_scene(tmp_path)
    fm.node("renderLayer", "heronRN:defaultRenderLayer", renderable=True)
    assert [r.name for r in host.renderables()] == ["rs_beauty", "rs_fg"]


def test_a_scene_without_animation_renders_the_current_frame(tmp_path):
    host, _ = light_scene(tmp_path)
    fm.setAttr("defaultRenderGlobals.animation", False)
    fm.currentTime(1017)
    assert host.frames() == "1017"
    fm.setAttr("defaultRenderGlobals.animation", True)
    fm.setAttr("defaultRenderGlobals.byFrameStep", 2.0)
    assert host.frames() == "1001-1048:2"


def test_the_renderable_camera(tmp_path):
    host, _ = light_scene(tmp_path)
    assert host.camera() == "shotCam", "the artist's camera before persp"
    fm.setAttr("shotCamShape.renderable", False)
    assert host.camera() == "persp"
    fm.setAttr("perspShape.renderable", False)
    assert host.camera() is None


def test_dependencies_are_tagged_by_the_layers_that_read_them(tmp_path):
    host, _ = light_scene(tmp_path)
    show = str(tmp_path)
    ws = os.path.join(show, "seq", "sm0140", "maya")
    deps = {d.node: d for d in host.dependencies()}
    assert set(deps) == {"heron_diffuse", "caustics", "skyImage", "splashShape", "fogShape",
                         "heron_anim_AlembicNode", "heronRN", "scene files"}
    assert deps["heron_diffuse"].path == os.path.join(show, "assets", "heron", "tex", "heron_diffuse.<UDIM>.exr")
    assert deps["heron_diffuse"].renderables is None, "the heron is in both layers"
    assert deps["caustics"].path == os.path.join(ws, "sourceimages", "caustics", "caustic.####.exr"), \
        "relative to the workspace, the frame as #"
    assert deps["caustics"].renderables == ["rs_beauty"], "only beauty has the water (rs_old is not renderable)"
    assert deps["splashShape"].path == os.path.join(show, "fx", "splash", "splash.####.ass")
    assert deps["splashShape"].renderables == ["rs_beauty"]
    assert deps["fogShape"].renderables == ["rs_fg"]
    assert deps["skyImage"].renderables is None, "the sky is in both"
    assert deps["heron_anim_AlembicNode"].renderables is None and deps["heronRN"].renderables is None, \
        "the scene reads caches and references whenever it opens"
    assert deps["scene files"].path == os.path.join(show, "color", "config.ocio") and deps["scene files"].optional
    assert not any(d.optional for n, d in deps.items() if n != "scene files")


def test_dependencies_collect_every_tile_and_frame(tmp_path):
    host, scene = light_scene(tmp_path)
    col = gather(str(tmp_path), scene, host.dependencies())
    assert col.problems == []
    rels = sorted(col.files)
    assert [r for r in rels if "heron_diffuse" in r] == [
        "assets/heron/tex/heron_diffuse.1001.exr", "assets/heron/tex/heron_diffuse.1002.exr",
        "assets/heron/tex/heron_diffuse.1011.exr"]
    assert len([r for r in rels if "caustic." in r]) == 10
    assert len([r for r in rels if "splash." in r]) == 48
    assert len(col.files) == 76, "listed files the nodes already read are not counted twice"


def test_a_layer_the_master_layer_holds_everything(tmp_path):
    host, _ = light_scene(tmp_path, master=True)
    deps = {d.node: d for d in host.dependencies()}
    assert deps["caustics"].renderables == ["defaultRenderLayer", "rs_beauty"]
    assert deps["fogShape"].renderables == ["defaultRenderLayer", "rs_fg"]


def test_a_layer_maya_gives_no_members_for_reads_everything(tmp_path):
    host, _ = light_scene(tmp_path)
    fm.state().members["rs_fg"] = []
    deps = {d.node: d for d in host.dependencies()}
    assert deps["caustics"].renderables is None, "nothing is left out on a guess"


def test_a_texture_on_faces_and_one_nothing_uses(tmp_path):
    host, _ = light_scene(tmp_path)
    show = str(tmp_path)
    fm.mesh("rock")
    fm.state().members["rs_fg"].append("rock")
    rock = write(os.path.join(show, "assets", "rock", "rock.exr"))
    fm.file_texture("rock_tex", rock, ["rockShape.f[0:11]"])
    fm.file_texture("spare_tex", os.path.join(show, "assets", "spare.exr"))
    deps = {d.node: d for d in host.dependencies()}
    assert deps["rock_tex"].renderables == ["rs_fg"], "faces count as their object"
    assert deps["spare_tex"].renderables is None, "unused: not left out on a guess"


def test_references_unloaded_and_with_copy_numbers(tmp_path):
    host, _ = light_scene(tmp_path)
    show = str(tmp_path)
    prop = write(os.path.join(show, "assets", "prop", "prop.mb"))
    fm.reference("propRN", prop, copy=True)
    fm.reference("oldRN", os.path.join(show, "assets", "old.ma"), loaded=False)
    fm.node("reference", "sharedReferenceNode", file="", loaded=True)
    deps = {d.node: d for d in host.dependencies()}
    assert deps["propRN"].path == prop, "without Maya's {2}"
    assert deps["oldRN"].optional, "an unloaded reference is not read; missing is a warning"
    assert "sharedReferenceNode" not in deps


def test_zbrush_and_mudbox_tiles_are_one_pattern(tmp_path):
    host, scene = light_scene(tmp_path)
    show = str(tmp_path)
    tex = os.path.join(show, "assets", "cliff")
    for u, v in ((1, 1), (2, 1), (1, 2)):
        write(os.path.join(tex, f"cliff_u{u}_v{v}.tif"))
    fm.mesh("cliff")
    fm.file_texture("cliff_tex", os.path.join(tex, "cliff_u1_v1.tif"), ["cliffShape"], tiling=2)
    deps = [d for d in host.dependencies() if d.node == "cliff_tex"]
    assert [d.path for d in deps] == [os.path.join(tex, "cliff_u<U>_v<V>.tif")]
    col = gather(show, scene, deps)
    assert sorted(col.files) == ["assets/cliff/cliff_u1_v1.tif", "assets/cliff/cliff_u1_v2.tif",
                                 "assets/cliff/cliff_u2_v1.tif", "seq/sm0140/maya/scenes/sm0140_light_v003.ma"]


def test_maya_computed_pattern_wins(tmp_path):
    host, _ = light_scene(tmp_path)
    show = str(tmp_path)
    fm.file_texture("ripples", os.path.join(show, "fx", "ripple.0001.exr"), sequence=True,
                    pattern=os.path.join(show, "fx", "ripple.<f>.exr"))
    fm.file_texture("plain", os.path.join(show, "fx", "plain.0001.exr"),
                    pattern=os.path.join(show, "fx", "plain.<f>.exr"))
    deps = {d.node: d.path for d in host.dependencies()}
    assert deps["ripples"] == os.path.join(show, "fx", "ripple.<f>.exr")
    assert deps["plain"] == os.path.join(show, "fx", "plain.0001.exr"), "not a sequence: the one file"


def test_arnold_tile_token_and_a_missing_set(tmp_path):
    host, scene = light_scene(tmp_path)
    show = str(tmp_path)
    fm.ai_image("bark", os.path.join(show, "assets", "tree", "bark<tile>.exr"))
    deps = [d for d in host.dependencies() if d.node == "bark"]
    assert [d.path for d in deps] == [os.path.join(show, "assets", "tree", "bark_u<U>_v<V>.exr")], \
        "Arnold's <tile> is a Mudbox-style tile"
    col = gather(show, scene, deps)
    assert col.problems[0].path == "bark" and "no such file" in col.problems[0].message
    write(os.path.join(show, "assets", "tree", "bark_u1_v1.exr"))
    write(os.path.join(show, "assets", "tree", "bark_u2_v1.exr"))
    assert len(gather(show, scene, deps).files) == 3


def test_alembic_layers_and_gpu_caches(tmp_path):
    host, _ = light_scene(tmp_path)
    show = str(tmp_path)
    base = write(os.path.join(show, "anim", "base.abc"))
    over = write(os.path.join(show, "anim", "over.abc"))
    fm.node("AlembicNode", "layered_AlembicNode", abc_File="", abc_layerFiles=[base, over])
    fm.node("transform", "trees")
    fm.node("gpuCache", "treesShape", "trees", cacheFileName=os.path.join(show, "env", "trees.abc"),
            useFrameExtension=False)
    deps = host.dependencies()
    assert sorted(d.path for d in deps if d.node == "layered_AlembicNode") == [base, over]
    trees = [d for d in deps if d.node == "treesShape"][0]
    assert trees.renderables == [], "in no renderable layer: no layer reads it"


def test_environment_variables_and_the_selection(tmp_path, monkeypatch):
    host, _ = light_scene(tmp_path)
    monkeypatch.setenv("SHOW_ROOT", str(tmp_path))
    fm.file_texture("env_tex", "$SHOW_ROOT/assets/env.exr")
    fm.select("env_tex", "heron")
    assert host.selection() == [os.path.join(str(tmp_path), "assets", "env.exr")]


def test_an_untitled_scene_cannot_be_saved_from_here(tmp_path):
    fm.reset("")
    host = MayaHost(env={})
    assert host.scene_file() == ""
    try:
        host.save()
    except RuntimeError as e:
        assert "Save Scene As" in str(e)
    else:
        raise AssertionError("an untitled scene has nowhere to be saved")


def test_save_and_modified(tmp_path):
    host, scene = light_scene(tmp_path, modified=True)
    assert host.is_modified()
    host.save()
    assert not host.is_modified() and fm.state().saves == 1


def test_path_patterns():
    assert udim_path("/t/a.1001.exr") == "/t/a.<UDIM>.exr"
    assert udim_path("/t/a_v1012.1001.exr") == "/t/a_v1012.<UDIM>.exr", "the last tile-like number"
    assert udim_path("/t/a.1001.exr", "/t/a.<UDIM>.exr") == "/t/a.<UDIM>.exr"
    assert udim_path("/t/a.exr") == "/t/a.exr"
    assert uv_pattern("/t/c_u0_v0.tif") == "/t/c_u<U>_v<V>.tif"
    assert frame_path("/t/a.0001.exr") == "/t/a.####.exr"
    assert frame_path("/t/a.<UDIM>.0001.exr") == "/t/a.<UDIM>.####.exr"
    assert frame_path("/t/a.1001.ass.gz") == "/t/a.####.ass.gz"
    assert frame_path("/t/a.%04d.exr") == "/t/a.%04d.exr", "already a sequence"
    assert frame_path("/t/a.exr") == "/t/a.exr"
    assert seq.has_frames(frame_path("/t/a.1.exr"))
