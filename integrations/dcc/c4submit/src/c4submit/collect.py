"""Collecting a scene's files into a tree: every file the scene reads, at
its path under the project's root, named by its C4 ID. Only what the farm
lacks is uploaded later; this step reads the disk and nothing else.

IDs are cached by path, size and modification time
(``~/.cache/c4submit/ids.json``, or ``C4SUBMIT_CACHE``), so submitting a
shot again hashes only what changed.
"""

import json
import os
import tempfile
import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from typing import Callable, Dict, Iterable, List, Optional, Tuple

from c4submit import frames as framelib
from c4submit import seq
from c4submit.c4id import Interrupted, identify_file
from c4submit.host import Dependency
from c4submit.paths import norm, relative
from c4submit.tree import Tree, UnsupportedName

Progress = Callable[[str, int, int], None]


class Cancelled(Exception):
    """The artist cancelled."""


@dataclass
class Problem:
    level: str            # "error" or "warning"
    path: str             # what it is about: a file, a node, a field
    message: str
    fix: str = ""

    def as_error(self) -> Dict[str, str]:
        e = {"path": self.path, "message": self.message}
        if self.fix:
            e["fix"] = self.fix
        return e


@dataclass
class File:
    rel: str              # path in the tree
    abs: str              # path on this machine
    size: int = 0
    id: str = ""
    exec: bool = False


@dataclass
class Collection:
    root: str
    files: Dict[str, File] = field(default_factory=dict)
    problems: List[Problem] = field(default_factory=list)
    tree: Optional[Tree] = None

    @property
    def bytes(self) -> int:
        return sum(f.size for f in self.files.values())

    @property
    def errors(self) -> List[Problem]:
        return [p for p in self.problems if p.level == "error"]

    def tree_id(self) -> str:
        return self.tree.id() if self.tree is not None else ""


class IDCache:
    """C4 IDs of files by (path, size, mtime), kept across submissions."""

    def __init__(self, path: Optional[str] = None):
        self.path = path or os.environ.get("C4SUBMIT_CACHE") or os.path.join(
            os.path.expanduser("~"), ".cache", "c4submit", "ids.json")
        self._lock = threading.Lock()
        self._data: Optional[Dict[str, list]] = None
        self._dirty = False

    def _load(self) -> Dict[str, list]:
        if self._data is None:
            try:
                with open(self.path, encoding="utf-8") as f:
                    self._data = json.load(f)
            except (OSError, ValueError):
                self._data = {}
        return self._data

    def get(self, path: str, size: int, mtime: int) -> str:
        with self._lock:
            v = self._load().get(path)
        return v[2] if v and v[0] == size and v[1] == mtime else ""

    def put(self, path: str, size: int, mtime: int, cid: str) -> None:
        with self._lock:
            self._load()[path] = [size, mtime, cid]
            self._dirty = True

    def save(self) -> None:
        with self._lock:
            if not self._dirty or self._data is None:
                return
            d = os.path.dirname(self.path)
            try:
                os.makedirs(d, exist_ok=True)
                fd, tmp = tempfile.mkstemp(dir=d, prefix=".ids-")
                with os.fdopen(fd, "w", encoding="utf-8") as f:
                    json.dump(self._data, f)
                os.replace(tmp, self.path)
                self._dirty = False
            except OSError:
                pass  # a cache that cannot be written only costs time


class NoCache(IDCache):
    def __init__(self):
        super().__init__(path=os.devnull)
        self._data = {}

    def save(self) -> None:
        pass


def gather(root: str, scene_file: str, deps: Iterable[Dependency]) -> Collection:
    """The files a scene reads, by their paths, with what is missing or
    outside the root. Reads the disk but hashes nothing."""
    col = Collection(root=norm(root))
    if not root:
        col.problems.append(Problem("error", "root", "no project folder is set, so nothing can be placed in the job's tree",
                                    "set C4_ROOT to the folder the farm mounts as the project's root (for example /show)"))
        return col

    real_root = os.path.realpath(root)

    def add(path: str, who: str) -> None:
        rel = relative(root, path)
        if rel is None:
            col.problems.append(Problem("error", who or path, f"{path} is outside the project folder {col.root}",
                                        f"move it under {col.root}; a job reads only its tree"))
            return
        # The spelling is under the root; what is read must be too. A link
        # (to the file, or to a folder on the way) that leads out of the
        # project would send this machine's other files to the farm.
        real = os.path.realpath(path)
        if relative(real_root, real) is None:
            col.problems.append(Problem("error", who or path, f"{path} is a link to {norm(real)}, outside the project folder {col.root}",
                                        "copy the file into the project folder instead of linking to it; "
                                        "c4 sends only files under the project folder"))
            return
        if rel not in col.files:
            col.files[rel] = File(rel=rel, abs=path)

    if not scene_file:
        col.problems.append(Problem("error", "scene", "the scene has never been saved", "save it first; the farm reads the saved file"))
    elif not os.path.isfile(scene_file):
        col.problems.append(Problem("error", "scene", f"{scene_file} does not exist", "save the scene"))
    else:
        add(scene_file, "scene")
    for d in deps:
        who = d.node or d.path
        try:
            frames = framelib.parse(d.frames) if d.frames and seq.has_frames(d.path) else None
        except framelib.FrameError as e:
            col.problems.append(Problem("error", who, f"{d.path}: its frames {d.frames!r} do not read: {e}"))
            continue
        found, missing = seq.expand(d.path, frames)
        if not found:
            level = "warning" if d.optional else "error"
            col.problems.append(Problem(level, who, f"{d.path}: no such file" + ("s" if seq.has_frames(d.path) or seq.has_tiles(d.path) else ""),
                                        "fix the path, or render or copy the files first"))
            continue
        if missing:
            col.problems.append(Problem("warning", who, f"{d.path} lacks frames {framelib.format(missing)}",
                                        "render or copy them, or the units that read them will fail"))
        for p in found:
            add(p, who)
    return col


def identify(col: Collection, cache: Optional[IDCache] = None, progress: Optional[Progress] = None,
             cancelled: Optional[Callable[[], bool]] = None, workers: int = 0) -> Collection:
    """Hashes every file (cached ones are not re-read) and builds the tree."""
    cache = cache or IDCache()
    files = list(col.files.values())
    done, total, lock = [0], len(files), threading.Lock()

    def one(f: File) -> Tuple[File, Optional[str]]:
        if cancelled and cancelled():
            raise Cancelled()
        try:
            st = os.stat(f.abs)
            f.size, f.exec = st.st_size, bool(st.st_mode & 0o111) and os.name != "nt"
            mtime = int(st.st_mtime_ns)
            cid = cache.get(f.abs, f.size, mtime)
            if not cid:
                try:
                    cid = identify_file(f.abs, cancelled)
                except Interrupted:
                    raise Cancelled() from None
                cache.put(f.abs, f.size, mtime, cid)
            f.id = cid
            return f, None
        except OSError as e:
            return f, str(e)
        finally:
            with lock:
                done[0] += 1
                n = done[0]
            if progress:
                progress("hash", n, total)

    n = workers or min(8, (os.cpu_count() or 2))
    with ThreadPoolExecutor(max_workers=n) as ex:
        results = list(ex.map(one, files))
    cache.save()
    tree = Tree()
    for f, err in results:
        if err:
            col.problems.append(Problem("error", f.rel, f"cannot read {f.abs}: {err}"))
            del col.files[f.rel]
            continue
        try:
            tree.add_file(f.rel, f.id, f.size, f.exec)
        except UnsupportedName as e:
            col.problems.append(Problem("error", f.rel, str(e), "rename the file"))
            del col.files[f.rel]
        except ValueError as e:
            col.problems.append(Problem("error", f.rel, str(e)))
            del col.files[f.rel]
    col.tree = tree
    return col
