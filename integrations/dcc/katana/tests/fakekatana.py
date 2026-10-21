"""A fake ``Katana`` (``FarmAPI``, ``NodegraphAPI``, ``KatanaFile``,
``Callbacks``, and its ``QtCore``/``QtWidgets`` from fakekatanaqt), with
only what c4_katana uses.

A project is nodes with parameters (groups of string and number
parameters, with UI hints) and ports, groups with send and return ports
inside them, and Render nodes that say what FarmAPI says of them: the
Render nodes they depend on, their outputs and their frame range. Build
one with the helpers at the bottom (``node``, ``render``, ``group``).

What the fakes cannot prove about the real Katana is in CHECKLIST.md.
"""

import sys
import types
from typing import Any, Callable, Dict, List, Optional

import fakekatanaqt


# --- parameters ----------------------------------------------------------------

class Parameter:
    def __init__(self, node: "Node", name: str, value: Any = None, hint: Optional[dict] = None,
                 parent: Optional["Parameter"] = None):
        self.node, self.name, self.parent = node, name, parent
        self.hint = hint or {}
        self.children: List[Parameter] = []
        if isinstance(value, dict):
            self.type, self.value = "group", None
            for k, v in value.items():
                h = None
                if isinstance(v, tuple):
                    v, h = v
                self.children.append(Parameter(node, k, v, h, self))
        elif isinstance(value, (int, float)):
            self.type, self.value = "number", float(value)
        else:
            self.type, self.value = "string", value if value is not None else ""

    def getName(self) -> str:  # noqa: N802
        return self.name

    def getFullName(self) -> str:  # noqa: N802
        parts, p = [], self
        while p is not None and p.parent is not None:
            parts.append(p.name)
            p = p.parent
        return ".".join([self.node.getName()] + parts[::-1])

    def getType(self) -> str:  # noqa: N802
        return self.type

    def getChildren(self) -> List["Parameter"]:  # noqa: N802
        return list(self.children)

    def getChild(self, name: str) -> Optional["Parameter"]:  # noqa: N802
        return next((c for c in self.children if c.name == name), None)

    def getValue(self, time: float):  # noqa: N802
        if callable(self.value):
            return self.value(time)
        return self.value

    def setValue(self, v: Any, time: float = 0) -> None:  # noqa: N802
        self.value = float(v) if self.type == "number" else v
        _s.dirty = True

    def getHintString(self) -> str:  # noqa: N802
        return repr(self.hint) if self.hint else ""


# --- ports and nodes ------------------------------------------------------------

class Port:
    def __init__(self, node: "Node", name: str, kind: str):
        self.node, self.name, self.kind = node, name, kind
        self.connected: List[Port] = []

    def getNode(self) -> "Node":  # noqa: N802
        return self.node

    def getName(self) -> str:  # noqa: N802
        return self.name

    def getConnectedPorts(self) -> List["Port"]:  # noqa: N802
        return list(self.connected)

    def connect(self, other: "Port") -> None:
        self.connected.append(other)
        other.connected.append(self)

    def __repr__(self):
        return f"<Port {self.node.getName()}.{self.name} {self.kind}>"


class Node:
    def __init__(self, ntype: str, name: str, parent: Optional["Node"] = None, params: Optional[dict] = None):
        if name in _s.nodes:
            raise ValueError(f"Katana keeps node names unique: {name} exists")
        self.type, self.name, self.parent = ntype, name, parent
        self.params = Parameter(self, "", dict(params or {}))
        self.inputs: List[Port] = []
        self.outputs = [Port(self, "out", "out")]
        self.sends: Dict[str, Port] = {}
        self.returns: Dict[str, Port] = {}
        self.children: List[Node] = []
        self.bypassed = False
        self.selected = False
        # What FarmAPI says of a Render node.
        self.deps: List[str] = []
        self.render_outputs: List[dict] = []
        self.range: Optional[tuple] = None
        if parent is not None:
            parent.children.append(self)
        _s.nodes[name] = self

    def getName(self) -> str:  # noqa: N802
        return self.name

    def getType(self) -> str:  # noqa: N802
        return self.type

    def getParent(self) -> Optional["Node"]:  # noqa: N802
        return self.parent if self.parent is not None else _s.root

    def getChildren(self) -> List["Node"]:  # noqa: N802
        return list(self.children)

    def getParameters(self) -> Parameter:  # noqa: N802
        return self.params

    def getParameter(self, path: str) -> Optional[Parameter]:  # noqa: N802
        p: Optional[Parameter] = self.params
        for part in path.split("."):
            p = p.getChild(part) if p is not None else None
        return p

    def getInputPorts(self) -> List[Port]:  # noqa: N802
        return list(self.inputs)

    def getInputPort(self, name: str) -> Optional[Port]:  # noqa: N802
        return next((p for p in self.inputs if p.name == name), None)

    def getOutputPorts(self) -> List[Port]:  # noqa: N802
        return list(self.outputs)

    def getSendPort(self, name: str) -> Optional[Port]:  # noqa: N802
        return self.sends.get(name)

    def getReturnPort(self, name: str) -> Optional[Port]:  # noqa: N802
        return self.returns.get(name)

    def isBypassed(self) -> bool:  # noqa: N802
        return self.bypassed

    def setBypassed(self, on: bool) -> None:  # noqa: N802
        self.bypassed = bool(on)
        _s.dirty = True

    def add_input(self, src: "Node") -> Port:
        port = Port(self, f"i{len(self.inputs)}", "in")
        self.inputs.append(port)
        if src is not None:
            port.connect(src.outputs[0])
        return port

    def __repr__(self):
        return f"<{self.type} {self.name}>"


class _State:
    def __init__(self):
        self.nodes: Dict[str, Node] = {}
        self.root: Optional[Node] = None
        self.path = ""
        self.dirty = False
        self.saves = 0
        self.start, self.end = 1001.0, 1048.0
        self.menu: List[tuple] = []
        self.popup: List[tuple] = []
        self.callbacks: List[tuple] = []
        self.clicked: Optional[Node] = None
        self.dependency_calls: List[Any] = []


_s = _State()


def state() -> _State:
    return _s


# --- NodegraphAPI ---------------------------------------------------------------

NodegraphAPI = types.ModuleType("NodegraphAPI")


def GetAllNodes():  # noqa: N802
    return [n for n in _s.nodes.values()]


def GetAllNodesByType(t: str):  # noqa: N802
    return [n for n in _s.nodes.values() if n.type == t]


def GetNode(name: str):  # noqa: N802
    return _s.nodes.get(name)


def GetRootNode():  # noqa: N802
    return _s.root


def GetSourceFile():  # noqa: N802
    return _s.path


def GetAllSelectedNodes():  # noqa: N802
    return [n for n in _s.nodes.values() if n.selected]


for _f in (GetAllNodes, GetAllNodesByType, GetNode, GetRootNode, GetSourceFile, GetAllSelectedNodes):
    setattr(NodegraphAPI, _f.__name__, _f)


# --- FarmAPI ---------------------------------------------------------------------

FarmAPI = types.ModuleType("FarmAPI")


def GetKatanaFileName():  # noqa: N802
    return _s.path


def GetSceneFrameRange():  # noqa: N802
    return {"start": _s.start, "end": _s.end}


def GetSortedDependencyList(nodeList=None, *a, **kw):  # noqa: N802, N803
    """Each Render node's entry, the ones it depends on first."""
    _s.dependency_calls.append(nodeList)
    nodes = list(nodeList) if nodeList is not None else GetAllNodesByType("Render")
    by = {n.name: n for n in nodes}
    out, done = [], set()

    def visit(n: Node, stack=()) -> None:
        if n.name in done:
            return
        if n.name in stack:
            raise RuntimeError(f"dependency cycle through {n.name}")
        for d in n.deps:
            if d in by:
                visit(by[d], stack + (n.name,))
        done.add(n.name)
        e = {"name": n.name, "service": "prman", "deps": list(n.deps), "outputs": [dict(o) for o in n.render_outputs]}
        if n.range is not None:
            e["range"] = n.range
        out.append(e)

    for n in nodes:
        visit(n)
    return out


def AddFarmMenuOption(name: str, callback: Callable):  # noqa: N802
    _s.menu.append((name, callback))


def AddFarmPopupMenuOption(name: str, callback: Callable):  # noqa: N802
    _s.popup.append((name, callback))


def GetCurrentNode():  # noqa: N802
    return _s.clicked


for _f in (GetKatanaFileName, GetSceneFrameRange, GetSortedDependencyList, AddFarmMenuOption,
           AddFarmPopupMenuOption, GetCurrentNode):
    setattr(FarmAPI, _f.__name__, _f)


# --- KatanaFile and Callbacks -------------------------------------------------------

KatanaFile = types.ModuleType("KatanaFile")


def IsFileDirty():  # noqa: N802
    return _s.dirty


def Save(path: str, *a, **kw):  # noqa: N802
    with open(path, "ab") as f:
        f.write(b"<!-- saved -->\n")
    _s.dirty = False
    _s.saves += 1


KatanaFile.IsFileDirty, KatanaFile.Save = IsFileDirty, Save

Callbacks = types.ModuleType("Callbacks")


class _Type:
    onStartupComplete = "onStartupComplete"


def addCallback(kind: str, fn: Callable):  # noqa: N802
    _s.callbacks.append((kind, fn))


Callbacks.Type, Callbacks.addCallback = _Type, addCallback


def fire(kind: str) -> int:
    fns = [fn for k, fn in _s.callbacks if k == kind]
    for fn in fns:
        fn(objectHash=None)
    return len(fns)


Katana = types.ModuleType("Katana")
Katana.FarmAPI, Katana.NodegraphAPI, Katana.KatanaFile, Katana.Callbacks = FarmAPI, NodegraphAPI, KatanaFile, Callbacks
Katana.QtCore, Katana.QtWidgets = fakekatanaqt.QtCore, fakekatanaqt.QtWidgets


def install() -> None:
    """Puts the fake in ``sys.modules`` as ``Katana``."""
    sys.modules["Katana"] = Katana


# --- building projects ------------------------------------------------------------------

def reset(path: str = "", first: float = 1001, last: float = 1048, dirty: bool = False) -> _State:
    global _s
    _s = _State()
    _s.path, _s.start, _s.end, _s.dirty = path, float(first), float(last), dirty
    _s.root = Node("Group", "rootNode", params={"inTime": float(first), "outTime": float(last)})
    del _s.nodes["rootNode"]   # the root holds the graph; FarmAPI never lists it
    fakekatanaqt.reset()
    return _s


def node(ntype: str, name: str, *inputs: Optional[Node], parent: Optional[Node] = None, **params) -> Node:
    n = Node(ntype, name, parent, params)
    for src in inputs:
        n.add_input(src)
    return n


def group(name: str, *inputs: Optional[Node], parent: Optional[Node] = None, ntype: str = "Group") -> Node:
    """A group; its inputs are sent inside through its send ports."""
    g = Node(ntype, name, parent)
    g.type = ntype
    for src in inputs:
        port = Port(g, f"i{len(g.inputs)}", "in")
        g.inputs.append(port)
        g.sends[port.name] = Port(g, port.name, "send")
        if src is not None:
            port.connect(src.outputs[0])
    g.returns["out"] = Port(g, "out", "return")
    return g


def from_group_input(inner: Node, g: Node, name: str = "i0") -> None:
    """An inner node takes the group's input."""
    port = Port(inner, f"i{len(inner.inputs)}", "in")
    inner.inputs.append(port)
    port.connect(g.sends[name])


def render(name: str, *inputs: Optional[Node], outputs=(), deps=(), frames: Optional[tuple] = None,
           parent: Optional[Node] = None) -> Node:
    """A Render node: what it renders to, the Render nodes it depends on,
    and its own frame range when it has one."""
    n = node("Render", name, *inputs, parent=parent)
    n.render_outputs = [o if isinstance(o, dict) else {"name": "primary", "outputLocation": o, "enabled": True}
                        for o in outputs]
    n.deps = list(deps)
    n.range = frames
    return n


reset()
