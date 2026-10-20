"""A fake ``nuke`` and ``nukescripts``, with only what c4_nuke uses.

A script is nodes with knobs, inputs and groups, built by the helpers at
the bottom (``read``, ``write``, ``group``). Knobs record their arguments,
and every knob made is kept in ``created`` so a test can read everything
the artist would. ``executeInMainThread`` queues, as Nuke does; a test runs
the queue with ``run_main()`` (``settle(panel)`` does both).

What the fakes cannot prove about the real ``nuke`` is in CHECKLIST.md.
"""

import os
import sys
import threading
import types
from typing import Any, Callable, Dict, List, Optional

STARTLINE = 0x1000
READ_ONLY = 0x10000000
TABBEGINGROUP = 1
TABBEGINCLOSEDGROUP = 2
TABENDGROUP = -1
INPUTS = 1
HIDDEN_INPUTS = 2
EXPRESSIONS = 4

created: List["Knob"] = []


# --- knobs ---------------------------------------------------------------------

class Knob:
    default: Any = None

    def __init__(self, name: str, label: Optional[str] = None, value: Any = None, *rest):
        self._name = name
        self._label = name if label is None else label
        self._value = self.default if value is None else value
        self.args = (name, label, value) + rest
        self.flags = set()
        self._enabled = True
        self._visible = True
        self._tooltip = ""
        self.panel = None
        self.range = None
        created.append(self)

    def Class(self) -> str:  # noqa: N802
        return type(self).__name__

    def name(self) -> str:
        return self._name

    def label(self) -> str:
        return self._label

    def value(self):
        return self._value

    def getValue(self):  # noqa: N802
        return self._value

    def evaluate(self):
        return self._value

    def setValue(self, v) -> bool:  # noqa: N802
        self._value = v
        if self.panel is not None:
            self.panel.knobChanged(self)   # as the artist's edits do
        return True

    def setFlag(self, f):  # noqa: N802
        self.flags.add(f)

    def clearFlag(self, f):  # noqa: N802
        self.flags.discard(f)

    def getFlag(self, f):  # noqa: N802
        return f in self.flags

    def setEnabled(self, on):  # noqa: N802
        self._enabled = bool(on)

    def enabled(self) -> bool:
        return self._enabled

    def setVisible(self, on):  # noqa: N802
        self._visible = bool(on)

    def visible(self) -> bool:
        return self._visible

    def setTooltip(self, t):  # noqa: N802
        self._tooltip = t

    def tooltip(self) -> str:
        return self._tooltip

    def __repr__(self):
        return f"{self.Class()}({self._name}={self._value!r})"


class String_Knob(Knob):  # noqa: N801
    default = ""


class Multiline_Eval_String_Knob(String_Knob):  # noqa: N801
    pass


class EvalString_Knob(String_Knob):  # noqa: N801
    pass


class File_Knob(EvalString_Knob):  # noqa: N801
    def __init__(self, name, label=None, value=None):
        super().__init__(name, label, value)


class Boolean_Knob(Knob):  # noqa: N801
    default = False

    def value(self):
        return bool(self._value)


class Int_Knob(Knob):  # noqa: N801
    default = 0

    def __init__(self, name, label=None):
        super().__init__(name, label)

    def setRange(self, lo, hi):  # noqa: N802
        self.range = (lo, hi)

    def setValue(self, v):  # noqa: N802
        return super().setValue(int(v))


class Double_Knob(Int_Knob):  # noqa: N801
    default = 0.0

    def setValue(self, v):  # noqa: N802
        return Knob.setValue(self, float(v))


class Enumeration_Knob(Knob):  # noqa: N801
    def __init__(self, name, label, items):
        self._items = [str(i) for i in items]
        super().__init__(name, label, self._items[0] if self._items else "", list(items))

    def values(self):
        return list(self._items)

    def setValues(self, items):  # noqa: N802
        self._items = [str(i) for i in items]
        if self._value not in self._items:
            self._value = self._items[0] if self._items else ""

    def setValue(self, v):  # noqa: N802
        if isinstance(v, int):
            v = self._items[v]
        if v not in self._items:
            raise ValueError(f"{v!r} is not one of {self._items}")
        return super().setValue(v)


class Text_Knob(Knob):  # noqa: N801
    def __init__(self, name, label=None, text=""):
        super().__init__(name, label, text)


class PyScript_Knob(Knob):  # noqa: N801
    def __init__(self, name, label=None, command=""):
        super().__init__(name, label, command)


class Tab_Knob(Knob):  # noqa: N801
    def __init__(self, name, label=None, kind=0):
        super().__init__(name, label, kind)


# --- nodes and the script -------------------------------------------------------------

class Node:
    def __init__(self, cls: str, name: str, parent: Optional["Node"] = None, inputs=(), **knobs):
        self._class, self._name, self._parent = cls, name, parent
        self._inputs = list(inputs)
        self._knobs: Dict[str, Knob] = {}
        self._expr: List[Node] = []
        self.selected = False
        for k, v in knobs.items():
            self.add(k, v)
        if cls != "Root":
            _s.nodes.append(self)

    def add(self, name: str, value: Any, kind: Optional[type] = None) -> Knob:
        if kind is None:
            if isinstance(value, bool):
                kind = Boolean_Knob
            elif name in ("file", "proxy", "customOCIOConfigPath") or name.endswith("_file"):
                kind = File_Knob
            elif isinstance(value, int):
                kind = Int_Knob
            else:
                kind = String_Knob
        k = kind(name, name) if kind in (Int_Knob, Double_Knob, File_Knob) else kind(name, name, value)
        if kind in (Int_Knob, Double_Knob, File_Knob):
            k._value = value
        self._knobs[name] = k
        return k

    def Class(self) -> str:  # noqa: N802
        return self._class

    def name(self) -> str:
        return self._name

    def fullName(self) -> str:  # noqa: N802
        return f"{self._parent.fullName()}.{self._name}" if self._parent is not None else self._name

    def knobs(self) -> Dict[str, Knob]:
        return dict(self._knobs)

    def knob(self, name: str) -> Optional[Knob]:
        return self._knobs.get(name)

    def __getitem__(self, name: str) -> Knob:
        return self._knobs[name]

    def inputs(self) -> int:
        return len(self._inputs)

    def input(self, i: int) -> Optional["Node"]:
        return self._inputs[i] if 0 <= i < len(self._inputs) else None

    def setInput(self, i: int, n: Optional["Node"]) -> None:  # noqa: N802
        while len(self._inputs) <= i:
            self._inputs.append(None)
        self._inputs[i] = n

    def dependencies(self, what: int = INPUTS) -> List["Node"]:
        out = []
        if what & INPUTS:
            out += [n for n in self._inputs if n is not None]
        if what & EXPRESSIONS:
            out += self._expr
        return out

    def isSelected(self) -> bool:  # noqa: N802
        return self.selected

    def __repr__(self):
        return f"<{self._class} {self.fullName()}>"


class Root(Node):
    def __init__(self, path: str, first: int, last: int):
        super().__init__("Root", "Root", first_frame=first, last_frame=last, project_directory="",
                         customOCIOConfigPath="", OCIO_config="nuke-default")
        self.path = path
        self._modified = False

    def name(self) -> str:
        return self.path or "Root"

    def modified(self) -> bool:
        return self._modified

    def setModified(self, on: bool) -> None:  # noqa: N802
        self._modified = bool(on)


class _State:
    def __init__(self):
        self.nodes: List[Node] = []
        self.root: Optional[Root] = None
        self.saves = 0
        self.views = ["main"]
        self.menus: Dict[str, "Menu"] = {}
        self.messages: List[str] = []
        self.next_filename: Optional[str] = None
        self.asked: List[dict] = []
        self.cancel = False
        self.tasks: List["ProgressTask"] = []
        self.queue: List[tuple] = []
        self.lock = threading.Lock()
        self.plugin_path: List[str] = []


_s = _State()


def reset(path: str = "", first: int = 1001, last: int = 1048) -> Root:
    """A new, empty script (saved at ``path`` when given)."""
    global _s
    _s = _State()
    _s.root = Root(path, first, last)
    del created[:]
    return _s.root


def state() -> _State:
    return _s


def root() -> Root:
    return _s.root


def allNodes(filter: Optional[str] = None, group: Optional[Node] = None, recurseGroups: bool = False):  # noqa: N802, A002
    parent = None if group is None or isinstance(group, Root) else group
    out = []
    for n in _s.nodes:
        p = n._parent
        inside = p is parent
        if not inside and recurseGroups:
            while p is not None and p is not parent:
                p = p._parent
            inside = p is parent
        if inside and (filter is None or n.Class() == filter):
            out.append(n)
    return out


def toNode(name: str) -> Optional[Node]:  # noqa: N802
    for n in _s.nodes:
        if n.fullName() == name:
            return n
    return None


def filename(node: Node, kind: Any = None) -> Optional[str]:
    k = node.knob("file")
    v = k.value() if k is not None else ""
    return v or None


def scriptSave(name: str = "") -> bool:  # noqa: N802
    path = name or _s.root.path
    with open(path, "ab") as f:
        f.write(b"# saved\n")
    _s.root._modified = False
    _s.saves += 1
    return True


def selectedNodes(filter: Optional[str] = None):  # noqa: N802, A002
    return [n for n in _s.nodes if n.selected and (filter is None or n.Class() == filter)]


def selectedNode():  # noqa: N802
    sel = selectedNodes()
    if not sel:
        raise ValueError("no node selected")
    return sel[-1]


def views():
    return list(_s.views)


def pluginPath():  # noqa: N802
    return list(_s.plugin_path)


def message(text: str) -> None:
    _s.messages.append(text)


def getFilename(message: str, pattern: Optional[str] = None, default: Optional[str] = None,  # noqa: N802
                favorites: Any = None, type: str = "", multiple: bool = False):  # noqa: A002
    _s.asked.append({"message": message, "pattern": pattern, "default": default, "type": type})
    return _s.next_filename


def executeInMainThread(call: Callable, args=(), kwargs=None) -> None:  # noqa: N802
    if not isinstance(args, tuple):
        args = (args,)
    with _s.lock:
        _s.queue.append((call, args, kwargs or {}))


def run_main() -> int:
    """Runs what workers queued for the main thread; returns how many."""
    n = 0
    while True:
        with _s.lock:
            if not _s.queue:
                return n
            call, args, kwargs = _s.queue.pop(0)
        call(*args, **kwargs)
        n += 1


class ProgressTask:
    def __init__(self, title: str):
        self.title = title
        self.messages: List[str] = []
        self.progress: List[int] = []
        _s.tasks.append(self)

    def setMessage(self, m: str) -> None:  # noqa: N802
        self.messages.append(m)

    def setProgress(self, p: int) -> None:  # noqa: N802
        self.progress.append(int(p))

    def isCancelled(self) -> bool:  # noqa: N802
        return _s.cancel


class Menu:
    def __init__(self, name: str):
        self.name = name
        self.items: Dict[str, Any] = {}

    def addMenu(self, name: str, **kw) -> "Menu":  # noqa: N802
        m = self.items.get(name)
        if not isinstance(m, Menu):
            m = self.items[name] = Menu(name)
        return m

    def addCommand(self, name: str, command: Any = None, shortcut: str = "", **kw):  # noqa: N802
        parts = name.split("/")
        m = self
        for p in parts[:-1]:
            m = m.addMenu(p)
        m.items[parts[-1]] = command
        return command

    def findItem(self, path: str):  # noqa: N802
        m: Any = self
        for p in path.split("/"):
            if not isinstance(m, Menu) or p not in m.items:
                return None
            m = m.items[p]
        return m


def menu(name: str) -> Menu:
    return _s.menus.setdefault(name, Menu(name))


GUI = True


# --- nukescripts -----------------------------------------------------------------------

class PythonPanel:
    def __init__(self, title: str = "", id: str = "", scrollable: bool = True):  # noqa: A002
        self.title, self.id = title, id
        self._knobs: List[Knob] = []
        self.shown = 0

    def addKnob(self, k: Knob) -> None:  # noqa: N802
        if any(x.name() == k.name() for x in self._knobs):
            raise ValueError(f"the panel already has a knob {k.name()}")
        k.panel = self
        self._knobs.append(k)

    def removeKnob(self, k: Knob) -> None:  # noqa: N802
        self._knobs.remove(k)
        k.panel = None

    def knobs(self) -> Dict[str, Knob]:
        return {k.name(): k for k in self._knobs}

    def order(self) -> List[str]:
        return [k.name() for k in self._knobs]

    def show(self) -> None:
        self.shown += 1
        self.knobChanged(Knob("showPanel"))

    def knobChanged(self, knob: Knob) -> None:  # noqa: N802
        pass


nukescripts = types.ModuleType("nukescripts")
nukescripts.PythonPanel = PythonPanel


def install() -> None:
    """Puts this module in ``sys.modules`` as ``nuke`` and ``nukescripts``."""
    sys.modules["nuke"] = sys.modules[__name__]
    sys.modules["nukescripts"] = nukescripts


# --- building scripts -------------------------------------------------------------------

def node(cls: str, name: str, *inputs: Optional[Node], parent: Optional[Node] = None, **knobs) -> Node:
    return Node(cls, name, parent=parent, inputs=inputs, **knobs)


def read(name: str, file: str, first: int = 1001, last: int = 1048, parent: Optional[Node] = None,
         disable: bool = False, **knobs) -> Node:
    return Node("Read", name, parent=parent, file=file, first=first, last=last, disable=disable, **knobs)


def write(name: str, file: str, *inputs: Optional[Node], first: Optional[int] = None, last: Optional[int] = None,
          disable: bool = False, reading: bool = False, cls: str = "Write", parent: Optional[Node] = None) -> Node:
    use = first is not None
    return Node(cls, name, parent=parent, inputs=inputs, file=file, use_limit=use,
                first=first if use else 1, last=last if use else 100, disable=disable, reading=reading)


def group(name: str, *inputs: Optional[Node], parent: Optional[Node] = None) -> Node:
    return Node("Group", name, parent=parent, inputs=inputs)


def type_into(panel: PythonPanel, name: str, value: Any) -> None:
    """The artist changes a knob."""
    panel.knobs()[name].setValue(value)


def press(panel: PythonPanel, name: str) -> bool:
    """The artist presses a button; False when it is greyed out."""
    k = panel.knobs()[name]
    if not k.enabled():
        return False
    panel.knobChanged(k)
    return True


def settle(panel: Any) -> None:
    """Lets the panel's workers finish and runs what they queued."""
    while True:
        panel.wait(10)
        if not run_main():
            return


reset()
