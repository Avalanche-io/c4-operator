"""The submit window, drawn in Maya's own controls.

``maya.cmds`` controls rather than Qt, so it is the same from Maya 2024 to
2026 whatever PySide each ships, and tests drive it with a fake ``maya``.
Top to bottom: Tool, Project, Shot, Needed by, Frames, one checkbox per
render layer, Size, Note, the tool's own parameters, then Check, Submit,
Show file and Save file, and the summary the check returns.

"Needed by" is the only urgency the artist sets. Check and Submit hash,
upload and talk to the farm on a worker thread; Maya's progress window
shows how far they are and cancels them, driven from the main thread,
where their answers come back too. Submit is available only after a check
passed and nothing has changed since.
"""

import os
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Tuple

from maya import cmds
from maya import utils as mayautils

from c4submit import summary as summarylib
from c4submit.collect import Cancelled
from c4submit.farm import Farm, FarmError, NotOnFarm
from c4submit.form import Field, Form
from c4submit.submitter import Checked, Draft, Submitted, Submitter, default_filename, prepare, save
from c4submit.tool import Tool

from c4_maya.host import MayaHost

TITLE = "Submit to C4"
WINDOW = "c4SubmitWindow"
SUGGESTED = "When the farm suggests"
TYPED = "At a time I type"
STANDARD = "Standard"
EACH_RANGE = "each render layer's own range"
EACH_LAYER = "each ticked render layer"
EACH_CAMERA = "each render layer's own camera"
STALE = "Changed since the check: Check again before you submit."
LOCAL_TOOLS = ("Setting C4_TOOL_PATH to the studio's tool folders lets this window draw the form and save "
               "the submission as a file, to submit when the farm takes it.")
PROGRESS_WORDS = {"hash": "Reading files", "upload": "Uploading"}
#: How often a worker asks the main thread whether the artist cancelled.
CANCEL_POLL = 0.25
#: Scene events after which the window reads the scene again.
SCENE_EVENTS = ("SceneSaved", "SceneOpened", "NewSceneOpened")

#: The value flag of each control the window makes.
VALUE_FLAG = {"textFieldGrp": "text", "textFieldButtonGrp": "text", "textField": "text", "scrollField": "text",
              "optionMenuGrp": "value", "checkBoxGrp": "value1", "checkBox": "value",
              "intFieldGrp": "value1", "floatFieldGrp": "value1"}
#: fileDialog2's fileMode for each file control.
FILE_MODES = {"CHOOSE_INPUT_FILE": 1, "CHOOSE_OUTPUT_FILE": 0, "CHOOSE_DIRECTORY": 3}
#: Controls whose typing is committed on Enter or on leaving the field.
TEXT_KINDS = ("textFieldGrp", "textFieldButtonGrp", "textField", "scrollField")


# --- the form, as widget specs ------------------------------------------------------

@dataclass
class WidgetSpec:
    """A control to make, without making it: what a field draws as, so the
    mapping is tested without Maya."""
    kind: str                      # the maya.cmds command: textFieldGrp, optionMenuGrp, frameLayout, ...
    name: str
    label: str = ""
    value: Any = None
    items: List[str] = field(default_factory=list)   # an option menu's choices
    tooltip: str = ""
    param: str = ""                # the parameter it edits; '' for groups
    role: str = "value"            # value, linked, begin, end
    collapsed: bool = False        # a group that starts folded
    file_mode: Optional[int] = None
    file_filter: str = ""


def control_name(param: str) -> str:
    return "c4p_" + param


def linked_name(param: str) -> str:
    return "c4p_" + param + "_linked"


def _text(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, (list, tuple)):
        return ", ".join(str(v) for v in value)
    if isinstance(value, bool):
        return "on" if value else "off"
    return str(value)


def widget_kind(f: Field) -> str:
    """The Maya control a field's control draws as."""
    c = f.control
    if c == "MULTILINE_EDIT":
        return "scrollField"
    if c == "DROPDOWN_LIST" and f.param.allowed:
        return "optionMenuGrp"
    if c == "CHECK_BOX":
        return "checkBoxGrp"
    if c == "SPIN_BOX":
        return "floatFieldGrp" if f.param.type == "FLOAT" else "intFieldGrp"
    if c.startswith("CHOOSE_"):
        return "textFieldButtonGrp"
    return "textFieldGrp"


def widget_value(f: Field, value: Any) -> Any:
    """A field's value as its control holds it."""
    kind = widget_kind(f)
    if kind == "checkBoxGrp":
        try:
            return bool(f.param.coerce(value)) if value is not None else False
        except ValueError:
            return False
    if kind in ("intFieldGrp", "floatFieldGrp"):
        try:
            v = f.param.coerce(value) if value not in (None, "") else 0
        except ValueError:
            v = 0
        return int(v) if kind == "intFieldGrp" else float(v)
    if kind == "optionMenuGrp":
        items = [str(a) for a in f.param.allowed or []]
        s = _text(value)
        return s if s in items else (items[0] if items else "")
    return _text(value)


def file_filter(f: Field) -> str:
    """fileDialog2's filter for a file field: the tool's file filters, then
    every file."""
    out = []
    for flt in f.param.ui.get("fileFilters") or []:
        pats = flt.get("patterns") or []
        if pats:
            out.append(f"{flt.get('label') or 'Files'} ({' '.join(pats)})")
    return ";;".join(out + ["All files (*.*)"])


def field_specs(f: Field, shown: Any = None) -> List[WidgetSpec]:
    """The controls one field draws: its own, and a "linked" checkbox beside
    it when its value comes from the scene. ``shown`` is what a linked field
    shows instead of its value (for one value per job)."""
    kind = widget_kind(f)
    tip = f.param.description
    if f.param.min is not None or f.param.max is not None:
        lo = "" if f.param.min is None else f"at least {f.param.min}"
        hi = "" if f.param.max is None else f"at most {f.param.max}"
        tip = (tip + " " if tip else "") + "(" + " and ".join(x for x in (lo, hi) if x) + ")"
    if f.derivable:
        tip = (tip + "\n" if tip else "") + "Linked: follows the scene until you type into it."
    text_kinds = ("textFieldGrp", "textFieldButtonGrp")
    value = shown if shown is not None and kind in text_kinds else widget_value(f, f.value)
    spec = WidgetSpec(kind, control_name(f.name), f.label, value, tooltip=tip, param=f.name,
                      items=[str(a) for a in f.param.allowed or []] if kind == "optionMenuGrp" else [])
    if kind == "textFieldButtonGrp":
        spec.file_mode = FILE_MODES.get(f.control, 1)
        spec.file_filter = file_filter(f) if f.control != "CHOOSE_DIRECTORY" else ""
    out = [spec]
    if f.derivable:
        out.append(WidgetSpec("checkBox", linked_name(f.name), "linked", bool(f.linked), param=f.name, role="linked",
                              tooltip="On: follow the scene. Typing into the field turns this off."))
    return out


def form_specs(form: Form, shown: Optional[Callable[[Field], Any]] = None) -> List[WidgetSpec]:
    """The controls a tool's form draws, in its order: each ``groupLabel``
    as an open frame, ``show: advanced`` fields in a folded one."""
    out: List[WidgetSpec] = []
    for i, (group, fields) in enumerate(form.groups()):
        plain = [f for f in fields if not f.advanced]
        folded = [f for f in fields if f.advanced]
        if plain:
            if group:
                out.append(WidgetSpec("frameLayout", f"c4g_{i}", group, role="begin"))
            for f in plain:
                out += field_specs(f, shown(f) if shown else None)
            if group:
                out.append(WidgetSpec("frameLayout", f"c4g_{i}_end", role="end"))
        if folded:
            label = group if group and not plain else (f"{group}: advanced" if group else "Advanced")
            out.append(WidgetSpec("frameLayout", f"c4g_{i}_folded", label, role="begin", collapsed=True))
            for f in folded:
                out += field_specs(f, shown(f) if shown else None)
            out.append(WidgetSpec("frameLayout", f"c4g_{i}_folded_end", role="end"))
    return out


def set_items(menu: str, items: List[str]) -> None:
    """Replaces an optionMenuGrp's choices."""
    for item in cmds.optionMenuGrp(menu, query=True, itemListLong=True) or []:
        cmds.deleteUI(item)
    for label in items:
        cmds.menuItem(label=label, parent=menu + "|OptionMenu")


def make_widget(spec: WidgetSpec, changed: Callable[[str], None],
                browse: Optional[Callable[[WidgetSpec], None]] = None) -> str:
    """Makes the control a spec describes under the current layout and
    returns its path. ``changed(name)`` runs when the artist edits it."""
    def cb(*_):
        changed(spec.name)

    k, v, tip = spec.kind, spec.value, spec.tooltip
    if k == "frameLayout":
        return cmds.frameLayout(spec.name, label=spec.label, collapsable=True, collapse=spec.collapsed,
                                marginWidth=6, marginHeight=4)
    if k == "optionMenuGrp":
        path = cmds.optionMenuGrp(spec.name, label=spec.label, annotation=tip, changeCommand=cb)
        set_items(path, spec.items)
        if v in spec.items:
            cmds.optionMenuGrp(path, edit=True, value=v)
        return path
    if k == "checkBoxGrp":
        return cmds.checkBoxGrp(spec.name, label=spec.label, numberOfCheckBoxes=1, value1=bool(v),
                                annotation=tip, changeCommand=cb)
    if k == "checkBox":
        return cmds.checkBox(spec.name, label=spec.label, value=bool(v), annotation=tip, changeCommand=cb)
    if k == "intFieldGrp":
        return cmds.intFieldGrp(spec.name, label=spec.label, numberOfFields=1, value1=int(v or 0),
                                annotation=tip, changeCommand=cb)
    if k == "floatFieldGrp":
        return cmds.floatFieldGrp(spec.name, label=spec.label, numberOfFields=1, value1=float(v or 0),
                                  precision=3, annotation=tip, changeCommand=cb)
    if k == "scrollField":
        cmds.text(spec.name + "_label", label=spec.label, align="left")
        return cmds.scrollField(spec.name, text=_text(v), wordWrap=True, height=60, annotation=tip, changeCommand=cb)
    if k == "textFieldButtonGrp":
        return cmds.textFieldButtonGrp(spec.name, label=spec.label, text=_text(v), buttonLabel="...",
                                       annotation=tip, adjustableColumn=2, changeCommand=cb,
                                       buttonCommand=lambda *_: browse(spec) if browse else None)
    return cmds.textFieldGrp(spec.name, label=spec.label, text=_text(v), annotation=tip, adjustableColumn=2,
                             changeCommand=cb)


def widget_read(f: Field, kind: str, path: str) -> Any:
    """What the artist typed into a field's control, as a value for the form."""
    v = getattr(cmds, kind)(path, query=True, **{VALUE_FLAG[kind]: True})
    if kind in ("checkBoxGrp", "checkBox"):
        return bool(v)
    if kind == "intFieldGrp":
        return int(v)
    if kind == "floatFieldGrp":
        return float(v)
    return v


# --- the window ---------------------------------------------------------------------------

class SubmitWindow:
    """The window. One is open at a time; opening another closes it."""

    def __init__(self, host: Optional[MayaHost] = None, farm: Optional[Farm] = None):
        self.host = host or MayaHost()
        self._farm = farm
        self.tools: List[Tool] = []
        self.project_info: Optional[dict] = None
        self.draft: Optional[Draft] = None
        self.checked: Optional[Checked] = None
        self.submitted: Optional[Submitted] = None
        self._checked_sig = ""
        self._due_values: List[str] = ["suggested", ""]
        self._summary = ""
        self._showing_file = False
        self._busy = False
        self._preparing = False
        self._updating = 0
        self._threads: List[threading.Thread] = []
        self._lock = threading.Lock()
        self._cancel = threading.Event()
        self._last_poll = 0.0
        self._progress_open = False
        self._latest: Tuple[str, int] = ("", 0)
        self._pump_pending = False
        self._layers: Dict[str, str] = {}      # control name -> render layer
        self._known: Dict[str, Any] = {}        # text control -> its text as the window last saw it
        self.ui: Dict[str, str] = {}            # control name -> its path
        self.kinds: Dict[str, str] = {}         # control name -> its command
        self.dynamic = ""
        self._build()

    # --- the farm -------------------------------------------------------------

    @property
    def farm(self) -> Farm:
        if self._farm is None:
            self._farm = Farm(client="c4submit-maya")
        return self._farm

    # --- controls ----------------------------------------------------------------

    def _cb(self, name: str) -> Callable:
        return lambda *_: self.changed(name)

    def _add(self, name: str, kind: str, path: str) -> str:
        self.ui[name], self.kinds[name] = path, kind
        if kind in TEXT_KINDS:
            self._known[name] = self.value(name)
        return path

    def _edit(self, name: str, **flags) -> None:
        if name in self.ui:
            getattr(cmds, self.kinds[name])(self.ui[name], edit=True, **flags)

    def _set(self, name: str, value: Any) -> None:
        kind = self.kinds.get(name)
        if kind is None:
            return
        self._updating += 1
        try:
            if kind == "optionMenuGrp":
                items = [cmds.menuItem(i, query=True, label=True)
                         for i in cmds.optionMenuGrp(self.ui[name], query=True, itemListLong=True) or []]
                if value not in items:
                    return
            self._edit(name, **{VALUE_FLAG.get(kind, "label"): value})
            if kind in TEXT_KINDS:
                self._known[name] = self.value(name)
        finally:
            self._updating -= 1

    def _pull(self) -> None:
        """Takes in what the artist typed but has not committed yet. A text
        field tells the window only on Enter or when it loses focus, so a
        Shot typed and then Check pressed would otherwise be missed."""
        typed = [(n, self.value(n)) for n, k in self.kinds.items()
                 if k in TEXT_KINDS and n not in ("c4_project", "c4_summary") and self.value(n) != self._known.get(n)]
        for name, text in typed:
            if name not in self.ui:
                continue
            if self.value(name) != text:   # a change taken in before it re-showed a linked value
                self._edit(name, **{VALUE_FLAG[self.kinds[name]]: text})
            self.changed(name)

    def value(self, name: str) -> Any:
        kind = self.kinds[name]
        return getattr(cmds, kind)(self.ui[name], query=True, **{VALUE_FLAG.get(kind, "label"): True})

    def _text_of(self, name: str) -> str:
        return (self.value(name) or "").strip()

    def _build(self) -> None:
        if cmds.window(WINDOW, exists=True):
            cmds.deleteUI(WINDOW)
        self.window = cmds.window(WINDOW, title=TITLE, widthHeight=(560, 860), sizeable=True)
        cmds.scrollLayout("c4_scroll", childResizable=True)
        self.main = cmds.columnLayout("c4_main", adjustableColumn=True, rowSpacing=4)
        self._add("c4_tool", "optionMenuGrp", cmds.optionMenuGrp("c4_tool", label="Tool", changeCommand=self._cb("c4_tool"),
                                                                annotation="The farm's tools that render Maya scenes."))
        self._add("c4_project", "textFieldGrp", cmds.textFieldGrp("c4_project", label="Project", text=self.host.project(),
                                                                  adjustableColumn=2, changeCommand=self._cb("c4_project")))
        self._add("c4_shot", "textFieldGrp", cmds.textFieldGrp("c4_shot", label="Shot", text=self.host.shot(),
                                                               adjustableColumn=2, changeCommand=self._cb("c4_shot")))
        cmds.rowLayout("c4_due_row", numberOfColumns=2, adjustableColumn=2)
        due = self._add("c4_due", "optionMenuGrp", cmds.optionMenuGrp("c4_due", label="Needed by",
                                                                      changeCommand=self._cb("c4_due")))
        set_items(due, [SUGGESTED, TYPED])
        self._add("c4_due_time", "textField", cmds.textField(
            "c4_due_time", text="", enable=False, changeCommand=self._cb("c4_due_time"),
            annotation='A time like "fri 18:00", "18:00" or "+3h", or a date like 2026-10-23.'))
        cmds.setParent("..")
        cmds.rowLayout("c4_frames_row", numberOfColumns=2, adjustableColumn=1)
        self._add("c4_frames", "textFieldGrp", cmds.textFieldGrp(
            "c4_frames", label="Frames", text=self.host.frames(), adjustableColumn=2,
            changeCommand=self._cb("c4_frames"),
            annotation="Frames to render, like 1001-1048 or 1001-1100:2. Linked, each render layer renders its own "
                       "range (or the scene's); typing here sets one range for all of them."))
        self._add("c4_frames_linked", "checkBox", cmds.checkBox(
            "c4_frames_linked", label="linked", value=True, changeCommand=self._cb("c4_frames_linked"),
            annotation="On: each render layer's own range. Typing frames turns this off."))
        cmds.setParent("..")
        self._rebuild()
        self._set_summary("Reading the farm's tools...")
        for event in SCENE_EVENTS:
            cmds.scriptJob(event=[event, self._scene_event], parent=self.window)

    def _set_summary(self, text: str) -> None:
        self._summary = text
        self._showing_file = False
        self._show_summary()

    def _show_summary(self) -> None:
        stale = self.checked is not None and self.submitted is None and self.signature() != self._checked_sig
        self._set("c4_summary", (STALE + "\n\n" if stale else "") + self._summary)

    def _shown(self, f: Field) -> Any:
        """What a linked field shows when its value is one per job."""
        d = self.draft
        if d is None or not f.linked:
            return None
        if f.param.source == "scene.renderables" and len(d.picked()) > 1:
            return EACH_LAYER
        if f.param.source == "scene.camera" and d.per_renderable:
            if len({d._derive(f.param.source, f.name, r) for r in d.picked()}) > 1:
                return EACH_CAMERA
        if f.param.source in ("scene.frames", "shot.frames") and d.per_renderable:
            if len({d.job_frames(r) for r in d.picked()}) > 1:
                return EACH_RANGE
        return None

    def _browse(self, spec: WidgetSpec) -> None:
        start = self._text_of(spec.name) if spec.name in self.ui else ""
        picked = cmds.fileDialog2(fileMode=spec.file_mode if spec.file_mode is not None else 1,
                                  fileFilter=spec.file_filter or None, caption=spec.label,
                                  startingDirectory=os.path.dirname(start) if start else None, dialogStyle=2)
        if picked:
            self._set(spec.name, picked[0])
            self.changed(spec.name)

    def _rebuild(self) -> None:
        """Re-draws everything below Frames: the render layers, Size, Note,
        the tool's parameters, the buttons and the summary."""
        if self.dynamic and cmds.columnLayout(self.dynamic, exists=True):
            cmds.deleteUI(self.dynamic)
        for name in [n for n in self.ui if n not in ("c4_tool", "c4_project", "c4_shot", "c4_due", "c4_due_time",
                                                     "c4_frames", "c4_frames_linked")]:
            self.ui.pop(name)
            self.kinds.pop(name)
            self._known.pop(name, None)
        self._layers = {}
        d = self.draft
        cmds.setParent(self.main)
        self.dynamic = cmds.columnLayout("c4_dynamic", adjustableColumn=True, rowSpacing=4)
        self._updating += 1
        try:
            takes_layer = d is not None and any(p.source == "scene.renderables" for p in d.tool.params)
            if d is not None and d.per_renderable:
                self._add("c4_layers", "frameLayout", cmds.frameLayout("c4_layers", label="Render layers",
                                                                        collapsable=False, marginWidth=6))
                for i, r in enumerate(d.renderables):
                    name = f"c4_layer_{i}"
                    label = f"{r.label}  {r.frames or 'scene range'}" + (f"  ({r.note})" if r.note else "")
                    tip = "Renders " + ", ".join(r.outputs) if r.outputs else "Renders this render layer"
                    self._add(name, "checkBox", cmds.checkBox(name, label=label, value=r.name in d.ticked,
                                                              annotation=tip, changeCommand=self._cb(name)))
                    self._layers[name] = r.name
                cmds.setParent("..")
            elif takes_layer:
                self._add("c4_layers", "text", cmds.text("c4_layers", align="left",
                                                         label="Render layers: none in this scene is renderable"))
            if d is not None and d.tool.sizes:
                size = self._add("c4_size", "optionMenuGrp", cmds.optionMenuGrp("c4_size", label="Size",
                                                                               changeCommand=self._cb("c4_size")))
                set_items(size, [STANDARD] + list(d.tool.sizes))
                if d.size:
                    cmds.optionMenuGrp(size, edit=True, value=d.size)
            cmds.text("c4_note_label", label="Note", align="left")
            self._add("c4_note", "scrollField", cmds.scrollField(
                "c4_note", text=d.note if d else "", wordWrap=True, height=48, changeCommand=self._cb("c4_note"),
                annotation="Shown with the jobs on the farm, for whoever looks at them."))
            if d is not None:
                self._draw_form(form_specs(d.form, self._shown))
            cmds.rowLayout("c4_buttons", numberOfColumns=4)
            for name, label, tip in (
                    ("c4_check", "Check", "Reads every file the scene needs and asks the farm when it would be ready. "
                                          "Nothing is sent to render."),
                    ("c4_submit", "Submit", "Sends the files the farm lacks and starts the jobs. Check first."),
                    ("c4_show_file", "Show file", "Shows the submission as a file, in place of the summary."),
                    ("c4_save_file", "Save file...", "Saves the submission as a file, to submit later with "
                                                     "c4 farm submit.")):
                self._add(name, "button", cmds.button(name, label=label, annotation=tip, width=110,
                                                      command=self._cb(name)))
            cmds.setParent("..")
            self._add("c4_summary", "scrollField", cmds.scrollField("c4_summary", text=self._summary, editable=False,
                                                                    wordWrap=False, height=260, font="fixedWidthFont"))
        finally:
            self._updating -= 1
            cmds.setParent(self.main)
        self._sync()

    def _draw_form(self, specs: List[WidgetSpec]) -> None:
        i = 0
        while i < len(specs):
            spec = specs[i]
            if spec.role == "begin":
                make_widget(spec, self.changed)
            elif spec.role == "end":
                cmds.setParent("..")
            else:
                beside = i + 1 < len(specs) and specs[i + 1].role == "linked"
                if beside:
                    cmds.rowLayout(spec.name + "_row", numberOfColumns=2, adjustableColumn=1)
                self._add(spec.name, spec.kind, make_widget(spec, self.changed, self._browse))
                if beside:
                    i += 1
                    self._add(specs[i].name, specs[i].kind, make_widget(specs[i], self.changed))
                    cmds.setParent("..")
            i += 1

    def _sync(self) -> None:
        """Brings every shown value and button in line with the draft."""
        d = self.draft
        self._updating += 1
        try:
            if d is not None:
                cmds.rowLayout(self.ui["c4_frames"].rsplit("|", 1)[0], edit=True, visible=d.tool.takes_frames)
                self._edit("c4_frames_linked", value=d.frames is None)
                if d.frames is None:
                    if d.per_renderable:
                        ranges = {d.job_frames(r) for r in d.picked()} or {d.scene_frames}
                    else:
                        ranges = {d.job_frames(None)}
                    self._set("c4_frames", ranges.pop() if len(ranges) == 1 else EACH_RANGE)
                for f in d.form.drawn():
                    name, lname = control_name(f.name), linked_name(f.name)
                    if lname in self.ui:
                        self._edit(lname, value=bool(f.linked))
                    if name in self.ui and f.linked:
                        shown = self._shown(f)
                        text = shown is not None and self.kinds[name] in ("textFieldGrp", "textFieldButtonGrp")
                        self._set(name, shown if text else widget_value(f, f.value))
            self._edit("c4_due_time", enable=self.value("c4_due") == TYPED)
            have = d is not None and not self._busy
            ready = (have and self.checked is not None and self.checked.ok and self.submitted is None
                     and self.signature() == self._checked_sig)
            self._edit("c4_check", enable=have)
            self._edit("c4_submit", enable=ready)
            self._edit("c4_show_file", enable=not self._busy and self.checked is not None)
            self._edit("c4_save_file", enable=not self._busy and self.checked is not None)
            if not self._showing_file:
                self._show_summary()
        finally:
            self._updating -= 1

    # --- what changed ----------------------------------------------------------------

    def signature(self) -> str:
        """Everything a check depends on, so a change since is seen: the
        draft's own signature (the scene as of its last read), and the
        host's as it is now, so an edit made since that read counts without
        reading the render layers again."""
        if self.draft is None:
            return ""
        return self.draft.signature() + "\nnow=" + self.host.signature()

    def changed(self, name: str) -> None:
        """The artist edited a control (or pressed a button)."""
        if self._updating:
            return
        if self.kinds.get(name) in TEXT_KINDS:
            self._known[name] = self.value(name)
        d = self.draft
        if name == "c4_check":
            self.check()
            return
        if name == "c4_submit":
            self.submit()
            return
        if name == "c4_show_file":
            self.show_file()
            return
        if name == "c4_save_file":
            self.save_file()
            return
        if name == "c4_project":
            if d is not None:
                d.project = self._text_of("c4_project")
            self.load()
        elif name == "c4_tool":
            names = [self._tool_label(t) for t in self.tools]
            if self.value("c4_tool") in names:
                self._use_tool(self.tools[names.index(self.value("c4_tool"))])
        elif d is None:
            pass
        elif name == "c4_shot":
            d.shot = self._text_of("c4_shot")
        elif name == "c4_due":
            labels = [cmds.menuItem(i, query=True, label=True)
                      for i in cmds.optionMenuGrp(self.ui["c4_due"], query=True, itemListLong=True) or []]
            v = self.value("c4_due")
            i = labels.index(v) if v in labels else len(labels) - 2
            due = self._due_values[i] if i < len(self._due_values) else "suggested"
            d.due = due if due else (self._text_of("c4_due_time") or "suggested")
        elif name == "c4_due_time":
            self._set("c4_due", TYPED)
            d.due = self._text_of("c4_due_time") or "suggested"
        elif name == "c4_frames":
            typed = self._text_of("c4_frames")
            d.frames = typed if typed and typed != EACH_RANGE else None
        elif name == "c4_frames_linked":
            if self.value("c4_frames_linked"):
                d.frames = None
            else:
                shown = self._text_of("c4_frames")
                d.frames = shown if shown and shown != EACH_RANGE else d.scene_frames
        elif name in self._layers:
            d.tick(self._layers[name], bool(self.value(name)))
        elif name == "c4_size":
            v = self.value("c4_size")
            d.size = "" if v == STANDARD else v
        elif name == "c4_note":
            d.note = self._text_of("c4_note")
        elif name.startswith("c4p_") and name.endswith("_linked"):
            d.form.link(name[len("c4p_"):-len("_linked")], bool(self.value(name)))
        elif name.startswith("c4p_"):
            try:
                f = d.form.field(name[len("c4p_"):])
            except KeyError:
                return
            d.form.set(f.name, widget_read(f, self.kinds[name], self.ui[name]))
        self._sync()

    def _scene_event(self) -> None:
        """A scene was saved or opened: read it again, unless it is the
        window's own save while it prepares a check."""
        if self._preparing or self._busy or self.draft is None:
            return
        mayautils.executeDeferred(self._refresh_scene)

    # --- the farm's tools and the project -----------------------------------------------

    def _tool_label(self, t: Tool) -> str:
        """A tool by its title, with its name when another has the same title."""
        same = [x for x in self.tools if x is not t and x.title == t.title]
        return f"{t.title} ({t.name})" if same else t.title

    def load(self) -> None:
        """Reads the project's milestones and the tools that render Maya
        scenes, off the main thread."""
        project = self._text_of("c4_project")
        scene = self.host.scene_file() or "untitled.ma"
        if not project:
            self._set_summary("Which project is this for? Fill in Project, or set C4_PROJECT in the studio's environment.")
            return
        self._set_summary(f"Reading {project}'s tools from the farm...")

        def work():
            info, tools, err = None, [], ""
            try:
                info = self.farm.project(project)
                tools = Submitter(self.farm).tools_for(project, scene)
                if not tools:
                    err = f"No tool on the farm renders Maya scenes for {project}. " + LOCAL_TOOLS
            except NotOnFarm as e:
                err = f"{e.message}, so this window has no form to draw. " + LOCAL_TOOLS
            except FarmError as e:
                hint = e.status in (0, 401, 403) and "C4_HUD" not in e.message
                err = e.message + (" (check C4_HUD and your sign-in)" if hint else "")
            except Exception as e:  # noqa: BLE001 - anything else is reported, never raised into Maya
                err = f"could not read the farm: {e}"
            mayautils.executeDeferred(self._loaded, project, info, tools, err)

        self._spawn(work)

    def _loaded(self, project: str, info: Optional[dict], tools: List[Tool], err: str) -> None:
        if not cmds.window(WINDOW, exists=True) or project != self._text_of("c4_project"):
            return  # closed, or the artist typed another project meanwhile; its own load answers
        self.project_info = info
        self.tools = tools
        choices = Submitter.due_choices(info)
        self._due_values = [c["value"] for c in choices[:-1]] + ["suggested", ""]
        labels = [c["label"] for c in choices[:-1]] + [SUGGESTED, TYPED]
        current = self.draft.due if self.draft else "suggested"
        self._updating += 1
        try:
            set_items(self.ui["c4_due"], labels)
            if current in self._due_values[:-1]:
                self._set("c4_due", labels[self._due_values.index(current)])
            else:
                self._set("c4_due", TYPED)
                self._set("c4_due_time", current)
            set_items(self.ui["c4_tool"], [self._tool_label(t) for t in tools])
        finally:
            self._updating -= 1
        if err or not tools:
            self.draft = None
            self.checked = None
            self._rebuild()
            self._set_summary(err)
            return
        keep = self.draft.tool.name if self.draft else ""
        tool = next((t for t in tools if t.name == keep), tools[0])
        self._set("c4_tool", self._tool_label(tool))
        self._use_tool(tool)
        self._set_summary("Check asks the farm when this would be ready. Nothing renders until you Submit.")

    def _use_tool(self, tool: Tool) -> None:
        old = self.draft
        d = Draft(self.host, tool, project=self.project_info)
        d.project = self._text_of("c4_project")
        d.shot = self._text_of("c4_shot")
        if old is not None:
            d.due, d.frames, d.note, d.size = old.due, old.frames, old.note, (old.size if old.size in tool.sizes else "")
            d.ticked = [r.name for r in d.renderables if r.name in old.ticked]
        else:
            labels = [cmds.menuItem(i, query=True, label=True)
                      for i in cmds.optionMenuGrp(self.ui["c4_due"], query=True, itemListLong=True) or []]
            v = self.value("c4_due")
            i = labels.index(v) if v in labels else 0
            due = self._due_values[i] if i < len(self._due_values) else "suggested"
            d.due = due or (self._text_of("c4_due_time") or "suggested")
        self.draft = d
        self.checked = None
        self.submitted = None
        self._rebuild()

    def _refresh_scene(self, later: bool = False) -> None:
        """Re-reads the scene (main thread), re-drawing the render layers
        when they changed. ``later`` re-draws after the current callback,
        because a button's own callback must not delete the button."""
        d = self.draft
        if d is None or not cmds.window(WINDOW, exists=True):
            return
        before = [(r.name, r.frames, r.note) for r in d.renderables]
        d.refresh()
        if [(r.name, r.frames, r.note) for r in d.renderables] == before:
            self._sync()
        elif later:
            mayautils.executeDeferred(self._rebuild)
        else:
            self._rebuild()

    # --- work off the main thread ----------------------------------------------------------

    def _spawn(self, fn: Callable[[], None]) -> threading.Thread:
        t = threading.Thread(target=fn, name="c4-submit", daemon=True)
        with self._lock:
            self._threads = [x for x in self._threads if x.is_alive()] + [t]
        t.start()
        return t

    def working(self) -> bool:
        """Whether the window's work is still in flight (for tests and scripts)."""
        with self._lock:
            return any(t.is_alive() for t in self._threads)

    def _start_progress(self, title: str) -> None:
        self._cancel.clear()
        self._last_poll = 0.0
        if self._progress_open:
            cmds.progressWindow(endProgress=True)
        cmds.progressWindow(title=title, status=title, progress=0, maxValue=100, isInterruptable=True)
        self._progress_open = True

    def _end_progress(self) -> None:
        if self._progress_open:
            self._progress_open = False
            cmds.progressWindow(endProgress=True)

    def _tick(self, stage: str, done: int, total: int) -> None:
        """Progress from a worker: the main thread shows the latest."""
        word = PROGRESS_WORDS.get(stage, stage)
        if stage == "upload":
            text = f"{word} {summarylib.size(done)} of {summarylib.size(total)}"
        else:
            text = f"{word}: {done:,} of {total:,}"
        with self._lock:
            self._latest = (text, int(100 * done / total) if total else 100)
            if self._pump_pending:
                return
            self._pump_pending = True
        mayautils.executeDeferred(self._pump)

    def _pump(self) -> None:
        with self._lock:
            text, pct = self._latest
            self._pump_pending = False
        if self._progress_open:
            cmds.progressWindow(edit=True, progress=pct, status=text)

    def _cancelled(self) -> bool:
        """Whether the artist cancelled, asked of the main thread at most
        every CANCEL_POLL seconds, from any worker."""
        if self._cancel.is_set():
            return True
        with self._lock:
            now = time.monotonic()
            if now - self._last_poll < CANCEL_POLL:
                return False
            self._last_poll = now
        try:
            if mayautils.executeInMainThreadWithResult(self._poll_cancel):
                self._cancel.set()
        except Exception:  # noqa: BLE001 - a Maya closing down cancels nothing
            pass
        return self._cancel.is_set()

    def _poll_cancel(self) -> bool:
        return self._progress_open and bool(cmds.progressWindow(query=True, isCancelled=True))

    def _busy_with(self, text: str) -> None:
        self._busy = True
        self._set_summary(text)
        self._sync()

    # --- Check -------------------------------------------------------------------------------

    def check(self) -> bool:
        """Reads the scene, then on a worker thread hashes its files and
        asks the farm to check the submission. False when it did not start."""
        if self._busy or self.draft is None:
            return False
        self._pull()
        self._refresh_scene(later=True)
        self._preparing = True
        try:
            prepared = prepare(self.draft)
        except Exception as e:  # noqa: BLE001 - a scene Maya cannot read reports, never raises
            self._set_summary(f"Could not read the scene: {e}")
            return False
        finally:
            self._preparing = False
        self.checked, self.submitted = None, None
        sig = self.signature()
        farm = self._farm
        self._busy_with("Checking with the farm...")
        self._start_progress("Checking with the farm")

        def work():
            result: Tuple[Optional[Checked], str] = (None, "")
            try:
                s = Submitter(farm or self.farm, progress=self._tick, cancelled=self._cancelled)
                result = (s.check(prepared), "")
            except Cancelled:
                result = (None, "Check cancelled.")
            except FarmError as e:
                result = (None, e.message)
            except Exception as e:  # noqa: BLE001
                result = (None, f"The check failed: {e}")
            mayautils.executeDeferred(self._check_done, result, sig)

        self._spawn(work)
        return True

    def _check_done(self, result: Tuple[Optional[Checked], str], sig: str) -> None:
        self._end_progress()
        checked, err = result
        self._busy = False
        if checked is None:
            self.checked = None
            self._set_summary(err)
            self._sync()
            return
        self.checked, self._checked_sig = checked, sig
        text = problems_text(checked) + summarylib.text(checked, errors=False)
        if checked.ok:
            text += "\n\nReady: press Submit to send it to the farm."
        elif not checked.errors:
            text += ("\n\nThe farm could not check this submission. Save file keeps it, "
                     "to submit with c4 farm submit when the farm takes it.")
        else:
            text += "\n\nFix what is listed and Check again."
        self._set_summary(text)
        self._sync()

    # --- Submit ------------------------------------------------------------------------------

    def submit(self) -> bool:
        """Sends what the farm lacks and starts the jobs, on a worker
        thread. Refused unless the last check passed and nothing changed."""
        if self._busy:
            return False
        self._pull()
        c = self.checked
        if c is None or not c.ok or self.submitted is not None:
            self._set_summary("Check first: Submit sends what the last check passed.")
            self._sync()
            return False
        if self.signature() != self._checked_sig:
            self._sync()
            return False
        farm = self.farm
        self._busy_with("Submitting to the farm...")
        self._start_progress("Submitting to the farm")

        def work():
            result: Tuple[Optional[Submitted], str] = (None, "")
            try:
                s = Submitter(farm, progress=self._tick, cancelled=self._cancelled)
                result = (s.submit(c), "")
            except Cancelled:
                result = (None, "Submit cancelled: nothing was started.")
            except FarmError as e:
                result = (None, "Not submitted: " + e.message + _farm_errors(e))
            except Exception as e:  # noqa: BLE001
                result = (None, f"Not submitted: {e}")
            mayautils.executeDeferred(self._submit_done, result)

        self._spawn(work)
        return True

    def _submit_done(self, result: Tuple[Optional[Submitted], str]) -> None:
        self._end_progress()
        out, err = result
        self._busy = False
        if out is None:
            self._set_summary(err)
            self._sync()
            return
        self.submitted = out
        base = getattr(self.farm, "base", "")
        rows = ["Submitted to the farm:"]
        for j in out.answer.get("jobs") or []:
            rows.append("  " + j.get("name", "") + (f"   {base}{j['link']}" if base and j.get("link") else ""))
        if out.uploaded:
            rows.append(f"{out.uploaded:,} files sent ({summarylib.size(out.uploaded_bytes)}); the farm had the rest.")
        else:
            rows.append("Nothing to send: the farm had every file.")
        self._set_summary("\n".join(rows))
        self._sync()

    # --- the file -------------------------------------------------------------------------------

    def show_file(self) -> bool:
        """Shows the submission as a file in place of the summary, or the
        summary again."""
        if self.checked is None:
            return False
        if self._showing_file:
            self._showing_file = False
            self._show_summary()
        else:
            self._showing_file = True
            self._set("c4_summary", self.checked.yaml())
        return True

    def save_file(self, path: Optional[str] = None) -> Optional[str]:
        """Saves the checked submission as a file; asks where when no path
        is given. Refused when the draft changed since the check."""
        c = self.checked
        if c is None:
            return None
        self._pull()
        if self.signature() != self._checked_sig:
            self._sync()
            return None
        if path is None:
            folder = os.path.dirname(self.draft.scene_file) if self.draft and self.draft.scene_file else ""
            picked = cmds.fileDialog2(fileMode=0, caption="Save the submission as", fileFilter="Submission (*.yaml)",
                                      startingDirectory=os.path.join(folder, default_filename(c)) if folder else None,
                                      dialogStyle=2)
            if not picked:
                return None
            path = picked[0]
        if not os.path.splitext(path)[1]:
            path += ".yaml"
        try:
            save(c, path)
        except OSError as e:
            self._set_summary(f"Could not save {path}: {e}")
            return None
        note = f" It has {len(c.errors)} problem{'s' if len(c.errors) != 1 else ''} to fix first." if c.errors else ""
        self._set_summary(f"Saved {path}.{note}\nSubmit it later with: c4 farm submit {os.path.basename(path)}"
                          f"\n\n{problems_text(c)}{summarylib.text(c, errors=False)}")
        return path


def problems_text(checked: Checked) -> str:
    """A check's errors, each with its fix on the line under it, then its
    warnings, ahead of the summary; '' when there are none."""
    rows = []
    if checked.errors:
        rows.append("To fix before submitting:")
        for e in checked.errors:
            rows.append("  " + (f"{e['path']}: " if e.get("path") else "") + e["message"])
            if e.get("fix"):
                rows.append("      " + e["fix"])
    if checked.warnings:
        rows.append("Worth knowing:")
        rows += ["  " + w for w in checked.warnings]
    return "\n".join(rows) + "\n\n" if rows else ""


def _farm_errors(e: FarmError) -> str:
    body = e.body
    errs = body if isinstance(body, list) else (body.get("errors") if isinstance(body, dict) else None)
    if not errs:
        return ""
    return "\n" + "\n".join(f"  {x.get('path', '')}: {x.get('message', '')}" for x in errs if isinstance(x, dict))


# --- opening it ----------------------------------------------------------------------------------

_open: List[SubmitWindow] = []


def open_panel(host: Optional[MayaHost] = None, farm: Optional[Farm] = None, show: bool = True) -> SubmitWindow:
    """Opens the window (closing one already open) and starts reading the
    farm's tools. Kept referenced here while it is open."""
    del _open[:]
    w = SubmitWindow(host=host, farm=farm)
    _open.append(w)
    w.load()
    if show:
        cmds.showWindow(w.window)
    return w
