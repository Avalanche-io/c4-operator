"""The katana-render tool: its script's logic, with a fake katana, and its
tool.yaml against what the farm plug-in gives."""

import gzip
import io
import json
import os
import sys

import pytest
import yaml
from c4submit.testing import write

TOOL = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "tools", "katana-render")
sys.path.insert(0, TOOL)

import katana_render as kr  # noqa: E402


# --- frames, commands, paths -------------------------------------------------------------------

def test_the_units_frames_and_katanas_t():
    assert kr.unit_frames({"C4_FRAME_START": "1001", "C4_FRAME_END": "1004"}) == [1001, 1002, 1003, 1004]
    assert kr.runs(kr.unit_frames({"C4_FRAMES": "1001,1004,1010-1012"})) == [(1001, 1004, 3), (1010, 1012, 1)]
    assert kr.katana_frames((1001, 1004, 1)) == "1001-1004"
    assert kr.katana_frames((1001, 1001, 1)) == "1001"
    assert kr.katana_frames((1001, 1007, 3)) == "1001,1004,1007"
    with pytest.raises(ValueError):
        kr.parse_frames("1001-")


def test_the_batch_command():
    assert kr.batch_command("/opt/katana/katana", "/show/l.katana", "Render_beauty", (1001, 1004, 1)) == [
        "/opt/katana/katana", "--batch", "--katana-file=/show/l.katana", "--render-node=Render_beauty", "-t",
        "1001-1004"]
    assert kr.batch_command("katana", "/s/l.katana", "R", (1, 1, 1), threads=8)[-1] == "--threads3d=8"
    assert kr.build_command("katana", "/t/build.py", "/tmp/p.katana") == ["katana", "--script=/t/build.py",
                                                                          "/tmp/p.katana"]


def test_katana_from_katana_root_or_path(tmp_path):
    exe = write(str(tmp_path / "Katana8.0v1" / "katana"), b"")
    assert kr.katana_exe({"KATANA_ROOT": os.path.dirname(exe)}) == exe
    assert kr.katana_exe({}, which=lambda n: "/usr/local/bin/" + n) == "/usr/local/bin/katana"
    assert kr.katana_exe({"KATANA_ROOT": "/nowhere"}, which=lambda n: None) is None


def test_the_path_map_on_a_projects_text():
    rules = [("WINDOWS", "S:\\show", "/tree"), ("POSIX", "/Volumes/show", "/tree")]
    text = ('<param value="S:\\show\\assets\\heron.abc"/><param value="s:/Show/tex/a.&lt;UDIM&gt;.tex"/>'
            '<param value="/Volumes/show/fx/fog.usd"/><param value="/Volumes/showreel/x.abc"/>'
            '<param value="S:/showreel/y.abc"/><param value="S:\\show"/>')
    assert kr.map_text(text, rules) == (
        '<param value="/tree/assets/heron.abc"/><param value="/tree/tex/a.&lt;UDIM&gt;.tex"/>'
        '<param value="/tree/fx/fog.usd"/><param value="/Volumes/showreel/x.abc"/>'
        '<param value="S:/showreel/y.abc"/><param value="/tree"/>'), "only whole folders, either slash, any case"


def test_a_mapped_copy_beside_the_project(tmp_path):
    rules = [("POSIX", "/Volumes/show", str(tmp_path))]
    plain = write(str(tmp_path / "l.katana"), b'<katana><p value="/Volumes/show/a.abc"/></katana>')
    copy = kr.mapped_copy(plain, rules)
    assert copy == str(tmp_path / ".c4-mapped-l.katana")
    assert open(copy, encoding="utf-8").read() == f'<katana><p value="{tmp_path}/a.abc"/></katana>'
    zipped = write(str(tmp_path / "z.katana"), gzip.compress(b'<p value="/Volumes/show/b.abc"/>'))
    assert gzip.decompress(open(kr.mapped_copy(zipped, rules), "rb").read()) == f'<p value="{tmp_path}/b.abc"/>'.encode()
    same = write(str(tmp_path / "s.katana"), b'<p value="/elsewhere/c.abc"/>')
    assert kr.mapped_copy(same, rules) is None, "nothing to map, no copy"


def test_image_paths_in_log_lines_and_their_frames():
    assert kr.image_paths("[INFO]: Copying /tmp/katana/render.1001.exr to /show/render/beauty.1001.exr") == [
        "/tmp/katana/render.1001.exr", "/show/render/beauty.1001.exr"]
    assert kr.image_paths("Render of Render_beauty complete") == []
    assert kr.frame_of("/show/render/beauty.1001.exr") == 1001


# --- the render, with a fake katana ---------------------------------------------------------------

class FakeKatana:
    """katana as a subprocess: a batch render writes each frame's images
    and logs a line per image; script mode saves a project."""

    def __init__(self, images, log="[INFO]: Render output %s", code=0, write_frames=None, build=True):
        self.calls, self.images, self.log, self.code = [], images, log, code
        self.write_frames, self.build = write_frames, build

    def __call__(self, cmd, stdout=None, stderr=None, env=None, cwd=None, **kw):
        self.calls.append({"cmd": cmd, "cwd": cwd})
        if cmd[1].startswith("--script="):
            if self.build:
                write(cmd[2], b"<katana/>")
            return _Proc([f"Running {cmd[1][9:]}"], 0)
        lines = []
        for f in kr.parse_frames(cmd[cmd.index("-t") + 1]):
            if self.write_frames is None or f in self.write_frames:
                for pattern in self.images:
                    write(pattern % f, b"img %d" % f)
                    if self.log:
                        lines.append(self.log % (pattern % f))
        return _Proc(lines, self.code)


class _Proc:
    def __init__(self, lines, code):
        self.stdout = io.StringIO("".join(line + "\n" for line in lines))
        self.code = code

    def wait(self):
        return self.code


def project(tmp_path):
    show = str(tmp_path)
    p = write(os.path.join(show, "seq", "sm0140", "katana", "sm0140_light_v002.katana"), b"<katana/>")
    katana = write(os.path.join(show, "opt", "katana", "katana"), b"")
    env = {"KATANA_ROOT": os.path.dirname(katana), "C4_TREE": show, "C4_FRAME_START": "1001", "C4_FRAME_END": "1003"}
    return show, p, katana, env


def run(argv, env, fake):
    out = []
    code = kr.main(argv, env, say=out.append, popen=fake)
    return code, out


def test_each_frame_as_it_finishes(tmp_path):
    show, p, katana, env = project(tmp_path)
    beauty = os.path.join(show, "render", "sm0140", "beauty", "beauty.%04d.exr")
    fake = FakeKatana([beauty])
    code, out = run([p, "--node=Render_beauty", "--threads=0"], env, fake)
    assert code == 0, out
    assert fake.calls[0]["cmd"] == [katana, "--batch", f"--katana-file={p}", "--render-node=Render_beauty", "-t",
                                    "1001-1003"]
    assert [l for l in out if l.startswith("c4_frame:")] == [f"c4_frame: {f} {beauty % f}" for f in (1001, 1002, 1003)]
    assert out.index(f"c4_frame: 1001 {beauty % 1001}") > out.index(f"[INFO]: Render output {beauty % 1002}"), \
        "1001 is reported once 1002 starts"
    assert out[0] == "openjd_status: rendering Render_beauty, frames 1001-1003" and out[-1] == "openjd_progress: 100"


def test_frames_found_under_the_tree_when_the_log_names_none(tmp_path):
    show, p, _, env = project(tmp_path)
    shadow = os.path.join(show, "render", "sm0140", "shadow", "shadow.%04d.exr")
    code, out = run([p, "--node=Render_shadow"], env, FakeKatana([shadow], log=None))
    assert code == 0, out
    assert [l for l in out if l.startswith("c4_frame:")] == [f"c4_frame: {f} {shadow % f}" for f in (1001, 1002, 1003)]


def test_a_frame_with_no_image_and_a_failed_batch_are_retried(tmp_path):
    show, p, _, env = project(tmp_path)
    img = os.path.join(show, "render", "a.%04d.exr")
    code, out = run([p, "--node=Render_a"], env, FakeKatana([img], write_frames={1001}))
    assert code == 1 and out[-1] == f"c4_fail: retry katana --batch exited 0 but wrote no image for frame 1002 under {show}"
    code, out = run([p, "--node=Render_a"], env, FakeKatana([img], code=3))
    assert code == 1 and out[-1] == "c4_fail: retry katana --batch exited 3 on frames 1001-1003"


def test_a_builder_script_makes_the_project_first(tmp_path):
    show, _, katana, env = project(tmp_path)
    build = write(os.path.join(show, "katana", "build_project.py"), b"# builds a project")
    fake = FakeKatana([os.path.join(show, "render", "test.%04d.exr")])
    code, out = run([build, "--node=Render_test"], dict(env, C4_FRAMES="1001-1002", C4_FRAME_START=""), fake)
    assert code == 0, out
    built = fake.calls[0]["cmd"][2]
    assert fake.calls[0]["cmd"][:2] == [katana, f"--script={build}"] and built.endswith("project.katana")
    assert fake.calls[1]["cmd"][2] == f"--katana-file={built}"
    assert not os.path.exists(built), "the built project is scratch, removed after"
    code, out = run([build, "--node=Render_test"], env, FakeKatana([], build=False))
    assert code == 2 and out[-1] == "c4_fail: permanent build_project.py did not build a project (exit 0)"


def test_the_path_map_renders_a_mapped_copy(tmp_path):
    show, p, _, env = project(tmp_path)
    write(p, b'<katana><p value="S:\\show\\anim\\heron.abc"/></katana>')
    pm = write(str(tmp_path / "pm.json"), json.dumps({"path_mapping_rules": [
        {"source_path_format": "WINDOWS", "source_path": "S:\\show", "destination_path": show}]}).encode())
    seen = []

    class Watching(FakeKatana):
        def __call__(self, cmd, **kw):
            f = cmd[2].split("=", 1)[1]
            seen.append((f, open(f, encoding="utf-8").read()))
            return super().__call__(cmd, **kw)

    code, out = run([p, "--node=Render_a"], dict(env, C4_PATH_MAP=pm),
                    Watching([os.path.join(show, "render", "a.%04d.exr")]))
    assert code == 0, out
    copy = os.path.join(os.path.dirname(p), ".c4-mapped-sm0140_light_v002.katana")
    assert seen == [(copy, f'<katana><p value="{show}/anim/heron.abc"/></katana>')]
    assert not os.path.exists(copy), "removed after the render"


def test_permanent_failures_it_can_name(tmp_path, monkeypatch):
    show, p, _, env = project(tmp_path)
    code, out = run([p], env, FakeKatana([]))
    assert code == 2 and out[-1] == "c4_fail: permanent no Render node given (--node)"
    code, out = run([os.path.join(show, "nope.katana"), "--node=R"], env, FakeKatana([]))
    assert code == 2 and out[-1] == f"c4_fail: permanent no Katana project at {os.path.join(show, 'nope.katana')}"
    txt = write(os.path.join(show, "a.txt"), b"")
    code, out = run([txt, "--node=R"], env, FakeKatana([]))
    assert code == 2 and out[-1] == "c4_fail: permanent a.txt is not a Katana project (.katana)"
    code, out = run([p, "--node=R"], {"KATANA_ROOT": env["KATANA_ROOT"]}, FakeKatana([]))
    assert code == 2 and "the unit has no frames" in out[-1]
    monkeypatch.setattr(kr.shutil, "which", lambda n: None)
    code, out = run([p, "--node=R"], dict(env, KATANA_ROOT="/nowhere"), FakeKatana([]))
    assert code == 2 and out[-1] == ("c4_fail: permanent no katana in $KATANA_ROOT or on PATH; "
                                     "run this tool on an image with Katana")
    code, out = run([p, "--node=R", "--threads=many"], env, FakeKatana([]))
    assert code == 2 and out[-1].startswith("c4_fail: permanent usage")


# --- the tool.yaml ----------------------------------------------------------------------------------

def test_the_tool_takes_what_the_plug_in_gives():
    with open(os.path.join(TOOL, "tool.yaml"), encoding="utf-8") as f:
        tool = yaml.safe_load(f)
    params = {p["name"]: p for p in tool["params"]}
    assert params["scene"]["from"] == "scene.file"
    assert params["node"]["from"] == "scene.renderables", "the plug-in's renderable name is the Render node's"
    assert "licences" not in tool.get("needs", {}), "the farm refuses tools that declare licences for now"
    assert os.path.isfile(os.path.join(TOOL, "test", tool["test"]["with"]["scene"]))
