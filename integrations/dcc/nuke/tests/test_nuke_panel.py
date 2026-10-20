"""The panel, driven the way an artist drives it, against the fake farm."""

import copy
import json
import os
import re

import fakenuke as nk
import pytest
from c4submit import yamlio
from c4submit.form import Form
from c4submit.testing import KARMA, NUKE_COMP, SALTMARSH, FakeFarm
from c4submit.tool import Tool
from nuke_scripts import comp_script

from c4_nuke import panel
from c4_nuke.panel import EACH_WRITE, STALE, SUGGESTED, TYPED, form_specs, knob_kind

# Words an artist never reads in this panel.
BANNED = re.compile(r"\b(image|images|pod|pods|namespace|namespaces|priority|priorities|kubernetes|k8s|"
                    r"container|containers|kubectl|cluster|deployment)\b", re.I)


@pytest.fixture
def farm():
    return FakeFarm(projects={"saltmarsh": dict(SALTMARSH, root="/show")})


def opened(tmp_path, farm, writes=None, modified=False):
    host, n = comp_script(tmp_path, modified=modified)
    p = panel.open_panel(writes=writes, host=host, farm=farm)
    nk.settle(p)
    return p, n


def value(p, name):
    return p.knobs()[name].value()


# --- knob specs ------------------------------------------------------------------

def test_knob_specs_from_the_nuke_comp_tool():
    form = Form(Tool({"tool": NUKE_COMP}))
    specs = form_specs(form)
    got = [(s.kind, s.name, s.label, s.role) for s in specs]
    assert got == [
        ("File_Knob", "p_script", "Script", "value"),
        ("Boolean_Knob", "p_script__linked", "linked", "linked"),
        ("String_Knob", "p_write", "Write node", "value"),
        ("Boolean_Knob", "p_write__linked", "linked", "linked"),
        ("Tab_Knob", "g_1_begin", "Advanced", "begin"),
        ("Boolean_Knob", "p_proxy", "Proxy mode", "value"),
        ("Int_Knob", "p_threads", "Threads", "value"),
        ("Tab_Knob", "g_1_end", "", "end"),
    ]
    by = {s.name: s for s in specs}
    assert by["p_write"].value == "Write_main", "the tool's default until the script says"
    assert by["p_write__linked"].value is True and by["p_write__linked"].startline is False, "beside its field"
    assert by["p_threads"].range == (0, 128) and by["p_threads"].value == 0
    assert by["p_proxy"].value is False


def test_knob_specs_for_every_control():
    tool = Tool({"name": "t", "kind": "once", "params": [
        {"name": "engine", "type": "STRING", "default": "BRAY_HdKarma", "allowedValues": ["BRAY_HdKarmaXPU", "BRAY_HdKarma"],
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
        ("Text_Knob", "g_0", "Render"),
        ("Enumeration_Knob", "p_engine", "Engine"),
        ("Multiline_Eval_String_Knob", "p_notes", "Notes"),
        ("Tab_Knob", "g_0_begin", "Render: advanced"),
        ("Boolean_Knob", "p_slate", "Slate"),
        ("Tab_Knob", "g_0_end", ""),
        ("Double_Knob", "p_gain", "Gain"),
        ("File_Knob", "p_outdir", "Outdir"),
        ("File_Knob", "p_movie", "Movie"),
        ("String_Knob", "p_tags", "Tags"),
    ], "wrangler fields are not drawn"
    by = {s.name: s for s in specs}
    assert by["p_engine"].items == ["BRAY_HdKarmaXPU", "BRAY_HdKarma"] and by["p_engine"].value == "BRAY_HdKarma"
    assert by["p_gain"].range == (0.0, 4.0) and by["p_gain"].value == 1.5
    assert by["p_tags"].value == "a, b"
    assert knob_kind(Form(Tool({"tool": KARMA})).field("renderer")) == "Enumeration_Knob"


# --- the whole flow ---------------------------------------------------------------

def test_open_check_submit(tmp_path, farm):
    p, n = opened(tmp_path, farm, modified=True)
    order = p.order()
    assert order[:7] == ["c4_tool", "c4_project", "c4_shot", "c4_due", "c4_due_time", "c4_frames", "c4_frames_linked"]
    assert order[7:10] == ["c4_writes", "c4_write_0", "c4_write_1"]
    assert "c4_size" not in order, "nuke-comp has no sizes"
    assert order[10] == "c4_note"
    assert order[11:19] == ["p_script", "p_script__linked", "p_write", "p_write__linked",
                            "g_1_begin", "p_proxy", "p_threads", "g_1_end"]
    assert order[-6:] == ["c4_buttons", "c4_check", "c4_submit", "c4_show_file", "c4_save_file", "c4_summary"]

    k = p.knobs()
    assert k["c4_tool"].values() == ["Nuke comp"], "only tools that run .nk scripts"
    assert value(p, "c4_project") == "saltmarsh" and value(p, "c4_shot") == "sm0140"
    assert k["c4_due"].values() == ["Dailies, Thu 18:00", "Client review, Fri 10:00", SUGGESTED, TYPED]
    assert value(p, "c4_due") == SUGGESTED
    assert value(p, "c4_frames") == "1001-1048" and value(p, "c4_frames_linked") is True
    assert k["c4_write_0"].label().startswith("Write_bg") and k["c4_write_0"].value() is True
    assert k["c4_write_1"].label() == "Write_main  1001-1048  (after Write_bg)"
    assert value(p, "p_write") == EACH_WRITE and value(p, "p_write__linked") is True
    assert value(p, "p_script").endswith("sm0140_comp_v004.nk")
    assert not k["c4_submit"].enabled() and not nk.press(p, "c4_submit"), "nothing checked yet"

    nk.type_into(p, "c4_due", "Dailies, Thu 18:00")
    assert p.draft.due == "dailies"
    assert nk.press(p, "c4_check")
    assert not k["c4_check"].enabled(), "one check at a time"
    nk.settle(p)
    assert nk.state().saves == 1, "the farm reads the saved script, so Check saves it"
    assert p.checked.ok, p.checked.errors
    text = value(p, "c4_summary")
    assert "sm0140-comp-bg-v001   Nuke comp · 48 frames" in text
    assert "after comp-bg" in text
    assert re.search(r"estimate\s+ready", text) and "dailies · slack" in text
    assert "98 found from sm0140_comp_v004.nk" in text and "98 to upload" in text
    assert text.endswith("Ready: press Submit to send it to the farm.")
    assert farm.calls[-1][0] == "check" and not farm.store
    hashing = [t for t in nk.state().tasks if t.title == "Checking with the farm"]
    assert hashing and hashing[0].messages[-1] == "Reading files: 98 of 98" and hashing[0].progress[-1] == 100

    assert k["c4_submit"].enabled()
    assert nk.press(p, "c4_submit")
    nk.settle(p)
    assert len(farm.jobs) == 1
    jobs = farm.jobs[0]["jobs"]
    assert list(jobs) == ["comp-bg", "comp-main"]
    assert jobs["comp-main"]["after"] == "comp-bg" and jobs["comp-main"]["with"]["write"] == "Write_main"
    assert farm.jobs[0]["due"] == "dailies"
    assert len(farm.store) == 98
    text = value(p, "c4_summary")
    assert text.startswith("Submitted to the farm:\n  sm0140-comp-bg-v001\n  sm0140-comp-main-v001")
    assert "98 files sent" in text
    assert not k["c4_submit"].enabled(), "a check submits once"

    # Everything the artist could read, in plain production words.
    seen = [s for kn in nk.created for s in (kn.label(), str(kn.value()), kn.tooltip())]
    seen += nk.state().messages + [t.title for t in nk.state().tasks] + [m for t in nk.state().tasks for m in t.messages]
    seen.append(p.checked.yaml())
    bad = sorted({m.group(0) for s in seen for m in BANNED.finditer(s)})
    assert bad == []


def test_save_file_writes_the_submission(tmp_path, farm):
    p, _ = opened(tmp_path, farm)
    nk.type_into(p, "c4_note", "comp v4: new edge treatment")
    assert not p.knobs()["c4_save_file"].enabled(), "a file is what a check checked"
    nk.press(p, "c4_check")
    nk.settle(p)
    nk.state().next_filename = str(tmp_path / "out" / "sm0140-comp")
    os.makedirs(str(tmp_path / "out"))
    assert nk.press(p, "c4_save_file")
    asked = nk.state().asked[-1]
    assert asked["default"] == os.path.join(str(tmp_path), "comp", "sm0140-comp-bg.yaml")
    path = str(tmp_path / "out" / "sm0140-comp.yaml")
    with open(path) as f:
        text = f.read()
    assert text.startswith("# Saved from nuke by c4submit")
    doc = yamlio.load(text)
    assert doc == p.checked.doc and doc["note"] == "comp v4: new edge treatment"
    assert value(p, "c4_summary").startswith(f"Saved {path}.")
    assert not farm.jobs, "saving submits nothing"

    assert nk.press(p, "c4_show_file")
    assert value(p, "c4_summary") == p.checked.yaml()
    nk.press(p, "c4_show_file")
    assert value(p, "c4_summary").startswith("Saved ")


def test_submit_is_refused_after_the_draft_changes(tmp_path, farm):
    p, n = opened(tmp_path, farm)
    nk.press(p, "c4_check")
    nk.settle(p)
    assert p.knobs()["c4_submit"].enabled()

    nk.type_into(p, "c4_shot", "sm0150")
    assert not p.knobs()["c4_submit"].enabled()
    assert value(p, "c4_summary").startswith(STALE)
    assert p.submit() is False and not farm.jobs
    assert p.save_file(str(tmp_path / "x.yaml")) is None, "nor saved: the file would not be what was checked"

    nk.type_into(p, "c4_shot", "sm0140")
    assert p.knobs()["c4_submit"].enabled(), "back as it was checked"

    nk.root().setModified(True)    # the artist edits the script
    assert p.submit() is False and not farm.jobs

    nk.press(p, "c4_check")
    nk.settle(p)
    assert p.submit() is True
    nk.settle(p)
    assert len(farm.jobs) == 1


def test_right_click_ticks_only_that_write(tmp_path, farm):
    host, n = comp_script(tmp_path)
    assert panel.submit_selected_writes(host=host, farm=farm, show=False) is None
    assert nk.state().messages == ["Select a Write node first, then choose Submit this Write to C4."]

    n["Write_bg"].selected = True
    p = panel.submit_selected_writes(host=host, farm=farm)
    nk.settle(p)
    assert p.shown == 1
    assert value(p, "c4_write_0") is True and value(p, "c4_write_1") is False
    assert p.draft.ticked == ["Write_bg"]
    assert value(p, "p_write") == "Write_bg", "one Write: the field shows it"
    nk.press(p, "c4_check")
    nk.settle(p)
    assert list(p.checked.doc["jobs"]) == ["comp"]
    assert p.checked.doc["jobs"]["comp"]["with"]["write"] == "Write_bg"


def test_one_write_alone_needs_the_others_render(tmp_path, farm):
    host, n = comp_script(tmp_path)
    n["Write_main"].selected = True
    p = panel.submit_selected_writes(host=host, farm=farm)
    nk.settle(p)
    nk.press(p, "c4_check")
    nk.settle(p)
    assert not p.checked.ok
    assert any(e["path"] == "Read_bg" and "bg.####.exr" in e["message"] for e in p.checked.errors)
    text = value(p, "c4_summary")
    assert text.startswith("To fix before submitting:\n  Read_bg: ") and text.endswith("Check again.")
    assert text.count("bg.####.exr: no such files") == 1, "each error once: the summary leaves them to the panel"
    assert "\n      fix the path, or render or copy the files first" in text
    nk.type_into(p, "c4_write_0", True)
    nk.press(p, "c4_check")
    nk.settle(p)
    assert p.checked.ok and p.checked.doc["jobs"]["comp-main"]["after"] == "comp-bg"


def test_frames_follow_each_write_until_typed(tmp_path, farm):
    p, n = opened(tmp_path, farm)
    nk.type_into(p, "c4_frames", "1001-1010")
    assert p.draft.frames == "1001-1010" and value(p, "c4_frames_linked") is False
    nk.press(p, "c4_check")
    nk.settle(p)
    assert {j["frames"] for j in p.checked.doc["jobs"].values()} == {"1001-1010"}
    nk.type_into(p, "c4_frames_linked", True)
    assert p.draft.frames is None and value(p, "c4_frames") == "1001-1048"
    n["Write_main"]["last"].setValue(1020)
    p._refresh_scene()
    assert value(p, "c4_frames") == "each Write node's own range"


def test_needed_by_a_typed_time(tmp_path, farm):
    p, _ = opened(tmp_path, farm)
    assert not p.knobs()["c4_due_time"].enabled()
    nk.type_into(p, "c4_due_time", "fri 18:00")
    assert value(p, "c4_due") == TYPED and p.draft.due == "fri 18:00"
    assert p.knobs()["c4_due_time"].enabled()
    nk.type_into(p, "c4_due", SUGGESTED)
    assert p.draft.due == "suggested"
    nk.press(p, "c4_check")
    nk.settle(p)
    assert "due" not in p.checked.doc


def test_parameter_knobs_edit_the_form(tmp_path, farm):
    p, _ = opened(tmp_path, farm)
    nk.type_into(p, "p_threads", 8)
    nk.type_into(p, "p_write", "Write_x")
    assert value(p, "p_write__linked") is False and p.draft.form.field("write").value == "Write_x"
    nk.type_into(p, "p_write__linked", True)
    assert value(p, "p_write") == EACH_WRITE
    nk.press(p, "c4_check")
    nk.settle(p)
    w = p.checked.doc["jobs"]["comp-bg"]["with"]
    assert w["threads"] == 8 and w["write"] == "Write_bg"


def test_size_shows_for_a_tool_with_sizes(tmp_path):
    heavy = copy.deepcopy(NUKE_COMP)
    heavy.update(name="nuke-comp-big", title="Nuke comp, big plates")
    heavy["needs"]["sizes"] = {"heavy": {"memory": "64G"}}
    farm = FakeFarm(tools=[NUKE_COMP, heavy, KARMA], projects={"saltmarsh": dict(SALTMARSH, root="/show")})
    p, _ = opened(tmp_path, farm)
    assert p.knobs()["c4_tool"].values() == ["Nuke comp", "Nuke comp, big plates"]
    nk.type_into(p, "c4_note", "kept across tools")
    nk.type_into(p, "c4_tool", "Nuke comp, big plates")
    assert p.draft.tool.name == "nuke-comp-big" and p.draft.note == "kept across tools"
    order = p.order()
    assert order.index("c4_size") == order.index("c4_note") - 1
    assert p.knobs()["c4_size"].values() == ["Standard", "heavy"]
    nk.type_into(p, "c4_size", "heavy")
    nk.press(p, "c4_check")
    nk.settle(p)
    assert p.checked.ok and {j["size"] for j in p.checked.doc["jobs"].values()} == {"heavy"}


def test_cancel_a_check(tmp_path, farm):
    p, _ = opened(tmp_path, farm)
    nk.state().cancel = True
    nk.press(p, "c4_check")
    nk.settle(p)
    assert p.checked is None and value(p, "c4_summary") == "Check cancelled."
    assert p.knobs()["c4_check"].enabled() and not p.knobs()["c4_submit"].enabled()


def test_a_farm_without_tools_yet(tmp_path):
    p, _ = opened(tmp_path, FakeFarm(serves_submissions=False))
    assert p.draft is None
    assert "does not serve tools yet" in value(p, "c4_summary")
    assert "C4_TOOL_PATH" in value(p, "c4_summary"), "it says how to get a form anyway"
    assert not p.knobs()["c4_check"].enabled()


def test_tools_from_c4_tool_path_on_a_farm_without_them(tmp_path, monkeypatch):
    tools = tmp_path / "tools" / "nuke-comp"
    tools.mkdir(parents=True)
    (tools / "tool.yaml").write_text(json.dumps(NUKE_COMP))
    monkeypatch.setenv("C4_TOOL_PATH", str(tmp_path / "tools"))
    p, _ = opened(tmp_path / "show", FakeFarm(serves_submissions=False))
    assert p.knobs()["c4_tool"].values() == ["Nuke comp"] and "p_write" in p.knobs()
    nk.press(p, "c4_check")
    nk.settle(p)
    c = p.checked
    assert c.errors == [] and not c.farm_checked and not p.knobs()["c4_submit"].enabled()
    text = value(p, "c4_summary")
    assert text.startswith("Worth knowing:\n  this farm does not take submissions yet")
    assert "Save file keeps it" in text
    path = p.save_file(str(tmp_path / "sm0140.yaml"))
    assert path and yamlio.load(open(path).read()) == c.doc


def test_no_farm_address(tmp_path):
    host, _ = comp_script(tmp_path)
    p = panel.open_panel(host=host)
    nk.settle(p)
    assert value(p, "c4_summary").startswith("the farm's address is not set: set C4_HUD")


def test_another_project(tmp_path, farm):
    farm.projects["heron"] = {"name": "heron", "milestones": []}
    p, _ = opened(tmp_path, farm)
    nk.type_into(p, "c4_project", "heron")
    nk.settle(p)
    assert p.draft.project == "heron"
    assert p.knobs()["c4_due"].values() == [SUGGESTED, TYPED]


def test_a_write_added_after_opening_is_drawn_at_check(tmp_path, farm):
    p, n = opened(tmp_path, farm)
    nk.write("Write_extra", os.path.join(str(tmp_path), "comp", "out", "extra.####.exr"), n["Grade1"])
    nk.press(p, "c4_check")
    assert "c4_write_2" not in p.knobs(), "not while Check's own callback runs"
    nk.settle(p)
    assert p.knobs()["c4_write_2"].label().startswith("Write_extra") and value(p, "c4_write_2") is True
    assert list(p.checked.doc["jobs"]) == ["comp-bg", "comp-main", "comp-extra"]
    assert p.knobs()["c4_submit"].enabled()
