"""Projects and tools for the tests, built in the fake Katana over real files."""

import os

import fakekatana as fk
from c4submit.testing import write

from c4_katana.host import KatanaHost

KATANA_RENDER = {
    "c4": "tool/1", "name": "katana-render", "title": "Katana render",
    "summary": "Renders a Katana project's Render node over frames.",
    "kind": "frames",
    "runs": {"exe": "katana",
             "command": ["{{Run.Exe}}", "--batch", "--katana-file={{Param.scene}}", "--render-node={{Param.node}}",
                         "-t", "{{Unit.FrameStart}}-{{Unit.FrameEnd}}"]},
    "frames": {"chunk": 4, "order": "binary"},
    "params": [
        {"name": "scene", "type": "PATH", "objectType": "FILE", "dataFlow": "IN", "from": "scene.file",
         "userInterface": {"control": "CHOOSE_INPUT_FILE", "label": "Project",
                           "fileFilters": [{"label": "Katana projects", "patterns": ["*.katana"]}]}},
        {"name": "node", "type": "STRING", "from": "scene.renderables", "userInterface": {"label": "Render node"}},
        {"name": "threads", "type": "INT", "default": 0, "minValue": 0, "maxValue": 128, "show": "advanced",
         "affectsOutput": False},
        {"name": "verbose", "type": "BOOL", "default": False, "show": "advanced",
         "userInterface": {"label": "Verbose log"}},
    ],
    "needs": {"cpu": 16, "memory": "64G"},
    "reuse": "exact",
}

ASSET = {"widget": "assetIdInput"}


def light_project(root, dirty: bool = False):
    """A lighting project: the heron (an Alembic cache, a look file, UDIM
    textures on a material), the marsh set (a USD stage), and two Render
    nodes. Render_shadow renders shadow maps; Render_beauty adds fog (a USD
    stage) and a material reading the shadow maps, so it depends on
    Render_shadow. A bypassed Render_old is left over."""
    show = str(root)
    project = write(os.path.join(show, "seq", "sm0140", "katana", "sm0140_light_v002.katana"), b"<katana/>\n")
    abc = write(os.path.join(show, "anim", "sm0140_heron.abc"), b"abc")
    usd = write(os.path.join(show, "assets", "marsh", "marsh_set.usda"), b"#usda 1.0\n")
    klf = write(os.path.join(show, "assets", "heron", "heron.klf"), b"klf")
    tex = os.path.join(show, "assets", "heron", "tex")
    for tile in (1001, 1002, 1011):
        write(os.path.join(tex, f"heron_diffuse.{tile}.tex"), b"tile %d" % tile)
    fog = write(os.path.join(show, "fx", "fog", "fog.usd"), b"fog")
    out = os.path.join(show, "render", "sm0140")

    fk.reset(project, 1001, 1048, dirty=dirty)
    heron = fk.node("Alembic_In", "heron_abc", abcAsset=(abc, ASSET), location="/root/world/geo/heron")
    marsh = fk.node("UsdIn", "marsh_usd", fileName=(usd, ASSET), location="/root/world/geo/marsh")
    merge1 = fk.node("Merge", "merge_geo", heron, marsh)
    look = fk.node("LookFileAssign", "heron_look", merge1,
                   args={"lookfile": {"asset": {"enable": 1, "value": klf}}})
    mat = fk.group("heron_mat", ntype="NetworkMaterialCreate")
    fk.node("PrmanShadingNode", "heron_diffuse_tex", parent=mat, nodeType="PxrTexture",
            parameters={"filename": {"enable": 1, "value": (os.path.join(tex, "heron_diffuse.<UDIM>.tex"), ASSET)},
                        "fallback": {"enable": 0, "value": ("/old/library/grey.tex", ASSET)}})
    merge2 = fk.node("Merge", "merge_look", look, mat)
    settings = fk.node("RenderSettings", "settings", merge2, args={"renderSettings": {"cameraName": {
        "enable": 1, "value": "/root/world/cam/shotCam"}}})
    shadow = fk.render("Render_shadow", settings, outputs=[os.path.join(out, "shadow", "shadow.####.exr")])
    fog_in = fk.node("UsdIn", "fog_usd", fileName=(fog, ASSET), location="/root/world/fx/fog")
    bmat = fk.group("beauty_mat", ntype="NetworkMaterialCreate")
    fk.node("PrmanShadingNode", "shadow_map", parent=bmat, nodeType="PxrTexture",
            parameters={"filename": {"enable": 1, "value": (os.path.join(out, "shadow", "shadow.####.exr"), ASSET)}})
    merge3 = fk.node("Merge", "merge_beauty", settings, fog_in, bmat)
    fk.render("Render_beauty", merge3, outputs=[os.path.join(out, "beauty", "beauty.####.exr")],
              deps=["Render_shadow"])
    old = fk.render("Render_old", settings, outputs=[os.path.join(out, "old", "old.####.exr")])
    old.bypassed = True
    fk.state().dirty = dirty
    env = {"C4_PROJECT": "saltmarsh", "C4_SHOT": "sm0140", "C4_ROOT": show}
    return KatanaHost(env=env), project, shadow
