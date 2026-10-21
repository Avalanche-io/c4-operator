"""The open Katana project, as c4submit reads it.

Each Render node that is not bypassed is a renderable, so each one ticked
is a job. Katana's own ``FarmAPI.GetSortedDependencyList`` says, for each
Render node, the Render nodes it depends on (whose renders it reads), its
render outputs and its frame range; a node is ``after`` the ones it depends
on, and one node's outputs are never collected as another's inputs.

Every file a node's parameters name is a dependency: Alembic caches, USD
stages, look files, textures on shading nodes, live groups, anything with
a file widget or a file's name. Each is tagged with the Render nodes
downstream of it in the node graph, so unticking a Render node leaves out
what only it reads. A USD file (a UsdIn's stage, or any parameter naming
one) is followed into what it reads in turn, its sublayers, references,
payloads and textures, with ``c4submit.usd``, each tagged as the node that
reads the stage; a binary layer that cannot be read without USD's Python is
sent and named in ``warnings()``. What a look file or a live group names in
turn is not read: those files must be under the project folder for the job,
and are a known limit.

Only ``Katana``'s ``FarmAPI``, ``NodegraphAPI`` and ``KatanaFile`` are
imported, and only by the functions that need them, so the module loads
(and is tested) with a fake ``Katana`` in ``sys.modules``. Every method
runs on Katana's main thread, as c4submit's Host requires.
"""

import ast
import os
import re
from typing import Dict, List, Optional, Set, Tuple

from c4submit import frames as framelib
from c4submit import paths, seq
from c4submit import usd as usdlib
from c4submit.host import Dependency, Host, Renderable

#: Nodes that are jobs.
RENDER_TYPES = ("Render",)
#: Nodes whose file parameters are where renders go, never inputs.
OUTPUT_TYPES = ("Render", "RenderOutputDefine", "ImageWrite")
#: Parameter widgets that pick a file to read, and ones that pick where to write.
INPUT_WIDGETS = ("assetIdInput", "fileInput", "assetIdInputWithBrowse", "scenegraphLocation_file")
OUTPUT_WIDGETS = ("assetIdOutput", "fileOutput")
#: A string parameter ending in one of these names a file, whatever its widget.
FILE_EXTENSIONS = (
    ".abc", ".usd", ".usda", ".usdc", ".usdz", ".klf", ".livegroup", ".katana", ".tx", ".tex", ".exr", ".tif",
    ".tiff", ".png", ".jpg", ".jpeg", ".hdr", ".hdri", ".ptex", ".ptx", ".vdb", ".ass", ".ass.gz", ".rib",
    ".ocio", ".cube", ".csp", ".spi1d", ".spi3d", ".lut", ".xml", ".bgeo", ".bgeo.sc", ".obj", ".fbx",
)
#: USD layers whose own sublayers, references and assets are followed.
USD_LAYERS = (".usd", ".usda", ".usdc")


def unread_warning(path: str) -> str:
    """What the artist is told about a binary layer USD's Python was not here to read."""
    return (f"{path} is binary USD and USD's Python is not available here, "
            "so what it reads in turn was not collected")


def usd_dependencies(path: str, label: str, renderables: Optional[List[str]]) -> Tuple[List[Dependency], List[str]]:
    """What a USD file reads in turn, tagged with the renderables of the
    node that reads it, and the binary layers that could not be read. A
    path that is not a USD layer on disk (a sequence, an asset ID) reads
    nothing here."""
    if not path.lower().endswith(USD_LAYERS) or seq.has_frames(path) or not os.path.isfile(path):
        return [], []
    deps, unread = usdlib.dependencies(path, node=f"{label} > {os.path.basename(path)}")
    for d in deps:
        d.renderables = None if renderables is None else list(renderables)
    return deps, unread


#: How a path begins: a variable, home, a root, a drive, or a folder name and a slash.
_PATH_START = re.compile(r"^(\$|~|/|\\\\|[a-z]:[/\\]|\.{1,2}[/\\]|[\w.-]+[/\\])")


def _katana():
    from Katana import FarmAPI, KatanaFile, NodegraphAPI  # noqa: PLC0415 - only inside Katana (or a fake)
    return FarmAPI, NodegraphAPI, KatanaFile


def _call(fn, *args, default=None):
    try:
        return fn(*args)
    except Exception:  # noqa: BLE001 - Katana raises its own
        return default


def node_key(name: str) -> str:
    """The name a Render node's job key is made from: ``shadow`` for
    ``Render_shadow`` (the stage is ``render`` already)."""
    return re.sub(r"^render[_-]?", "", name, flags=re.I) or name


def bypassed(node) -> bool:
    return bool(_call(node.isBypassed, default=False))


def render_nodes() -> list:
    """The Render nodes that would render: every one not bypassed."""
    _, ng, _ = _katana()
    out = []
    for t in RENDER_TYPES:
        out += [n for n in (ng.GetAllNodesByType(t) or []) if not bypassed(n)]
    return out


def _hint(param) -> dict:
    """A parameter's UI hints (a dict written as text), or {}."""
    text = _call(param.getHintString, default="") or ""
    if not text:
        return {}
    try:
        h = ast.literal_eval(text)
    except (ValueError, SyntaxError):
        return {}
    return h if isinstance(h, dict) else {}


def looks_like_file(value: str) -> bool:
    """Whether a string parameter's value is a file's path: a path with a
    file's extension."""
    v = value.strip().lower()
    if "\n" in v or not _PATH_START.match(v):
        return False
    return v.endswith(FILE_EXTENSIONS)


def param_files(node, frame: float) -> List[Tuple[str, str]]:
    """(path, parameter) for every file a node's parameters name to read.
    A shading node's parameter that is not enabled is its shader's default,
    so it is skipped."""
    out: List[Tuple[str, str]] = []

    def walk(p) -> None:
        kids = _call(p.getChildren, default=[]) or []
        if kids:
            by = {k.getName(): k for k in kids}
            if "enable" in by and "value" in by and not _call(by["enable"].getValue, frame, default=1):
                return
            for k in kids:
                walk(k)
            return
        if _call(p.getType, default="") != "string":
            return
        v = _call(p.getValue, frame, default="")
        if not isinstance(v, str) or not v.strip():
            return
        widget = _hint(p).get("widget", "")
        if widget in OUTPUT_WIDGETS:
            return
        if widget in INPUT_WIDGETS or looks_like_file(v):
            out.append((v.strip(), _call(p.getFullName, default=p.getName()) or p.getName()))

    root = _call(node.getParameters)
    if root is not None:
        walk(root)
    return out


class _Graph:
    """The node graph at one moment, with the walk upstream the host needs.
    Built per call, because the artist edits the graph between calls."""

    def __init__(self):
        self._up: Dict[str, Set[str]] = {}

    @staticmethod
    def _descendants(group) -> List:
        out, todo = [], list(_call(group.getChildren, default=[]) or [])
        while todo:
            n = todo.pop()
            out.append(n)
            todo.extend(_call(n.getChildren, default=[]) or [])
        return out

    def upstream(self, node) -> Set[str]:
        """Names of every node the given one's scene comes from. A group
        upstream counts with everything inside it; a node inside a group
        fed from the group's input goes on from the group's own input."""
        name = node.getName()
        if name in self._up:
            return self._up[name]
        seen: Set[str] = set()
        todo = [node]
        while todo:
            cur = todo.pop()
            parent = _call(cur.getParent)
            for port in _call(cur.getInputPorts, default=[]) or []:
                for other in _call(port.getConnectedPorts, default=[]) or []:
                    src = other.getNode()
                    if parent is not None and src is parent:
                        # The group's send port: the group's own input of that name.
                        outer = _call(parent.getInputPort, other.getName())
                        for o in (_call(outer.getConnectedPorts, default=[]) or []) if outer is not None else []:
                            n = o.getNode()
                            if n.getName() not in seen:
                                seen.add(n.getName())
                                todo.append(n)
                        continue
                    if src.getName() in seen:
                        continue
                    seen.add(src.getName())
                    todo.append(src)
                    for inner in self._descendants(src):
                        seen.add(inner.getName())
        seen.discard(name)
        self._up[name] = seen
        return seen


class KatanaHost(Host):
    """The open project, read through Katana's Python."""

    name = "katana"
    stage = "render"
    scene_word = "project"

    # --- the project ------------------------------------------------------

    def scene_file(self) -> str:
        farm, ng, _ = _katana()
        path = _call(farm.GetKatanaFileName, default=None)
        if path is None:
            path = _call(ng.GetSourceFile, default="")
        return path or ""

    def is_modified(self) -> bool:
        _, _, kf = _katana()
        return bool(_call(kf.IsFileDirty, default=False))

    def save(self) -> None:
        path = self.scene_file()
        if not path:
            raise RuntimeError("the project has never been saved; save it with File > Save As first")
        _, _, kf = _katana()
        kf.Save(path)

    def frames(self) -> str:
        """The project's frame range: FarmAPI's, else the root node's in
        and out times."""
        farm, ng, _ = _katana()
        r = _call(farm.GetSceneFrameRange, default=None)
        if isinstance(r, dict) and r.get("start") is not None and r.get("end") is not None:
            return framelib.span(int(round(r["start"])), int(round(r["end"])))
        root = _call(ng.GetRootNode)
        if root is None:
            return ""
        first = _call(lambda: root.getParameter("inTime").getValue(0))
        last = _call(lambda: root.getParameter("outTime").getValue(0))
        if first is None or last is None:
            return ""
        return framelib.span(int(round(first)), int(round(last)))

    def _first_frame(self) -> float:
        fr = self.frames()
        try:
            return float(framelib.parse(fr)[0]) if fr else 1.0
        except ValueError:
            return 1.0

    # --- paths ------------------------------------------------------------

    def absolute(self, path: str) -> str:
        """A path as Katana reads it: environment variables expanded,
        relative to the project file's folder."""
        path = os.path.expandvars((path or "").strip())
        if not path or paths.is_absolute(path):
            return paths.norm(path) if path else path
        base = os.path.dirname(self.scene_file())
        return paths.norm(base.rstrip("/\\") + "/" + path) if base else path

    # --- what renders ---------------------------------------------------------

    def entries(self) -> List[dict]:
        """FarmAPI's sorted dependency list for the Render nodes: each
        node's name, the Render nodes it depends on, its outputs and range,
        those it depends on first."""
        farm, _, _ = _katana()
        nodes = render_nodes()
        if not nodes:
            return []
        try:
            got = farm.GetSortedDependencyList(nodes)
        except TypeError:   # a Katana that takes no node list renders its current scope
            got = farm.GetSortedDependencyList()
        names = {n.getName() for n in nodes}
        return [e for e in got or [] if e.get("name") in names]

    def read(self) -> Tuple[str, List[Renderable]]:
        scene_range = self.frames()
        out = []
        entries = self.entries()
        names = {e["name"] for e in entries}
        for e in entries:
            name = e["name"]
            own = None
            rng = e.get("range")
            if isinstance(rng, (list, tuple)) and len(rng) == 2 and rng[0] is not None and rng[1] is not None:
                span = framelib.span(int(round(rng[0])), int(round(rng[1])))
                own = span if span != scene_range else None
            reads = [d for d in e.get("deps") or [] if d in names and d != name]
            outputs = []
            for o in e.get("outputs") or []:
                loc = (o or {}).get("outputLocation") or ""
                if loc and o.get("enabled", True) is not False:
                    p = self.absolute(loc)
                    if p not in outputs:
                        outputs.append(p)
            note = "after " + ", ".join(reads) if reads else ""
            out.append(Renderable(name=name, label=name, frames=own, outputs=outputs, reads=reads, key=node_key(name),
                                  note=note))
        return scene_range, out

    def renderables(self) -> List[Renderable]:
        return self.read()[1]

    def selection(self) -> List[str]:
        return []

    # --- what the project reads --------------------------------------------------

    def dependencies(self) -> List[Dependency]:
        _, ng, _ = _katana()
        self._unread: List[str] = []
        graph = _Graph()
        renders = render_nodes()
        ups = {r.getName(): graph.upstream(r) for r in renders}
        frame = self._first_frame()
        scene = self.scene_file()
        out: List[Dependency] = []
        for n in ng.GetAllNodes() or []:
            if n.getType() in OUTPUT_TYPES or bypassed(n):
                continue
            files = param_files(n, frame)
            if not files:
                continue
            name = n.getName()
            readers = [r for r, up in ups.items() if name in up]
            tag: Optional[List[str]] = None if not ups or len(readers) == len(ups) else readers
            for raw, param in files:
                p = self.absolute(raw)
                if not p or (scene and paths.same(p, scene)):
                    continue
                label = f"{name}.{param.split('.', 1)[-1]}" if param else name
                out.append(Dependency(path=p, node=label, renderables=tag))
                deps, unread = usd_dependencies(p, label, tag)
                out += deps
                self._unread += [u for u in unread if u not in self._unread]
        return out

    def warnings(self) -> List[str]:
        """Each binary USD layer whose own files could not be followed."""
        return [unread_warning(u) for u in getattr(self, "_unread", [])]
