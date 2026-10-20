"""Scenes on disk with the fake hou's nodes over them: a Solaris lighting
shot and an FX hip file with a ROP chain."""

import os

import fakehou as hou
from c4submit.testing import KARMA, SALTMARSH, FakeFarm, write

HOUDINI_ROP = {
    "c4": "tool/1", "name": "houdini-rop", "title": "Houdini ROP",
    # It renders the one ROP without its inputs (ignore_inputs=True in
    # hython): C4 Submit makes each ROP upstream its own job, run first.
    "summary": "Renders one ROP of a hip file with hython over frames, without its inputs.",
    "kind": "frames",
    "runs": {"image": "registry.petrelfinch.example/houdini:21.0.440", "exe": "hython",
             "command": ["{{Run.Exe}}", "-m", "c4_rop", "{{Param.hip}}", "{{Param.rop}}", "{{Unit.Frames}}"]},
    "frames": {"chunk": 4, "order": "binary"},
    "params": [
        {"name": "hip", "type": "PATH", "objectType": "FILE", "dataFlow": "IN", "from": "scene.file",
         "userInterface": {"control": "CHOOSE_INPUT_FILE", "label": "Hip file",
                           "fileFilters": [{"label": "Houdini", "patterns": ["*.hip", "*.hiplc", "*.hipnc"]}]}},
        {"name": "rop", "type": "STRING", "from": "scene.renderables", "userInterface": {"label": "ROP"},
         "description": "The ROP to render, by path; rendered with ignore_inputs=True."},
        {"name": "threads", "type": "INT", "default": 0, "minValue": 0, "maxValue": 128, "show": "advanced",
         "affectsOutput": False, "userInterface": {"label": "Threads"}},
        {"name": "verbose", "type": "BOOL", "default": False, "show": "advanced",
         "userInterface": {"label": "Verbose log"}},
    ],
    "needs": {"cpu": 16, "memory": "64G", "licences": {"sesi/houdini_engine": 1}},
    "reuse": "exact",
}

# Karma with more kinds of field: a toggle, a number, a group of its own.
KARMA_PLUS = dict(KARMA, params=KARMA["params"] + [
    {"name": "denoise", "type": "BOOL", "default": True, "userInterface": {"label": "Denoise", "groupLabel": "Quality"}},
    {"name": "samples", "type": "INT", "default": 64, "minValue": 1, "maxValue": 4096,
     "userInterface": {"label": "Pixel samples", "groupLabel": "Quality"}},
    {"name": "threads", "type": "INT", "default": 0, "minValue": 0, "show": "advanced"},
    {"name": "mplay", "type": "BOOL", "default": False, "show": "advanced", "userInterface": {"label": "Watch in MPlay"}},
    {"name": "checkpoint", "type": "PATH", "objectType": "DIRECTORY", "dataFlow": "OUT", "show": "advanced",
     "default": "render/checkpoints"},
    {"name": "secret", "type": "STRING", "default": "x", "show": "wrangler"},
])


def farm(tools=None) -> FakeFarm:
    return FakeFarm(tools=tools if tools is not None else [KARMA, HOUDINI_ROP],
                    projects={"saltmarsh": dict(SALTMARSH, root="/show")})


def env(monkeypatch, show: str, shot: str) -> None:
    monkeypatch.setenv("C4_PROJECT", "saltmarsh")
    monkeypatch.setenv("C4_ROOT", show)
    monkeypatch.setenv("C4_SHOT", shot)


def light_scene(root, default: str = "/Render/rendersettings", unresolved=()):
    """sm0220's lighting hip: a LOP whose stage has two render settings and
    reads a set layer, UDIM bark and an HDRI. Returns (show, stage LOP)."""
    show = os.path.join(str(root), "show")
    hip = write(os.path.join(show, "sm0220", "light", "sm0220_light_v004.hip"), b"HIP")
    hou.reset(hip=hip, JOB=show)
    layer = write(os.path.join(show, "assets", "set", "set.usdc"), b"set layer")
    for tile in (1001, 1002, 1011):
        write(os.path.join(show, "assets", "tree", "tex", f"bark.{tile}.exr"), b"bark %d" % tile)
    hdri = write(os.path.join(show, "lookdev", "hdri", "sky.exr"), b"sky")
    stage = hou.Stage(
        {"/Render": "Scope", "/Render/rendersettings": "RenderSettings", "/Render/beauty_only": "RenderSettings",
         "/Render/Products": "Scope", "/Render/Products/beauty": "RenderProduct", "/World": "Xform",
         "/World/tree": "Mesh"},
        {"renderSettingsPrimPath": default} if default else {},
        layers=[layer, "anon:0x7f3a:LOP:lighting"],
        assets=[os.path.join(show, "assets", "tree", "tex", "bark.<UDIM>.exr"), hdri,
                "/opt/hfs21.0/houdini/pic/Mandril.pic"],
        unresolved=list(unresolved))
    lop = hou.Node("/stage/karmarendersettings", "karmarenderproperties", "Lop", stage=stage)
    return show, lop


def c4render(lop):
    """A null LOP after the stage, to install the C4 interface on."""
    node = hou.node("/stage").createNode("null", "c4render1")
    node.setInput(0, lop)
    return node


def fx_scene(root, cache_on_disk: bool = False):
    """sm0140's FX hip: /out/sim caches the sim, /out/mantra1 renders it,
    /out/flip is a flipbook, merged into the C4 Submit null ROP. Returns
    (show, the null ROP)."""
    show = os.path.join(str(root), "show")
    hip = write(os.path.join(show, "sm0140", "fx", "sm0140_fx_v002.hip"), b"HIP")
    hou.reset(hip=hip, JOB=show)
    hipdir = os.path.dirname(hip)
    for f in range(1001, 1049):
        write(os.path.join(hipdir, "geo", f"fluid.{f:04d}.bgeo.sc"), b"fluid %d" % f)
        if cache_on_disk:
            write(os.path.join(hipdir, "cache", f"sim.{f:04d}.bgeo.sc"), b"sim %d" % f)
    for tile in (1001, 1002):
        write(os.path.join(hipdir, "tex", f"wood.{tile}.rat"), b"wood %d" % tile)
    write(os.path.join(show, "assets", "env", "env.abc"), b"abc")
    write(os.path.join(show, "otls", "studio_tools.hda"), b"hda")

    obj = hou.node("/obj").createNode("geo", "geo1")

    def sop(type_name, name, parm, value, bypass=False):
        n = hou.Node(f"{obj.path()}/{name}", type_name, "Sop", hou.NODE_PARMS[("Sop", type_name)]())
        n.parm(parm).set(value)
        n.bypass(bypass)
        return n

    sop("file", "fluid", "file", "$HIP/geo/fluid.$F4.bgeo.sc")
    sop("file", "simread", "file", "$HIP/cache/sim.$F4.bgeo.sc")
    sop("file", "wood", "file", "$HIP/tex/wood.<UDIM>.rat")
    sop("alembic", "env", "fileName", "$JOB/assets/env/env.abc")
    sop("file", "old", "file", "$HIP/geo/old_version.bgeo.sc", bypass=True)
    sop("file", "houdini_pic", "file", "$HFS/houdini/pic/butterfly.pic")
    sop("file", "inline", "file", "op:/obj/geo1/fluid")
    hou.extra_references.append((None, os.path.join(show, "otls", "studio_tools.hda")))

    out = hou.node("/out")
    sim = out.createNode("geometry", "sim")
    sim.parm("sopoutput").set("$HIP/cache/sim.$F4.bgeo.sc")
    sim.parm("trange").set(1)
    sim.parmTuple("f").set((1001, 1048, 1))
    mantra = out.createNode("ifd", "mantra1")
    mantra.parm("vm_picture").set("$HIP/render/sm0140_fx.$F4.exr")
    mantra.parm("trange").set(1)
    mantra.parmTuple("f").set((1001, 1048, 1))
    mantra.setInput(0, sim)
    flip = out.createNode("opengl", "flip")
    flip.parm("picture").set("$HIP/flip/sm0140.$F4.jpg")
    merge = out.createNode("merge", "merge1")
    merge.setInput(0, mantra)
    merge.setInput(1, flip)
    submit = out.createNode("null", "c4submit")
    submit.setInput(0, merge)
    return show, submit
