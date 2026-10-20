"""Paths as an artist's machine writes them (POSIX or Windows), compared
the way that machine compares them."""

import ntpath
import posixpath
from typing import Optional


def is_windows(p: str) -> bool:
    return bool(ntpath.splitdrive(p)[0]) or p.startswith("\\\\")


def is_absolute(p: str) -> bool:
    return p.startswith("/") or is_windows(p)


def norm(p: str) -> str:
    """A path in one spelling: forward slashes, no ``.`` or ``..``."""
    if not p:
        return p
    if is_windows(p):
        return ntpath.normpath(p).replace("\\", "/")
    return posixpath.normpath(p.replace("\\", "/"))


def same(a: str, b: str) -> bool:
    a, b = norm(a).rstrip("/"), norm(b).rstrip("/")
    if is_windows(a) or is_windows(b):
        return a.lower() == b.lower()
    return a == b


def relative(root: str, path: str) -> Optional[str]:
    """The path under root, slash-separated, or None when it is outside.
    Windows paths compare without case, as Windows does."""
    if not root:
        return None
    r, p = norm(root).rstrip("/"), norm(path).rstrip("/")
    fold = is_windows(r) or is_windows(p)
    rr, pp = (r.lower(), p.lower()) if fold else (r, p)
    if pp == rr:
        return "."
    if not pp.startswith(rr + "/"):
        return None
    return p[len(r) + 1:]
