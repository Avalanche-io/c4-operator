"""A fake ``hou``, and a minimal fake ``pxr``, holding only what c4_houdini
uses. ``install()`` puts them in ``sys.modules``.

Constructors take the keyword arguments Houdini's take (and no others), so
a misspelt argument fails here as it would in Houdini. What the fakes
cannot prove (the real behaviour of a USD ROP, ``hou.fileReferences``,
HDA definitions) is in CHECKLIST.md.

The fake USD ROP writes the stage's dependencies as JSON to its output
file, and the fake ``UsdUtils.ComputeAllDependencies`` reads them back, so
a test sees the file written and the dependencies read from it.
"""

import copy
import json
import os
import re
import sys
import types
from typing import Any, Dict, List, Optional


# --- errors and enums ---------------------------------------------------------


class Error(Exception):
    pass


class OperationFailed(Error):
    pass


class OperationInterrupted(Error):
    pass


class _Enum:
    def __init__(self, kind: str, *names: str):
        for n in names:
            setattr(self, n, f"{kind}.{n}")


parmTemplateType = _Enum("parmTemplateType", "String", "Int", "Float", "Toggle", "Button", "Folder", "Label",
                         "Separator", "Menu")
stringParmType = _Enum("stringParmType", "Regular", "FileReference", "NodeReference", "NodeReferenceList")
fileType = _Enum("fileType", "Any", "Image", "Geometry", "Directory", "Usd", "Hip")
menuType = _Enum("menuType", "Normal", "Mini", "StringReplace", "StringToggle", "ControlNextParameter")
folderType = _Enum("folderType", "Tabs", "Simple", "Collapsible", "RadioButtons", "MultiparmBlock")
scriptLanguage = _Enum("scriptLanguage", "Python", "Hscript")
parmCondType = _Enum("parmCondType", "DisableWhen", "HideWhen", "NoCookWhen")
parmNamingScheme = _Enum("parmNamingScheme", "Base1", "XYZW", "XYWH", "UVW", "RGBA", "MinMax", "MaxMin", "StartEnd",
                         "BeginEnd")
parmLook = _Enum("parmLook", "Regular")
fileChooserMode = _Enum("fileChooserMode", "Read", "Write", "ReadAndWrite")
severityType = _Enum("severityType", "Message", "ImportantMessage", "Warning", "Error", "Fatal")


# --- parameter templates ----------------------------------------------------------


class ParmTemplate:
    _type = ""

    def _common(self, name, label, disable_when=None, is_hidden=False, is_label_hidden=False, join_with_next=False,
                help=None, script_callback=None, script_callback_language=None, tags=None):
        self._name, self._label = name, label
        self._conditionals = {}
        if disable_when:
            self._conditionals[parmCondType.DisableWhen] = disable_when
        self._hidden, self._label_hidden, self._join = is_hidden, is_label_hidden, join_with_next
        self._help = help
        self._callback, self._callback_language = script_callback or "", script_callback_language
        self._tags = dict(tags or {})
        self._num = 1
        self._default: tuple = ()
        self._default_expr: tuple = ()
        self._scheme = parmNamingScheme.Base1

    def name(self):
        return self._name

    def label(self):
        return self._label

    def type(self):
        return self._type

    def tags(self):
        return dict(self._tags)

    def help(self):
        return self._help or ""

    def isHidden(self):
        return self._hidden

    def joinsWithNext(self):
        return self._join

    def conditionals(self):
        return dict(self._conditionals)

    def setConditional(self, cond_type, conditional):
        self._conditionals[cond_type] = conditional

    def disableWhen(self):
        return self._conditionals.get(parmCondType.DisableWhen, "")

    def scriptCallback(self):
        return self._callback

    def scriptCallbackLanguage(self):
        return self._callback_language

    def numComponents(self):
        return self._num

    def defaultValue(self):
        return self._default

    def defaultExpression(self):
        return self._default_expr

    def namingScheme(self):
        return self._scheme

    def __repr__(self):
        return f"<{type(self).__name__} {self._name}>"


class _MenuMixin:
    def _menu(self, menu_items, menu_labels, item_generator_script, item_generator_script_language, menu_type):
        self._items, self._labels = tuple(menu_items), tuple(menu_labels)
        self._script, self._script_language = item_generator_script or "", item_generator_script_language
        self._menu_type = menu_type

    def menuItems(self):
        return self._items

    def menuLabels(self):
        return self._labels

    def itemGeneratorScript(self):
        return self._script

    def itemGeneratorScriptLanguage(self):
        return self._script_language

    def menuType(self):
        return self._menu_type


class StringParmTemplate(ParmTemplate, _MenuMixin):
    _type = parmTemplateType.String

    def __init__(self, name, label, num_components, default_value=(), naming_scheme=parmNamingScheme.Base1,
                 string_type=stringParmType.Regular, file_type=fileType.Any, menu_items=(), menu_labels=(),
                 icon_names=(), item_generator_script=None, item_generator_script_language=None,
                 menu_type=menuType.Normal, disable_when=None, is_hidden=False, is_label_hidden=False,
                 join_with_next=False, help=None, script_callback=None, script_callback_language=scriptLanguage.Hscript,
                 tags={}, default_expression=(), default_expression_language=()):
        self._common(name, label, disable_when, is_hidden, is_label_hidden, join_with_next, help, script_callback,
                     script_callback_language, tags)
        self._num, self._default, self._default_expr = num_components, tuple(default_value), tuple(default_expression)
        self._scheme = naming_scheme
        self._string_type, self._file_type = string_type, file_type
        self._menu(menu_items, menu_labels, item_generator_script, item_generator_script_language, menu_type)

    def stringType(self):
        return self._string_type

    def fileType(self):
        return self._file_type


class IntParmTemplate(ParmTemplate, _MenuMixin):
    _type = parmTemplateType.Int

    def __init__(self, name, label, num_components, default_value=(), min=0, max=10, min_is_strict=False,
                 max_is_strict=False, naming_scheme=parmNamingScheme.Base1, menu_items=(), menu_labels=(),
                 icon_names=(), item_generator_script=None, item_generator_script_language=None,
                 menu_type=menuType.Normal, disable_when=None, is_hidden=False, is_label_hidden=False,
                 join_with_next=False, help=None, script_callback=None, script_callback_language=scriptLanguage.Hscript,
                 tags={}, default_expression=(), default_expression_language=()):
        self._common(name, label, disable_when, is_hidden, is_label_hidden, join_with_next, help, script_callback,
                     script_callback_language, tags)
        self._num, self._default, self._default_expr = num_components, tuple(default_value), tuple(default_expression)
        self._scheme = naming_scheme
        self._min, self._max, self._min_strict, self._max_strict = min, max, min_is_strict, max_is_strict
        self._menu(menu_items, menu_labels, item_generator_script, item_generator_script_language, menu_type)

    def minValue(self):
        return self._min

    def maxValue(self):
        return self._max

    def minIsStrict(self):
        return self._min_strict

    def maxIsStrict(self):
        return self._max_strict


class FloatParmTemplate(ParmTemplate):
    _type = parmTemplateType.Float

    def __init__(self, name, label, num_components, default_value=(), min=0.0, max=10.0, min_is_strict=False,
                 max_is_strict=False, look=parmLook.Regular, naming_scheme=parmNamingScheme.XYZW, disable_when=None,
                 is_hidden=False, is_label_hidden=False, join_with_next=False, help=None, script_callback=None,
                 script_callback_language=scriptLanguage.Hscript, tags={}, default_expression=(),
                 default_expression_language=()):
        self._common(name, label, disable_when, is_hidden, is_label_hidden, join_with_next, help, script_callback,
                     script_callback_language, tags)
        self._num, self._default, self._default_expr = num_components, tuple(default_value), tuple(default_expression)
        self._scheme = naming_scheme
        self._min, self._max, self._min_strict, self._max_strict = min, max, min_is_strict, max_is_strict

    def minValue(self):
        return self._min

    def maxValue(self):
        return self._max

    def minIsStrict(self):
        return self._min_strict

    def maxIsStrict(self):
        return self._max_strict


class ToggleParmTemplate(ParmTemplate):
    _type = parmTemplateType.Toggle

    def __init__(self, name, label, default_value=False, disable_when=None, is_hidden=False, is_label_hidden=False,
                 join_with_next=False, help=None, script_callback=None, script_callback_language=scriptLanguage.Hscript,
                 tags={}, default_expression="", default_expression_language=scriptLanguage.Hscript):
        self._common(name, label, disable_when, is_hidden, is_label_hidden, join_with_next, help, script_callback,
                     script_callback_language, tags)
        self._default = (bool(default_value),)


class ButtonParmTemplate(ParmTemplate):
    _type = parmTemplateType.Button

    def __init__(self, name, label, disable_when=None, is_hidden=False, is_label_hidden=False, join_with_next=False,
                 help=None, script_callback=None, script_callback_language=scriptLanguage.Hscript, tags={}):
        self._common(name, label, disable_when, is_hidden, is_label_hidden, join_with_next, help, script_callback,
                     script_callback_language, tags)


class SeparatorParmTemplate(ParmTemplate):
    _type = parmTemplateType.Separator

    def __init__(self, name, is_hidden=False, tags={}):
        self._common(name, "", is_hidden=is_hidden, tags=tags)


class LabelParmTemplate(ParmTemplate):
    _type = parmTemplateType.Label

    def __init__(self, name, label, column_labels=(), is_hidden=False, is_label_hidden=False, join_with_next=False,
                 help=None, tags={}):
        self._common(name, label, is_hidden=is_hidden, is_label_hidden=is_label_hidden, join_with_next=join_with_next,
                     help=help, tags=tags)


class FolderParmTemplate(ParmTemplate):
    _type = parmTemplateType.Folder

    def __init__(self, name, label, parm_templates=(), folder_type=folderType.Tabs, is_hidden=False,
                 ends_tab_group=False, tags={}, conditionals={}, tab_conditionals={}):
        self._common(name, label, is_hidden=is_hidden, tags=tags)
        self._children = [copy.deepcopy(t) for t in parm_templates]
        self._folder_type = folder_type

    def folderType(self):
        return self._folder_type

    def parmTemplates(self):
        return tuple(self._children)

    def setParmTemplates(self, parm_templates):
        self._children = [copy.deepcopy(t) for t in parm_templates]

    def addParmTemplate(self, parm_template):
        self._children.append(copy.deepcopy(parm_template))


def _walk(templates):
    for t in templates:
        yield t
        if isinstance(t, FolderParmTemplate):
            yield from _walk(t._children)


def _container(templates: list, name: str) -> Optional[list]:
    """The list holding the template called name, at any depth."""
    for t in templates:
        if t.name() == name:
            return templates
        if isinstance(t, FolderParmTemplate):
            found = _container(t._children, name)
            if found is not None:
                return found
    return None


class ParmTemplateGroup:
    def __init__(self, parm_templates=()):
        self._templates = [copy.deepcopy(t) for t in parm_templates]

    def entries(self):
        return tuple(self._templates)

    def parmTemplates(self):
        return tuple(self._templates)

    def find(self, name):
        for t in _walk(self._templates):
            if t.name() == name:
                return copy.deepcopy(t)
        return None

    def append(self, parm_template):
        self._templates.append(copy.deepcopy(parm_template))

    def _where(self, name_or_template):
        name = name_or_template if isinstance(name_or_template, str) else name_or_template.name()
        holder = _container(self._templates, name)
        if holder is None:
            raise OperationFailed(f"no parameter {name}")
        return holder, [t.name() for t in holder].index(name)

    def remove(self, name_or_template):
        holder, i = self._where(name_or_template)
        del holder[i]

    def replace(self, name_or_template, parm_template):
        holder, i = self._where(name_or_template)
        holder[i] = copy.deepcopy(parm_template)

    def insertBefore(self, name_or_template, parm_template):
        holder, i = self._where(name_or_template)
        holder.insert(i, copy.deepcopy(parm_template))

    def insertAfter(self, name_or_template, parm_template):
        holder, i = self._where(name_or_template)
        holder.insert(i + 1, copy.deepcopy(parm_template))

    def leaves(self) -> List[ParmTemplate]:
        """Every template that makes parameters (not folders): a test helper."""
        return [t for t in _walk(self._templates) if not isinstance(t, FolderParmTemplate)]


# --- variables and expressions ------------------------------------------------------

variables: Dict[str, str] = {}
_current_frame = [1001.0]
_VAR = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)")
_CHS = re.compile(r'`chs\("([^"]+)"\)`')


def frame() -> float:
    return _current_frame[0]


def setFrame(f: float) -> None:
    _current_frame[0] = float(f)


def getenv(name: str, default_value=None):
    return variables.get(name, os.environ.get(name, default_value))


def _expand(s: str, node=None) -> str:
    def chs(m):
        if node is None:
            raise OperationFailed("chs() outside a node")
        return node.parm(m.group(1)).eval()

    s = _CHS.sub(chs, s)
    if "`" in s:
        raise NotImplementedError(f"the fake hou expands only `chs(...)`: {s}")

    def var(m):
        name = m.group(1) or m.group(2)
        fm = re.fullmatch(r"F(\d*)", name)
        if fm:
            return str(int(frame())).zfill(int(fm.group(1) or 0))
        if name == "OS" and node is not None:
            return node.name()
        if name in variables:
            return variables[name]
        return m.group(0)

    return _VAR.sub(var, s)


def expandString(s: str) -> str:
    return _expand(s)


# --- parameters and nodes ---------------------------------------------------------


class Parm:
    def __init__(self, node, template, index: int, name: str):
        self._node, self._template, self._index, self._name = node, template, index, name
        self._locked = False
        d = template.defaultValue()
        e = template.defaultExpression()
        self._raw: Any = d[index] if index < len(d) else ("" if template.type() == parmTemplateType.String else 0)
        self._expr: Optional[str] = e[index] if index < len(e) and e[index] else None

    def name(self):
        return self._name

    def node(self):
        return self._node

    def parmTemplate(self):
        return copy.deepcopy(self._template)

    def eval(self):
        t = self._template.type()
        if self._expr is not None:
            v = _expand(self._expr, self._node)
            return float(v) if t == parmTemplateType.Float else int(float(v))
        if t == parmTemplateType.String:
            return _expand(str(self._raw), self._node)
        if t == parmTemplateType.Float:
            return float(self._raw)
        if t in (parmTemplateType.Int, parmTemplateType.Toggle):
            return int(self._raw)
        return self._raw

    def evalAsString(self):
        return str(self.eval())

    def unexpandedString(self):
        if self._template.type() != parmTemplateType.String:
            raise OperationFailed(f"{self._name} is not a string parameter")
        return str(self._raw)

    def set(self, value):
        if self._locked:
            raise OperationFailed(f"{self._name} is locked")
        self._expr = None
        t = self._template.type()
        if t == parmTemplateType.String:
            if not isinstance(value, str):
                raise TypeError(f"{self._name} takes a string, not {value!r}")
        elif t == parmTemplateType.Float:
            value = float(value)
        elif t in (parmTemplateType.Int, parmTemplateType.Toggle):
            if isinstance(value, str):
                raise TypeError(f"{self._name} takes a number, not {value!r}")
            value = int(value)
        self._raw = value

    def setExpression(self, expr: str):
        self._expr = expr

    def lock(self, on: bool):
        self._locked = bool(on)

    def isLocked(self):
        return self._locked

    def pressButton(self):
        """Runs the callback, as clicking does."""
        _callback(self)


class ParmTuple:
    def __init__(self, parms: List[Parm]):
        self._parms = parms

    def eval(self):
        return tuple(p.eval() for p in self._parms)

    def set(self, values):
        for p, v in zip(self._parms, values):
            p.set(v)

    def __iter__(self):
        return iter(self._parms)


def _component_names(t: ParmTemplate) -> List[str]:
    n = t.numComponents()
    if n == 1:
        return [t.name()]
    scheme = t.namingScheme()
    if scheme == parmNamingScheme.XYZW:
        return [t.name() + c for c in "xyzw"[:n]]
    return [t.name() + str(i + 1) for i in range(n)]


def _callback(parm: Parm) -> None:
    script = parm._template.scriptCallback()
    if not script:
        return
    if parm._template.scriptCallbackLanguage() != scriptLanguage.Python:
        raise NotImplementedError("the fake runs Python callbacks only")
    kwargs = {"node": parm.node(), "parm": parm, "parm_name": parm.name(), "script_value": parm.evalAsString()}
    exec(compile(script, f"<callback {parm.name()}>", "exec"), {"kwargs": kwargs, "hou": sys.modules["hou"]})


def change(parm: Parm, value) -> None:
    """Sets a parameter as the artist does in the parameter pane: the value,
    then its callback."""
    parm.set(value)
    _callback(parm)


def run_menu(parm: Parm) -> List[str]:
    """Runs a parameter's menu script as Houdini does: as a function body."""
    t = parm._template
    script = t.itemGeneratorScript()
    if not script:
        return [x for pair in zip(t.menuItems(), t.menuLabels()) for x in pair]
    body = "def _menu(kwargs):\n" + "\n".join("    " + ln for ln in script.splitlines())
    scope = {"hou": sys.modules["hou"]}
    exec(compile(body, f"<menu {parm.name()}>", "exec"), scope)
    return scope["_menu"]({"node": parm.node(), "parm": parm})


class NodeTypeCategory:
    def __init__(self, name: str):
        self._name = name

    def name(self):
        return self._name


_categories = {n: NodeTypeCategory(n) for n in ("Lop", "Driver", "Sop", "Object", "Manager")}


def lopNodeTypeCategory():
    return _categories["Lop"]


def ropNodeTypeCategory():
    return _categories["Driver"]


class NodeType:
    def __init__(self, name: str, category: str):
        self._name, self._category = name, _categories[category]

    def name(self):
        return self._name

    def category(self):
        return self._category


_registered_types: Dict[tuple, NodeType] = {}


def nodeType(category, name):
    return _registered_types.get((category.name(), name))


_nodes: Dict[str, "Node"] = {}
_ids = [0]
_selected: List["Node"] = []


def _child_category(parent_path: str, type_name: str) -> str:
    if parent_path == "/out":
        return "Driver"
    if parent_path == "/stage":
        return "Lop"
    if parent_path == "/obj":
        return "Object"
    parent = _nodes.get(parent_path)
    return parent.type().category().name() if parent else "Manager"


class Node:
    def __init__(self, path: str, type_name: str, category: str, templates=(), stage=None):
        self._path = path
        self._type = NodeType(type_name, category)
        _ids[0] += 1
        self._id = _ids[0]
        self._inputs: List[Optional[Node]] = []
        self._bypassed = False
        self._group = ParmTemplateGroup(templates)
        self._parms: Dict[str, Parm] = {}
        self._tuples: Dict[str, ParmTuple] = {}
        self._sync()
        self._stage = stage
        self._errors: List[str] = []
        self._selected = False
        _nodes[path] = self

    # identity
    def path(self):
        return self._path

    def name(self):
        return self._path.rsplit("/", 1)[-1]

    def type(self):
        return self._type

    def sessionId(self):
        return self._id

    def parent(self):
        return _nodes.get(os.path.dirname(self._path)) or _Network(os.path.dirname(self._path))

    def node(self, rel: str):
        path = rel if rel.startswith("/") else os.path.normpath(os.path.join(self._path, rel))
        return _nodes.get(path)

    # wiring
    def inputs(self):
        return tuple(self._inputs)

    def setInput(self, index: int, node):
        while len(self._inputs) <= index:
            self._inputs.append(None)
        self._inputs[index] = node

    def isBypassed(self):
        return self._bypassed

    def bypass(self, on: bool):
        self._bypassed = bool(on)

    # parameters
    def _sync(self):
        old = self._parms
        self._parms, self._tuples = {}, {}
        for t in self._group.leaves():
            if t.type() in (parmTemplateType.Separator, parmTemplateType.Label):
                continue
            comps = []
            for i, n in enumerate(_component_names(t)):
                p = old.get(n)
                if p is not None and p._template.type() == t.type():
                    p._template = t
                else:
                    p = Parm(self, t, i, n)
                self._parms[n] = p
                comps.append(p)
            self._tuples[t.name()] = ParmTuple(comps)

    def parm(self, name: str):
        return self._parms.get(name)

    def parms(self):
        return tuple(self._parms.values())

    def parmTuple(self, name: str):
        return self._tuples.get(name)

    def parmTemplateGroup(self):
        return copy.deepcopy(self._group)

    def setParmTemplateGroup(self, group):
        self._group = copy.deepcopy(group)
        self._sync()

    def addSpareParmTuple(self, template):
        self._group.append(template)
        self._sync()

    # LOPs
    def stage(self):
        if self._type.category().name() != "Lop":
            raise OperationFailed("not a LOP")
        if self._stage is not None:
            return self._stage
        for i in self._inputs:
            if i is not None:
                return i.stage()
        return None

    # ROPs
    def render(self, frame_range=(), res=(), output_file=None, output_format=None, to_flipbook=False, quality=2,
               ignore_inputs=False, method=None, ignore_bypass_flags=False, ignore_lock_flags=False, verbose=False,
               output_progress=False):
        if self._type.name() != "usd":
            raise NotImplementedError("the fake renders only the USD ROP")
        src = _nodes[self.parm("loppath").eval()]
        out = self.parm("lopoutput").eval()
        stage = src.stage()
        rendered.append({"rop": self._path, "source": src.path(), "output": out, "frames": tuple(frame_range),
                         "trange": self.parm("trange").eval()})
        with open(out, "w", encoding="utf-8") as f:
            json.dump(stage.dependencies(out), f)

    def errors(self):
        return tuple(self._errors)

    # the network
    def createNode(self, type_name: str, name: Optional[str] = None):
        return _Network(self._path).createNode(type_name, name)

    def children(self):
        return tuple(n for p, n in _nodes.items() if os.path.dirname(p) == self._path)

    def destroy(self):
        _nodes.pop(self._path, None)

    def moveToGoodPosition(self):
        pass

    def setSelected(self, on, clear_all_selected=False):
        if clear_all_selected:
            _selected.clear()
        if on:
            _selected.append(self)

    def __repr__(self):
        return f"<Node {self._path}>"


# Templates the node types used here are made with, as Houdini's own.
def _rop_parms(output: Optional[str] = None) -> list:
    out = [IntParmTemplate("trange", "Valid Frame Range", 1, default_value=(0,)),
           FloatParmTemplate("f", "Start/End/Inc", 3, default_value=(1, 240, 1), naming_scheme=parmNamingScheme.Base1)]
    if output:
        out.append(StringParmTemplate(output, "Output", 1, default_value=("",), string_type=stringParmType.FileReference,
                                      tags={"filechooser_mode": "write"}))
    return out


NODE_PARMS = {
    ("Driver", "usd"): lambda: _rop_parms("lopoutput") + [StringParmTemplate("loppath", "LOP Path", 1, default_value=("",),
                                                                              string_type=stringParmType.NodeReference)],
    ("Driver", "ifd"): lambda: _rop_parms("vm_picture"),
    ("Driver", "karma"): lambda: _rop_parms("picture"),
    ("Driver", "geometry"): lambda: _rop_parms("sopoutput"),
    ("Driver", "opengl"): lambda: _rop_parms("picture"),
    ("Driver", "alembic"): lambda: _rop_parms("filename"),
    ("Driver", "fetch"): lambda: [StringParmTemplate("source", "Source", 1, default_value=("",),
                                                     string_type=stringParmType.NodeReference)],
    ("Driver", "studio_cache"): lambda: _rop_parms() + [
        StringParmTemplate("cachefile", "Cache File", 1, default_value=("",), string_type=stringParmType.FileReference,
                           tags={"filechooser_mode": "write"})],
    ("Sop", "file"): lambda: [StringParmTemplate("file", "Geometry File", 1, default_value=("",),
                                                 string_type=stringParmType.FileReference,
                                                 tags={"filechooser_mode": "read"})],
    ("Sop", "alembic"): lambda: [StringParmTemplate("fileName", "File Name", 1, default_value=("",),
                                                    string_type=stringParmType.FileReference)],
    ("Object", "geo"): lambda: [],
}


class _Network:
    """A network that is not itself a node in the fake (/, /out, /stage, /obj)."""

    def __init__(self, path: str):
        self._path = path

    def path(self):
        return self._path

    def createNode(self, type_name: str, name: Optional[str] = None):
        name = name or f"{type_name}1"
        path = f"{self._path.rstrip('/')}/{name}"
        n = 2
        while path in _nodes:
            path = f"{self._path.rstrip('/')}/{name}{n}"
            n += 1
        cat = _child_category(self._path, type_name)
        maker = NODE_PARMS.get((cat, type_name))
        return Node(path, type_name, cat, maker() if maker else [])


_networks = ("/", "/out", "/stage", "/obj")


def node(path: str):
    if path in _nodes:
        return _nodes[path]
    if path in _networks:
        return _Network(path)
    return None


def selectedNodes():
    return tuple(_selected)


# --- the hip file -------------------------------------------------------------------


class _HipFile:
    def __init__(self):
        self._path = ""
        self.modified = False
        self.saves = 0

    def path(self):
        return self._path or os.path.join(os.getcwd(), "untitled.hip")

    def name(self):
        return os.path.basename(self.path())

    def isNewFile(self):
        return not self._path

    def hasUnsavedChanges(self):
        return self.modified

    def save(self, file_name=None, save_to_recent_files=True):
        path = file_name or self.path()
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            f.write("HIP fake\n")
        self._path, self.modified = path, False
        self.saves += 1
        variables.update(HIP=os.path.dirname(path), HIPFILE=path,
                         HIPNAME=os.path.splitext(os.path.basename(path))[0])


hipFile = _HipFile()
extra_references: List[tuple] = []


def fileReferences(project_dir_variable="HIP", include_all_refs=True):
    """(parm, path) for every file parameter with a value, plus references
    without a parameter (HDA files)."""
    out = []
    for n in list(_nodes.values()):
        for p in n.parms():
            t = p._template
            if t.type() == parmTemplateType.String and t.stringType() == stringParmType.FileReference and p.eval():
                out.append((p, p.eval()))
    return tuple(out) + tuple(extra_references)


# --- the UI and long operations -----------------------------------------------------


class _UI:
    def __init__(self):
        self.available = True
        self.answers: List[int] = []
        self.files: List[str] = []
        self.messages: List[dict] = []
        self.status: List[str] = []

    def displayMessage(self, text, buttons=("OK",), severity=severityType.Message, default_choice=0,
                       close_choice=None, help=None, title=None, details=None, details_label=None,
                       details_expanded=False):
        self.messages.append({"text": text, "buttons": tuple(buttons), "details": details, "title": title})
        return self.answers.pop(0) if self.answers else default_choice

    def selectFile(self, start_directory=None, title=None, collapse_sequences=False, file_type=fileType.Any,
                   pattern=None, default_value=None, multiple_select=False, image_chooser=None,
                   chooser_mode=fileChooserMode.ReadAndWrite, width=0, height=0):
        return self.files.pop(0) if self.files else ""

    def setStatusMessage(self, message, severity=severityType.Message):
        self.status.append(message)


ui = _UI()


def isUIAvailable():
    return ui.available


class InterruptableOperation:
    """Records progress; raises OperationInterrupted from the first update
    once ``interrupt`` is set, as pressing Cancel does."""

    interrupt = False
    updates: List[tuple] = []

    def __init__(self, operation_name, long_operation_name=None, open_interrupt_dialog=False):
        self.name = operation_name

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def updateProgress(self, percentage=-1.0):
        self.updateLongProgress(percentage)

    def updateLongProgress(self, percentage=-1.0, long_op_status=None):
        InterruptableOperation.updates.append((percentage, long_op_status))
        if InterruptableOperation.interrupt:
            raise OperationInterrupted("cancelled")


rendered: List[dict] = []


def reset(hip: str = "", **env: str) -> None:
    """A new session: no nodes, no hip file, the given variables."""
    _nodes.clear()
    _selected.clear()
    _registered_types.clear()
    extra_references.clear()
    rendered.clear()
    variables.clear()
    variables.update(FSTART="1001", FEND="1048", HFS="/opt/hfs21.0", JOB="/show")
    variables.update(env)
    setFrame(1001)
    hipFile._path, hipFile.modified, hipFile.saves = "", False, 0
    if hip:
        hipFile._path = hip
        variables.update(HIP=os.path.dirname(hip), HIPFILE=hip, HIPNAME=os.path.splitext(os.path.basename(hip))[0])
    else:
        variables["HIP"] = os.getcwd()
    global ui
    ui.__init__()
    InterruptableOperation.interrupt = False
    InterruptableOperation.updates = []


# --- pxr ----------------------------------------------------------------------------


class _SdfPath:
    def __init__(self, s: str):
        self.pathString = s

    def __str__(self):
        return self.pathString


class _AssetPath:
    def __init__(self, path: str):
        self.path = path


class _Layer:
    def __init__(self, path: str, anonymous: bool = False):
        self.realPath = "" if anonymous else path
        self.identifier = path
        self.anonymous = anonymous


class Prim:
    def __init__(self, stage: "Stage", path: str, type_name: str):
        self._stage, self._path, self._type = stage, path, type_name

    def IsValid(self):
        return self._type is not None

    def __bool__(self):
        return self.IsValid()

    def GetPath(self):
        return _SdfPath(self._path)

    def GetName(self):
        return self._path.rsplit("/", 1)[-1]

    def GetTypeName(self):
        return self._type or ""

    def IsA(self, schema):
        return self._type == schema.typeName


class Stage:
    """A stage: prims by path and type, metadata, and the files the stage
    would read once written (layers, assets, unresolved)."""

    def __init__(self, prims: Dict[str, str], metadata: Optional[dict] = None, layers=(), assets=(), unresolved=()):
        self._prims = dict(prims)
        self._meta = dict(metadata or {})
        self._layers, self._assets, self._unresolved = list(layers), list(assets), list(unresolved)

    def GetPrimAtPath(self, path):
        path = str(path)
        return Prim(self, path, self._prims.get(path))

    def GetMetadata(self, key):
        return self._meta.get(key, "")

    def _under(self, path: str):
        return [Prim(self, p, t) for p, t in sorted(self._prims.items()) if p == path or p.startswith(path + "/")]

    def dependencies(self, root: str) -> dict:
        return {"layers": [root] + self._layers, "assets": self._assets, "unresolved": self._unresolved}


def _prim_range(prim):
    return iter(prim._stage._under(prim._path))


def _compute_all_dependencies(asset_path, processingFunc=None):
    path = asset_path.path if isinstance(asset_path, _AssetPath) else str(asset_path)
    with open(path, encoding="utf-8") as f:
        d = json.load(f)
    layers = [_Layer(p, anonymous=p.startswith("anon:")) for p in d["layers"]]
    return layers, list(d["assets"]), list(d["unresolved"])


def _pxr_modules() -> Dict[str, types.ModuleType]:
    pxr = types.ModuleType("pxr")
    usd = types.ModuleType("pxr.Usd")
    usd.Stage = Stage
    usd.Prim = Prim
    usd.PrimRange = _prim_range
    render = types.ModuleType("pxr.UsdRender")
    render.Settings = type("Settings", (), {"typeName": "RenderSettings"})
    render.Product = type("Product", (), {"typeName": "RenderProduct"})
    utils = types.ModuleType("pxr.UsdUtils")
    utils.ComputeAllDependencies = _compute_all_dependencies
    sdf = types.ModuleType("pxr.Sdf")
    sdf.AssetPath = _AssetPath
    sdf.Path = _SdfPath
    sdf.Layer = _Layer
    pxr.Usd, pxr.UsdRender, pxr.UsdUtils, pxr.Sdf = usd, render, utils, sdf
    return {"pxr": pxr, "pxr.Usd": usd, "pxr.UsdRender": render, "pxr.UsdUtils": utils, "pxr.Sdf": sdf}


def install() -> None:
    sys.modules["hou"] = sys.modules[__name__]
    sys.modules.update(_pxr_modules())

