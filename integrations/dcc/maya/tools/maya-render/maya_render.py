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
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from typing import Callable, Dict, Iterable, List, Mapping, Optional, Tuple

#: Image files a render writes.
IMAGE_EXTENSIONS = (".exr", ".tif", ".tiff", ".png", ".jpg", ".jpeg", ".iff", ".dpx", ".tga", ".hdr", ".sgi",
                    ".bmp", ".deepexr")
#: A path to an image in a log line: absolute, ending in an image extension.
_IMAGE_IN_LINE = re.compile(r"((?:[A-Za-z]:)?[\\/][^\s'\"`<>|]*?\.(?:" +
                            "|".join(e[1:] for e in IMAGE_EXTENSIONS) + r"))(?=[\s'\"`]|$)", re.I)
_LAST_NUMBER = re.compile(r"(\d+)(?=\D*$)")


# --- the unit's frames ------------------------------------------------------------

def parse_frames(label: str) -> List[int]:
    """A frame list as the farm writes it: ``1001``, ``1001-1004``,
    ``1001-1010:2`` (or ``x2``), and lists of those joined by commas."""
    out: List[int] = []
    for part in (label or "").replace(" ", "").split(","):
        if not part:
            continue
        m = re.fullmatch(r"(-?\d+)(?:-(-?\d+)(?:[:x](\d+))?)?", part)
        if not m:
            raise ValueError(f"the frames {label!r} do not read ({part!r})")
        first = int(m.group(1))
        last = int(m.group(2)) if m.group(2) is not None else first
        step = int(m.group(3) or 1)
        if last < first or step < 1:
            raise ValueError(f"the frames {label!r} do not read ({part!r})")
        out += [f for f in range(first, last + 1, step) if f not in out]
    return out


def unit_frames(env: Mapping[str, str]) -> List[int]:
    """This unit's frames: ``C4_FRAMES`` when it is a list, else
    ``C4_FRAME_START`` to ``C4_FRAME_END`` by ``C4_FRAME_STEP``."""
    label = env.get("C4_FRAMES", "")
    if "," in label:
        return parse_frames(label)
    start = env.get("C4_FRAME_START", "")
    if not start:
        return parse_frames(label)
    end = env.get("C4_FRAME_END") or start
    step = env.get("C4_FRAME_STEP") or "1"
    return parse_frames(f"{start}-{end}:{step}")


def runs(frames: List[int]) -> List[Tuple[int, int, int]]:
    """Frames as (start, end, step) runs, each one ``Render`` call."""
    out: List[Tuple[int, int, int]] = []
    fs = sorted(set(frames))
    i = 0
    while i < len(fs):
        if i + 1 == len(fs):
            out.append((fs[i], fs[i], 1))
            break
        step = fs[i + 1] - fs[i]
        j = i + 1
        while j + 1 < len(fs) and fs[j + 1] - fs[j] == step:
            j += 1
        out.append((fs[i], fs[j], step))
        i = j + 1
    return out


# --- the farm's path map (OpenJD pathmapping-1.0) -----------------------------------------

def read_path_map(path: str) -> List[Tuple[str, str, str]]:
    """(format, source, destination) for each rule in the file
    ``C4_PATH_MAP`` names, longest source first; [] with no file."""
    if not path:
        return []
    with open(path, encoding="utf-8") as f:
        doc = json.load(f)
    rules = [(r.get("source_path_format", "POSIX").upper(), r["source_path"], r["destination_path"])
             for r in doc.get("path_mapping_rules") or []]
    return sorted(rules, key=lambda r: -len(r[1]))


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


# --- which frames are finished -----------------------------------------------------------------

def image_paths(line: str) -> List[str]:
    """The image files a log line names."""
    return [m.group(1) for m in _IMAGE_IN_LINE.finditer(line)]


def frame_of(path: str) -> Optional[int]:
    """A frame's number from its file's name: the last number before the extension."""
    stem = os.path.basename(path)
    for ext in IMAGE_EXTENSIONS:
        if stem.lower().endswith(ext):
            stem = stem[:-len(ext)]
            break
    m = _LAST_NUMBER.search(stem)
    return int(m.group(1)) if m else None


def new_images(top: str, since: float, frames: Iterable[int]) -> Dict[int, str]:
    """Image files under ``top`` written since ``since``, by frame, for
    the frames given: the first by name when a frame has several (its AOVs)."""
    want, out = set(frames), {}
    if not top or not os.path.isdir(top):
        return out
    for d, dirs, names in os.walk(top):
        dirs[:] = sorted(x for x in dirs if x != ".c4")
        for name in sorted(names):
            if not name.lower().endswith(IMAGE_EXTENSIONS):
                continue
            f = frame_of(name)
            if f not in want or f in out:
                continue
            p = os.path.join(d, name)
            try:
                if os.path.getmtime(p) >= since:
                    out[f] = p
            except OSError:
                continue
    return out


class Finished:
    """Reports frames as they finish. ``Render`` goes through its frames in
    order, so a file for one frame means every earlier frame with a file
    is done. Only files written during this run count, so a texture or a
    plate the log names is never taken for a frame."""

    def __init__(self, frames: List[int], since: float, say: Callable[[str], None]):
        self.frames, self.since, self.say = list(frames), since, say
        self.files: Dict[int, str] = {}
        self.done: List[int] = []

    def _new(self, path: str) -> bool:
        try:
            return os.path.getmtime(path) >= self.since
        except OSError:
            return True   # not there yet: being written; checked again when reported

    def saw(self, path: str) -> None:
        f = frame_of(path)
        if f not in self.frames or not self._new(path):
            return
        self.files.setdefault(f, path)
        for g in self.frames:
            if g < f and g in self.files:
                self.report(g)

    def report(self, frame: int) -> None:
        if frame in self.done or not os.path.isfile(self.files.get(frame, "")):
            return
        self.done.append(frame)
        self.say(f"c4_frame: {frame} {self.files[frame]}")
        self.say(f"openjd_progress: {round(100 * len(self.done) / len(self.frames))}")

    def finish(self, found: Mapping[int, str]) -> List[int]:
        """Report the rest, from the files found after the run; the frames
        still without a file."""
        for f in self.frames:
            if f not in self.files or not os.path.isfile(self.files[f]):
                if f in found:
                    self.files[f] = found[f]
            self.report(f)
        return [f for f in self.frames if f not in self.done]


# --- the render ---------------------------------------------------------------------------------

def fail(say: Callable[[str], None], kind: str, reason: str) -> int:
    say(f"c4_fail: {kind} {reason}")
    return 2 if kind == "permanent" else 1


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
    say = say or (lambda line: print(line, flush=True))
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
    done = Finished(frames, since, say)
    for run in runs(frames):
        cmd = render_command(render, a.scene, project, a.layer, a.camera, a.renderer, run, pre)
        proc = popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=child_env, cwd=project,
                     universal_newlines=True, errors="replace")
        for line in proc.stdout:
            line = line.rstrip("\n")
            say(line)
            for p in image_paths(line):
                done.saw(p)
        code = proc.wait()
        if code != 0:
            return fail(say, "retry", f"Render exited {code} on frames {run[0]}-{run[1]}")
    missing = done.finish(new_images(top, since, frames))
    if missing:
        return fail(say, "retry", f"Render exited 0 but wrote no image for frame {missing[0]} under {top}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:], os.environ))
