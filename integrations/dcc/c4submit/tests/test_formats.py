"""The submission and tool formats as a front end reads and writes them:
the format's own examples pass, mistakes come back as path, message and
fix, YAML round-trips, and a tool's parameters draw a form."""

import glob
import os

import pytest
import yaml

from c4submit import submission, yamlio
from c4submit.form import Form
from c4submit.testing import KARMA, NUKE_COMP
from c4submit.tool import Tool

HERE = os.path.dirname(__file__)
# The schema's passing fixtures (schema/fixtures), once they are in the repository.
SCHEMA_PASS = os.path.join(HERE, "..", "..", "..", "..", "schema", "fixtures", "submit", "pass")


def _examples():
    paths = sorted(glob.glob(os.path.join(HERE, "fixtures", "submit", "*.yaml")))
    paths += sorted(glob.glob(os.path.join(SCHEMA_PASS, "*.yaml")))
    return paths


@pytest.mark.parametrize("path", _examples(), ids=os.path.basename)
def test_examples_pass_the_local_check(path):
    with open(path, encoding="utf-8") as f:
        doc = yaml.safe_load(f)
    if not (isinstance(doc, dict) and "jobs" in doc):
        pytest.skip("not a submission (a plan a submission names)")
    assert submission.check(doc) == []


SCHEMA_FAIL = os.path.join(HERE, "..", "..", "..", "..", "schema", "fixtures", "submit", "fail")


@pytest.mark.parametrize("path", sorted(glob.glob(os.path.join(SCHEMA_FAIL, "*.yaml"))), ids=os.path.basename)
def test_refusals_match_the_schemas_fixtures(path):
    """The schema's refusals, word for word: every structural one locally,
    and those that need the farm (a tool's parameters, the project's
    milestones) either word for word or left to the farm."""
    import json
    with open(path, encoding="utf-8") as f:
        doc = yaml.safe_load(f)
    with open(path[:-5] + ".expect.json", encoding="utf-8") as f:
        want = json.load(f)
    got = submission.check(doc)
    if want["by"] == "schema":
        assert got == want["errors"]
    else:
        assert got in ([], want["errors"])


def _comp():
    with open(os.path.join(HERE, "fixtures", "submit", "comp.yaml"), encoding="utf-8") as f:
        return yaml.safe_load(f)


def test_after_names_a_key_with_a_suggestion():
    # An agent typed comp-v4 for the key comp.
    doc = _comp()
    doc["jobs"]["movie"]["after"] = "comp-v4"
    assert submission.check(doc) == [{
        "path": "jobs.movie.after",
        "message": 'no job "comp-v4" in this submission',
        "fix": 'jobs follow jobs by their key in the file; did you mean "comp"?',
    }]


def test_unknown_fields_and_shapes():
    doc = _comp()
    doc["priority"] = 50
    doc["jobs"]["comp"]["fames"] = "1-2"
    doc["jobs"]["comp"]["frames"] = "1048-1001"
    doc["due"] = "next tuesday-ish"
    doc["jobs"]["Movie"] = doc["jobs"].pop("movie")
    errs = {e["path"]: e for e in submission.check(doc)}
    assert errs["priority"]["message"] == "a submission has no priority: work is ordered by when it is due"
    assert errs["jobs.comp.fames"]["fix"] == 'did you mean "frames"?'
    assert errs["jobs.comp.frames"]["message"] == '"1048-1001" is not a frame list'
    assert "due" in errs
    assert errs["jobs.Movie"]["message"] == '"Movie" is not a job key'


def test_cycles_and_self():
    doc = {"c4": "submit/1", "jobs": {"a": {"tool": "t", "after": "b"}, "b": {"tool": "t", "after": {"a": "all"}},
                                      "c": {"tool": "t", "after": "c"}}}
    msgs = [e["message"] for e in submission.check(doc)]
    assert "a job cannot follow itself" in msgs
    assert "a and b follow each other, which is a cycle" in msgs


@pytest.mark.parametrize("due", ["dailies", "fri 18:00", "18:00", "+3h", "2026-10-23", "2026-10-24T06:00:00-04:00", "suggested"])
def test_due_forms(due):
    assert submission.due_problem(due) == ""


def test_job_keys():
    assert submission.job_key("comp") == "comp"
    assert submission.job_key("comp", "Write_main", several=True) == "comp-main"
    assert submission.job_key("comp", "Write1", several=True) == "comp-1"
    assert submission.job_key("light", "/Render/beauty settings", several=True) == "light-render-beauty-settings"
    assert submission.KEY.match(submission.job_key("comp", "x" * 80, several=True))


def test_yaml_round_trip_keeps_values_and_types():
    doc = {"c4": "submit/1", "project": "saltmarsh", "shot": "sm0140", "due": "fri 18:00", "tree": "c4" + "1" * 88,
           "note": "comp v4: new edge treatment, per Thursday's notes #2",
           "jobs": {"comp": {"tool": "nuke-comp", "frames": "1001-1048",
                             "with": {"script": "comp/sm0140 comp.nk", "write": "Write_main", "proxy": False,
                                      "threads": 8, "lut": "$F4", "when": "18:00", "on": "yes", "n": "1001",
                                      "d": "2026-10-23"}},
                    "movie": {"tool": "transcode", "after": ["comp"], "with": {}}}}
    text = yamlio.dump(doc, ["a comment"])
    assert text.startswith("# a comment\n")
    assert "with: {script:" in text or "with:\n" in text
    assert yaml.safe_load(text) == doc
    assert yamlio.load(text) == doc


def test_tool_reads_params_and_draws_a_form():
    t = Tool({"name": "nuke-comp", "version": "v2", "id": "c4x", "tool": NUKE_COMP})
    assert (t.name, t.version, t.kind, t.title) == ("nuke-comp", "v2", "frames", "Nuke comp")
    assert t.accepts_scene("/show/comp/sm0140_comp_v004.nk")
    assert not t.accepts_scene("/show/light/sm0140.usda")
    assert Tool(KARMA).accepts_scene("x.usdc")
    script = t.param("script")
    assert script.required and script.control == "CHOOSE_INPUT_FILE"
    assert t.param("proxy").control == "CHECK_BOX"
    assert t.param("threads").check(200) == "must be at most 128"
    assert t.param("threads").check("x") == "must be a whole number"
    assert Tool(KARMA).param("renderer").check("RenderMan") == "must be one of BRAY_HdKarmaXPU, BRAY_HdKarma"

    scene = {"scene.file": "/show/comp/a.nk", "scene.renderables": "Write_bg"}
    form = Form(t, derive=scene.get)
    assert [f.name for f in form.drawn()] == ["script", "write", "proxy", "threads"]
    assert [g for g, _ in form.groups()] == ["", "Advanced"]
    assert form.values() == {"script": "/show/comp/a.nk", "write": "Write_bg"}
    assert form.set("threads", "8") == ""
    assert form.values()["threads"] == 8
    form.set("write", "Write_fg")
    assert not form.field("write").linked and form.values()["write"] == "Write_fg"
    form.link("write")
    assert form.values()["write"] == "Write_bg"
    form.link("write", False)
    form.refresh({"scene.file": "/show/comp/a.nk", "scene.renderables": "Write_other"}.get)
    assert form.values()["write"] == "Write_bg"  # unlinked keeps what it showed
    form.set("threads", 500)
    assert form.errors() == [("threads", "must be at most 128")]


def test_required_field_without_a_scene():
    form = Form(Tool(NUKE_COMP))
    assert ("script", "needs a value") in form.errors()
    assert "script" not in form.values()


def test_local_tools_stand_in_when_the_farm_lists_none(tmp_path, monkeypatch):
    import json
    from c4submit.submitter import Submitter
    from c4submit.testing import FakeFarm
    from c4submit.tool import local_tools
    (tmp_path / "nuke-comp").mkdir()
    (tmp_path / "nuke-comp" / "tool.yaml").write_text(json.dumps(NUKE_COMP))
    (tmp_path / "broken").mkdir()
    (tmp_path / "broken" / "tool.yaml").write_text("{not json")
    assert [t.name for t in local_tools(str(tmp_path))] == ["nuke-comp"]
    monkeypatch.setenv("C4_TOOL_PATH", str(tmp_path))
    tools = Submitter(FakeFarm(serves_submissions=False)).tools_for("saltmarsh", "/show/a.nk")
    assert [t.name for t in tools] == ["nuke-comp"]
    assert Submitter.due_choices(None) == [{"value": "suggested", "label": "When the farm suggests", "at": ""}]
