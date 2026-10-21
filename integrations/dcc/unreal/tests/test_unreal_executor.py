"""Render (Remote) with C4 as the remote executor, as an artist presses it,
against the fake farm."""

import json
import os
import re
import runpy
import time

import fakeunreal as ue
from c4submit import yamlio
from c4submit.collect import Cancelled
from c4submit.submitter import Submitter
from c4submit.testing import FakeFarm
from unreal_projects import ALL_FILES, UNREAL_RENDER, env, farm, saltmarsh

import c4_unreal
from c4_unreal import executor
from c4_unreal.executor import C4FarmExecutor, C4FarmOptions

BANNED = re.compile(r"\b(image|images|pod|pods|namespace|namespaces|priority|priorities|kubernetes|k8s|"
                    r"container|containers|kubectl|cluster|deployment)\b", re.I)


def ready(tmp_path, monkeypatch, fake=None):
    show, proj, queue = saltmarsh(tmp_path)
    env(monkeypatch, show)
    fake = fake if fake is not None else farm()
    monkeypatch.setattr(executor, "farm_factory", lambda: fake)
    return show, proj, queue, fake


def render_remote(queue):
    C4FarmExecutor().execute(queue)
    assert ue.state.finished == 1, "the queue is told the executor is done, whatever happened"


def pick(**values):
    def edit(opts):
        for k, v in values.items():
            setattr(opts, k, v)
    return edit


def test_render_remote_checks_then_submits(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)
    ue.state.dirty = ["/Game/Env/Rock"]
    ue.state.on_options = pick(needed_by="Dailies, Thu 18:00", note="lighting v3")
    ue.state.answers = ["yes", "ok"]
    render_remote(queue)

    seen = ue.state.options_seen[0]
    assert seen["project"] == "saltmarsh" and seen["shot"] == "sh010" and seen["tool"] == "Unreal render"
    assert executor._CHOICES["needed_by"] == ["Dailies, Thu 18:00", "Client review, Fri 10:00", "When the farm suggests"]
    assert executor._CHOICES["tool"] == ["Unreal render"], "only tools that render .uproject files"
    assert ue.state.save_dialogs == 1, "Unreal's own Save dialog for the unsaved rock"

    summary, done = ue.state.messages()
    heads = [ln for ln in summary.splitlines() if ln.startswith("sh010-render-")]
    assert heads == ["sh010-render-sh010-lighting-v001   Unreal render · one unit over 1001-1010",
                     "sh010-render-sh020-lighting-v001   Unreal render · 24 frames in 3 units"]
    assert re.search(r"due\s+.*· Dailies, Thu 18:00 · slack ", summary)
    assert f"{ALL_FILES} found from Saltmarsh.uproject" in summary
    assert summary.endswith("Yes: submit to the farm.\nNo: show the submission as a file, to save it.\n"
                            "Cancel: send nothing.")
    assert ue.state.shown[1][3] == "YES_NO_CANCEL"

    assert len(fake.jobs) == 1
    doc = fake.jobs[0]
    assert doc["due"] == "dailies" and doc["note"] == "lighting v3" and doc["shot"] == "sh010"
    assert doc["jobs"]["render-sh010-lighting"] == {"tool": "unreal-render", "frames": "1001-1010", "with": {
        "project": "unreal/Saltmarsh/Saltmarsh.uproject", "map": "/Game/Maps/Sh010",
        "sequence": "/Game/Cinematics/sh010_seq.sh010_seq", "config": "C4/queue/01-sh010-lighting.utxt"}}
    assert doc["jobs"]["render-sh020-lighting"]["frames"] == "1001-1024"
    assert "old_test" not in json.dumps(doc), "the disabled job stays home"
    assert len(fake.store) == ALL_FILES
    assert done.startswith("Submitted to the farm:\n  sh010-render-sh010-lighting-v001\n  sh010-render-sh020-lighting-v001")
    assert f"{ALL_FILES} files sent" in done
    titles = [t.desc for t in ue.state.tasks]
    assert titles == ["Reading saltmarsh's tools from the farm", "Checking with the farm", "Submitting to the farm"]
    assert all(t.can_cancel for t in ue.state.tasks)


def test_a_second_submission_sends_only_what_changed(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)
    ue.state.answers = ["yes", "ok"]
    render_remote(queue)
    with open(os.path.join(proj, "Content", "Env", "Rock.uasset"), "ab") as f:
        f.write(b" a new rock")
    ue.state.finished = 0
    ue.state.answers = ["yes", "ok"]
    render_remote(queue)
    assert ue.state.messages()[-2].count(f"{ALL_FILES} found") == 1 and "1 to upload" in ue.state.messages()[-2]
    assert "\n1 files sent" in ue.state.messages()[-1] and len(fake.jobs) == 2
    assert "sh010-render-sh010-lighting-v002" in ue.state.messages()[-1]


def test_show_file_then_save_it(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)
    ue.state.answers = ["no", "no", "no", "yes", "ok"]   # show it, back, show it again, save it
    render_remote(queue)
    msgs = ue.state.messages()
    path = os.path.join(proj, "Saved", "C4", "sh010-render-sh010-lighting.yaml")
    assert msgs[1].startswith("# Saved from unreal by c4submit") and "\nc4: submit/1\n" in msgs[1]
    assert msgs[1].endswith(
        f"Yes: save it as {path}.\nNo: back to the summary.\nCancel: send nothing.")
    assert msgs[2] == msgs[0], "back to the summary"
    with open(path) as f:
        text = f.read()
    assert text.startswith("# Saved from unreal by c4submit")
    assert yamlio.load(text)["jobs"]["render-sh020-lighting"]["with"]["config"] == "C4/queue/02-sh020-lighting.utxt"
    assert msgs[-1] == f"Saved {path}.\nSubmit it later with: c4 farm submit sh010-render-sh010-lighting.yaml"
    assert not fake.jobs, "saving submits nothing"
    assert any(m.startswith("C4: the submission:\n# Saved from unreal") for _, m in ue.state.logs), \
        "all of it in the Output Log"


def test_cancel_sends_nothing(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)
    ue.state.answers = ["cancel"]
    render_remote(queue)
    assert not fake.jobs and not fake.store
    ue.state.finished = 0
    ue.state.options_ok = False
    render_remote(queue)
    assert len(ue.state.messages()) == 1, "cancelled in the details panel: no check, no summary"


def test_errors_come_with_their_fix_and_no_submit(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)
    ue.state.dirty = ["/Game/Maps/Sh010"]
    ue.state.refuse_save = True
    ue.state.answers = ["no"]
    render_remote(queue)
    kind, title, text, buttons = ue.state.shown[-1]
    assert title == "C4 Farm: not ready" and buttons == "YES_NO", "no Submit"
    assert text.startswith("To fix before submitting:\n  scene: could not save the project: some maps or assets "
                           "were not saved")
    assert "\n      save it yourself; the farm reads the saved project" in text
    assert text.endswith("Fix what is listed, then render again.\n\n"
                         "Yes: show the submission as a file, to save it.\nNo: send nothing.")
    assert not fake.jobs


def test_how_the_project_was_read_is_in_the_summary(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)
    monkeypatch.setenv("C4_UNREAL_CONTENT", "referenced")
    ue.state.registry_fails = True
    ue.state.answers = ["yes", "ok"]
    render_remote(queue)
    summary = ue.state.messages()[0]
    assert summary.startswith("Worth knowing:\n  the asset registry could not list what the jobs read "
                              "(the asset registry is still loading), so all of Content is sent\n")
    assert len(fake.jobs) == 1, "a warning does not stop the submission"


def test_cancel_a_check(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)

    class Slow(Submitter):
        def check(self, prepared, explain=False):
            self._tick("hash", 5, 19)
            while not self.cancelled():
                time.sleep(0.002)
            raise Cancelled()

    monkeypatch.setattr(executor, "Submitter", Slow)
    ue.state.cancel_after = 3
    ue.state.answers = ["ok"]
    render_remote(queue)
    assert ue.state.messages() == ["Check cancelled: nothing was sent."]
    assert "Reading files: 5 of 19" in ue.state.tasks[-1].frames and not fake.jobs


def test_changed_since_the_check(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)

    def edit_then_submit(title, text):
        ue.state.dirty = ["/Game/Env/Rock"]   # the artist moves the rock before answering
        return "yes"

    ue.state.answers = [edit_then_submit, "ok"]
    render_remote(queue)
    assert ue.state.messages()[-1] == executor.STALE and not fake.jobs


def test_needed_by_a_typed_time_and_a_size(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)
    ue.state.on_options = pick(needed_at="fri 18:00", size="heavy")
    ue.state.answers = ["yes", "ok"]
    render_remote(queue)
    doc = fake.jobs[0]
    assert doc["due"] == "fri 18:00" and {j["size"] for j in doc["jobs"].values()} == {"heavy"}
    assert executor._CHOICES["size"] == ["Standard", "heavy"]
    ue.state.finished = 0
    ue.state.on_options = pick(needed_at="")
    ue.state.answers = ["cancel"]
    render_remote(queue)
    assert ue.state.options_seen[-1]["needed_at"] == "fri 18:00", "Needed by is kept for the next render"


def test_a_project_typed_into_the_panel(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)
    monkeypatch.delenv("C4_PROJECT")
    ue.state.on_options = lambda o: setattr(o, "project", o.project or "saltmarsh")
    ue.state.answers = ["yes", "ok"]
    render_remote(queue)
    assert [s["project"] for s in ue.state.options_seen] == ["", "saltmarsh"], "shown again with its tools"
    assert fake.jobs[0]["project"] == "saltmarsh"


def test_a_farm_without_tools(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch, fake=FakeFarm(serves_submissions=False))
    ue.state.answers = ["ok"]
    render_remote(queue)
    msg = ue.state.messages()[0]
    assert "does not serve tools yet" in msg and "C4_TOOL_PATH" in msg


def test_tools_from_c4_tool_path(tmp_path, monkeypatch):
    tools = tmp_path / "tools" / "unreal-render"
    tools.mkdir(parents=True)
    (tools / "tool.yaml").write_text(json.dumps(UNREAL_RENDER))
    monkeypatch.setenv("C4_TOOL_PATH", str(tmp_path / "tools"))
    show, proj, queue, fake = ready(tmp_path / "s", monkeypatch, fake=FakeFarm(serves_submissions=False))
    ue.state.answers = ["yes", "yes", "ok"]   # show the file, save it
    render_remote(queue)
    first = ue.state.messages()[0]
    assert first.startswith("Worth knowing:\n  this farm does not take submissions yet")
    assert "Saved as a file, it can be submitted with c4 farm submit" in first
    assert ue.state.messages()[-1].startswith("Saved ")


def test_no_farm_address(tmp_path, monkeypatch):
    show, proj, queue = saltmarsh(tmp_path)
    env(monkeypatch, show)
    ue.state.answers = ["ok"]
    render_remote(queue)
    assert ue.state.messages()[0].startswith("the farm's address is not set")


def test_an_empty_queue(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)
    for j in queue.jobs:
        j.set_is_enabled(False)
    ue.state.answers = ["ok"]
    render_remote(queue)
    assert ue.state.messages() == ["The queue has no enabled job to render."]


def test_a_failure_is_shown_and_the_queue_still_released(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)

    def boom(self):
        raise RuntimeError("the editor is shutting down")

    monkeypatch.setattr(executor.Flow, "run", boom)
    ue.state.answers = ["ok"]
    render_remote(queue)
    assert ue.state.messages() == ["Could not submit: the editor is shutting down"]
    assert C4FarmExecutor().is_rendering() is False


def test_the_startup_script_loads_the_executor(tmp_path):
    here = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    runpy.run_path(os.path.join(here, "python", "init_unreal.py"))
    assert ("log", "C4: C4FarmExecutor is ready; pick it as the Movie Render Queue's Default Remote Executor") \
        in ue.state.logs
    assert c4_unreal.EXECUTOR == "/Engine/PythonTypes.C4FarmExecutor"


def test_no_farm_words_anywhere_the_artist_reads(tmp_path, monkeypatch):
    show, proj, queue, fake = ready(tmp_path, monkeypatch)
    ue.state.answers = ["no", "no", "yes", "ok"]
    render_remote(queue)
    ue.state.finished = 0
    ue.state.dirty = ["/Game/Maps/Sh010"]
    ue.state.refuse_save = True
    ue.state.answers = ["yes", "cancel"]   # a check with errors: show the file, then send nothing
    render_remote(queue)
    seen =[m[1] for m in ue.state.shown] + ue.state.messages() + [m for _, m in ue.state.logs]
    seen += [f for t in ue.state.tasks for f in t.frames + [t.desc]]
    seen += [str(v) for meta in ue.uprops[C4FarmOptions].values() for v in meta.values()]
    seen += [executor.LOCAL_TOOLS, executor.STALE] + list(executor.PROGRESS_WORDS.values())
    bad = {m.group(0) for s in seen for m in BANNED.finditer(s)}
    assert bad == set()
