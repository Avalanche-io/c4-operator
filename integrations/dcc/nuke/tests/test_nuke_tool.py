"""The nuke-render tool: its script's logic, run against the fake nuke,
and its tool.yaml against the panel's expectations."""

import json
import os
import sys

import fakenuke as fn
import pytest
import yaml
from c4submit.testing import write

TOOL = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "tools", "nuke-render")
sys.path.insert(0, TOOL)

import nuke_render as nr  # noqa: E402


# --- frames, path maps, files ---------------------------------------------------------------

def test_the_units_frames_from_the_farms_variables():
    assert nr.unit_frames({"C4_FRAME_START": "1001", "C4_FRAME_END": "1004"}) == [1001, 1002, 1003, 1004]
    assert nr.unit_frames({"C4_FRAME_START": "1001", "C4_FRAME_END": "1007", "C4_FRAME_STEP": "3"}) == [1001, 1004, 1007]
    assert nr.unit_frames({"C4_FRAMES": "1001,1004,1010-1012"}) == [1001, 1004, 1010, 1011, 1012]
    assert nr.unit_frames({"C4_FRAMES": "1001-1002"}) == [1001, 1002]
    assert nr.unit_frames({"C4_FRAME_START": "7"}) == [7]
    assert nr.parse_frames("1-9x4") == [1, 5, 9]
    with pytest.raises(ValueError):
        nr.parse_frames("1004-1001")


def test_frame_files_and_views():
    assert nr.frame_file("/show/comp/out/main.####.exr", 1001) == "/show/comp/out/main.1001.exr"
    assert nr.frame_file("/show/comp/out/main.%04d.exr", 7) == "/show/comp/out/main.0007.exr"
    assert nr.frame_file("/show/a.%d.exr", 7) == "/show/a.7.exr"
    assert nr.frame_file("/show/a.#.exr", 1001) == "/show/a.1001.exr", "padding is a minimum"
    assert nr.view_files("/show/%V/a_%v.exr", ["left", "right"]) == ["/show/left/a_l.exr", "/show/right/a_r.exr"]
    assert nr.view_files("/show/a.exr", ["left", "right"]) == ["/show/a.exr"]
    assert nr.absolute("out/a.exr", "/show/comp") == os.path.normpath("/show/comp/out/a.exr")
    assert nr.absolute("C:/show/a.exr", "/x") == "C:/show/a.exr"
    assert nr.as_bool("true") and not nr.as_bool("false") and not nr.as_bool("")


def test_the_path_map_maps_the_artists_root_to_the_tree(tmp_path):
    pm = write(str(tmp_path / "path-map.json"), json.dumps({"version": "pathmapping-1.0", "path_mapping_rules": [
        {"source_path_format": "WINDOWS", "source_path": "S:\\show", "destination_path": "/tree"},
        {"source_path_format": "POSIX", "source_path": "/Volumes/show", "destination_path": "/tree"},
        {"source_path_format": "POSIX", "source_path": "/Volumes/show/plates", "destination_path": "/plates"},
    ]}).encode())
    rules = nr.read_path_map(pm)
    assert nr.map_path("s:/Show/comp/a.####.exr", rules) == "/tree/comp/a.####.exr", "either slash, any case"
    assert nr.map_path("S:\\show\\plates\\p.exr", rules) == "/tree/plates/p.exr"
    assert nr.map_path("/Volumes/show/plates/p.exr", rules) == "/plates/p.exr", "the longest source wins"
    assert nr.map_path("/Volumes/showreel/a.exr", rules) == "/Volumes/showreel/a.exr", "only whole folders"
    assert nr.map_path("/tree/comp/a.exr", rules) == "/tree/comp/a.exr", "already the farm's"
    assert nr.read_path_map("") == []


# --- the render, in the fake nuke ------------------------------------------------------------

class Run:
    """nuke's terminal-mode calls the script makes, on the fake: opening a
    script, filename filters, and execute writing each frame's file."""

    def __init__(self, monkeypatch, fail_on=None, error="Write_main: something broke", writes=True):
        self.opened, self.filters, self.executed = [], [], []
        self.fail_on, self.error, self.writes = fail_on, error, writes
        monkeypatch.setattr(fn, "scriptOpen", self.open, raising=False)
        monkeypatch.setattr(fn, "addFilenameFilter", self.filters.append, raising=False)
        monkeypatch.setattr(fn, "execute", self.execute, raising=False)

    def open(self, path):
        if not os.path.isfile(path):
            raise RuntimeError(f"{path}: No such file")
        self.opened.append(path)

    def execute(self, node, first, last, incr):
        self.executed.append((node.fullName(), first, last, incr))
        if first == self.fail_on:
            raise RuntimeError(self.error)
        if self.writes:
            path = nr.frame_file(fn.filename(node), first)
            for f in self.filters:
                path = f(path)
            write(path, b"exr %d" % first)


def comp(tmp_path, out="comp/out/main.####.exr"):
    show = str(tmp_path)
    script = write(os.path.join(show, "comp", "sm0140_comp_v004.nk"), b"Root {}\n")
    fn.reset(script)
    fn.Node("Read", "Read_plate", file=os.path.join(show, "plates", "sm0140.####.exr"))
    fn.Node("Write", "Write_main", file=os.path.join(show, out) if out else "", disable=False)
    fn.Node("Write", "Write_bg", file=os.path.join(show, "comp/out/bg.####.exr"))
    return show, script


def lines_of(argv, env):
    out = []
    code = nr.main(argv, env, nuke=fn, say=out.append)
    return code, out


ENV = {"C4_FRAME_START": "1001", "C4_FRAME_END": "1003"}


def test_renders_each_frame_and_says_which_file(tmp_path, monkeypatch):
    show, script = comp(tmp_path)
    run = Run(monkeypatch)
    code, out = lines_of([script, "Write_main", "false"], ENV)
    assert code == 0, out
    assert run.opened == [script]
    assert run.executed == [("Write_main", 1001, 1001, 1), ("Write_main", 1002, 1002, 1), ("Write_main", 1003, 1003, 1)]
    frames = [l for l in out if l.startswith("c4_frame:")]
    assert frames == [f"c4_frame: {f} " + os.path.join(show, "comp", "out", f"main.{f}.exr") for f in (1001, 1002, 1003)]
    assert out[0] == "openjd_status: rendering Write_main, frames 1001-1003"
    assert [l for l in out if l.startswith("openjd_progress")][-1] == "openjd_progress: 100"
    assert run.filters == [], "no path map, no filter"


def test_a_write_inside_a_group_by_its_full_name(tmp_path, monkeypatch):
    show, script = comp(tmp_path)
    g = fn.Node("Group", "Group1")
    fn.Node("Write", "Write1", parent=g, file=os.path.join(show, "comp/out/g.####.exr"))
    Run(monkeypatch)
    code, out = lines_of([script, "Group1.Write1"], {"C4_FRAMES": "1001"})
    assert code == 0 and out[-2] == "c4_frame: 1001 " + os.path.join(show, "comp", "out", "g.1001.exr")


def test_permanent_failures_it_can_name(tmp_path, monkeypatch):
    show, script = comp(tmp_path)
    Run(monkeypatch)
    code, out = lines_of([os.path.join(show, "nope.nk"), "Write_main"], ENV)
    assert code == 2 and out[-1] == f"c4_fail: permanent no Nuke script at {os.path.join(show, 'nope.nk')}"
    code, out = lines_of([script, "Write_gone"], ENV)
    assert code == 2 and out[-1] == ("c4_fail: permanent no Write node 'Write_gone' in sm0140_comp_v004.nk; "
                                     "its Write nodes are Write_bg, Write_main")
    code, out = lines_of([script, "Read_plate"], ENV)
    assert code == 2 and "no Write node 'Read_plate'" in out[-1], "a Read is not a Write"
    fn.toNode("Write_main").knob("disable").setValue(True)
    code, out = lines_of([script, "Write_main"], ENV)
    assert code == 2 and out[-1] == "c4_fail: permanent the Write node Write_main is disabled in sm0140_comp_v004.nk"
    code, out = lines_of([script, "Write_bg"], {})
    assert code == 2 and "the unit has no frames" in out[-1]
    code, out = lines_of([script], ENV)
    assert code == 2 and "usage" in out[-1]


def test_a_missing_input_is_permanent_anything_else_is_retried(tmp_path, monkeypatch):
    _, script = comp(tmp_path)
    Run(monkeypatch, fail_on=1002, error="Read_plate: Read error: No such file or directory")
    code, out = lines_of([script, "Write_main"], ENV)
    assert code == 2 and out[-1].startswith("c4_fail: permanent Nuke failed on frame 1002: Read_plate: Read error")
    assert [l for l in out if l.startswith("c4_frame:")] == [out[1]], "frame 1001 was done before it failed"
    Run(monkeypatch, fail_on=1001, error="out of memory\nwhile rendering")
    code, out = lines_of([script, "Write_main"], ENV)
    assert code == 1 and out[-1] == "c4_fail: retry Nuke failed on frame 1001: out of memory while rendering"


def test_a_frame_nuke_did_not_write_is_retried(tmp_path, monkeypatch):
    show, script = comp(tmp_path)
    Run(monkeypatch, writes=False)
    code, out = lines_of([script, "Write_main"], ENV)
    assert code == 1
    assert out[-1] == "c4_fail: retry Nuke rendered frame 1001 but did not write " + os.path.join(
        show, "comp", "out", "main.1001.exr")


def test_proxy_mode_relative_outputs_and_stereo(tmp_path, monkeypatch):
    show, script = comp(tmp_path, out="")
    fn.toNode("Write_main").knob("file").setValue("out/%V/main.%04d.exr")
    fn.state().views = ["left", "right"]
    fn.root().add("proxy", False, kind=fn.Boolean_Knob)
    run = Run(monkeypatch, writes=False)

    def execute(node, first, last, incr):
        for v in ("left", "right"):
            write(os.path.join(show, "comp", "out", v, f"main.{first:04d}.exr"), b"exr")
    monkeypatch.setattr(fn, "execute", execute)
    code, out = lines_of([script, "Write_main", "true"], {"C4_FRAMES": "1001"})
    assert code == 0, out
    assert fn.root().knob("proxy").value() is True
    assert out[1] == "c4_frame: 1001 " + os.path.normpath(os.path.join(show, "comp", "out", "left", "main.1001.exr"))
    assert run.executed == []


def test_the_path_map_reaches_nukes_filename_filter(tmp_path, monkeypatch):
    tree = str(tmp_path / "tree")
    script = write(os.path.join(tree, "comp", "a.nk"), b"Root {}\n")
    fn.reset(script)
    fn.Node("Write", "Write_main", file="S:/show/comp/out/a.####.exr")
    pm = write(str(tmp_path / "pm.json"), json.dumps({"path_mapping_rules": [
        {"source_path_format": "WINDOWS", "source_path": "S:/show", "destination_path": tree}]}).encode())
    run = Run(monkeypatch)
    code, out = lines_of([script, "Write_main"], {"C4_FRAMES": "1001", "C4_PATH_MAP": pm})
    assert code == 0, out
    assert len(run.filters) == 1 and run.filters[0]("S:\\show\\plates\\p.exr") == tree + "/plates/p.exr"
    assert out[1] == f"c4_frame: 1001 {tree}/comp/out/a.1001.exr"


# --- the tool.yaml ----------------------------------------------------------------------------------

def test_the_tool_takes_what_the_panel_gives():
    with open(os.path.join(TOOL, "tool.yaml"), encoding="utf-8") as f:
        tool = yaml.safe_load(f)
    params = {p["name"]: p for p in tool["params"]}
    assert params["script"]["from"] == "scene.file" and params["script"]["type"] == "PATH"
    assert params["write"]["from"] == "scene.renderables", "the panel's renderable name is the Write's full name"
    assert "licences" not in tool.get("needs", {}), "the farm refuses tools that declare licences for now"
    assert "{{Tool.Dir}}/nuke_render.py" in tool["runs"]["command"]
    for k in tool["test"]["with"].values():
        assert not str(k).endswith(".nk") or os.path.isfile(os.path.join(TOOL, "test", k))
