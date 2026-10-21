"""c4submit against answers a real farm gave (fixtures/farm/*.json,
recorded from a local farm made by c4 farm up), so the fake farm's shapes
cannot drift from the real one's unnoticed."""

import json
import os

from c4submit import summary
from c4submit.collect import NoCache
from c4submit.host import Dependency
from c4submit.submitter import Draft, Submitter, prepare
from c4submit.testing import FakeHost, write
from c4submit.tool import Tool

HERE = os.path.dirname(__file__)

# The local farm's comp tool, as GET /tools/saltmarsh/comp gave it.
COMP = {"name": "comp", "title": "Composite", "kind": "frames", "version": "v2",
        "frames": {"chunk": 4, "order": "binary"},
        "params": [
            {"name": "script", "type": "PATH", "objectType": "FILE", "dataFlow": "IN", "from": "scene.file",
             "userInterface": {"control": "CHOOSE_INPUT_FILE", "label": "Comp script",
                               "fileFilters": [{"label": "Nuke", "patterns": ["*.nk"]},
                                               {"label": "OpenImageIO script", "patterns": ["*.sh"]}]}},
            {"name": "engine", "type": "STRING", "default": "auto", "allowedValues": ["auto", "nuke", "oiio"],
             "userInterface": {"control": "DROPDOWN_LIST", "label": "Engine"}, "show": "advanced"}]}


def recorded(name):
    with open(os.path.join(HERE, "fixtures", "farm", name), encoding="utf-8") as f:
        return json.load(f)


class Replay:
    """A farm that answers a dry run as the real one did."""

    def __init__(self, answer):
        self.answer = answer

    def missing(self, project, ids):
        return list(ids)

    def check(self, doc, explain=False):
        return self.answer


def scene(tmp_path):
    show = str(tmp_path)
    nk = write(os.path.join(show, "comp", "sm0140_comp_v004.nk"), b"# comp\n")
    for f in range(1001, 1009):
        write(os.path.join(show, "plates", f"sm0140.{f}.exr"), b"%d" % f)
    return FakeHost(scene=nk, frames="1001-1008",
                    deps=[Dependency(os.path.join(show, "plates", "sm0140.####.exr"), "1001-1008", node="Read1")],
                    env={"C4_PROJECT": "saltmarsh", "C4_SHOT": "sm0140", "C4_ROOT": show})


def test_a_real_answer_reads_into_the_summary(tmp_path):
    rec = recorded("dryrun-two-jobs.json")
    eta = rec["answer"]["jobs"][0]["estimate"]["eta50"]
    assert len(eta.split(".")[1]) > 12, "the farm writes nanoseconds, which is the point of this test"
    d = Draft(scene(tmp_path), Tool(COMP), project={"root": "/show", "milestones": []})
    d.due = "+3h"
    c = Submitter(Replay(rec["answer"]), cache=NoCache()).check(prepare(d))
    assert c.ok, c.errors
    lines = summary.lines(c)
    assert [ln.text for ln in lines if ln.label == ""][0] == "sm0140-comp-v001   Composite · 8 frames in 2 units"
    assert any(ln.label == "estimate" and ln.text.startswith("ready ") for ln in lines), summary.text(c)
    assert any(ln.label == "due" and "slack" in ln.text for ln in lines)


def test_a_refusal_here_is_the_farms(tmp_path):
    rec = recorded("dryrun-refused.json")
    d = Draft(scene(tmp_path), Tool(COMP), project={"root": "/show", "milestones": []})
    d.form.set("engine", "renderman")
    p = prepare(d)
    assert [pr.as_error() for pr in p.problems] == rec["answer"]["errors"]
