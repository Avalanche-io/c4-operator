"""Render one render layer of a Maya scene over this unit's frames.

``mayapy`` runs it, as the tool's command does::

    mayapy maya_render.py SCENE --layer=LAYER [--camera=CAMERA] [--renderer=RENDERER]

It runs Maya's command-line renderer, ``Render``, from beside ``mayapy``
(``Render -r RENDERER -proj WORKSPACE -rl LAYER -cam CAMERA -s START -e END
-b STEP SCENE``), passes its log through, and works out which frames are
finished from the image files written during the run: each path the
renderer's log names (Arnold's ``writing file``, Redshift's ``Saved file``)
and each new image under the job's tree. A frame is reported with
``c4_frame: <frame> <path>`` once a later frame has started or the render
has ended, and a frame with no new image fails the unit, to be retried.
``mayapy`` itself takes no licence; ``Render`` does.

The unit's frames come from the farm's variables (``C4_FRAME_START``,
``C4_FRAME_END``, ``C4_FRAME_STEP``, or ``C4_FRAMES`` for a list); a list
is rendered as one ``Render`` per run of evenly spaced frames. Everything
but ``main`` is plain Python, tested without Maya.
"""

import argparse
import os
import shutil
import subprocess
import sys
import tempfile
import time
from typing import Callable, Dict, List, Mapping, Optional, Tuple

# c4tool.py, beside this script, is a copy of integrations/dcc/toolkit/c4tool.py.
_HERE = os.path.dirname(os.path.abspath(globals().get("__file__") or sys.argv[0]))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)

import c4tool  # noqa: E402
from c4tool import fail, parse_frames, read_path_map, runs, unit_frames  # noqa: E402,F401

#: Image files a render writes.
IMAGE_EXTENSIONS = (".exr", ".tif", ".tiff", ".png", ".jpg", ".jpeg", ".iff", ".dpx", ".tga", ".hdr", ".sgi",
                    ".bmp", ".deepexr")
#: Which files are images, and the frame each holds.
IMAGES = c4tool.Images(IMAGE_EXTENSIONS)
image_paths, frame_of, new_images = IMAGES.paths, IMAGES.frame_of, IMAGES.new


# --- the farm's path map (OpenJD pathmapping-1.0) -----------------------------------------

def _mel_string(s: str) -> str:
    return '"' + s.replace("\\", "\\\\").replace('"', '\\"') + '"'


def dirmap_mel(rules: List[Tuple[str, str, str]]) -> str:
    """MEL that turns on Maya's directory mapping for each rule, so the
    files the scene names on the artist's machine are read from the tree.
    A Windows source is mapped with both its slashes."""
    if not rules:
        return ""
    out = ["dirmap -en true;"]
    for fmt, src, dst in rules:
        srcs = [src.replace("\\", "/")]
        if fmt == "WINDOWS" and "\\" in src:
            srcs.append(src)
        for s in srcs:
            out.append(f"dirmap -m {_mel_string(s)} {_mel_string(dst)};")
    return " ".join(out)


# --- Maya's side ------------------------------------------------------------------------------

def workspace_of(scene: str) -> str:
    """The Maya project a scene belongs to: the nearest folder above it
    with a ``workspace.mel``, else the folder above ``scenes/``, else the
    scene's own folder."""
    d = os.path.dirname(os.path.abspath(scene))
    probe = d
    while True:
        if os.path.isfile(os.path.join(probe, "workspace.mel")):
            return probe
        up = os.path.dirname(probe)
        if up == probe:
            break
        probe = up
    if os.path.basename(d).lower() == "scenes":
        return os.path.dirname(d)
    return d


def render_exe(mayapy: str, which: Optional[Callable[[str], Optional[str]]] = None) -> Optional[str]:
    """``Render`` beside ``mayapy`` (``Render.exe`` on Windows), else on PATH."""
    here = os.path.dirname(os.path.abspath(mayapy))
    for name in ("Render", "Render.exe"):
        p = os.path.join(here, name)
        if os.path.isfile(p):
            return p
    return (which or shutil.which)("Render")


def render_command(render: str, scene: str, project: str, layer: str, camera: str, renderer: str,
                   run: Tuple[int, int, int], pre_render: str = "") -> List[str]:
    """``Render``'s arguments for one run of frames. ``file`` keeps the
    scene's own renderer. Arnold is asked for a log that names each image
    it writes (``-ai:lve 2``)."""
    start, end, step = run
    cmd = [render, "-r", renderer or "file", "-proj", project, "-s", str(start), "-e", str(end), "-b", str(step)]
    if layer:
        cmd += ["-rl", layer]
    if camera:
        cmd += ["-cam", camera]
    if renderer == "arnold":
        cmd += ["-ai:lve", "2"]
    if pre_render:
        cmd += ["-preRender", pre_render]
    return cmd + [scene]


def app_dir_env(env: Mapping[str, str], mkdtemp: Callable[[], str] = tempfile.mkdtemp) -> Dict[str, str]:
    """``MAYA_APP_DIR`` in a scratch folder when the unit's home cannot be
    written (units run as an unprivileged user), so Maya can keep its
    preferences; {} when nothing needs changing."""
    if env.get("MAYA_APP_DIR"):
        return {}
    home = env.get("HOME") or os.path.expanduser("~")
    if home and os.path.isdir(home) and os.access(home, os.W_OK):
        return {}
    return {"MAYA_APP_DIR": mkdtemp()}


# --- the render ---------------------------------------------------------------------------------

def parse_args(argv: List[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(prog="maya_render.py")
    p.add_argument("scene")
    p.add_argument("--layer", default="")
    p.add_argument("--camera", default="")
    p.add_argument("--renderer", default="file")
    return p.parse_args(argv)


def main(argv: List[str], env: Mapping[str, str], mayapy: str = sys.executable,
         say: Optional[Callable[[str], None]] = None, popen=subprocess.Popen,
         clock: Callable[[], float] = time.time) -> int:
    say = say or c4tool.say
    try:
        a = parse_args(argv)
    except SystemExit:
        return fail(say, "permanent", "usage: maya_render.py SCENE --layer=LAYER [--camera=CAMERA] [--renderer=R]")
    if not os.path.isfile(a.scene):
        return fail(say, "permanent", f"no Maya scene at {a.scene}")
    if not a.scene.lower().endswith((".ma", ".mb")):
        return fail(say, "permanent", f"{os.path.basename(a.scene)} is not a Maya scene (.ma or .mb)")
    try:
        frames = unit_frames(env)
    except ValueError as e:
        return fail(say, "permanent", str(e))
    if not frames:
        return fail(say, "permanent", "the unit has no frames (C4_FRAME_START or C4_FRAMES)")
    try:
        rules = read_path_map(env.get("C4_PATH_MAP", ""))
    except (OSError, ValueError, KeyError) as e:
        return fail(say, "permanent", f"the farm's path map {env.get('C4_PATH_MAP')} does not read: {e}")
    render = render_exe(mayapy)
    if not render:
        return fail(say, "permanent", f"no Render beside {mayapy} or on PATH; run this tool on an image with Maya")
    project = workspace_of(a.scene)
    child_env = dict(env)
    child_env.update(app_dir_env(env))
    top = env.get("C4_TREE") or project
    pre = dirmap_mel(rules)

    say(f"openjd_status: rendering {a.layer or 'the scene'} with {a.renderer}, frames {frames[0]}-{frames[-1]}")
    since = clock() - 1
    done = c4tool.Finished(frames, since, say, IMAGES)
    for run in runs(frames):
        cmd = render_command(render, a.scene, project, a.layer, a.camera, a.renderer, run, pre)
        code = c4tool.stream(popen, cmd, child_env, project, say, done.saw_line)
        if code != 0:
            return fail(say, "retry", f"Render exited {code} on frames {run[0]}-{run[1]}")
    missing = done.finish(new_images(top, since, frames))
    if missing:
        return fail(say, "retry", f"Render exited 0 but wrote no image for frame {missing[0]} under {top}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:], os.environ))
