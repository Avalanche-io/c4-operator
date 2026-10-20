"""The open Nuke script, as c4submit reads it.

Each Write node that is not disabled is a renderable, so each one ticked is
a job. A Write whose upstream graph reads another Write's render (a Read on
the same files, or a Write with "read file" on) is ``after`` that Write, so
a comp that renders a background first and the main comp over it is two
jobs in the right order. Every file a node reads is a dependency, tagged
with the Writes downstream of it, so unticking a Write leaves out what only
it reads.

Only ``nuke`` is imported, and only by the methods that need it, so the
module loads (and is tested) with a fake ``nuke`` in ``sys.modules``. Every
method runs on Nuke's main thread, as c4submit's Host requires.
"""

import os
import re
from typing import Dict, Iterable, List, Optional, Set

from c4submit import frames as framelib
from c4submit import paths, seq
from c4submit.host import Dependency, Host, Renderable

#: Node classes that write files when rendered: the renderables.
WRITE_CLASSES = ("Write", "DeepWrite", "WriteGeo")
#: Node classes whose file is the one a selection means (``scene.selection``).
READ_CLASSES = ("Read", "DeepRead", "ReadGeo", "ReadGeo2")
#: Nodes whose file knobs are outputs or the viewer's own, never inputs.
NOT_INPUTS = WRITE_CLASSES + ("GenerateLUT", "Viewer")
#: File knobs that Nuke reads only in proxy mode: missing is a warning.
OPTIONAL_KNOBS = ("proxy",)


def _nuke():
    import nuke  # noqa: PLC0415 - only inside Nuke (or a test's fake)
    return nuke


def _knob(node, name: str):
    try:
        return node.knob(name)
    except Exception:  # noqa: BLE001 - Nuke raises its own on odd nodes
        return None


def _on(node, name: str) -> bool:
    k = _knob(node, name)
    try:
        return bool(k.value()) if k is not None else False
    except Exception:  # noqa: BLE001
        return False


def _int(node, name: str) -> Optional[int]:
    k = _knob(node, name)
    if k is None:
        return None
    try:
        return int(k.value())
    except (TypeError, ValueError):
        return None


def is_write(node) -> bool:
    return node.Class() in WRITE_CLASSES


def is_read(node) -> bool:
    return node.Class() in READ_CLASSES


def _range(node, pairs=(("first", "last"), ("range_first", "range_last"))) -> Optional[str]:
    for a, b in pairs:
        first, last = _int(node, a), _int(node, b)
        if first is not None and last is not None:
            return framelib.span(first, last)
    return None


class _Graph:
    """The script's nodes at one moment, with the walks the host needs.
    Built per call, because the artist edits the script between calls."""

    def __init__(self, host: "NukeHost"):
        nuke = _nuke()
        self.host = host
        self.nodes = list(nuke.allNodes(group=nuke.root(), recurseGroups=True))
        self.by_full = {n.fullName(): n for n in self.nodes}
        self.children: Dict[str, List] = {}
        for n in self.nodes:
            full = n.fullName()
            parent = full.rsplit(".", 1)[0] if "." in full else ""
            self.children.setdefault(parent, []).append(n)
        self.writes = [n for n in self.nodes if is_write(n) and not _on(n, "disable")]
        self._files: Dict[str, List[tuple]] = {}
        self._up: Dict[str, Set[str]] = {}

    # --- files ------------------------------------------------------------

    def files(self, node) -> List[tuple]:
        """(path, knob name) for every file the node reads: each file knob
        with a value, made absolute. A Write's output counts only when it
        reads it back ("read file" on)."""
        full = node.fullName()
        if full in self._files:
            return self._files[full]
        out: List[tuple] = []
        if is_write(node):
            if _on(node, "reading"):
                out = [(p, "file") for p in self.host.outputs(node)]
        elif node.Class() not in NOT_INPUTS and not _on(node, "disable") and not (
                _knob(node, "read_from_file") is not None and not _on(node, "read_from_file")):
            for name, k in sorted((node.knobs() or {}).items()):
                if k.Class() != "File_Knob":
                    continue
                p = self.host.knob_path(node, k)
                if p:
                    out.append((p, name))
        self._files[full] = out
        return out

    # --- the graph upstream of a node ---------------------------------------

    def _group_of(self, node):
        full = node.fullName()
        return self.by_full.get(full.rsplit(".", 1)[0]) if "." in full else None

    def _feeds(self, node) -> List:
        """The nodes whose output this node takes: its inputs, through
        groups (a group's Output inside it; an Input to the group's own
        input), and nodes it reads by expression."""
        full = node.fullName()
        inner = self.children.get(full)
        if inner and node.Class() != "Root":
            outs = [n for n in inner if n.Class() == "Output"]
            if outs:
                return outs
        if node.Class() == "Input":
            group = self._group_of(node)
            if group is None:
                return []
            num = _int(node, "number")
            if num is None:
                m = re.search(r"(\d+)$", node.name())
                num = int(m.group(1)) - 1 if m else 0
            try:
                n = group.input(num)
            except Exception:  # noqa: BLE001
                n = None
            return [n] if n is not None else []
        out = []
        try:
            count = node.inputs()
        except Exception:  # noqa: BLE001
            count = 0
        for i in range(count):
            n = node.input(i)
            if n is not None:
                out.append(n)
        try:
            out += list(node.dependencies(_nuke().EXPRESSIONS))
        except Exception:  # noqa: BLE001 - no expression links, or an older Nuke
            pass
        return out

    def upstream(self, node) -> Set[str]:
        """Full names of every node the given one's pixels come from. Stops
        at a Write that reads its own file back, since its render, not its
        inputs, is what flows on."""
        full = node.fullName()
        if full in self._up:
            return self._up[full]
        seen: Set[str] = set()
        todo = self._feeds(node)
        while todo:
            n = todo.pop()
            f = n.fullName()
            if f in seen or f == full:
                continue
            seen.add(f)
            if is_write(n) and _on(n, "reading"):
                continue
            todo.extend(self._feeds(n))
        self._up[full] = seen
        return seen


class NukeHost(Host):
    """The open script, read through the ``nuke`` module."""

    name = "nuke"
    stage = "comp"
    scene_word = "script"

    # --- the script ------------------------------------------------------

    def scene_file(self) -> str:
        name = _nuke().root().name() or ""
        return "" if name in ("", "Root") else name

    def is_modified(self) -> bool:
        return bool(_nuke().root().modified())

    def save(self) -> None:
        if not self.scene_file():
            raise RuntimeError("the script has never been saved; save it with File > Save As first")
        _nuke().scriptSave()

    def frames(self) -> str:
        root = _nuke().root()
        first, last = _int(root, "first_frame"), _int(root, "last_frame")
        if first is None or last is None:
            return ""
        return framelib.span(first, last)

    # --- paths ------------------------------------------------------------

    def project_dir(self) -> str:
        """Where relative paths in the script point: the root's project
        directory, else the script's folder."""
        k = _knob(_nuke().root(), "project_directory")
        d = ""
        if k is not None:
            try:
                d = k.evaluate() if hasattr(k, "evaluate") else k.value()
            except Exception:  # noqa: BLE001 - a broken expression in the knob
                d = k.value()
        d = (d or "").strip()
        if not d:
            d = os.path.dirname(self.scene_file())
        return d

    def absolute(self, path: str) -> str:
        path = (path or "").strip()
        if not path or paths.is_absolute(path):
            return path
        base = self.project_dir()
        return paths.norm(base.rstrip("/\\") + "/" + path) if base else path

    def knob_path(self, node, knob) -> str:
        """A file knob's path as Nuke reads it, frame tokens kept. The
        ``file`` knob goes through ``nuke.filename`` (expressions evaluated);
        another knob is evaluated only when it holds an expression, since
        evaluating fills in the current frame."""
        if knob.name() == "file":
            p = _nuke().filename(node)
        else:
            p = knob.value()
            if p and "[" in p:
                try:
                    p = knob.evaluate()
                except Exception:  # noqa: BLE001
                    pass
        return self.absolute(p or "")

    def outputs(self, write) -> List[str]:
        p = self.absolute(_nuke().filename(write) or "")
        return [p] if p else []

    @staticmethod
    def views(path: str) -> List[str]:
        """A stereo path (``%V``, ``%v``) as one path per view in the script."""
        if "%V" not in path and "%v" not in path:
            return [path]
        try:
            views = list(_nuke().views())
        except Exception:  # noqa: BLE001
            views = []
        if not views:
            return [path]
        return [path.replace("%V", v).replace("%v", v[:1].lower()) for v in views]

    # --- what renders -------------------------------------------------------

    def renderables(self) -> List[Renderable]:
        g = _Graph(self)
        made: Dict[str, str] = {}
        for w in g.writes:
            for o in self.outputs(w):
                made.setdefault(seq.canonical(o), w.fullName())
        out = []
        for w in g.writes:
            name = w.fullName()
            reads: List[str] = []
            for f in sorted(g.upstream(w)):
                n = g.by_full.get(f)
                if n is None:
                    continue
                for p, _ in g.files(n):
                    by = made.get(seq.canonical(p))
                    if by and by != name and by not in reads:
                        reads.append(by)
            frames = _range(w) if _on(w, "use_limit") else None
            note = ""
            if reads:
                note = "after " + ", ".join(reads)
            out.append(Renderable(name=name, label=name, frames=frames, outputs=self.outputs(w),
                                  reads=reads, note=note))
        return out

    def dependencies(self) -> List[Dependency]:
        nuke = _nuke()
        g = _Graph(self)
        ups = {w.fullName(): g.upstream(w) for w in g.writes}
        out: List[Dependency] = []
        for n in g.nodes:
            files = g.files(n)
            if not files:
                continue
            full = n.fullName()
            needed = [w for w, up in ups.items() if full in up]
            frames = _range(n) if not is_write(n) else (_range(n) if _on(n, "use_limit") else None)
            for p, knob in files:
                for v in self.views(p):
                    out.append(Dependency(path=v, frames=frames if seq.has_frames(v) else None, node=full,
                                          renderables=list(needed), optional=knob in OPTIONAL_KNOBS))
        out += self._ocio(nuke.root())
        return out

    def _ocio(self, root) -> List[Dependency]:
        """The root's custom OCIO config, with the files under its search
        paths (the LUTs it names), which every job reads."""
        k = _knob(root, "customOCIOConfigPath")
        if k is None:
            return []
        which = _knob(root, "OCIO_config")
        if which is not None and str(which.value()) != "custom":
            return []
        cfg = self.absolute(k.value() or "")
        if not cfg:
            return []
        out = [Dependency(path=cfg, node="Project Settings > Color")]
        for d in ocio_search_dirs(cfg):
            for top, _, names in os.walk(d):
                for name in sorted(names):
                    out.append(Dependency(path=os.path.join(top, name), node="Project Settings > Color"))
        return out

    def selection(self) -> List[str]:
        out = []
        for n in _nuke().selectedNodes():
            if is_read(n):
                p = self.absolute(_nuke().filename(n) or "")
                if p:
                    out.append(p)
        return out


def ocio_search_dirs(config: str) -> Iterable[str]:
    """The folders an OCIO config's ``search_path`` names, relative to the
    config, that exist. Both spellings: ``a:b`` and ``[a, b]``."""
    try:
        with open(config, encoding="utf-8", errors="replace") as f:
            text = f.read()
    except OSError:
        return []
    m = re.search(r"^search_path\s*:\s*(.+)$", text, re.M)
    if not m:
        return []
    raw = m.group(1).strip()
    if raw.startswith("["):
        items = [s.strip().strip("'\"") for s in raw.strip("[]").split(",")]
    else:
        items = raw.strip("'\"").split(":")
    base = os.path.dirname(config)
    out = []
    for s in items:
        s = os.path.expandvars(s.strip())
        if not s:
            continue
        d = s if os.path.isabs(s) else os.path.join(base, s)
        if os.path.isdir(d) and d not in out:
            out.append(d)
    return out
