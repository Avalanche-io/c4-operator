"""c4tool, the helper the tool scripts share: the source itself, loaded
under its own name so a tool folder's copy on sys.path is never what is
tested."""

import importlib.util
import io
import json
import os
import time

import pytest

SOURCE = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "c4tool.py")
_spec = importlib.util.spec_from_file_location("c4tool_source", SOURCE)
ct = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(ct)


def write(path, data=b"x"):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(data)
    return path


# --- frames ----------------------------------------------------------------------------------

def test_frame_lists_as_the_farm_writes_them():
    assert ct.parse_frames("1001") == [1001]
    assert ct.parse_frames("1001-1004") == [1001, 1002, 1003, 1004]
    assert ct.parse_frames("1001-1007:3") == [1001, 1004, 1007]
    assert ct.parse_frames("1-9x4") == [1, 5, 9]
    assert ct.parse_frames("1001, 1004,1003-1004") == [1001, 1004, 1003], "each once, in order"
    assert ct.parse_frames("-3--1") == [-3, -2, -1]
    assert ct.parse_frames("") == []
    for bad in ("1004-1001", "1-4:0", "a-b", "1-4:2:1"):
        with pytest.raises(ValueError):
            ct.parse_frames(bad)


def test_the_units_frames_from_the_farms_variables():
    assert ct.unit_frames({"C4_FRAME_START": "1001", "C4_FRAME_END": "1003"}) == [1001, 1002, 1003]
    assert ct.unit_frames({"C4_FRAME_START": "1", "C4_FRAME_END": "7", "C4_FRAME_STEP": "3"}) == [1, 4, 7]
    assert ct.unit_frames({"C4_FRAME_START": "7"}) == [7]
    assert ct.unit_frames({"C4_FRAMES": "1001,1004,1010-1012", "C4_FRAME_START": "1001"}) == [1001, 1004, 1010, 1011,
                                                                                            1012], "a list wins"
    assert ct.unit_frames({"C4_FRAMES": "1001-1002"}) == [1001, 1002]
    assert ct.unit_frames({}) == []


def test_runs_of_evenly_spaced_frames():
    assert ct.runs([1, 2, 3, 4]) == [(1, 4, 1)]
    assert ct.runs([1, 3, 5, 10, 11]) == [(1, 5, 2), (10, 11, 1)]
    assert ct.runs([7]) == [(7, 7, 1)]
    assert ct.runs([4, 1, 2, 2]) == [(1, 2, 1), (4, 4, 1)], "sorted, each once"
    assert ct.runs([]) == []


# --- the path map --------------------------------------------------------------------------

def test_the_path_map(tmp_path):
    pm = write(str(tmp_path / "path-map.json"), json.dumps({"version": "pathmapping-1.0", "path_mapping_rules": [
        {"source_path_format": "WINDOWS", "source_path": "S:\\show", "destination_path": "/tree"},
        {"source_path": "/Volumes/show", "destination_path": "/tree"},
        {"source_path_format": "POSIX", "source_path": "/Volumes/show/plates", "destination_path": "/plates"},
    ]}).encode())
    rules = ct.read_path_map(pm)
    assert rules[0] == ("POSIX", "/Volumes/show/plates", "/plates"), "longest source first; POSIX by default"
    assert ct.map_path("s:/Show/comp/a.####.exr", rules) == "/tree/comp/a.####.exr", "either slash, any case"
    assert ct.map_path("S:\\show", rules) == "/tree"
    assert ct.map_path("/Volumes/show/plates/p.exr", rules) == "/plates/p.exr"
    assert ct.map_path("/Volumes/showreel/a.exr", rules) == "/Volumes/showreel/a.exr", "only whole folders"
    assert ct.map_path("/tree/comp/a.exr", rules) == "/tree/comp/a.exr", "already the farm's"
    assert ct.map_path("", rules) == "" and ct.map_path("/a", []) == "/a"
    assert ct.read_path_map("") == []
    with pytest.raises(KeyError):
        ct.read_path_map(write(str(tmp_path / "bad.json"), b'{"path_mapping_rules": [{"source_path": "/a"}]}'))


# --- the lines a unit prints ---------------------------------------------------------------------

def test_failures_and_frames_as_lines():
    out = []
    assert ct.fail(out.append, "permanent", "no scene") == 2
    assert ct.fail(out.append, "retry", "out of memory") == 1
    ct.frame_done(out.append, 1001, "/tree/a.1001.exr", 1, 3)
    assert out == ["c4_fail: permanent no scene", "c4_fail: retry out of memory",
                   "c4_frame: 1001 /tree/a.1001.exr", "openjd_progress: 33"]


def test_say_prints_at_once(capsys):
    ct.say("openjd_status: rendering")
    assert capsys.readouterr().out == "openjd_status: rendering\n"


class _Proc:
    def __init__(self, lines, code):
        self.stdout, self.code = io.StringIO("".join(f"{x}\n" for x in lines)), code

    def wait(self):
        return self.code


def test_stream_passes_each_line_through():
    got, seen, out = {}, [], []

    def popen(cmd, **kw):
        got.update(kw, cmd=cmd)
        return _Proc(["one", "two"], 3)

    assert ct.stream(popen, ["render", "-x"], {"A": "1"}, "/work", out.append, seen.append) == 3
    assert out == ["one", "two"] and seen == ["one", "two"]
    assert got["cmd"] == ["render", "-x"] and got["env"] == {"A": "1"} and got["cwd"] == "/work"
    assert ct.stream(lambda cmd, **kw: _Proc([], 0), ["x"], {}, "/", out.append) == 0


# --- finished frames -------------------------------------------------------------------------

def test_images_in_log_lines_and_their_frames():
    im = ct.Images()
    assert im.paths("writing file `/show/images/beauty.0001.exr'") == ["/show/images/beauty.0001.exr"]
    assert im.paths('Saved file "C:\\show\\img\\a.1002.tif" in 3s') == ["C:\\show\\img\\a.1002.tif"]
    assert im.paths("reading /show/geo/a.abc") == []
    assert im.frame_of("/show/beauty_v002.1001.exr") == 1001
    assert im.frame_of("/show/beauty.exr") is None
    assert im.frame_of("/show/a.0007.deepexr") == 7
    katana = ct.Images((".exr",))
    assert katana.paths("wrote /show/a.0001.iff") == [] and im.paths("wrote /show/a.0001.iff") == ["/show/a.0001.iff"]


def test_new_images_under_the_tree(tmp_path):
    top = str(tmp_path)
    old = write(os.path.join(top, "images", "a.0001.exr"))
    os.utime(old, (1, 1))
    write(os.path.join(top, "images", "a.0002.exr"))
    write(os.path.join(top, "images", "a.0002.z.png"))
    write(os.path.join(top, "images", "a.0003.txt"))
    write(os.path.join(top, ".c4", "a.0004.exr"))
    found = ct.Images().new(top, time.time() - 60, [1, 2, 3, 4])
    assert found == {2: os.path.join(top, "images", "a.0002.exr")}, "new images only, first by name, not under .c4"
    assert ct.Images().new(os.path.join(top, "none"), 0, [1]) == {}


def test_finished_reports_a_frame_once_a_later_one_starts(tmp_path):
    out = []
    a, b = write(str(tmp_path / "a.0001.exr")), str(tmp_path / "a.0002.exr")
    done = ct.Finished([1, 2], time.time() - 60, out.append)
    done.saw_line(f"writing {a}")
    assert out == [], "frame 1 may still be being written"
    done.saw_line(f"writing {b}")
    assert out == [f"c4_frame: 1 {a}", "openjd_progress: 50"]
    assert done.finish({}) == [2], "frame 2 has no file"
    write(b)
    assert done.finish({}) == [] and out[-2:] == [f"c4_frame: 2 {b}", "openjd_progress: 100"]


def test_finished_ignores_old_files_and_takes_what_is_found_after(tmp_path):
    out = []
    old = write(str(tmp_path / "plate.0001.exr"))
    os.utime(old, (1, 1))
    done = ct.Finished([1, 2], time.time() - 60, out.append, ct.Images((".exr",)))
    done.saw(old)
    done.saw(str(tmp_path / "a.0002.exr"))
    assert out == [], "an old file is never a frame"
    new = write(str(tmp_path / "a.0001.exr"))
    assert done.finish({1: new}) == [2]
    assert out == [f"c4_frame: 1 {new}", "openjd_progress: 50"]
