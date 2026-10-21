"""The submit dialog, in Katana's own Qt, opened from Katana's farm menus.

Katana's UI is Qt; the dialog takes it the way Katana plug-ins do
(``from Katana import QtCore, QtWidgets``), so it uses whichever binding
the Katana version ships. Top to bottom: Tool, Project, Shot, Needed by,
Frames, one checkbox per Render node, Size, Note, the tool's own
parameters, then Check, Submit, Show file and Save file, and the summary
the check returns.

"Needed by" is the only urgency the artist sets. Check and Submit hash,
upload and talk to the farm on a worker thread. A timer on the main
thread takes their answers and their progress back (a progress dialog
whose Cancel stops them), so nothing touches Qt off the main thread.
Submit is available only after a check passed and nothing has changed
since.
"""

import importlib
import os
import queue
import threading
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Tuple

from c4submit import summary as summarylib
from c4submit.collect import Cancelled
from c4submit.farm import Farm, FarmError, NotOnFarm
from c4submit.form import Field, Form
from c4submit.submitter import Checked, Draft, Submitted, Submitter, default_filename, prepare, save
from c4submit.tool import Tool

from c4_katana.host import KatanaHost


def _qt():
    """Katana's Qt, else the first binding that imports."""
    try:
        from Katana import QtCore, QtWidgets  # noqa: PLC0415
        return QtCore, QtWidgets
    except ImportError:
        pass
    for binding in ("PySide6", "PySide2", "PyQt5"):
        try:
            return (importlib.import_module(binding + ".QtCore"), importlib.import_module(binding + ".QtWidgets"))
        except ImportError:
            continue
    raise ImportError("the C4 dialog needs Katana's Qt")


QtCore, QtWidgets = _qt()

TITLE = "Submit to C4"
SUGGESTED = "When the farm suggests"
TYPED = "At a time I type"
STANDARD = "Standard"
EACH_RANGE = "each Render node's own range"
EACH_NODE = "each ticked Render node"
STALE = "Changed since the check: Check again before you submit."
LOCAL_TOOLS = ("Setting C4_TOOL_PATH to the studio's tool folders lets this dialog draw the form and save "
               "the submission as a file, to submit when the farm takes it.")
PROGRESS_WORDS = {"hash": "Reading files", "upload": "Uploading"}
#: How often, in milliseconds, the main thread takes in the workers' news.
POLL_MS = 50
#: Kinds whose value is text the artist types.
TEXT_KINDS = ("line", "file", "text")


# --- the form, as widget specs ----------------------------------------------------

@dataclass
class WidgetSpec:
    """A widget to make, without making it: what a field draws as, so the
    mapping is tested without Qt."""
    kind: str                      # line, text, combo, check, int, float, file, linked, group, fold, end
    name: str
    label: str = ""
    value: Any = None
    items: List[str] = field(default_factory=list)   # a combo's choices
    tooltip: str = ""
    param: str = ""
    file_mode: str = ""            # open, save or dir, for a file field
    file_filter: str = ""
    minimum: Optional[float] = None
    maximum: Optional[float] = None


def widget_name(param: str) -> str:
    return "p_" + param


def linked_name(param: str) -> str:
    return "p_" + param + "__linked"


def _text(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, (list, tuple)):
        return ", ".join(str(v) for v in value)
    if isinstance(value, bool):
        return "on" if value else "off"
    return str(value)


def widget_kind(f: Field) -> str:
    """The widget a field's control draws as."""
    c = f.control
    if c == "MULTILINE_EDIT":
        return "text"
    if c == "DROPDOWN_LIST" and f.param.allowed:
        return "combo"
    if c == "CHECK_BOX":
        return "check"
    if c == "SPIN_BOX":
        return "float" if f.param.type == "FLOAT" else "int"
    if c.startswith("CHOOSE_"):
        return "file"
    return "line"


def widget_value(f: Field, value: Any) -> Any:
    """A field's value as its widget holds it."""
    kind = widget_kind(f)
    if kind == "check":
        try:
            return bool(f.param.coerce(value)) if value is not None else False
        except ValueError:
            return False
    if kind in ("int", "float"):
        try:
            v = f.param.coerce(value) if value not in (None, "") else 0
        except ValueError:
            v = 0
        return int(v) if kind == "int" else float(v)
    if kind == "combo":
        items = [str(a) for a in f.param.allowed or []]
        s = _text(value)
        return s if s in items else (items[0] if items else "")
    return _text(value)


def file_filter(f: Field) -> str:
    """A file dialog's filter for a file field: the tool's file filters,
    then every file."""
    out = []
    for flt in f.param.ui.get("fileFilters") or []:
        pats = flt.get("patterns") or []
        if pats:
            out.append(f"{flt.get('label') or 'Files'} ({' '.join(pats)})")
    return ";;".join(out + ["All files (*)"])


def field_specs(f: Field, shown: Any = None) -> List[WidgetSpec]:
    """The widgets one field draws: its own, and a "linked" checkbox beside
    it when its value comes from the project. ``shown`` is what a linked
    field shows instead of its value (for one value per job)."""
    kind = widget_kind(f)
    tip = f.param.description
    if f.derivable:
        tip = (tip + "\n" if tip else "") + "Linked: follows the project until you type into it."
    value = shown if shown is not None and kind in ("line", "file") else widget_value(f, f.value)
    spec = WidgetSpec(kind, widget_name(f.name), f.label, value, tooltip=tip, param=f.name,
                      items=[str(a) for a in f.param.allowed or []] if kind == "combo" else [])
    if kind in ("int", "float"):
        spec.minimum = f.param.min if f.param.min is not None else -1e9
        spec.maximum = f.param.max if f.param.max is not None else 1e9
    if kind == "file":
        spec.file_mode = {"CHOOSE_OUTPUT_FILE": "save", "CHOOSE_DIRECTORY": "dir"}.get(f.control, "open")
        spec.file_filter = file_filter(f) if spec.file_mode != "dir" else ""
    out = [spec]
    if f.derivable:
        out.append(WidgetSpec("linked", linked_name(f.name), "linked", bool(f.linked), param=f.name,
                              tooltip="On: follow the project. Typing into the field turns this off."))
    return out


def form_specs(form: Form, shown: Optional[Callable[[Field], Any]] = None) -> List[WidgetSpec]:
    """The widgets a tool's form draws, in its order: each ``groupLabel``
    as a titled box, ``show: advanced`` fields in a folded section."""
    out: List[WidgetSpec] = []
    for i, (group, fields) in enumerate(form.groups()):
        plain = [f for f in fields if not f.advanced]
        folded = [f for f in fields if f.advanced]
        if plain:
            if group:
                out.append(WidgetSpec("group", f"g_{i}", group))
            for f in plain:
                out += field_specs(f, shown(f) if shown else None)
            if group:
                out.append(WidgetSpec("end", f"g_{i}_end"))
        if folded:
            label = group if group and not plain else (f"{group}: advanced" if group else "Advanced")
            out.append(WidgetSpec("fold", f"g_{i}_folded", label))
            for f in folded:
                out += field_specs(f, shown(f) if shown else None)
            out.append(WidgetSpec("end", f"g_{i}_folded_end"))
    return out


def make_widget(spec: WidgetSpec):
    """The Qt widget a value spec describes."""
    k = spec.kind
    if k == "combo":
        w = QtWidgets.QComboBox()
        w.addItems(spec.items)
        i = w.findText(_text(spec.value))
        if i >= 0:
            w.setCurrentIndex(i)
    elif k in ("check", "linked"):
        w = QtWidgets.QCheckBox(spec.label if k == "linked" else "")
        w.setChecked(bool(spec.value))
    elif k in ("int", "float"):
        w = QtWidgets.QSpinBox() if k == "int" else QtWidgets.QDoubleSpinBox()
        if k == "float":
            w.setDecimals(3)
        w.setRange(int(spec.minimum) if k == "int" else spec.minimum, int(spec.maximum) if k == "int" else spec.maximum)
        w.setValue(spec.value or 0)
    elif k == "text":
        w = QtWidgets.QPlainTextEdit()
        w.setPlainText(_text(spec.value))
        w.setMaximumHeight(60)
    else:
        w = QtWidgets.QLineEdit(_text(spec.value))
    w.setObjectName(spec.name)
    if spec.tooltip:
        w.setToolTip(spec.tooltip)
    return w


def read_widget(kind: str, w) -> Any:
    if kind in ("line", "file"):
        return w.text()
    if kind == "text":
        return w.toPlainText()
    if kind == "combo":
        return w.currentText()
    if kind in ("check", "linked"):
        return bool(w.isChecked())
    if kind == "int":
        return int(w.value())
    if kind == "float":
        return float(w.value())
    if kind == "label":
        return w.text()
    return None


def write_widget(kind: str, w, value: Any) -> None:
    if kind in ("line", "file", "label"):
        w.setText(_text(value))
    elif kind == "text":
        if w.toPlainText() != _text(value):
            w.setPlainText(_text(value))
    elif kind == "combo":
        i = w.findText(_text(value))
        if i >= 0:
            w.setCurrentIndex(i)
    elif kind in ("check", "linked"):
        w.setChecked(bool(value))
    elif kind == "int":
        w.setValue(int(value or 0))
    elif kind == "float":
        w.setValue(float(value or 0))


def set_items(combo, items: List[str]) -> None:
    combo.clear()
    combo.addItems(items)


# --- the dialog -----------------------------------------------------------------------

class SubmitDialog(QtWidgets.QDialog):
    """The dialog. ``nodes``, when given, are the only Render nodes ticked
    when it opens (the Render node's "Submit this Render node to C4")."""

    def __init__(self, host: Optional[KatanaHost] = None, farm: Optional[Farm] = None,
                 nodes: Optional[List[str]] = None, parent: Any = None):
        QtWidgets.QDialog.__init__(self, parent)
        self.setWindowTitle(TITLE)
        self.setObjectName("c4SubmitDialog")
        self.host = host or KatanaHost()
        self._farm = farm
        self._only = list(nodes) if nodes is not None else None
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
        self._updating = 0
        self._threads: List[threading.Thread] = []
        self._lock = threading.Lock()
        self._inbox: "queue.Queue[Tuple[Callable, tuple]]" = queue.Queue()
        self._cancel = threading.Event()
        self._latest: Optional[Tuple[str, int]] = None
        self._progress = None
        self._nodes: Dict[str, str] = {}      # widget name -> Render node
        self.w: Dict[str, Any] = {}             # widget name -> widget
        self.kinds: Dict[str, str] = {}
        self._timer = QtCore.QTimer(self)
        self._timer.timeout.connect(self._poll)

        self.main = QtWidgets.QVBoxLayout(self)
        top = QtWidgets.QFormLayout()
        self.main.addLayout(top)
        tool = self._add("c4_tool", "combo", QtWidgets.QComboBox())
        tool.setToolTip("The farm's tools that render Katana projects.")
        tool.activated.connect(self._cb("c4_tool"))
        top.addRow("Tool", tool)
        project = self._add("c4_project", "line", QtWidgets.QLineEdit(self.host.project()))
        project.editingFinished.connect(self._cb("c4_project"))
        top.addRow("Project", project)
        shot = self._add("c4_shot", "line", QtWidgets.QLineEdit(self.host.shot()))
        shot.textEdited.connect(self._cb("c4_shot"))
        top.addRow("Shot", shot)
        due_row = QtWidgets.QHBoxLayout()
        due = self._add("c4_due", "combo", QtWidgets.QComboBox())
        set_items(due, [SUGGESTED, TYPED])
        due.activated.connect(self._cb("c4_due"))
        due_time = self._add("c4_due_time", "line", QtWidgets.QLineEdit(""))
        due_time.setPlaceholderText("fri 18:00")
        due_time.setToolTip('A time like "fri 18:00", "18:00" or "+3h", or a date like 2026-10-23.')
        due_time.textEdited.connect(self._cb("c4_due_time"))
        due_row.addWidget(due)
        due_row.addWidget(due_time)
        top.addRow("Needed by", due_row)
        frames_row = QtWidgets.QHBoxLayout()
        frames = self._add("c4_frames", "line", QtWidgets.QLineEdit(self.host.frames()))
        frames.setToolTip("Frames to render, like 1001-1048 or 1001-1100:2. Linked, each Render node renders its "
                          "own range (or the project's); typing here sets one range for all of them.")
        frames.textEdited.connect(self._cb("c4_frames"))
        linked = self._add("c4_frames_linked", "linked", QtWidgets.QCheckBox("linked"))
        linked.setChecked(True)
        linked.setToolTip("On: each Render node's own range. Typing frames turns this off.")
        linked.clicked.connect(self._cb("c4_frames_linked"))
        frames_row.addWidget(frames)
        frames_row.addWidget(linked)
        self._frames_label = QtWidgets.QLabel("Frames")
        top.addRow(self._frames_label, frames_row)
        self.dynamic = None
        self._rebuild()
        self._set_summary("Reading the farm's tools...")

    # --- the farm -------------------------------------------------------------

    @property
    def farm(self) -> Farm:
        if self._farm is None:
            self._farm = Farm(client="c4submit-katana")
        return self._farm

    # --- widgets ------------------------------------------------------------------

    def _cb(self, name: str) -> Callable:
        return lambda *_: self.changed(name)

    def _add(self, name: str, kind: str, w):
        w.setObjectName(name)
        self.w[name], self.kinds[name] = w, kind
        return w

    def value(self, name: str) -> Any:
        return read_widget(self.kinds[name], self.w[name])

    def _text_of(self, name: str) -> str:
        return (self.value(name) or "").strip()

    def _set(self, name: str, value: Any) -> None:
        if name not in self.w:
            return
        self._updating += 1
        try:
            write_widget(self.kinds[name], self.w[name], value)
        finally:
            self._updating -= 1

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
        if d is None or not f.linked or not d.per_renderable:
            return None
        if f.param.source == "scene.renderables" and len(d.picked()) > 1:
            return EACH_NODE
        if f.param.source in ("scene.frames", "shot.frames") and len({d.job_frames(r) for r in d.picked()}) > 1:
            return EACH_RANGE
        return None

    def _browse(self, spec: WidgetSpec) -> None:
        start = self._text_of(spec.name)
        folder = os.path.dirname(start) if start else ""
        if spec.file_mode == "dir":
            picked = QtWidgets.QFileDialog.getExistingDirectory(self, spec.label, folder)
        elif spec.file_mode == "save":
            picked, _ = QtWidgets.QFileDialog.getSaveFileName(self, spec.label, folder, spec.file_filter)
        else:
            picked, _ = QtWidgets.QFileDialog.getOpenFileName(self, spec.label, folder, spec.file_filter)
        if picked:
            self._set(spec.name, picked)
            self.changed(spec.name)

    def _rebuild(self) -> None:
        """Re-draws everything below Frames: the Render nodes, Size, Note,
        the tool's parameters, the buttons and the summary."""
        if self.dynamic is not None:
            self.main.removeWidget(self.dynamic)
            self.dynamic.setParent(None)
            self.dynamic.deleteLater()
        keep = ("c4_tool", "c4_project", "c4_shot", "c4_due", "c4_due_time", "c4_frames", "c4_frames_linked")
        for name in [n for n in self.w if n not in keep]:
            self.w.pop(name)
            self.kinds.pop(name)
        self._nodes = {}
        d = self.draft
        self.dynamic = QtWidgets.QWidget(self)
        self.dynamic.setObjectName("c4_dynamic")
        col = QtWidgets.QVBoxLayout(self.dynamic)
        col.setContentsMargins(0, 0, 0, 0)
        self._updating += 1
        try:
            takes_node = d is not None and any(p.source == "scene.renderables" for p in d.tool.params)
            if d is not None and d.per_renderable:
                box = self._add("c4_renders", "label", QtWidgets.QGroupBox("Render nodes"))
                rows = QtWidgets.QVBoxLayout(box)
                for i, r in enumerate(d.renderables):
                    name = f"c4_render_{i}"
                    label = f"{r.label}  {r.frames or 'project range'}" + (f"  ({r.note})" if r.note else "")
                    c = self._add(name, "check", QtWidgets.QCheckBox(label))
                    c.setChecked(r.name in d.ticked)
                    c.setToolTip("Renders " + ", ".join(r.outputs) if r.outputs else "Renders this Render node")
                    c.clicked.connect(self._cb(name))
                    rows.addWidget(c)
                    self._nodes[name] = r.name
                col.addWidget(box)
            elif takes_node:
                col.addWidget(self._add("c4_renders", "label", QtWidgets.QLabel(
                    "Render nodes: none in this project to render")))
            form = QtWidgets.QFormLayout()
            col.addLayout(form)
            if d is not None and d.tool.sizes:
                size = self._add("c4_size", "combo", QtWidgets.QComboBox())
                set_items(size, [STANDARD] + list(d.tool.sizes))
                if d.size:
                    size.setCurrentIndex(size.findText(d.size))
                size.activated.connect(self._cb("c4_size"))
                form.addRow("Size", size)
            note = self._add("c4_note", "text", QtWidgets.QPlainTextEdit(d.note if d else ""))
            note.setMaximumHeight(48)
            note.setToolTip("Shown with the jobs on the farm, for whoever looks at them.")
            note.textChanged.connect(self._cb("c4_note"))
            form.addRow("Note", note)
            if d is not None:
                self._draw_form(col, form, form_specs(d.form, self._shown))
            buttons = QtWidgets.QHBoxLayout()
            for name, label, tip in (
                    ("c4_check", "Check", "Reads every file the project needs and asks the farm when it would be "
                                          "ready. Nothing is sent to render."),
                    ("c4_submit", "Submit", "Sends the files the farm lacks and starts the jobs. Check first."),
                    ("c4_show_file", "Show file", "Shows the submission as a file, in place of the summary."),
                    ("c4_save_file", "Save file...", "Saves the submission as a file, to submit later with "
                                                     "c4 farm submit.")):
                b = self._add(name, "button", QtWidgets.QPushButton(label))
                b.setToolTip(tip)
                b.clicked.connect(self._cb(name))
                buttons.addWidget(b)
            col.addLayout(buttons)
            s = self._add("c4_summary", "text", QtWidgets.QPlainTextEdit(self._summary))
            s.setReadOnly(True)
            s.setMinimumHeight(220)
            s.setStyleSheet("font-family: monospace;")
            col.addWidget(s)
        finally:
            self._updating -= 1
        self.main.insertWidget(1, self.dynamic)
        self._sync()

    def _draw_form(self, col, form, specs: List[WidgetSpec]) -> None:
        """Lays out the tool's fields: each group a box, each folded group
        behind a toggle, a "linked" checkbox beside a derived field."""
        target = form
        i = 0
        while i < len(specs):
            spec = specs[i]
            if spec.kind == "group":
                box = QtWidgets.QGroupBox(spec.label)
                box.setObjectName(spec.name)
                target = QtWidgets.QFormLayout(box)
                col.addWidget(box)
            elif spec.kind == "fold":
                toggle = QtWidgets.QToolButton()
                toggle.setObjectName(spec.name)
                toggle.setText(spec.label)
                toggle.setCheckable(True)
                inner = QtWidgets.QWidget()
                inner.setObjectName(spec.name + "_body")
                inner.setVisible(False)
                toggle.toggled.connect(inner.setVisible)
                self.w[spec.name], self.kinds[spec.name] = toggle, "fold"
                col.addWidget(toggle)
                col.addWidget(inner)
                target = QtWidgets.QFormLayout(inner)
            elif spec.kind == "end":
                target = QtWidgets.QFormLayout()
                col.addLayout(target)
            else:
                w = self._add(spec.name, spec.kind, make_widget(spec))
                self._connect(spec, w)
                row = QtWidgets.QHBoxLayout()
                row.addWidget(w)
                if spec.kind == "file":
                    b = QtWidgets.QPushButton("...")
                    b.setObjectName(spec.name + "__browse")
                    b.setToolTip("Choose a file")
                    b.clicked.connect(lambda *_, s=spec: self._browse(s))
                    row.addWidget(b)
                if i + 1 < len(specs) and specs[i + 1].kind == "linked":
                    i += 1
                    lk = self._add(specs[i].name, "linked", make_widget(specs[i]))
                    self._connect(specs[i], lk)
                    row.addWidget(lk)
                target.addRow(spec.label, row)
            i += 1

    def _connect(self, spec: WidgetSpec, w) -> None:
        cb = self._cb(spec.name)
        if spec.kind in ("line", "file"):
            w.textEdited.connect(cb)
        elif spec.kind == "text":
            w.textChanged.connect(cb)
        elif spec.kind == "combo":
            w.activated.connect(cb)
        elif spec.kind in ("check", "linked"):
            w.clicked.connect(cb)
        elif spec.kind in ("int", "float"):
            w.valueChanged.connect(cb)

    def _sync(self) -> None:
        """Brings every shown value and button in line with the draft."""
        d = self.draft
        self._updating += 1
        try:
            if d is not None:
                for w in (self.w["c4_frames"], self.w["c4_frames_linked"], self._frames_label):
                    w.setVisible(d.tool.takes_frames)
                self._set("c4_frames_linked", d.frames is None)
                if d.frames is None:
                    if d.per_renderable:
                        ranges = {d.job_frames(r) for r in d.picked()} or {d.scene_frames}
                    else:
                        ranges = {d.job_frames(None)}
                    self._set("c4_frames", ranges.pop() if len(ranges) == 1 else EACH_RANGE)
                for f in d.form.drawn():
                    name, lname = widget_name(f.name), linked_name(f.name)
                    if lname in self.w:
                        self._set(lname, bool(f.linked))
                    if name in self.w and f.linked:
                        shown = self._shown(f)
                        text = shown is not None and self.kinds[name] in ("line", "file")
                        self._set(name, shown if text else widget_value(f, f.value))
            self.w["c4_due_time"].setEnabled(self.value("c4_due") == TYPED)
            have = d is not None and not self._busy
            ready = (have and self.checked is not None and self.checked.ok and self.submitted is None
                     and self.signature() == self._checked_sig)
            self.w["c4_check"].setEnabled(have)
            self.w["c4_submit"].setEnabled(ready)
            self.w["c4_show_file"].setEnabled(not self._busy and self.checked is not None)
            self.w["c4_save_file"].setEnabled(not self._busy and self.checked is not None)
            if not self._showing_file:
                self._show_summary()
        finally:
            self._updating -= 1

    # --- what changed ----------------------------------------------------------------

    def signature(self) -> str:
        """Everything a check depends on, so a change since is seen: the
        draft's own signature (the project as of its last read), and the
        host's as it is now, so an edit since that read counts."""
        if self.draft is None:
            return ""
        return self.draft.signature() + "\nnow=" + self.host.signature()

    def changed(self, name: str) -> None:
        """The artist edited a widget (or pressed a button)."""
        if self._updating:
            return
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
            if d is not None and d.project == self._text_of("c4_project"):
                return
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
            combo = self.w["c4_due"]
            i = combo.currentIndex()
            due = self._due_values[i] if 0 <= i < len(self._due_values) else "suggested"
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
        elif name in self._nodes:
            d.tick(self._nodes[name], bool(self.value(name)))
        elif name == "c4_size":
            v = self.value("c4_size")
            d.size = "" if v == STANDARD else v
        elif name == "c4_note":
            d.note = self._text_of("c4_note")
        elif name.startswith("p_") and name.endswith("__linked"):
            d.form.link(name[2:-len("__linked")], bool(self.value(name)))
        elif name.startswith("p_"):
            try:
                f = d.form.field(name[2:])
            except KeyError:
                return
            d.form.set(f.name, read_widget(self.kinds[name], self.w[name]))
        self._sync()

    # --- the farm's tools and the project -----------------------------------------------

    def _tool_label(self, t: Tool) -> str:
        """A tool by its title, with its name when another has the same title."""
        same = [x for x in self.tools if x is not t and x.title == t.title]
        return f"{t.title} ({t.name})" if same else t.title

    def load(self) -> None:
        """Reads the project's milestones and the tools that render Katana
        projects, off the main thread."""
        project = self._text_of("c4_project")
        scene = self.host.scene_file() or "untitled.katana"
        if not project:
            self._set_summary("Which project is this for? Fill in Project, "
                              "or set C4_PROJECT in the studio's environment.")
            return
        self._set_summary(f"Reading {project}'s tools from the farm...")

        def work():
            info, tools, err = None, [], ""
            try:
                info = self.farm.project(project)
                tools = Submitter(self.farm).tools_for(project, scene)
                if not tools:
                    err = f"No tool on the farm renders Katana projects for {project}. " + LOCAL_TOOLS
            except NotOnFarm as e:
                err = f"{e.message}, so this dialog has no form to draw. " + LOCAL_TOOLS
            except FarmError as e:
                hint = e.status in (0, 401, 403) and "C4_HUD" not in e.message
                err = e.message + (" (check C4_HUD and your sign-in)" if hint else "")
            except Exception as e:  # noqa: BLE001 - anything else is reported, never raised into Katana
                err = f"could not read the farm: {e}"
            self._later(self._loaded, project, info, tools, err)

        self._spawn(work)

    def _loaded(self, project: str, info: Optional[dict], tools: List[Tool], err: str) -> None:
        if project != self._text_of("c4_project"):
            return  # the artist typed another project meanwhile; its own load answers
        self.project_info = info
        self.tools = tools
        choices = Submitter.due_choices(info)
        self._due_values = [c["value"] for c in choices[:-1]] + ["suggested", ""]
        labels = [c["label"] for c in choices[:-1]] + [SUGGESTED, TYPED]
        current = self.draft.due if self.draft else "suggested"
        self._updating += 1
        try:
            set_items(self.w["c4_due"], labels)
            if current in self._due_values[:-1]:
                self.w["c4_due"].setCurrentIndex(self._due_values.index(current))
            else:
                self.w["c4_due"].setCurrentIndex(len(labels) - 1)
                self._set("c4_due_time", current)
            set_items(self.w["c4_tool"], [self._tool_label(t) for t in tools])
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
            d.due, d.frames, d.note = old.due, old.frames, old.note
            d.size = old.size if old.size in tool.sizes else ""
            d.ticked = [r.name for r in d.renderables if r.name in old.ticked]
        else:
            i = self.w["c4_due"].currentIndex()
            due = self._due_values[i] if 0 <= i < len(self._due_values) else "suggested"
            d.due = due or (self._text_of("c4_due_time") or "suggested")
        if self._only is not None:
            d.ticked = [r.name for r in d.renderables if r.name in self._only]
            self._only = None
        d.form.refresh(d._derive)
        self.draft = d
        self.checked = None
        self.submitted = None
        self._rebuild()

    def _refresh_scene(self) -> None:
        """Re-reads the project, re-drawing the Render nodes when they changed."""
        d = self.draft
        before = [(r.name, r.frames, r.note) for r in d.renderables]
        d.refresh()
        if [(r.name, r.frames, r.note) for r in d.renderables] == before:
            self._sync()
        else:
            self._rebuild()

    # --- work off the main thread ----------------------------------------------------------

    def _later(self, fn: Callable, *args) -> None:
        """From a worker: run ``fn(*args)`` on the main thread, at the next poll."""
        self._inbox.put((fn, args))

    def _spawn(self, fn: Callable[[], None]) -> threading.Thread:
        t = threading.Thread(target=fn, name="c4-submit", daemon=True)
        with self._lock:
            self._threads = [x for x in self._threads if x.is_alive()] + [t]
        t.start()
        if not self._timer.isActive():
            self._timer.start(POLL_MS)
        return t

    def working(self) -> bool:
        """Whether the dialog's work is still in flight (for tests and scripts)."""
        with self._lock:
            alive = any(t.is_alive() for t in self._threads)
        return alive or not self._inbox.empty()

    def _poll(self) -> None:
        """Main thread, on the timer: progress, Cancel, and the workers' answers."""
        with self._lock:
            latest, self._latest = self._latest, None
        if self._progress is not None:
            if latest is not None:
                self._progress.setLabelText(latest[0])
                self._progress.setValue(latest[1])
            if self._progress.wasCanceled():
                self._cancel.set()
        while True:
            try:
                fn, args = self._inbox.get_nowait()
            except queue.Empty:
                break
            fn(*args)
        if not self.working():
            self._timer.stop()

    def _tick(self, stage: str, done: int, total: int) -> None:
        """Progress from a worker: the main thread shows the latest."""
        word = PROGRESS_WORDS.get(stage, stage)
        if stage == "upload":
            text = f"{word} {summarylib.size(done)} of {summarylib.size(total)}"
        else:
            text = f"{word}: {done:,} of {total:,}"
        with self._lock:
            self._latest = (text, int(100 * done / total) if total else 100)

    def _start_progress(self, title: str) -> None:
        self._cancel.clear()
        self._progress = QtWidgets.QProgressDialog(title, "Cancel", 0, 100, self)
        self._progress.setWindowTitle(TITLE)
        self._progress.setMinimumDuration(0)
        self._progress.setAutoClose(False)
        self._progress.setValue(0)
        self._progress.show()

    def _end_progress(self) -> None:
        if self._progress is not None:
            self._progress.close()
            self._progress = None

    def _busy_with(self, text: str) -> None:
        self._busy = True
        self._set_summary(text)
        self._sync()

    # --- Check -------------------------------------------------------------------------------

    def check(self) -> bool:
        """Reads the project, then on a worker thread hashes its files and
        asks the farm to check the submission. False when it did not start."""
        if self._busy or self.draft is None:
            return False
        self._refresh_scene()
        try:
            prepared = prepare(self.draft)
        except Exception as e:  # noqa: BLE001 - a project Katana cannot read reports, never raises
            self._set_summary(f"Could not read the project: {e}")
            return False
        self.checked, self.submitted = None, None
        sig = self.signature()
        farm = self._farm
        self._busy_with("Checking with the farm...")
        self._start_progress("Checking with the farm")

        def work():
            result: Tuple[Optional[Checked], str] = (None, "")
            try:
                s = Submitter(farm or self.farm, progress=self._tick, cancelled=self._cancel.is_set)
                result = (s.check(prepared), "")
            except Cancelled:
                result = (None, "Check cancelled.")
            except FarmError as e:
                result = (None, e.message)
            except Exception as e:  # noqa: BLE001
                result = (None, f"The check failed: {e}")
            self._later(self._check_done, result, sig)

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
                s = Submitter(farm, progress=self._tick, cancelled=self._cancel.is_set)
                result = (s.submit(c), "")
            except Cancelled:
                result = (None, "Submit cancelled: nothing was started.")
            except FarmError as e:
                result = (None, "Not submitted: " + e.message + _farm_errors(e))
            except Exception as e:  # noqa: BLE001
                result = (None, f"Not submitted: {e}")
            self._later(self._submit_done, result)

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
        if self.signature() != self._checked_sig:
            self._sync()
            return None
        if path is None:
            folder = os.path.dirname(self.draft.scene_file) if self.draft and self.draft.scene_file else ""
            start = os.path.join(folder, default_filename(c)) if folder else default_filename(c)
            path, _ = QtWidgets.QFileDialog.getSaveFileName(self, "Save the submission as", start,
                                                            "Submission (*.yaml)")
            if not path:
                return None
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

_open: List[SubmitDialog] = []


def main_window():
    """Katana's main window, to parent the dialog to; None without one."""
    try:
        from UI4.App import MainWindow  # noqa: PLC0415
        return MainWindow.GetMainWindow()
    except Exception:  # noqa: BLE001 - another Katana, or no UI
        return None


def open_dialog(nodes: Optional[List[str]] = None, host: Optional[KatanaHost] = None, farm: Optional[Farm] = None,
                show: bool = True) -> SubmitDialog:
    """Opens the dialog (closing one already open) and starts reading the
    farm's tools. Kept referenced here while it is open."""
    for old in _open:
        old.close()
    del _open[:]
    d = SubmitDialog(host=host, farm=farm, nodes=nodes, parent=main_window())
    _open.append(d)
    d.load()
    if show:
        d.show()
        d.raise_()
        d.activateWindow()
    return d
