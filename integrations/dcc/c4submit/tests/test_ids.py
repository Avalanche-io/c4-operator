import os
import shutil
import subprocess

import pytest

from c4submit.c4id import identify, identify_file, is_id
from c4submit.tree import Tree, UnsupportedName, natural_compare

EMPTY = "c459dsjfscH38cYeXXYogktxf4Cd9ibshE3BHUo6a58hBXmRQdZrAkZzsWcbWtDg5oQstpDuni4Hirj75GEmTc1sFT"
FOO = "c45xZeXwMSpqXjpDumcHMA6mhoAmGHkUo7r9WmN2UgSEQzj9KjgseaQdkEJ11fGb5S1WEENcV3q8RFWwEeVpC7Fjk2"

HERE = os.path.dirname(__file__)


def _operator():
    """bin/c4-operator of this checkout, or of the main checkout when this
    is a worktree (bin/ is not tracked), or C4_OPERATOR_BIN."""
    repo = os.path.normpath(os.path.join(HERE, "..", "..", "..", ".."))
    cands = [os.environ.get("C4_OPERATOR_BIN", ""), os.path.join(repo, "bin", "c4-operator")]
    try:
        common = subprocess.run(["git", "-C", repo, "rev-parse", "--git-common-dir"], capture_output=True,
                                text=True).stdout.strip()
        if common:
            cands.append(os.path.join(repo, common, "..", "bin", "c4-operator"))
    except OSError:
        pass
    return next((c for c in cands if c and os.access(c, os.X_OK)), "")


OPERATOR = _operator()


def test_identify_matches_the_c4_cli(tmp_path):
    assert identify(b"") == EMPTY
    assert identify(b"foo") == FOO
    assert identify("foo") == FOO
    p = tmp_path / "foo"
    p.write_bytes(b"foo")
    assert identify_file(str(p)) == FOO
    assert is_id(FOO) and not is_id(FOO[:-1]) and not is_id("c4" + "0" * 88)


def test_natural_order():
    names = ["render.10.exr", "render.2.exr", "render.02.exr", "a", "a1", "b", "10", "9", "x9y", "x10y"]
    import functools
    got = sorted(names, key=functools.cmp_to_key(natural_compare))
    assert got == ["a", "a1", "b", "render.2.exr", "render.02.exr", "render.10.exr", "x9y", "x10y", "9", "10"]


def _scan(root):
    """A Tree of a folder on disk, the way the operator's scanner reads it."""
    t = Tree()
    for d, dirs, files in os.walk(root):
        rel = os.path.relpath(d, root).replace(os.sep, "/")
        for n in dirs:
            p = os.path.join(d, n)
            r = n if rel == "." else f"{rel}/{n}"
            if os.path.islink(p):
                t.add_link(r, os.readlink(p))
            elif not os.listdir(p):
                t.add_dir(r)
        for n in files:
            p = os.path.join(d, n)
            r = n if rel == "." else f"{rel}/{n}"
            if os.path.islink(p):
                t.add_link(r, os.readlink(p))
            else:
                t.add_file(r, identify_file(p), os.path.getsize(p), bool(os.stat(p).st_mode & 0o111))
    return t


def _sample(root):
    def w(rel, data, mode=None):
        p = root / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(data)
        if mode:
            p.chmod(mode)
    w("f.txt", b"hello")
    w("zero", b"")
    w("a/b/render.10.exr", b"x" * 1000)
    w("a/b/render.2.exr", b"y")
    w("a/render.02.exr", b"z")
    w("run.sh", b"#!/bin/sh\n", 0o755)
    w('sp ace[1]/x/we "ird.txt', b'q"')
    w("lnk/k", b"k")
    (root / "empty").mkdir()
    os.symlink("../f.txt", root / "lnk" / "l")
    w("nolink/inner/m", b"m")
    os.symlink("../../f.txt", root / "nolink" / "inner" / "link")


# The operator's ID for _sample, from `c4-operator tree-id` at c4-operator
# 06ae785. Recorded so the test holds where the binary is not built.
SAMPLE_ID = "c43HWnLTuDZzzS4Kuh7VtzCncqUeuTZqRVnLsVHBchALVt8FjE9N5ijuHJevj3omChjWPo9mPpSUY4XqcUugp1fWDh"


def test_tree_id_is_the_operators(tmp_path):
    _sample(tmp_path)
    assert _scan(tmp_path).id() == SAMPLE_ID


@pytest.mark.skipif(not OPERATOR, reason="bin/c4-operator is not built")
def test_tree_id_against_the_binary(tmp_path):
    _sample(tmp_path)
    for extra in ([], ["deep/er/still/1.exr"], ["01", "1", "001.txt"]):
        for rel in extra:
            p = tmp_path / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(rel.encode())
        out = subprocess.run([OPERATOR, "tree-id", str(tmp_path)], capture_output=True, text=True, check=True)
        assert _scan(tmp_path).id() == out.stdout.strip()


def test_empty_tree_and_view_request():
    t = Tree()
    assert t.id() == EMPTY
    t.add_file("plates/a.1001.exr", FOO, 3)
    t.add_file("comp/sm0140.nk", EMPTY, 0, True)
    t.add_dir("render")
    body = t.view_request()
    assert body["files"][0] == {"path": "comp/sm0140.nk", "id": EMPTY, "size": 0, "exec": True}
    assert body["dirs"] == ["render/"]


@pytest.mark.parametrize("bad", ["/abs", "a//b", "a/../b", "./a", ""])
def test_tree_paths_are_clean(bad):
    with pytest.raises(ValueError):
        Tree().add_file(bad, FOO, 3)


def test_names_the_local_id_cannot_encode():
    with pytest.raises(UnsupportedName):
        Tree().add_file("a\\b", FOO, 3)
    with pytest.raises(ValueError):
        t = Tree()
        t.add_file("a", FOO, 3)
        t.add_file("a/b", FOO, 3)


@pytest.mark.skipif(shutil.which("c4") is None, reason="the c4 CLI is not installed")
def test_identify_against_the_cli(tmp_path):
    p = tmp_path / "blob"
    p.write_bytes(os.urandom(3 << 20))
    out = subprocess.run(["c4", str(p)], capture_output=True, text=True, check=True).stdout.split()
    assert identify_file(str(p)) == out[-1]
