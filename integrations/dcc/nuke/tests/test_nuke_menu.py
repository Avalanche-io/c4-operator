"""menu.py: what a studio's NUKE_PATH adds to Nuke."""

import os
import runpy
import sys

import fakenuke as nk
from scripts import comp_script
from c4submit.testing import FakeFarm, SALTMARSH

MENU = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "menu.py")


def test_menu_adds_both_entries(tmp_path, monkeypatch):
    src = os.path.normpath(os.path.join(os.path.dirname(MENU), "..", "c4submit", "src"))
    monkeypatch.setattr(sys, "path", [p for p in sys.path if os.path.normpath(p) != src])
    monkeypatch.setitem(sys.modules, "c4submit", None)   # not importable yet
    runpy.run_path(MENU, run_name="__main__")
    assert src in sys.path, "c4submit is found beside the Nuke folder"
    assert callable(nk.menu("Nuke").findItem("C4/Submit to farm..."))
    assert callable(nk.menu("Node Graph").findItem("C4/Submit this Write to C4"))


def test_menu_commands_open_the_panel(tmp_path, monkeypatch):
    host, n = comp_script(tmp_path)        # a new script, so menus are made after it
    runpy.run_path(MENU, run_name="__main__")
    monkeypatch.setenv("C4_PROJECT", "saltmarsh")
    monkeypatch.setenv("C4_SHOT", "sm0140")
    monkeypatch.setenv("C4_ROOT", str(tmp_path))
    from c4_nuke import panel
    farm = FakeFarm(projects={"saltmarsh": SALTMARSH})
    monkeypatch.setattr(panel, "Farm", lambda **kw: farm)
    n["Write_main"].selected = True
    nk.menu("Node Graph").findItem("C4/Submit this Write to C4")()
    p = panel._open[-1]
    nk.settle(p)
    assert p.shown == 1 and p.draft.ticked == ["Write_main"]
    nk.menu("Nuke").findItem("C4/Submit to farm...")()
    p = panel._open[-1]
    nk.settle(p)
    assert p.draft.ticked == ["Write_bg", "Write_main"]
