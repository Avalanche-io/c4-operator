"""A scene from someone else must never send this machine's other files to
the farm: a link under the project folder that leads out of it is refused,
by where it leads, not by how its path is spelled."""

import os

import pytest

from c4submit import collect
from c4submit.host import Dependency
from c4submit.testing import write

pytestmark = pytest.mark.skipif(os.name == "nt", reason="symbolic links need privileges on Windows")


def _errors(col):
    return [p for p in col.problems if p.level == "error"]


def test_a_linked_file_leading_out_is_refused(tmp_path):
    show, home = tmp_path / "show", tmp_path / "home"
    secret = write(str(home / ".ssh" / "id_ed25519"), b"not for the farm")
    scene = write(str(show / "sm" / "comp.nk"))
    os.makedirs(show / "sm" / "tex")
    os.symlink(secret, show / "sm" / "tex" / "key.exr")
    col = collect.gather(str(show), scene, [Dependency(str(show / "sm" / "tex" / "key.exr"), node="Read_key")])
    errs = _errors(col)
    assert len(errs) == 1 and errs[0].path == "Read_key"
    assert errs[0].message == (f"{show}/sm/tex/key.exr is a link to {os.path.realpath(secret)}, "
                               f"outside the project folder {show}")
    assert "copy the file into the project folder" in errs[0].fix
    assert "sm/tex/key.exr" not in col.files, "nothing outside the project is gathered to be sent"


def test_a_linked_folder_on_the_way_is_refused(tmp_path):
    show, home = tmp_path / "show", tmp_path / "home"
    write(str(home / "Documents" / "plate.1001.exr"), b"x")
    scene = write(str(show / "comp.nk"))
    os.symlink(home / "Documents", show / "plates")
    col = collect.gather(str(show), scene, [Dependency(str(show / "plates" / "plate.####.exr"), "1001", node="Read1")])
    assert [e.path for e in _errors(col)] == ["Read1"] and "is a link to" in _errors(col)[0].message
    assert list(col.files) == ["comp.nk"]


def test_a_linked_scene_leading_out_is_refused(tmp_path):
    show, elsewhere = tmp_path / "show", tmp_path / "elsewhere"
    real = write(str(elsewhere / "comp.nk"))
    os.makedirs(show)
    os.symlink(real, show / "comp.nk")
    col = collect.gather(str(show), str(show / "comp.nk"), [])
    assert [e.path for e in _errors(col)] == ["scene"] and "is a link to" in _errors(col)[0].message


def test_links_inside_the_project_and_a_linked_root_still_work(tmp_path):
    volume = tmp_path / "Volumes" / "show"
    scene = write(str(volume / "sm" / "comp.nk"))
    write(str(volume / "lib" / "grain.exr"), b"g")
    os.symlink(volume / "lib" / "grain.exr", volume / "sm" / "grain.exr")  # a link that stays inside
    os.symlink(volume, tmp_path / "show")  # the project root itself is a link, as /show often is
    show = tmp_path / "show"
    col = collect.gather(str(show), str(show / "sm" / "comp.nk"), [Dependency(str(show / "sm" / "grain.exr"), node="Read_grain")])
    assert _errors(col) == [] and sorted(col.files) == ["sm/comp.nk", "sm/grain.exr"]
    del scene
