"""The open Maya scene, as c4submit reads it.

Each renderable render layer is a renderable, so each one ticked is a job:
the Render Setup layers (``rs_beauty``) and the master layer
(``defaultRenderLayer``) when it is renderable, or the legacy layers of a
scene that still uses them. A layer that renders its own frame range has
it. Layers do not read each other's renders, so no layer is ``after``
another.

Every file the scene reads is a dependency: file textures (``<UDIM>``
tiles and image sequences), references, Arnold ``aiImage``, ``aiStandIn``
and ``aiVolume`` files (sequences), Alembic and GPU caches, and whatever
else Maya lists for the scene. A file read only by objects in some layers
is tagged with those layers, so unticking a layer leaves out what only it
reads. References and Alembic caches are read whenever the scene opens,
so every layer needs them.

Only ``maya.cmds`` is imported, and only by the functions that need it,
so the module loads (and is tested) with a fake ``maya`` in
``sys.modules``. Every method runs on Maya's main thread, as c4submit's
Host requires.
"""

import os
import re
from typing import Callable, Dict, Iterable, List, Optional, Sequence, Set

from c4submit import frames as framelib
from c4submit import paths, seq
from c4submit.host import Dependency, Host, Renderable

#: The master layer's node.
MASTER = "defaultRenderLayer"
#: The node holding the render settings each layer may override.
GLOBALS = "defaultRenderGlobals"
#: Cameras every new scene has; another renderable camera is preferred.
STARTUP_CAMERAS = ("persp", "top", "front", "side")
#: uvTilingMode values on a file node.
TILES_OFF, TILES_ZBRUSH, TILES_MUDBOX, TILES_UDIM = 0, 1, 2, 3

_UV = re.compile(r"<[uU]>|<[vV]>")
_UV_IN_NAME = re.compile(r"([._-]?)u(\d+)_v(\d+)(?=\D*$)")
_UDIM_IN_NAME = re.compile(r"(?<!\d)1\d{3}(?!\d)")
_LAST_NUMBER = re.compile(r"(\d+)(?=\D*$)")


def _cmds():
    from maya import cmds  # noqa: PLC0415 - only inside Maya (or a test's fake)
    return cmds


def _get(plug: str, default=None):
    """An attribute's value, or ``default`` when the node or attribute is
    not there (Maya raises ValueError or RuntimeError)."""
    try:
        v = _cmds().getAttr(plug)
    except Exception:  # noqa: BLE001 - Maya raises its own
        return default
    return default if v is None else v


def _ls(*args, **kw) -> List[str]:
    try:
        return list(_cmds().ls(*args, **kw) or [])
    except Exception:  # noqa: BLE001 - an unknown node type in this Maya
        return []


def layer_key(layer: str) -> str:
    """The name a layer's job key is made from: ``beauty`` for
    ``rs_beauty``, ``master`` for the master layer."""
    if layer == MASTER:
        return "master"
    return layer[3:] if layer.startswith("rs_") and len(layer) > 3 else layer


def layer_label(layer: str) -> str:
    """A layer as Maya's Render Setup window names it."""
    return "masterLayer" if layer == MASTER else layer_key(layer)


def render_layers() -> List[str]:
    """This scene's render layers, the master layer first. A referenced
    scene's layers (``ref:defaultRenderLayer``) are not this scene's."""
    names = [n for n in _ls(type="renderLayer") if ":" not in n]
    return sorted(names, key=lambda n: n != MASTER)


def visit_layers(layers: Sequence[str], fn: Callable[[str], object]) -> Dict[str, object]:
    """``fn(layer)`` with each layer made current in turn, so its overrides
    of the render settings apply, then the artist's layer made current
    again. Switching is not an edit: the scene's unsaved state is kept."""
    cmds = _cmds()
    current = cmds.editRenderLayerGlobals(query=True, currentRenderLayer=True)
    was_modified = bool(cmds.file(query=True, modified=True))
    out: Dict[str, object] = {}
    on = current
    try:
        for layer in sorted(layers, key=lambda n: n != current):
            if layer != on:
                cmds.editRenderLayerGlobals(currentRenderLayer=layer)
                on = layer
            out[layer] = fn(layer)
    finally:
        if on != current:
            cmds.editRenderLayerGlobals(currentRenderLayer=current)
            if not was_modified:
                cmds.file(modified=False)
    return out


def _span() -> str:
    """The current layer's frame range from the render settings."""
    if not _get(GLOBALS + ".animation", True):
        try:
            t = _cmds().currentTime(query=True)
        except Exception:  # noqa: BLE001
            return ""
        return str(int(round(t)))
    first, last = _get(GLOBALS + ".startFrame"), _get(GLOBALS + ".endFrame")
    if first is None or last is None:
        return ""
    by = _get(GLOBALS + ".byFrameStep", 1) or 1
    return framelib.span(int(round(first)), int(round(last)), max(1, int(round(by))))


# --- paths with tiles and frames -------------------------------------------------

def udim_path(path: str, pattern: str = "") -> str:
    """A UDIM texture's path with ``<UDIM>`` for its tile: the node's
    computed pattern when it has one, else the path with its four-digit
    tile number (1001 and up) replaced."""
    if pattern and seq.has_tiles(pattern):
        return pattern
    d, base = os.path.split(path)
    ms = list(_UDIM_IN_NAME.finditer(base))
    if not ms:
        return path
    m = ms[-1]
    base = base[:m.start()] + "<UDIM>" + base[m.end():]
    return os.path.join(d, base) if d else base


def uv_pattern(path: str, pattern: str = "") -> str:
    """A ZBrush or Mudbox tiled texture's path with ``<U>`` and ``<V>`` for
    its tile (``_u0_v0`` or ``_u1_v1`` in the name)."""
    if pattern and _UV.search(pattern):
        return pattern
    d, base = os.path.split(path)
    m = _UV_IN_NAME.search(base)
    if not m:
        return path
    base = base[:m.start()] + m.group(1) + "u<U>_v<V>" + base[m.end():]
    return os.path.join(d, base) if d else base


def uv_tiles(pattern: str) -> List[str]:
    """The files on disk a ``<U>``/``<V>`` pattern names, sorted."""
    d, base = os.path.split(pattern)
    rx = re.compile("".join(r"\d+" if _UV.fullmatch(p) else re.escape(p) for p in re.split(r"(<[uUvV]>)", base)))
    try:
        names = os.listdir(d or ".")
    except OSError:
        return []
    return sorted(os.path.join(d, n) for n in names if rx.fullmatch(n) and os.path.isfile(os.path.join(d, n)))


def frame_path(path: str) -> str:
    """An image or cache sequence's path with ``#`` for its frame: the last
    number in the file's name, at its padding. A path that already holds a
    frame token is kept."""
    if seq.has_frames(path):
        return path
    d, base = os.path.split(path)
    stem = base
    for tok in ("<UDIM>", "<udim>", "<U>", "<V>"):
        stem = stem.replace(tok, "\0" * len(tok))
    m = _LAST_NUMBER.search(stem)
    if not m:
        return path
    base = base[:m.start()] + "#" * len(m.group(1)) + base[m.end():]
    return os.path.join(d, base) if d else base


# --- the host ----------------------------------------------------------------------

class MayaHost(Host):
    """The open scene, read through ``maya.cmds``."""

    name = "maya"
    stage = "render"
    scene_word = "scene"

    # --- the scene ------------------------------------------------------

    def scene_file(self) -> str:
        return _cmds().file(query=True, sceneName=True) or ""

    def is_modified(self) -> bool:
        return bool(_cmds().file(query=True, modified=True))

    def save(self) -> None:
        if not self.scene_file():
            raise RuntimeError("the scene has never been saved; save it with File > Save Scene As first")
        _cmds().file(save=True)

    def frames(self) -> str:
        """The master layer's frame range (what a layer without its own
        range renders)."""
        cmds = _cmds()
        if cmds.editRenderLayerGlobals(query=True, currentRenderLayer=True) == MASTER:
            return _span()
        return str(visit_layers([MASTER], lambda _: _span())[MASTER])

    # --- paths ------------------------------------------------------------

    def workspace(self) -> str:
        """The project's root, where Maya resolves relative paths."""
        try:
            return _cmds().workspace(query=True, rootDirectory=True) or ""
        except Exception:  # noqa: BLE001
            return ""

    def absolute(self, path: str) -> str:
        """A path as Maya reads it: environment variables expanded,
        relative to the workspace's root."""
        path = os.path.expandvars((path or "").strip())
        if not path:
            return path
        if paths.is_absolute(path):
            return paths.norm(path)
        base = self.workspace() or os.path.dirname(self.scene_file())
        return paths.norm(base.rstrip("/\\") + "/" + path) if base else path

    # --- what renders -------------------------------------------------------

    def renderables(self) -> List[Renderable]:
        layers = [n for n in render_layers() if _get(n + ".renderable", False)]
        if not layers:
            return []
        pad = int(_get(GLOBALS + ".extensionPadding", 4) or 4)

        def read(layer: str):
            return _span(), self.outputs(layer, pad)

        seen = visit_layers(layers + ([MASTER] if MASTER not in layers else []), read)
        scene_range = seen[MASTER][0]
        out = []
        for layer in layers:
            span, outputs = seen[layer]
            own = span if span and span != scene_range else None
            out.append(Renderable(name=layer, label=layer_label(layer), frames=own, outputs=outputs,
                                  key=layer_key(layer), note="its own range" if own else ""))
        return out

    def outputs(self, layer: str, pad: int = 4) -> List[str]:
        """The images a layer writes, as patterns: the render settings'
        file name prefix, with ``#`` for the frame."""
        try:
            names = _cmds().renderSettings(genericFrameImageName="#" * max(1, pad), fullPath=True, layer=layer)
        except Exception:  # noqa: BLE001 - a renderer that cannot say
            return []
        out = []
        for n in names or []:
            p = self.absolute(n)
            if p and p not in out:
                out.append(p)
        return out

    def camera(self) -> Optional[str]:
        """The renderable camera: one the artist made, before persp and the
        other startup cameras."""
        cmds = _cmds()
        found = []
        for shape in _ls(type="camera", long=True):
            if not _get(shape + ".renderable", False):
                continue
            parent = cmds.listRelatives(shape, parent=True) or []
            found.append(parent[0] if parent else shape.rsplit("|", 1)[-1])
        if not found:
            return None
        found.sort(key=lambda c: (c.rsplit("|", 1)[-1] in STARTUP_CAMERAS, c))
        return found[0]

    def selection(self) -> List[str]:
        out = []
        for n in _ls(selection=True, type="file"):
            p = self.absolute(_get(n + ".fileTextureName", "") or "")
            if p:
                out.append(p)
        return out

    # --- what the scene reads ---------------------------------------------------

    def dependencies(self) -> List[Dependency]:
        layers = [n for n in render_layers() if _get(n + ".renderable", False)]
        members = _layer_members(layers)
        out: List[Dependency] = []

        def add(path: str, node: str, tagged: bool = True, optional: bool = False):
            if path:
                out.append(Dependency(path=path, node=node, optional=optional,
                                      renderables=_layers_of(node, members) if tagged else None))

        for n in _ls(type="file"):
            for p in self._texture(n):
                add(p, n)
        for n in _ls(type="aiImage"):
            for p in self._tiled(self.absolute(_get(n + ".filename", "") or "")):
                add(p, n)
        for t, attr in (("aiStandIn", "dso"), ("aiVolume", "filename"), ("gpuCache", "cacheFileName")):
            for n in _ls(type=t):
                p = self.absolute(_get(f"{n}.{attr}", "") or "")
                if p and _get(n + ".useFrameExtension", False):
                    p = frame_path(p)
                add(p, n)
        for n in _ls(type="AlembicNode"):
            files = [_get(n + ".abc_File", "") or ""]
            layered = _get(n + ".abc_layerFiles", []) or []
            files += [layered] if isinstance(layered, str) else list(layered)
            for p in files:
                add(self.absolute(p), n, tagged=False)
        for ref, path, loaded in self._references():
            add(path, ref, tagged=False, optional=not loaded)
        out += self._listed(out)
        return out

    def _texture(self, node: str) -> List[str]:
        """The files a file node reads: its tiles and frames as patterns."""
        p = self.absolute(_get(node + ".fileTextureName", "") or "")
        if not p:
            return []
        mode = int(_get(node + ".uvTilingMode", TILES_OFF) or TILES_OFF)
        pattern = self.absolute(_get(node + ".computedFileTextureNamePattern", "") or "")
        if mode == TILES_UDIM:
            p = udim_path(p, pattern)
        elif mode in (TILES_ZBRUSH, TILES_MUDBOX):
            p = uv_pattern(p, pattern)
        if _get(node + ".useFrameExtension", False):
            p = frame_path(p)
        return self._tiled(p)

    @staticmethod
    def _tiled(p: str) -> List[str]:
        """A path, or the tiles of a ``<U>``/``<V>`` (or Arnold ``<tile>``)
        pattern, which c4submit does not expand itself. A pattern with no
        tiles on disk stays as it is, so the check says it is missing."""
        if not p:
            return []
        if "<tile>" in p:
            p = p.replace("<tile>", "_u<U>_v<V>")
        if not _UV.search(p):
            return [p]
        return uv_tiles(p) or [p]

    def _references(self) -> List[tuple]:
        """(reference node, file, loaded) for each reference, nested ones too."""
        cmds = _cmds()
        out = []
        for ref in _ls(type="reference"):
            if ref == "sharedReferenceNode" or ref.endswith("_UNKNOWN_REF_NODE_"):
                continue
            try:
                path = cmds.referenceQuery(ref, filename=True, withoutCopyNumber=True)
                loaded = bool(cmds.referenceQuery(ref, isLoaded=True))
            except Exception:  # noqa: BLE001 - a reference node with no file
                continue
            if path:
                out.append((ref, self.absolute(path), loaded))
        return out

    def _listed(self, known: Iterable[Dependency]) -> List[Dependency]:
        """Files Maya lists for the scene that no node above accounts for.
        Missing ones are warnings: Maya lists files a render may not read."""
        try:
            listed = _cmds().file(query=True, list=True, withoutCopyNumber=True) or []
        except Exception:  # noqa: BLE001
            return []
        scene = self.scene_file()
        pats = [d.path for d in known]
        plain = {paths.norm(p) for p in pats}
        out, seen = [], set()
        for raw in listed:
            p = self.absolute(raw)
            if not p or _UV.search(p) or paths.same(p, scene) or p in plain or p in seen or os.path.isdir(p):
                continue
            if any((seq.has_frames(x) or seq.has_tiles(x)) and seq.matches(x, p) for x in pats):
                continue
            seen.add(p)
            out.append(Dependency(path=p, node="scene files", optional=True))
        return out


# --- which layers read a node -------------------------------------------------------

def _layer_members(layers: Sequence[str]) -> Dict[str, Optional[Set[str]]]:
    """Each layer's objects, as full DAG paths with everything under them;
    None for a layer that holds everything (the master layer) or whose
    members Maya does not say (then it is taken to read every file)."""
    cmds = _cmds()
    out: Dict[str, Optional[Set[str]]] = {}
    for layer in layers:
        if layer == MASTER:
            out[layer] = None
            continue
        try:
            names = cmds.editRenderLayerMembers(layer, query=True, fullNames=True) or []
        except Exception:  # noqa: BLE001
            names = []
        if not names:
            out[layer] = None
            continue
        full = set(_ls(names, long=True))
        try:
            full |= set(cmds.listRelatives(list(full), allDescendents=True, fullPath=True) or [])
        except Exception:  # noqa: BLE001
            pass
        out[layer] = full
    return out


def _users(node: str) -> Set[str]:
    """The objects (full DAG paths) that render what a node reads: the node
    itself when it is an object, the objects downstream of it, and the
    members of the shading groups it feeds."""
    cmds = _cmds()
    out: Set[str] = set(_ls(node, dag=True, long=True))
    try:
        future = cmds.listHistory(node, future=True) or []
    except Exception:  # noqa: BLE001
        future = []
    out |= set(_ls(future, dag=True, long=True))
    for sg in _ls(future, type="shadingEngine"):
        try:
            members = cmds.sets(sg, query=True) or []
        except Exception:  # noqa: BLE001
            members = []
        objs = [m.split(".", 1)[0] for m in members]   # faces (pCube1.f[0:5]) are their object's
        out |= set(_ls(objs, long=True))
    return out


def _within(path: str, members: Set[str]) -> bool:
    if path in members:
        return True
    while "|" in path.strip("|"):
        path = path.rsplit("|", 1)[0]
        if path in members:
            return True
    return False


def _layers_of(node: str, members: Dict[str, Optional[Set[str]]]) -> Optional[List[str]]:
    """The layers whose objects use a node; None when every layer does, or
    when it cannot be told (nothing is left out on a guess)."""
    if not members:
        return None
    users = _users(node)
    if not users:
        return None
    out = [layer for layer, m in members.items() if m is None or any(_within(u, m) for u in users)]
    return None if len(out) == len(members) else out
