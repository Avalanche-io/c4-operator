"""Render one Render node of a Katana project over this unit's frames.

Python 3 runs it, as the tool's command does::

    python3 katana_render.py PROJECT --node=NODE [--threads=N]

It runs Katana's batch render::

    katana --batch --katana-file=PROJECT --render-node=NODE -t START-END

passes its log through, and works out which frames are finished from the
image files written during the run: each path the log names and each new
image under the job's tree. A frame is reported with ``c4_frame: <frame>
<path>`` once a later frame has started or the render has ended, and a
frame with no new image fails the unit, to be retried.

When the artist's project folder is somewhere else on their machine (the
farm's path map has rules), the project is a text file naming their paths,
so a copy with the farm's paths is rendered instead, beside it, and removed
after. A ``.py`` in place of a project is run by Katana's script mode to
build one first (``katana --script=BUILD.py OUT.katana``), as the tool's
test does: a project file is not written by hand.

Katana is ``$KATANA_ROOT/katana``, else ``katana`` on PATH. The unit's
frames come from the farm's variables (``C4_FRAME_START``,
``C4_FRAME_END``, ``C4_FRAME_STEP``, or ``C4_FRAMES`` for a list); a list
is rendered as one batch per run of evenly spaced frames. Everything but
``main`` is plain Python, tested without Katana.
"""

import argparse
import gzip
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from typing import Callable, List, Mapping, Optional, Tuple

# c4tool.py, beside this script, is a copy of integrations/dcc/toolkit/c4tool.py.
_HERE = os.path.dirname(os.path.abspath(globals().get("__file__") or sys.argv[0]))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)

import c4tool  # noqa: E402
from c4tool import fail, parse_frames, read_path_map, runs, unit_frames  # noqa: E402,F401

#: Image files a render writes.
IMAGE_EXTENSIONS = (".exr", ".tif", ".tiff", ".png", ".jpg", ".jpeg", ".dpx", ".tga", ".hdr", ".deepexr")
#: Which files are images, and the frame each holds.
IMAGES = c4tool.Images(IMAGE_EXTENSIONS)
image_paths, frame_of, new_images = IMAGES.paths, IMAGES.frame_of, IMAGES.new


# --- the unit's frames ------------------------------------------------------------

def katana_frames(run: Tuple[int, int, int]) -> str:
    """A run as Katana's ``-t`` takes it: ``1001-1004``, ``1001``, or each
    frame of a stepped run joined by commas."""
    start, end, step = run
    if start == end:
        return str(start)
    if step == 1:
        return f"{start}-{end}"
    return ",".join(str(f) for f in range(start, end + 1, step))


# --- the farm's path map (OpenJD pathmapping-1.0) -----------------------------------------

def map_text(text: str, rules: List[Tuple[str, str, str]]) -> str:
    """A project's text with each source root the farm's: a root followed
    by a slash, a quote or the end of a value. A Windows root matches
    either slash and any case, and the rest of its path gets forward
    slashes."""
    for fmt, src, dst in rules:
        dst = dst.rstrip("/")
        if fmt == "WINDOWS":
            s = src.replace("\\", "/").rstrip("/")
            pat = re.compile("".join("[\\\\/]" if c == "/" else re.escape(c) for c in s) +
                             r"((?:[\\/][^\"'<>\s]*)?)(?=[\"'<>\s]|$)", re.I)
            text = pat.sub(lambda m: dst + m.group(1).replace("\\", "/"), text)
            continue
        s = src.rstrip("/")
        text = re.sub(re.escape(s) + r"(?=[/\"'<>\s]|$)", lambda m: dst, text)
    return text


def mapped_copy(project: str, rules: List[Tuple[str, str, str]]) -> Optional[str]:
    """A copy of the project with the farm's paths, beside it (so its
    relative paths still resolve); None when nothing in it changes. A
    gzipped project stays gzipped."""
    with open(project, "rb") as f:
        raw = f.read()
    zipped = raw[:2] == b"\x1f\x8b"
    text = (gzip.decompress(raw) if zipped else raw).decode("utf-8", errors="surrogateescape")
    out = map_text(text, rules)
    if out == text:
        return None
    data = out.encode("utf-8", errors="surrogateescape")
    d, name = os.path.split(project)
    copy = os.path.join(d, ".c4-mapped-" + name)
    with open(copy, "wb") as f:
        f.write(gzip.compress(data) if zipped else data)
    return copy


# --- Katana's side ----------------------------------------------------------------------------

def katana_exe(env: Mapping[str, str], which: Optional[Callable[[str], Optional[str]]] = None) -> Optional[str]:
    """``$KATANA_ROOT/katana`` (``katana.bat`` on Windows), else ``katana`` on PATH."""
    root = env.get("KATANA_ROOT", "")
    for name in ("katana", "katana.bat", "katana.exe"):
        p = os.path.join(root, name) if root else ""
        if p and os.path.isfile(p):
            return p
    return (which or shutil.which)("katana")


def batch_command(katana: str, project: str, node: str, run: Tuple[int, int, int], threads: int = 0) -> List[str]:
    """Katana's batch render of one Render node over one run of frames.
    ``threads`` above 0 sets the renderer's threads (``--threads3d``)."""
    cmd = [katana, "--batch", f"--katana-file={project}", f"--render-node={node}", "-t", katana_frames(run)]
    if threads > 0:
        cmd.append(f"--threads3d={threads}")
    return cmd


def build_command(katana: str, script: str, out: str) -> List[str]:
    """Katana's script mode running a ``.py`` that saves a project to ``out``."""
    return [katana, f"--script={script}", out]


# --- the render ---------------------------------------------------------------------------------

def parse_args(argv: List[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(prog="katana_render.py")
    p.add_argument("project")
    p.add_argument("--node", default="")
    p.add_argument("--threads", type=int, default=0)
    return p.parse_args(argv)


def main(argv: List[str], env: Mapping[str, str], say: Optional[Callable[[str], None]] = None,
         popen=subprocess.Popen, clock: Callable[[], float] = time.time) -> int:
    say = say or c4tool.say
    try:
        a = parse_args(argv)
    except SystemExit:
        return fail(say, "permanent", "usage: katana_render.py PROJECT --node=NODE [--threads=N]")
    if not a.node:
        return fail(say, "permanent", "no Render node given (--node)")
    if not os.path.isfile(a.project):
        return fail(say, "permanent", f"no Katana project at {a.project}")
    if not a.project.lower().endswith((".katana", ".py")):
        return fail(say, "permanent", f"{os.path.basename(a.project)} is not a Katana project (.katana)")
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
    katana = katana_exe(env)
    if not katana:
        return fail(say, "permanent", "no katana in $KATANA_ROOT or on PATH; run this tool on an image with Katana")

    cwd = os.path.dirname(os.path.abspath(a.project))
    child_env = dict(env)
    top = env.get("C4_TREE") or cwd
    project, scratch, made = a.project, None, None
    try:
        if project.lower().endswith(".py"):
            scratch = tempfile.mkdtemp(prefix="c4-katana-")
            built = os.path.join(scratch, "project.katana")
            say(f"openjd_status: building the project with {os.path.basename(project)}")
            code = c4tool.stream(popen, build_command(katana, project, built), child_env, cwd, say)
            if code != 0 or not os.path.isfile(built):
                return fail(say, "permanent", f"{os.path.basename(project)} did not build a project (exit {code})")
            project = built
        elif rules:
            made = mapped_copy(project, rules)
            project = made or project

        say(f"openjd_status: rendering {a.node}, frames {frames[0]}-{frames[-1]}")
        since = clock() - 1
        done = c4tool.Finished(frames, since, say, IMAGES)
        for run in runs(frames):
            code = c4tool.stream(popen, batch_command(katana, project, a.node, run, a.threads), child_env, cwd, say,
                                 done.saw_line)
            if code != 0:
                return fail(say, "retry", f"katana --batch exited {code} on frames {katana_frames(run)}")
        missing = done.finish(new_images(top, since, frames))
        if missing:
            return fail(say, "retry", f"katana --batch exited 0 but wrote no image for frame {missing[0]} under {top}")
        return 0
    finally:
        if made:
            try:
                os.remove(made)
            except OSError:
                pass
        if scratch:
            shutil.rmtree(scratch, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:], os.environ))
