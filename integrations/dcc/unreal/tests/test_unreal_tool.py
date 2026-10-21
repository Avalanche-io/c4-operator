"""The unreal-render tool the farm runs: its wrapper's arguments, frames,
command line, the job's manifest patched for a unit, Unreal's log and
output files read back as frames, a whole run against a stand-in for
UnrealEditor-Cmd, and the executor filling the shipped tool.yaml's fields."""

import copy
import importlib.util
import os
import re
import stat
import sys

import fakeunreal as ue
import pytest
from c4submit import yamlio
from c4submit.testing import SALTMARSH, FakeFarm
from unreal_projects import env, saltmarsh

from c4_unreal import executor
from c4_unreal.executor import C4FarmExecutor

TOOL = os.path.normpath(os.path.join(os.path.dirname(__file__), "..", "tools", "unreal-render"))


def _wrapper():
    spec = importlib.util.spec_from_file_location("unreal_render_wrapper", os.path.join(TOOL, "unreal_render.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


W = _wrapper()


def tool_yaml() -> dict:
    with open(os.path.join(TOOL, "tool.yaml"), encoding="utf-8") as f:
        return yamlio.load(f.read())


#: A one-job queue manifest in the shape Unreal's export text is taken to
#: have (declarations first, then each object's properties in a block of its
#: name). No manifest from a real Unreal is in the repository; CHECKLIST.md
#: says how to check this against one.
MANIFEST = """\
Begin Object Class=/Script/MovieRenderPipelineCore.MoviePipelineQueue Name="MoviePipelineQueue_0" ExportPath="/Script/MovieRenderPipelineCore.MoviePipelineQueue'/Engine/Transient.MoviePipelineQueue_0'"
   Begin Object Class=/Script/MovieRenderPipelineCore.MoviePipelineExecutorJob Name="MoviePipelineExecutorJob_0"
      Begin Object Class=/Script/MovieRenderPipelineCore.MoviePipelinePrimaryConfig Name="MoviePipelinePrimaryConfig_0"
         Begin Object Class=/Script/MovieRenderPipelineCore.MoviePipelineOutputSetting Name="MoviePipelineOutputSetting_0"
         End Object
         Begin Object Class=/Script/MovieRenderPipelineCore.MoviePipelineImageSequenceOutput_EXR Name="MoviePipelineImageSequenceOutput_EXR_0"
         End Object
      End Object
   End Object
   Begin Object Name="MoviePipelineExecutorJob_0"
      Begin Object Name="MoviePipelinePrimaryConfig_0"
         Begin Object Name="MoviePipelineOutputSetting_0"
            OutputDirectory=(Path="{project_dir}/Saved/MovieRenders/")
            FileNameFormat="{sequence_name}.{frame_number}"
            bUseCustomPlaybackRange=True
            CustomStartFrame=1001
            CustomEndFrame=1011
         End Object
         Begin Object Name="MoviePipelineImageSequenceOutput_EXR_0"
            OutputDirectory=(Path="untouched")
         End Object
         Settings(0)="/Script/MovieRenderPipelineCore.MoviePipelineOutputSetting'MoviePipelineOutputSetting_0'"
      End Object
      JobName="sh010_lighting"
      Sequence="/Game/Cinematics/sh010_seq.sh010_seq"
      Map="/Game/Maps/Sh010.Sh010"
   End Object
   Jobs(0)="/Script/MovieRenderPipelineCore.MoviePipelineExecutorJob'MoviePipelineExecutorJob_0'"
End Object
"""


def output_block(text: str, name: str = "MoviePipelineOutputSetting_0") -> list:
    lines = text.splitlines()
    i = max(n for n, ln in enumerate(lines) if ln.strip().startswith(f'Begin Object Name="{name}"'))
    j = next(n for n in range(i + 1, len(lines)) if lines[n].strip() == "End Object")
    return [ln.strip() for ln in lines[i + 1:j]]


# --- arguments ------------------------------------------------------------------

def test_args():
    assert W.parse_args(["/show/unreal/Saltmarsh/Saltmarsh.uproject", "map=/Game/Maps/Sh010",
                         "sequence=/Game/Cinematics/sh010_seq.sh010_seq", "config=C4/queue/01-sh010-lighting.utxt"]) == {
        "project": "/show/unreal/Saltmarsh/Saltmarsh.uproject", "map": "/Game/Maps/Sh010",
        "sequence": "/Game/Cinematics/sh010_seq.sh010_seq", "config": "C4/queue/01-sh010-lighting.utxt"}
    assert W.parse_args(["P.uproject", "map=", "config=c.utxt"])["map"] == "", "an empty map is kept, not dropped"


@pytest.mark.parametrize("argv,says", [
    ([], "needs the project"),
    (["Saltmarsh.blend", "config=c.utxt"], "Saltmarsh.blend is not an Unreal project"),
    (["P.uproject", "map=/Game/M"], "no config"),
    (["P.uproject", "config=c.utxt", "level=x"], "not 'level=x'"),
])
def test_bad_args_are_permanent(argv, says):
    with pytest.raises(W.Refusal) as e:
        W.parse_args(argv)
    assert e.value.kind == "permanent" and says in e.value.reason


def test_frames_and_the_range_movie_render_queue_takes():
    assert W.unit_frames({"C4_FRAME_START": "1001", "C4_FRAME_END": "1010"}) == list(range(1001, 1011))
    assert W.unit_frames({"C4_FRAMES": "1001-1005:2"}) == [1001, 1003, 1005]
    assert W.mrq_range(range(1001, 1011)) == (1001, 1011, 1), "the end is one past the last frame"
    assert W.mrq_range([1001, 1003, 1005]) == (1001, 1006, 2)
    assert W.mrq_range([1001]) == (1001, 1002, 1)
    with pytest.raises(W.Refusal, match="one range of frames, not 1001...1012 with gaps"):
        W.mrq_range([1001, 1002, 1010, 1012])
    with pytest.raises(W.Refusal, match="no frames"):
        W.unit_frames({})


def test_config_and_job_names():
    assert W.resolve_config("C4/queue/01-sh010-lighting.utxt", "/show/unreal/Saltmarsh/Saltmarsh.uproject") == \
        os.path.normpath("/show/unreal/Saltmarsh/C4/queue/01-sh010-lighting.utxt")
    assert W.resolve_config("/elsewhere/q.utxt", "/show/P/P.uproject") == os.path.normpath("/elsewhere/q.utxt")
    assert W.job_name("C4/queue/01-sh010-lighting.utxt") == "sh010-lighting"
    assert W.job_name("C4/queue/12-sh010.utxt") == "sh010" and W.job_name("q/01.utxt") == "01"


def test_the_editor_beside_unreals_python():
    assert W.editor_for("/home/ue4/UnrealEngine/Engine/Binaries/ThirdParty/Python3/Linux/bin/python3", "linux") == \
        "/home/ue4/UnrealEngine/Engine/Binaries/Linux/UnrealEditor-Cmd"
    assert W.editor_for("C:\\Program Files\\Epic Games\\UE_5.5\\Engine\\Binaries\\ThirdParty\\Python3\\Win64\\python.exe",
                        "win32") == "C:\\Program Files\\Epic Games\\UE_5.5\\Engine\\Binaries\\Win64\\UnrealEditor-Cmd.exe"
    assert W.editor_for("C:/Program Files/Epic Games/UE_5.5/Engine/Binaries/ThirdParty/Python3/Win64/python.exe",
                        "win32") == "C:/Program Files/Epic Games/UE_5.5/Engine/Binaries/Win64/UnrealEditor-Cmd.exe"
    assert W.editor_for("/Users/Shared/Epic Games/UE_5.5/Engine/Binaries/ThirdParty/Python3/Mac/bin/python3",
                        "darwin") == "/Users/Shared/Epic Games/UE_5.5/Engine/Binaries/Mac/UnrealEditor-Cmd"
    with pytest.raises(W.Refusal, match="set UNREAL_EDITOR"):
        W.editor_for("/usr/bin/python3", "linux")


def test_the_command_line():
    cmd = W.command("/ue/UnrealEditor-Cmd", "/show/P/P.uproject", "MovieRenderPipeline/C4/x.utxt")
    assert cmd[:4] == ["/ue/UnrealEditor-Cmd", "/show/P/P.uproject",
                       "MoviePipelineEntryMap?game=/Script/MovieRenderPipelineCore.MoviePipelineGameMode", "-game"]
    assert "-MoviePipelineConfig=MovieRenderPipeline/C4/x.utxt" in cmd
    assert {"-RenderOffscreen", "-Unattended", "-windowed", "-log", "-StdOut"} <= set(cmd)


# --- the manifest ---------------------------------------------------------------------

def test_the_units_frames_and_folder_replace_the_jobs():
    text = W.set_output(MANIFEST, 1001, 1003, 1, "/show/render/sh010-lighting")
    assert output_block(text) == [
        'OutputDirectory=(Path="/show/render/sh010-lighting/")',
        'FileNameFormat="{sequence_name}.{frame_number}"',
        "bUseCustomPlaybackRange=True", "CustomStartFrame=1001", "CustomEndFrame=1003", "OutputFrameStep=1"]
    assert output_block(text, "MoviePipelineImageSequenceOutput_EXR_0") == ['OutputDirectory=(Path="untouched")'], \
        "another object's property of the same name is not the output setting's"
    assert text.endswith("End Object\n") and text.count("\n") == MANIFEST.count("\n") + 1
    assert "            OutputFrameStep=1\n         End Object" in text, "added at the block's own indent"


def test_a_job_left_at_its_defaults_gets_every_property():
    bare = re.sub(r'^\s*(OutputDirectory|bUseCustomPlaybackRange|CustomStartFrame|CustomEndFrame)=.*\n', "",
                  MANIFEST, flags=re.M)
    assert "CustomStartFrame" not in bare and "OutputDirectory" not in bare
    text = W.set_output(bare, 1001, 1049, 2, "C:\\renders\\sh020")
    assert sorted(output_block(text)) == sorted([
        'FileNameFormat="{sequence_name}.{frame_number}"', "bUseCustomPlaybackRange=True", "CustomStartFrame=1001",
        "CustomEndFrame=1049", "OutputFrameStep=2", 'OutputDirectory=(Path="C:/renders/sh020/")'])


def test_only_a_declaration_takes_the_properties():
    one = "\n".join([
        'Begin Object Class=/Script/MovieRenderPipelineCore.MoviePipelineQueue Name="Q"',
        '   Begin Object Class=/Script/MovieRenderPipelineCore.MoviePipelineOutputSetting Name="Out"',
        '   End Object',
        'End Object', ""])
    text = W.set_output(one, 5, 9, 1, "/t/render/j")
    assert "      CustomStartFrame=5\n" in text and "      CustomEndFrame=9\n" in text


def test_a_graph_config_is_refused():
    graph = MANIFEST.replace("MoviePipelineOutputSetting", "MovieGraphConfig")
    with pytest.raises(W.Refusal) as e:
        W.set_output(graph, 1001, 1003, 1, "/t")
    assert e.value.kind == "permanent" and "Movie Render Graph" in e.value.reason


def test_the_manifest_renders_the_jobs_map():
    assert W.renders_map(MANIFEST, "/Game/Maps/Sh010")
    assert not W.renders_map(MANIFEST, "/Game/Maps/Sh01"), "a prefix of the map is another map"
    assert not W.renders_map(MANIFEST, "/Game/Maps/Sh020")
    assert W.renders_map(MANIFEST, ""), "no map given: nothing to compare"


# --- what Unreal says and writes ---------------------------------------------------------

@pytest.mark.parametrize("line,pct", [
    ("LogMovieRenderPipeline: Display: [ 12.30s] Rendering progress: 45.5%", 45.5),
    ("LogMovieRenderPipeline: Display: Shot 1/1 frame 12/48", 25.0),
    ("LogMovieRenderPipeline: Display: Frame 48 of 48", 100.0),
    ("LogMovieRenderPipeline: Warning: 120% over budget", 100.0),
    ("LogShaderCompilers: Display: Compiling shaders: 45%", None),
    ("LogMovieRenderPipeline: Display: Finished rendering 1 shot", None),
    ("LogInit: Display: Engine is initialized.", None),
])
def test_progress_from_the_log(line, pct):
    assert W.progress_of(line) == pct


def test_frames_from_file_names():
    assert W.frame_of("sh010_seq.1001.exr") == 1001
    assert W.frame_of("sh010_seq.FinalImage.1001.exr") == 1001
    assert W.frame_of("v002/sh010_0042.png") == 42
    assert W.frame_of("sh010_seq.mov") is None and W.frame_of("notes.txt") is None
    assert W.frame_of("sh010_seq.exr") is None, "sh010 is a name, not frame 10"
    assert W.frame_of("1001.exr") == 1001


def test_a_frame_is_done_once_a_later_one_is_written():
    found = {1001: ["/r/a.1001.exr"], 1002: ["/r/a.1002.exr", "/r/b.1002.exr"], 1003: ["/r/a.1003.exr"]}
    told = set()
    assert W.finished(found, range(1001, 1006), told, final=False) == [(1001, "/r/a.1001.exr"), (1002, "/r/a.1002.exr")]
    told |= {1001, 1002}
    assert W.finished(found, range(1001, 1006), told, final=False) == [], "1003 may still be being written"
    assert W.finished(found, range(1001, 1006), told, final=True) == [(1003, "/r/a.1003.exr")]
    assert W.finished({999: ["/r/a.0999.exr"]}, range(1001, 1003), set(), final=True) == [], "not the unit's"


def test_on_disk(tmp_path):
    for p in ("FinalImage/sh.1001.exr", "FinalImage/sh.1002.exr", "Depth/sh.1001.exr", "sh.mov", "log.txt"):
        os.makedirs(os.path.dirname(str(tmp_path / p)), exist_ok=True)
        (tmp_path / p).write_bytes(b"x")
    got = W.on_disk(str(tmp_path))
    assert sorted(got) == [1001, 1002] and len(got[1001]) == 2


# --- a whole run, with a stand-in for UnrealEditor-Cmd --------------------------------------

FAKE_EDITOR = r'''
import os, re, sys
project, manifest = sys.argv[1], next(a.split("=", 1)[1] for a in sys.argv if a.startswith("-MoviePipelineConfig="))
text = open(os.path.join(os.path.dirname(project), "Saved", manifest)).read()
start, end = (int(re.search(k + r"=(\d+)", text).group(1)) for k in ("CustomStartFrame", "CustomEndFrame"))
folder = re.search(r'OutputDirectory=\(Path="([^"]*)"\)', text).group(1)
print("LogInit: Display: Engine is initialized.", flush=True)
for i, f in enumerate(range(start, end)):
    if os.environ.get("FAKE_SKIP") == str(f):
        continue
    open(os.path.join(folder, "sh010_seq.%04d.png" % f), "wb").close()
    print("LogMovieRenderPipeline: Display: frame %d/%d" % (i + 1, end - start), flush=True)
sys.exit(int(os.environ.get("FAKE_EXIT", "0")))
'''


def project(tmp_path):
    proj = tmp_path / "show" / "unreal" / "Saltmarsh"
    (proj / "C4" / "queue").mkdir(parents=True)
    (proj / "Saltmarsh.uproject").write_text("{}")
    (proj / "C4" / "queue" / "01-sh010-lighting.utxt").write_text(MANIFEST)
    editor = tmp_path / "UnrealEditor-Cmd"
    editor.write_text(f"#!{sys.executable}\n{FAKE_EDITOR}")
    editor.chmod(editor.stat().st_mode | stat.S_IEXEC)
    opts = {"project": str(proj / "Saltmarsh.uproject"), "map": "/Game/Maps/Sh010",
            "sequence": "/Game/Cinematics/sh010_seq.sh010_seq", "config": "C4/queue/01-sh010-lighting.utxt"}
    environ = {"C4_TREE": str(tmp_path / "show"), "C4_FRAME_START": "1001", "C4_FRAME_END": "1003",
               "UNREAL_EDITOR": str(editor), "PATH": os.environ.get("PATH", "")}
    return proj, opts, environ


needs_posix = pytest.mark.skipif(os.name == "nt", reason="the stand-in editor is a script with a #! line")


@needs_posix
def test_a_run_reports_each_frame(tmp_path, monkeypatch):
    monkeypatch.setattr(W, "POLL", 0.05)
    proj, opts, environ = project(tmp_path)
    lines = []
    assert W.run(opts, environ, out=lambda line, flush=False: lines.append(line)) == 0
    render = str(tmp_path / "show" / "render" / "sh010-lighting").replace("\\", "/")
    assert [ln for ln in lines if ln.startswith("c4_frame:")] == [
        f"c4_frame: {f} {render}/sh010_seq.{f}.png" for f in (1001, 1002, 1003)]
    assert lines[0] == "openjd_status: rendering sh010-lighting, frames 1001-1003, with Movie Render Queue"
    assert "LogInit: Display: Engine is initialized." in lines, "Unreal's log passes through"
    assert [ln for ln in lines if ln.startswith("openjd_progress")][-1] == "openjd_progress: 100.0"
    copied = (proj / "Saved" / "MovieRenderPipeline" / "C4" / "sh010-lighting-1001-1003.utxt").read_text()
    assert "CustomEndFrame=1004" in copied and (proj / "C4" / "queue" / "01-sh010-lighting.utxt").read_text() == MANIFEST


@needs_posix
def test_a_frame_unreal_did_not_write_is_a_retry(tmp_path):
    proj, opts, environ = project(tmp_path)
    environ["FAKE_SKIP"] = "1002"
    with pytest.raises(W.Refusal) as e:
        W.run(opts, environ, out=lambda *a, **k: None)
    assert e.value.kind == "retry" and "wrote no file for frame 1002" in e.value.reason


@needs_posix
def test_unreal_failing_is_a_retry(tmp_path):
    proj, opts, environ = project(tmp_path)
    environ["FAKE_EXIT"] = "3"
    with pytest.raises(W.Refusal) as e:
        W.run(opts, environ, out=lambda *a, **k: None)
    assert e.value.kind == "retry" and e.value.reason == "Unreal exited 3 with 3 of 3 frames written"


def test_what_the_run_refuses_before_starting_unreal(tmp_path):
    proj, opts, environ = project(tmp_path)
    with pytest.raises(W.Refusal, match="does not render /Game/Maps/Sh020"):
        W.run(dict(opts, map="/Game/Maps/Sh020"), environ)
    with pytest.raises(W.Refusal, match="no queue manifest at"):
        W.run(dict(opts, config="C4/queue/09-gone.utxt"), environ)
    with pytest.raises(W.Refusal, match="no Unreal project at"):
        W.run(dict(opts, project=str(proj / "Other.uproject")), environ)
    with pytest.raises(W.Refusal, match="no UnrealEditor-Cmd at"):
        W.run(opts, dict(environ, UNREAL_EDITOR=str(tmp_path / "nowhere" / "UnrealEditor-Cmd")))


# --- the shipped tool.yaml -------------------------------------------------------------

def test_the_shipped_tool():
    t = tool_yaml()
    assert t["kind"] == "frames" and t["frames"]["chunk"] == "all"
    assert t["runs"]["command"] == ["{{Run.Exe}}", "{{Tool.Dir}}/unreal_render.py", "{{Param.project}}",
                                    "map={{Param.map}}", "sequence={{Param.sequence}}", "config={{Param.config}}"]
    assert t["runs"]["image"] == "registry.example.com/unreal:5.5" and t["runs"]["host"]["has"] == ["unreal-5.5"]
    for exe in [t["runs"]["exe"]] + list(t["runs"]["host"]["exe"].values()):
        assert "/Engine/Binaries/ThirdParty/Python3/" in exe, "Unreal's own Python, so editor_for finds the editor"
    params = {p["name"]: p for p in t["params"]}
    assert {k: params[k]["from"] for k in ("project", "map", "sequence", "config")} == {
        "project": "scene.file", "map": "scene.renderables", "sequence": "scene.renderables",
        "config": "scene.renderables"}
    test = t["test"]
    proj = os.path.join(TOOL, test["tree"], test["with"]["project"])
    assert os.path.isfile(proj) and os.path.isfile(W.resolve_config(test["with"]["config"], proj))
    assert test["expect"][0].startswith(f"render/{W.job_name(test['with']['config'])}/")


def test_the_executor_fills_the_shipped_tool(tmp_path, monkeypatch):
    show, proj, queue = saltmarsh(tmp_path)
    env(monkeypatch, show)
    tool = tool_yaml()
    # c4submit's fake farm counts units with a numeric chunk only; the
    # farm's own reading of chunk: all is one unit per job.
    tool["frames"]["chunk"] = 100000
    fake = FakeFarm(tools=[tool], projects={"saltmarsh": dict(copy.deepcopy(SALTMARSH), root="/show")})
    monkeypatch.setattr(executor, "farm_factory", lambda: fake)
    ue.state.answers = ["yes", "ok"]
    C4FarmExecutor().execute(queue)
    summary, _ = ue.state.messages()
    assert re.search(r"sh010-render-sh010-lighting-v001\s+Unreal render · 10 frames in 1 units?\b", summary), summary
    job = fake.jobs[0]["jobs"]["render-sh010-lighting"]
    assert job == {"tool": "unreal-render", "frames": "1001-1010", "with": {
        "project": "unreal/Saltmarsh/Saltmarsh.uproject", "map": "/Game/Maps/Sh010",
        "sequence": "/Game/Cinematics/sh010_seq.sh010_seq", "config": "C4/queue/01-sh010-lighting.utxt"}}
