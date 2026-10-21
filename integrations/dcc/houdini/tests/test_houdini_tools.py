"""The farm-side tool folders in houdini/tools/: their scripts over the fake
hou (and a fake pdg), the shipped test scenes built by them, and the
folders as the C4 nodes offer them."""

import importlib.util
import os
import shutil
import sys
import types

import fakehou as hou
import pytest

from c4submit import tool as toollib
from c4submit import yamlio
from c4_houdini import host as hostlib

TOOLS = os.path.normpath(os.path.join(os.path.dirname(__file__), "..", "tools"))


def script(tool: str, name: str):
    """A tool folder's script as a module, fresh for each test."""
    spec = importlib.util.spec_from_file_location(f"c4tool_{name}", os.path.join(TOOLS, tool, name + ".py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def tool_dict(name: str) -> dict:
    with open(os.path.join(TOOLS, name, "tool.yaml"), encoding="utf-8") as f:
        return yamlio.load(f.read())


def tree(tmp_path, tool: str) -> str:
    """A copy of a tool's test tree, as the farm mounts it for its test."""
    dst = str(tmp_path / "tree")
    shutil.copytree(os.path.join(TOOLS, tool, "test"), dst)
    return dst


def lines(capsys) -> list:
    return [ln for ln in capsys.readouterr().out.splitlines() if ln]


# --- the folders -----------------------------------------------------------------------


def test_each_folder_runs_its_script_with_the_arguments_it_takes():
    rop, pdg, karma = tool_dict("houdini-rop"), tool_dict("pdg-cook"), tool_dict("karma")
    assert rop["runs"]["command"] == ["{{Run.Exe}}", "{{Tool.Dir}}/houdini_rop.py", "{{Param.hip}}", "{{Param.rop}}",
                                      "{{Unit.Frames}}"]
    assert pdg["runs"]["command"] == ["{{Run.Exe}}", "{{Tool.Dir}}/pdg_cook.py", "{{Param.hip}}", "{{Param.top}}"]
    assert karma["runs"]["command"][0] == "{{Run.Exe}}" and karma["runs"]["command"][-1] == "{{Param.stage}}"
    for t in (rop, pdg, karma):
        assert "licences" not in t.get("needs", {}), "the farm cannot count seats yet"
        assert t["runs"]["image"].startswith("registry.example.com/"), "a placeholder the studio replaces"
        assert t["runs"]["host"]["has"] == ["houdini-21.0"]
        assert set(t["runs"]["host"]["exe"]) == {"linux", "windows", "macos"}, "{{Run.Exe}} is empty on a host without one"
        test = t["test"]
        for k, v in test["with"].items():
            if k in ("hip", "stage"):
                assert os.path.isfile(os.path.join(TOOLS, t["name"], "test", v)), v


def test_the_nodes_offer_the_folders_by_the_scene_they_take(tmp_path):
    tools = toollib.local_tools(TOOLS)
    by = {t.name: t for t in tools}
    assert set(by) == {"houdini-rop", "karma", "pdg-cook"}
    hip = [t.name for t in tools if t.accepts_scene("/show/sm0140/fx/sm0140_fx_v002.hip")]
    usd = [t.name for t in tools if t.accepts_scene("/show/sm0220/light/usd/sm0220_c4render.usd")]
    assert sorted(hip) == ["houdini-rop", "pdg-cook"] and usd == ["karma"]
    assert hostlib.lists_for(by["pdg-cook"]) == "tops" and hostlib.lists_for(by["houdini-rop"]) == "rops"
    assert not by["pdg-cook"].takes_frames and by["houdini-rop"].takes_frames


# --- houdini-rop: frames -------------------------------------------------------------------


def test_unit_frames_in_openjd_grammar():
    rop = script("houdini-rop", "houdini_rop")
    assert rop.parse_frames("1001") == [1001]
    assert rop.parse_frames("1001-1004") == [1001, 1002, 1003, 1004]
    assert rop.parse_frames("1001-1010:3") == [1001, 1004, 1007, 1010]
    assert rop.parse_frames("1001,1004,1010-1012,1004") == [1001, 1004, 1010, 1011, 1012]
    for bad in ("a-b", "1010-1001", "1001-1010:0"):
        with pytest.raises(rop.Fail) as e:
            rop.parse_frames(bad)
        assert e.value.cls == "permanent"


def test_frames_rendered_in_the_fewest_ranges():
    rop = script("houdini-rop", "houdini_rop")
    assert rop.ranges([1001, 1002, 1003, 1004]) == [(1001, 1004, 1)]
    assert rop.ranges([1001, 1004, 1007, 1010]) == [(1001, 1010, 3)]
    assert rop.ranges([1001, 1004, 1010, 1011, 1012]) == [(1001, 1004, 3), (1010, 1012, 1)]
    assert rop.ranges([1001]) == [(1001, 1001, 1)]


def test_output_parm_is_the_one_the_submit_rop_reads(tmp_path):
    from houdini_scenes import fx_scene
    rop = script("houdini-rop", "houdini_rop")
    show, submit = fx_scene(tmp_path)
    hou.node("/out").createNode("studio_cache", "cache2")
    fetch = hou.node("/out").createNode("fetch", "fetch_mantra")
    fetch.parm("source").set("/out/mantra1")
    for path in ("/out/sim", "/out/mantra1", "/out/flip", "/out/cache2"):
        n = hou.node(path)
        assert rop.output_parm(n).name() == hostlib.output_parms(n)[0].name(), path
    assert rop.output_parm(fetch).node().path() == "/out/mantra1", "a Fetch ROP writes its source's files"
    hou.setFrame(1001)
    assert rop.frame_file(rop.output_parm(hou.node("/out/sim")), 1017) == \
        os.path.join(show, "sm0140", "fx", "cache", "sim.1017.bgeo.sc")


# --- houdini-rop: a run -------------------------------------------------------------------


def test_builds_the_test_scene_and_renders_the_rop_without_inputs(tmp_path, capsys):
    rop = script("houdini-rop", "houdini_rop")
    t = tree(tmp_path, "houdini-rop")
    test = tool_dict("houdini-rop")["test"]
    py = os.path.join(t, test["with"]["hip"])
    assert rop.main([py, test["with"]["rop"], str(test["frames"])]) == 0
    out = lines(capsys)
    frames = [ln for ln in out if ln.startswith("c4_frame:")]
    want = [os.path.join(t, e) for e in test["expect"]]
    assert frames == [f"c4_frame: 1001 {want[0]}", f"c4_frame: 1002 {want[1]}"], out
    assert all(os.path.isfile(w) for w in want)
    assert hou.hipFile.loads == [os.path.join(t, "fx", "rop_setup.hip")], "the scene saved beside the .py is opened"
    (r,) = hou.rendered
    assert r["rop"] == "/out/cache" and r["frames"] == (1001, 1002, 1) and r["ignore_inputs"] is True
    assert "openjd_status: building the scene with rop_setup.py" in out
    assert not [ln for ln in out if ln.startswith("c4_fail")]


def test_a_stepped_unit_renders_only_its_frames(tmp_path, capsys):
    rop = script("houdini-rop", "houdini_rop")
    t = tree(tmp_path, "houdini-rop")
    assert rop.main([os.path.join(t, "fx", "rop_setup.py"), "/out/cache", "1001-1005:2,1010"]) == 0
    assert [r["frames"] for r in hou.rendered] == [(1001, 1005, 2), (1010, 1010, 1)]
    assert [ln.split()[1] for ln in lines(capsys) if ln.startswith("c4_frame:")] == ["1001", "1003", "1005", "1010"]


def _scene(tmp_path):
    hip = str(tmp_path / "shot" / "fx.hip")
    os.makedirs(os.path.dirname(hip))
    with open(hip, "w") as f:
        f.write("HIP fake\n")
    hou.reset(hip=hip)
    cache = hou.node("/out").createNode("geometry", "cache")
    cache.parm("sopoutput").set("$HIP/geo/fx.$F4.bgeo.sc")
    hou.Node("/obj/geo1", "geo", "Object")
    hou.Node("/obj/geo1/box1", "box", "Sop")
    return hip, cache


@pytest.mark.parametrize("args, cls, words", [
    (["{missing}", "/out/cache", "1001"], "permanent", "no hip file at"),
    (["{hip}", "/out/nope", "1001"], "permanent", "the hip file has no node /out/nope"),
    (["{hip}", "/obj/geo1/box1", "1001"], "permanent", "/obj/geo1/box1 is a box node, not a ROP"),
    (["{hip}", "/out/cache", "ten"], "permanent", "are not a frame list"),
    (["{notahip}", "/out/cache", "1001"], "permanent", "is not a hip file"),
])
def test_what_it_cannot_render_is_named(tmp_path, capsys, args, cls, words):
    rop = script("houdini-rop", "houdini_rop")
    hip, _ = _scene(tmp_path)
    other = str(tmp_path / "shot" / "fx.ma")
    open(other, "w").close()
    args = [a.format(missing=str(tmp_path / "gone.hip"), hip=hip, notahip=other) for a in args]
    assert rop.main(args) == 2
    (fail,) = [ln for ln in lines(capsys) if ln.startswith("c4_fail")]
    assert fail.startswith(f"c4_fail: {cls} ") and words in fail, fail


def test_a_render_error_or_a_missing_frame_is_a_retry(tmp_path, capsys):
    rop = script("houdini-rop", "houdini_rop")
    hip, cache = _scene(tmp_path)
    cache.fail_render = "Error: cannot write geometry"
    assert rop.main([hip, "/out/cache", "1001-1002"]) == 1
    assert lines(capsys)[-1] == "c4_fail: retry /out/cache failed to render: Error: cannot write geometry"

    cache.fail_render = ""
    cache.skip_frames = (1002, 1003)
    assert rop.main([hip, "/out/cache", "1001-1003"]) == 1
    out = lines(capsys)
    assert [ln for ln in out if ln.startswith("c4_frame:")] == [f"c4_frame: 1001 {tmp_path}/shot/geo/fx.1001.bgeo.sc"]
    assert out[-1] == f"c4_fail: retry /out/cache rendered but wrote no {tmp_path}/shot/geo/fx.1002.bgeo.sc " \
                      "for frame 1002 (and 1 more frames)"


def test_load_warnings_are_logged_and_the_render_goes_on(tmp_path, capsys):
    rop = script("houdini-rop", "houdini_rop")
    hip, _ = _scene(tmp_path)
    hou.hipFile.load_warning = "Bad node type found: studio::cloud::2.0"
    assert rop.main([hip, "/out/cache", "1001"]) == 0
    out = lines(capsys)
    assert "houdini: the hip file loaded with warnings: Bad node type found: studio::cloud::2.0" in out
    assert out[-1].startswith("c4_frame: 1001 ")


def test_a_builder_that_saves_nothing_is_named(tmp_path, capsys):
    rop = script("houdini-rop", "houdini_rop")
    py = tmp_path / "build.py"
    py.write_text("import hou\nhou.node('/obj').createNode('geo', 'x')\n")
    assert rop.main([str(py), "/out/cache", "1001"]) == 2
    assert lines(capsys)[-1] == f"c4_fail: permanent build.py did not save the scene to {tmp_path}/build.hip"
    py.write_text("raise ValueError('no box')\n")
    assert rop.main([str(py), "/out/cache", "1001"]) == 2
    assert lines(capsys)[-1] == "c4_fail: permanent build.py did not build the scene: no box"


# --- pdg-cook ----------------------------------------------------------------------------


class _State:
    def __init__(self, name):
        self.name = name


def fake_pdg():
    """The pdg names pdg_cook.py uses."""
    m = types.ModuleType("pdg")
    m.EventType = types.SimpleNamespace(WorkItemStateChange="EventType.WorkItemStateChange")
    m.workItemState = types.SimpleNamespace(**{n: _State(n) for n in (
        "Uncooked", "Waiting", "Scheduled", "Cooking", "CookedSuccess", "CookedCache", "CookedFail", "CookedCancel")})
    return m


class _Context:
    def __init__(self):
        self.handlers = []

    def addEventHandler(self, handler, event_type, pass_handler=False):
        assert event_type == "EventType.WorkItemStateChange" and not pass_handler
        self.handlers.append(handler)
        return handler

    def removeEventHandler(self, handler):
        self.handlers.remove(handler)

    def emit(self, item, node, state):
        for h in list(self.handlers):
            h(types.SimpleNamespace(workItemId=item, node=types.SimpleNamespace(name=node), currentState=state))


@pytest.fixture
def pdg(monkeypatch):
    """A fake PDG cook: the TOP node's inputs' Generic Generators make the
    work items, and a Python Script TOP runs its script for each one (as
    the local scheduler would), with states sent to the graph's handlers."""
    mod = fake_pdg()
    monkeypatch.setitem(sys.modules, "pdg", mod)
    ctx = _Context()
    cooked = []
    st = mod.workItemState

    def getPDGGraphContext(self):
        return ctx

    def cookWorkItems(self, block=False, generate_only=False, tops_only=False, save_prompt=False):
        assert block, "the unit waits for the cook"
        cooked.append(self.path())
        gen = next(n for n in self.inputs() if n is not None and n.type().name() == "genericgenerator")
        for i in range(gen.parm("itemcount").eval()):
            ctx.emit(i, self.name(), st.Scheduled)
            ctx.emit(i, self.name(), st.Cooking)
            if self.type().name() == "pythonscript":
                exec(self.parm("script").eval(), {"work_item": types.SimpleNamespace(index=i)})
            ctx.emit(i, self.name(), getattr(self, "fail_items", {}).get(i, st.CookedSuccess))

    monkeypatch.setattr(hou.Node, "getPDGGraphContext", getPDGGraphContext, raising=False)
    monkeypatch.setattr(hou.Node, "cookWorkItems", cookWorkItems, raising=False)
    return types.SimpleNamespace(module=mod, ctx=ctx, cooked=cooked)


def test_cooks_the_test_scene_with_a_local_scheduler(tmp_path, capsys, pdg, monkeypatch):
    cook = script("pdg-cook", "pdg_cook")
    monkeypatch.setattr(cook, "unit_cpus", lambda: 6)
    t = tree(tmp_path, "pdg-cook")
    test = tool_dict("pdg-cook")["test"]
    assert cook.main([os.path.join(t, test["with"]["hip"]), test["with"]["top"]]) == 0
    assert all(os.path.isfile(os.path.join(t, e)) for e in test["expect"]), test["expect"]
    assert pdg.cooked == ["/obj/tops/write"], "a network cooks its output node"
    sched = hou.node("/obj/tops/c4_local")
    assert sched.type().name() == "localscheduler" and hou.node("/obj/tops").parm("topscheduler").eval() == sched.path()
    assert sched.parm("maxprocsmenu").eval() == "-1" and sched.parm("maxprocs").eval() == 6
    out = lines(capsys)
    progress = [int(ln.split()[1]) for ln in out if ln.startswith("openjd_progress:")]
    assert progress == sorted(progress) and progress[-1] == 100 and max(progress[:-1]) < 100, progress
    assert out[-1] == "openjd_status: 3 work items cooked"
    assert "openjd_status: cooking with a local scheduler, 6 slots" in out
    assert not pdg.ctx.handlers, "the handler is removed after the cook"
    assert not [ln for ln in out if ln.startswith("c4_fail")]


def test_failed_work_items_fail_the_unit(tmp_path, capsys, pdg):
    cook = script("pdg-cook", "pdg_cook")
    t = tree(tmp_path, "pdg-cook")
    cook.load_scene(os.path.join(t, "fx", "pdg_setup.py"))
    hou.node("/obj/tops/write").fail_items = {1: pdg.module.workItemState.CookedFail}
    assert cook.main([os.path.join(t, "fx", "pdg_setup.hip"), "/obj/tops/write"]) == 1
    out = lines(capsys)
    assert out[-1] == "c4_fail: retry 1 work items failed, in write"
    assert "openjd_status: 1 of 2 work items so far cooked, 1 failed" in out
    assert not [ln for ln in out if ln == "openjd_progress: 100"]


def test_cook_targets(tmp_path, pdg):
    cook = script("pdg-cook", "pdg_cook")
    hou.reset(hip=str(tmp_path / "a.hip"))
    tops = hou.node("/obj").createNode("topnet", "tops")
    gen = tops.createNode("genericgenerator", "gen")
    write = tops.createNode("pythonscript", "write")
    write.setInput(0, gen)
    hou.node("/obj").createNode("topnet", "empty")
    hou.node("/obj").createNode("geo", "geo1")
    gen.setDisplayFlag(True)
    assert cook.cook_target("/obj/tops").path() == "/obj/tops/gen", "the node with the display flag"
    assert cook.cook_target("/obj/tops/write").path() == "/obj/tops/write", "a TOP node is cooked itself"
    for path, words in (("/obj/empty", "has no output node; set the display flag"),
                        ("/obj/geo1", "/obj/geo1 is a geo node, not a TOP network or a TOP node"),
                        ("/obj/gone", "the hip file has no node /obj/gone")):
        with pytest.raises(cook.Fail) as e:
            cook.cook_target(path)
        assert e.value.cls == "permanent" and words in e.value.reason, e.value.reason


def test_local_scheduler_is_made_once_and_only_where_a_network_has_one(tmp_path, pdg):
    cook = script("pdg-cook", "pdg_cook")
    hou.reset(hip=str(tmp_path / "a.hip"))
    tops = hou.node("/obj").createNode("topnet", "tops")
    sub = tops.createNode("subnet", "inner")
    node = sub.createNode("pythonscript", "write")
    assert [n.path() for n in cook.networks(node)] == ["/obj/tops/inner", "/obj/tops"]
    assert cook.local_scheduler(sub, 4) is None, "a TOP subnet cooks with its network's scheduler"
    a = cook.local_scheduler(tops, 4)
    b = cook.local_scheduler(tops, 8)
    assert a.path() == b.path() == "/obj/tops/c4_local" and b.parm("maxprocs").eval() == 8
    assert len([n for n in tops.children() if n.type().name() == "localscheduler"]) == 1


def test_unit_cpus_from_the_cgroup_quota(tmp_path):
    cook = script("pdg-cook", "pdg_cook")
    q = tmp_path / "cpu.max"
    q.write_text("400000 100000\n")
    assert cook.unit_cpus(str(q)) == 4
    q.write_text("50000 100000\n")
    assert cook.unit_cpus(str(q)) == 1
    q.write_text("max 100000\n")
    assert cook.unit_cpus(str(q)) >= 1
    assert cook.unit_cpus(str(tmp_path / "none")) >= 1


def test_progress_counts_work_items_as_they_finish():
    cook = script("pdg-cook", "pdg_cook")
    out = []
    p = cook.Progress(out.append)
    p.change(0, "gen", "workItemState.Scheduled")
    p.change(1, "gen", _State("Cooking"))
    assert out == [], "nothing has finished"
    p.change(0, "gen", _State("CookedSuccess"))
    p.change(2, "write", _State("Waiting"))
    p.change(1, "gen", "workItemState.CookedCache")
    p.change(2, "write", _State("CookedSuccess"))
    assert [ln for ln in out if ln.startswith("openjd_progress")] == [
        "openjd_progress: 50", "openjd_progress: 66", "openjd_progress: 99"], "never down, and not 100 before the end"
    p.finish()
    assert out[-2:] == ["openjd_progress: 100", "openjd_status: 3 work items cooked"]

    q = cook.Progress(out.append)
    q.change(0, "write", _State("CookedFail"))
    q.change(1, "render", _State("CookedFail"))
    with pytest.raises(cook.Fail) as e:
        q.finish()
    assert e.value.cls == "retry" and e.value.reason == "2 work items failed, in render, write"
    q.change(0, "write", _State("CookedSuccess"))
    assert q.failed == {1: "render"}, "a work item cooked again after failing is no longer failed"
