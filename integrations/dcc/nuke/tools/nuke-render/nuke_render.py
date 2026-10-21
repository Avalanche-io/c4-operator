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

import json
import os
import re
import sys
from typing import Callable, List, Mapping, Optional, Tuple

#: The node classes that write files (the panel's renderables).
WRITE_CLASSES = ("Write", "DeepWrite", "WriteGeo")
#: What Nuke says when a Read's file is not there (a frame or a whole
#: sequence). The files are the job's tree, so another try cannot help.
MISSING_INPUT = re.compile(r"No such file or directory|Read error|file not found", re.I)

_HASHES = re.compile(r"#+")
_PRINTF = re.compile(r"%(0?)(\d*)d")


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


def map_path(path: str, rules: List[Tuple[str, str, str]]) -> str:
    """A path from the artist's machine as it is on the farm. A Windows
    source matches whichever way its slashes lean and in any case."""
    if not path or not rules:
        return path
    for fmt, src, dst in rules:
        if path.startswith(dst.rstrip("/") + "/") or path == dst:
            return path   # already the farm's
        if fmt == "WINDOWS":
            p, s = path.replace("\\", "/"), src.replace("\\", "/").rstrip("/")
            if p.lower() == s.lower() or p.lower().startswith(s.lower() + "/"):
                return dst.rstrip("/") + p[len(s):]
            continue
        s = src.rstrip("/")
        if path == s or path.startswith(s + "/"):
            return dst.rstrip("/") + path[len(s):]
    return path


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

def fail(say: Callable[[str], None], kind: str, reason: str) -> int:
    say(f"c4_fail: {kind} {reason}")
    return 2 if kind == "permanent" else 1


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
        say(f"c4_frame: {frame} {files[0]}")
        say(f"openjd_progress: {round(100 * (i + 1) / len(frames))}")
    return 0


def main(argv: List[str], env: Mapping[str, str], nuke=None, say: Optional[Callable[[str], None]] = None) -> int:
    say = say or (lambda line: print(line, flush=True))
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
