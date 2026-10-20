"""Scripts for the tests, built in the fake nuke over real files."""

import os

import fakenuke as nk
from c4submit.testing import write

from c4_nuke.host import NukeHost


def comp_script(root, modified: bool = False):
    """The comp of c4submit's own tests, as a Nuke script: Write_bg comps
    the plate over the light render; Write_main reads bg's render back,
    grades it and puts the show LUT on it. A disabled Write is left over."""
    show = str(root)
    nk_path = write(os.path.join(show, "comp", "sm0140_comp_v004.nk"), b"# nuke script\n")
    for f in range(1001, 1049):
        write(os.path.join(show, "plates", f"sm0140.{f}.exr"), b"plate %d" % f)
        write(os.path.join(show, "render", "light", f"sm0140_beauty.{f:04d}.exr"), b"beauty %d" % f)
    write(os.path.join(show, "luts", "show.cube"), b"LUT")
    r = nk.reset(nk_path)
    r.setModified(modified)
    n = {}
    n["Read_plate"] = nk.read("Read_plate", os.path.join(show, "plates", "sm0140.####.exr"))
    n["Read_beauty"] = nk.read("Read_beauty", os.path.join(show, "render", "light", "sm0140_beauty.%04d.exr"))
    n["Merge1"] = nk.node("Merge2", "Merge1", n["Read_plate"], n["Read_beauty"])
    n["Write_bg"] = nk.write("Write_bg", os.path.join(show, "comp", "out", "bg.%04d.exr"), n["Merge1"])
    n["Read_bg"] = nk.read("Read_bg", os.path.join(show, "comp", "out", "bg.####.exr"))
    n["Grade1"] = nk.node("Grade", "Grade1", n["Read_bg"])
    n["OCIOFileTransform1"] = nk.node("OCIOFileTransform", "OCIOFileTransform1", n["Grade1"],
                                      file=os.path.join(show, "luts", "show.cube"))
    n["Write_main"] = nk.write("Write_main", os.path.join(show, "comp", "out", "main.####.exr"),
                               n["OCIOFileTransform1"], first=1001, last=1048)
    n["Write_old"] = nk.write("Write_old", os.path.join(show, "comp", "out", "old.####.exr"), n["Grade1"], disable=True)
    env = {"C4_PROJECT": "saltmarsh", "C4_SHOT": "sm0140", "C4_ROOT": show}
    return NukeHost(env=env), n
