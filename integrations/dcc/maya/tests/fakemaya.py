"""A fake ``maya`` (``maya.cmds``, ``maya.utils``, ``maya.mel``), with only
what c4_maya uses.

A scene is nodes with attributes, DAG parents and connections, render
layers with members and overrides of the render settings, built by the
helpers at the bottom (``mesh``, ``layer``, ``file_texture``, ...). The UI
commands make controls that record their flags, so a test can read every
label, value and annotation the artist would. ``executeDeferred`` queues,
as Maya does; ``settle(panel)`` runs the queue until the panel's workers
are done. Workers that call ``executeInMainThreadWithResult`` wait for the
test's thread to run it, as they wait for Maya's main thread.

What the fakes cannot prove about the real ``maya`` is in CHECKLIST.md.
"""

import os
import sys
import threading
import time
import types
from typing import Any, Callable, Dict, List, Optional

SHORT = {"q": "query", "e": "edit", "ex": "exists", "sn": "sceneName", "rd": "rootDirectory",
         "l": "label", "tx": "text", "v": "value", "en": "enable", "vis": "visible", "ann": "annotation",
         "cc": "changeCommand", "c": "command", "p": "parent"}

DAG_TYPES = ("transform", "mesh", "camera", "aiStandIn", "aiVolume", "gpuCache", "aiSkyDomeLight", "nurbsCurve")
SHAPE_TYPES = DAG_TYPES[1:]


# --- the scene ---------------------------------------------------------------------

class Node:
    def __init__(self, ntype: str, name: str, parent: Optional["Node"] = None, **attrs):
        if name in _s.nodes:
            raise ValueError(f"the fake keeps node names unique: {name} exists")
        self.type, self.name, self.parent = ntype, name, parent
        self.attrs: Dict[str, Any] = dict(attrs)
        _s.nodes[name] = self

    @property
    def dag(self) -> bool:
        return self.type in DAG_TYPES

    @property
    def path(self) -> str:
        if not self.dag:
            return self.name
        return (self.parent.path if self.parent is not None else "") + "|" + self.name

    def isa(self, t: str) -> bool:
        if t == self.type:
            return True
        if t == "dagNode":
            return self.dag
        if t == "shape":
            return self.type in SHAPE_TYPES
        return False


class _State:
    def __init__(self):
        self.nodes: Dict[str, Node] = {}
        self.path = ""
        self.modified = False
        self.saves = 0
        self.workspace = ""
        self.current_layer = "defaultRenderLayer"
        self.switches = 0
        self.overrides: Dict[str, Dict[str, Any]] = {}
        self.members: Dict[str, List[str]] = {}
        self.sets: Dict[str, List[str]] = {}
        self.edges: List[tuple] = []
        self.listed: List[str] = []
        self.selection: List[str] = []
        self.time = 1001.0
        self.batch = False
        # UI
        self.ui: Dict[str, "Control"] = {}
        self.parent = ""
        self.menu_parent = ""
        self.counter = 0
        self.progress: Optional[dict] = None
        self.progress_log: List[dict] = []
        self.cancel = False
        self.next_filename: Optional[str] = None
        self.asked: List[dict] = []
        self.jobs: Dict[int, tuple] = {}
        self.warnings: List[str] = []
        self.queue: List[tuple] = []
        self.lock = threading.Lock()


_s = _State()


def state() -> _State:
    return _s


def _long(kw: dict) -> dict:
    return {SHORT.get(k, k): v for k, v in kw.items()}


def _node(name: str) -> Node:
    n = _s.nodes.get(name.rsplit("|", 1)[-1].split(".", 1)[0])
    if n is None:
        raise ValueError(f"No object matches name: {name}")
    return n


def _names(args) -> List[str]:
    out = []
    for a in args:
        if isinstance(a, (list, tuple)):
            out += [str(x) for x in a]
        elif a is not None:
            out.append(str(a))
    return out


class Cmds(types.ModuleType):
    pass


cmds = Cmds("maya.cmds")


def _cmd(fn: Callable) -> Callable:
    setattr(cmds, fn.__name__, fn)
    return fn


@_cmd
def getAttr(plug: str):  # noqa: N802
    name, attr = plug.split(".", 1)
    n = _node(name)
    o = _s.overrides.get(_s.current_layer, {})
    if f"{n.name}.{attr}" in o:
        return o[f"{n.name}.{attr}"]
    if n.name == "defaultRenderGlobals" and attr in o:
        return o[attr]
    if attr not in n.attrs:
        raise ValueError(f"No object matches name: {plug}")
    return n.attrs[attr]


@_cmd
def setAttr(plug: str, value, type=None):  # noqa: N802, A002
    name, attr = plug.split(".", 1)
    _node(name).attrs[attr] = value
    _s.modified = True


@_cmd
def objExists(name: str) -> bool:  # noqa: N802
    try:
        _node(name)
        return True
    except ValueError:
        return False


@_cmd
def ls(*args, type=None, long=False, selection=False, dag=False, **kw):  # noqa: A002
    names = _names(args)
    if selection:
        nodes = [_s.nodes[n] for n in _s.selection if n in _s.nodes]
    elif args:
        nodes = []
        for x in names:
            try:
                nodes.append(_node(x))
            except ValueError:
                pass
    else:
        nodes = list(_s.nodes.values())
    types_ = [type] if isinstance(type, str) else list(type or [])
    out = []
    for n in nodes:
        if types_ and not any(n.isa(t) for t in types_):
            continue
        if dag and not n.dag:
            continue
        v = n.path if long and n.dag else n.name
        if v not in out:
            out.append(v)
    return out


@_cmd
def objectType(name: str, isAType: Optional[str] = None):  # noqa: N802, N803
    n = _node(name)
    return n.isa(isAType) if isAType else n.type


@_cmd
def listRelatives(*args, parent=False, allDescendents=False, fullPath=False, **kw):  # noqa: N803
    out = []
    for x in _names(args):
        n = _node(x)
        if parent:
            if n.parent is not None:
                out.append(n.parent.path if fullPath else n.parent.name)
            continue
        if allDescendents:
            for m in _s.nodes.values():
                p = m.parent
                while p is not None and p is not n:
                    p = p.parent
                if p is n:
                    out.append(m.path if fullPath else m.name)
    return out or None


@_cmd
def listHistory(name: str, future: bool = False, **kw):  # noqa: N802
    start = _node(name).name
    seen, todo = [start], [start]
    while todo:
        cur = todo.pop(0)
        for a, b in _s.edges:
            nxt = b if future else a
            if (a if future else b) == cur and nxt not in seen:
                seen.append(nxt)
                todo.append(nxt)
    return seen


@_cmd
def sets(name: str, query: bool = False, **kw):
    return list(_s.sets.get(name, [])) if query else None


@_cmd
def editRenderLayerMembers(layer: str, query: bool = False, fullNames: bool = False, **kw):  # noqa: N802, N803
    names = _s.members.get(layer, [])
    return [_node(n).path if fullNames else n for n in names]


@_cmd
def editRenderLayerGlobals(query: bool = False, currentRenderLayer=None, **kw):  # noqa: N802, N803
    if query:
        return _s.current_layer
    _node(currentRenderLayer)
    _s.current_layer = currentRenderLayer
    _s.switches += 1
    _s.modified = True   # as Maya may; the host keeps the artist's unsaved state
    return None


@_cmd
def renderSettings(firstImageName=False, genericFrameImageName=None, fullPath=False, layer=None, **kw):  # noqa: N802, N803
    layer = layer or _s.current_layer
    short = "masterLayer" if layer == "defaultRenderLayer" else layer.replace("rs_", "", 1)
    stem = os.path.splitext(os.path.basename(_s.path))[0] or "untitled"
    frame = genericFrameImageName if genericFrameImageName is not None else "1001"
    rel = f"images/{short}/{stem}_{short}.{frame}.exr"
    return [os.path.join(_s.workspace, rel) if fullPath else rel]


@_cmd
def workspace(query: bool = False, rootDirectory: bool = False, **kw):  # noqa: N803
    kw = _long(kw)
    if kw.get("rootDirectory") or rootDirectory:
        return _s.workspace.rstrip("/") + "/" if _s.workspace else ""
    return ""


@_cmd
def file(*args, **kw):  # noqa: A001
    kw = _long(kw)
    if kw.get("query"):
        if kw.get("sceneName"):
            return _s.path
        if kw.get("modified"):
            return _s.modified
        if kw.get("list"):
            out = [_s.path] if _s.path else []
            for n in _s.nodes.values():
                if n.type == "file" and n.attrs.get("fileTextureName"):
                    out.append(n.attrs["fileTextureName"])
                if n.type == "reference":
                    out.append(n.attrs["file"] + ("{2}" if not kw.get("withoutCopyNumber") and n.attrs.get("copy") else ""))
            return out + list(_s.listed)
        raise TypeError(f"the fake file() cannot query {kw}")
    if kw.get("save"):
        with open(_s.path, "ab") as f:
            f.write(b"// saved\n")
        _s.modified = False
        _s.saves += 1
        return _s.path
    if "modified" in kw:
        _s.modified = bool(kw["modified"])
        return None
    raise TypeError(f"the fake file() cannot do {kw}")


@_cmd
def referenceQuery(ref: str, filename: bool = False, withoutCopyNumber: bool = False, isLoaded: bool = False, **kw):  # noqa: N802, N803
    n = _node(ref)
    if isLoaded:
        return n.attrs.get("loaded", True)
    if filename:
        return n.attrs["file"] + ("{2}" if n.attrs.get("copy") and not withoutCopyNumber else "")
    raise TypeError("the fake referenceQuery() cannot do that")


@_cmd
def currentTime(*args, query: bool = False, **kw):  # noqa: N802
    if query or kw.get("q"):
        return _s.time
    _s.time = float(args[0])
    return _s.time


@_cmd
def about(batch: bool = False, version: bool = False, **kw):
    if batch:
        return _s.batch
    return "2025"


@_cmd
def warning(msg: str) -> None:
    _s.warnings.append(msg)


# --- UI --------------------------------------------------------------------------------

LAYOUTS = ("window", "columnLayout", "rowLayout", "frameLayout", "scrollLayout")
VALUE_FLAG = {"textFieldGrp": "text", "textField": "text", "textFieldButtonGrp": "text", "scrollField": "text",
              "checkBoxGrp": "value1", "checkBox": "value", "intFieldGrp": "value1", "floatFieldGrp": "value1",
              "optionMenuGrp": "value", "text": "label"}
DEFAULTS = {"text": "", "value1": 0, "value": False, "enable": True, "visible": True, "editable": True,
            "annotation": "", "label": ""}

created: List["Control"] = []


class Control:
    def __init__(self, kind: str, short: str, parent: str, flags: dict):
        self.kind, self.short, self.parent = kind, short, parent
        self.path = f"{parent}|{short}" if parent else short
        self.flags = dict(flags)
        self.items: List[str] = []          # an option menu's menuItem paths
        self.shown = 0
        created.append(self)

    def get(self, flag: str):
        if self.kind == "optionMenuGrp" and flag == "value":
            sel = self.flags.get("_selected", 0)
            return _s.ui[self.items[sel]].flags.get("label", "") if self.items else ""
        if self.kind == "optionMenuGrp" and flag == "itemListLong":
            return list(self.items) or None
        if self.kind == "optionMenuGrp" and flag == "numberOfItems":
            return len(self.items)
        if self.kind == "optionMenuGrp" and flag == "select":
            return self.flags.get("_selected", 0) + 1
        return self.flags.get(flag, DEFAULTS.get(flag))

    def edit(self, flags: dict) -> None:
        for k, v in flags.items():
            if self.kind == "optionMenuGrp" and k == "value":
                labels = [_s.ui[i].flags.get("label") for i in self.items]
                if v not in labels:
                    raise RuntimeError(f"optionMenuGrp: {v!r} is not one of {labels}")
                self.flags["_selected"] = labels.index(v)
            elif self.kind == "optionMenuGrp" and k == "select":
                self.flags["_selected"] = int(v) - 1
            else:
                self.flags[k] = v

    @property
    def value(self):
        return self.get(VALUE_FLAG.get(self.kind, "value"))

    def callbacks(self) -> List[Callable]:
        names = ("changeCommand", "changeCommand1", "command", "onCommand", "offCommand", "onCommand1", "offCommand1")
        return [self.flags[n] for n in names if callable(self.flags.get(n))]

    def __repr__(self):
        return f"{self.kind}({self.path}={self.value!r})"


def _find(name: str) -> Optional[Control]:
    if name in _s.ui:
        return _s.ui[name]
    for c in _s.ui.values():
        if c.short == name:
            return c
    return None


def control(short: str) -> Control:
    c = _find(short)
    if c is None:
        raise KeyError(f"no control {short}")
    return c


def _ui_command(kind: str) -> Callable:
    def cmd(*args, **kw):
        kw = _long(kw)
        name = args[0] if args else None
        if kw.pop("exists", False):
            return name is not None and _find(name) is not None
        if kw.pop("query", False):
            c = _find(name)
            if c is None:
                raise RuntimeError(f"{kind}: object '{name}' not found")
            (flag,) = [k for k in kw if kw[k]] or ["value"]
            return c.get(flag)
        if kw.pop("edit", False):
            c = _find(name)
            if c is None:
                raise RuntimeError(f"{kind}: object '{name}' not found")
            c.edit(kw)
            return c.path
        parent = kw.pop("parent", None)
        if kind == "window" or kind == "menu" and parent is None:
            parent = ""
        elif parent is None:
            parent = _s.menu_parent if kind == "menuItem" else _s.parent
        if not name:
            _s.counter += 1
            name = f"{kind}{_s.counter}"
        c = Control(kind, name, parent, kw)
        if c.path in _s.ui:
            raise RuntimeError(f"{kind}: '{c.path}' is not unique")
        _s.ui[c.path] = c
        if kind in LAYOUTS:
            _s.parent = c.path
        if kind == "optionMenuGrp":
            inner = Control("optionMenu", "OptionMenu", c.path, {})   # where Maya keeps its items
            _s.ui[inner.path] = inner
            _s.menu_parent = inner.path
        if kind == "menu":
            _s.menu_parent = c.path
        if kind == "menuItem":
            owner = _s.ui.get(parent)
            if owner is not None and owner.kind == "optionMenu":
                _s.ui[owner.parent].items.append(c.path)
        return c.path
    cmd.__name__ = kind
    return cmd


for _k in ("window", "columnLayout", "rowLayout", "frameLayout", "scrollLayout", "text", "separator",
           "textFieldGrp", "textField", "optionMenuGrp", "menuItem", "checkBoxGrp", "checkBox", "intFieldGrp",
           "floatFieldGrp", "textFieldButtonGrp", "scrollField", "button", "menu"):
    setattr(cmds, _k, _ui_command(_k))


@_cmd
def setParent(where: str = "..", **kw):  # noqa: N802
    if kw.get("menu"):
        _s.menu_parent = where if where != ".." else ""
        return _s.menu_parent
    if where == "..":
        _s.parent = _s.parent.rsplit("|", 1)[0] if "|" in _s.parent else ""
    else:
        if _find(where) is None:
            raise RuntimeError(f"setParent: object '{where}' not found")
        _s.parent = _find(where).path
    return _s.parent


@_cmd
def deleteUI(*args, **kw):  # noqa: N802
    for name in _names(args):
        c = _find(name)
        if c is None:
            raise RuntimeError(f"deleteUI: object '{name}' not found")
        for p in [p for p in _s.ui if p == c.path or p.startswith(c.path + "|")]:
            del _s.ui[p]
        for owner in _s.ui.values():
            if c.path in owner.items:
                owner.items.remove(c.path)
                owner.flags["_selected"] = 0
        if _s.parent == c.path or _s.parent.startswith(c.path + "|"):
            _s.parent = c.parent
        for jid in [j for j, (_, _, p) in _s.jobs.items() if p and (p == c.path or p.startswith(c.path + "|"))]:
            del _s.jobs[jid]


@_cmd
def showWindow(name: str) -> None:  # noqa: N802
    control(name).shown += 1


@_cmd
def progressWindow(*args, **kw):  # noqa: N802
    kw = _long(kw)
    if kw.get("query"):
        if kw.get("isCancelled"):
            return _s.cancel
        p = _s.progress or {}
        (flag,) = [k for k in kw if k != "query"]
        return p.get(flag)
    if kw.get("endProgress"):
        if _s.progress is not None:
            _s.progress["ended"] = True
        _s.progress = None
        return None
    if kw.pop("edit", False):
        if _s.progress is None:
            raise RuntimeError("progressWindow: no progress window to edit")
        _s.progress.update(kw)
        _s.progress["log"].append(dict(kw))
        return None
    if _s.progress is not None:
        raise RuntimeError("progressWindow: one is open already")
    _s.progress = dict(kw, log=[], ended=False)
    _s.progress_log.append(_s.progress)
    return None


@_cmd
def fileDialog2(**kw):  # noqa: N802
    _s.asked.append(dict(kw))
    return [_s.next_filename] if _s.next_filename else None


@_cmd
def scriptJob(event=None, parent=None, kill=None, exists=None, **kw):  # noqa: N802
    if kill is not None:
        _s.jobs.pop(kill, None)
        return None
    if exists is not None:
        return exists in _s.jobs
    _s.counter += 1
    _s.jobs[_s.counter] = (event[0], event[1], parent)
    return _s.counter


def fire(event: str) -> int:
    """Maya fires a scriptJob event; returns how many jobs ran."""
    jobs = [fn for e, fn, _ in list(_s.jobs.values()) if e == event]
    for fn in jobs:
        fn()
    return len(jobs)


# --- maya.utils and maya.mel ----------------------------------------------------------------

utils = types.ModuleType("maya.utils")


def executeDeferred(fn: Callable, *args, **kwargs) -> None:  # noqa: N802
    with _s.lock:
        _s.queue.append((fn, args, kwargs, None))


def executeInMainThreadWithResult(fn: Callable, *args, **kwargs):  # noqa: N802
    if threading.current_thread() is threading.main_thread():
        return fn(*args, **kwargs)
    box: dict = {"done": threading.Event()}
    with _s.lock:
        _s.queue.append((fn, args, kwargs, box))
    if not box["done"].wait(30):
        raise RuntimeError("the main thread never ran it")
    if "error" in box:
        raise box["error"]
    return box["value"]


utils.executeDeferred = executeDeferred
utils.executeInMainThreadWithResult = executeInMainThreadWithResult

mel = types.ModuleType("maya.mel")


def _eval(text: str):
    if "gMainWindow" in text:
        return "MayaWindow"
    raise RuntimeError(f"the fake mel cannot run {text!r}")


mel.eval = _eval


def run_main() -> int:
    """Runs what workers queued for the main thread; returns how many."""
    n = 0
    while True:
        with _s.lock:
            if not _s.queue:
                return n
            fn, args, kwargs, box = _s.queue.pop(0)
        if box is None:
            fn(*args, **kwargs)
        else:
            try:
                box["value"] = fn(*args, **kwargs)
            except Exception as e:  # noqa: BLE001
                box["error"] = e
            box["done"].set()
        n += 1


def settle(panel: Any, timeout: float = 30) -> None:
    """Runs the main thread's queue until the panel's workers are done."""
    end = time.time() + timeout
    while time.time() < end:
        ran = run_main()
        if not ran and not panel.working():
            with _s.lock:
                if not _s.queue:
                    return
        time.sleep(0.002)
    raise TimeoutError("the panel's work never finished")


maya = types.ModuleType("maya")
maya.cmds, maya.utils, maya.mel = cmds, utils, mel
maya.__path__ = []   # a package, so "from maya import cmds" works


def install() -> None:
    """Puts the fake in ``sys.modules`` as ``maya``, ``maya.cmds``,
    ``maya.utils`` and ``maya.mel``."""
    sys.modules["maya"] = maya
    sys.modules["maya.cmds"] = cmds
    sys.modules["maya.utils"] = utils
    sys.modules["maya.mel"] = mel


# --- the artist ---------------------------------------------------------------------------

def type_into(short: str, value: Any) -> None:
    """The artist changes a control: its value, then its change commands."""
    c = control(short)
    if not c.get("enable"):
        raise RuntimeError(f"{short} is greyed out")
    if c.kind == "optionMenuGrp":
        c.edit({"value": value})
    else:
        c.flags[VALUE_FLAG.get(c.kind, "value")] = value
    for fn in c.callbacks():
        fn(value)


def type_only(short: str, text: str) -> None:
    """The artist types into a text field and, without pressing Enter or
    leaving it, presses a button: Maya has not run its change command."""
    control(short).flags[VALUE_FLAG[control(short).kind]] = text


def press(short: str) -> bool:
    """The artist presses a button; False when it is greyed out."""
    c = control(short)
    if not c.get("enable"):
        return False
    c.flags["command"](False)
    return True


def browse(short: str) -> None:
    """The artist presses a file field's browse button."""
    control(short).flags["buttonCommand"]()


def children(path: str) -> List[Control]:
    """A layout's controls, in the order they were made, all the way down."""
    return [c for p, c in _s.ui.items() if p.startswith(path + "|")]


def order(path: str) -> List[str]:
    return [c.short for c in children(path)]


# --- building scenes -------------------------------------------------------------------------

def reset(path: str = "", first: float = 1001, last: float = 1048, ws: str = "", modified: bool = False) -> _State:
    """A new scene (saved at ``path`` when given), with the master layer,
    the render settings and the startup cameras."""
    global _s
    _s = _State()
    _s.path, _s.workspace, _s.modified = path, ws, modified
    del created[:]
    Node("renderGlobals", "defaultRenderGlobals", startFrame=float(first), endFrame=float(last),
         byFrameStep=1.0, animation=True, extensionPadding=4, imageFilePrefix="<RenderLayer>/<Scene>_<RenderLayer>")
    Node("renderLayer", "defaultRenderLayer", renderable=True)
    for cam in ("persp", "top", "front", "side"):
        camera(cam, renderable=(cam == "persp"))
    return _s


def node(ntype: str, name: str, parent: Optional[str] = None, **attrs) -> str:
    Node(ntype, name, _node(parent) if parent else None, **attrs)
    return name


def mesh(name: str, parent: Optional[str] = None) -> str:
    """A transform with a mesh shape; returns the transform."""
    node("transform", name, parent)
    node("mesh", name + "Shape", name)
    return name


def camera(name: str, renderable: bool = True) -> str:
    node("transform", name)
    node("camera", name + "Shape", name, renderable=renderable)
    return name


def layer(name: str, renderable: bool = True, members=(), plugs: Optional[Dict[str, Any]] = None,
          **overrides) -> str:
    """A render layer with its objects, its overrides of the render
    settings (``startFrame=1001.0``) and of any other attribute
    (``plugs={"shotCamShape.renderable": False}``)."""
    node("renderLayer", name, renderable=renderable)
    _s.members[name] = list(members)
    o = {k: float(v) if isinstance(v, int) and not isinstance(v, bool) else v for k, v in overrides.items()}
    o.update(plugs or {})
    if o:
        _s.overrides[name] = o
    return name


def connect(src: str, dst: str) -> None:
    _s.edges.append((src, dst))


def shade(texture: str, objects=(), name: str = "") -> str:
    """A shader reading ``texture``, in a shading group assigned to ``objects``."""
    name = name or texture + "SG"
    shader = node("aiStandardSurface", name.replace("SG", "") + "Shader")
    node("shadingEngine", name)
    connect(texture, shader)
    connect(shader, name)
    _s.sets[name] = list(objects)
    return name


def file_texture(name: str, path: str, objects=(), tiling: int = 0, pattern: str = "", sequence: bool = False) -> str:
    node("file", name, fileTextureName=path, uvTilingMode=tiling, computedFileTextureNamePattern=pattern,
         useFrameExtension=sequence)
    if objects:
        shade(name, objects)
    return name


def standin(name: str, dso: str, sequence: bool = False, parent: Optional[str] = None) -> str:
    node("transform", name, parent)
    node("aiStandIn", name + "Shape", name, dso=dso, useFrameExtension=sequence)
    return name


def volume(name: str, path: str, sequence: bool = False) -> str:
    node("transform", name)
    node("aiVolume", name + "Shape", name, filename=path, useFrameExtension=sequence)
    return name


def alembic(name: str, path: str, feeds: Optional[str] = None) -> str:
    node("AlembicNode", name, abc_File=path, abc_layerFiles=[])
    if feeds:
        connect(name, feeds)
    return name


def ai_image(name: str, path: str, feeds: Optional[str] = None) -> str:
    node("aiImage", name, filename=path)
    if feeds:
        connect(name, feeds)
    return name


def reference(name: str, path: str, loaded: bool = True, copy: bool = False) -> str:
    node("reference", name, file=path, loaded=loaded, copy=copy)
    return name


def listed(*paths: str) -> None:
    """Files Maya lists for the scene that no node here reads."""
    _s.listed.extend(paths)


def select(*names: str) -> None:
    _s.selection = list(names)


reset()
