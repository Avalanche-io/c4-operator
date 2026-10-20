"""From a scene on disk to submitted jobs, against the fake farm in memory
and over HTTP."""

import os

import pytest

from c4submit import collect, summary
from c4submit.farm import Farm, FarmError
from c4submit.host import Dependency, Renderable
from c4submit.submitter import Draft, Submitter, default_filename, prepare, save
from c4submit.testing import NOW, SALTMARSH, FakeFarm, FakeHost, serve, write
from c4submit.tool import Tool
from c4submit import yamlio


def comp_scene(root, modified=False, extra_deps=()):
    """A comp script with two Writes: bg, and main which reads bg's render."""
    show = str(root)
    nk = write(os.path.join(show, "comp", "sm0140_comp_v004.nk"), b"# nuke script\n")
    for f in range(1001, 1049):
        write(os.path.join(show, "plates", f"sm0140.{f}.exr"), b"plate %d" % f)
        write(os.path.join(show, "render", "light", f"sm0140_beauty.{f:04d}.exr"), b"beauty %d" % f)
    write(os.path.join(show, "luts", "show.cube"), b"LUT")
    renderables = [
        Renderable("Write_bg", outputs=[os.path.join(show, "comp", "out", "bg.%04d.exr")]),
        Renderable("Write_main", frames="1001-1048", outputs=[os.path.join(show, "comp", "out", "main.####.exr")],
                   reads=["Write_bg"]),
    ]
    deps = [
        Dependency(os.path.join(show, "plates", "sm0140.####.exr"), "1001-1048", node="Read_plate"),
        Dependency(os.path.join(show, "render", "light", "sm0140_beauty.%04d.exr"), "1001-1048", node="Read_beauty"),
        Dependency(os.path.join(show, "comp", "out", "bg.####.exr"), "1001-1048", node="Read_bg", renderables=["Write_main"]),
        Dependency(os.path.join(show, "luts", "show.cube"), node="OCIOFileTransform1"),
    ] + list(extra_deps)
    env = {"C4_PROJECT": "saltmarsh", "C4_SHOT": "sm0140", "C4_ROOT": show}
    return FakeHost(scene=nk, frames="1001-1048", renderables=renderables, deps=deps, env=env, modified=modified)


@pytest.fixture
def farm():
    return FakeFarm(projects={"saltmarsh": dict(SALTMARSH, root="/show")})


def draft_for(host, farm):
    tool = Tool(farm.tool("saltmarsh", "nuke-comp"))
    return Draft(host, tool, project=farm.project("saltmarsh"))


def test_two_writes_become_two_jobs_with_after(tmp_path, farm, nocache):
    host = comp_scene(tmp_path, modified=True)
    d = draft_for(host, farm)
    d.due = "dailies"
    p = prepare(d)
    assert host.saves == 1, "the farm reads the saved script, so prepare saves it"
    assert p.problems == []
    assert p.doc["jobs"] == {
        "comp-bg": {"tool": "nuke-comp", "frames": "1001-1048",
                    "with": {"script": "comp/sm0140_comp_v004.nk", "write": "Write_bg"}},
        "comp-main": {"tool": "nuke-comp", "frames": "1001-1048",
                      "with": {"script": "comp/sm0140_comp_v004.nk", "write": "Write_main"}, "after": "comp-bg"},
    }
    assert p.doc["root"] == str(tmp_path), "the artist's root differs from the farm's /show, so it is recorded"
    # bg's render is made by the first job, so it is not collected.
    col = p.collection
    assert len(col.files) == 1 + 48 + 48 + 1
    assert not any(r.startswith("comp/out/") for r in col.files)

    s = Submitter(farm, cache=nocache)
    c = s.check(p)
    assert c.ok, c.errors
    assert c.doc["tree"] == col.tree_id() and len(c.missing) == len(col.files)
    assert list(c.doc)[:6] == ["c4", "project", "shot", "due", "tree", "root"]
    assert farm.calls[-1][0] == "check" and not farm.store, "a check uploads nothing"

    lines = summary.lines(c, now=NOW)
    heads = [ln.text for ln in lines if ln.label == ""]
    assert heads[0].startswith("sm0140-comp-bg-v001   Nuke comp · 48 frames · 12 units")
    assert "after comp-bg" in heads[1]
    dues = [ln for ln in lines if ln.label == "due"]
    assert dues and "dailies" in dues[0].text and "slack" in dues[0].text
    assert any(ln.label == "licences" and "6 of 12 seats free now" in ln.text for ln in lines)
    files = [ln.text for ln in lines if ln.label == "files"][0]
    assert files.startswith("98 found from sm0140_comp_v004.nk") and "98 to upload" in files

    out = s.submit(c)
    assert out.jobs == ["sm0140-comp-bg-v001", "sm0140-comp-main-v001"]
    assert out.uploaded == 98 and len(farm.store) == 98
    assert c.tree_id in farm.views

    # Again, unchanged: nothing to upload, and the next versions.
    p2 = prepare(draft_for(host, farm))
    c2 = s.check(p2)
    assert c2.missing == [] and c2.doc["tree"] == c.doc["tree"]
    assert s.submit(c2).jobs == ["sm0140-comp-bg-v002", "sm0140-comp-main-v002"]


def test_untick_and_typed_frames(tmp_path, farm, nocache):
    host = comp_scene(tmp_path)
    d = draft_for(host, farm)
    d.tick("Write_bg", False)
    d.frames = "1001-1010"
    p = prepare(d)
    assert list(p.doc["jobs"]) == ["comp"]
    job = p.doc["jobs"]["comp"]
    assert job["frames"] == "1001-1010" and "after" not in job and job["with"]["write"] == "Write_main"
    # Write_main reads bg's render, which is not in this submission and not on disk.
    errs = [pr for pr in p.collection.problems if pr.level == "error"]
    assert errs and "bg.####.exr" in errs[0].message and errs[0].path == "Read_bg"
    c = Submitter(farm, cache=nocache).check(p)
    assert not c.ok and c.answer is None, "local errors stop before the farm is asked"


def test_files_outside_the_root_and_unsaved(tmp_path, farm, nocache):
    outside = write(str(tmp_path / "elsewhere" / "grain.exr"))
    show = tmp_path / "show"
    host = comp_scene(show, extra_deps=[Dependency(outside, node="Read_grain")], modified=True)
    d = draft_for(host, farm)
    d.save_scene = False
    p = prepare(d)
    assert any(pr.level == "warning" and "unsaved" in pr.message for pr in p.problems)
    errs = [pr for pr in p.collection.problems if pr.level == "error"]
    assert errs[0].path == "Read_grain" and "outside the project folder" in errs[0].message


def test_no_shot_and_no_root(tmp_path, farm):
    host = comp_scene(tmp_path)
    host.env = {"C4_PROJECT": "saltmarsh"}
    d = Draft(host, Tool(farm.tool("saltmarsh", "nuke-comp")), project={"root": "/nonexistent-root"})
    p = prepare(d)
    assert [pr.path for pr in p.problems] == ["shot"]
    assert p.collection.problems[0].path == "root"


def test_shot_from_pattern(tmp_path):
    host = FakeHost(scene=str(tmp_path / "shows/saltmarsh/sm0220/comp/x.nk"),
                    env={"C4_SHOT_PATTERN": r"/(?P<shot>sm\d{4})/"})
    assert host.shot() == "sm0220"


def test_a_check_the_farm_refuses(tmp_path, farm, nocache):
    host = comp_scene(tmp_path)
    d = draft_for(host, farm)
    d.form.set("threads", 4)
    d.size = "heavy"
    c = Submitter(farm, cache=nocache).check(prepare(d))
    assert not c.ok
    assert {"path": "jobs.comp-bg.size", "message": 'nuke-comp has no size "heavy"'} in c.errors
    with pytest.raises(FarmError):
        Submitter(farm, cache=nocache).submit(c)


def test_saved_file_is_the_submission(tmp_path, farm, nocache):
    host = comp_scene(tmp_path)
    d = draft_for(host, farm)
    d.due = "fri 18:00"
    d.note = "comp v4: new edge treatment"
    c = Submitter(farm, cache=nocache).check(prepare(d))
    path = save(c, str(tmp_path / default_filename(c)))
    assert os.path.basename(path) == "sm0140-comp-bg.yaml"
    with open(path) as f:
        text = f.read()
    assert text.startswith("# Saved from fake by c4submit")
    assert yamlio.load(text) == c.doc


def test_farm_without_submissions_still_checks_and_saves(tmp_path, nocache):
    farm = FakeFarm(serves_submissions=False)
    host = comp_scene(tmp_path)
    d = Draft(host, Tool({"tool": FakeFarm().tools_by["nuke-comp"]}), project=None)
    c = Submitter(farm, cache=nocache).check(prepare(d))
    assert c.errors == [] and not c.ok and not c.farm_checked
    assert any("does not take submissions yet" in w for w in c.warnings)
    assert "root" not in c.doc, "without the farm's root path, none is recorded"


def test_cancel_while_hashing(tmp_path, farm, nocache):
    host = comp_scene(tmp_path)
    s = Submitter(farm, cache=nocache, cancelled=lambda: True)
    with pytest.raises(collect.Cancelled):
        s.check(prepare(draft_for(host, farm)))


def test_id_cache_skips_unchanged_files(tmp_path, farm, monkeypatch):
    host = comp_scene(tmp_path)
    cache = collect.IDCache(str(tmp_path / "cache.json"))
    s = Submitter(farm, cache=cache)
    s.check(prepare(draft_for(host, farm)))
    reads = []
    real = collect.identify_file
    monkeypatch.setattr(collect, "identify_file", lambda p: reads.append(p) or real(p))
    write(os.path.join(str(tmp_path), "luts", "show.cube"), b"LUT v2, longer")
    s2 = Submitter(farm, cache=collect.IDCache(str(tmp_path / "cache.json")))
    s2.check(prepare(draft_for(host, farm)))
    assert [os.path.basename(r) for r in reads] == ["show.cube"]


def test_over_http(tmp_path, nocache):
    fake = FakeFarm(projects={"saltmarsh": dict(SALTMARSH, root=str(tmp_path))})
    served = serve(fake, token="tok")
    try:
        token_file = tmp_path / "token"
        token_file.write_text("tok\n")
        farm = Farm(base=served.url, token_file=str(token_file))
        host = comp_scene(tmp_path)
        info = farm.project("saltmarsh")
        assert [m["name"] for m in info["milestones"]] == ["dailies", "client-review"]
        s = Submitter(farm, cache=nocache)
        tools = s.tools_for("saltmarsh", host.scene_file())
        assert [t.name for t in tools] == ["nuke-comp"]
        assert [c["value"] for c in s.due_choices(info)] == ["dailies", "client-review", "suggested"]
        d = Draft(host, tools[0], project=info)
        d.due = "client-review"
        p = prepare(d)
        assert "root" not in p.doc, "the artist's root is the farm's, so nothing is recorded"
        c = s.check(p)
        assert c.ok, c.errors
        out = s.submit(c)
        assert out.jobs == ["sm0140-comp-bg-v001", "sm0140-comp-main-v001"]
        assert len(fake.store) == 98

        # A file the farm refuses comes back as errors, not an exception.
        d.size = "heavy"
        c = s.check(prepare(d))
        assert c.farm_checked and {"path": "jobs.comp-bg.size", "message": 'nuke-comp has no size "heavy"'} in c.errors

        bad = Farm(base=served.url, token="wrong")
        with pytest.raises(FarmError) as e:
            bad.tools("saltmarsh")
        assert e.value.status == 401 and e.value.message == "sign in to the farm"
    finally:
        served.close()


def test_upload_failure_names_the_error(tmp_path, farm, nocache):
    host = comp_scene(tmp_path)
    s = Submitter(farm, cache=nocache)
    c = s.check(prepare(draft_for(host, farm)))
    farm.fail_put = c.missing[3]
    with pytest.raises(FarmError) as e:
        s.submit(c)
    assert e.value.status == 500
    assert not farm.jobs


def test_unreachable_farm(tmp_path, nocache):
    host = comp_scene(tmp_path)
    tool = Tool({"tool": FakeFarm().tools_by["nuke-comp"]})
    c = Submitter(Farm(base="http://127.0.0.1:9"), cache=nocache).check(prepare(Draft(host, tool)))
    assert c.errors and c.errors[0]["message"].startswith("cannot reach http://127.0.0.1:9")
