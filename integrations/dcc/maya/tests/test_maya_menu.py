"""userSetup.py and the C4 menu: what the module file adds to Maya."""

import os
import runpy
import sys

import fakemaya as fm
from c4submit.testing import SALTMARSH, FakeFarm
from maya_scenes import MAYA_RENDER, light_scene

SCRIPTS = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "scripts")
USER_SETUP = os.path.join(SCRIPTS, "userSetup.py")


def test_user_setup_finds_c4submit_and_adds_the_menu_once_the_ui_is_up(monkeypatch):
    src = os.path.normpath(os.path.join(SCRIPTS, "..", "..", "c4submit", "src"))
    monkeypatch.setattr(sys, "path", [p for p in sys.path if os.path.normpath(p) != src])
    monkeypatch.setitem(sys.modules, "c4submit", None)   # not importable yet
    runpy.run_path(USER_SETUP, run_name="__main__")
    assert src in sys.path, "c4submit is found beside the Maya folder"
    assert not fm.cmds.menu("c4Menu", exists=True), "not before Maya's main window exists"
    fm.run_main()
    menu = fm.control("c4Menu")
    assert menu.get("label") == "C4" and menu.parent == "MayaWindow"
    assert fm.control("c4SubmitItem").get("label") == "Submit to farm..."


def test_no_menu_in_batch_maya():
    fm.state().batch = True
    runpy.run_path(USER_SETUP, run_name="__main__")
    fm.run_main()
    assert not fm.cmds.menu("c4Menu", exists=True)


def test_adding_the_menu_again_replaces_it():
    from c4_maya import menu
    menu.add_menu()
    menu.add_menu()
    assert len([c for c in fm.state().ui.values() if c.short == "c4Menu"]) == 1


def test_the_menu_item_opens_the_window(tmp_path, monkeypatch):
    host, _ = light_scene(tmp_path)
    for k, v in host.env.items():
        monkeypatch.setenv(k, v)
    from c4_maya import menu, panel
    farm = FakeFarm(tools=[MAYA_RENDER], projects={"saltmarsh": SALTMARSH})
    monkeypatch.setattr(panel, "Farm", lambda **kw: farm)
    menu.add_menu()
    fm.control("c4SubmitItem").flags["command"](False)
    w = panel._open[-1]
    fm.settle(w)
    assert fm.control(panel.WINDOW).shown == 1
    assert w.draft is not None and w.draft.ticked == ["rs_beauty", "rs_fg"]
