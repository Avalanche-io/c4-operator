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
    assert heads[0] == "sm0140-comp-bg-v001   Nuke comp · 48 frames in 12 units"
    assert heads[1] == "sm0140-comp-main-v001   Nuke comp · 48 frames in 12 units, after sm0140-comp-bg-v001"
    dues = [ln for ln in lines if ln.label == "due"]
    assert dues and "Dailies, Thu 18:00" in dues[0].text and "slack" in dues[0].text
    # The second follows the first, which does not exist in a dry run: no estimate for it yet.
    assert [ln.level for ln in lines if ln.label == "estimate" and ln.job == "comp-main"] == ["warn"]
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
    monkeypatch.setattr(collect, "identify_file", lambda p, c=None: reads.append(p) or real(p, c))
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


def test_parameter_errors_once_per_job(tmp_path, farm):
    host = comp_scene(tmp_path)
    d = draft_for(host, farm)
    d.form.set("threads", 500)
    p = prepare(d)
    errs = [(pr.path, pr.message) for pr in p.problems]
    assert errs == [("jobs.comp-bg.with.threads", "Threads: must be at most 128"),
                    ("jobs.comp-main.with.threads", "Threads: must be at most 128")]


def test_submit_refuses_a_stale_check(tmp_path, farm, nocache):
    host = comp_scene(tmp_path)
    d = draft_for(host, farm)
    s = Submitter(farm, cache=nocache)
    c = s.check(prepare(d))
    assert c.ok and not c.stale()
    d.note = "changed my mind"
    assert c.stale()
    with pytest.raises(FarmError) as e:
        s.submit(c)
    assert "changed since the check" in e.value.message
    d.note = ""
    write(host.scene_file(), b"# nuke script, saved again\n")
    assert c.stale()
    assert not [ln for ln in summary.lines(s.check(prepare(d)), errors=False) if ln.level == "error"]


def test_a_host_names_its_jobs(tmp_path, farm):
    host = comp_scene(tmp_path)
    host._renderables = [Renderable("/out/mantra1", key="mantra1"), Renderable("/out/mantra2", key="mantra2")]
    assert list(draft_for(host, farm).keys()) == ["comp-mantra1", "comp-mantra2"]


def test_a_scene_written_for_every_submission(tmp_path, farm):
    host = comp_scene(tmp_path, modified=True)
    host.writes_scene = True
    d = draft_for(host, farm)
    d.save_scene = False
    msgs = [pr.message for pr in prepare(d).problems]
    assert msgs == ["the script is written for each submission; with that off, the farm renders the last one written"]
    host.modified = False
    d.save_scene = True
    prepare(d)
    assert host.saves == 1, "written even when nothing is unsaved"


def test_cancel_inside_a_big_file(tmp_path):
    from c4submit.c4id import Interrupted, identify_file
    big = write(str(tmp_path / "big.exr"), b"x" * (3 << 20))
    asked = []
    with pytest.raises(Interrupted):
        identify_file(big, lambda: asked.append(1) or len(asked) > 1)
    assert len(asked) == 2, "asked between chunks, and stopped at the second"


def test_keys_fit_the_job_name_under_a_long_shot(tmp_path):
    long = "Write_beauty_main_character_hero_pass_left_"
    host = FakeHost(scene=write(str(tmp_path / "a.nk")), renderables=[Renderable(long + "A"), Renderable(long + "B")],
                    env={"C4_ROOT": str(tmp_path), "C4_SHOT": "saltmarsh-sq40-sh0140"})
    d = Draft(host, Tool(FakeFarm().tools_by["nuke-comp"]))
    keys = list(d.keys())
    assert len(set(keys)) == 2
    assert all(len(f"{d.shot}-{k}-v001") <= 50 for k in keys)
    doc = {"c4": "submit/1", "shot": d.shot, "jobs": {k: {"tool": "nuke-comp"} for k in keys}}
    from c4submit import submission
    assert submission.check(doc) == []


def test_ticking_moves_the_derived_field_and_the_files(tmp_path, farm):
    host = comp_scene(tmp_path)
    nobody = write(os.path.join(str(tmp_path), "luts", "unused.cube"))
    host._deps.append(Dependency(nobody, node="Unused1", renderables=[]))
    d = draft_for(host, farm)
    assert d.form.field("write").value == "Write_bg"
    d.tick("Write_bg", False)
    assert d.form.field("write").value == "Write_main", "follows the tick without a refresh"
    col = prepare(d).collection
    assert "luts/unused.cube" not in col.files, "renderables=[] means no job reads it"


def test_a_host_reads_range_and_renderables_together(tmp_path, farm):
    host = comp_scene(tmp_path)
    calls = []
    host.read = lambda: calls.append(1) or ("1-10", [Renderable("Write_x")])
    d = draft_for(host, farm)
    assert calls == [1] and d.scene_frames == "1-10" and [r.name for r in d.renderables] == ["Write_x"]


def test_a_renderable_gives_fields_its_own_values(tmp_path):
    blender = {"c4": "tool/1", "name": "blender-render", "kind": "frames", "runs": {"image": "x", "command": ["x"]},
               "params": [
                   {"name": "blend", "type": "PATH", "from": "scene.file",
                    "userInterface": {"fileFilters": [{"patterns": ["*.blend"]}]}},
                   {"name": "scene", "type": "STRING", "from": "scene.renderables"},
                   {"name": "view_layer", "type": "STRING", "from": "scene.renderables"},
                   {"name": "camera", "type": "STRING", "default": "", "from": "scene.camera"}]}
    host = FakeHost(scene=write(str(tmp_path / "sh010.blend")), env={"C4_ROOT": str(tmp_path), "C4_SHOT": "sh010"},
                    renderables=[Renderable("sh010/bg", key="bg", values={"scene": "sh010", "view_layer": "bg"}),
                                 Renderable("sh010/fg", key="fg", values={"scene": "sh010", "view_layer": "fg",
                                                                          "scene.camera": "closeCam"})])
    d = Draft(host, Tool(blender))
    assert d.form.field("view_layer").value == "bg", "the first ticked one stands for the form"
    jobs = prepare(d).doc["jobs"]
    assert jobs["comp-bg"]["with"] == {"blend": "sh010.blend", "scene": "sh010", "view_layer": "bg"}
    assert jobs["comp-fg"]["with"] == {"blend": "sh010.blend", "scene": "sh010", "view_layer": "fg", "camera": "closeCam"}


def test_the_hosts_own_state_makes_a_check_stale(tmp_path, farm, nocache):
    host = comp_scene(tmp_path)
    d = draft_for(host, farm)
    c = Submitter(farm, cache=nocache).check(prepare(d))
    assert not c.stale()
    host.modified = True          # an edit only the application knows of
    d.refresh()                   # the panel refreshes on the main thread
    assert c.stale()


def test_a_hosts_warnings_reach_the_check(tmp_path, farm, nocache):
    host = comp_scene(tmp_path)
    host.warnings = lambda: ["every file was sent: the application could not say which ones the jobs read"]
    c = Submitter(farm, cache=nocache).check(prepare(draft_for(host, farm)))
    assert c.ok and "every file was sent: the application could not say which ones the jobs read" in c.warnings


def test_a_renderable_asks_for_its_whole_range_in_one_unit(tmp_path, farm, nocache):
    host = comp_scene(tmp_path)
    host._renderables = [Renderable("Write_sim", frames="1001-1048", chunk="all")]
    d = draft_for(host, farm)
    p = prepare(d)
    assert p.doc["jobs"]["comp"]["chunk"] == "all"
    c = Submitter(farm, cache=nocache).check(p)
    assert c.ok, c.errors
    assert c.answer["jobs"][0]["what"] == "one unit over 1001-1048"


def test_a_dropped_submit_is_retried_with_its_key_and_makes_one_set(tmp_path, farm, nocache):
    host = comp_scene(tmp_path)
    s = Submitter(farm, cache=nocache)
    s.submit_backoff = 0
    c = s.check(prepare(draft_for(host, farm)))
    farm.drop_after_submit = 1            # the jobs are made, the answer never arrives
    out = s.submit(c)
    assert out.jobs == ["sm0140-comp-bg-v001", "sm0140-comp-main-v001"]
    assert len(farm.jobs) == 1, "one set of jobs, however many tries"
    assert out.answer["message"] == "submitted already, with this key"
    assert sum(1 for k in farm.calls if k[0] == "submit") == 2


def test_the_key_goes_over_the_wire(tmp_path, nocache):
    fake = FakeFarm(projects={"saltmarsh": dict(SALTMARSH, root=str(tmp_path))})
    served = serve(fake)
    try:
        farm = Farm(base=served.url)
        host = comp_scene(tmp_path)
        s = Submitter(farm, cache=nocache)
        c = s.check(prepare(Draft(host, Tool(farm.tool("saltmarsh", "nuke-comp")), project=farm.project("saltmarsh"))))
        s.submit(c)
        again = farm.submit(c.doc, key=c.key)      # a retry the panel never saw answered
        assert again["message"] == "submitted already, with this key"
        assert len(fake.jobs) == 1 and fake.by_key.keys() == {c.key}
    finally:
        served.close()


def test_a_sequence_read_over_each_jobs_frames(tmp_path, farm, nocache):
    from c4submit.host import JOB_FRAMES
    show = str(tmp_path)
    for f in range(1001, 1061):
        write(os.path.join(show, "fx", f"smoke.{f}.vdb"), b"%d" % f)
    host = comp_scene(tmp_path, extra_deps=[
        Dependency(os.path.join(show, "fx", "smoke.####.vdb"), JOB_FRAMES, node="ReadSmoke", renderables=["Write_main"])])
    host._renderables[1].frames = "1001-1010"           # main renders ten frames
    d = draft_for(host, farm)
    col = prepare(d).collection
    smoke = sorted(r for r in col.files if r.startswith("fx/"))
    assert smoke == [f"fx/smoke.{f}.vdb" for f in range(1001, 1011)], "the frames of the job that reads it, not all 60"
    d.tick("Write_main", False)
    assert not any(r.startswith("fx/") for r in prepare(d).collection.files), "no ticked job reads it"


def test_handles_read_the_frames_around_each_job(tmp_path, farm):
    from c4submit.host import JOB_FRAMES
    show = str(tmp_path)
    for f in range(995, 1061):
        write(os.path.join(show, "fx", f"trail.{f:04d}.bgeo"), b"%d" % f)
    host = comp_scene(tmp_path, extra_deps=[
        Dependency(os.path.join(show, "fx", "trail.####.bgeo"), JOB_FRAMES, node="Trail", handles=(2, 1))])
    host._renderables[0].frames = host._renderables[1].frames = "1001-1010"
    col = prepare(draft_for(host, farm)).collection
    got = sorted(int(r.split(".")[1]) for r in col.files if r.startswith("fx/"))
    assert got == list(range(999, 1012)), "two before, one after"
