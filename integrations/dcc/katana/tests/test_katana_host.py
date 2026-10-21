"""KatanaHost: what the open project tells c4submit."""

import os

import fakekatana as fk
from c4submit.collect import gather
from c4submit.testing import write
from katana_scenes import ASSET, light_project

from c4_katana.host import KatanaHost, looks_like_file, param_files


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
