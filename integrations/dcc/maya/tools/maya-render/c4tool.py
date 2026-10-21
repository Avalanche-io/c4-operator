"""What the DCC tool scripts share: this unit's frames, the farm's path
map, the lines a unit prints, and which frames a render has finished.

A published tool is one self-contained folder (the farm mounts its tree),
so each tool folder that uses this carries its own copy, beside its
script, which imports it::

    import c4tool

The source is ``integrations/dcc/toolkit/c4tool.py``; a test there fails
when a tool folder's copy differs from it. Change the source, then copy it
into every tool folder that has one.

Standard library only, Python 3.9 and up.
"""

import json
import os
import re
import subprocess
from typing import Callable, Dict, Iterable, List, Mapping, Optional, Sequence, Tuple

#: The image files a render writes, unless a tool names its own.
IMAGE_EXTENSIONS = (".exr", ".tif", ".tiff", ".png", ".jpg", ".jpeg", ".iff", ".dpx", ".tga", ".hdr", ".sgi",
                    ".bmp", ".deepexr")

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
    """Frames as (start, end, step) runs of evenly spaced frames, in order,
    each one call of a renderer that takes a start, an end and a step."""
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
    ``C4_PATH_MAP`` names, longest source first; [] with no file. The farm
    writes it from the submitter's roots (``C4_SOURCE_ROOTS``)."""
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


# --- what a unit prints ---------------------------------------------------------------------

def say(line: str) -> None:
    """A line on stdout, at once, where the farm reads it."""
    print(line, flush=True)


def fail(say: Callable[[str], None], kind: str, reason: str) -> int:
    """Says why the unit failed (``kind`` is ``permanent`` when no retry
    can help, else ``retry``) and returns the exit status: 2 or 1."""
    say(f"c4_fail: {kind} {reason}")
    return 2 if kind == "permanent" else 1


def frame_done(say: Callable[[str], None], frame: int, path: str, done: int, total: int) -> None:
    """A finished frame and its file, and the unit's progress: ``done``
    frames of ``total``."""
    say(f"c4_frame: {frame} {path}")
    say(f"openjd_progress: {round(100 * done / total)}")


def stream(popen, cmd: Sequence[str], env: Mapping[str, str], cwd: str, say: Callable[[str], None],
           on_line: Optional[Callable[[str], None]] = None) -> int:
    """Runs a command, passing each line of its output (and its errors)
    through ``say`` and to ``on_line``; its exit status."""
    proc = popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=env, cwd=cwd,
                 universal_newlines=True, errors="replace")
    for line in proc.stdout:
        line = line.rstrip("\n")
        say(line)
        if on_line:
            on_line(line)
    return proc.wait()


# --- which frames are finished -----------------------------------------------------------------

class Images:
    """Which files are a render's images, by their extensions, and the
    frame each holds."""

    def __init__(self, extensions: Sequence[str] = IMAGE_EXTENSIONS):
        self.extensions = tuple(extensions)
        # A path to an image in a log line: absolute, ending in an image extension.
        self._in_line = re.compile(r"((?:[A-Za-z]:)?[\\/][^\s'\"`<>|]*?\.(?:" +
                                   "|".join(e[1:] for e in self.extensions) + r"))(?=[\s'\"`]|$)", re.I)

    def paths(self, line: str) -> List[str]:
        """The image files a log line names."""
        return [m.group(1) for m in self._in_line.finditer(line)]

    def frame_of(self, path: str) -> Optional[int]:
        """A frame's number from its file's name: the last number before the extension."""
        stem = os.path.basename(path)
        for ext in self.extensions:
            if stem.lower().endswith(ext):
                stem = stem[:-len(ext)]
                break
        m = _LAST_NUMBER.search(stem)
        return int(m.group(1)) if m else None

    def new(self, top: str, since: float, frames: Iterable[int]) -> Dict[int, str]:
        """Image files under ``top`` written since ``since``, by frame, for
        the frames given: the first by name when a frame has several (its AOVs)."""
        want, out = set(frames), {}
        if not top or not os.path.isdir(top):
            return out
        for d, dirs, names in os.walk(top):
            dirs[:] = sorted(x for x in dirs if x != ".c4")
            for name in sorted(names):
                if not name.lower().endswith(self.extensions):
                    continue
                f = self.frame_of(name)
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
    """Reports frames as they finish. A renderer that goes through its
    frames in order has finished every earlier frame with a file once it
    names a file for a later one. Only files written during this run
    count, so a texture or a plate the log names is never taken for a frame."""

    def __init__(self, frames: List[int], since: float, say: Callable[[str], None], images: Optional[Images] = None):
        self.frames, self.since, self.say = list(frames), since, say
        self.images = images or Images()
        self.files: Dict[int, str] = {}
        self.done: List[int] = []

    def _new(self, path: str) -> bool:
        try:
            return os.path.getmtime(path) >= self.since
        except OSError:
            return True   # not there yet: being written; checked again when reported

    def saw(self, path: str) -> None:
        f = self.images.frame_of(path)
        if f not in self.frames or not self._new(path):
            return
        self.files.setdefault(f, path)
        for g in self.frames:
            if g < f and g in self.files:
                self.report(g)

    def saw_line(self, line: str) -> None:
        """Each image file a log line names."""
        for p in self.images.paths(line):
            self.saw(p)

    def report(self, frame: int) -> None:
        if frame in self.done or not os.path.isfile(self.files.get(frame, "")):
            return
        self.done.append(frame)
        frame_done(self.say, frame, self.files[frame], len(self.done), len(self.frames))

    def finish(self, found: Mapping[int, str]) -> List[int]:
        """Report the rest, from the files found after the run; the frames
        still without a file."""
        for f in self.frames:
            if f not in self.files or not os.path.isfile(self.files[f]):
                if f in found:
                    self.files[f] = found[f]
            self.report(f)
        return [f for f in self.frames if f not in self.done]
