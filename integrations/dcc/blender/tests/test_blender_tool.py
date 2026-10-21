"""The blender-render tool the farm runs: its script's arguments, frames,
engine names and output paths, the file set up for one view layer, and the
panel filling the shipped tool.yaml's fields."""

import importlib.util
import os
import re
import types

import fakebpy as bf
import pytest
from blender_scenes import env, lighting
from c4submit import yamlio
from c4submit.testing import SALTMARSH, FakeFarm

from c4_blender import panel

TOOL = os.path.normpath(os.path.join(os.path.dirname(__file__), "..", "tools", "blender-render"))


def _script():
    spec = importlib.util.spec_from_file_location("blender_render_script", os.path.join(TOOL, "render.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


R = _script()


def tool_yaml() -> dict:
    with open(os.path.join(TOOL, "tool.yaml"), encoding="utf-8") as f:
        return yamlio.load(f.read())


# --- arguments ------------------------------------------------------------------

def test_args_after_the_dashes():
    argv = ["blender", "-b", "x.blend", "--python", "render.py", "--", "scene=sh010", "view_layer=bg",
            "camera=", "engine=CYCLES", "samples=64"]
    assert R.parse_args(argv) == {"scene": "sh010", "view_layer": "bg", "camera": "", "engine": "CYCLES",
                                  "samples": "64"}
    assert R.parse_args(["--", "scene=a=b", "view_layer=x"])["scene"] == "a=b", "a name may hold ="


@pytest.mark.parametrize("args,says", [
    (["scene=sh010"], "needs scene= and view_layer="),
    (["scene=sh010", "view_layer=bg", "colour=red"], "not 'colour=red'"),
    (["scene=sh010", "view_layer=bg", "bg"], "not 'bg'"),
    (["scene=sh010", "view_layer=bg", "engine=RENDERMAN"], "engine RENDERMAN: use file, CYCLES, EEVEE, WORKBENCH"),
    (["scene=sh010", "view_layer=bg", "samples=-1"], "samples '-1' is not a whole number"),
    (["scene=sh010", "view_layer=bg", "samples=lots"], "samples 'lots'"),
])
def test_bad_args_are_permanent(args, says):
    with pytest.raises(R.Refusal) as e:
        R.parse_args(["--"] + args)
    assert e.value.kind == "permanent" and says in e.value.reason
    assert e.value.line().startswith("c4_fail: permanent ")


def test_unit_frames():
    assert R.unit_frames({"C4_FRAME_START": "1001", "C4_FRAME_END": "1004"}) == [1001, 1002, 1003, 1004]
    assert R.unit_frames({"C4_FRAME_START": "1001", "C4_FRAME_END": "1007", "C4_FRAME_STEP": "3"}) == [1001, 1004, 1007]
    assert R.unit_frames({"C4_FRAME_START": "1001"}) == [1001]
    assert R.unit_frames({"C4_FRAMES": "1001,1004,1010-1012", "C4_FRAME_START": "1001"}) == [1001, 1004, 1010, 1011, 1012]
    assert R.unit_frames({"C4_FRAMES": "1001-1005:2"}) == [1001, 1003, 1005]
    with pytest.raises(R.Refusal, match="no frames"):
        R.unit_frames({})


def test_engine_names_by_blender_version():
    assert R.engine_id("file", (4, 2, 0)) is None and R.engine_id("", (5, 2, 2)) is None
    assert R.engine_id("EEVEE", (4, 2, 0)) == "BLENDER_EEVEE_NEXT"
    assert R.engine_id("EEVEE", (4, 5, 3)) == "BLENDER_EEVEE_NEXT"
    assert R.engine_id("EEVEE", (5, 2, 2)) == "BLENDER_EEVEE"
    assert R.engine_id("EEVEE", (4, 1, 1)) == "BLENDER_EEVEE"
    assert R.engine_id("WORKBENCH", (4, 2, 0)) == "BLENDER_WORKBENCH"
    assert R.engine_id("CYCLES", (5, 2, 2)) == "CYCLES"


# --- where frames go ------------------------------------------------------------------

def test_output_stays_where_the_file_says_inside_the_tree():
    light = "/show/sh010/lighting"
    assert R.output_path("//render/sh010_", light, "/show", "bg", 1) == "/show/sh010/lighting/render/sh010_"
    assert R.output_path("//../render/", light, "/show", "bg", 1) == "/show/sh010/render/"
    assert R.output_path("/show/sh010/out/sh010_####.exr", light, "/show", "bg", 1) == "/show/sh010/out/sh010_####.exr"
    assert R.output_path("render/sh010_", light, "/show", "bg", 1) == "/show/sh010/lighting/render/sh010_", \
        "a path with no // is from the file's folder too"


def test_each_view_layer_gets_its_folder_when_a_scene_has_several():
    light = "/show/sh010/lighting"
    assert R.output_path("//render/sh010_", light, "/show", "bg", 2) == "/show/sh010/lighting/render/bg/sh010_"
    assert R.output_path("//render/", light, "/show", "fg", 3) == "/show/sh010/lighting/render/fg/"


def test_an_output_outside_the_tree_goes_to_render():
    light = "/show/sh010/lighting"
    assert R.output_path("/mnt/renders/sh010/sh010_", light, "/show", "bg", 1) == "/show/render/sh010_"
    assert R.output_path("/tmp/", light, "/show", "bg", 1) == "/show/render/"
    assert R.output_path("", light, "/show", "bg", 1) == "/show/sh010/lighting/"
    assert R.output_path("//../../../elsewhere/x_", light, "/show", "fg", 2) == "/show/render/fg/x_"
    assert R.output_path("/showreel/x_", light, "/show", "bg", 1) == "/show/render/x_", "a prefix is not a folder"


# --- the file --------------------------------------------------------------------------

def blend(tmp_path, layers=("bg", "fg", "old"), version=(5, 2, 2)):
    s = bf.Scene("sh010", frame_start=1001, frame_end=1048)
    for name in layers:
        s.layer(name, use=name != "old")
    s.render.filepath = "//render/sh010_"
    s.cycles = bf.Struct(samples=128)
    s.eevee = bf.Struct(taa_render_samples=64)
    cam = bf.ID("CAM_sh010", type="CAMERA")
    s.camera = cam
    objects = bf.Collection([cam, bf.ID("CAM_alt", type="CAMERA"), bf.ID("Cube", type="MESH")])
    data = types.SimpleNamespace(scenes=bf.Collection([s]), objects=objects,
                                 filepath=str(tmp_path / "sh010" / "lighting" / "sh010_lighting_v003.blend"))
    return types.SimpleNamespace(data=data, app=types.SimpleNamespace(version=version)), s


def opts(**kw):
    o = {"scene": "sh010", "view_layer": "fg", "camera": "", "engine": "", "samples": ""}
    o.update(kw)
    return o


def test_prepare_renders_one_view_layer(tmp_path):
    bpy, s = blend(tmp_path)
    assert R.prepare(bpy, opts(), str(tmp_path)) is s
    assert [(vl.name, vl.use) for vl in s.view_layers] == [("bg", False), ("fg", True), ("old", False)]
    assert s.render.filepath == f"{tmp_path}/sh010/lighting/render/fg/sh010_".replace("\\", "/"), \
        "bg and fg both render, so each has its folder"
    assert s.camera.name == "CAM_sh010" and s.render.engine == "CYCLES" and s.cycles.samples == 128, "the file's"


def test_prepare_takes_camera_engine_and_samples(tmp_path):
    bpy, s = blend(tmp_path)
    R.prepare(bpy, opts(camera="CAM_alt", engine="EEVEE", samples="16"), str(tmp_path))
    assert s.camera.name == "CAM_alt" and s.render.engine == "BLENDER_EEVEE" and s.eevee.taa_render_samples == 16
    bpy, s = blend(tmp_path, version=(4, 2, 0))
    R.prepare(bpy, opts(engine="EEVEE"), str(tmp_path))
    assert s.render.engine == "BLENDER_EEVEE_NEXT"
    bpy, s = blend(tmp_path)
    R.prepare(bpy, opts(samples="8"), str(tmp_path))
    assert s.cycles.samples == 8


def test_a_lone_view_layer_keeps_the_files_path(tmp_path):
    bpy, s = blend(tmp_path, layers=("ViewLayer", "old"))
    R.prepare(bpy, opts(view_layer="ViewLayer"), str(tmp_path))
    assert s.render.filepath.endswith("/sh010/lighting/render/sh010_")
    bpy, s = blend(tmp_path, layers=("ViewLayer", "old"))
    R.prepare(bpy, opts(view_layer="old"), str(tmp_path))
    assert s.render.filepath.endswith("/render/old/sh010_"), "a view layer the file does not render is a second job"


@pytest.mark.parametrize("o,says", [
    (opts(scene="sh020"), "the file has no scene sh020; its scenes are sh010"),
    (opts(view_layer="fx"), "scene sh010 has no view layer fx; its view layers are bg, fg, old"),
    (opts(camera="CAM_nope"), "the file has no camera CAM_nope"),
    (opts(camera="Cube"), "the file has no camera Cube"),
])
def test_prepare_refuses_what_the_file_lacks(tmp_path, o, says):
    bpy, _ = blend(tmp_path)
    with pytest.raises(R.Refusal) as e:
        R.prepare(bpy, o, str(tmp_path))
    assert e.value.kind == "permanent" and e.value.reason == says


def test_a_scene_with_no_camera(tmp_path):
    bpy, s = blend(tmp_path)
    s.camera = None
    with pytest.raises(R.Refusal, match="scene sh010 has no camera"):
        R.prepare(bpy, opts(), str(tmp_path))


class FakeRender:
    """bpy.ops.render and RenderSettings.frame_path, writing a file per frame."""

    def __init__(self, scene, write=True):
        self.scene, self.write, self.calls = scene, write, []
        scene.render.frame_path = self.frame_path
        scene.frame_step = 3

    def frame_path(self, frame):
        return f"{self.scene.render.filepath}{frame:04d}.png"

    def render(self, animation=False, scene=""):
        s = self.scene
        self.calls.append((animation, scene, s.frame_start, s.frame_end, s.frame_step))
        if self.write:
            p = self.frame_path(s.frame_start)
            os.makedirs(os.path.dirname(p), exist_ok=True)
            open(p, "wb").close()
        return {"FINISHED"}


def test_render_reports_each_frame_on_disk(tmp_path):
    bpy, s = blend(tmp_path)
    R.prepare(bpy, opts(), str(tmp_path))
    fake = FakeRender(s)
    bpy.ops = types.SimpleNamespace(render=fake)
    lines = []
    R.render(bpy, s, [1050, 1001, 1003], out=lambda line, flush=False: lines.append(line))
    assert fake.calls == [(True, "sh010", 1050, 1050, 1), (True, "sh010", 1001, 1001, 1), (True, "sh010", 1003, 1003, 1)], \
        "one-frame animations, past the scene's end and back"
    frames = [line for line in lines if line.startswith("c4_frame:")]
    assert frames == [f"c4_frame: {f} {fake.frame_path(f)}" for f in (1050, 1001, 1003)]
    assert [line for line in lines if line.startswith("openjd_progress")][-1] == "openjd_progress: 100.0"
    assert "openjd_status: rendering sh010 / fg, frame 1050" in lines


def test_a_frame_blender_did_not_write_is_a_retry(tmp_path):
    bpy, s = blend(tmp_path)
    R.prepare(bpy, opts(), str(tmp_path))
    bpy.ops = types.SimpleNamespace(render=FakeRender(s, write=False))
    with pytest.raises(R.Refusal) as e:
        R.render(bpy, s, [1001], out=lambda *a, **k: None)
    assert e.value.kind == "retry" and "did not write" in e.value.reason


# --- the shipped tool.yaml -------------------------------------------------------------

def test_the_command_passes_every_argument_by_name():
    t = tool_yaml()
    cmd = t["runs"]["command"]
    assert cmd[:3] == ["bash", "{{Tool.Dir}}/blender-render.sh", "{{Param.blend}}"]
    assert cmd[3:] == [f"{k}={{{{Param.{k}}}}}" for k in R.KEYS], \
        "key=value, so an empty camera is not dropped and the rest do not shift"
    params = {p["name"]: p for p in t["params"]}
    assert set(params) == {"blend"} | set(R.KEYS)
    assert params["engine"]["allowedValues"] == list(R.ENGINES)
    assert (params["scene"]["from"], params["view_layer"]["from"], params["camera"]["from"]) == \
        ("scene.renderables", "scene.renderables", "scene.camera")
    with open(os.path.join(TOOL, "blender-render.sh"), encoding="utf-8") as f:
        sh = f.read()
    assert "--python-exit-code 1" in sh and re.search(r'--python "\$here/render.py" -- "\$@"', sh)
    assert os.path.isfile(os.path.join(TOOL, t["test"]["tree"], t["test"]["with"]["blend"]))


def test_the_panel_fills_the_shipped_tool(tmp_path, monkeypatch):
    show, _ = lighting(tmp_path)
    env(monkeypatch, show)
    fake = FakeFarm(tools=[tool_yaml()], projects={"saltmarsh": dict(SALTMARSH, root="/show")})
    monkeypatch.setattr(panel, "farm_factory", lambda: fake)
    bf.draw(panel.C4_PT_farm)
    bf.settle()
    c = panel._controllers[bf.context.scene.as_pointer()]
    assert c.draft.tool.name == "blender-render"
    bf.bpy.ops.c4.check("INVOKE_DEFAULT")
    bf.settle()
    assert c.checked.errors == [], c.summary_text()
    assert "48 frames in 12 units" in c.summary_text()
    bf.bpy.ops.c4.submit("INVOKE_DEFAULT")
    bf.settle()
    jobs = fake.jobs[0]["jobs"]
    assert jobs["render-bg"]["with"] == {"blend": "sh010/lighting/sh010_lighting_v003.blend", "scene": "sh010",
                                         "view_layer": "bg", "camera": "CAM_sh010"}
    assert jobs["render-fg"]["with"]["view_layer"] == "fg"
