"""TOP networks in the C4 Submit ROP: listed for a tool that cooks them
(pdg-cook), each carrying the node its cook cooks."""

import os

import fakehou as hou
from houdini_scenes import HOUDINI_ROP, env, farm, fx_scene

from c4submit import yamlio
from c4submit.testing import KARMA
from c4_houdini import nodes
from c4_houdini.host import RopHost

TOOLS = os.path.normpath(os.path.join(os.path.dirname(__file__), "..", "tools"))
SUBMIT = 0


def pdg_cook() -> dict:
    with open(os.path.join(TOOLS, "pdg-cook", "tool.yaml"), encoding="utf-8") as f:
        return yamlio.load(f.read())


def tops(submit):
    """Three TOP networks: /obj/tops (output node write), /obj/empty (no
    output node), and /out/wedges wired into the C4 Submit ROP."""
    net = hou.node("/obj").createNode("topnet", "tops")
    gen = net.createNode("genericgenerator", "gen")
    write = net.createNode("pythonscript", "write")
    write.setInput(0, gen)
    write.setDisplayFlag(True)
    hou.node("/obj").createNode("topnet", "empty")
    wedges = hou.node("/out").createNode("topnet", "wedges")
    wedges.createNode("genericgenerator", "wedge").setDisplayFlag(True)
    submit.setInput(1, wedges)
    return net, wedges


def test_tops_listed_for_a_tool_that_cooks_them(tmp_path):
    show, submit = fx_scene(tmp_path)
    tops(submit)
    host = RopHost(submit)
    assert [r.name for r in host.renderables()] == ["/out/sim", "/out/mantra1", "/out/flip", "/out/wedges"], \
        "for a ROP tool, ROPs (a TOP network in /out is one)"
    host.lists = "tops"
    rs = {r.name: r for r in host.renderables()}
    assert list(rs) == ["/obj/empty", "/obj/tops", "/out/wedges"], "every TOP network in the hip file, by path"
    assert [n for n, r in rs.items() if r.enabled] == ["/out/wedges"], "the one wired in is ticked"
    assert rs["/obj/tops"].values == {"top": "/obj/tops/write"} and rs["/obj/tops"].note == "cooks write"
    assert rs["/obj/tops"].key == "tops" and rs["/obj/tops"].label == "/obj/tops"
    assert rs["/obj/empty"].values == {} and "no output node" in rs["/obj/empty"].note
    assert rs["/obj/tops"].frames is None and rs["/obj/tops"].outputs == []


def test_the_only_network_is_ticked_when_none_is_wired(tmp_path):
    show, submit = fx_scene(tmp_path)
    net = hou.node("/obj").createNode("topnet", "tops")
    net.createNode("pythonscript", "write").setDisplayFlag(True)
    host = RopHost(submit)
    host.lists = "tops"
    assert [(r.name, r.enabled) for r in host.renderables()] == [("/obj/tops", True)]
    hou.node("/obj").createNode("topnet", "more")
    assert [r.enabled for r in host.renderables()] == [False, False], "with two, the artist picks"


def test_pdg_cook_submits_the_ticked_networks_without_frames(tmp_path, monkeypatch):
    show, submit = fx_scene(tmp_path, cache_on_disk=True)
    net, wedges = tops(submit)
    env(monkeypatch, show, "sm0140")
    fake = farm(tools=[KARMA, HOUDINI_ROP, pdg_cook()])
    monkeypatch.setattr(nodes, "farm_factory", lambda: fake)
    nodes.install(submit)
    assert submit.parm("c4_tool").eval() == "houdini-rop"
    menu = hou.run_menu(submit.parm("c4_tool"))
    assert "pdg-cook" in menu and "karma" not in menu, "both hip tools are offered"

    hou.change(submit.parm("c4_tool"), "pdg-cook")
    assert submit.parm("c4_renderables").eval() == "/out/wedges"
    labels = hou.run_menu(submit.parm("c4_renderables"))
    assert labels == ["/obj/empty", "/obj/empty  (no output node: set the display flag on the node to cook)",
                      "/obj/tops", "/obj/tops  (cooks write)", "/out/wedges", "/out/wedges  (cooks wedge)"]
    assert submit.parm("c4p_top").eval() == "/out/wedges/wedge", "the field shows the node the cook cooks"

    hou.change(submit.parm("c4_renderables"), "/obj/tops /out/wedges")
    hou.ui.answers = [SUBMIT]
    submit.parm("c4_submit").pressButton()
    panel = nodes.panel_for(submit)
    assert panel.checked.ok, panel.checked.errors
    (doc,) = fake.jobs
    assert doc["jobs"] == {
        "render-tops": {"tool": "pdg-cook", "with": {"hip": "sm0140/fx/sm0140_fx_v002.hip", "top": "/obj/tops/write"}},
        "render-wedges": {"tool": "pdg-cook", "with": {"hip": "sm0140/fx/sm0140_fx_v002.hip", "top": "/out/wedges/wedge"}},
    }, "a once tool: no frames"

    hou.change(submit.parm("c4_tool"), "houdini-rop")
    assert submit.parm("c4_renderables").eval().split() == ["/out/mantra1", "/out/flip", "/out/wedges"], \
        "back to ROPs for a ROP tool"
