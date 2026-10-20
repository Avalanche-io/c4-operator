import pytest

from c4submit import frames, seq
from c4submit.testing import write


@pytest.mark.parametrize("expr,want", [
    ("1001-1005", [1001, 1002, 1003, 1004, 1005]),
    ("1001-1009:4", [1001, 1005, 1009]),
    ("1001,1050-1052", [1001, 1050, 1051, 1052]),
    (" 7 , 3-4 ", [3, 4, 7]),
    ("-2-1", [-2, -1, 0, 1]),
    (1001, [1001]),
])
def test_parse(expr, want):
    assert frames.parse(expr) == want


@pytest.mark.parametrize("bad", ["", "a-b", "10-1", "1-10:0", "1-", True, "1;2"])
def test_parse_refuses(bad):
    with pytest.raises(frames.FrameError):
        frames.parse(bad)


@pytest.mark.parametrize("fs,want", [
    ([1001, 1002, 1003], "1001-1003"),
    ([1, 3, 5, 7], "1-7:2"),
    ([1, 3], "1,3"),
    ([5], "5"),
    ([1, 2, 3, 10, 20, 30, 31], "1-3,10-30:10,31"),
])
def test_format(fs, want):
    assert frames.format(fs) == want
    assert frames.parse(want) == sorted(fs)


def test_span():
    assert frames.span(1001, 1048) == "1001-1048"
    assert frames.span(1, 1) == "1"
    assert frames.span(10, 1, 2) == "1-10:2"


@pytest.mark.parametrize("pattern", ["a.####.exr", "a.%04d.exr", "a.$F4.exr", "a.${F4}.exr", "a.<F4>.exr"])
def test_frame_tokens(pattern):
    assert seq.has_frames(pattern)
    assert seq.at_frame(pattern, 7) == "a.0007.exr"
    assert seq.frame_of(pattern, "a.1001.exr") == 1001
    assert seq.frame_of(pattern, "b.1001.exr") is None
    assert seq.canonical(pattern) == "a.<F4>.exr"


def test_unpadded_and_negative():
    assert seq.at_frame("x.$F.exr", 12) == "x.12.exr"
    assert seq.at_frame("x.%04d.exr", -3) == "x.-0003.exr"
    assert not seq.has_frames("$FOO/x.exr")
    assert seq.at_frame("$FOO/x.$F2.exr", 5) == "$FOO/x.05.exr"


def test_expand(tmp_path):
    for f in (1001, 1002, 1004):
        write(str(tmp_path / f"plates/p.{f}.exr"))
    write(str(tmp_path / "plates/p.notes.txt"))
    pat = str(tmp_path / "plates/p.####.exr")
    found, missing = seq.expand(pat, [1001, 1002, 1003, 1004])
    assert [p[-13:] for p in found] == ["/p.1001.exr", "/p.1002.exr", "/p.1004.exr"] or len(found) == 3
    assert missing == [1003]
    every, none_missing = seq.expand(pat)
    assert len(every) == 3 and none_missing == []
    single = str(tmp_path / "plates/p.notes.txt")
    assert seq.expand(single) == ([single], [])
    assert seq.expand(str(tmp_path / "nope.exr")) == ([], [])


def test_expand_tiles_and_glob_characters(tmp_path):
    for tile in (1001, 1002, 1011):
        write(str(tmp_path / f"tex [v2]/wood.{tile}.tx"))
    write(str(tmp_path / "tex [v2]/wood.notes.tx"))
    found, _ = seq.expand(str(tmp_path / "tex [v2]/wood.<UDIM>.tx"))
    assert len(found) == 3
    for f in (1, 2):
        for tile in (1001, 1002):
            write(str(tmp_path / f"anim/t.{tile}.{f:04d}.tx"))
    found, missing = seq.expand(str(tmp_path / "anim/t.<UDIM>.####.tx"), [1, 2, 3])
    assert len(found) == 4 and missing == [3]
