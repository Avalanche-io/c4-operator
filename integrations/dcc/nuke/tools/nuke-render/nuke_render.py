"""Render one Write node of a Nuke script over this unit's frames.

Nuke runs it in terminal mode, as the tool's command does::

    nuke -m THREADS -t nuke_render.py SCRIPT WRITE PROXY

It is what ``nuke -x -X WRITE -F FRAMES SCRIPT`` does, done in Nuke's own
Python so that the unit holds one licence and knows each frame's file:
it opens the script (through the farm's path map when the artist's root
differs from the farm's), renders the Write one frame at a time with
``nuke.execute``, checks the frame's file is there, and prints
``c4_frame: <frame> <path>`` and ``openjd_progress``. A failure it can name
is ``c4_fail: permanent`` (no script, no such Write, a missing input) or
``c4_fail: retry``.

The unit's frames come from the farm's variables (``C4_FRAME_START``,
``C4_FRAME_END``, ``C4_FRAME_STEP``, or ``C4_FRAMES`` for a list), so
nothing here parses Nuke's own frame syntax. Everything but ``main`` is
plain Python, tested without Nuke.
"""

import os
import re
import sys
from typing import Callable, List, Mapping, Optional

# c4tool.py, beside this script, is a copy of integrations/dcc/toolkit/c4tool.py.
_HERE = os.path.dirname(os.path.abspath(globals().get("__file__") or sys.argv[0]))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)

import c4tool  # noqa: E402
from c4tool import fail, map_path, parse_frames, read_path_map, unit_frames  # noqa: E402,F401

#: The node classes that write files (the panel's renderables).
WRITE_CLASSES = ("Write", "DeepWrite", "WriteGeo")
#: What Nuke says when a Read's file is not there (a frame or a whole
#: sequence). The files are the job's tree, so another try cannot help.
MISSING_INPUT = re.compile(r"No such file or directory|Read error|file not found", re.I)

_HASHES = re.compile(r"#+")
_PRINTF = re.compile(r"%(0?)(\d*)d")


# --- a frame's file -----------------------------------------------------------------------

def frame_file(pattern: str, frame: int) -> str:
    """A Write's file for one frame: ``####`` and ``%04d`` filled in."""
    out = _HASHES.sub(lambda m: f"{frame:0{len(m.group(0))}d}", pattern)
    return _PRINTF.sub(lambda m: f"{frame:0{int(m.group(2) or 0)}d}" if m.group(1) else str(frame), out)


def view_files(path: str, views: List[str]) -> List[str]:
    """A stereo path (``%V``, ``%v``) as one path per view."""
    if "%V" not in path and "%v" not in path or not views:
        return [path]
    return [path.replace("%V", v).replace("%v", v[:1].lower()) for v in views]


def absolute(path: str, base: str) -> str:
    """A relative output path, from the script's project directory."""
    if not path or os.path.isabs(path) or re.match(r"^[A-Za-z]:[\\/]", path) or not base:
        return path
    return os.path.normpath(os.path.join(base, path))


def as_bool(value: str) -> bool:
    return str(value).strip().lower() in ("1", "true", "yes", "on")


# --- the render ---------------------------------------------------------------------------------

def _knob_value(node, name: str, default=None):
    try:
        k = node.knob(name)
        return default if k is None else k.value()
    except Exception:  # noqa: BLE001 - Nuke raises its own
        return default


def project_dir(nuke, script: str) -> str:
    """Where Nuke resolves the script's relative paths: the root's project
    directory, else the script's folder."""
    try:
        k = nuke.root().knob("project_directory")
        d = (k.evaluate() if k is not None else "") or ""
    except Exception:  # noqa: BLE001
        d = ""
    return d.strip() or os.path.dirname(script)


def render(nuke, script: str, write_name: str, proxy: bool, frames: List[int], rules,
           say: Callable[[str], None]) -> int:
    if not os.path.isfile(script):
        return fail(say, "permanent", f"no Nuke script at {script}")
    if rules:
        nuke.addFilenameFilter(lambda p: map_path(p, rules))
    try:
        nuke.scriptOpen(script)
    except Exception as e:  # noqa: BLE001 - Nuke raises RuntimeError with its reason
        return fail(say, "permanent", f"Nuke could not open {script}: {e}")
    write = nuke.toNode(write_name)
    if write is None or write.Class() not in WRITE_CLASSES:
        names = sorted(n.fullName() for n in nuke.allNodes(group=nuke.root(), recurseGroups=True)
                       if n.Class() in WRITE_CLASSES)
        return fail(say, "permanent", f"no Write node {write_name!r} in {os.path.basename(script)}; "
                                      f"its Write nodes are {', '.join(names) or 'none'}")
    if _knob_value(write, "disable", False):
        return fail(say, "permanent", f"the Write node {write_name} is disabled in {os.path.basename(script)}")
    if proxy:
        nuke.root().knob("proxy").setValue(True)
    pattern = absolute(nuke.filename(write) or "", project_dir(nuke, script))
    if not pattern:
        return fail(say, "permanent", f"the Write node {write_name} has no file")
    try:
        views = list(nuke.views())
    except Exception:  # noqa: BLE001
        views = []

    say(f"openjd_status: rendering {write_name}, frames {frames[0]}-{frames[-1]}")
    for i, frame in enumerate(frames):
        files = [map_path(frame_file(p, frame), rules) for p in view_files(pattern, views)]
        for f in files:
            if os.path.dirname(f):
                os.makedirs(os.path.dirname(f), exist_ok=True)
        try:
            nuke.execute(write, frame, frame, 1)
        except Exception as e:  # noqa: BLE001
            reason = f"Nuke failed on frame {frame}: {e}".replace("\n", " ")
            return fail(say, "permanent" if MISSING_INPUT.search(str(e)) else "retry", reason)
        missing = [f for f in files if not os.path.isfile(f)]
        if missing:
            return fail(say, "retry", f"Nuke rendered frame {frame} but did not write {missing[0]}")
        c4tool.frame_done(say, frame, files[0], i + 1, len(frames))
    return 0


def main(argv: List[str], env: Mapping[str, str], nuke=None, say: Optional[Callable[[str], None]] = None) -> int:
    say = say or c4tool.say
    if len(argv) < 2:
        return fail(say, "permanent", "usage: nuke -t nuke_render.py SCRIPT WRITE [PROXY]")
    script, write_name = argv[0], argv[1]
    proxy = as_bool(argv[2]) if len(argv) > 2 else False
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
    if nuke is None:
        import nuke  # noqa: PLC0415 - only inside Nuke
    return render(nuke, script, write_name, proxy, frames, rules, say)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:], os.environ))
