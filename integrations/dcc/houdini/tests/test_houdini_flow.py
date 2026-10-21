"""From a node to submitted jobs, against c4submit's fake farm, with real
files on disk: the buttons as an artist presses them."""

import json
import os
import time

import fakehou as hou
from houdini_scenes import c4render, env, farm, fx_scene, light_scene

from c4submit import collect, yamlio
from c4submit.testing import KARMA, SALTMARSH, FakeFarm
from c4_houdini import nodes

SUBMIT, SHOW_FILE, CANCEL = 0, 1, 2


def lighting(tmp_path, monkeypatch, **kw):
    show, lop = light_scene(tmp_path, **kw)
    env(monkeypatch, show, "sm0220")
    fake = farm()
    monkeypatch.setattr(nodes, "farm_factory", lambda: fake)
    node = c4render(lop)
    nodes.install(node)
    return show, node, fake


def fx(tmp_path, monkeypatch, **kw):
    show, submit = fx_scene(tmp_path, **kw)
    env(monkeypatch, show, "sm0140")
    fake = farm()
    monkeypatch.setattr(nodes, "farm_factory", lambda: fake)
    nodes.install(submit)
    return show, submit, fake


def summary(node) -> str:
    # As the parameter pane shows it: eval() would expand the $F4 in a path.
    return node.parm("c4_summary").unexpandedString()


def test_lighting_check_then_submit(tmp_path, monkeypatch):
    show, node, fake = lighting(tmp_path, monkeypatch)
    hou.change(node.parm("c4_due"), "client-review")

    node.parm("c4_check").pressButton()
    text = summary(node)
    assert text.startswith("  files      6 found from sm0220_c4render1.usd"), text
    assert "6 to upload" in text and "\nsm0220-light-v001   Karma render · 48 frames in 48 units\n" in text
    assert "estimate   ready" in text
    assert "due" in text and "Client review, Fri 10:00 · slack" in text
    assert not fake.jobs and not fake.store, "a check submits and uploads nothing"
    assert hou.rendered[-1]["source"] == "/stage/karmarendersettings"
    assert hou.rendered[-1]["frames"] == (1001, 1048, 1)
    files = sorted(nodes.panel_for(node).checked.collection.files)
    assert files == ["assets/set/set.usdc", "assets/tree/tex/bark.1001.exr", "assets/tree/tex/bark.1002.exr",
                     "assets/tree/tex/bark.1011.exr", "lookdev/hdri/sky.exr", "sm0220/light/usd/sm0220_c4render1.usd"]

    hou.ui.answers = [SUBMIT]
    node.parm("c4_submit").pressButton()
    asked = hou.ui.messages[-1]
    assert asked["buttons"] == ("Submit", "Show file", "Cancel") and "slack" in asked["text"]
    (doc,) = fake.jobs
    assert doc["project"] == "saltmarsh" and doc["shot"] == "sm0220" and doc["due"] == "client-review"
    assert doc["jobs"] == {"light": {"tool": "karma", "frames": "1001-1048",
                                     "with": {"stage": "sm0220/light/usd/sm0220_c4render1.usd",
                                              "settings": "/Render/rendersettings"}}}
    assert len(fake.store) == 6
    assert summary(node).startswith("Submitted sm0220-light-v001, 6 files uploaded")
    assert any(u[1] and u[1].startswith("Reading files") for u in hou.InterruptableOperation.updates)


def test_lighting_two_settings_typed_frames_and_size(tmp_path, monkeypatch):
    show, node, fake = lighting(tmp_path, monkeypatch)
    hou.change(node.parm("c4_renderables"), "/Render/rendersettings /Render/beauty_only")
    hou.change(node.parm("c4_frames_link"), 0)
    hou.change(node.parm("c4_frames"), "1001-1010")
    hou.change(node.parm("c4_size"), "heavy")
    hou.change(node.parm("c4p_renderer"), "BRAY_HdKarma")
    hou.ui.answers = [SUBMIT]
    node.parm("c4_submit").pressButton()
    (doc,) = fake.jobs
    assert set(doc["jobs"]) == {"light-beauty-only", "light-rendersettings"}
    for key, job in doc["jobs"].items():
        assert job["frames"] == "1001-1010" and job["size"] == "heavy" and job["with"]["renderer"] == "BRAY_HdKarma"
    assert doc["jobs"]["light-beauty-only"]["with"]["settings"] == "/Render/beauty_only"
    assert hou.rendered[-1]["frames"] == (1001, 1010, 1), "the stage is written over the frames rendered"


def test_rop_jobs_follow_the_rop_graph(tmp_path, monkeypatch):
    show, submit, fake = fx(tmp_path, monkeypatch)
    assert submit.parm("c4_tool").eval() == "houdini-rop", "karma does not render hip files"
    assert submit.parm("c4_renderables").eval() == "/out/mantra1 /out/flip"
    hou.change(submit.parm("c4_renderables"), "/out/sim /out/mantra1 /out/flip")
    hou.change(submit.parm("c4_due"), "dailies")

    submit.parm("c4_check").pressButton()
    panel = nodes.panel_for(submit)
    assert panel.checked.ok, panel.checked.errors
    jobs = panel.checked.doc["jobs"]
    assert list(jobs) == ["render-sim", "render-mantra1", "render-flip"]
    assert jobs["render-mantra1"]["after"] == "render-sim"
    assert "after" not in jobs["render-flip"]
    assert jobs["render-mantra1"]["with"] == {"hip": "sm0140/fx/sm0140_fx_v002.hip", "rop": "/out/mantra1"}
    assert jobs["render-flip"]["frames"] == "1001"
    files = sorted(panel.checked.collection.files)
    assert not [f for f in files if "/cache/" in f or "/render/" in f or "/flip/" in f], \
        "what a submitted ROP writes is not collected, nor read from disk"
    assert len([f for f in files if "/geo/fluid." in f]) == 48, "a $F4 sequence is collected frame by frame"
    assert "sm0140/fx/tex/wood.1001.rat" in files and "sm0140/fx/tex/wood.1002.rat" in files
    assert "assets/env/env.abc" in files and "otls/studio_tools.hda" in files and "sm0140/fx/sm0140_fx_v002.hip" in files
    text = summary(submit)
    assert "after sm0140-render-sim-v001" in text and "Dailies, Thu 18:00 · slack" in text
    assert "bypassed" not in text and "old_version.bgeo.sc: no such file" in text, "a bypassed node's file is a warning"

    hou.ui.answers = [SUBMIT]
    submit.parm("c4_submit").pressButton()
    (doc,) = fake.jobs
    assert list(doc["jobs"]) == ["render-sim", "render-mantra1", "render-flip"]
    assert summary(submit).startswith("Submitted sm0140-render-sim-v001, sm0140-render-mantra1-v001")


def test_an_upstream_cache_left_unticked_is_read_from_disk(tmp_path, monkeypatch):
    show, submit, fake = fx(tmp_path, monkeypatch, cache_on_disk=True)
    submit.parm("c4_check").pressButton()
    panel = nodes.panel_for(submit)
    assert panel.checked.ok, panel.checked.errors
    assert list(panel.checked.doc["jobs"]) == ["render-mantra1", "render-flip"]
    assert len([f for f in panel.checked.collection.files if "/cache/sim." in f]) == 48


def test_a_missing_cache_stops_the_submission(tmp_path, monkeypatch):
    show, submit, fake = fx(tmp_path, monkeypatch)
    hou.ui.answers = [0]
    submit.parm("c4_submit").pressButton()
    assert not fake.jobs
    assert "sim.$F4.bgeo.sc: no such files" in summary(submit)
    assert hou.ui.messages[-1]["text"].startswith("The farm cannot take this yet.")


def test_save_file_writes_the_submission(tmp_path, monkeypatch):
    show, node, fake = lighting(tmp_path, monkeypatch)
    hou.change(node.parm("c4_note"), "key light v4")
    path = str(tmp_path / "saved" / "sm0220.yaml")
    os.makedirs(os.path.dirname(path))
    hou.ui.files = [path]
    node.parm("c4_save").pressButton()
    with open(path, encoding="utf-8") as f:
        text = f.read()
    assert text.startswith("# Saved from houdini by c4submit")
    doc = yamlio.load(text)
    assert doc == nodes.panel_for(node).checked.doc
    assert doc["note"] == "key light v4" and doc["tree"].startswith("c4")
    assert summary(node).endswith(f"Saved to {path}")
    assert not fake.jobs


def test_save_file_default_name_without_ui(tmp_path, monkeypatch):
    show, node, fake = lighting(tmp_path, monkeypatch)
    hou.ui.available = False
    path = nodes.panel_for(node).save_file()
    assert path == os.path.join(show, "sm0220", "light", "sm0220-light.yaml") and os.path.isfile(path)


def test_show_file_then_cancel_submits_nothing(tmp_path, monkeypatch):
    show, node, fake = lighting(tmp_path, monkeypatch)
    hou.ui.answers = [SHOW_FILE, 0, CANCEL]
    node.parm("c4_submit").pressButton()
    shown = [m for m in hou.ui.messages if m["details"]]
    assert shown and shown[0]["details"].startswith("# Saved from houdini") and "tool: karma" in shown[0]["details"]
    assert not fake.jobs and not fake.store
    assert summary(node).endswith("Not submitted.")


def test_cancel_stops_a_check(tmp_path, monkeypatch):
    show, node, fake = lighting(tmp_path, monkeypatch)
    hou.InterruptableOperation.interrupt = True
    node.parm("c4_check").pressButton()
    assert summary(node) == "Check cancelled."
    assert not [c for c in fake.calls if c[0] in ("missing", "check")], "the farm is not asked"
    assert not hou.rendered, "nor is the stage written"


def test_cancel_once_the_stage_is_written(tmp_path, monkeypatch):
    show, node, fake = lighting(tmp_path, monkeypatch)
    real = hou.InterruptableOperation.updateLongProgress

    def update(self, percentage=-1.0, long_op_status=None):
        # Cancel is pressed once the stage is written, while files are read.
        real(self, percentage, long_op_status)
        if hou.rendered:
            raise hou.OperationInterrupted("cancelled")

    monkeypatch.setattr(hou.InterruptableOperation, "updateLongProgress", update)
    assert nodes.panel_for(node).check() is None
    assert summary(node) == "Check cancelled." and hou.rendered
    assert not [c for c in fake.calls if c[0] == "check"], "the farm is not asked"


def test_cancel_while_reading_files(tmp_path, monkeypatch):
    show, submit, fake = fx(tmp_path, monkeypatch, cache_on_disk=True)
    real_id, real_update = collect.identify_file, hou.InterruptableOperation.updateLongProgress
    read = []

    def slow(path, cancelled=None):
        time.sleep(0.03)
        read.append(path)
        return real_id(path, cancelled)

    def update(self, percentage=-1.0, long_op_status=None):
        real_update(self, percentage, long_op_status)
        if (long_op_status or "").startswith("Reading files"):
            raise hou.OperationInterrupted("cancelled")

    monkeypatch.setattr(collect, "identify_file", slow)
    monkeypatch.setattr(hou.InterruptableOperation, "updateLongProgress", update)
    assert nodes.panel_for(submit).check() is None
    assert summary(submit) == "Check cancelled."
    assert 0 < len(read) < 100, "files stop being read"
    assert not [c for c in fake.calls if c[0] in ("missing", "check")]


def test_submit_without_ui_submits(tmp_path, monkeypatch):
    show, node, fake = lighting(tmp_path, monkeypatch)
    hou.ui.available = False
    out = nodes.panel_for(node).submit()
    assert out.jobs == ["sm0220-light-v001"] and not hou.ui.messages


def checks(fake) -> int:
    return len([c for c in fake.calls if c[0] == "check"])


def test_a_check_that_holds_is_reused(tmp_path, monkeypatch):
    show, node, fake = lighting(tmp_path, monkeypatch)
    node.parm("c4_check").pressButton()
    assert (len(hou.rendered), checks(fake)) == (1, 1)
    hou.ui.files = [str(tmp_path / "a.yaml")]
    node.parm("c4_save").pressButton()
    node.parm("c4_show").pressButton()
    assert (len(hou.rendered), checks(fake)) == (1, 1), "nothing changed, so the check holds"
    assert os.path.isfile(tmp_path / "a.yaml")

    node.parm("c4_note").set("changed without a callback")
    hou.ui.files = [str(tmp_path / "b.yaml")]
    node.parm("c4_save").pressButton()
    assert (len(hou.rendered), checks(fake)) == (2, 2), "the node changed, so it is checked again"
    assert yamlio.load((tmp_path / "b.yaml").read_text())["note"] == "changed without a callback"

    hou.hipFile.modified = True
    node.parm("c4_show").pressButton()
    assert (len(hou.rendered), checks(fake)) == (3, 3), "an unsaved hip file may hold a different stage"

    hou.hipFile.modified = False
    hou.ui.answers = [SUBMIT]
    node.parm("c4_submit").pressButton()
    assert checks(fake) == 3 and len(fake.jobs) == 1, "Submit reuses the check that holds"


def test_a_hip_saved_since_the_check_is_checked_again(tmp_path, monkeypatch):
    show, submit, fake = fx(tmp_path, monkeypatch, cache_on_disk=True)
    submit.parm("c4_check").pressButton()
    panel = nodes.panel_for(submit)
    assert not panel.stale()
    hou.hipFile.save()   # the saved hip file is what the farm opens: a new one is a new submission
    assert panel.stale()
    hou.ui.answers = [SUBMIT]
    submit.parm("c4_submit").pressButton()
    assert checks(fake) == 2 and len(fake.jobs) == 1
    assert fake.jobs[0]["tree"] == panel.checked.tree_id


def test_tools_from_c4_tool_path_when_the_farm_lists_none(tmp_path, monkeypatch):
    show, lop = light_scene(tmp_path)
    env(monkeypatch, show, "sm0220")
    fake = FakeFarm(tools=[], projects={"saltmarsh": dict(SALTMARSH, root="/show")}, serves_submissions=False)
    monkeypatch.setattr(nodes, "farm_factory", lambda: fake)

    node = c4render(lop)
    nodes.install(node)
    assert "set C4_TOOL_PATH" in summary(node) and node.parm("c4p_stage") is None

    tools = tmp_path / "tools" / "karma"
    tools.mkdir(parents=True)
    (tools / "tool.yaml").write_text(json.dumps(KARMA))
    monkeypatch.setenv("C4_TOOL_PATH", str(tmp_path / "tools"))
    hou.change(node.parm("c4_project"), "saltmarsh")
    assert node.parm("c4_tool").eval() == "karma" and node.parm("c4p_stage") is not None

    path = str(tmp_path / "sm0220.yaml")
    hou.ui.files = [path]
    node.parm("c4_save").pressButton()
    assert "does not take submissions yet" in summary(node)
    assert yamlio.load(open(path, encoding="utf-8").read())["jobs"]["light"]["tool"] == "karma"


def test_a_sequence_is_sent_for_the_jobs_frames_only(tmp_path, monkeypatch):
    show, submit, fake = fx(tmp_path, monkeypatch)
    hou.node("/out/mantra1").parmTuple("f").set((1001, 1010, 1))
    submit.parm("c4_check").pressButton()
    panel = nodes.panel_for(submit)
    files = sorted(panel.checked.collection.files)
    fluid = [f for f in files if "/geo/fluid." in f]
    assert fluid == [f"sm0140/fx/geo/fluid.{f}.bgeo.sc" for f in range(1001, 1011)], \
        "mantra1 renders 1001-1010 and flip 1001: the sequence is sent for those frames"


def test_a_retimed_sequence_is_sent_whole(tmp_path, monkeypatch):
    show, submit, fake = fx(tmp_path, monkeypatch)
    hou.node("/out/mantra1").parmTuple("f").set((1001, 1010, 1))
    rest = hou.Node("/obj/geo1/rest", "timeshift", "Sop", hou.NODE_PARMS[("Sop", "timeshift")]())
    rest.setInput(0, hou.node("/obj/geo1/fluid"))
    submit.parm("c4_check").pressButton()
    files = nodes.panel_for(submit).checked.collection.files
    assert len([f for f in files if "/geo/fluid." in f]) == 48, "a Time Shift may read any frame"
