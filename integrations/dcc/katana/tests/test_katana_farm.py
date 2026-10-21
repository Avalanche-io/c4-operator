"""Startup/init.py and the farm menus: what KATANA_RESOURCES adds to Katana."""

import os
import runpy
import sys

import fakekatana as fk
from c4submit.testing import SALTMARSH, FakeFarm
from katana_scenes import KATANA_RENDER, light_project

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
INIT = os.path.join(ROOT, "Startup", "init.py")


def menus():
    return [n for n, _ in fk.state().menu], [n for n, _ in fk.state().popup]


def test_init_finds_c4submit_and_registers_once_katana_has_started(monkeypatch):
    src = os.path.normpath(os.path.join(ROOT, "..", "c4submit", "src"))
    monkeypatch.setattr(sys, "path", [p for p in sys.path if os.path.normpath(p) != src])
    monkeypatch.setitem(sys.modules, "c4submit", None)   # not importable yet
    runpy.run_path(INIT, run_name="__main__")
    assert src in sys.path, "c4submit is found beside the Katana folder"
    assert menus() == ([], []), "not before start-up completes"
    assert fk.fire("onStartupComplete") == 1
    assert menus() == (["Submit to C4..."], ["Submit this Render node to C4..."])
    from c4_katana import farm
    farm.register()
    assert menus() == (["Submit to C4..."], ["Submit this Render node to C4..."]), "once"


def setup(tmp_path, monkeypatch):
    host, _, shadow = light_project(tmp_path)
    for k, v in host.env.items():
        monkeypatch.setenv(k, v)
    from c4_katana import dialog, farm
    fake = FakeFarm(tools=[KATANA_RENDER], projects={"saltmarsh": SALTMARSH})
    monkeypatch.setattr(dialog, "Farm", lambda **kw: fake)
    farm.register()
    return dialog, shadow


def run(dialog, callback):
    import fakekatanaqt as qt
    d = callback()
    while d.working() or d._timer.isActive():
        qt.fire_timers()
    return d


def test_the_farm_menu_opens_the_dialog_with_every_render_node(tmp_path, monkeypatch):
    dialog, _ = setup(tmp_path, monkeypatch)
    d = run(dialog, fk.state().menu[0][1])
    assert d.shown == 1 and d.draft.ticked == ["Render_shadow", "Render_beauty"]


def test_a_render_nodes_farm_menu_ticks_only_it(tmp_path, monkeypatch):
    dialog, shadow = setup(tmp_path, monkeypatch)
    fk.state().clicked = shadow
    d = run(dialog, fk.state().popup[0][1])
    assert d.draft.ticked == ["Render_shadow"]


def test_without_a_clicked_node_the_selected_render_nodes(tmp_path, monkeypatch):
    dialog, _ = setup(tmp_path, monkeypatch)
    fk.state().nodes["Render_beauty"].selected = True
    fk.state().nodes["settings"].selected = True
    d = run(dialog, fk.state().popup[0][1])
    assert d.draft.ticked == ["Render_beauty"]
    for n in fk.state().nodes.values():
        n.selected = False
    d = run(dialog, fk.state().popup[0][1])
    assert d.draft.ticked == ["Render_shadow", "Render_beauty"], "nothing clicked or selected: every one"


def test_a_katana_without_start_up_callbacks_registers_at_once(monkeypatch):
    from c4_katana import farm
    monkeypatch.delattr(fk.Katana, "Callbacks")
    farm.install()
    assert menus() == (["Submit to C4..."], ["Submit this Render node to C4..."])
