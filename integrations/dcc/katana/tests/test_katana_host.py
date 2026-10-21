"""KatanaHost: what the open project tells c4submit."""

import os

import fakekatana as fk
from c4submit.collect import gather
from c4submit.testing import write
from katana_scenes import ASSET, light_project

from c4_katana.host import KatanaHost, expression_sequence, looks_like_file, param_files
from c4submit.host import JOB_FRAMES


def by_name(items):
    return {r.name: r for r in items}


def test_render_nodes_become_renderables_after_what_they_depend_on(tmp_path):
    host, project, _ = light_project(tmp_path)
    frames, rs = host.read()
    assert frames == "1001-1048"
    assert [r.name for r in rs] == ["Render_shadow", "Render_beauty"], "FarmAPI's order: dependencies first"
    rs = by_name(rs)
    assert "Render_old" not in rs, "a bypassed Render node renders nothing"
    assert rs["Render_beauty"].reads == ["Render_shadow"] and rs["Render_beauty"].note == "after Render_shadow"
    assert rs["Render_shadow"].reads == [] and rs["Render_shadow"].frames is None
    out = os.path.join(str(tmp_path), "render", "sm0140")
    assert rs["Render_shadow"].outputs == [os.path.join(out, "shadow", "shadow.####.exr")]
    assert rs["Render_beauty"].key == "beauty"
    assert host.scene_file() == project
    assert fk.state().dependency_calls[-1] is not None, "FarmAPI is asked about the Render nodes themselves"


def test_a_render_nodes_own_range_and_disabled_outputs(tmp_path):
    host, _, shadow = light_project(tmp_path)
    shadow.range = (1001.0, 1010.0)
    shadow.render_outputs.append({"name": "deep", "outputLocation": "/tmp/deep.####.exr", "enabled": False})
    rs = by_name(host.renderables())
    assert rs["Render_shadow"].frames == "1001-1010"
    assert len(rs["Render_shadow"].outputs) == 1, "a disabled output is not written"
    shadow.range = (1001.0, 1048.0)
    assert by_name(host.renderables())["Render_shadow"].frames is None, "the project's range is not its own"


def test_the_frame_range_from_the_root_when_farmapi_has_none(tmp_path, monkeypatch):
    host, _, _ = light_project(tmp_path)
    monkeypatch.setattr(fk.FarmAPI, "GetSceneFrameRange", lambda: None)
    fk.state().root.getParameter("outTime").setValue(1100)
    assert host.frames() == "1001-1100"


def test_dependencies_are_tagged_by_the_render_nodes_downstream(tmp_path):
    host, _, _ = light_project(tmp_path)
    show = str(tmp_path)
    deps = {d.node: d for d in host.dependencies()}
    assert set(deps) == {"heron_abc.abcAsset", "marsh_usd.fileName", "heron_look.args.lookfile.asset.value",
                         "heron_diffuse_tex.parameters.filename.value", "fog_usd.fileName",
                         "shadow_map.parameters.filename.value"}, \
        "not the disabled fallback, the camera's location, Render nodes' outputs or the project itself"
    assert deps["heron_abc.abcAsset"].renderables is None, "both Render nodes read the heron"
    assert deps["heron_diffuse_tex.parameters.filename.value"].path == os.path.join(
        show, "assets", "heron", "tex", "heron_diffuse.<UDIM>.tex")
    assert deps["heron_diffuse_tex.parameters.filename.value"].renderables is None, "inside a material group"
    assert deps["fog_usd.fileName"].renderables == ["Render_beauty"]
    assert deps["shadow_map.parameters.filename.value"].renderables == ["Render_beauty"]


def test_a_node_no_render_node_reads(tmp_path):
    host, _, _ = light_project(tmp_path)
    fk.node("Alembic_In", "spare_abc", abcAsset=(str(tmp_path / "anim" / "spare.abc"), ASSET))
    deps = {d.node: d for d in host.dependencies()}
    assert deps["spare_abc.abcAsset"].renderables == [], "upstream of no Render node: no job reads it"


def test_a_bypassed_node_reads_nothing(tmp_path):
    host, _, _ = light_project(tmp_path)
    fk.state().nodes["fog_usd"].bypassed = True
    assert "fog_usd.fileName" not in {d.node for d in host.dependencies()}


def test_inside_a_group_fed_from_outside(tmp_path):
    host, _, _ = light_project(tmp_path)
    show = str(tmp_path)
    marsh = fk.state().nodes["marsh_usd"]
    g = fk.group("rim_pass", marsh)
    grade = fk.node("UsdIn", "rim_usd", parent=g, fileName=(write(os.path.join(show, "fx", "rim.usd")), ASSET))
    fk.from_group_input(grade, g)
    fk.render("Render_rim", g, outputs=[os.path.join(show, "render", "rim.####.exr")])
    deps = {d.node: d for d in host.dependencies()}
    assert deps["rim_usd.fileName"].renderables == ["Render_rim"], "inside the group upstream of Render_rim"
    assert deps["marsh_usd.fileName"].renderables is None, "through the group's input: every Render node reads it"


def test_a_render_node_inside_a_group_reads_through_its_input(tmp_path):
    host, _, _ = light_project(tmp_path)
    show = str(tmp_path)
    fog = fk.state().nodes["fog_usd"]
    g = fk.group("fx_group", fog)
    inner = fk.node("Render", "Render_fx", parent=g)
    inner.render_outputs = [{"name": "primary", "outputLocation": os.path.join(show, "render", "fx.####.exr")}]
    fk.from_group_input(inner, g)
    deps = {d.node: d for d in host.dependencies()}
    assert deps["fog_usd.fileName"].renderables == ["Render_beauty", "Render_fx"]


def test_files_are_found_by_widget_or_by_name(tmp_path):
    fk.reset(str(tmp_path / "a.katana"))
    n = fk.node("OpScript", "ops", script={"lua": "-- reads /show/a.abc"},
                user={"cache": "/show/fx/sim.bgeo.sc", "notes": "see /show/docs", "asset": ("asset:42", ASSET),
                      "out": ("/show/out.exr", {"widget": "assetIdOutput"})})
    got = sorted(p for p, _ in param_files(n, 1001))
    assert got == ["/show/fx/sim.bgeo.sc", "asset:42"], "a file's name, or a file widget; never an output"
    assert looks_like_file("/show/a.USDA") and not looks_like_file("plate.exr") and not looks_like_file("/root/world")


def test_paths_relative_to_the_project_and_variables(tmp_path, monkeypatch):
    host, project, _ = light_project(tmp_path)
    monkeypatch.setenv("SHOW", str(tmp_path))
    fk.node("Alembic_In", "rel_abc", abcAsset=("../cache/rel.abc", ASSET))
    fk.node("Alembic_In", "env_abc", abcAsset=("$SHOW/anim/env.abc", ASSET))
    deps = {d.node: d.path for d in host.dependencies()}
    assert deps["rel_abc.abcAsset"] == os.path.join(str(tmp_path), "seq", "sm0140", "cache", "rel.abc")
    assert deps["env_abc.abcAsset"] == os.path.join(str(tmp_path), "anim", "env.abc")


def test_collected_files(tmp_path):
    host, project, _ = light_project(tmp_path)
    col = gather(str(tmp_path), project, [d for d in host.dependencies() if "shadow_map" not in d.node])
    assert col.problems == []
    assert len(col.files) == 8, "the project, the cache, the set, the look file, three tiles and the fog"


def test_save_and_dirty(tmp_path):
    host, project, _ = light_project(tmp_path, dirty=True)
    assert host.is_modified()
    host.save()
    assert not host.is_modified() and fk.state().saves == 1
    assert host.signature() == f"{project}|False"


def test_an_unsaved_project_cannot_be_saved_from_here():
    fk.reset("")
    host = KatanaHost(env={})
    assert host.scene_file() == ""
    try:
        host.save()
    except RuntimeError as e:
        assert "File > Save As" in str(e)
    else:
        raise AssertionError("an unsaved project has nowhere to be saved")


def test_an_expression_naming_each_frames_file_is_a_sequence_over_the_jobs_frames(tmp_path):
    host, project, _ = light_project(tmp_path)
    show = str(tmp_path)
    smoke = fk.node("Alembic_In", "smoke_abc",
                    abcAsset=(lambda t: f"{show}/fx/smoke/smoke.{int(t):04d}.abc", ASSET))
    fk.render("Render_fx", smoke, outputs=[os.path.join(show, "render", "fx.####.exr")], frames=(1001, 1010))
    fk.node("UsdIn", "vdb_in", fileName=(lambda t: f"{show}/fx/fog/fog.%04d.vdb", ASSET))
    fk.node("Alembic_In", "offset_abc", abcAsset=(lambda t: f"{show}/fx/later.{int(t) + 10:04d}.abc", ASSET))
    fk.node("Alembic_In", "same_abc", abcAsset=(lambda t: f"{show}/anim/same.abc", ASSET))
    fk.node("Alembic_In", "typed_seq", abcAsset=(f"{show}/fx/typed.####.abc", ASSET))
    deps = {d.node: d for d in host.dependencies()}
    d = deps["smoke_abc.abcAsset"]
    assert d.path == f"{show}/fx/smoke/smoke.####.abc" and d.frames == JOB_FRAMES, "the pattern, over the jobs' frames"
    assert d.renderables == ["Render_fx"], "tagged as before"
    assert (deps["vdb_in.fileName"].path, deps["vdb_in.fileName"].frames) == (f"{show}/fx/fog/fog.%04d.vdb",
                                                                              JOB_FRAMES), "a pattern it gives as one"
    assert (deps["offset_abc.abcAsset"].path, deps["offset_abc.abcAsset"].frames) == (f"{show}/fx/later.1011.abc",
                                                                                      None), "an offset: as before"
    assert deps["same_abc.abcAsset"].frames is None and deps["same_abc.abcAsset"].path == f"{show}/anim/same.abc"
    assert deps["typed_seq.abcAsset"].frames is None, "a typed pattern, not an expression: every file, as before"
    assert sorted(p for p, _ in param_files(fk.state().nodes["smoke_abc"], 1001)) == [f"{show}/fx/smoke/smoke.####.abc"]


def test_an_unpadded_frame_and_a_katana_without_isexpression(tmp_path, monkeypatch):
    fk.reset(str(tmp_path / "a.katana"), 8, 12)
    n = fk.node("Alembic_In", "a", abcAsset=(lambda t: f"/show/fx/a.{int(t)}.abc", ASSET))
    assert param_files(n, 9) == [("/show/fx/a.#.abc", "a.abcAsset")], "9 then 10: unpadded"
    monkeypatch.delattr(fk.Parameter, "isExpression")
    assert param_files(n, 9) == [("/show/fx/a.#.abc", "a.abcAsset")], "known from two frames"
    m = fk.node("Alembic_In", "b", abcAsset=(lambda t: "/show/fx/b.####.abc", ASSET))
    assert param_files(m, 9) == [("/show/fx/b.####.abc", "b.abcAsset")]
    assert expression_sequence(m.getParameter("abcAsset"), "/show/fx/b.####.abc", 9) is None, \
        "a pattern, with no way to know it is an expression: as before"


def test_only_the_jobs_frames_of_an_expressions_sequence_are_sent(tmp_path):
    from c4submit.submitter import Draft, prepare
    from c4submit.tool import Tool
    from katana_scenes import KATANA_RENDER

    host, project, _ = light_project(tmp_path)
    show = str(tmp_path)
    for f in range(1001, 1049):
        write(os.path.join(show, "fx", "smoke", f"smoke.{f:04d}.abc"), b"%d" % f)
    smoke = fk.node("Alembic_In", "smoke_abc",
                    abcAsset=(lambda t: f"{show}/fx/smoke/smoke.{int(t):04d}.abc", ASSET))
    fk.render("Render_fx", smoke, outputs=[os.path.join(show, "render", "fx.####.exr")], frames=(1001, 1010))
    files = prepare(Draft(host, Tool(KATANA_RENDER), {"name": "saltmarsh", "root": show})).collection.files
    got = sorted(f for f in files if f.startswith("fx/smoke/"))
    assert got == [f"fx/smoke/smoke.{f}.abc" for f in range(1001, 1011)], "Render_fx's ten frames, not 48 or one"
