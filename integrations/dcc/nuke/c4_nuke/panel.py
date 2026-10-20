"""The submit panel, drawn in Nuke's own knobs.

A ``nukescripts.PythonPanel`` rather than Qt, so it is the same from Nuke 14
to 16 whatever PySide each ships, and tests drive it with a fake ``nuke``.
Top to bottom: Tool, Project, Shot, Needed by, Frames, one checkbox per
Write node, Size, Note, the tool's own parameters, then Check, Submit,
Show file and Save file, and the summary the check returns.

"Needed by" is the only urgency the artist sets. Check and Submit hash,
upload and talk to the farm on a worker thread, with a cancellable
progress bar, and bring their answers back to the main thread. Submit is
available only after a check passed and nothing has changed since.
"""

import json
import os
import threading
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Tuple

import nuke
import nukescripts

from c4submit import summary as summarylib
from c4submit.collect import Cancelled
from c4submit.farm import Farm, FarmError, NotOnFarm
from c4submit.form import Field, Form
from c4submit.submitter import Checked, Draft, Submitted, Submitter, default_filename, prepare, save
from c4submit.tool import Tool

from c4_nuke.host import NukeHost, is_write

TITLE = "Submit to C4"
SUGGESTED = "When the farm suggests"
TYPED = "At a time I type"
STANDARD = "Standard"
EACH_RANGE = "each Write node's own range"
EACH_WRITE = "each ticked Write node"
STALE = "Changed since the check: Check again before you submit."
PROGRESS_WORDS = {"hash": "Reading files", "upload": "Uploading"}


# --- the form, as knob specs -------------------------------------------------

@dataclass
class KnobSpec:
    """A knob to make, without making it: what a field draws as, so the
    mapping is tested without Nuke."""
    kind: str                      # the nuke knob class: "String_Knob", "Tab_Knob", ...
    name: str
    label: str = ""
    value: Any = None
    items: List[str] = field(default_factory=list)   # an Enumeration_Knob's choices
    tooltip: str = ""
    startline: bool = True
    range: Optional[Tuple[float, float]] = None
    param: str = ""                # the parameter it edits; '' for dividers and groups
    role: str = "value"            # value, linked, divider, begin, end


def knob_name(param: str) -> str:
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


def knob_value(f: Field, value: Any) -> Any:
    """A field's value as its knob holds it."""
    spec_kind = knob_kind(f)
    if spec_kind == "Boolean_Knob":
        try:
            return bool(f.param.coerce(value)) if value is not None else False
        except ValueError:
            return False
    if spec_kind in ("Int_Knob", "Double_Knob"):
        try:
            v = f.param.coerce(value) if value not in (None, "") else 0
        except ValueError:
            v = 0
        return int(v) if spec_kind == "Int_Knob" else float(v)
    if spec_kind == "Enumeration_Knob":
        items = [str(a) for a in f.param.allowed or []]
        s = _text(value)
        return s if s in items else (items[0] if items else "")
    return _text(value)


def knob_kind(f: Field) -> str:
    """The Nuke knob a field's control draws as."""
    c = f.control
    if c == "MULTILINE_EDIT":
        return "Multiline_Eval_String_Knob"
    if c == "DROPDOWN_LIST" and f.param.allowed:
        return "Enumeration_Knob"
    if c == "CHECK_BOX":
        return "Boolean_Knob"
    if c == "SPIN_BOX":
        return "Double_Knob" if f.param.type == "FLOAT" else "Int_Knob"
    if c.startswith("CHOOSE_"):
        return "File_Knob"
    return "String_Knob"


def field_specs(f: Field, shown: Any = None) -> List[KnobSpec]:
    """The knobs one field draws: its control, and a "linked" checkbox
    beside it when its value comes from the script. ``shown`` is what a
    linked field shows instead of its value (for one value per job)."""
    kind = knob_kind(f)
    tip = f.param.description
    if f.derivable:
        tip = (tip + "\n" if tip else "") + "Linked: follows the script until you type into it."
    value = shown if shown is not None and kind in ("String_Knob", "File_Knob") else knob_value(f, f.value)
    rng = None
    if kind in ("Int_Knob", "Double_Knob") and (f.param.min is not None or f.param.max is not None):
        rng = (f.param.min if f.param.min is not None else 0,
               f.param.max if f.param.max is not None else max(100, f.param.min or 0))
    out = [KnobSpec(kind, knob_name(f.name), f.label, value, tooltip=tip, range=rng, param=f.name,
                    items=[str(a) for a in f.param.allowed or []] if kind == "Enumeration_Knob" else [])]
    if f.derivable:
        out.append(KnobSpec("Boolean_Knob", linked_name(f.name), "linked", bool(f.linked), startline=False,
                            tooltip="On: follow the script. Typing into the field turns this off.",
                            param=f.name, role="linked"))
    return out


def form_specs(form: Form, shown: Optional[Callable[[Field], Any]] = None) -> List[KnobSpec]:
    """The knobs a tool's form draws, in its order: each ``groupLabel`` as
    a divider, ``show: advanced`` fields in a closed group."""
    out: List[KnobSpec] = []
    for i, (group, fields) in enumerate(form.groups()):
        plain = [f for f in fields if not f.advanced]
        folded = [f for f in fields if f.advanced]
        if plain:
            if group:
                out.append(KnobSpec("Text_Knob", f"g_{i}", group, role="divider"))
            for f in plain:
                out += field_specs(f, shown(f) if shown else None)
        if folded:
            label = group if group and not plain else (f"{group}: advanced" if group else "Advanced")
            out.append(KnobSpec("Tab_Knob", f"g_{i}_begin", label, role="begin"))
            for f in folded:
                out += field_specs(f, shown(f) if shown else None)
            out.append(KnobSpec("Tab_Knob", f"g_{i}_end", "", role="end"))
    return out


def make_knob(spec: KnobSpec):
    """A Nuke knob from a spec."""
    if spec.kind == "Tab_Knob":
        mode = nuke.TABBEGINCLOSEDGROUP if spec.role == "begin" else nuke.TABENDGROUP
        k = nuke.Tab_Knob(spec.name, spec.label, mode)
    elif spec.kind == "Text_Knob":
        k = nuke.Text_Knob(spec.name, spec.label, _text(spec.value))
    elif spec.kind == "Enumeration_Knob":
        k = nuke.Enumeration_Knob(spec.name, spec.label, list(spec.items))
        if spec.value in spec.items:
            k.setValue(spec.value)
    elif spec.kind == "Boolean_Knob":
        k = nuke.Boolean_Knob(spec.name, spec.label, bool(spec.value))
    elif spec.kind == "PyScript_Knob":
        k = nuke.PyScript_Knob(spec.name, spec.label, "")
    else:
        k = getattr(nuke, spec.kind)(spec.name, spec.label)
        if spec.value is not None:
            k.setValue(spec.value)
    if spec.range is not None and hasattr(k, "setRange"):
        try:
            k.setRange(spec.range[0], spec.range[1])
        except Exception:  # noqa: BLE001 - not every numeric knob takes a range
            pass
    if spec.tooltip:
        k.setTooltip(spec.tooltip)
    if spec.kind in ("Boolean_Knob", "PyScript_Knob", "Text_Knob", "Tab_Knob") or not spec.startline:
        (k.setFlag if spec.startline else k.clearFlag)(nuke.STARTLINE)
    return k


def knob_read(f: Field, knob) -> Any:
    """What the artist typed into a field's knob, as a value for the form."""
    v = knob.value()
    kind = knob_kind(f)
    if kind == "Boolean_Knob":
        return bool(v)
    if kind == "Int_Knob":
        return int(v)
    if kind == "Double_Knob":
        return float(v)
    return v


# --- the panel -----------------------------------------------------------------

class SubmitPanel(nukescripts.PythonPanel):
    """The panel. ``writes``, when given, are the only Write nodes ticked
    when it opens (the Write node's "Submit this Write to C4")."""

    def __init__(self, host: Optional[NukeHost] = None, farm: Optional[Farm] = None,
                 writes: Optional[List[str]] = None):
        nukescripts.PythonPanel.__init__(self, TITLE, "c4.submit")
        self.host = host or NukeHost()
        self._farm = farm
        self._only = list(writes) if writes is not None else None
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
        self._writes: Dict[str, str] = {}      # knob name -> Write's full name
        self._dynamic: List[Any] = []
        self.k: Dict[str, Any] = {}

        self._add(nuke.Enumeration_Knob("c4_tool", "Tool", [" "]))
        self._add(nuke.String_Knob("c4_project", "Project", self.host.project()))
        self._add(nuke.String_Knob("c4_shot", "Shot", self.host.shot()))
        self._add(nuke.Enumeration_Knob("c4_due", "Needed by", [SUGGESTED, TYPED]))
        t = self._add(nuke.String_Knob("c4_due_time", "", ""))
        t.clearFlag(nuke.STARTLINE)
        t.setTooltip('A time like "fri 18:00", "18:00" or "+3h", or a date like 2026-10-23.')
        f = self._add(nuke.String_Knob("c4_frames", "Frames", self.host.frames()))
        f.setTooltip("Frames to render, like 1001-1048 or 1001-1100:2. Linked, each Write node renders its own "
                     "range (or the script's); typing here sets one range for all of them.")
        lk = self._add(nuke.Boolean_Knob("c4_frames_linked", "linked", True))
        lk.clearFlag(nuke.STARTLINE)
        lk.setTooltip("On: each Write node's own range. Typing frames turns this off.")
        self._rebuild()
        self._set_summary("Reading the farm's tools...")

    # --- the farm -------------------------------------------------------------

    @property
    def farm(self) -> Farm:
        if self._farm is None:
            self._farm = Farm(client="c4submit-nuke")
        return self._farm

    # --- knobs ------------------------------------------------------------------

    def _add(self, knob, dynamic: bool = False):
        self.addKnob(knob)
        self.k[knob.name()] = knob
        if dynamic:
            self._dynamic.append(knob)
        return knob

    def _set(self, name: str, value: Any) -> None:
        k = self.k.get(name)
        if k is None:
            return
        self._updating += 1
        try:
            k.setValue(value)
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
        if d is None or not f.linked:
            return None
        if f.param.source == "scene.renderables" and len(d.picked()) > 1:
            return EACH_WRITE
        if f.param.source in ("scene.frames", "shot.frames") and d.per_renderable:
            ranges = {d.job_frames(r) for r in d.picked()}
            if len(ranges) > 1:
                return EACH_RANGE
        return None

    def _rebuild(self) -> None:
        """Re-draws everything below Frames: the Write nodes, Size, Note,
        the tool's parameters, the buttons and the summary."""
        for k in self._dynamic:
            self.removeKnob(k)
            self.k.pop(k.name(), None)
        self._dynamic = []
        self._writes = {}
        d = self.draft
        self._updating += 1
        try:
            if d is not None and d.per_renderable:
                self._add(nuke.Text_Knob("c4_writes", "Write nodes", ""), True)
                for i, r in enumerate(d.renderables):
                    label = r.name + "  " + (r.frames or "script range") + (f"  ({r.note})" if r.note else "")
                    k = self._add(nuke.Boolean_Knob(f"c4_write_{i}", label, r.name in d.ticked), True)
                    k.setFlag(nuke.STARTLINE)
                    k.setTooltip("Renders " + ", ".join(r.outputs) if r.outputs else "Renders this Write node")
                    self._writes[k.name()] = r.name
            elif d is not None and not d.renderables and any(p.source == "scene.renderables" for p in d.tool.params):
                self._add(nuke.Text_Knob("c4_writes", "Write nodes", "none in this script to render"), True)
            if d is not None and d.tool.sizes:
                self._add(nuke.Enumeration_Knob("c4_size", "Size", [STANDARD] + list(d.tool.sizes)), True)
                if d.size:
                    self.k["c4_size"].setValue(d.size)
            note = self._add(nuke.Multiline_Eval_String_Knob("c4_note", "Note", d.note if d else ""), True)
            note.setTooltip("Shown with the jobs on the farm, for whoever looks at them.")
            if d is not None:
                for spec in form_specs(d.form, self._shown):
                    self._add(make_knob(spec), True)
            self._add(nuke.Text_Knob("c4_buttons", ""), True)
            for i, (name, label) in enumerate((("c4_check", "Check"), ("c4_submit", "Submit"),
                                               ("c4_show_file", "Show file"), ("c4_save_file", "Save file..."))):
                b = self._add(nuke.PyScript_Knob(name, label, ""), True)
                (b.setFlag if i == 0 else b.clearFlag)(nuke.STARTLINE)
            self.k["c4_check"].setTooltip("Reads every file the script needs and asks the farm when it would be "
                                          "ready. Nothing is sent to render.")
            self.k["c4_submit"].setTooltip("Sends the files the farm lacks and starts the jobs. Check first.")
            self.k["c4_save_file"].setTooltip("Saves the submission as a file, to submit later with c4 farm submit.")
            s = self._add(nuke.Multiline_Eval_String_Knob("c4_summary", "", self._summary), True)
            s.setFlag(nuke.STARTLINE)
            if getattr(nuke, "READ_ONLY", None) is not None:
                s.setFlag(nuke.READ_ONLY)
        finally:
            self._updating -= 1
        self._sync()

    def _sync(self) -> None:
        """Brings every shown value and button in line with the draft."""
        d = self.draft
        self._updating += 1
        try:
            if d is not None:
                self.k["c4_frames"].setVisible(d.tool.takes_frames)
                self.k["c4_frames_linked"].setVisible(d.tool.takes_frames)
                self.k["c4_frames_linked"].setValue(d.frames is None)
                if d.frames is None:
                    if d.per_renderable:
                        ranges = {d.job_frames(r) for r in d.picked()} or {d.scene_frames}
                    else:
                        ranges = {d.job_frames(None)}
                    self.k["c4_frames"].setValue(ranges.pop() if len(ranges) == 1 else EACH_RANGE)
                for f in d.form.drawn():
                    k, lk = self.k.get(knob_name(f.name)), self.k.get(linked_name(f.name))
                    if lk is not None:
                        lk.setValue(bool(f.linked))
                    if k is not None and f.linked:
                        shown = self._shown(f)
                        k.setValue(shown if shown is not None and knob_kind(f) in ("String_Knob", "File_Knob")
                                   else knob_value(f, f.value))
            due = self.k["c4_due"]
            self.k["c4_due_time"].setEnabled(due.value() == TYPED)
            have = self.draft is not None and not self._busy
            ready = (have and self.checked is not None and self.checked.ok and self.submitted is None
                     and self.signature() == self._checked_sig)
            self.k["c4_check"].setEnabled(have)
            self.k["c4_submit"].setEnabled(ready)
            self.k["c4_show_file"].setEnabled(not self._busy and self.checked is not None)
            self.k["c4_save_file"].setEnabled(not self._busy and self.checked is not None)
            if not self._showing_file:
                self._show_summary()
        finally:
            self._updating -= 1

    # --- what changed -------------------------------------------------------------

    def signature(self) -> str:
        """Everything a check depends on, so a change since is seen: the
        draft's fields, and the script's file and unsaved state."""
        d = self.draft
        if d is None:
            return ""
        scene = d.scene_file
        try:
            st = os.stat(scene)
            stamp = [st.st_size, st.st_mtime_ns]
        except OSError:
            stamp = []
        state = {
            "tool": d.tool.name, "project": d.project, "shot": d.shot, "due": d.due, "frames": d.frames,
            "note": d.note, "size": d.size, "ticked": sorted(d.ticked),
            "form": {f.name: [f.value, f.linked] for f in d.form.fields},
            "renderables": [[r.name, r.frames, r.outputs, r.reads] for r in d.renderables],
            "scene": [scene, stamp, self.host.is_modified()],
        }
        return json.dumps(state, sort_keys=True, default=str)

    def knobChanged(self, knob) -> None:  # noqa: N802 - Nuke's name
        if self._updating:
            return
        name = knob.name()
        if name == "showPanel":
            if self.draft is not None:
                self._refresh_scene()
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
            if d is not None:
                d.project = knob.value().strip()
            self.load()
        elif name == "c4_tool":
            names = [self._tool_label(t) for t in self.tools]
            if knob.value() in names:
                self._use_tool(self.tools[names.index(knob.value())])
        elif d is None:
            pass
        elif name == "c4_shot":
            d.shot = knob.value().strip()
        elif name == "c4_due":
            labels = list(knob.values())
            i = labels.index(knob.value()) if knob.value() in labels else len(labels) - 2
            v = self._due_values[i]
            d.due = v if v else (self.k["c4_due_time"].value().strip() or "suggested")
        elif name == "c4_due_time":
            typed = knob.value().strip()
            self._set("c4_due", TYPED)
            d.due = typed or "suggested"
        elif name == "c4_frames":
            typed = knob.value().strip()
            d.frames = typed if typed and typed != EACH_RANGE else None
        elif name == "c4_frames_linked":
            if knob.value():
                d.frames = None
            else:
                shown = self.k["c4_frames"].value().strip()
                d.frames = shown if shown and shown != EACH_RANGE else d.scene_frames
        elif name in self._writes:
            d.tick(self._writes[name], bool(knob.value()))
        elif name == "c4_size":
            d.size = "" if knob.value() == STANDARD else knob.value()
        elif name == "c4_note":
            d.note = knob.value().strip()
        elif name.startswith("p_") and name.endswith("__linked"):
            d.form.link(name[2:-len("__linked")], bool(knob.value()))
        elif name.startswith("p_"):
            try:
                f = d.form.field(name[2:])
            except KeyError:
                return
            d.form.set(f.name, knob_read(f, knob))
        self._sync()

    # --- the farm's tools and the project ------------------------------------------

    def _tool_label(self, t: Tool) -> str:
        """A tool by its title, with its name when another has the same title."""
        same = [x for x in self.tools if x is not t and x.title == t.title]
        return f"{t.title} ({t.name})" if same else t.title

    def load(self) -> None:
        """Reads the project's milestones and the tools that run Nuke
        scripts, off the main thread."""
        project = self.k["c4_project"].value().strip()
        scene = self.host.scene_file() or "untitled.nk"
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
                    err = f"No tool on the farm renders Nuke scripts for {project}."
            except NotOnFarm as e:
                err = f"{e.message}, so this panel has nothing to fill in."
            except FarmError as e:
                hint = e.status in (0, 401, 403) and "C4_HUD" not in e.message
                err = e.message + (" (check C4_HUD and your sign-in)" if hint else "")
            except Exception as e:  # noqa: BLE001 - anything else is reported, never raised into Nuke
                err = f"could not read the farm: {e}"
            nuke.executeInMainThread(self._loaded, args=(project, info, tools, err))

        self._spawn(work)

    def _loaded(self, project: str, info: Optional[dict], tools: List[Tool], err: str) -> None:
        if project != self.k["c4_project"].value().strip():
            return  # the artist typed another project meanwhile; its own load answers
        self.project_info = info
        self.tools = tools
        choices = Submitter(self._farm).due_choices(info)
        self._due_values = [c["value"] for c in choices[:-1]] + ["suggested", ""]
        labels = [c["label"] for c in choices[:-1]] + [SUGGESTED, TYPED]
        due = self.k["c4_due"]
        current = self.draft.due if self.draft else "suggested"
        self._updating += 1
        try:
            due.setValues(labels)
            if current in self._due_values[:-1]:
                due.setValue(labels[self._due_values.index(current)])
            else:
                due.setValue(TYPED)
                self.k["c4_due_time"].setValue(current)
            self.k["c4_tool"].setValues([self._tool_label(t) for t in tools] or [" "])
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
        d.project = self.k["c4_project"].value().strip()
        d.shot = self.k["c4_shot"].value().strip()
        if old is not None:
            d.due, d.frames, d.note = old.due, old.frames, old.note
            d.ticked = [r.name for r in d.renderables if r.name in old.ticked]
        else:
            due = self.k["c4_due"]
            i = list(due.values()).index(due.value()) if due.value() in list(due.values()) else 0
            v = self._due_values[i] if i < len(self._due_values) else "suggested"
            d.due = v or (self.k["c4_due_time"].value().strip() or "suggested")
        if self._only is not None:
            d.ticked = [r.name for r in d.renderables if r.name in self._only]
            self._only = None
        self.draft = d
        self.checked = None
        self.submitted = None
        self._rebuild()

    def _refresh_scene(self, later: bool = False) -> None:
        """Re-reads the script (main thread), re-drawing the Write nodes
        when they changed. ``later`` re-draws after the current callback,
        because a button's own callback must not remove the button."""
        d = self.draft
        before = [(r.name, r.frames, r.note) for r in d.renderables]
        d.refresh()
        if [(r.name, r.frames, r.note) for r in d.renderables] == before:
            self._sync()
        elif later:
            nuke.executeInMainThread(self._rebuild)
        else:
            self._rebuild()

    # --- work off the main thread -------------------------------------------------------

    def _spawn(self, fn: Callable[[], None]) -> threading.Thread:
        t = threading.Thread(target=fn, name="c4-submit", daemon=True)
        with self._lock:
            self._threads = [x for x in self._threads if x.is_alive()] + [t]
        t.start()
        return t

    def wait(self, timeout: Optional[float] = None) -> None:
        """Waits for the panel's work in flight (for tests and scripts)."""
        while True:
            with self._lock:
                alive = [t for t in self._threads if t.is_alive()]
            if not alive:
                return
            for t in alive:
                t.join(timeout)

    def _progress(self, task) -> Callable[[str, int, int], None]:
        def tick(stage: str, done: int, total: int) -> None:
            word = PROGRESS_WORDS.get(stage, stage)
            if stage == "upload":
                task.setMessage(f"{word} {summarylib.size(done)} of {summarylib.size(total)}")
            else:
                task.setMessage(f"{word}: {done:,} of {total:,}")
            task.setProgress(int(100 * done / total) if total else 100)
        return tick

    def _busy_with(self, text: str) -> None:
        self._busy = True
        self._set_summary(text)
        self._sync()

    # --- Check ----------------------------------------------------------------------------

    def check(self) -> bool:
        """Reads the script, then on a worker thread hashes its files and
        asks the farm to check the submission. False when it did not start."""
        if self._busy or self.draft is None:
            return False
        self._refresh_scene(later=True)
        try:
            prepared = prepare(self.draft)
        except Exception as e:  # noqa: BLE001 - a script Nuke cannot read reports, never raises
            self._set_summary(f"Could not read the script: {e}")
            return False
        self.checked, self.submitted = None, None
        sig = self.signature()
        farm = self.farm if self._farm is not None else None
        self._busy_with("Checking with the farm...")

        def work():
            task = nuke.ProgressTask("Checking with the farm")
            result: Tuple[Optional[Checked], str] = (None, "")
            try:
                s = Submitter(farm or self.farm, progress=self._progress(task),
                              cancelled=lambda: task.isCancelled())
                result = (s.check(prepared), "")
            except Cancelled:
                result = (None, "Check cancelled.")
            except FarmError as e:
                result = (None, e.message)
            except Exception as e:  # noqa: BLE001
                result = (None, f"The check failed: {e}")
            finally:
                del task
            nuke.executeInMainThread(self._check_done, args=(result, sig))

        self._spawn(work)
        return True

    def _check_done(self, result: Tuple[Optional[Checked], str], sig: str) -> None:
        checked, err = result
        self._busy = False
        if checked is None:
            self.checked = None
            self._set_summary(err)
            self._sync()
            return
        self.checked, self._checked_sig = checked, sig
        text = summarylib.text(checked)
        if checked.ok:
            text += "\n\nReady: press Submit to send it to the farm."
        elif not checked.errors:
            text += ("\n\nThe farm could not check this submission. Save file keeps it, "
                     "to submit with c4 farm submit when the farm takes it.")
        else:
            text += "\n\nFix what is listed and Check again."
        self._set_summary(text)
        self._sync()

    # --- Submit ---------------------------------------------------------------------------

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

        def work():
            task = nuke.ProgressTask("Submitting to the farm")
            result: Tuple[Optional[Submitted], str] = (None, "")
            try:
                s = Submitter(farm, progress=self._progress(task), cancelled=lambda: task.isCancelled())
                result = (s.submit(c), "")
            except Cancelled:
                result = (None, "Submit cancelled: nothing was started.")
            except FarmError as e:
                result = (None, "Not submitted: " + e.message + _farm_errors(e))
            except Exception as e:  # noqa: BLE001
                result = (None, f"Not submitted: {e}")
            finally:
                del task
            nuke.executeInMainThread(self._submit_done, args=(result,))

        self._spawn(work)
        return True

    def _submit_done(self, result: Tuple[Optional[Submitted], str]) -> None:
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

    # --- the file ---------------------------------------------------------------------------

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
            path = nuke.getFilename("Save the submission as", "*.yaml",
                                    default=os.path.join(folder, default_filename(c)) if folder else default_filename(c),
                                    type="save")
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
                          f"\n\n{summarylib.text(c)}")
        return path


def _farm_errors(e: FarmError) -> str:
    body = e.body
    errs = body if isinstance(body, list) else (body.get("errors") if isinstance(body, dict) else None)
    if not errs:
        return ""
    return "\n" + "\n".join(f"  {x.get('path', '')}: {x.get('message', '')}" for x in errs if isinstance(x, dict))


# --- opening it -------------------------------------------------------------------------------

_open: List[SubmitPanel] = []


def open_panel(writes: Optional[List[str]] = None, host: Optional[NukeHost] = None, farm: Optional[Farm] = None,
               show: bool = True) -> SubmitPanel:
    """Opens the panel and starts reading the farm's tools. Kept referenced
    here, because Nuke closes a panel Python has let go of."""
    p = SubmitPanel(host=host, farm=farm, writes=writes)
    _open.append(p)
    p.load()
    if show:
        p.show()
    return p


def submit_selected_writes(host: Optional[NukeHost] = None, farm: Optional[Farm] = None,
                           show: bool = True) -> Optional[SubmitPanel]:
    """The Write node's "Submit this Write to C4": the panel with only the
    selected Write nodes ticked."""
    writes = [n.fullName() for n in nuke.selectedNodes() if is_write(n)]
    if not writes:
        nuke.message("Select a Write node first, then choose Submit this Write to C4.")
        return None
    return open_panel(writes=writes, host=host, farm=farm, show=show)
