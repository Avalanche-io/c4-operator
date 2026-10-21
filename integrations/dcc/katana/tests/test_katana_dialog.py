"""The dialog, driven the way an artist drives it, against the fake farm."""

import copy
import json
import os
import re
import threading
import time

import fakekatana as fk
import fakekatanaqt as qt
import pytest
from c4submit import collect, yamlio
from c4submit.form import Form
from c4submit.testing import SALTMARSH, FakeFarm
from c4submit.tool import Tool
from katana_scenes import KATANA_RENDER, light_project

from c4_katana import dialog
from c4_katana.dialog import EACH_NODE, STALE, SUGGESTED, TYPED, form_specs, widget_kind

# Words an artist never reads in this dialog.
BANNED = re.compile(r"\b(image|images|pod|pods|namespace|namespaces|priority|priorities|kubernetes|k8s|"
                    r"container|containers|kubectl|cluster|deployment)\b", re.I)
# The artist's own paths are theirs, not the dialog's words.
PATH = re.compile(r"\S*[/\\]\S*")


@pytest.fixture
def farm():
    return FakeFarm(tools=[KATANA_RENDER], projects={"saltmarsh": dict(SALTMARSH, root="/show")})


def settle(d, timeout: float = 30) -> None:
    """Qt's event loop, until the dialog's work is done."""
    end = time.time() + timeout
    while time.time() < end:
        qt.fire_timers()
        if not d.working() and not d._timer.isActive():
            return
        time.sleep(0.002)
    raise TimeoutError("the dialog's work never finished")


def opened(tmp_path, farm, nodes=None, dirty=False):
    host, project, _ = light_project(tmp_path, dirty=dirty)
    d = dialog.open_dialog(nodes=nodes, host=host, farm=farm)
    settle(d)
    return d


def text(d, name):
    return dialog.read_widget(d.kinds[name], d.w[name])


def summary(d):
    return d.w["c4_summary"].toPlainText()


def check(d):
    assert qt.click(d.w["c4_check"])
    settle(d)


def order(d):
    return [w.objectName() for w in d.main.widgets() if w.objectName()]


# --- widget specs ---------------------------------------------------------------------

def test_widget_specs_from_the_katana_render_tool():
    specs = form_specs(Form(Tool({"tool": KATANA_RENDER})))
    got = [(s.kind, s.name, s.label) for s in specs]
    assert got == [
        ("file", "p_scene", "Project"),
        ("linked", "p_scene__linked", "linked"),
        ("line", "p_node", "Render node"),
        ("linked", "p_node__linked", "linked"),
        ("fold", "g_1_folded", "Advanced"),
        ("int", "p_threads", "Threads"),
        ("check", "p_verbose", "Verbose log"),
        ("end", "g_1_folded_end", ""),
    ]
    by = {s.name: s for s in specs}
    assert by["p_scene"].file_mode == "open"
    assert by["p_scene"].file_filter == "Katana projects (*.katana);;All files (*)"
    assert (by["p_threads"].minimum, by["p_threads"].maximum, by["p_threads"].value) == (0, 128, 0)
    assert by["p_node__linked"].value is True and by["p_verbose"].value is False


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
    assert [(s.kind, s.name, s.label) for s in specs] == [
        ("group", "g_0", "Render"),
        ("combo", "p_engine", "Engine"),
        ("text", "p_notes", "Notes"),
        ("end", "g_0_end", ""),
        ("fold", "g_0_folded", "Render: advanced"),
        ("check", "p_slate", "Slate"),
        ("end", "g_0_folded_end", ""),
        ("float", "p_gain", "Gain"),
        ("file", "p_outdir", "Outdir"),
        ("file", "p_movie", "Movie"),
        ("line", "p_tags", "Tags"),
    ], "wrangler fields are not drawn"
    by = {s.name: s for s in specs}
    assert by["p_engine"].items == ["a", "b"] and by["p_engine"].value == "b"
    assert by["p_gain"].value == 1.5 and (by["p_gain"].minimum, by["p_gain"].maximum) == (0.0, 4.0)
    assert by["p_outdir"].file_mode == "dir" and by["p_movie"].file_mode == "save"
    assert by["p_tags"].value == "a, b"
    assert widget_kind(Form(tool).field("slate")) == "check"


# --- the whole flow -----------------------------------------------------------------------

def test_open_check_submit(tmp_path, farm):
    d = opened(tmp_path, farm, dirty=True)
    assert d.shown == 1 and d.windowTitle() == "Submit to C4"
    names = order(d)
    assert names[:7] == ["c4_tool", "c4_project", "c4_shot", "c4_due", "c4_due_time", "c4_frames", "c4_frames_linked"]
    assert names[7:11] == ["c4_dynamic", "c4_renders", "c4_render_0", "c4_render_1"]
    assert "c4_size" not in names, "katana-render has no sizes"
    assert names[11] == "c4_note"
    assert names[12:20] == ["p_scene", "p_scene__browse", "p_scene__linked", "p_node", "p_node__linked",
                            "g_1_folded", "g_1_folded_body", "p_threads"]
    assert names[-5:] == ["c4_check", "c4_submit", "c4_show_file", "c4_save_file", "c4_summary"]
    body = [x for x in qt.created if x.objectName() == "g_1_folded_body"][-1]
    assert not d.w["g_1_folded"].isChecked() and not body.isVisible(), "Advanced starts folded"

    w = d.w
    assert w["c4_tool"].items() == ["Katana render"]
    assert text(d, "c4_project") == "saltmarsh" and text(d, "c4_shot") == "sm0140"
    assert w["c4_due"].items() == ["Dailies, Thu 18:00", "Client review, Fri 10:00", SUGGESTED, TYPED]
    assert text(d, "c4_due") == SUGGESTED and not w["c4_due_time"].isEnabled()
    assert text(d, "c4_frames") == "1001-1048" and text(d, "c4_frames_linked") is True
    assert w["c4_render_0"].text() == "Render_shadow  project range" and w["c4_render_0"].isChecked()
    assert w["c4_render_1"].text() == "Render_beauty  project range  (after Render_shadow)"
    assert text(d, "p_node") == EACH_NODE and text(d, "p_node__linked") is True
    assert text(d, "p_scene").endswith("sm0140_light_v002.katana")
    assert not w["c4_submit"].isEnabled() and not qt.click(w["c4_submit"]), "nothing checked yet"

    qt.choose(w["c4_due"], "Dailies, Thu 18:00")
    assert d.draft.due == "dailies"
    assert qt.click(w["c4_check"])
    assert not w["c4_check"].isEnabled(), "one check at a time"
    settle(d)
    assert fk.state().saves == 1, "the farm reads the saved project, so Check saves it"
    assert d.checked.ok, d.checked.errors
    s = summary(d)
    assert "sm0140-render-shadow-v001   Katana render · 48 frames in 12 units" in s
    assert "sm0140-render-beauty-v001   Katana render · 48 frames in 12 units, after sm0140-render-shadow-v001" in s
    assert re.search(r"estimate\s+it follows sm0140-render-shadow-v001", s), "no estimate until shadow exists"
    assert "Dailies, Thu 18:00 · slack" in s
    assert "8 found from sm0140_light_v002.katana" in s and "8 to upload" in s, \
        "shadow's own render is not collected for beauty"
    assert s.endswith("Ready: press Submit to send it to the farm.")
    progress = qt.state.progress[0]
    assert progress.closed and progress.labels[0] == "Checking with the farm"
    assert progress.labels[-1] == "Reading files: 8 of 8" and progress.values[-1] == 100
    assert farm.calls[-1][0] == "check" and not farm.store

    assert w["c4_submit"].isEnabled()
    assert qt.click(w["c4_submit"])
    settle(d)
    doc = farm.jobs[0]
    assert list(doc["jobs"]) == ["render-shadow", "render-beauty"], "one job per Render node"
    assert doc["jobs"]["render-beauty"]["after"] == "render-shadow"
    assert doc["jobs"]["render-beauty"]["with"]["node"] == "Render_beauty"
    assert doc["jobs"]["render-shadow"]["with"]["scene"] == "seq/sm0140/katana/sm0140_light_v002.katana"
    assert doc["due"] == "dailies" and len(farm.store) == 8
    s = summary(d)
    assert s.startswith("Submitted to the farm:\n  sm0140-render-shadow-v001\n  sm0140-render-beauty-v001")
    assert "8 files sent" in s
    assert not w["c4_submit"].isEnabled(), "a check submits once"

    # Everything the artist could read, in plain production words.
    seen = [t for x in qt.created for t in x.texts()] + [d.checked.yaml(), summary(d)]
    bad = sorted({m.group(0) for t in seen for m in BANNED.finditer(PATH.sub("", t or ""))})
    assert bad == []


def test_save_file_writes_the_submission(tmp_path, farm):
    d = opened(tmp_path, farm)
    qt.type_text(d.w["c4_note"], "light v2: warmer key")
    assert not d.w["c4_save_file"].isEnabled(), "a file is what a check checked"
    check(d)
    os.makedirs(str(tmp_path / "out"))
    qt.state.next_filename = str(tmp_path / "out" / "sm0140-light")
    assert qt.click(d.w["c4_save_file"])
    asked = qt.state.asked[-1]
    assert asked["how"] == "save" and asked["filter"] == "Submission (*.yaml)"
    assert asked["directory"] == os.path.join(str(tmp_path), "seq", "sm0140", "katana", "sm0140-render-shadow.yaml")
    path = str(tmp_path / "out" / "sm0140-light.yaml")
    with open(path) as f:
        body = f.read()
    assert body.startswith("# Saved from katana by c4submit")
    doc = yamlio.load(body)
    assert doc == d.checked.doc and doc["note"] == "light v2: warmer key"
    assert summary(d).startswith(f"Saved {path}.")
    assert not farm.jobs, "saving submits nothing"
    assert qt.click(d.w["c4_show_file"])
    assert summary(d) == d.checked.yaml()
    qt.click(d.w["c4_show_file"])
    assert summary(d).startswith("Saved ")


def test_submit_is_refused_after_a_change(tmp_path, farm):
    d = opened(tmp_path, farm)
    check(d)
    assert d.w["c4_submit"].isEnabled()
    qt.type_text(d.w["c4_shot"], "sm0150")
    assert not d.w["c4_submit"].isEnabled() and summary(d).startswith(STALE)
    assert d.submit() is False and not farm.jobs
    assert d.save_file(str(tmp_path / "x.yaml")) is None, "nor saved: the file would not be what was checked"
    qt.type_text(d.w["c4_shot"], "sm0140")
    assert d.w["c4_submit"].isEnabled(), "back as it was checked"
    qt.click(d.w["c4_render_1"])
    assert not d.w["c4_submit"].isEnabled(), "unticking a Render node is a change"
    qt.click(d.w["c4_render_1"])
    assert d.w["c4_submit"].isEnabled()
    fk.state().dirty = True    # the artist edits the project
    assert d.submit() is False and not farm.jobs
    check(d)
    assert d.submit() is True
    settle(d)
    assert len(farm.jobs) == 1


def test_the_render_nodes_farm_menu_ticks_only_it(tmp_path, farm):
    d = opened(tmp_path, farm, nodes=["Render_shadow"])
    assert d.draft.ticked == ["Render_shadow"]
    assert not d.w["c4_render_1"].isChecked()
    assert text(d, "p_node") == "Render_shadow", "one node: the field shows it"
    check(d)
    assert list(d.checked.doc["jobs"]) == ["render"]
    assert d.checked.doc["jobs"]["render"]["with"]["node"] == "Render_shadow"
    assert "6 found" not in summary(d) and "7 found" in summary(d), "no fog: only beauty reads it"


def test_one_node_alone_needs_the_others_render(tmp_path, farm):
    d = opened(tmp_path, farm, nodes=["Render_beauty"])
    check(d)
    assert not d.checked.ok
    s = summary(d)
    assert s.startswith("To fix before submitting:\n  shadow_map.parameters.filename.value: ")
    assert "shadow.####.exr: no such files" in s and s.endswith("Check again.")
    qt.click(d.w["c4_render_0"])
    check(d)
    assert d.checked.ok and d.checked.doc["jobs"]["render-beauty"]["after"] == "render-shadow"


def test_frames_follow_each_node_until_typed(tmp_path, farm):
    host, _, shadow = light_project(tmp_path)
    shadow.range = (1001.0, 1010.0)
    d = dialog.open_dialog(host=host, farm=farm)
    settle(d)
    assert text(d, "c4_frames") == "each Render node's own range"
    assert d.w["c4_render_0"].text() == "Render_shadow  1001-1010"
    qt.type_text(d.w["c4_frames"], "1001-1005")
    assert d.draft.frames == "1001-1005" and text(d, "c4_frames_linked") is False
    check(d)
    assert {j["frames"] for j in d.checked.doc["jobs"].values()} == {"1001-1005"}
    qt.click(d.w["c4_frames_linked"])
    assert d.draft.frames is None and text(d, "c4_frames") == "each Render node's own range"
    check(d)
    assert d.checked.doc["jobs"]["render-shadow"]["frames"] == "1001-1010"


def test_needed_by_a_typed_time(tmp_path, farm):
    d = opened(tmp_path, farm)
    d.w["c4_due_time"].setEnabled(True)
    qt.type_text(d.w["c4_due_time"], "fri 18:00")
    assert text(d, "c4_due") == TYPED and d.draft.due == "fri 18:00" and d.w["c4_due_time"].isEnabled()
    qt.choose(d.w["c4_due"], SUGGESTED)
    assert d.draft.due == "suggested" and not d.w["c4_due_time"].isEnabled()
    check(d)
    assert "due" not in d.checked.doc


def test_parameter_widgets_edit_the_form(tmp_path, farm):
    d = opened(tmp_path, farm)
    qt.click(d.w["g_1_folded"])
    assert [w for w in qt.created if w.objectName() == "g_1_folded_body"][-1].isVisible(), "Advanced unfolds"
    qt.spin(d.w["p_threads"], 8)
    qt.click(d.w["p_verbose"])
    qt.type_text(d.w["p_node"], "Render_x")
    assert text(d, "p_node__linked") is False and d.draft.form.field("node").value == "Render_x"
    qt.click(d.w["p_node__linked"])
    assert text(d, "p_node") == EACH_NODE
    check(d)
    w = d.checked.doc["jobs"]["render-beauty"]["with"]
    assert w["threads"] == 8 and w["verbose"] is True and w["node"] == "Render_beauty"


def test_browse_for_a_project(tmp_path, farm):
    d = opened(tmp_path, farm)
    other = str(tmp_path / "seq" / "sm0140" / "katana" / "sm0140_light_v001.katana")
    qt.state.next_filename = other
    qt.click([w for w in qt.created if w.objectName() == "p_scene__browse"][-1])
    assert qt.state.asked[-1]["how"] == "open" and qt.state.asked[-1]["filter"].startswith("Katana projects (*.katana)")
    assert text(d, "p_scene") == other and text(d, "p_scene__linked") is False


def test_size_shows_for_a_tool_with_sizes(tmp_path):
    big = copy.deepcopy(KATANA_RENDER)
    big.update(name="katana-render-big", title="Katana render, heavy scenes")
    big["needs"]["sizes"] = {"heavy": {"memory": "256G"}}
    farm = FakeFarm(tools=[KATANA_RENDER, big], projects={"saltmarsh": dict(SALTMARSH, root="/show")})
    d = opened(tmp_path, farm)
    assert d.w["c4_tool"].items() == ["Katana render", "Katana render, heavy scenes"]
    qt.type_text(d.w["c4_note"], "kept across tools")
    qt.choose(d.w["c4_tool"], "Katana render, heavy scenes")
    assert d.draft.tool.name == "katana-render-big" and d.draft.note == "kept across tools"
    names = order(d)
    assert names.index("c4_size") == names.index("c4_note") - 1
    assert d.w["c4_size"].items() == ["Standard", "heavy"]
    qt.choose(d.w["c4_size"], "heavy")
    check(d)
    assert d.checked.ok and {j["size"] for j in d.checked.doc["jobs"].values()} == {"heavy"}


def test_cancel_a_check(tmp_path, farm, monkeypatch):
    d = opened(tmp_path, farm)
    gate, real = threading.Event(), collect.identify_file

    def slow(path, cancelled=None):
        gate.wait(10)       # the first file waits until the artist has pressed Cancel
        return real(path, cancelled)

    monkeypatch.setattr(collect, "identify_file", slow)
    assert qt.click(d.w["c4_check"])
    qt.state.cancel = True
    qt.fire_timers()           # the main thread sees Cancel pressed
    gate.set()
    settle(d)
    assert d.checked is None and summary(d) == "Check cancelled."
    assert qt.state.progress[-1].closed
    assert d.w["c4_check"].isEnabled() and not d.w["c4_submit"].isEnabled()


def test_a_farm_without_tools_yet(tmp_path):
    d = opened(tmp_path, FakeFarm(serves_submissions=False))
    assert d.draft is None
    assert "does not serve tools yet" in summary(d) and "C4_TOOL_PATH" in summary(d)
    assert not d.w["c4_check"].isEnabled()


def test_tools_from_c4_tool_path_on_a_farm_without_them(tmp_path, monkeypatch):
    tools = tmp_path / "tools" / "katana-render"
    tools.mkdir(parents=True)
    (tools / "tool.yaml").write_text(json.dumps(KATANA_RENDER))
    monkeypatch.setenv("C4_TOOL_PATH", str(tmp_path / "tools"))
    d = opened(tmp_path / "show", FakeFarm(serves_submissions=False))
    assert "p_node" in d.w
    check(d)
    c = d.checked
    assert c.errors == [] and not c.farm_checked and not d.w["c4_submit"].isEnabled()
    assert summary(d).startswith("Worth knowing:\n  this farm does not take submissions yet")
    path = d.save_file(str(tmp_path / "sm0140.yaml"))
    assert path and yamlio.load(open(path).read()) == c.doc


def test_no_farm_address(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path))     # no farm config either
    host, _, _ = light_project(tmp_path)
    d = dialog.open_dialog(host=host)
    settle(d)
    assert "C4_HUD" in summary(d)


def test_another_project(tmp_path, farm):
    farm.projects["heron"] = {"name": "heron", "milestones": []}
    d = opened(tmp_path, farm)
    qt.type_text(d.w["c4_project"], "heron")
    settle(d)
    assert d.draft.project == "heron" and d.w["c4_due"].items() == [SUGGESTED, TYPED]


def test_a_render_node_added_after_opening_is_drawn_at_check(tmp_path, farm):
    d = opened(tmp_path, farm)
    fk.render("Render_rim", fk.state().nodes["settings"], outputs=[str(tmp_path / "render" / "rim.####.exr")])
    check(d)
    assert d.w["c4_render_2"].text().startswith("Render_rim") and d.w["c4_render_2"].isChecked()
    assert list(d.checked.doc["jobs"]) == ["render-shadow", "render-beauty", "render-rim"]
    assert d.w["c4_submit"].isEnabled()


def test_opening_again_closes_the_old_dialog(tmp_path, farm):
    d1 = opened(tmp_path, farm)
    d2 = dialog.open_dialog(host=d1.host, farm=farm)
    settle(d2)
    assert d1.closed == 1 and dialog._open == [d2]
