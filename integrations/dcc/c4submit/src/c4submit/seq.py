"""File paths that stand for sequences and tiles, in the spellings DCCs
write them: frames as ``####``, ``%04d``, ``$F4``, ``${F4}`` or ``<F4>``,
and texture tiles as ``<UDIM>``, ``<UVTILE>``, ``%(UDIM)d``, ``_MAPID_`` or
``<U>_<V>``. A front end expands them to the files on disk a job reads."""

import glob
import os
import re
from typing import Iterable, List, Optional, Tuple

from c4submit.paths import is_windows, norm

_FRAME = re.compile(
    r"(?P<hash>#+)"
    r"|%(?P<pct>0?\d*)d"
    r"|\$\{F(?P<brace>\d*)\}"
    r"|\$F(?P<dollar>\d*)(?![A-Za-z0-9_])"   # $F4_x is a variable named F4_x, not a frame
    r"|<F(?P<angle>\d*)>"
)
_TILE = re.compile(r"<UDIM>|<udim>|<UVTILE>|<uvtile>|%\(UDIM\)d|_MAPID_|<U>_<V>|<u>_<v>")


def has_frames(path: str) -> bool:
    return bool(_FRAME.search(path))


def has_tiles(path: str) -> bool:
    return bool(_TILE.search(path))


def _pad(m: "re.Match") -> int:
    if m.group("hash") is not None:
        return len(m.group("hash"))
    for g in ("pct", "brace", "dollar", "angle"):
        v = m.group(g)
        if v is not None:
            return int(v) if v else 0
    return 0


def at_frame(path: str, frame: int) -> str:
    """The path of one frame of a sequence."""
    # As printf pads: the sign counts toward the width (-005 for %04d).
    return _FRAME.sub(lambda m: "%0*d" % (_pad(m), frame), path)


def _pieces(pattern: str):
    """The pattern as literal text and tokens: ('lit', s), ('frame', m) or
    ('tile', s)."""
    tokens = sorted([(m.start(), m.end(), "frame", m) for m in _FRAME.finditer(pattern)]
                    + [(m.start(), m.end(), "tile", m) for m in _TILE.finditer(pattern)],
                    key=lambda t: t[0])
    out, last = [], 0
    for start, end, kind, m in tokens:
        if start < last:
            continue
        if start > last:
            out.append(("lit", pattern[last:start]))
        out.append((kind, m))
        last = end
    if last < len(pattern):
        out.append(("lit", pattern[last:]))
    return out


def _glob(pattern: str) -> str:
    return "".join(glob.escape(p) if k == "lit" else "*" for k, p in _pieces(pattern))


def _regex(pattern: str) -> "re.Pattern":
    rx = ""
    for kind, p in _pieces(pattern):
        if kind == "lit":
            rx += re.escape(p)
        elif kind == "frame":
            rx += r"(?P<f%d>-?\d+)" % len(rx)
        else:
            rx += r"\d+(?:_\d+)?"
    return re.compile(rx)


def frame_of(pattern: str, path: str) -> Optional[int]:
    """The frame a file of the pattern holds, or None when it is not one of
    the pattern's files. (A pattern without frame tokens holds no frame.)"""
    m = _regex(pattern).fullmatch(path)
    if not m or not m.groupdict():
        return None
    values = {int(v) for v in m.groupdict().values()}
    return values.pop() if len(values) == 1 else None


def matches(pattern: str, path: str) -> bool:
    """Whether a file is one of the pattern's files. On Windows, glob joins
    with backslashes, so both are compared in one spelling."""
    if is_windows(pattern) or is_windows(path):
        pattern, path = norm(pattern), norm(path)
    return bool(_regex(pattern).fullmatch(path))


def expand(path: str, frames: Optional[Iterable[int]] = None) -> Tuple[List[str], List[int]]:
    """The files on disk a path stands for, and the frames it lacks.

    A plain path is itself when it exists. A sequence with ``frames`` is
    those frames (and their tiles); without ``frames`` it is every file
    that matches. Missing frames are listed only when ``frames`` is given."""
    if not has_frames(path) and not has_tiles(path):
        return ([path] if os.path.isfile(path) else []), []
    if not has_frames(path) or frames is None:
        found = [p for p in glob.glob(_glob(path)) if os.path.isfile(p) and matches(path, p)]
        return sorted(found), []
    found, missing = [], []
    for f in frames:
        one = at_frame(path, f)
        if has_tiles(one):
            tiles = [p for p in glob.glob(_glob(one)) if os.path.isfile(p) and matches(one, p)]
            found.extend(sorted(tiles))
            if not tiles:
                missing.append(f)
        elif os.path.isfile(one):
            found.append(one)
        else:
            missing.append(f)
    return found, missing


def canonical(path: str) -> str:
    """One spelling of a sequence path, so ``####``, ``%04d`` and ``$F4``
    name the same files: tokens as ``<F4>`` (an unpadded ``#``, ``%d`` and
    ``$F`` all as ``<F1>``), slashes forward, Windows paths in lower case."""
    out = _FRAME.sub(lambda m: "<F%d>" % max(_pad(m), 1), norm(path))
    return out.lower() if is_windows(path) else out
