"""Render one view layer of one scene of a Blender file over this unit's
frames. blender-render.sh runs it in Blender:

    blender -b FILE.blend --python-exit-code 1 -P render.py -- scene=S view_layer=L [camera=C] [engine=E] [samples=N]

The unit's frames come from C4_FRAME_START, C4_FRAME_END and C4_FRAME_STEP
(or the list in C4_FRAMES), and every output stays under C4_TREE.

Only that view layer renders: the scene's other view layers are turned off
for this run (the file is not saved). The frames go where the file's output
path says, resolved from the file's folder, with two changes:

- an output outside the job's tree goes to ``render/`` at the top of the
  tree, keeping its file name;
- when the scene has more than one view layer to render, each goes in a
  folder named for it (``render/bg/sh010_1001.exr``), so the jobs of one
  scene's view layers do not write the same files.

Each frame rendered and found on disk is a ``c4_frame: <frame> <path>``
line, with ``openjd_progress``. A scene, view layer or camera the file does
not have is ``c4_fail: permanent``.

Everything but ``main`` and ``render`` is plain Python, so the tests run it
without Blender; ``prepare`` takes ``bpy`` as an argument for the same
reason.
"""

import os
import sys
from typing import Dict, List, Mapping, Optional, Sequence

#: The engine choices, as the tool's ``engine`` parameter names them.
ENGINES = ("file", "CYCLES", "EEVEE", "WORKBENCH")
#: The parameters render.py takes after ``--``.
KEYS = ("scene", "view_layer", "camera", "engine", "samples")


class Refusal(Exception):
    """A failure the script can name: ``kind`` is ``permanent`` or ``retry``."""

    def __init__(self, kind: str, reason: str):
        super().__init__(reason)
        self.kind, self.reason = kind, reason

    def line(self) -> str:
        return f"c4_fail: {self.kind} {self.reason}"


# --- arguments ------------------------------------------------------------------

def parse_args(argv: Sequence[str]) -> Dict[str, str]:
    """The ``key=value`` arguments after ``--`` in Blender's argv. A key may
    be given empty (``camera=``); one not given is empty."""
    rest = list(argv[list(argv).index("--") + 1:]) if "--" in argv else []
    out = {k: "" for k in KEYS}
    for a in rest:
        k, eq, v = a.partition("=")
        if not eq or k not in KEYS:
            raise Refusal("permanent", f"render.py takes {', '.join(k + '=' for k in KEYS)}, not {a!r}")
        out[k] = v
    if not out["scene"] or not out["view_layer"]:
        raise Refusal("permanent", "render.py needs scene= and view_layer=")
    if out["engine"] and out["engine"] not in ENGINES:
        raise Refusal("permanent", f"engine {out['engine']}: use {', '.join(ENGINES)}")
    try:
        if out["samples"] and int(out["samples"]) < 0:
            raise ValueError
    except ValueError:
        raise Refusal("permanent", f"samples {out['samples']!r} is not a whole number of 0 or more") from None
    return out


def unit_frames(env: Mapping[str, str]) -> List[int]:
    """The unit's frames, as c4-unit gives them: C4_FRAME_START, C4_FRAME_END
    and C4_FRAME_STEP, or C4_FRAMES (``1001,1004,1010-1012``) when the chunk is
    not one range."""
    label = env.get("C4_FRAMES", "")
    if "," in label or (label and not env.get("C4_FRAME_START")):
        out: List[int] = []
        for part in label.split(","):
            span, _, step = part.strip().partition(":")
            a, _, b = span.partition("-")
            out += list(range(int(a), int(b or a) + 1, int(step or 1)))
        return out
    start = env.get("C4_FRAME_START", "")
    if not start:
        raise Refusal("permanent", "the unit has no frames (C4_FRAME_START is not set)")
    end = env.get("C4_FRAME_END") or start
    step = env.get("C4_FRAME_STEP") or "1"
    return list(range(int(start), int(end) + 1, int(step)))


def engine_id(choice: str, version: Sequence[int]) -> Optional[str]:
    """Blender's name for an engine choice in this Blender, or None to keep
    the file's. EEVEE is BLENDER_EEVEE_NEXT from 4.2 until 5.0, which named
    it BLENDER_EEVEE again."""
    if not choice or choice == "file":
        return None
    if choice == "EEVEE":
        return "BLENDER_EEVEE_NEXT" if (4, 2) <= tuple(version[:2]) < (5, 0) else "BLENDER_EEVEE"
    if choice == "WORKBENCH":
        return "BLENDER_WORKBENCH"
    return choice


# --- where frames go ------------------------------------------------------------------

def _norm(p: str) -> str:
    return os.path.normpath(p).replace("\\", "/")


def inside(path: str, tree: str) -> bool:
    p, t = _norm(path), _norm(tree).rstrip("/")
    return p == t or p.startswith(t + "/")


def output_path(filepath: str, blend_dir: str, tree: str, view_layer: str, layers: int) -> str:
    """Where a view layer's frames go, as a Blender output path (``#`` and
    the extension still Blender's to add). ``filepath`` is the scene's own
    (``//render/sh010_``), ``layers`` how many of the scene's view layers
    render."""
    p = (filepath or "//").strip()
    slash = p.endswith(("/", "\\"))
    if p.startswith("//"):
        p = os.path.join(blend_dir, p[2:])
    elif not os.path.isabs(p):
        p = os.path.join(blend_dir, p)
    p = _norm(p) + ("/" if slash else "")
    if not inside(p, tree):
        p = _norm(os.path.join(tree, "render")) + "/" + ("" if slash else os.path.basename(p.rstrip("/")))
    if layers > 1:
        folder, name = p.rsplit("/", 1)
        p = f"{folder}/{view_layer}/{name}"
    return p


# --- the file --------------------------------------------------------------------------

def _names(coll) -> str:
    return ", ".join(x.name for x in coll) or "none"


def prepare(bpy, opts: Mapping[str, str], tree: str):
    """Sets up the open file to render one view layer: the scene, only that
    view layer on, the camera, engine and samples asked for, and the output
    under the tree. Returns the scene."""
    scene = bpy.data.scenes.get(opts["scene"])
    if scene is None:
        raise Refusal("permanent", f"the file has no scene {opts['scene']}; its scenes are {_names(bpy.data.scenes)}")
    layer = scene.view_layers.get(opts["view_layer"])
    if layer is None:
        raise Refusal("permanent", f"scene {scene.name} has no view layer {opts['view_layer']}; "
                                   f"its view layers are {_names(scene.view_layers)}")
    # the view layers the file renders, which are the jobs one scene makes
    rendering = sum(1 for vl in scene.view_layers if vl.use) + (0 if layer.use else 1)
    for vl in scene.view_layers:
        # by name: bpy gives a new Python object on every access
        vl.use = vl.name == layer.name
    if opts.get("camera"):
        cam = bpy.data.objects.get(opts["camera"])
        if cam is None or getattr(cam, "type", "") != "CAMERA":
            raise Refusal("permanent", f"the file has no camera {opts['camera']}")
        scene.camera = cam
    if scene.camera is None:
        raise Refusal("permanent", f"scene {scene.name} has no camera; set one or give the tool's camera")
    engine = engine_id(opts.get("engine", ""), bpy.app.version)
    if engine:
        scene.render.engine = engine
    samples = int(opts.get("samples") or 0)
    if samples:
        if scene.render.engine == "CYCLES":
            scene.cycles.samples = samples
        elif "EEVEE" in scene.render.engine:
            scene.eevee.taa_render_samples = samples
    blend_dir = os.path.dirname(bpy.data.filepath)
    scene.render.filepath = output_path(scene.render.filepath, blend_dir, tree, layer.name, rendering)
    return scene


def render(bpy, scene, frames: Sequence[int], out=print) -> None:
    """Renders each frame, as an animation one frame long so Blender names
    the file as it does for any animation render, and reports it once its
    file is on disk."""
    scene.frame_step = 1
    for i, f in enumerate(frames):
        # in the order Blender's start-before-end clamping allows
        if f > scene.frame_end:
            scene.frame_end, scene.frame_start = f, f
        else:
            scene.frame_start, scene.frame_end = f, f
        out(f"openjd_status: rendering {scene.name} / {_on(scene)}, frame {f}", flush=True)
        if "FINISHED" not in bpy.ops.render.render(animation=True, scene=scene.name):
            raise Refusal("retry", f"Blender did not finish frame {f}")
        path = scene.render.frame_path(frame=f)
        if not os.path.isfile(path):
            raise Refusal("retry", f"Blender rendered frame {f} but did not write {path}")
        out(f"c4_frame: {f} {path}", flush=True)
        out(f"openjd_progress: {100.0 * (i + 1) / len(frames):.1f}", flush=True)


def _on(scene) -> str:
    return next((vl.name for vl in scene.view_layers if vl.use), "")


def main() -> int:
    import bpy  # noqa: PLC0415 - only inside Blender

    try:
        opts = parse_args(sys.argv)
        frames = unit_frames(os.environ)
        tree = os.environ.get("C4_TREE") or os.path.dirname(bpy.data.filepath)
        scene = prepare(bpy, opts, tree)
        render(bpy, scene, frames)
    except Refusal as e:
        print(e.line(), flush=True)
        return 2 if e.kind == "permanent" else 1
    return 0


if __name__ == "__main__":
    code = main()
    if code:
        # --python-exit-code 1 makes Blender exit 1 on an exception; the
        # c4_fail line already said why.
        raise SystemExit(code)
