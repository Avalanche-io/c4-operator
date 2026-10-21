"""The maya-render tool: its script's logic, with a fake Render, and its
tool.yaml against what the window gives."""

import io
import json
import os
import sys

import pytest
import yaml
from c4submit.testing import write

TOOL = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "tools", "maya-render")
sys.path.insert(0, TOOL)

import maya_render as mr  # noqa: E402


# --- frames, commands, paths -------------------------------------------------------------------

def test_the_units_frames_and_its_runs():
    assert mr.unit_frames({"C4_FRAME_START": "1001", "C4_FRAME_END": "1004"}) == [1001, 1002, 1003, 1004]
    assert mr.unit_frames({"C4_FRAMES": "1001,1004,1010-1012"}) == [1001, 1004, 1010, 1011, 1012]
    assert mr.runs([1001, 1002, 1003, 1004]) == [(1001, 1004, 1)]
    assert mr.runs([1001, 1003, 1005]) == [(1001, 1005, 2)]
    assert mr.runs([1001, 1004, 1010, 1011, 1012]) == [(1001, 1004, 3), (1010, 1012, 1)]
    assert mr.runs([7]) == [(7, 7, 1)]
    with pytest.raises(ValueError):
        mr.unit_frames({"C4_FRAMES": "a-b"})


def test_the_render_command():
    cmd = mr.render_command("/maya/bin/Render", "/show/maya/scenes/a.ma", "/show/maya", "rs_beauty", "shotCam",
                            "arnold", (1001, 1004, 1))
    assert cmd == ["/maya/bin/Render", "-r", "arnold", "-proj", "/show/maya", "-s", "1001", "-e", "1004", "-b", "1",
                   "-rl", "rs_beauty", "-cam", "shotCam", "-ai:lve", "2", "/show/maya/scenes/a.ma"]
    cmd = mr.render_command("Render", "/s/a.mb", "/s", "defaultRenderLayer", "", "file", (1, 9, 2), "dirmap -en true;")
    assert cmd == ["Render", "-r", "file", "-proj", "/s", "-s", "1", "-e", "9", "-b", "2", "-rl", "defaultRenderLayer",
                   "-preRender", "dirmap -en true;", "/s/a.mb"], "no camera: the layer's own; file: the scene's renderer"


def test_the_scenes_project(tmp_path):
    show = str(tmp_path)
    scene = write(os.path.join(show, "seq", "sm0140", "maya", "scenes", "light", "a.ma"), b"//Maya ASCII")
    assert mr.workspace_of(scene) == os.path.dirname(scene), "no workspace.mel anywhere: its folder"
    write(os.path.join(show, "seq", "sm0140", "maya", "workspace.mel"), b"")
    assert mr.workspace_of(scene) == os.path.join(show, "seq", "sm0140", "maya")
    other = write(os.path.join(show, "x", "scenes", "b.ma"), b"")
    assert mr.workspace_of(other) == os.path.join(show, "x"), "above scenes/"


def test_render_beside_mayapy(tmp_path):
    bin_ = str(tmp_path / "bin")
    mayapy = write(os.path.join(bin_, "mayapy"), b"")
    assert mr.render_exe(mayapy, which=lambda n: None) is None
    assert mr.render_exe(mayapy, which=lambda n: "/usr/bin/" + n) == "/usr/bin/Render"
    render = write(os.path.join(bin_, "Render"), b"")
    assert mr.render_exe(mayapy) == render


def test_the_path_map_as_dirmap(tmp_path):
    pm = write(str(tmp_path / "pm.json"), json.dumps({"version": "pathmapping-1.0", "path_mapping_rules": [
        {"source_path_format": "WINDOWS", "source_path": "S:\\show", "destination_path": "/tree"},
        {"source_path_format": "POSIX", "source_path": "/Volumes/show", "destination_path": "/tree"}]}).encode())
    rules = mr.read_path_map(pm)
    assert mr.dirmap_mel(rules) == ('dirmap -en true; dirmap -m "/Volumes/show" "/tree"; '
                                    'dirmap -m "S:/show" "/tree"; dirmap -m "S:\\\\show" "/tree";'), \
        "a Windows root with either slash, the backslash escaped for MEL"
    assert mr.dirmap_mel([]) == ""


def test_maya_prefs_when_home_cannot_be_written(tmp_path):
    assert mr.app_dir_env({"HOME": str(tmp_path)}) == {}
    assert mr.app_dir_env({"HOME": "/nonexistent/home"}, mkdtemp=lambda: "/tmp/m") == {"MAYA_APP_DIR": "/tmp/m"}
    assert mr.app_dir_env({"HOME": "/nonexistent", "MAYA_APP_DIR": "/prefs"}) == {}


def test_image_paths_in_log_lines_and_their_frames():
    assert mr.image_paths("00:00:12  420MB | [driver_exr] writing file `/show/maya/images/beauty/a.1001.exr'") == [
        "/show/maya/images/beauty/a.1001.exr"]
    assert mr.image_paths("Saved file 'C:\\show\\images\\a.1002.exr' in 0.2s") == ["C:\\show\\images\\a.1002.exr"]
    assert mr.image_paths('Successfully written image file "/show/img/a_1003.png"') == ["/show/img/a_1003.png"]
    assert mr.image_paths("Rendering frame 1001 of /show/maya/scenes/a.ma") == []
    assert mr.frame_of("/show/images/beauty/sm0140_beauty.1001.exr") == 1001
    assert mr.frame_of("a_0007.png") == 7 and mr.frame_of("still.exr") is None


# --- the render, with a fake Render --------------------------------------------------------------

class FakeRender:
    """Render as a subprocess: writes each frame's images and logs lines,
    as a list of what to say per frame, or fails."""

    def __init__(self, images, log=None, code=0, write_frames=None):
        self.calls, self.images, self.log, self.code = [], images, log, code
        self.write_frames = write_frames

    def __call__(self, cmd, stdout=None, stderr=None, env=None, cwd=None, **kw):
        self.calls.append({"cmd": cmd, "env": env, "cwd": cwd})
        start, end, step = int(cmd[cmd.index("-s") + 1]), int(cmd[cmd.index("-e") + 1]), int(cmd[cmd.index("-b") + 1])
        lines = []
        for f in range(start, end + 1, step):
            if self.write_frames is None or f in self.write_frames:
                for pattern in self.images:
                    p = pattern % f
                    write(p, b"img %d" % f)
                    if self.log:
                        lines.append(self.log % p)
        lines.append("Result: done")
        return _Proc(lines, self.code)


class _Proc:
    def __init__(self, lines, code):
        self.stdout = io.StringIO("".join(line + "\n" for line in lines))
        self.code = code

    def wait(self):
        return self.code


def scene(tmp_path):
    show = str(tmp_path)
    ws = os.path.join(show, "seq", "sm0140", "maya")
    write(os.path.join(ws, "workspace.mel"), b"")
    s = write(os.path.join(ws, "scenes", "sm0140_light_v003.ma"), b"//Maya ASCII 2025 scene\n")
    mayapy = write(os.path.join(show, "maya", "bin", "mayapy"), b"")
    write(os.path.join(show, "maya", "bin", "Render"), b"")
    return show, ws, s, mayapy


def run(argv, env, mayapy, render):
    out = []
    code = mr.main(argv, env, mayapy=mayapy, say=out.append, popen=render)
    return code, out


def test_each_frame_from_the_log_as_it_finishes(tmp_path):
    show, ws, s, mayapy = scene(tmp_path)
    beauty = os.path.join(ws, "images", "beauty", "sm0140_light_v003_beauty.%d.exr")
    render = FakeRender([beauty], log="00:00:12 | [driver_exr] writing file `%s'")
    env = {"C4_FRAME_START": "1001", "C4_FRAME_END": "1003", "C4_TREE": show, "HOME": show}
    code, out = run([s, "--layer=rs_beauty", "--camera=shotCam", "--renderer=arnold"], env, mayapy, render)
    assert code == 0, out
    assert render.calls[0]["cmd"][:2] == [os.path.join(show, "maya", "bin", "Render"), "-r"]
    assert render.calls[0]["cwd"] == ws and "-ai:lve" in render.calls[0]["cmd"]
    frames = [l for l in out if l.startswith("c4_frame:")]
    assert frames == [f"c4_frame: {f} {beauty % f}" for f in (1001, 1002, 1003)]
    first = out.index(frames[0])
    assert out[first - 1].endswith(beauty % 1002 + "'"), "1001 is reported once 1002 starts writing"
    assert out[0] == "openjd_status: rendering rs_beauty with arnold, frames 1001-1003"
    assert out[-1] == "openjd_progress: 100"


def test_frames_found_under_the_tree_when_the_log_names_none(tmp_path):
    show, ws, s, mayapy = scene(tmp_path)
    old = write(os.path.join(ws, "sourceimages", "caustic.1001.png"), b"texture")
    os.utime(old, (1, 1))
    img = os.path.join(ws, "images", "test.%04d.png")
    render = FakeRender([img, os.path.join(ws, "images", "z_depth.%04d.png")])
    env = {"C4_FRAMES": "1,2", "C4_TREE": show, "HOME": show}
    code, out = run([s, "--layer=defaultRenderLayer", "--renderer=sw"], env, mayapy, render)
    assert code == 0, out
    assert [l for l in out if l.startswith("c4_frame:")] == [f"c4_frame: 1 {img % 1}", f"c4_frame: 2 {img % 2}"]
    assert "-cam" not in render.calls[0]["cmd"], "no camera given: the layer's own"


def test_a_frame_with_no_image_is_retried_and_a_failed_render_too(tmp_path):
    show, ws, s, mayapy = scene(tmp_path)
    img = os.path.join(ws, "images", "a.%04d.exr")
    env = {"C4_FRAME_START": "1", "C4_FRAME_END": "3", "C4_TREE": show, "HOME": show}
    code, out = run([s, "--layer=rs_a"], env, mayapy, FakeRender([img], write_frames={1, 2}))
    assert code == 1 and out[-1] == f"c4_fail: retry Render exited 0 but wrote no image for frame 3 under {show}"
    assert [l for l in out if l.startswith("c4_frame:")] == [f"c4_frame: 1 {img % 1}", f"c4_frame: 2 {img % 2}"]
    code, out = run([s, "--layer=rs_a"], env, mayapy, FakeRender([img], code=211))
    assert code == 1 and out[-1] == "c4_fail: retry Render exited 211 on frames 1-3"


def test_a_list_of_frames_is_one_render_per_run(tmp_path):
    show, ws, s, mayapy = scene(tmp_path)
    render = FakeRender([os.path.join(ws, "images", "a.%04d.exr")])
    env = {"C4_FRAMES": "1001,1004,1010-1011", "C4_TREE": show, "HOME": show}
    code, out = run([s, "--layer=rs_a"], env, mayapy, render)
    assert code == 0, out
    ranges = [(c["cmd"][c["cmd"].index("-s") + 1], c["cmd"][c["cmd"].index("-e") + 1], c["cmd"][c["cmd"].index("-b") + 1])
              for c in render.calls]
    assert ranges == [("1001", "1004", "3"), ("1010", "1011", "1")]


def test_permanent_failures_it_can_name(tmp_path, monkeypatch):
    show, ws, s, mayapy = scene(tmp_path)
    env = {"C4_FRAMES": "1", "HOME": show}
    code, out = run([os.path.join(ws, "nope.ma"), "--layer=rs_a"], env, mayapy, FakeRender([]))
    assert code == 2 and out[-1] == f"c4_fail: permanent no Maya scene at {os.path.join(ws, 'nope.ma')}"
    txt = write(os.path.join(ws, "notes.txt"), b"")
    code, out = run([txt], env, mayapy, FakeRender([]))
    assert code == 2 and out[-1] == "c4_fail: permanent notes.txt is not a Maya scene (.ma or .mb)"
    code, out = run([s], {}, mayapy, FakeRender([]))
    assert code == 2 and "the unit has no frames" in out[-1]
    lone = write(str(tmp_path / "elsewhere" / "mayapy"), b"")
    monkeypatch.setattr(mr.shutil, "which", lambda name: None)
    code, out = run([s], env, lone, FakeRender([]))
    assert code == 2 and out[-1] == (f"c4_fail: permanent no Render beside {lone} or on PATH; "
                                     "run this tool on an image with Maya")
    code, out = run([], env, mayapy, FakeRender([]))
    assert code == 2 and out[-1].startswith("c4_fail: permanent usage")


def test_the_path_map_reaches_render(tmp_path):
    show, ws, s, mayapy = scene(tmp_path)
    pm = write(str(tmp_path / "pm.json"), json.dumps({"path_mapping_rules": [
        {"source_path_format": "POSIX", "source_path": "/Volumes/show", "destination_path": show}]}).encode())
    render = FakeRender([os.path.join(ws, "images", "a.%04d.exr")])
    env = {"C4_FRAMES": "1", "C4_TREE": show, "HOME": show, "C4_PATH_MAP": pm}
    code, out = run([s, "--layer=rs_a"], env, mayapy, render)
    assert code == 0, out
    cmd = render.calls[0]["cmd"]
    assert cmd[cmd.index("-preRender") + 1] == f'dirmap -en true; dirmap -m "/Volumes/show" "{show}";'


# --- the tool.yaml ----------------------------------------------------------------------------------

def test_the_tool_takes_what_the_window_gives():
    with open(os.path.join(TOOL, "tool.yaml"), encoding="utf-8") as f:
        tool = yaml.safe_load(f)
    params = {p["name"]: p for p in tool["params"]}
    assert params["scene"]["from"] == "scene.file"
    assert params["layer"]["from"] == "scene.renderables", "the window's renderable name is the layer's node"
    assert params["camera"]["from"] == "scene.camera", "each layer gives its camera as scene.camera"
    assert "licences" not in tool.get("needs", {}), "the farm refuses tools that declare licences for now"
    assert "--camera={{Param.camera}}" in tool["runs"]["command"], "one argument, dropped whole when empty"
    assert os.path.isfile(os.path.join(TOOL, "test", tool["test"]["with"]["scene"]))
    assert os.path.isfile(os.path.join(TOOL, "test", "maya", "workspace.mel"))
