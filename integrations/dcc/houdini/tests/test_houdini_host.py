"""The two hosts over the fake hou: what each node reads of the scene."""

import os

import fakehou as hou
from houdini_scenes import c4render, fx_scene, light_scene

from c4_houdini.host import LopHost, RopHost, expand


def test_expand_keeps_frame_variables(tmp_path):
    hou.reset(hip=str(tmp_path / "shot" / "a.hip"), JOB="/show")
    hip = str(tmp_path / "shot")
    assert expand("$HIP/geo/fx.$F4.bgeo.sc") == f"{hip}/geo/fx.$F4.bgeo.sc"
    assert expand("${HIP}/geo/fx.${F4}.bgeo.sc") == f"{hip}/geo/fx.${{F4}}.bgeo.sc"
    assert expand("$JOB/tex/wood.<UDIM>.rat") == "/show/tex/wood.<UDIM>.rat"
    assert expand("$HIP/render/$HIPNAME.$F.exr") == f"{hip}/render/a.$F.exr"
    assert expand("$FSTART") == "1001", "$F followed by a letter is another variable"


def test_render_settings_prims_become_renderables(tmp_path):
    _, lop = light_scene(tmp_path)
    host = LopHost(c4render(lop))
    rs = host.renderables()
    assert [r.name for r in rs] == ["/Render/beauty_only", "/Render/rendersettings"]
    assert [r.name for r in rs if r.enabled] == ["/Render/rendersettings"], "the stage's default is ticked"


def test_without_a_default_the_first_is_ticked_and_one_outside_render_is_found(tmp_path):
    _, lop = light_scene(tmp_path, default="")
    host = LopHost(c4render(lop))
    assert [(r.name, r.enabled) for r in host.renderables()] == [("/Render/beauty_only", True),
                                                                  ("/Render/rendersettings", False)]
    lop._stage._prims["/Shots/sm0220/settings"] = "RenderSettings"
    lop._stage._meta["renderSettingsPrimPath"] = "/Shots/sm0220/settings"
    assert [(r.name, r.enabled) for r in host.renderables()][-1] == ("/Shots/sm0220/settings", True)


def test_lop_without_input_has_nothing(tmp_path):
    light_scene(tmp_path)
    node = hou.node("/stage").createNode("null", "c4render1")
    host = LopHost(node)
    assert host.scene_file() == "" and host.renderables() == [] and host.dependencies() == []


def test_stage_is_written_by_a_usd_rop_and_its_files_read_back(tmp_path):
    show, lop = light_scene(tmp_path, unresolved=["../missing/leaf.exr"])
    node = c4render(lop)
    from c4_houdini import nodes
    nodes.rebuild(node, None)  # the node's own parameters: the stage file and the frame range
    node.parm("c4_shot").set("sm0220")
    host = LopHost(node)
    usd = os.path.join(show, "sm0220", "light", "usd", "sm0220_c4render1.usd")
    assert host.scene_file() == usd and host.frames() == "1001-1048"
    assert host.dependencies() == [], "nothing is read before the stage is written"

    host.write_frames = "1001-1048:4"
    host.save()
    assert os.path.isfile(usd)
    (r,) = hou.rendered
    assert r["source"] == "/stage/karmarendersettings" and r["output"] == usd
    assert r["frames"] == (1001, 1045, 4) and r["trange"] == 1
    assert hou.node("/out/c4_write_stage") is None, "the USD ROP is removed after writing"

    deps = {d.path: d for d in host.dependencies()}
    assert sorted(deps) == sorted([
        os.path.join(show, "assets", "set", "set.usdc"),
        os.path.join(show, "assets", "tree", "tex", "bark.<UDIM>.exr"),
        os.path.join(show, "lookdev", "hdri", "sky.exr"),
        os.path.join(show, "sm0220", "light", "missing", "leaf.exr"),
    ]), "the written file itself, anonymous layers and Houdini's own files are left out"
    assert {d.node for d in deps.values()} == {"/stage/karmarendersettings"}


def test_frame_range_parms_and_write_frames(tmp_path):
    _, lop = light_scene(tmp_path)
    node = c4render(lop)
    from c4_houdini import nodes
    nodes.rebuild(node, None)
    host = LopHost(node)
    node.parmTuple("c4_f").set((1010, 1020, 2))
    assert host.frames() == "1010-1020:2"
    assert host._write_range() == (1010, 1020, 2)
    host.write_frames = "1001,1005,1100"
    assert host._write_range() == (1001, 1100, 1), "an uneven list is written over its whole span"


def test_rop_inputs_become_jobs_with_after(tmp_path):
    show, submit = fx_scene(tmp_path)
    host = RopHost(submit)
    hip = os.path.join(show, "sm0140", "fx")
    rs = {r.name: r for r in host.renderables()}
    assert list(rs) == ["/out/sim", "/out/mantra1", "/out/flip"], "upstream first"
    sim, mantra, flip = rs["/out/sim"], rs["/out/mantra1"], rs["/out/flip"]
    assert (mantra.enabled, flip.enabled, sim.enabled) == (True, True, False)
    assert sim.note == "upstream of mantra1"
    assert mantra.reads == ["/out/sim"] and flip.reads == [] and sim.reads == []
    assert mantra.frames == "1001-1048" and flip.frames == "1001", "a ROP set to the current frame renders it"
    assert mantra.outputs == [f"{hip}/render/sm0140_fx.$F4.exr"]
    assert sim.outputs == [f"{hip}/cache/sim.$F4.bgeo.sc"]
    assert mantra.label == "mantra1"

    hou.node("/out/flip").bypass(True)
    flip = {r.name: r for r in host.renderables()}["/out/flip"]
    assert not flip.enabled and flip.note == "bypassed"


def test_studio_rop_outputs_found_by_their_file_chooser(tmp_path):
    show, submit = fx_scene(tmp_path)
    rop = hou.node("/out").createNode("studio_cache", "cache2")
    rop.parm("cachefile").set("$HIP/cache/studio.$F4.vdb")
    submit.setInput(1, rop)
    r = {r.name: r for r in RopHost(submit).renderables()}["/out/cache2"]
    assert r.outputs == [os.path.join(show, "sm0140", "fx", "cache", "studio.$F4.vdb")]


def test_fetch_renders_its_source(tmp_path):
    show, submit = fx_scene(tmp_path)
    fetch = hou.node("/out").createNode("fetch", "fetch_mantra")
    fetch.parm("source").set("/out/mantra1")
    submit.setInput(0, fetch)
    r = {r.name: r for r in RopHost(submit).renderables()}["/out/fetch_mantra"]
    assert r.frames == "1001-1048" and r.outputs[0].endswith("render/sm0140_fx.$F4.exr")


def test_hip_dependencies(tmp_path):
    show, submit = fx_scene(tmp_path)
    hip = os.path.join(show, "sm0140", "fx")
    deps = {d.path: d for d in RopHost(submit).dependencies()}
    assert sorted(deps) == sorted([
        f"{hip}/geo/fluid.$F4.bgeo.sc",
        f"{hip}/cache/sim.$F4.bgeo.sc",
        f"{hip}/tex/wood.<UDIM>.rat",
        f"{show}/assets/env/env.abc",
        f"{hip}/geo/old_version.bgeo.sc",
        f"{show}/otls/studio_tools.hda",
    ]), "ROP outputs, $HFS files and op: paths are not inputs"
    assert deps[f"{hip}/geo/old_version.bgeo.sc"].optional, "a bypassed node's file is only a warning"
    assert not deps[f"{hip}/geo/fluid.$F4.bgeo.sc"].optional
    assert deps[f"{hip}/geo/fluid.$F4.bgeo.sc"].node == "/obj/geo1/fluid"


def test_hip_file_save(tmp_path):
    show, submit = fx_scene(tmp_path)
    host = RopHost(submit)
    assert host.scene_file() == os.path.join(show, "sm0140", "fx", "sm0140_fx_v002.hip")
    hou.hipFile.modified = True
    assert host.is_modified()
    host.save()
    assert hou.hipFile.saves == 1 and not host.is_modified()
    hou.hipFile._path = ""
    assert host.scene_file() == ""
    try:
        host.save()
    except RuntimeError as e:
        assert "never been saved" in str(e)
    else:
        raise AssertionError("an untitled hip file is not saved to the current folder")


def test_shot_pattern_reads_the_hip_path(tmp_path, monkeypatch):
    show, submit = fx_scene(tmp_path)
    monkeypatch.setenv("C4_SHOT_PATTERN", r"/(?P<shot>sm\d{4})/")
    assert RopHost(submit).shot() == "sm0140"
