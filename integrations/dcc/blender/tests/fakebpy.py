"""A fake ``bpy``, with only what c4_blender uses.

The file's data is plain objects built by tests (``Scene``, ``Image``,
``Library``...) in ``bpy.data``. ``bpy.props`` makes descriptors that
``bpy.utils.register_class`` installs from a class's annotations, so
property groups, update callbacks, enum ``get``/``set`` and collections
behave as Blender's do. Panels draw into a recording ``Layout``;
``draw(panel)`` returns it. ``bpy.app.timers`` queue, as Blender's do; a
test runs them (and modal operators' timer events) with ``settle()``.

``install()`` puts the fake in ``sys.modules`` as ``bpy``; ``uninstall()``
takes it out again, so no other folder's tests see it. What the fake cannot
prove about the real ``bpy`` is in CHECKLIST.md.
"""

import os
import re
import sys
import time
import types
from typing import Any, Callable, Dict, List, Optional

MODULE_NAMES = ("bpy", "bpy.types", "bpy.props", "bpy.utils", "bpy.path", "bpy.app", "bpy.app.timers",
                "bpy.app.handlers", "bpy.ops")


# --- data ------------------------------------------------------------------------

class Collection(list):
    """``bpy_prop_collection``: by index, or by name."""

    def __getitem__(self, k):
        if isinstance(k, str):
            for x in self:
                if x.name == k:
                    return x
            raise KeyError(f'bpy_prop_collection[key]: key "{k}" not found')
        return list.__getitem__(self, k)

    def get(self, k, default=None):
        try:
            return self[k]
        except (KeyError, IndexError):
            return default

    def __contains__(self, k):
        if isinstance(k, str):
            return any(x.name == k for x in self)
        return any(x is k for x in self)

    def keys(self):
        return [x.name for x in self]


class Struct:
    def __init__(self, **kw):
        self.__dict__.update(kw)

    def __repr__(self):
        return f"{type(self).__name__}({getattr(self, 'name', '')!r})"


class ID(Struct):
    def __init__(self, name: str = "", **kw):
        self.name = name
        self.users = 1
        self.library = None
        self.use_fake_user = False
        self.packed_file = None
        super().__init__(**kw)

    def as_pointer(self) -> int:
        return id(self)

    @property
    def id_data(self):
        return self

    @property
    def name_full(self) -> str:
        return self.name if self.library is None else f"{self.name} [{self.library.name}]"


class Scene(ID):
    def __init__(self, name: str, frame_start: int = 1, frame_end: int = 250, frame_step: int = 1, **kw):
        self.frame_start, self.frame_end, self.frame_step = frame_start, frame_end, frame_step
        self.camera = None
        self.render = Struct(filepath="/tmp/", use_file_extension=True, engine="CYCLES", use_sequencer=True,
                             image_settings=Struct(file_format="PNG"))
        self.view_layers = Collection()
        self.use_nodes = False
        self.node_tree = None
        self.sequence_editor = None
        super().__init__(name, **kw)

    def layer(self, name: str, use: bool = True):
        vl = Struct(name=name, use=use)
        self.view_layers.append(vl)
        return vl


class Image(ID):
    def __init__(self, name: str, filepath: str = "", source: str = "FILE", **kw):
        self.filepath, self.source = filepath, source
        self.type = "IMAGE"
        self.packed_files = Collection()
        super().__init__(name, **kw)


class NodeTree(ID):
    def __init__(self, name: str = "", bl_idname: str = "ShaderNodeTree", nodes=(), **kw):
        self.bl_idname = bl_idname
        self.nodes = Collection(nodes)
        super().__init__(name, **kw)


class Node(Struct):
    def __init__(self, bl_idname: str, name: str = "", **kw):
        super().__init__(bl_idname=bl_idname, name=name or bl_idname, mute=False, **kw)


def image_user(frame_start: int = 1, frame_duration: int = 100, frame_offset: int = 0, use_cyclic: bool = False):
    return Struct(frame_start=frame_start, frame_duration=frame_duration, frame_offset=frame_offset,
                  use_cyclic=use_cyclic)


class Data:
    def __init__(self, filepath: str = ""):
        self.filepath = filepath
        self.is_dirty = False
        for name in ("scenes", "images", "libraries", "movieclips", "sounds", "cache_files", "volumes", "fonts",
                     "materials", "worlds", "lights", "node_groups", "textures", "texts", "objects"):
            setattr(self, name, Collection())
        # Paths blend_paths reports that none of the collections above
        # holds (a fluid cache's folder, a point cache), as Blender stores them.
        self.extra_paths: List[str] = []

    @property
    def is_saved(self) -> bool:
        return bool(self.filepath)

    def names(self) -> List[str]:
        """Every data-block and view layer name, for tests that look for
        farm words in what the artist reads but not in the artist's own names."""
        out = []
        for coll in vars(self).values():
            if isinstance(coll, Collection):
                for x in coll:
                    out.append(x.name)
                    out += [vl.name for vl in getattr(x, "view_layers", [])]
        return out


# --- properties ------------------------------------------------------------------------

def _store(obj) -> dict:
    return obj.__dict__.setdefault("_fake_props", {})


def _adopt(item, owner) -> None:
    item.__dict__["id_data"] = owner if isinstance(owner, ID) else getattr(owner, "id_data", None)


class Prop:
    """A ``bpy.props`` definition; a descriptor once registered."""

    def __init__(self, kind: str, **kw):
        self.kind = kind
        self.kw = kw

    # --- values

    def _initial(self, obj):
        k = self.kind
        if k == "Pointer":
            v = self.kw["type"]()
            _adopt(v, obj)
            return v
        if k == "Collection":
            return PropCollection(self.kw["type"], obj)
        if "default" in self.kw:
            return self.kw["default"]
        if k == "Enum":
            items = self.items(obj)
            return items[0][0] if items else ""
        return {"String": "", "Bool": False, "Int": 0, "Float": 0.0}[k]

    def items(self, obj) -> List[tuple]:
        """(identifier, name, description, number), however they were given."""
        raw = self.kw.get("items") or []
        if callable(raw):
            raw = raw(obj, context)
        out = []
        for i, t in enumerate(raw):
            num = t[-1] if len(t) >= 4 and isinstance(t[-1], int) else i
            out.append((t[0], t[1], t[2] if len(t) > 2 else "", num))
        return out

    def coerce(self, obj, v):
        k = self.kind
        if k == "String":
            if not isinstance(v, str):
                raise TypeError(f"expected a string type, not {type(v).__name__}")
            return v
        if k == "Bool":
            if not isinstance(v, (bool, int)):
                raise TypeError(f"expected True/False or 0/1, not {type(v).__name__}")
            return bool(v)
        if k in ("Int", "Float"):
            if isinstance(v, bool) or not isinstance(v, (int, float)) or (k == "Int" and isinstance(v, float)):
                raise TypeError(f"expected an {k.lower()} type, not {type(v).__name__}")
            v = int(v) if k == "Int" else float(v)
            if "min" in self.kw:
                v = max(v, self.kw["min"])
            if "max" in self.kw:
                v = min(v, self.kw["max"])
            return v
        if k == "Enum":
            idents = [t[0] for t in self.items(obj)]
            if v not in idents:
                raise TypeError(f"enum \"{v}\" not found in {tuple(idents)}")
            return v
        raise TypeError(f"{k}Property is read-only")

    # --- descriptor

    def __get__(self, obj, cls=None):
        if obj is None:
            return self
        if self.kind == "Enum" and "get" in self.kw:
            n = self.kw["get"](obj)
            for ident, _, _, num in self.items(obj):
                if num == n:
                    return ident
            return ""
        st = _store(obj)
        if self not in st:
            st[self] = self._initial(obj)
        return st[self]

    def __set__(self, obj, value):
        value = self.coerce(obj, value)
        if self.kind == "Enum" and "set" in self.kw:
            num = next(n for ident, _, _, n in self.items(obj) if ident == value)
            self.kw["set"](obj, num)
        else:
            _store(obj)[self] = value
        if self.kw.get("update"):
            self.kw["update"](obj, context)

    def __hash__(self):
        return id(self)

    def __eq__(self, other):
        return self is other


class PropCollection:
    def __init__(self, typ, owner):
        self._typ, self._owner, self._items = typ, owner, []

    def add(self):
        it = self._typ()
        _adopt(it, self._owner)
        self._items.append(it)
        return it

    def remove(self, i: int) -> None:
        del self._items[i]

    def clear(self) -> None:
        self._items.clear()

    def __len__(self):
        return len(self._items)

    def __iter__(self):
        return iter(list(self._items))

    def __getitem__(self, k):
        if isinstance(k, str):
            for x in self._items:
                if x.name == k:
                    return x
            raise KeyError(k)
        return self._items[k]

    def get(self, k, default=None):
        try:
            return self[k]
        except (KeyError, IndexError):
            return default


def _prop_factory(kind):
    def make(**kw):
        return Prop(kind, **kw)
    make.__name__ = kind + "Property"
    return make


# --- types ---------------------------------------------------------------------------

class PropertyGroup:
    pass


class Operator:
    bl_idname = ""
    bl_label = ""
    bl_description = ""
    bl_options: set = set()

    def report(self, kind, message: str) -> None:
        state.reports.append((sorted(kind)[0], message))


class Panel:
    bl_idname = ""
    bl_label = ""


class WindowManager(Struct):
    def progress_begin(self, lo, hi):
        state.progress.append(("begin", lo, hi))

    def progress_update(self, v):
        state.progress.append(("update", v))

    def progress_end(self):
        state.progress.append(("end",))

    def fileselect_add(self, op):
        state.fileselect = op

    def modal_handler_add(self, op) -> bool:
        state.modal.append(op)
        return True

    def event_timer_add(self, step, window=None):
        t = Struct(time_step=step)
        state.event_timers.append(t)
        return t

    def event_timer_remove(self, t):
        state.event_timers.remove(t)


class Area(Struct):
    def tag_redraw(self):
        state.redraws += 1


class Event(Struct):
    def __init__(self, type: str = "NONE", value: str = "PRESS"):  # noqa: A002 - Blender's name
        super().__init__(type=type, value=value)


# --- layout ------------------------------------------------------------------------------

class Entry(Struct):
    @property
    def enabled(self) -> bool:
        lay = self.layout
        while lay is not None:
            if not lay.enabled:
                return False
            lay = lay.parent
        return self.poll


class Layout:
    """A ``UILayout`` that records what is drawn."""

    def __init__(self, kind: str = "layout", parent: Optional["Layout"] = None):
        self.kind, self.parent = kind, parent
        self.children: List[Any] = []
        self.enabled = self.active = True
        self.alert = False
        self.use_property_split = False
        self.use_property_decorate = True
        self.scale_y = 1.0

    def _child(self, kind):
        lay = Layout(kind, self)
        self.children.append(lay)
        return lay

    def row(self, align=False, heading=""):
        return self._child("row")

    def column(self, align=False, heading=""):
        return self._child("column")

    def box(self):
        return self._child("box")

    def split(self, factor=0.0, align=False):
        return self._child("split")

    def separator(self, factor=1.0):
        pass

    def label(self, text="", icon="NONE"):
        self.children.append(Entry(kind="label", text=text, icon=icon, layout=self, poll=True))

    def prop(self, data, property, text=None, icon="NONE", toggle=-1, icon_only=False, expand=False,  # noqa: A002
             slider=False, emboss=True, placeholder="", index=-1):
        p = getattr(type(data), property, None)
        if not isinstance(p, Prop):
            raise AttributeError(f"rna_uiItemR: property not found: {type(data).__name__}.{property}")
        shown = p.kw.get("name", property) if text is None else text
        self.children.append(Entry(kind="prop", data=data, attr=property, text=shown, icon=icon, layout=self,
                                   poll=True, prop=p))

    def operator(self, operator, text=None, icon="NONE"):
        cls = state.operators.get(operator)
        if cls is None:
            raise AttributeError(f"unknown operator {operator}")
        ok = cls.poll(context) if hasattr(cls, "poll") else True
        e = Entry(kind="operator", idname=operator, text=cls.bl_label if text is None else text, icon=icon,
                  layout=self, poll=bool(ok), props=Struct())
        self.children.append(e)
        return e.props

    def panel(self, idname, default_closed=False):
        state.panels.setdefault(idname, not default_closed)
        header = self._child("panel-header")
        body = self._child("panel-body") if state.panels[idname] else None
        return header, body


def walk(layout: Layout) -> List[Entry]:
    out = []
    for c in layout.children:
        out += walk(c) if isinstance(c, Layout) else [c]
    return out


def draw(panel_cls) -> Layout:
    p = panel_cls()
    p.layout = Layout()
    if hasattr(panel_cls, "poll") and not panel_cls.poll(context):
        return p.layout
    p.draw(context)
    return p.layout


# --- app, utils, path, ops ---------------------------------------------------------------

class _Timers:
    def register(self, fn, first_interval=0.0, persistent=False):
        state.timers.append(fn)

    def unregister(self, fn):
        if fn not in state.timers:
            raise ValueError("Error: function is not registered")
        state.timers.remove(fn)

    def is_registered(self, fn) -> bool:
        return fn in state.timers


def persistent(fn):
    fn._bpy_persistent = True
    return fn


def register_class(cls) -> None:
    if cls in state.registered:
        raise ValueError(f"register_class(...): already registered as a subclass '{cls.__name__}'")
    if issubclass(cls, Operator) and not re.match(r"^[a-z0-9_]+\.[a-z0-9_]+$", cls.bl_idname):
        raise ValueError(f"{cls.__name__}: an operator's bl_idname is category.name in lower case, not {cls.bl_idname!r}")
    if issubclass(cls, Panel) and "_PT_" not in cls.bl_idname:
        raise ValueError(f"{cls.__name__}: a panel's bl_idname holds _PT_")
    for name, v in dict(cls.__dict__.get("__annotations__", {})).items():
        if not isinstance(v, Prop):
            continue
        typ = v.kw.get("type")
        if typ is not None and typ not in state.registered:
            raise ValueError(f"{cls.__name__}.{name}: {typ.__name__} must be registered first")
        setattr(cls, name, v)
    state.registered.append(cls)
    if issubclass(cls, Operator):
        state.operators[cls.bl_idname] = cls


def unregister_class(cls) -> None:
    if cls not in state.registered:
        raise RuntimeError(f"unregister_class(...): missing bl_rna attribute from '{cls.__name__}'")
    state.registered.remove(cls)
    if issubclass(cls, Operator):
        state.operators.pop(cls.bl_idname, None)


def abspath(path, *, start=None, library=None):
    """As ``bpy.path.abspath``: ``//`` is the file's folder, or the
    library's when one is given."""
    if path.startswith("//"):
        if library is not None:
            start = os.path.dirname(abspath(library.filepath))
        return os.path.join(os.path.dirname(data.filepath) if start is None else start, path[2:])
    return path


def blend_paths(absolute=False, packed=False, local=False) -> List[str]:
    out = []

    def add(p, lib=None):
        if p:
            out.append(abspath(p, library=lib) if absolute else p)

    for img in data.images:
        if img.source in ("GENERATED", "VIEWER") or (img.packed_file and not packed) or (local and img.library):
            continue
        add(img.filepath, img.library)
    for lib in data.libraries:
        add(lib.filepath, getattr(lib, "parent", None))
    for coll in (data.movieclips, data.sounds, data.cache_files, data.volumes, data.fonts):
        for x in coll:
            if (x.packed_file and not packed) or x.filepath == "<builtin>":
                continue
            add(x.filepath, x.library)
    for p in data.extra_paths:
        add(p)
    return out


def save_mainfile(filepath=None, **kw):
    path = filepath or data.filepath
    if not path:
        raise RuntimeError("Error: Unable to save an \"untitled\" file, use \"Save As\" instead")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    state.saves += 1
    with open(path, "wb") as f:
        f.write(b"BLENDER-fake save %d\n" % state.saves)
    data.filepath = path
    data.is_dirty = False
    return {"FINISHED"}


class _Op:
    def __init__(self, idname: str):
        self.idname = idname

    def __call__(self, *args, **kw):
        if self.idname == "wm.save_mainfile":
            return save_mainfile(**kw)
        cls = state.operators.get(self.idname)
        if cls is None:
            raise AttributeError(f"Calling operator \"bpy.ops.{self.idname}\" error, could not be found")
        if hasattr(cls, "poll") and not cls.poll(context):
            raise RuntimeError(f"Operator bpy.ops.{self.idname}.poll() failed, context is incorrect")
        op = cls()
        for k, v in kw.items():
            setattr(op, k, v)
        mode = args[0] if args else "EXEC_DEFAULT"
        if mode == "INVOKE_DEFAULT" and hasattr(op, "invoke"):
            return op.invoke(context, Event("NONE"))
        return op.execute(context)


class _OpsCategory:
    def __init__(self, cat: str):
        self.cat = cat

    def __getattr__(self, name):
        return _Op(f"{self.cat}.{name}")


class _Ops(types.ModuleType):
    def __getattr__(self, cat):
        if cat.startswith("__"):
            raise AttributeError(cat)
        return _OpsCategory(cat)


# --- state, and running what Blender would run ---------------------------------------------

class State:
    def __init__(self):
        self.timers: List[Callable] = []
        self.registered: List[type] = []
        self.operators: Dict[str, type] = {}
        self.reports: List[tuple] = []
        self.progress: List[tuple] = []
        self.modal: List[Any] = []
        self.event_timers: List[Any] = []
        self.fileselect = None
        self.panels: Dict[str, bool] = {}
        self.redraws = 0
        self.saves = 0


class Context(Struct):
    pass


state = State()
data = Data()
context = Context()
bpy = None   # the module, once installed


def reset(filepath: str = "", scenes=None) -> Data:
    """A new, empty file (keeping registered classes); returns ``bpy.data``."""
    global data
    data = Data(filepath)
    for s in scenes or [Scene("Scene")]:
        data.scenes.append(s)
    wm = WindowManager(clipboard="", windows=[Struct(screen=Struct(areas=[Area(type="PROPERTIES"),
                                                                          Area(type="VIEW_3D")]))])
    context.__dict__.clear()
    context.__dict__.update(scene=data.scenes[0], window_manager=wm, window=wm.windows[0],
                            region=Struct(width=420), preferences=Struct(system=Struct(ui_scale=1.0)))
    regs, ops = state.registered, state.operators
    state.__init__()
    state.registered, state.operators = regs, ops
    if bpy is not None:
        bpy.data = data
        bpy.context = context
    return data


def run_timers() -> None:
    for fn in list(state.timers):
        if fn not in state.timers:
            continue
        r = fn()
        if r is None and fn in state.timers:
            state.timers.remove(fn)


def send(event_type: str = "TIMER") -> None:
    """An event to every running modal operator, as Blender sends one."""
    for op in list(state.modal):
        r = op.modal(context, Event(event_type))
        if r & {"FINISHED", "CANCELLED"}:
            state.modal.remove(op)


def settle(timeout: float = 20.0) -> None:
    """Runs timers and modal operators until none is left, as Blender's
    event loop would, while worker threads finish."""
    end = time.time() + timeout
    while state.timers or state.modal:
        run_timers()
        send("TIMER")
        if time.time() > end:
            raise AssertionError(f"still busy after {timeout}s: {state.timers} {state.modal}")
        time.sleep(0.002)


def choose_file(path: str):
    """The artist picks a path in the file browser an operator opened."""
    op, state.fileselect = state.fileselect, None
    assert op is not None, "no file browser is open"
    op.filepath = path
    return op.execute(context)


# --- installing ------------------------------------------------------------------------------

def _build():
    m = types.ModuleType("bpy")
    m.__file__ = __file__
    t = types.ModuleType("bpy.types")
    for c in (PropertyGroup, Operator, Panel, Scene, WindowManager, Context):
        setattr(t, c.__name__, c)
    p = types.ModuleType("bpy.props")
    for kind in ("String", "Bool", "Int", "Float", "Enum", "Pointer", "Collection"):
        setattr(p, kind + "Property", _prop_factory(kind))
    u = types.ModuleType("bpy.utils")
    u.register_class, u.unregister_class, u.blend_paths = register_class, unregister_class, blend_paths
    pa = types.ModuleType("bpy.path")
    pa.abspath = abspath
    pa.basename = lambda path: os.path.basename(path[2:] if path.startswith("//") else path)
    app = types.ModuleType("bpy.app")
    app.version = (4, 2, 0)
    app.background = False
    app.timers = _Timers()
    h = types.ModuleType("bpy.app.handlers")
    h.persistent = persistent
    h.load_post = []
    app.handlers = h
    m.types, m.props, m.utils, m.path, m.app, m.ops = t, p, u, pa, app, _Ops("bpy.ops")
    m.data, m.context = data, context
    return m


_modules: Dict[str, Any] = {}


def install():
    """Puts the fake in ``sys.modules`` as ``bpy``."""
    global bpy
    if bpy is None:
        bpy = _build()
        _modules.update({"bpy": bpy, "bpy.types": bpy.types, "bpy.props": bpy.props, "bpy.utils": bpy.utils,
                         "bpy.path": bpy.path, "bpy.app": bpy.app, "bpy.app.timers": bpy.app.timers,
                         "bpy.app.handlers": bpy.app.handlers, "bpy.ops": bpy.ops})
        reset()
    sys.modules.update(_modules)
    return bpy


def uninstall() -> None:
    for name in MODULE_NAMES:
        if sys.modules.get(name) is _modules.get(name):
            sys.modules.pop(name, None)
