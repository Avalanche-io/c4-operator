"""The project and its queue as c4submit reads them."""

import os

import fakeunreal as ue
import pytest
from unreal_projects import saltmarsh

from c4_unreal.host import UnrealHost, package_of, soft_path


def rel(proj, deps):
    return sorted(os.path.relpath(d.path, proj).replace(os.sep, "/") for d in deps)


def test_each_enabled_job_is_a_renderable(tmp_path):
    show, proj, queue = saltmarsh(tmp_path)
    h = UnrealHost(queue)
    rs = h.renderables()
    assert [(r.name, r.key, r.frames) for r in rs] == [
        ("sh010_lighting", "sh010_lighting", "1001-1010"),   # the custom range ends before its end frame
        ("sh020_lighting", "sh020_lighting", "1001-1024")]   # as does the sequence's playback range
    assert rs[0].values == {"map": "/Game/Maps/Sh010", "sequence": "/Game/Cinematics/sh010_seq.sh010_seq",
                            "config": "C4/queue/01-sh010-lighting.utxt"}
    assert rs[1].values["config"] == "C4/queue/02-sh020-lighting.utxt"
    assert h.frames() == "1001-1010" and h.read()[0] == "1001-1010"
    assert h.scene_file() == os.path.join(proj, "Saltmarsh.uproject") and h.scene_word == "project"


def test_jobs_with_the_same_name_and_no_sequence(tmp_path):
    show, proj, queue = saltmarsh(tmp_path)
    queue.jobs[1].job_name = "sh010_lighting"
    queue.jobs.append(ue.MoviePipelineExecutorJob("empty", "/Game/Maps/Sh020.Sh020", ""))
    rs = UnrealHost(queue).renderables()
    assert [r.name for r in rs] == ["sh010_lighting", "sh010_lighting (2)", "empty"]
    assert rs[2].frames is None and rs[2].values["sequence"] == ""


def test_the_shot_from_a_jobs_sequence(tmp_path, monkeypatch):
    show, proj, queue = saltmarsh(tmp_path)
    monkeypatch.setenv("C4_SHOT_PATTERN", r"/(?P<shot>sh\d{3})_")
    assert UnrealHost(queue).shot() == "sh010", "a project holds many shots; the first job's sequence names one"
    monkeypatch.setenv("C4_SHOT", "sh099")
    assert UnrealHost(queue).shot() == "sh099"


def test_unsaved_packages_and_saving(tmp_path):
    show, proj, queue = saltmarsh(tmp_path)
    h = UnrealHost(queue)
    assert not h.is_modified()
    ue.state.dirty = ["/Game/Maps/Sh010", "/Game/Env/Rock"]
    assert h.is_modified() and h.signature().endswith("|/Game/Env/Rock,/Game/Maps/Sh010")
    h.save()
    assert ue.state.save_dialogs == 1 and not h.is_modified(), "Unreal's own Save dialog"
    with open(os.path.join(proj, "C4", "queue", "01-sh010-lighting.utxt")) as f:
        manifest = f.read()
    assert "Name=sh010_lighting" in manifest and "sh020" not in manifest, "one job per manifest"
    assert os.path.isfile(os.path.join(proj, "C4", "queue", "02-sh020-lighting.utxt"))
    assert not os.path.exists(os.path.join(proj, "C4", "queue", "03-old-test.utxt")), "not the disabled job"
    ue.state.dirty = ["/Game/Env/Rock"]
    ue.state.refuse_save = True
    with pytest.raises(RuntimeError, match="were not saved"):
        h.save()


def test_all_of_content_and_never_this_machines_folders(tmp_path):
    show, proj, queue = saltmarsh(tmp_path)
    deps = UnrealHost(queue).dependencies()
    assert rel(proj, deps) == [
        "C4/queue/01-sh010-lighting.utxt", "C4/queue/02-sh020-lighting.utxt",
        "Config/DefaultEditor.ini", "Config/DefaultEngine.ini",
        "Content/Blueprints/BP_Mode.uasset", "Content/Cinematics/sh010_seq.uasset", "Content/Cinematics/sh020_seq.uasset",
        "Content/Env/Rock.uasset", "Content/Env/Rock.ubulk", "Content/Env/Unused.uasset",
        "Content/Maps/Sh010.umap", "Content/Maps/Sh010_BuiltData.uasset", "Content/Maps/Sh020.umap",
        "Content/Movies/intro.mp4", "Content/__ExternalActors__/Maps/Sh010/0/AB/KJ2.uasset",
        "Plugins/SaltTools/Content/Props/Lamp.uasset", "Plugins/SaltTools/Resources/Icon128.png",
        "Plugins/SaltTools/SaltTools.uplugin",
    ], "not Saved, Intermediate, DerivedDataCache, Binaries (the plug-in's too), .git or hidden files"
    by = {os.path.basename(d.path): d for d in deps}
    assert by["01-sh010-lighting.utxt"].renderables == ["sh010_lighting"]
    assert by["Rock.uasset"].renderables is None


def test_only_what_the_jobs_reference(tmp_path, monkeypatch):
    show, proj, queue = saltmarsh(tmp_path)
    monkeypatch.setenv("C4_UNREAL_CONTENT", "referenced")
    deps = UnrealHost(queue).dependencies()
    got = rel(proj, [d for d in deps if not d.optional])
    assert "Content/Env/Unused.uasset" not in got, "nothing reads it"
    for want in ("Content/Maps/Sh010.umap", "Content/Maps/Sh010_BuiltData.uasset", "Content/Env/Rock.uasset",
                 "Content/Env/Rock.ubulk", "Content/__ExternalActors__/Maps/Sh010/0/AB/KJ2.uasset",
                 "Plugins/SaltTools/Content/Props/Lamp.uasset", "Content/Blueprints/BP_Mode.uasset",
                 "Content/Movies/intro.mp4", "Plugins/SaltTools/SaltTools.uplugin", "Config/DefaultEngine.ini"):
        assert want in got
    assert got.count("Plugins/SaltTools/Content/Props/Lamp.uasset") == 1
    by = {d.path: d for d in deps}
    lamp = by[os.path.join(proj, "Plugins", "SaltTools", "Content", "Props", "Lamp.uasset")]
    assert lamp.node == "/SaltTools/Props/Lamp" and lamp.renderables == ["sh010_lighting"], \
        "a World Partition actor's plug-in prop, for the job whose map holds it"
    rock = by[os.path.join(proj, "Content", "Env", "Rock.uasset")]
    assert rock.renderables == ["sh010_lighting", "sh020_lighting"], "the config's default map is Sh010, for every job"
    gone = [d for d in deps if d.optional]
    assert [(d.node, d.path.endswith("Content/Missing/Gone.uasset")) for d in gone] == [("/Game/Missing/Gone", True)]
    asked = ue.state.registry_asked
    assert "/Engine/BasicShapes/Cube" not in asked and "/Script/Engine" not in asked, "the farm's Unreal has those"


def test_a_registry_that_fails_sends_all_of_content(tmp_path, monkeypatch):
    show, proj, queue = saltmarsh(tmp_path)
    monkeypatch.setenv("C4_UNREAL_CONTENT", "referenced")
    ue.state.registry_fails = True
    h = UnrealHost(queue)
    deps = h.dependencies()
    assert "Content/Env/Unused.uasset" in rel(proj, deps)
    assert h.warnings() == ["the asset registry could not list what the jobs read (the asset registry is still "
                            "loading), so all of Content is sent"]
    assert any(lvl == "warning" and "all of Content is sent" in m for lvl, m in ue.state.logs)
    ue.state.registry_fails = False
    h.dependencies()
    assert h.warnings() == [], "asked again, the registry answered"


def test_warnings_for_code_and_an_unknown_content_mode(tmp_path, monkeypatch):
    show, proj, queue = saltmarsh(tmp_path)
    h = UnrealHost(queue)
    h.dependencies()
    assert h.warnings() == [], "a project of content alone"
    with open(os.path.join(proj, "Saltmarsh.uproject"), "w") as f:
        f.write('{"FileVersion": 3, "Modules": [{"Name": "Saltmarsh", "Type": "Runtime"}]}')
    with open(os.path.join(proj, "Plugins", "SaltTools", "SaltTools.uplugin"), "w") as f:
        f.write('{"Modules": [{"Name": "SaltTools"}]}')
    monkeypatch.setenv("C4_UNREAL_CONTENT", "some")
    h.dependencies()
    assert h.warnings() == [
        "Saltmarsh, SaltTools have C++ code; Binaries are never sent, so the farm's Unreal must build it or have it "
        "already",
        'C4_UNREAL_CONTENT is "some", which is neither "all" nor "referenced"; all of Content is sent']


def test_paths():
    assert package_of("/Game/Maps/Sh010.Sh010") == "/Game/Maps/Sh010"
    assert package_of("/Game/Maps/Sh010") == "/Game/Maps/Sh010"
    assert soft_path(ue.SoftObjectPath("")) == "" and soft_path(None) == ""
    assert soft_path(ue.SoftObjectPath("/Game/A.A")) == "/Game/A.A"
