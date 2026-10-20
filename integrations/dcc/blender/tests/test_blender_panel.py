"""The panel, driven the way an artist drives it, against the fake farm."""

import copy
import json
import os
import re
import time

import fakebpy as bf
import pytest
from blender_scenes import BLENDER_RENDER, LIGHTING_FILES, env, farm, lighting
from c4submit import yamlio
from c4submit.collect import Cancelled
from c4submit.form import Form
from c4submit.submitter import Submitter
from c4submit.testing import KARMA, NUKE_COMP, FakeFarm
from c4submit.tool import Tool

import c4_blender
from c4_blender import panel
from c4_blender.panel import EACH_LAYER, STALE, TYPED, form_specs, widget_kind

# Words an artist never reads in this panel. "image" is Blender's own word
# for a texture, so the scene's data-block names are left out of the search:
# what is searched is what the panel and the farm say.
BANNED = re.compile(r"\b(image|images|pod|pods|namespace|namespaces|priority|priorities|kubernetes|k8s|"
                    r"container|containers|kubectl|cluster|deployment)\b", re.I)


def opened(tmp_path, monkeypatch, modified=False, fake=None):
    show, data = lighting(tmp_path, modified=modified)
    env(monkeypatch, show)
    fake = fake if fake is not None else farm()
    monkeypatch.setattr(panel, "farm_factory", lambda: fake)
    bf.draw(panel.C4_PT_farm)   # the first draw makes the scene's panel
    bf.settle()
    return show, ctl(), fake


def ctl():
    return panel._controllers[bf.context.scene.as_pointer()]


def settings():
    return bf.context.scene.c4_farm


def field(name):
    return settings().fields[name]


def layer(name):
    return settings().layers[name]


def ops():
    return bf.bpy.ops.c4


def drawn():
    out = []
    for e in bf.walk(bf.draw(panel.C4_PT_farm)):
        if e.kind == "prop":
            d = e.data
            if isinstance(d, panel.C4_PG_settings):
                out.append(e.attr)
            elif isinstance(d, panel.C4_PG_layer):
                out.append("layer:" + d.name)
            else:
                out.append(f"{d.name}.{e.attr}")
        elif e.kind == "operator":
            out.append(e.idname)
        else:
            out.append("label:" + e.text)
    return out


def entry(key):
    for e in bf.walk(bf.draw(panel.C4_PT_farm)):
        if e.kind == "operator" and e.idname == key:
            return e
        if e.kind == "prop" and (e.attr == key or f"{getattr(e.data, 'name', '')}.{e.attr}" == key):
            return e
    raise KeyError(key)


def check(invoke=True):
    r = ops().check("INVOKE_DEFAULT" if invoke else "EXEC_DEFAULT")
    bf.settle()
    return r


# --- widget specs -----------------------------------------------------------------------

def test_widget_specs_from_the_blender_render_tool():
    artist, advanced = form_specs(Form(Tool({"tool": BLENDER_RENDER})))
    assert [(s.kind, s.param, s.label, s.attr, s.linked) for s in artist] == [
        ("path", "blend", "Blender file", "path_value", True),
        ("text", "view_layer", "View layer", "string_value", True),
        ("menu", "engine", "Render engine", "choice", None),
    ], "fields taken from the file start linked; engine has no link toggle"
    assert [(s.kind, s.param, s.label) for s in advanced] == [
        ("text", "camera", "Camera"), ("int", "samples", "Samples (0 keeps the file's)"), ("bool", "denoise", "Denoise")]
    by = {s.param: s for s in artist + advanced}
    assert by["view_layer"].value == "Scene/ViewLayer", "the tool's default until the file says"
    assert by["engine"].options == ["CYCLES", "BLENDER_EEVEE_NEXT", "BLENDER_WORKBENCH"]
    assert by["engine"].value == "CYCLES" and by["engine"].value_attr == "string_value"
    assert by["samples"].value == 0 and by["denoise"].value is True
    assert "Linked: follows the file" in by["view_layer"].tooltip


def test_widget_specs_for_every_control():
    tool = Tool({"name": "t", "kind": "once", "params": [
        {"name": "engine", "type": "STRING", "default": "B", "allowedValues": ["A", "B"],
         "userInterface": {"control": "DROPDOWN_LIST", "groupLabel": "Render"}},
        {"name": "notes", "type": "STRING", "userInterface": {"control": "MULTILINE_EDIT", "groupLabel": "Render"}},
        {"name": "gain", "type": "FLOAT", "default": 1.5, "minValue": 0.0, "maxValue": 4.0},
        {"name": "outdir", "type": "PATH", "objectType": "DIRECTORY"},
        {"name": "movie", "type": "PATH", "dataFlow": "OUT"},
        {"name": "tags", "type": "LIST[STRING]", "default": ["a", "b"]},
        {"name": "slate", "type": "BOOL", "default": True, "show": "advanced", "userInterface": {"groupLabel": "Render"}},
        {"name": "secret", "type": "STRING", "show": "wrangler"},
    ]})
    artist, advanced = form_specs(Form(tool))
    assert [(s.kind, s.param or s.label) for s in artist] == [
        ("heading", "Render"), ("menu", "engine"), ("multiline", "notes"), ("float", "gain"),
        ("folder", "outdir"), ("path", "movie"), ("text", "tags")], "wrangler fields are not drawn"
    assert [(s.kind, s.param or s.label) for s in advanced] == [("heading", "Render"), ("bool", "slate")]
    by = {s.param: s for s in artist}
    assert by["engine"].value == "B" and by["gain"].value == 1.5 and by["tags"].value == "a, b"
    assert by["outdir"].attr == "folder_value"
    assert widget_kind(Form(Tool({"tool": KARMA})).field("renderer")) == "menu"


# --- the whole flow ------------------------------------------------------------------------

def test_open_check_submit(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch, modified=True)
    order = drawn()
    top = order[:order.index("c4.save_file") + 1]
    assert top == ["tool", "project", "shot", "due", "frames", "frames_linked",
                   "label:View layers", "c4.refresh", "layer:sh010/bg", "layer:sh010/fg", "layer:sh010_comp/comp",
                   "size", "note",
                   "blend.path_value", "blend.linked", "view_layer.string_value", "view_layer.linked", "engine.choice",
                   "label:Advanced", "c4.check", "c4.submit", "c4.show_file", "c4.save_file"]
    s = settings()
    assert [t.name for t in c.tools] == ["blender-render"], "only tools that render .blend files"
    assert s.tool == "blender-render" and s.project == "saltmarsh" and s.shot == "sh010"
    assert [i[1] for i in panel._due_items(s, bf.context)] == [
        "Dailies, Thu 18:00", "Client review, Fri 10:00", "When the farm suggests", TYPED]
    assert s.due == "suggested"
    assert s.frames == "1001-1048" and s.frames_linked is True
    assert layer("sh010/bg").ticked and layer("sh010/fg").ticked and not layer("sh010_comp/comp").ticked
    assert layer("sh010/bg").label == "bg  1001-1048"
    assert layer("sh010_comp/comp").label == "sh010_comp / comp  1001-1048  (scene sh010_comp · after bg, fg)"
    assert field("view_layer").string_value == EACH_LAYER and field("view_layer").linked
    assert field("blend").path_value.endswith("sh010_lighting_v003.blend") and field("blend").linked
    assert not entry("c4.submit").enabled and not entry("c4.show_file").enabled, "nothing checked yet"
    with pytest.raises(RuntimeError, match="poll"):
        ops().submit()

    s.due = "dailies"
    assert c.draft.due == "dailies"
    assert ops().check("INVOKE_DEFAULT") == {"RUNNING_MODAL"}
    assert c.busy and not entry("c4.check").enabled and entry("c4.cancel").enabled, "one check at a time"
    bf.settle()
    assert bf.state.saves == 1, "the farm reads the saved file, so Check saves it"
    assert c.checked.ok, c.checked.errors
    text = c.summary_text()
    assert "sh010-render-bg-v001   Blender render · 48 frames · 12 units" in text
    assert "sh010-render-fg-v001   Blender render · 48 frames · 12 units" in text
    assert re.search(r"estimate\s+ready", text) and "dailies · slack" in text
    assert f"{LIGHTING_FILES} found from sh010_lighting_v003.blend" in text and f"{LIGHTING_FILES} to upload" in text
    assert text.endswith("Ready: press Submit to send it to the farm.")
    assert fake.calls[-1][0] == "check" and not fake.store
    assert ("begin", 0, 100) in bf.state.progress and bf.state.progress[-1] == ("end",)
    assert not bf.state.modal and not bf.state.event_timers, "the modal check ended with the work"
    files = sorted(c.checked.collection.files)
    assert "assets/tex/wood.1011.exr" in files and "fx/smoke/smoke_0048.png" in files
    assert "fx/smoke/smoke_0049.png" not in files and "fx/vdb/explosion_0011.vdb" not in files

    assert entry("c4.submit").enabled
    assert ops().submit("INVOKE_DEFAULT") == {"RUNNING_MODAL"}
    bf.settle()
    assert len(fake.jobs) == 1
    jobs = fake.jobs[0]["jobs"]
    assert list(jobs) == ["render-bg", "render-fg"]
    assert jobs["render-bg"] == {"tool": "blender-render", "frames": "1001-1048", "with": {
        "blend": "sh010/lighting/sh010_lighting_v003.blend", "view_layer": "sh010/bg", "camera": "CAM_sh010"}}
    assert jobs["render-fg"]["with"]["view_layer"] == "sh010/fg" and "after" not in jobs["render-fg"]
    assert fake.jobs[0]["due"] == "dailies" and len(fake.store) == LIGHTING_FILES
    text = c.summary_text()
    assert text.startswith("Submitted to the farm:\n  sh010-render-bg-v001\n  sh010-render-fg-v001")
    assert f"{LIGHTING_FILES} files sent" in text
    assert not entry("c4.submit").enabled, "a check submits once"


def test_a_scene_that_reads_another_comes_after_it(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch)
    layer("sh010_comp/comp").ticked = True
    assert c.draft.ticked == ["sh010/bg", "sh010/fg", "sh010_comp/comp"]
    check()
    jobs = c.checked.doc["jobs"]
    assert list(jobs) == ["render-bg", "render-fg", "render-comp"]
    assert jobs["render-comp"]["after"] == ["render-bg", "render-fg"]
    assert jobs["render-comp"]["with"]["view_layer"] == "sh010_comp/comp"
    assert c.checked.ok and len(c.checked.collection.files) == LIGHTING_FILES, "sh010's render is made, not sent"

    layer("sh010/bg").ticked = False
    layer("sh010/fg").ticked = False
    check()
    assert not c.checked.ok
    assert any(e["path"] == "sh010_render" and "sh010_####.exr" in e["message"] for e in c.checked.errors)
    text = c.summary_text()
    assert text.startswith("To fix before submitting:\n  sh010_render: ") and text.endswith("Check again.")
    assert "\n      fix the path, or render or copy the files first" in text
    assert "after" not in c.checked.doc["jobs"]["render"]


def test_save_file_writes_the_submission(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch)
    settings().note = "lighting v3: new key"
    assert not entry("c4.save_file").enabled, "a file is what a check checked"
    check()
    assert ops().save_file("INVOKE_DEFAULT") == {"RUNNING_MODAL"}
    browser = bf.state.fileselect
    assert browser.filepath == os.path.join(show, "sh010", "lighting", "sh010-render-bg.yaml")
    os.makedirs(str(tmp_path / "out"))
    assert bf.choose_file(str(tmp_path / "out" / "sh010-lighting")) == {"FINISHED"}
    path = str(tmp_path / "out" / "sh010-lighting.yaml")
    with open(path) as f:
        text = f.read()
    assert text.startswith("# Saved from blender by c4submit")
    doc = yamlio.load(text)
    assert doc == c.checked.doc and doc["note"] == "lighting v3: new key"
    assert c.summary_text().startswith(f"Saved {path}.\nSubmit it later with: c4 farm submit sh010-lighting.yaml")
    assert ("INFO", f"Saved {path}") in bf.state.reports
    assert not fake.jobs, "saving submits nothing"

    assert ops().show_file() == {"FINISHED"}
    assert c.summary_text() == c.checked.yaml().rstrip("\n")
    assert "c4.copy_file" in drawn() and entry("c4.show_file").text == "Show summary"
    ops().copy_file()
    assert bf.context.window_manager.clipboard == c.checked.yaml()
    ops().show_file()
    assert c.summary_text().startswith("Saved ")


def test_submit_is_refused_after_a_change(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch)
    check()
    assert entry("c4.submit").enabled

    settings().shot = "sh020"
    assert not entry("c4.submit").enabled
    assert c.summary_text().startswith(STALE)
    assert c.submit() is False and not fake.jobs
    assert c.save_file(str(tmp_path / "x.yaml")) is None, "nor saved: the file would not be what was checked"
    assert ops().save_file("INVOKE_DEFAULT") == {"CANCELLED"} and bf.state.fileselect is None

    settings().shot = "sh010"
    assert entry("c4.submit").enabled, "back as it was checked"

    bf.data.is_dirty = True     # the artist moves a light
    assert not entry("c4.submit").enabled and c.submit() is False
    bf.data.is_dirty = False
    layer("sh010/fg").ticked = False
    assert not entry("c4.submit").enabled
    layer("sh010/fg").ticked = True
    assert entry("c4.submit").enabled

    check()
    assert ops().submit() == {"FINISHED"}
    bf.settle()
    assert len(fake.jobs) == 1


def test_esc_cancels_a_check(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch)

    class Slow(Submitter):
        def check(self, prepared, explain=False):
            self._tick("hash", 3, 72)
            while not self.cancelled():
                time.sleep(0.002)
            raise Cancelled()

    monkeypatch.setattr(panel, "Submitter", Slow)
    ops().check("INVOKE_DEFAULT")
    for _ in range(50):
        bf.run_timers()
        if "Reading files: 3 of 72" in c.summary_text():
            break
        time.sleep(0.01)
    assert c.summary_text() == "Checking with the farm...\nReading files: 3 of 72"
    assert ("update", 4) in bf.state.progress
    bf.send("ESC")
    bf.settle()
    assert c.checked is None and c.summary_text() == "Check cancelled."
    assert entry("c4.check").enabled and not entry("c4.submit").enabled and "c4.cancel" not in drawn()

    ops().check("INVOKE_DEFAULT")
    assert ops().cancel() == {"FINISHED"}, "the Cancel button does the same"
    bf.settle()
    assert c.summary_text() == "Check cancelled."


def test_frames_follow_each_layer_until_typed(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch)
    settings().frames = "1001-1010"
    assert c.draft.frames == "1001-1010" and settings().frames_linked is False
    check()
    assert {j["frames"] for j in c.checked.doc["jobs"].values()} == {"1001-1010"}
    settings().frames_linked = True
    assert c.draft.frames is None and settings().frames == "1001-1048"
    layer("sh010_comp/comp").ticked = True
    bf.data.scenes["sh010_comp"].frame_end = 1020
    c.refresh_scene()
    assert settings().frames == "each view layer's own range"


def test_needed_by_a_typed_time(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch)
    assert "due_time" not in drawn()
    settings().due_time = "fri 18:00"
    assert settings().due == "typed" and c.draft.due == "fri 18:00"
    assert "due_time" in drawn()
    settings().due = "suggested"
    assert c.draft.due == "suggested"
    check()
    assert "due" not in c.checked.doc


def test_parameter_widgets_edit_the_form(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch)
    assert "samples.int_value" not in drawn(), "advanced fields are folded away"
    bf.state.panels["C4_PT_farm_advanced"] = True
    order = drawn()
    assert order[order.index("label:Advanced") + 1:order.index("c4.check")] == [
        "camera.string_value", "camera.linked", "samples.int_value", "denoise.bool_value"]
    assert field("camera").string_value == "CAM_sh010" and field("camera").linked

    field("samples").int_value = 64
    field("engine").choice = "BLENDER_EEVEE_NEXT"
    assert field("engine").string_value == "BLENDER_EEVEE_NEXT"
    field("view_layer").string_value = "sh010/bg"
    assert not field("view_layer").linked and c.draft.form.field("view_layer").value == "sh010/bg"
    field("view_layer").linked = True
    assert field("view_layer").string_value == EACH_LAYER
    field("samples").int_value = 70000
    assert field("samples").int_value == 70000 and c.draft.form.field("samples").error == "must be at most 65536"
    field("samples").int_value = 64
    check()
    w = c.checked.doc["jobs"]["render-bg"]["with"]
    assert w["samples"] == 64 and w["engine"] == "BLENDER_EEVEE_NEXT" and w["view_layer"] == "sh010/bg"
    assert c.checked.doc["jobs"]["render-fg"]["with"]["view_layer"] == "sh010/fg"


def test_size_and_another_tool(tmp_path, monkeypatch):
    plain = copy.deepcopy(BLENDER_RENDER)
    plain.update(name="blender-quick", title="Blender quick render")
    plain["needs"] = {"cpu": 4}
    fake = farm([BLENDER_RENDER, plain, KARMA])
    show, c, _ = opened(tmp_path, monkeypatch, fake=fake)
    assert [i[1] for i in panel._tool_items(settings(), bf.context)] == ["Blender render", "Blender quick render"]
    assert [i[1] for i in panel._size_items(settings(), bf.context)] == ["Standard", "heavy"]
    settings().size = "heavy"
    settings().note = "kept across tools"
    check()
    assert {j["size"] for j in c.checked.doc["jobs"].values()} == {"heavy"}
    settings().tool = "blender-quick"
    assert c.draft.tool.name == "blender-quick" and c.draft.note == "kept across tools"
    assert c.checked is None and "size" not in drawn(), "this tool has no sizes"
    assert settings().size_value == ""


def test_choices_are_kept_with_the_file(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch)
    s = settings()
    layer("sh010/fg").ticked = False
    s.note = "for review"
    s.due = "client-review"
    field("samples").int_value = 128
    field("view_layer").string_value = "sh010/bg"
    field("view_layer").linked = True
    field("camera").linked = False
    panel.forget()                 # the file is closed and opened again
    bf.draw(panel.C4_PT_farm)
    bf.settle()
    d = ctl().draft
    assert d is not c.draft
    assert d.ticked == ["sh010/bg"] and d.note == "for review" and d.due == "client-review"
    assert d.form.field("samples").value == 128
    assert d.form.field("view_layer").linked and not d.form.field("camera").linked
    assert d.form.field("camera").value == "CAM_sh010"


def test_a_layer_added_after_opening_is_drawn(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch)
    bf.data.scenes["sh010"].layer("fx")
    assert "layer:sh010/fx" not in drawn(), "a draw may not write: it asks a timer to"
    bf.settle()
    assert "layer:sh010/fx" in drawn() and layer("sh010/fx").ticked
    check()
    assert list(c.checked.doc["jobs"]) == ["render-bg", "render-fg", "render-fx"]


def test_a_file_never_saved(tmp_path, monkeypatch):
    env(monkeypatch, str(tmp_path))
    bf.reset("").scenes[0].layer("ViewLayer")
    bf.data.is_dirty = True
    monkeypatch.setattr(panel, "farm_factory", lambda: farm())
    bf.draw(panel.C4_PT_farm)
    bf.settle()
    c = ctl()
    assert c.draft.tool.name == "blender-render", "an untitled file is a .blend"
    check()
    assert any("could not save the file: the file has never been saved" in e["message"] for e in c.checked.errors)
    assert not entry("c4.submit").enabled


def test_a_farm_without_tools_yet(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch, fake=FakeFarm(serves_submissions=False))
    assert c.draft is None
    assert "does not serve tools yet" in c.summary_text() and "C4_TOOL_PATH" in c.summary_text()
    assert "c4.check" not in drawn() and "c4.reload" in drawn()


def test_tools_from_c4_tool_path(tmp_path, monkeypatch):
    tools = tmp_path / "tools" / "blender-render"
    tools.mkdir(parents=True)
    (tools / "tool.yaml").write_text(json.dumps(BLENDER_RENDER))
    monkeypatch.setenv("C4_TOOL_PATH", str(tmp_path / "tools"))
    show, c, fake = opened(tmp_path / "show", monkeypatch, fake=FakeFarm(serves_submissions=False))
    assert settings().tool == "blender-render" and "view_layer.string_value" in drawn()
    check()
    ch = c.checked
    assert ch.errors == [] and not ch.farm_checked and not entry("c4.submit").enabled
    text = c.summary_text()
    assert text.startswith("Worth knowing:\n  this farm does not take submissions yet")
    assert "Save file keeps it" in text
    path = c.save_file(str(tmp_path / "sh010.yaml"))
    assert path and yamlio.load(open(path).read()) == ch.doc


def test_no_farm_address(tmp_path, monkeypatch):
    show, data = lighting(tmp_path)
    env(monkeypatch, show)
    bf.draw(panel.C4_PT_farm)
    bf.settle()
    assert ctl().summary_text().startswith("the farm's address is not set: set C4_HUD")


def test_another_project(tmp_path, monkeypatch):
    fake = farm()
    fake.projects["heron"] = {"name": "heron", "milestones": []}
    show, c, _ = opened(tmp_path, monkeypatch, fake=fake)
    settings().due = "dailies"
    settings().project = "heron"
    bf.settle()
    assert c.draft.project == "heron"
    assert [i[0] for i in panel._due_items(settings(), bf.context)] == ["suggested", "typed"]
    assert settings().due == "suggested" and c.draft.due == "suggested", "heron has no dailies"


def test_register_and_unregister():
    assert hasattr(bf.bpy.types.Scene, "c4_farm") and "c4.check" in bf.state.operators
    assert panel._on_load in bf.bpy.app.handlers.load_post
    c4_blender.unregister()
    assert not hasattr(bf.bpy.types.Scene, "c4_farm") and "c4.check" not in bf.state.operators
    assert panel._on_load not in bf.bpy.app.handlers.load_post
    c4_blender.register()
    assert c4_blender.bl_info["blender"] == (4, 2, 0)


def test_opening_another_file_drops_the_panel(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch)
    for h in bf.bpy.app.handlers.load_post:
        h(None)
    assert panel._controllers == {}


def test_no_farm_words_anywhere_the_artist_reads(tmp_path, monkeypatch):
    show, c, fake = opened(tmp_path, monkeypatch, modified=True)
    seen = []

    def look():
        for e in bf.walk(bf.draw(panel.C4_PT_farm)):
            seen.append(e.text)
        seen.append(c.summary_text())

    bf.state.panels["C4_PT_farm_advanced"] = True
    look()
    layer("sh010_comp/comp").ticked = True
    check()
    look()
    ops().show_file()
    look()
    ops().show_file()
    ops().submit()
    bf.settle()
    look()
    layer("sh010/bg").ticked = False
    layer("sh010/fg").ticked = False
    check()                          # a check with errors in it
    look()
    s = settings()
    for fn in (panel._tool_items, panel._due_items, panel._size_items):
        seen += [t[1] + " " + t[2] for t in fn(s, bf.context)]
    for cls in panel.CLASSES:
        seen += [getattr(cls, "bl_label", ""), getattr(cls, "bl_description", "")]
        for p in vars(cls).values():
            if isinstance(p, bf.Prop):
                seen += [p.kw.get("name", ""), p.kw.get("description", "")]
    seen += [m for _, m in bf.state.reports]
    seen += [str(v) for v in c4_blender.bl_info.values()]
    seen += [panel.LOCAL_TOOLS, STALE, panel.EACH_RANGE, EACH_LAYER] + list(panel.PROGRESS_WORDS.values())
    names = sorted(bf.data.names(), key=len, reverse=True)
    bad = set()
    for text in seen:
        for n in names:
            text = text.replace(n, "")
        bad |= {m.group(0) for m in BANNED.finditer(text)}
    assert bad == set()
    assert "reference image" in bf.data.names(), "the search leaves out the file's own names, not the panel's words"
