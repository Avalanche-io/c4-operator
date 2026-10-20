"""The parameter interface drawn from a tool, and the node's callbacks."""

import json
import os

import fakehou as hou
import pytest
from houdini_scenes import KARMA_PLUS, c4render, env, farm, light_scene

from c4submit.submitter import Draft
from c4submit.testing import KARMA, FakeHost
from c4submit.tool import Tool
from c4_houdini import nodes

HERE = os.path.dirname(os.path.abspath(__file__))
WORDS = ("image", "pod", "namespace", "priority", "kubernetes", "container", "gpu request")


def karma_draft(tool=KARMA_PLUS):
    host = FakeHost(scene="/show/sm0220/light/usd/sm0220.usd", frames="1001-1048")
    return Draft(host, Tool(tool))


def by_name(group):
    return {t.name(): t for t in group.leaves()}


def test_header_is_the_same_for_every_tool():
    lop = [t.name() for t in nodes.header("lop")]
    rop = [t.name() for t in nodes.header("rop")]
    assert lop == ["c4_tool", "c4_project", "c4_shot", "c4_due", "c4_usdfile", "c4_f", "c4_frames", "c4_frames_link",
                   "c4_renderables", "c4_hassizes", "c4_size", "c4_note"]
    assert rop == [n for n in lop if n not in ("c4_usdfile", "c4_f")]
    h = {t.name(): t for t in nodes.header("lop")}
    assert h["c4_due"].label() == "Needed by" and h["c4_due"].menuType() == hou.menuType.StringReplace
    assert h["c4_due"].defaultValue() == ("suggested",)
    assert h["c4_renderables"].label() == "Render settings" and h["c4_renderables"].menuType() == hou.menuType.StringToggle
    assert {t.name(): t for t in nodes.header("rop")}["c4_renderables"].label() == "ROPs"
    assert h["c4_frames"].joinsWithNext() and h["c4_frames"].disableWhen() == "{ c4_frames_link == 1 }"
    assert h["c4_size"].conditionals()[hou.parmCondType.HideWhen] == "{ c4_hassizes == 0 }"
    assert h["c4_f"].defaultExpression() == ("$FSTART", "$FEND", "1")
    assert h["c4_usdfile"].defaultValue() == ('$HIP/usd/`chs("c4_shot")`_$OS.usd',)
    assert h["c4_usdfile"].tags()["filechooser_mode"] == "write", "the stage file is written, so it is never collected"


def test_tool_folder_drawn_from_karma():
    d = karma_draft()
    g = nodes.group("lop", d)
    names = [t.name() for t in g.entries()]
    assert names.index(nodes.TOOL_FOLDER) == names.index(nodes.ACTIONS) - 1, "the Tool folder sits above the buttons"
    folder = g.find(nodes.TOOL_FOLDER)
    assert folder.label() == "Tool"
    top = [t.name() for t in folder.parmTemplates()]
    assert top == ["c4p_stage", "c4p_stage_link", "c4p_settings", "c4p_settings_link", "c4p_renderer", "c4g_quality",
                   "c4g_advanced"]
    t = by_name(g)
    # derived fields: a Linked toggle beside them that disables them
    assert t["c4p_stage"].stringType() == hou.stringParmType.FileReference
    assert t["c4p_stage"].tags() == {"filechooser_mode": "read", "filechooser_pattern": "*.usd *.usda *.usdc"}
    assert t["c4p_stage"].joinsWithNext() and t["c4p_stage"].disableWhen() == "{ c4p_stage_link == 1 }"
    assert t["c4p_stage_link"].type() == hou.parmTemplateType.Toggle and t["c4p_stage_link"].defaultValue() == (True,)
    assert t["c4p_settings"].label() == "Render settings" and t["c4p_settings"].defaultValue() == ("/Render/rendersettings",)
    # a menu
    assert t["c4p_renderer"].label() == "Engine" and not t["c4p_renderer"].joinsWithNext()
    assert t["c4p_renderer"].menuItems() == ("BRAY_HdKarmaXPU", "BRAY_HdKarma")
    assert "c4p_renderer_link" not in t
    # groupLabel is a folder; advanced fields are folded
    quality = g.find("c4g_quality")
    assert quality.label() == "Quality" and quality.folderType() == hou.folderType.Simple
    assert t["c4p_denoise"].type() == hou.parmTemplateType.Toggle and t["c4p_denoise"].defaultValue() == (True,)
    assert t["c4p_samples"].type() == hou.parmTemplateType.Int
    assert (t["c4p_samples"].minValue(), t["c4p_samples"].maxValue(), t["c4p_samples"].defaultValue()) == (1, 4096, (64,))
    adv = g.find("c4g_advanced")
    assert adv.label() == "Advanced" and adv.folderType() == hou.folderType.Collapsible
    assert [x.name() for x in adv.parmTemplates()] == ["c4p_threads", "c4p_mplay", "c4p_checkpoint"]
    assert t["c4p_checkpoint"].fileType() == hou.fileType.Directory
    assert t["c4p_checkpoint"].tags()["filechooser_mode"] == "write"
    assert "c4p_secret" not in t, "wrangler fields are the HUD's"
    # every parameter calls back into c4_houdini
    for name in ("c4p_stage", "c4p_stage_link", "c4p_renderer", "c4p_samples", "c4_check", "c4_tool"):
        assert t[name].scriptCallback() == nodes.CALLBACK
        assert t[name].scriptCallbackLanguage() == hou.scriptLanguage.Python


def test_actions_and_summary():
    t = by_name(nodes.static_group("rop"))
    assert [t[n].label() for n in ("c4_check", "c4_submit", "c4_show", "c4_save")] == [
        "Check", "Submit", "Show file", "Save file"]
    assert t["c4_summary"].tags()["editor"] == "1" and not t["c4_summary"].scriptCallback()


def test_no_farm_words_in_the_interface():
    for kind in ("lop", "rop"):
        for t in nodes.group(kind, karma_draft()).leaves():
            text = f"{t.label()} {t.help()}".lower()
            assert not [w for w in WORDS if w in text], (t.name(), text)


def test_install_draws_the_tool_and_menus(tmp_path, monkeypatch):
    show, lop = light_scene(tmp_path)
    env(monkeypatch, show, "sm0220")
    fake = farm()
    monkeypatch.setattr(nodes, "farm_factory", lambda: fake)
    node = c4render(lop)
    nodes.install(node)
    assert node.parm("c4_tool").eval() == "karma", "karma renders .usd stages; houdini-rop does not"
    assert node.parm("c4_project").eval() == "saltmarsh" and node.parm("c4_shot").eval() == "sm0220"
    assert node.parm("c4_usdfile").eval() == os.path.join(show, "sm0220", "light", "usd", "sm0220_c4render1.usd")
    assert node.parm("c4p_stage").eval() == node.parm("c4_usdfile").eval() and node.parm("c4p_stage_link").eval() == 1
    assert node.parm("c4p_settings").eval() == "/Render/rendersettings"
    assert node.parm("c4_frames").eval() == "1001-1048" and node.parm("c4_frames_link").eval() == 1
    assert node.parm("c4_renderables").eval() == "/Render/rendersettings"
    assert node.parm("c4_hassizes").eval() == 1
    # menus, run as Houdini runs them
    assert hou.run_menu(node.parm("c4_tool")) == ["karma", "Karma render"]
    assert hou.run_menu(node.parm("c4_due")) == ["dailies", "Dailies, Thu 18:00", "client-review",
                                                 "Client review, Fri 10:00", "suggested", "When the farm suggests"]
    assert hou.run_menu(node.parm("c4_renderables")) == ["/Render/beauty_only", "/Render/beauty_only",
                                                         "/Render/rendersettings", "/Render/rendersettings"]
    assert hou.run_menu(node.parm("c4_size")) == ["", "Normal", "heavy", "heavy"]
    assert hou.run_menu(node.parm("c4p_renderer")) == ["BRAY_HdKarmaXPU", "BRAY_HdKarmaXPU", "BRAY_HdKarma", "BRAY_HdKarma"]


def test_link_toggles_and_typed_values(tmp_path, monkeypatch):
    show, lop = light_scene(tmp_path)
    env(monkeypatch, show, "sm0220")
    fake = farm()
    monkeypatch.setattr(nodes, "farm_factory", lambda: fake)
    node = c4render(lop)
    panel = nodes.install(node)
    form = panel.draft.form

    hou.change(node.parm("c4p_settings_link"), 0)
    assert not form.field("settings").linked and form.field("settings").value == "/Render/rendersettings"
    hou.change(node.parm("c4p_settings"), "/Render/beauty_only")
    assert form.field("settings").value == "/Render/beauty_only"
    hou.change(node.parm("c4p_settings_link"), 1)
    assert node.parm("c4p_settings").eval() == "/Render/rendersettings", "relinked, it shows the scene's again"

    hou.change(node.parm("c4p_renderer"), "BRAY_HdKarma")
    assert form.field("renderer").value == "BRAY_HdKarma"
    hou.change(node.parm("c4p_renderer"), "Cycles")
    assert "Engine: must be one of" in node.parm("c4_summary").eval()
    assert node.parm("c4_summary").isLocked(), "the summary is read-only"

    hou.change(node.parm("c4_frames_link"), 0)
    hou.change(node.parm("c4_frames"), "1001-1010")
    assert panel.draft.frames == "1001-1010"
    hou.change(node.parm("c4_renderables"), "/Render/beauty_only /Render/rendersettings")
    assert panel.draft.ticked == ["/Render/beauty_only", "/Render/rendersettings"]
    hou.change(node.parm("c4_due"), "fri 18:00")
    hou.change(node.parm("c4_size"), "heavy")
    hou.change(node.parm("c4_note"), "new key light")
    assert (panel.draft.due, panel.draft.size, panel.draft.note) == ("fri 18:00", "heavy", "new key light")


def test_tool_change_rebuilds_the_tool_folder(tmp_path, monkeypatch):
    show, lop = light_scene(tmp_path)
    env(monkeypatch, show, "sm0220")
    preview = {"c4": "tool/1", "name": "usd-preview", "title": "USD preview", "kind": "frames",
               "params": [{"name": "stage", "type": "PATH", "from": "scene.file",
                           "userInterface": {"fileFilters": [{"patterns": ["*.usd"]}]}},
                          {"name": "res", "type": "STRING", "default": "half", "allowedValues": ["full", "half"]}]}
    fake = farm(tools=[dict(KARMA_PLUS, name="karma"), preview])
    monkeypatch.setattr(nodes, "farm_factory", lambda: fake)
    node = c4render(lop)
    panel = nodes.install(node)
    hou.change(node.parm("c4_due"), "dailies")
    assert node.parm("c4p_samples") is not None
    hou.change(node.parm("c4_tool"), "usd-preview")
    assert panel.draft.tool.name == "usd-preview"
    assert node.parm("c4p_samples") is None and node.parm("c4p_res").eval() == "half"
    assert node.parm("c4_hassizes").eval() == 0 and node.parm("c4_size").eval() == ""
    assert panel.draft.due == "dailies", "the header carries over"
    assert hou.run_menu(node.parm("c4_tool")) == ["karma", "Karma render", "usd-preview", "USD preview"]


def test_reopened_hip_keeps_what_was_typed(tmp_path, monkeypatch):
    show, lop = light_scene(tmp_path)
    env(monkeypatch, show, "sm0220")
    fake = farm(tools=[KARMA_PLUS])
    monkeypatch.setattr(nodes, "farm_factory", lambda: fake)
    node = c4render(lop)
    nodes.install(node)
    hou.change(node.parm("c4p_samples"), 256)
    hou.change(node.parm("c4p_renderer"), "BRAY_HdKarma")
    nodes._panels.clear()   # a new session: the parameters are all there is
    node.parm("c4_note").set("reopened")
    hou.change(node.parm("c4_note"), "reopened")
    panel = nodes.panel_for(node)
    assert panel.draft.form.field("samples").value == 256
    assert panel.draft.form.field("renderer").value == "BRAY_HdKarma"
    assert node.parm("c4p_samples").eval() == 256


def test_without_a_farm_the_summary_says_why(tmp_path, monkeypatch):
    show, lop = light_scene(tmp_path)
    env(monkeypatch, show, "sm0220")
    node = c4render(lop)
    nodes.install(node)   # C4_HUD is not set
    assert node.parm("c4_summary").eval().startswith("C4: the farm's address is not set: set C4_HUD")
    assert node.parm(nodes.PREFIX + "stage") is None
    node.parm("c4_check").pressButton()
    assert "C4_HUD" in node.parm("c4_summary").eval()


def test_install_refuses_other_networks(tmp_path):
    light_scene(tmp_path)
    sop = hou.Node("/obj/geo1/null1", "null", "Sop")
    with pytest.raises(ValueError):
        nodes.install(sop)


def test_shelf_places_a_null_with_the_interface(tmp_path, monkeypatch):
    show, lop = light_scene(tmp_path)
    env(monkeypatch, show, "sm0220")
    fake = farm()
    monkeypatch.setattr(nodes, "farm_factory", lambda: fake)
    lop.setSelected(True, clear_all_selected=True)
    node = nodes.place("lop")
    assert node.path() == "/stage/c4render" and node.inputs() == (lop,)
    assert node.parm("c4_renderables").eval() == "/Render/rendersettings"


def test_package_file():
    with open(os.path.join(HERE, "..", "c4.json"), encoding="utf-8") as f:
        pkg = json.load(f)
    env = {}
    for e in pkg["env"]:
        for k, v in e.items():
            env.setdefault(k, []).append(v)
    assert env["C4_DCC"] == ["$HOUDINI_PACKAGE_PATH/.."]
    pythonpath = [v["value"] for v in env["PYTHONPATH"]]
    assert pythonpath == ["$C4_DCC/houdini/python", "$C4_DCC/c4submit/src"]
    assert all(v["method"] == "prepend" for v in env["PYTHONPATH"])
    assert pkg["hpath"] == "$C4_DCC/houdini"
    # what the paths name exists
    dcc = os.path.normpath(os.path.join(HERE, "..", ".."))
    assert os.path.isdir(os.path.join(dcc, "houdini", "python", "c4_houdini"))
    assert os.path.isdir(os.path.join(dcc, "c4submit", "src", "c4submit"))
    assert os.path.isfile(os.path.join(dcc, "houdini", "toolbar", "c4.shelf"))


def test_hda_sections_call_c4_houdini(tmp_path, monkeypatch):
    from c4_houdini import build_hdas
    assert [a[2] for a in build_hdas.ASSETS] == ["c4::c4render::1.0", "c4::c4submit::1.0"]
    show, lop = light_scene(tmp_path)
    env(monkeypatch, show, "sm0220")
    fake = farm()
    monkeypatch.setattr(nodes, "farm_factory", lambda: fake)
    node = c4render(lop)
    node.setParmTemplateGroup(nodes.static_group("lop"))   # what the HDA's definition holds
    exec(compile(build_hdas.ON_CREATED, "OnCreated", "exec"), {"kwargs": {"node": node}})
    assert node.parm("c4_tool").eval() == "karma" and node.parm("c4p_settings") is not None
    module = {}
    exec(compile(build_hdas.PYTHON_MODULE, "PythonModule", "exec"), module)
    assert module["menu"]({"node": node}, "tool") == ["karma", "Karma render"]
    node.parm("c4_note").set("from the HDA")
    module["changed"]({"node": node, "parm_name": "c4_note"})
    assert nodes.panel_for(node).draft.note == "from the HDA"


def test_typed_text_is_not_expanded(tmp_path, monkeypatch):
    show, lop = light_scene(tmp_path)
    env(monkeypatch, show, "sm0220")
    fake = farm(tools=[dict(KARMA, params=KARMA["params"] + [
        {"name": "cryptomatte", "type": "PATH", "default": "", "userInterface": {"label": "Cryptomatte"}}])])
    monkeypatch.setattr(nodes, "farm_factory", lambda: fake)
    node = c4render(lop)
    panel = nodes.install(node)
    hou.change(node.parm("c4_note"), "fixed $F4 flicker")
    hou.change(node.parm("c4p_cryptomatte"), "$HIP/render/crypto.$F4.exr")
    assert panel.draft.note == "fixed $F4 flicker"
    assert panel.draft.form.field("cryptomatte").value == os.path.join(show, "sm0220", "light", "render", "crypto.$F4.exr")
