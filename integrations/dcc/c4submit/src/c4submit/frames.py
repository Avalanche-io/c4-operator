"""Frame lists in Open Job Description's range grammar, which a submission's
``frames`` uses: ``1001-1240``, ``1001-1240:2``, ``1001,1050-1100``."""

import re
from typing import Iterable, List

_ITEM = re.compile(r"^\s*(-?\d+)\s*(?:-\s*(-?\d+)\s*(?::\s*(\d+)\s*)?)?$")

# A frame list longer than this is almost certainly a typo (a missing
# comma joining two frames); the farm would refuse it anyway.
MAX_FRAMES = 1_000_000


class FrameError(ValueError):
    pass


def parse(expr) -> List[int]:
    """The frames of an expression, ascending and without duplicates. An int
    is one frame."""
    if isinstance(expr, bool):
        raise FrameError("frames are numbers or a range expression, not true or false")
    if isinstance(expr, int):
        return [expr]
    if not isinstance(expr, str) or not expr.strip():
        raise FrameError("no frames: write a range like 1001-1100")
    out = set()
    for item in expr.split(","):
        m = _ITEM.match(item)
        if not m:
            raise FrameError(f"{item.strip()!r} is not a frame or a range (1001, 1001-1100, 1001-1100:2)")
        start = int(m.group(1))
        if m.group(2) is None:
            out.add(start)
            continue
        end, step = int(m.group(2)), int(m.group(3) or 1)
        if step < 1:
            raise FrameError(f"{item.strip()!r}: the step after ':' is 1 or more")
        if end < start:
            raise FrameError(f"{item.strip()!r}: a range runs from its first frame to its last")
        if (end - start) // step + 1 > MAX_FRAMES:
            raise FrameError(f"{item.strip()!r}: more than {MAX_FRAMES:,} frames")
        out.update(range(start, end + 1, step))
    return sorted(out)


def format(frames: Iterable[int]) -> str:  # noqa: A001 - the module's verb
    """The shortest expression of a set of frames in the same grammar, with
    runs of an even step written ``a-b:s``."""
    fs = sorted(set(frames))
    parts, i = [], 0
    while i < len(fs):
        j = i
        if i + 1 < len(fs):
            step = fs[i + 1] - fs[i]
            while j + 1 < len(fs) and fs[j + 1] - fs[j] == step:
                j += 1
            if step > 1 and j - i < 2:
                j = i  # two frames a step apart read better as a list
        if j == i:
            parts.append(str(fs[i]))
        elif fs[i + 1] - fs[i] == 1:
            parts.append(f"{fs[i]}-{fs[j]}")
        else:
            parts.append(f"{fs[i]}-{fs[j]}:{fs[i + 1] - fs[i]}")
        i = j + 1
    return ",".join(parts)


def span(first: int, last: int, step: int = 1) -> str:
    """The expression of a DCC's first, last and step."""
    if last < first:
        first, last = last, first
    if first == last:
        return str(first)
    return f"{first}-{last}" if step <= 1 else f"{first}-{last}:{step}"


def check(expr) -> str:
    """An error for an expression that does not parse, else ''."""
    try:
        parse(expr)
    except FrameError as e:
        return str(e)
    return ""
