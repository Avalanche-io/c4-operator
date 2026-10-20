"""The ID of a tree, computed the way the farm computes a view's
(c4-operator ``internal/view``), so a front end can name a tree before
any of it is uploaded.

A view's ID is the C4 ID of its root record: the one-level canonical c4m
listing of the directory's children, files first and then directories,
each in natural order. Timestamps are null everywhere; mode is null except
on executable files, which record ``-rwxr-xr-x``. A directory's entry
carries the ID of its own record and a size propagated from its children
as c4m does it. ``c4-operator tree-id DIR`` prints the same ID for the same
folder on disk, and ``POST /views/{project}`` answers it for the same file
list.
"""

import functools
from typing import Dict, List, Optional, Tuple

from c4submit.c4id import identify

EXEC_MODE = "-rwxr-xr-x"


class UnsupportedName(ValueError):
    """A name the local computation does not encode (a control character,
    a backslash or ``¤``). The farm can still name such a tree; it just
    has to be uploaded first."""


def _int64(n: int) -> int:
    n &= (1 << 64) - 1
    return n - (1 << 64) if n >> 63 else n


def _segments(s: str) -> List[Tuple[str, bool, int]]:
    out, cur, numeric = [], "", False
    for i, ch in enumerate(s):
        digit = "0" <= ch <= "9"
        if i == 0:
            cur, numeric = ch, digit
        elif digit != numeric:
            out.append(cur)
            cur, numeric = ch, digit
        else:
            cur += ch
    if cur:
        out.append(cur)
    segs = []
    for text in out:
        isnum = "0" <= text[0] <= "9"
        value = 0
        if isnum:
            for ch in text:
                value = _int64(value * 10 + ord(ch) - 48)
        segs.append((text, isnum, value))
    return segs


def natural_compare(a: str, b: str) -> int:
    """c4m's natural order: numbers by value (``2`` before ``10``; equal
    values, shorter first), text before numbers, text by code point."""
    sa, sb = _segments(a), _segments(b)
    for (ta, na, va), (tb, nb, vb) in zip(sa, sb):
        if na and nb:
            if va != vb:
                return -1 if va < vb else 1
            if len(ta) != len(tb):
                return -1 if len(ta) < len(tb) else 1
        elif na != nb:
            return 1 if na else -1
        elif ta != tb:
            return -1 if ta < tb else 1
    if len(sa) != len(sb):
        return -1 if len(sa) < len(sb) else 1
    return 0


natural_key = functools.cmp_to_key(natural_compare)


def _check_name(name: str) -> None:
    for ch in name:
        if ch in "\\¤" or not (ch.isprintable() or ch == " "):
            raise UnsupportedName(f"{name!r}: has a character the tree ID computed here does not encode")


def _escape(s: str, brackets: bool) -> str:
    specials = ' "[]' if brackets else ' "'
    if not any(c in specials for c in s):
        return s
    return "".join("\\" + c if c in specials else c for c in s)


def _line(mode: str, size: int, name: str, cid: Optional[str], target: Optional[str] = None) -> str:
    if name.endswith("/"):
        shown = _escape(name[:-1], True) + "/"
    else:
        shown = _escape(name, True)
    parts = [mode, "-", "-" if size < 0 else str(size), shown]
    if target:
        parts += ["->", _escape(target, False)]
    parts.append(cid or "-")
    return " ".join(parts)


class _File:
    __slots__ = ("id", "size", "exec", "target")

    def __init__(self, cid: Optional[str], size: int, exec_: bool, target: Optional[str]):
        self.id, self.size, self.exec, self.target = cid, size, exec_, target

    def line(self, name: str) -> str:
        if self.target is not None:
            return _line("-", -1, name, None, self.target)
        return _line(EXEC_MODE if self.exec else "-", self.size, name, self.id)


class _Dir:
    def __init__(self):
        self.files: Dict[str, _File] = {}
        self.dirs: Dict[str, "_Dir"] = {}

    def identify(self) -> Tuple[str, int]:
        """The directory's (ID, size), as its parent lists it."""
        files = sorted(self.files, key=natural_key)
        dirs = sorted(self.dirs, key=natural_key)
        lines = [self.files[n].line(n) for n in files]
        sums_null, total, nbytes = False, 0, 0
        for n in files:
            f = self.files[n]
            line = f.line(n)
            if f.target is not None or f.size < 0:
                sums_null = True
            else:
                total += f.size
            nbytes += len(line.encode("utf-8")) + 1
        for n in dirs:
            cid, size = self.dirs[n].identify()
            lines.append(_line("-", size, n + "/", cid))
            # c4m's propagation reads a child directory of null size as
            # empty, so it counts as 0 here, as the operator records it.
            counted = max(size, 0)
            total += counted
            nbytes += len(_line("-", counted, n + "/", cid).encode("utf-8")) + 1
        record = "".join(line + "\n" for line in lines)
        cid = identify(record)
        if not lines:
            return cid, 0
        return cid, -1 if sums_null else total + nbytes


class Tree:
    """A tree of stored objects, built from paths, as ``POST /views``
    takes it."""

    def __init__(self):
        self._root = _Dir()
        self._files: Dict[str, dict] = {}
        self._links: Dict[str, str] = {}
        self._dirs: List[str] = []

    @staticmethod
    def _split(path: str) -> List[str]:
        parts = path.split("/")
        if not path or path.startswith("/") or any(p in ("", ".", "..") for p in parts):
            raise ValueError(f"{path!r}: a tree path is relative, clean and slash-separated")
        for p in parts:
            _check_name(p)
        return parts

    def _parent(self, parts: List[str]) -> _Dir:
        d = self._root
        for p in parts[:-1]:
            if p in d.files:
                raise ValueError(f"{'/'.join(parts)}: {p} is a file")
            d = d.dirs.setdefault(p, _Dir())
        if parts[-1] in d.dirs:
            raise ValueError(f"{'/'.join(parts)}: is a directory")
        return d

    def add_file(self, path: str, cid: str, size: int, exec_: bool = False) -> None:
        parts = self._split(path)
        prev = self._files.get(path)
        if prev and (prev["id"] != cid or prev["size"] != size):
            raise ValueError(f"{path}: added twice with different contents")
        self._parent(parts).files[parts[-1]] = _File(cid, size, exec_, None)
        entry = {"path": path, "id": cid, "size": size}
        if exec_:
            entry["exec"] = True
        self._files[path] = entry

    def add_link(self, path: str, target: str) -> None:
        parts = self._split(path)
        _check_name(target)
        self._parent(parts).files[parts[-1]] = _File(None, -1, False, target)
        self._links[path] = target

    def add_dir(self, path: str) -> None:
        """An empty directory (any other directory is implied by its paths)."""
        parts = self._split(path.rstrip("/"))
        d = self._root
        for p in parts:
            d = d.dirs.setdefault(p, _Dir())
        self._dirs.append(path.rstrip("/") + "/")

    def __len__(self) -> int:
        return len(self._files)

    def __contains__(self, path: str) -> bool:
        return path in self._files

    def id(self) -> str:
        return self._root.identify()[0]

    def view_request(self) -> dict:
        """The body of ``POST /views/{project}`` for this tree."""
        body = {"files": [self._files[p] for p in sorted(self._files)]}
        if self._dirs:
            body["dirs"] = sorted(self._dirs)
        if self._links:
            body["links"] = [{"path": p, "target": t} for p, t in sorted(self._links.items())]
        return body
