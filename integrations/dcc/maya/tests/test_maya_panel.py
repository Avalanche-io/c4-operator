"""The window, driven the way an artist drives it, against the fake farm."""

import copy
import json
import os
import re

import fakemaya as fm
import pytest
from c4submit import yamlio
from c4submit.form import Form
from c4submit.testing import SALTMARSH, FakeFarm
from c4submit.tool import Tool
from maya_scenes import MAYA_RENDER, light_scene

from c4_maya import panel
from c4_maya.panel import EACH_LAYER, STALE, SUGGESTED, TYPED, form_specs, widget_kind

# Words an artist never reads in this window.
BANNED = re.compile(r"\b(image|images|pod|pods|namespace|namespaces|priority|priorities|kubernetes|k8s|"
                    r"container|containers|kubectl|cluster|deployment)\b", re.I)
# The artist's own paths (Maya's "images" folder) are theirs, not the window's words.
PATH = re.compile(r"\S*[/\\]\S*")


@pytest.fixture
def farm():
    return FakeFarm(tools=[MAYA_RENDER], projects={"saltmarsh": dict(SALTMARSH, root="/show")})


def opened(tmp_path, farm, modified=False, **kw):
    host, scene = light_scene(tmp_path, modified=modified, **kw)
    w = panel.open_panel(host=host, farm=farm)
    fm.settle(w)
    return w


def value(name):
    return fm.control(name).value


def summary():
    return value("c4_summary")


def enabled(name):
    return bool(fm.control(name).get("enable"))


def check(w):
    assert fm.press("c4_check")
    fm.settle(w)


# --- widget specs -------------------------------------------------------------------

def test_widget_specs_from_the_maya_render_tool():
    specs = form_specs(Form(Tool({"tool": MAYA_RENDER})))
    got = [(s.kind, s.name, s.label, s.role) for s in specs]
    assert got == [
        ("textFieldButtonGrp", "c4p_scene", "Scene", "value"),
        ("checkBox", "c4p_scene_linked", "linked", "linked"),
        ("textFieldGrp", "c4p_layer", "Render layer", "value"),
        ("checkBox", "c4p_layer_linked", "linked", "linked"),
        ("textFieldGrp", "c4p_camera", "Camera", "value"),
        ("checkBox", "c4p_camera_linked", "linked", "linked"),
        ("optionMenuGrp", "c4p_renderer", "Renderer", "value"),
        ("frameLayout", "c4g_1_folded", "Advanced", "begin"),
        ("intFieldGrp", "c4p_threads", "Threads", "value"),
        ("checkBoxGrp", "c4p_verbose", "Verbose log", "value"),
        ("frameLayout", "c4g_1_folded_end", "", "end"),
    ]
    by = {s.name: s for s in specs}
    assert by["c4p_scene"].file_mode == 1 and by["c4p_scene"].file_filter == "Maya scenes (*.ma *.mb);;All files (*.*)"
    assert by["c4p_layer"].value == "defaultRenderLayer", "the tool's default until the scene says"
    assert by["c4p_layer_linked"].value is True
    assert by["c4p_renderer"].items == ["arnold", "mayaSoftware"] and by["c4p_renderer"].value == "arnold"
    assert by["c4p_threads"].value == 0 and "at most 128" in by["c4p_threads"].tooltip
    assert by["c4g_1_folded"].collapsed is True
    assert by["c4p_verbose"].value is False


def test_widget_specs_for_every_control():
    tool = Tool({"name": "t", "kind": "once", "params": [
        {"name": "engine", "type": "STRING", "default": "b", "allowedValues": ["a", "b"],
         "userInterface": {"control": "DROPDOWN_LIST", "groupLabel": "Render"}},
        {"name": "notes", "type": "STRING", "userInterface": {"control": "MULTILINE_EDIT", "groupLabel": "Render"}},
        {"name": "gain", "type": "FLOAT", "default": 1.5, "minValue": 0.0, "maxValue": 4.0},
        {"name": "outdir", "type": "PATH", "objectType": "DIRECTORY"},
        {"name": "movie", "type": "PATH", "dataFlow": "OUT"},
        {"name": "tags", "type": "LIST[STRING]", "default": ["a", "b"]},
        {"name": "slate", "type": "BOOL", "default": True, "show": "advanced", "userInterface": {"groupLabel": "Render"}},
        {"name": "secret", "type": "STRING", "show": "wrangler"},
    ]})
    specs = form_specs(Form(tool))
    got = [(s.kind, s.name, s.label) for s in specs]
    assert got == [
        ("frameLayout", "c4g_0", "Render"),
        ("optionMenuGrp", "c4p_engine", "Engine"),
        ("scrollField", "c4p_notes", "Notes"),
        ("frameLayout", "c4g_0_end", ""),
        ("frameLayout", "c4g_0_folded", "Render: advanced"),
        ("checkBoxGrp", "c4p_slate", "Slate"),
        ("frameLayout", "c4g_0_folded_end", ""),
        ("floatFieldGrp", "c4p_gain", "Gain"),
        ("textFieldButtonGrp", "c4p_outdir", "Outdir"),
        ("textFieldButtonGrp", "c4p_movie", "Movie"),
        ("textFieldGrp", "c4p_tags", "Tags"),
    ], "wrangler fields are not drawn"
    by = {s.name: s for s in specs}
    assert by["c4p_engine"].value == "b" and by["c4p_gain"].value == 1.5 and by["c4p_tags"].value == "a, b"
    assert by["c4p_outdir"].file_mode == 3 and by["c4p_outdir"].file_filter == ""
    assert by["c4p_movie"].file_mode == 0
    assert widget_kind(Form(tool).field("slate")) == "checkBoxGrp"


# --- the whole flow -------------------------------------------------------------------

def test_open_check_submit(tmp_path, farm):
    w = opened(tmp_path, farm, modified=True)
    assert fm.control(panel.WINDOW).shown == 1
    order = [n for n in fm.order(w.main) if not n.startswith(("menuItem", "OptionMenu"))]
    assert order[:9] == ["c4_tool", "c4_project", "c4_shot", "c4_due_row", "c4_due", "c4_due_time",
                         "c4_frames_row", "c4_frames", "c4_frames_linked"]
    assert order[9:13] == ["c4_dynamic", "c4_layers", "c4_layer_0", "c4_layer_1"]
    assert "c4_size" not in order, "maya-render has no sizes"
    assert order[13:15] == ["c4_note_label", "c4_note"]
    assert [n for n in order[15:] if n.startswith("c4p_") and not n.endswith("_row")] == [
        "c4p_scene", "c4p_scene_linked", "c4p_layer", "c4p_layer_linked", "c4p_camera", "c4p_camera_linked",
        "c4p_renderer", "c4p_threads", "c4p_verbose"]
    assert order.index("c4g_1_folded") < order.index("c4p_threads")
    assert order[-6:] == ["c4_buttons", "c4_check", "c4_submit", "c4_show_file", "c4_save_file", "c4_summary"]
    assert fm.control("c4g_1_folded").get("collapse") is True, "advanced starts folded"

    assert [fm.control(i).get("label") for i in fm.control("c4_tool").items] == ["Maya render"]
    assert value("c4_project") == "saltmarsh" and value("c4_shot") == "sm0140"
    assert [fm.control(i).get("label") for i in fm.control("c4_due").items] == [
        "Dailies, Thu 18:00", "Client review, Fri 10:00", SUGGESTED, TYPED]
    assert value("c4_due") == SUGGESTED and not enabled("c4_due_time")
    assert value("c4_frames") == "each render layer's own range" and value("c4_frames_linked") is True
    assert fm.control("c4_layer_0").get("label") == "beauty  scene range" and value("c4_layer_0") is True
    assert fm.control("c4_layer_1").get("label") == "fg  1001-1010  (its own range)"
    assert value("c4p_layer") == EACH_LAYER and value("c4p_layer_linked") is True
    assert value("c4p_camera") == "shotCam"
    assert value("c4p_scene").endswith("sm0140_light_v003.ma")
    assert not enabled("c4_submit") and not fm.press("c4_submit"), "nothing checked yet"

    fm.type_into("c4_due", "Dailies, Thu 18:00")
    assert w.draft.due == "dailies"
    assert fm.press("c4_check")
    assert not enabled("c4_check"), "one check at a time"
    assert fm.state().progress is not None and fm.state().progress["title"] == "Checking with the farm"
    fm.settle(w)
    assert fm.state().saves == 1, "the farm reads the saved scene, so Check saves it"
    assert fm.state().progress is None, "the progress window closes"
    assert w.checked.ok, w.checked.errors
    text = summary()
    assert "sm0140-render-beauty-v001   Maya render · 48 frames in 12 units" in text
    assert "sm0140-render-fg-v001   Maya render · 10 frames in 3 units" in text
    assert "after" not in text, "layers do not wait for each other"
    assert len(re.findall(r"estimate\s+ready", text)) == 2, "each layer has its own estimate"
    assert "Dailies, Thu 18:00 · slack" in text, "the milestone as the project names it"
    assert "76 found from sm0140_light_v003.ma" in text and "76 to upload" in text
    assert "licences" not in text
    assert text.endswith("Ready: press Submit to send it to the farm.")
    assert farm.calls[-1][0] == "check" and not farm.store
    log = fm.state().progress_log[0]
    assert log["ended"] and log["log"][-1]["status"] == "Reading files: 76 of 76" and log["log"][-1]["progress"] == 100

    assert enabled("c4_submit")
    assert fm.press("c4_submit")
    fm.settle(w)
    assert len(farm.jobs) == 1
    doc = farm.jobs[0]
    assert list(doc["jobs"]) == ["render-beauty", "render-fg"], "one job per render layer"
    beauty, fg = doc["jobs"]["render-beauty"], doc["jobs"]["render-fg"]
    assert beauty["with"]["layer"] == "rs_beauty" and fg["with"]["layer"] == "rs_fg"
    assert beauty["frames"] == "1001-1048" and fg["frames"] == "1001-1010"
    assert beauty["with"]["camera"] == "shotCam" and beauty["with"]["scene"] == "seq/sm0140/maya/scenes/sm0140_light_v003.ma"
    assert "after" not in beauty and "after" not in fg, "layers do not wait for each other"
    assert doc["due"] == "dailies" and len(farm.store) == 76
    text = summary()
    assert text.startswith("Submitted to the farm:\n  sm0140-render-beauty-v001\n  sm0140-render-fg-v001")
    assert "76 files sent" in text
    assert not enabled("c4_submit"), "a check submits once"

    # Everything the artist could read, in plain production words.
    seen = [str(c.flags.get(f, "")) for c in fm.created for f in ("label", "text", "annotation", "title", "status",
                                                                  "buttonLabel", "caption")]
    seen += [str(v) for p in fm.state().progress_log for e in p["log"] for v in e.values()]
    seen += [w.checked.yaml(), summary()]
    bad = sorted({m.group(0) for s in seen for m in BANNED.finditer(PATH.sub("", s))})
    assert bad == []


def test_save_file_writes_the_submission(tmp_path, farm):
    w = opened(tmp_path, farm)
    fm.type_into("c4_note", "light v3: warmer key")
    assert not enabled("c4_save_file"), "a file is what a check checked"
    check(w)
    os.makedirs(str(tmp_path / "out"))
    fm.state().next_filename = str(tmp_path / "out" / "sm0140-light")
    assert fm.press("c4_save_file")
    asked = fm.state().asked[-1]
    assert asked["fileMode"] == 0 and asked["fileFilter"] == "Submission (*.yaml)"
    assert asked["startingDirectory"] == os.path.join(str(tmp_path), "seq", "sm0140", "maya", "scenes",
                                                      "sm0140-render-beauty.yaml")
    path = str(tmp_path / "out" / "sm0140-light.yaml")
    with open(path) as f:
        text = f.read()
    assert text.startswith("# Saved from maya by c4submit")
    doc = yamlio.load(text)
    assert doc == w.checked.doc and doc["note"] == "light v3: warmer key"
    assert summary().startswith(f"Saved {path}.")
    assert not farm.jobs, "saving submits nothing"

    assert fm.press("c4_show_file")
    assert summary() == w.checked.yaml()
    fm.press("c4_show_file")
    assert summary().startswith("Saved ")


def test_submit_is_refused_after_a_change(tmp_path, farm):
    w = opened(tmp_path, farm)
    check(w)
    assert enabled("c4_submit")

    fm.type_into("c4_shot", "sm0150")
    assert not enabled("c4_submit")
    assert summary().startswith(STALE)
    assert w.submit() is False and not farm.jobs
    assert w.save_file(str(tmp_path / "x.yaml")) is None, "nor saved: the file would not be what was checked"

    fm.type_into("c4_shot", "sm0140")
    assert enabled("c4_submit"), "back as it was checked"

    fm.type_into("c4_layer_1", False)
    assert not enabled("c4_submit"), "unticking a render layer is a change"
    fm.type_into("c4_layer_1", True)
    assert enabled("c4_submit")

    fm.state().modified = True    # the artist edits the scene
    assert w.submit() is False and not farm.jobs

    check(w)
    assert w.submit() is True
    fm.settle(w)
    assert len(farm.jobs) == 1


def test_typing_not_yet_committed_is_taken_in(tmp_path, farm):
    w = opened(tmp_path, farm)
    fm.type_only("c4_shot", "sm0150")
    fm.type_only("c4_note", "rim light pass")
    fm.type_only("c4p_camera", "witnessCam")
    check(w)
    doc = w.checked.doc
    assert doc["shot"] == "sm0150" and doc["note"] == "rim light pass"
    assert doc["jobs"]["render-beauty"]["with"]["camera"] == "witnessCam" and value("c4p_camera_linked") is False
    assert enabled("c4_submit")
    fm.type_only("c4_shot", "sm0160")
    assert w.submit() is False and not farm.jobs, "Submit sees it too, so it sends only what was checked"
    assert summary().startswith(STALE)


def test_saving_the_scene_after_a_check_needs_another(tmp_path, farm):
    w = opened(tmp_path, farm)
    check(w)
    with open(w.draft.scene_file, "a") as f:
        f.write("// a new light\n")
    fm.fire("SceneSaved")
    fm.settle(w)
    assert not enabled("c4_submit") and summary().startswith(STALE)


def test_one_layer_ticked(tmp_path, farm):
    w = opened(tmp_path, farm)
    fm.type_into("c4_layer_0", False)
    assert w.draft.ticked == ["rs_fg"]
    assert value("c4p_layer") == "rs_fg", "one layer: the field shows it"
    assert value("c4_frames") == "1001-1010"
    check(w)
    jobs = w.checked.doc["jobs"]
    assert list(jobs) == ["render"] and jobs["render"]["with"]["layer"] == "rs_fg"
    assert "18 found" in summary(), "only what fg reads: no caustics, no splash"


def test_each_layer_renders_its_own_camera(tmp_path, farm):
    host, _ = light_scene(tmp_path)
    fm.camera("closeCam", renderable=False)
    fm.state().overrides["rs_fg"].update({"closeCamShape.renderable": True, "shotCamShape.renderable": False})
    w = panel.open_panel(host=host, farm=farm)
    fm.settle(w)
    assert value("c4p_camera") == "each render layer's own camera" and value("c4p_camera_linked") is True
    fm.type_into("c4_layer_0", False)
    assert value("c4p_camera") == "closeCam", "fg alone: its camera"
    fm.type_into("c4_layer_0", True)
    check(w)
    jobs = w.checked.doc["jobs"]
    assert jobs["render-beauty"]["with"]["camera"] == "shotCam" and jobs["render-fg"]["with"]["camera"] == "closeCam"
    fm.type_into("c4p_camera", "witnessCam")
    check(w)
    assert {j["with"]["camera"] for j in w.checked.doc["jobs"].values()} == {"witnessCam"}, "typed: one for all"


def test_a_file_no_ticked_layer_reads_is_left_out(tmp_path, farm):
    host, _ = light_scene(tmp_path)
    fm.node("transform", "trees")
    fm.node("gpuCache", "treesShape", "trees", cacheFileName=str(tmp_path / "env" / "missing_trees.abc"),
            useFrameExtension=False)
    w = panel.open_panel(host=host, farm=farm)
    fm.settle(w)
    check(w)
    assert w.checked.ok, "the trees are in no renderable layer, so their missing cache is not asked for"


def test_no_renderable_layer(tmp_path, farm):
    host, _ = light_scene(tmp_path)
    for n in ("rs_beauty", "rs_fg"):
        fm.setAttr(n + ".renderable", False)
    w = panel.open_panel(host=host, farm=farm)
    fm.settle(w)
    assert fm.control("c4_layers").get("label") == "Render layers: none in this scene is renderable"
    check(w)
    assert list(w.checked.doc["jobs"]) == ["render"]
    assert "layer" not in w.checked.doc["jobs"]["render"]["with"], "the tool's default stands, so the file omits it"


def test_frames_follow_each_layer_until_typed(tmp_path, farm):
    w = opened(tmp_path, farm)
    fm.type_into("c4_frames", "1001-1005")
    assert w.draft.frames == "1001-1005" and value("c4_frames_linked") is False
    check(w)
    assert {j["frames"] for j in w.checked.doc["jobs"].values()} == {"1001-1005"}
    fm.type_into("c4_frames_linked", True)
    assert w.draft.frames is None and value("c4_frames") == "each render layer's own range"


def test_needed_by_a_typed_time(tmp_path, farm):
    w = opened(tmp_path, farm)
    fm.control("c4_due_time").flags["enable"] = True    # as if the artist chose "At a time I type"
    fm.type_into("c4_due_time", "fri 18:00")
    assert value("c4_due") == TYPED and w.draft.due == "fri 18:00"
    assert enabled("c4_due_time")
    fm.type_into("c4_due", SUGGESTED)
    assert w.draft.due == "suggested" and not enabled("c4_due_time")
    check(w)
    assert "due" not in w.checked.doc


def test_parameter_controls_edit_the_form(tmp_path, farm):
    w = opened(tmp_path, farm)
    fm.type_into("c4p_threads", 8)
    fm.type_into("c4p_renderer", "mayaSoftware")
    fm.type_into("c4p_verbose", True)
    fm.type_into("c4p_camera", "witnessCam")
    assert value("c4p_camera_linked") is False and w.draft.form.field("camera").value == "witnessCam"
    fm.type_into("c4p_layer", "rs_x")
    fm.type_into("c4p_layer_linked", True)
    assert value("c4p_layer") == EACH_LAYER
    check(w)
    with_ = w.checked.doc["jobs"]["render-fg"]["with"]
    assert with_["threads"] == 8 and with_["renderer"] == "mayaSoftware" and with_["verbose"] is True
    assert with_["camera"] == "witnessCam" and with_["layer"] == "rs_fg"


def test_browse_for_a_file(tmp_path, farm):
    w = opened(tmp_path, farm)
    other = str(tmp_path / "seq" / "sm0140" / "maya" / "scenes" / "sm0140_light_v002.ma")
    fm.state().next_filename = other
    fm.browse("c4p_scene")
    asked = fm.state().asked[-1]
    assert asked["fileMode"] == 1 and asked["fileFilter"].startswith("Maya scenes (*.ma *.mb)")
    assert value("c4p_scene") == other and value("c4p_scene_linked") is False
    assert w.draft.form.field("scene").value == other


def test_size_shows_for_a_tool_with_sizes(tmp_path):
    big = copy.deepcopy(MAYA_RENDER)
    big.update(name="maya-render-big", title="Maya render, heavy scenes")
    big["needs"]["sizes"] = {"heavy": {"memory": "256G"}}
    farm = FakeFarm(tools=[MAYA_RENDER, big], projects={"saltmarsh": dict(SALTMARSH, root="/show")})
    w = opened(tmp_path, farm)
    assert [fm.control(i).get("label") for i in fm.control("c4_tool").items] == [
        "Maya render", "Maya render, heavy scenes"]
    fm.type_into("c4_note", "kept across tools")
    fm.type_into("c4_tool", "Maya render, heavy scenes")
    assert w.draft.tool.name == "maya-render-big" and w.draft.note == "kept across tools"
    order = fm.order(w.dynamic)
    assert order.index("c4_size") < order.index("c4_note_label")
    assert [fm.control(i).get("label") for i in fm.control("c4_size").items] == ["Standard", "heavy"]
    fm.type_into("c4_size", "heavy")
    check(w)
    assert w.checked.ok and {j["size"] for j in w.checked.doc["jobs"].values()} == {"heavy"}


def test_cancel_a_check(tmp_path, farm):
    w = opened(tmp_path, farm)
    fm.state().cancel = True       # the artist presses Esc on the progress window
    check(w)
    assert w.checked is None and summary() == "Check cancelled."
    assert fm.state().progress is None
    assert enabled("c4_check") and not enabled("c4_submit")


def test_a_farm_without_tools_yet(tmp_path):
    w = opened(tmp_path, FakeFarm(serves_submissions=False))
    assert w.draft is None
    assert "does not serve tools yet" in summary() and "C4_TOOL_PATH" in summary()
    assert not enabled("c4_check")


def test_tools_from_c4_tool_path_on_a_farm_without_them(tmp_path, monkeypatch):
    tools = tmp_path / "tools" / "maya-render"
    tools.mkdir(parents=True)
    (tools / "tool.yaml").write_text(json.dumps(MAYA_RENDER))
    monkeypatch.setenv("C4_TOOL_PATH", str(tmp_path / "tools"))
    w = opened(tmp_path / "show", FakeFarm(serves_submissions=False))
    assert "c4p_layer" in w.ui
    check(w)
    c = w.checked
    assert c.errors == [] and not c.farm_checked and not enabled("c4_submit")
    assert summary().startswith("Worth knowing:\n  this farm does not take submissions yet")
    assert "Save file keeps it" in summary()
    path = w.save_file(str(tmp_path / "sm0140.yaml"))
    assert path and yamlio.load(open(path).read()) == c.doc


def test_a_missing_file_is_listed_with_its_fix(tmp_path, farm):
    w = opened(tmp_path, farm)
    for f in range(1, 11):
        os.remove(str(tmp_path / "seq" / "sm0140" / "maya" / "sourceimages" / "caustics" / f"caustic.{f:04d}.exr"))
    check(w)
    assert not w.checked.ok
    text = summary()
    assert text.startswith("To fix before submitting:\n  caustics: ") and text.endswith("Check again.")
    assert "caustic.####.exr: no such files" in text
    fm.type_into("c4_layer_0", False)
    check(w)
    assert w.checked.ok, "fg does not read the caustics"


def test_a_tool_that_needs_licences_is_refused_with_its_fix(tmp_path):
    counted = copy.deepcopy(MAYA_RENDER)
    counted["needs"]["licences"] = {"autodesk/arnold": 1}
    w = opened(tmp_path, FakeFarm(tools=[counted], projects={"saltmarsh": dict(SALTMARSH, root="/show")}))
    check(w)
    assert not w.checked.ok and not enabled("c4_submit")
    text = summary()
    assert text.startswith("To fix before submitting:\n  jobs.render-beauty.tool: maya-render needs licences")
    assert "      use a version of the tool that declares none for now" in text
    assert text.endswith("Check again.")


def test_no_farm_address(tmp_path):
    host, _ = light_scene(tmp_path)
    w = panel.open_panel(host=host)
    fm.settle(w)
    assert summary().startswith("the farm's address is not set: set C4_HUD")


def test_another_project(tmp_path, farm):
    farm.projects["heron"] = {"name": "heron", "milestones": []}
    w = opened(tmp_path, farm)
    fm.type_into("c4_project", "heron")
    fm.settle(w)
    assert w.draft.project == "heron"
    assert [fm.control(i).get("label") for i in fm.control("c4_due").items] == [SUGGESTED, TYPED]


def test_a_layer_added_after_opening_is_drawn_at_check(tmp_path, farm):
    w = opened(tmp_path, farm)
    fm.layer("rs_rim", members=["heron"])
    fm.press("c4_check")
    assert "c4_layer_2" not in w.ui, "not while Check's own callback runs"
    fm.settle(w)
    assert fm.control("c4_layer_2").get("label").startswith("rim") and value("c4_layer_2") is True
    assert list(w.checked.doc["jobs"]) == ["render-beauty", "render-fg", "render-rim"]
    assert enabled("c4_submit")


def test_opening_again_replaces_the_window(tmp_path, farm):
    w1 = opened(tmp_path, farm)
    host = w1.host
    w2 = panel.open_panel(host=host, farm=farm)
    fm.settle(w2)
    assert panel._open == [w2]
    assert len([c for c in fm.state().ui.values() if c.kind == "window"]) == 1
    assert len(fm.state().jobs) == len(panel.SCENE_EVENTS), "the old window's scene jobs went with it"
