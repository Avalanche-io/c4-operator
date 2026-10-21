"""The C4 Farm panel, in Render Properties.

Top to bottom: Tool, Project, Shot, Needed by, Frames, one checkbox per
view layer, Size, Note, the tool's own parameters (advanced ones folded
away), then Check, Submit, Show file and Save file, and the summary the
check returns.

The artist's choices live in a property group on the scene
(``scene.c4_farm``), so they are saved with the file. The c4submit draft
they describe lives in a ``Controller`` per scene, in Python only.
Blender's data is read and written on the main thread alone: ``prepare``
reads the file there, then the check or the submission (hashing,
uploads, the farm) runs on a worker thread, and a ``bpy.app.timers``
timer brings its answer back to the main thread. Check and Submit started
from their buttons stay modal while the work runs, so Esc cancels it;
the rest of Blender stays usable.

"Needed by" is the only urgency the artist sets. Submit is available only
after a check passed and nothing has changed since, in the panel or in
the file.
"""

import json
import os
import textwrap
import threading
from contextlib import contextmanager
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Tuple

import bpy
from bpy.props import (BoolProperty, CollectionProperty, EnumProperty, FloatProperty, IntProperty, PointerProperty,
                       StringProperty)

from c4submit import summary as summarylib
from c4submit.collect import Cancelled
from c4submit.farm import Farm, FarmError, NotOnFarm
from c4submit.form import Field, Form
from c4submit.submitter import Checked, Draft, Submitted, Submitter, default_filename, prepare, save
from c4submit.tool import Tool

from .host import BlenderHost

TITLE = "C4 Farm"
SUGGESTED = "When the farm suggests"
TYPED = "At a time I type"
STANDARD = "Standard"
EACH_RANGE = "each view layer's own range"
EACH_LAYER = "each ticked view layer"
STALE = "Changed since the check: Check again before you submit."
LOCAL_TOOLS = ("Setting C4_TOOL_PATH to the studio's tool folders lets this panel draw the form and save "
               "the submission as a file, to submit when the farm takes it.")
PROGRESS_WORDS = {"hash": "Reading files", "upload": "Uploading"}
ICONS = {"error": "ERROR", "warn": "INFO"}

#: How the panel reaches the farm; tests replace it.
farm_factory: Callable[[], Farm] = lambda: Farm(client="c4submit-blender")  # noqa: E731


# --- the form, as widget specs ---------------------------------------------------

#: The field item's property each kind of widget draws.
ATTRS = {"text": "string_value", "multiline": "string_value", "menu": "choice", "path": "path_value",
         "folder": "folder_value", "int": "int_value", "float": "float_value", "bool": "bool_value"}


@dataclass
class WidgetSpec:
    """A widget to draw, without drawing it: what a field draws as, so the
    mapping is tested without Blender."""
    kind: str                       # heading, text, multiline, menu, bool, int, float, path, folder
    param: str = ""                 # the parameter it edits; '' for a heading
    label: str = ""
    value: Any = None
    options: List[str] = field(default_factory=list)   # a menu's choices
    linked: Optional[bool] = None   # None: not taken from the file, so no link toggle
    advanced: bool = False
    tooltip: str = ""

    @property
    def attr(self) -> str:
        """The field item's property drawn."""
        return ATTRS.get(self.kind, "")

    @property
    def value_attr(self) -> str:
        """The field item's property that holds the value (a menu's choice
        is kept as text)."""
        return "string_value" if self.kind == "menu" else self.attr


def widget_kind(f: Field) -> str:
    c = f.control
    if c == "MULTILINE_EDIT":
        return "multiline"
    if c == "DROPDOWN_LIST" and f.param.allowed:
        return "menu"
    if c == "CHECK_BOX":
        return "bool"
    if c == "SPIN_BOX":
        return "float" if f.param.type == "FLOAT" else "int"
    if c == "CHOOSE_DIRECTORY":
        return "folder"
    if c.startswith("CHOOSE_"):
        return "path"
    return "text"


def _text(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, (list, tuple)):
        return ", ".join(str(v) for v in value)
    if isinstance(value, bool):
        return "on" if value else "off"
    return str(value)


def widget_value(f: Field, value: Any) -> Any:
    """A field's value as its widget holds it."""
    kind = widget_kind(f)
    if kind == "bool":
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
    if kind == "menu":
        items = [str(a) for a in f.param.allowed or []]
        s = _text(value)
        return s if s in items else (items[0] if items else "")
    return _text(value)


def field_spec(f: Field, shown: Any = None) -> WidgetSpec:
    """The widget one field draws. ``shown`` is what a linked field shows
    instead of its value (when it has one value per job)."""
    kind = widget_kind(f)
    tip = f.param.description
    if f.derivable:
        tip = (tip + "\n" if tip else "") + "Linked: follows the file until you type into it."
    value = shown if shown is not None and kind in ("text", "path") else widget_value(f, f.value)
    return WidgetSpec(kind, f.name, f.label, value, options=[str(a) for a in f.param.allowed or []] if kind == "menu" else [],
                      linked=bool(f.linked) if f.derivable else None, advanced=f.advanced, tooltip=tip)


def form_specs(form: Form, shown: Optional[Callable[[Field], Any]] = None) -> Tuple[List[WidgetSpec], List[WidgetSpec]]:
    """The widgets a tool's form draws, in its order: (the artist's, the
    advanced ones for the folded sub-panel). Each ``groupLabel`` is a
    heading."""
    artist: List[WidgetSpec] = []
    advanced: List[WidgetSpec] = []
    for group, fields in form.groups():
        plain = [f for f in fields if not f.advanced]
        folded = [f for f in fields if f.advanced]
        if plain:
            if group:
                artist.append(WidgetSpec("heading", label=group))
            artist += [field_spec(f, shown(f) if shown else None) for f in plain]
        if folded:
            if group and group != "Advanced":
                advanced.append(WidgetSpec("heading", label=group))
            advanced += [field_spec(f, shown(f) if shown else None) for f in folded]
    return artist, advanced


def item_value(item) -> Any:
    """What the artist typed into a field's widget, as a value for the form."""
    k = item.kind
    if k == "bool":
        return bool(item.bool_value)
    if k == "int":
        return int(item.int_value)
    if k == "float":
        return float(item.float_value)
    if k == "path":
        return item.path_value
    if k == "folder":
        return item.folder_value
    return item.string_value


def layer_label(r) -> str:
    return r.label + "  " + (r.frames or "") + (f"  ({r.note})" if r.note else "")


def wrap(text: str, width: int) -> List[str]:
    """A summary line in rows that fit the panel."""
    lead = len(text) - len(text.lstrip(" "))
    return textwrap.wrap(text, max(16, width), break_long_words=True, break_on_hyphens=False,
                         initial_indent="", subsequent_indent=" " * (lead + 2)) or [""]


# --- the panel's state, on the scene ----------------------------------------------------

_controllers: Dict[int, "Controller"] = {}
_hush = [0]
_ITEMS: Dict[tuple, list] = {}


@contextmanager
def _hushed():
    """Writes the panel makes itself, which are not the artist's edits."""
    _hush[0] += 1
    try:
        yield
    finally:
        _hush[0] -= 1


def _put(obj, attr: str, value: Any) -> None:
    """Sets a property only when it changes, without its update running."""
    if getattr(obj, attr) != value:
        with _hushed():
            setattr(obj, attr, value)


def _owner(obj, context) -> Optional["Controller"]:
    scene = getattr(obj, "id_data", None) or getattr(context, "scene", None)
    return _controllers.get(scene.as_pointer()) if scene is not None else None


def _on(what: str):
    def update(self, context):
        if _hush[0]:
            return
        c = _owner(self, context)
        if c is not None:
            c.changed(what, self)
    return update


def _keep(items: list) -> list:
    # Blender reads a dynamic enum's strings after the callback returns, so
    # Python must keep them alive.
    return _ITEMS.setdefault(tuple(items), items)


def _number(items, ident: str, default: int = 0) -> int:
    for t in items:
        if t[0] == ident:
            return t[3]
    return default


def _ident(items, number: int) -> str:
    for t in items:
        if t[3] == number:
            return t[0]
    return ""


def _tool_items(self, context):
    c = _owner(self, context)
    tools = c.tools if c is not None else []
    items = [(t.name, c.tool_label(t), t.summary or t.title, i) for i, t in enumerate(tools)]
    return _keep(items or [("NONE", "None yet", "The farm lists no tool for this project yet", 0)])


def _tool_get(self):
    return _number(_tool_items(self, bpy.context), self.tool_name)


def _tool_set(self, n):
    ident = _ident(_tool_items(self, bpy.context), n)
    if ident and ident != "NONE":
        self.tool_name = ident


def _due_items(self, context):
    c = _owner(self, context)
    choices = c.due_choices if c is not None else Submitter.due_choices(None)
    items = [(ch["value"], ch["label"], "The project's milestone" if ch["value"] != "suggested"
              else "The farm picks the time from the shot's schedule", i) for i, ch in enumerate(choices)]
    items.append(("typed", TYPED, 'A time like "fri 18:00", "18:00" or "+3h", or a date like 2026-10-23', len(items)))
    return _keep(items)


def _due_get(self):
    items = _due_items(self, bpy.context)
    return _number(items, self.due_value or "suggested", _number(items, "suggested"))


def _due_set(self, n):
    self.due_value = _ident(_due_items(self, bpy.context), n) or "suggested"


def _size_items(self, context):
    c = _owner(self, context)
    sizes = list(c.draft.tool.sizes) if c is not None and c.draft is not None else []
    items = [("_standard", STANDARD, "The tool's usual size", 0)]
    items += [(s, s, f"The tool's {s} size", i + 1) for i, s in enumerate(sizes)]
    return _keep(items)


def _size_get(self):
    return _number(_size_items(self, bpy.context), self.size_value or "_standard")


def _size_set(self, n):
    ident = _ident(_size_items(self, bpy.context), n)
    self.size_value = "" if ident in ("", "_standard") else ident


def _choice_items(self, context):
    opts = [o for o in self.options.split("\n") if o]
    return _keep([(o, o, "", i) for i, o in enumerate(opts)] or [("NONE", "", "", 0)])


def _choice_get(self):
    return _number(_choice_items(self, bpy.context), self.string_value)


def _choice_set(self, n):
    ident = _ident(_choice_items(self, bpy.context), n)
    if ident and ident != "NONE":
        self.string_value = ident


class C4_PG_layer(bpy.types.PropertyGroup):  # noqa: N801 - Blender's naming
    name: StringProperty(name="View layer")
    label: StringProperty(name="Label")
    ticked: BoolProperty(name="Render", description="Render this view layer on the farm, as a job of its own",
                         default=True, update=_on("layer"))


class C4_PG_field(bpy.types.PropertyGroup):  # noqa: N801
    name: StringProperty(name="Parameter")
    label: StringProperty(name="Label")
    kind: StringProperty(name="Kind")
    options: StringProperty(name="Choices")
    advanced: BoolProperty(name="Advanced")
    derivable: BoolProperty(name="From the file")
    typed: BoolProperty(name="Typed")
    string_value: StringProperty(name="Value", update=_on("field"))
    path_value: StringProperty(name="File", subtype="FILE_PATH", update=_on("field"))
    folder_value: StringProperty(name="Folder", subtype="DIR_PATH", update=_on("field"))
    int_value: IntProperty(name="Value", update=_on("field"))
    float_value: FloatProperty(name="Value", update=_on("field"))
    bool_value: BoolProperty(name="Value", update=_on("field"))
    choice: EnumProperty(name="Value", items=_choice_items, get=_choice_get, set=_choice_set)
    linked: BoolProperty(name="Linked", description="On: follows the file. Typing into the field turns this off",
                         update=_on("linked"))


class C4_PG_settings(bpy.types.PropertyGroup):  # noqa: N801
    tool_name: StringProperty(name="Tool", update=_on("tool"))
    tool: EnumProperty(name="Tool", description="What renders the file on the farm", items=_tool_items,
                       get=_tool_get, set=_tool_set)
    project: StringProperty(name="Project", description="The farm project the shot belongs to",
                            update=_on("project"))
    shot: StringProperty(name="Shot", description="The shot the frames are for", update=_on("shot"))
    due_value: StringProperty(name="Needed by", default="suggested", update=_on("due"))
    due: EnumProperty(name="Needed by", description="When the frames are needed: the farm works out the rest",
                      items=_due_items, get=_due_get, set=_due_set)
    due_time: StringProperty(name="Time", description='A time like "fri 18:00", "18:00" or "+3h", or a date like '
                             '2026-10-23', update=_on("due_time"))
    frames: StringProperty(name="Frames", description="Frames to render, like 1001-1048 or 1001-1100:2. Linked, "
                           "each view layer renders its scene's range; typing here sets one range for all of them",
                           update=_on("frames"))
    frames_linked: BoolProperty(name="Linked", default=True, description="On: each view layer renders its scene's "
                                "frame range. Typing frames turns this off", update=_on("frames_linked"))
    layers: CollectionProperty(type=C4_PG_layer, name="View layers")
    size_value: StringProperty(name="Size", update=_on("size"))
    size: EnumProperty(name="Size", description="How big a machine each frame needs, as the tool names them",
                       items=_size_items, get=_size_get, set=_size_set)
    note: StringProperty(name="Note", description="Shown with the jobs on the farm, for whoever looks at them",
                         update=_on("note"))
    fields: CollectionProperty(type=C4_PG_field, name="Parameters")
    fields_tool: StringProperty(name="Parameters of")


# --- work off the main thread ------------------------------------------------------------

class _Job:
    """Work on a worker thread: its progress, its cancel flag, its answer."""

    def __init__(self, title: str):
        self.title = title
        self.cancel = threading.Event()
        self.lock = threading.Lock()
        self.progress: Optional[Tuple[str, int, int]] = None
        self.result: Any = None
        self.thread: Optional[threading.Thread] = None

    def tick(self, stage: str, done: int, total: int) -> None:
        with self.lock:
            self.progress = (stage, done, total)


# --- the controller ----------------------------------------------------------------------

Row = Tuple[str, str, str]   # (label, text, level): a summary row


class Controller:
    """The panel for one scene: its draft, the farm's tools and the last
    check. Every method runs on the main thread."""

    def __init__(self, scene):
        self.key = scene.as_pointer()
        self.host = BlenderHost(scene=scene.name)
        self._farm: Optional[Farm] = None
        self.tools: List[Tool] = []
        self.project_info: Optional[dict] = None
        self.due_choices = Submitter.due_choices(None)
        self.draft: Optional[Draft] = None
        self.checked: Optional[Checked] = None
        self.submitted: Optional[Submitted] = None
        self.checked_sig = ""
        self.rows: List[Row] = [("", "Reading the farm's tools...", "info")]
        self.message = ""
        self.showing_file = False
        self.busy = False
        self.loading = ""
        self.jobs: List[_Job] = []
        self._seen: Optional[tuple] = None
        self._refresh_due = False

    # --- the scene and the farm ---------------------------------------------

    def scene(self):
        for s in bpy.data.scenes:
            if s.as_pointer() == self.key:
                return s
        return None

    @property
    def settings(self):
        s = self.scene()
        return s.c4_farm if s is not None else None

    @property
    def farm(self) -> Farm:
        if self._farm is None:
            self._farm = farm_factory()
        return self._farm

    def tool_label(self, t: Tool) -> str:
        """A tool by its title, with its name when another has the same title."""
        same = [x for x in self.tools if x is not t and x.title == t.title]
        return f"{t.title} ({t.name})" if same else t.title

    def _say(self, text: str, level: str = "info") -> None:
        self.message = text
        self.rows = [("", line, level) for line in text.split("\n")]
        self.showing_file = False
        self._redraw()

    def _redraw(self) -> None:
        wm = getattr(bpy.context, "window_manager", None)
        for w in getattr(wm, "windows", None) or []:
            for a in w.screen.areas:
                if a.type == "PROPERTIES":
                    a.tag_redraw()

    # --- opening -------------------------------------------------------------

    def start(self) -> None:
        """Fills in Project and Shot from the studio's environment (else
        what the file kept), then reads the farm's tools."""
        s = self.settings
        if s is None:
            return
        self.host.scene_name = self.scene().name
        _put(s, "project", self.host.project() or s.project)
        _put(s, "shot", self.host.shot() or s.shot)
        self.load()

    def load(self) -> None:
        """Reads the project's milestones and the tools that render Blender
        files, off the main thread."""
        s = self.settings
        project = s.project.strip()
        if not project:
            self._say("Which project is this for? Fill in Project, or set C4_PROJECT in the studio's environment.")
            return
        self.loading = project
        self._say(f"Reading {project}'s tools from the farm...")
        scene_file = self.host.scene_file() or "untitled.blend"

        def work(job):
            info, tools, err = None, [], ""
            try:
                farm = self.farm
                info = farm.project(project)
                tools = Submitter(farm).tools_for(project, scene_file)
                if not tools:
                    err = f"No tool on the farm renders Blender files for {project}. " + LOCAL_TOOLS
            except NotOnFarm as e:
                err = f"{e.message}, so this panel has no form to draw. " + LOCAL_TOOLS
            except FarmError as e:
                hint = e.status in (0, 401, 403) and "C4_HUD" not in e.message
                err = e.message + (" (check C4_HUD and your sign-in)" if hint else "")
            except Exception as e:  # noqa: BLE001 - reported, never raised into Blender
                err = f"could not read the farm: {e}"
            return info, tools, err

        self._run(work, lambda r: self._loaded(project, *r))

    def _loaded(self, project: str, info: Optional[dict], tools: List[Tool], err: str) -> None:
        s = self.settings
        if s is None or project != s.project.strip():
            return  # the artist typed another project meanwhile; its own load answers
        self.loading = ""
        self.project_info = info
        self.tools = tools
        self.due_choices = Submitter.due_choices(info)
        values = [c["value"] for c in self.due_choices] + ["typed"]
        if (s.due_value or "suggested") not in values:
            _put(s, "due_value", "suggested")
        if err or not tools:
            self.draft = self.checked = self.submitted = None
            self._say(err, "error")
            return
        keep = s.tool_name or (self.draft.tool.name if self.draft else "")
        self.use_tool(next((t for t in tools if t.name == keep), tools[0]))
        self._say("Check asks the farm when this would be ready. Nothing renders until you Submit.")

    def use_tool(self, tool: Tool) -> None:
        """A draft for the tool, with what the artist chose in the panel
        (kept in the file) put back."""
        s = self.settings
        d = Draft(self.host, tool, project=self.project_info)
        d.project, d.shot, d.note = s.project.strip(), s.shot.strip(), s.note.strip()
        d.due = self._due()
        d.size = s.size_value if s.size_value in tool.sizes else ""
        d.frames = None if s.frames_linked else (s.frames.strip() or None)
        if len(s.layers):
            listed = {it.name: it.ticked for it in s.layers}
            d.ticked = [r.name for r in d.renderables if listed.get(r.name, r.enabled)]
        if s.fields_tool == tool.name:
            for it in s.fields:
                try:
                    f = d.form.field(it.name)
                except KeyError:
                    continue
                if it.typed:
                    d.form.set(it.name, item_value(it))
                if f.derivable:
                    d.form.link(it.name, bool(it.linked))
        self.draft = d
        self.checked = self.submitted = None
        self.showing_file = False
        _put(s, "tool_name", tool.name)
        _put(s, "size_value", d.size)
        self._rebuild_fields()
        self._rebuild_layers()
        self._seen = self._snapshot()
        self.sync()

    # --- keeping the panel and the draft in step ---------------------------------

    def _due(self) -> str:
        s = self.settings
        v = s.due_value or "suggested"
        if v == "typed":
            return s.due_time.strip() or "suggested"
        return v

    def changed(self, what: str, obj) -> None:
        """An edit the artist made in the panel."""
        s, d = self.settings, self.draft
        if what == "project":
            if d is not None:
                d.project = s.project.strip()
            self.load()
            return
        if what == "tool":
            t = next((t for t in self.tools if t.name == s.tool_name), None)
            if t is not None and (d is None or t.name != d.tool.name):
                self.use_tool(t)
            return
        if d is None:
            return
        if what == "shot":
            d.shot = s.shot.strip()
        elif what == "due":
            d.due = self._due()
        elif what == "due_time":
            _put(s, "due_value", "typed")
            d.due = self._due()
        elif what == "frames":
            typed = s.frames.strip()
            d.frames = typed if typed and typed != EACH_RANGE else None
        elif what == "frames_linked":
            if s.frames_linked:
                d.frames = None
            else:
                shown = s.frames.strip()
                d.frames = shown if shown and shown != EACH_RANGE else d.scene_frames
        elif what == "layer":
            d.tick(obj.name, bool(obj.ticked))
        elif what == "size":
            d.size = s.size_value
        elif what == "note":
            d.note = s.note.strip()
        elif what == "field":
            d.form.set(obj.name, item_value(obj))
        elif what == "linked":
            d.form.link(obj.name, bool(obj.linked))
        self.sync()

    def _shown(self, f: Field) -> Any:
        """What a linked field shows when its value is one per job."""
        d = self.draft
        if d is None or not f.linked or not f.param.source or not d.per_renderable:
            return None
        if f.param.source in ("scene.frames", "shot.frames"):
            return EACH_RANGE if len({d.job_frames(r) for r in d.picked()}) > 1 else None
        # One value for every ticked view layer shows as itself (the form
        # holds the first one's); several show as "each ticked view layer".
        per_job = {json.dumps(d._derive(f.param.source, f.name, r), default=str) for r in d.picked()}
        return EACH_LAYER if len(per_job) > 1 else None

    def _rebuild_fields(self) -> None:
        s, d = self.settings, self.draft
        artist, advanced = form_specs(d.form, self._shown)
        with _hushed():
            s.fields.clear()
            for spec in artist + advanced:
                if spec.kind == "heading":
                    continue
                it = s.fields.add()
                it.name, it.label, it.kind = spec.param, spec.label, spec.kind
                it.advanced, it.derivable = spec.advanced, spec.linked is not None
                it.linked = bool(spec.linked)
                it.options = "\n".join(spec.options)
                it.typed = d.form.field(spec.param).touched
                setattr(it, spec.value_attr, spec.value)
            s.fields_tool = d.tool.name

    def _rebuild_layers(self) -> None:
        s, d = self.settings, self.draft
        want = [(r.name, layer_label(r)) for r in d.renderables]
        if want == [(it.name, it.label) for it in s.layers]:
            return
        with _hushed():
            s.layers.clear()
            for r in d.renderables:
                it = s.layers.add()
                it.name, it.label, it.ticked = r.name, layer_label(r), r.name in d.ticked

    def sync(self) -> None:
        """Brings every value the panel shows in line with the draft."""
        s, d = self.settings, self.draft
        if d is None or s is None:
            return
        _put(s, "frames_linked", d.frames is None)
        if d.frames is None:
            if d.per_renderable:
                ranges = {d.job_frames(r) for r in d.picked()} or {d.scene_frames}
            else:
                ranges = {d.job_frames(None)}
            _put(s, "frames", ranges.pop() if len(ranges) == 1 else EACH_RANGE)
        for it in s.layers:
            _put(it, "ticked", it.name in d.ticked)
        for it in s.fields:
            try:
                f = d.form.field(it.name)
            except KeyError:
                continue
            spec = field_spec(f, self._shown(f))
            if f.derivable:
                _put(it, "linked", bool(f.linked))
            if f.derivable and f.linked:
                _put(it, spec.value_attr, spec.value)
            _put(it, "typed", f.touched)
        self._redraw()

    def _snapshot(self) -> tuple:
        """What the panel's view layers are drawn from, cheaply."""
        return (bpy.data.filepath,) + tuple(
            (s.name, s.frame_start, s.frame_end, s.frame_step) + tuple((vl.name, bool(vl.use)) for vl in s.view_layers)
            for s in bpy.data.scenes)

    def watch(self) -> None:
        """Called as the panel draws: when the file's view layers or frame
        ranges changed, re-reads them on a timer (a draw may not write)."""
        if self.draft is None or self.busy or self._refresh_due or self._snapshot() == self._seen:
            return
        self._refresh_due = True

        def later():
            self._refresh_due = False
            if _controllers.get(self.key) is self and self.draft is not None and not self.busy:
                self.refresh_scene()
            return None

        bpy.app.timers.register(later, first_interval=0.0)

    def refresh_scene(self) -> None:
        """Re-reads the file: its view layers, frames and derived values."""
        if self.draft is None:
            return
        self.host.scene_name = self.scene().name
        self.draft.refresh()
        self._rebuild_layers()
        self._seen = self._snapshot()
        self.sync()

    # --- what changed since the check ----------------------------------------------

    def signature(self) -> str:
        """Everything a check depends on: the draft's signature, which holds
        the host's (the file's path and unsaved edits) as of the last
        refresh, and the host's now, since Blender tells the panel of no
        edit: a panel draws often, and re-reading the file on every draw
        would cost more than this."""
        if self.draft is None:
            return ""
        return self.draft.signature() + "\nnow=" + self.host.signature()

    def stale(self) -> bool:
        return self.checked is not None and self.submitted is None and self.signature() != self.checked_sig

    def ready(self) -> bool:
        return (self.draft is not None and not self.busy and self.checked is not None and self.checked.ok
                and self.submitted is None and self.signature() == self.checked_sig)

    # --- running work ----------------------------------------------------------------

    def _run(self, work: Callable[[_Job], Any], done: Callable[[Any], None], title: str = "") -> _Job:
        """Runs ``work`` on a worker thread and ``done`` with its answer on
        the main thread, from a timer that polls for it."""
        job = _Job(title)

        def target():
            job.result = work(job)

        job.thread = threading.Thread(target=target, name="c4-farm", daemon=True)
        self.jobs.append(job)
        if title:
            bpy.context.window_manager.progress_begin(0, 100)
        job.thread.start()

        def poll():
            if job.thread.is_alive():
                if title:
                    self._show_progress(job)
                return 0.1
            if job in self.jobs:
                self.jobs.remove(job)
            if title:
                bpy.context.window_manager.progress_end()
            if _controllers.get(self.key) is self:   # not when another file was opened meanwhile
                done(job.result)
                self._redraw()
            return None

        bpy.app.timers.register(poll, first_interval=0.1)
        return job

    def _show_progress(self, job: _Job) -> None:
        with job.lock:
            p = job.progress
        if p is None:
            return
        stage, done, total = p
        word = PROGRESS_WORDS.get(stage, stage)
        if stage == "upload":
            text = f"{word} {summarylib.size(done)} of {summarylib.size(total)}"
        else:
            text = f"{word}: {done:,} of {total:,}"
        bpy.context.window_manager.progress_update(int(100 * done / total) if total else 100)
        self.rows = [("", job.title + "...", "info"), ("", text, "info")]
        self._redraw()

    def cancel(self) -> bool:
        busy = [j for j in self.jobs if j.title]
        for j in busy:
            j.cancel.set()
        return bool(busy)

    # --- Check ----------------------------------------------------------------------------

    def check(self) -> bool:
        """Saves and reads the file, then on a worker thread hashes its
        files and asks the farm to check the submission. False when it did
        not start."""
        if self.busy or self.draft is None:
            return False
        self.refresh_scene()
        try:
            prepared = prepare(self.draft)
        except Exception as e:  # noqa: BLE001 - a file Blender cannot read reports, never raises
            self._say(f"Could not read the file: {e}", "error")
            return False
        self.checked = self.submitted = None
        sig = self.signature()
        farm = self.farm
        self.busy = True
        self._say("Checking with the farm...")

        def work(job):
            try:
                s = Submitter(farm, progress=job.tick, cancelled=job.cancel.is_set)
                return s.check(prepared), ""
            except Cancelled:
                return None, "Check cancelled."
            except FarmError as e:
                return None, e.message
            except Exception as e:  # noqa: BLE001
                return None, f"The check failed: {e}"

        self._run(work, lambda r: self._check_done(r, sig), title="Checking with the farm")
        return True

    def _check_done(self, result: Tuple[Optional[Checked], str], sig: str) -> None:
        checked, err = result
        self.busy = False
        if checked is None:
            self.checked = None
            self._say(err, "info" if err.endswith("cancelled.") else "error")
            return
        self.checked, self.checked_sig = checked, sig
        rows = problem_rows(checked) + summary_rows(checked)
        if checked.ok:
            rows.append(("", "Ready: press Submit to send it to the farm.", "ok"))
        elif not checked.errors:
            rows.append(("", "The farm could not check this submission. Save file keeps it, "
                             "to submit with c4 farm submit when the farm takes it.", "warn"))
        else:
            rows.append(("", "Fix what is listed and Check again.", "error"))
        self.rows, self.message, self.showing_file = rows, "", False

    # --- Submit -----------------------------------------------------------------------------

    def submit(self) -> bool:
        """Sends what the farm lacks and starts the jobs, on a worker
        thread. Refused unless the last check passed and nothing changed."""
        if self.busy:
            return False
        c = self.checked
        if c is None or not c.ok or self.submitted is not None:
            self._say("Check first: Submit sends what the last check passed.")
            return False
        # Re-read the file, so the check's own staleness (which c4submit's
        # Submit refuses on too) sees what changed in Blender since.
        self.refresh_scene()
        if self.signature() != self.checked_sig or c.stale():
            self.message = STALE
            self._redraw()
            return False
        farm = self.farm
        self.busy = True
        self._say("Submitting to the farm...")

        def work(job):
            try:
                s = Submitter(farm, progress=job.tick, cancelled=job.cancel.is_set)
                return s.submit(c), ""
            except Cancelled:
                return None, "Submit cancelled: nothing was started."
            except FarmError as e:
                return None, "Not submitted: " + e.message + _farm_errors(e)
            except Exception as e:  # noqa: BLE001
                return None, f"Not submitted: {e}"

        self._run(work, self._submit_done, title="Submitting to the farm")
        return True

    def _submit_done(self, result: Tuple[Optional[Submitted], str]) -> None:
        out, err = result
        self.busy = False
        if out is None:
            self._say(err, "info" if "cancelled" in err else "error")
            return
        self.submitted = out
        base = getattr(self.farm, "base", "")
        rows = [("", "Submitted to the farm:", "ok")]
        for j in out.answer.get("jobs") or []:
            rows.append(("", "  " + j.get("name", "") + (f"   {base}{j['link']}" if base and j.get("link") else ""), "ok"))
        if out.uploaded:
            rows.append(("", f"{out.uploaded:,} files sent ({summarylib.size(out.uploaded_bytes)}); "
                             "the farm had the rest.", "ok"))
        else:
            rows.append(("", "Nothing to send: the farm had every file.", "ok"))
        self.rows, self.message, self.showing_file = rows, "", False

    # --- the file -----------------------------------------------------------------------------

    def show_file(self) -> bool:
        """Shows the submission as a file in place of the summary, or the
        summary again."""
        if self.checked is None:
            return False
        self.showing_file = not self.showing_file
        self._redraw()
        return True

    def save_file(self, path: str) -> Optional[str]:
        """Saves the checked submission as a file. Refused when anything
        changed since the check."""
        c = self.checked
        if c is None:
            return None
        if self.signature() != self.checked_sig:
            self.message = STALE
            return None
        if not os.path.splitext(path)[1]:
            path += ".yaml"
        try:
            save(c, path)
        except OSError as e:
            self._say(f"Could not save {path}: {e}", "error")
            return None
        note = f" It has {len(c.errors)} problem{'s' if len(c.errors) != 1 else ''} to fix first." if c.errors else ""
        self.rows = ([("", f"Saved {path}.{note}", "ok"),
                      ("", f"Submit it later with: c4 farm submit {os.path.basename(path)}", "ok")]
                     + problem_rows(c) + summary_rows(c))
        self.message, self.showing_file = f"Saved {path}", False
        self._redraw()
        return path

    def default_path(self) -> str:
        c = self.checked
        name = default_filename(c) if c is not None else "submission.yaml"
        folder = os.path.dirname(self.host.scene_file())
        return os.path.join(folder, name) if folder else name

    # --- what the panel shows -----------------------------------------------------------------

    def display_rows(self) -> List[Row]:
        if self.showing_file and self.checked is not None:
            rows = [("", line, "file") for line in self.checked.yaml().splitlines()]
        else:
            rows = list(self.rows)
        if self.stale():
            rows.insert(0, ("", STALE, "warn"))
        return rows

    def summary_text(self) -> str:
        """The summary as plain text, as the panel shows it."""
        return "\n".join(text if not label else f"  {label:<10} {text}" for label, text, _ in self.display_rows())

    def forget(self) -> None:
        for j in self.jobs:
            j.cancel.set()


def problem_rows(checked: Checked) -> List[Row]:
    """A check's errors, each with its fix on the row under it, then its
    warnings, ahead of the summary."""
    rows: List[Row] = []
    if checked.errors:
        rows.append(("", "To fix before submitting:", "error"))
        for e in checked.errors:
            rows.append(("", "  " + (f"{e['path']}: " if e.get("path") else "") + e["message"], "error"))
            if e.get("fix"):
                rows.append(("", "      " + e["fix"], "fix"))
    if checked.warnings:
        rows.append(("", "Worth knowing:", "warn"))
        rows += [("", "  " + w, "warn") for w in checked.warnings]
    return rows


def summary_rows(checked: Checked) -> List[Row]:
    return [(ln.label, ln.text, ln.level) for ln in summarylib.lines(checked, errors=False)]


def _farm_errors(e: FarmError) -> str:
    body = e.body
    errs = body if isinstance(body, list) else (body.get("errors") if isinstance(body, dict) else None)
    if not errs:
        return ""
    return "\n" + "\n".join(f"  {x.get('path', '')}: {x.get('message', '')}" for x in errs if isinstance(x, dict))


def controller_for(scene) -> Controller:
    """The scene's panel, made the first time it is drawn and started on a
    timer (a panel's draw may not write to the file's data)."""
    c = _controllers.get(scene.as_pointer())
    if c is None:
        c = _controllers[scene.as_pointer()] = Controller(scene)

        def start():
            if _controllers.get(c.key) is c:
                c.start()
            return None

        bpy.app.timers.register(start, first_interval=0.0)
    return c


def forget() -> None:
    """Drops every scene's panel, as when another file is opened."""
    for c in _controllers.values():
        c.forget()
    _controllers.clear()


# --- operators ----------------------------------------------------------------------------

def _c(context) -> Optional[Controller]:
    scene = getattr(context, "scene", None)
    return _controllers.get(scene.as_pointer()) if scene is not None else None


class _Watch:
    """Keeps the operator running while its work runs on a worker thread,
    so Esc cancels it; every other event passes through to Blender."""

    _timer = None

    def _watch(self, context):
        wm = context.window_manager
        self._timer = wm.event_timer_add(0.25, window=context.window)
        wm.modal_handler_add(self)
        return {"RUNNING_MODAL"}

    def modal(self, context, event):
        c = _c(context)
        if c is None or not c.busy:
            self._unwatch(context)
            return {"FINISHED"}
        if event.type == "ESC" and event.value == "PRESS":
            c.cancel()
            return {"RUNNING_MODAL"}
        return {"PASS_THROUGH"}

    def _unwatch(self, context):
        if self._timer is not None:
            context.window_manager.event_timer_remove(self._timer)
            self._timer = None


class C4_OT_check(_Watch, bpy.types.Operator):  # noqa: N801
    bl_idname = "c4.check"
    bl_label = "Check"
    bl_description = ("Saves the file, reads every file the ticked view layers need and asks the farm when they "
                      "would be ready. Nothing renders. Esc cancels")
    bl_options = {"REGISTER"}

    @classmethod
    def poll(cls, context):
        c = _c(context)
        return c is not None and c.draft is not None and not c.busy

    def execute(self, context):
        c = _c(context)
        if not c.check():
            self.report({"WARNING"}, c.message or "Nothing to check")
            return {"CANCELLED"}
        return {"FINISHED"}

    def invoke(self, context, event):
        if self.execute(context) != {"FINISHED"}:
            return {"CANCELLED"}
        return self._watch(context)


class C4_OT_submit(_Watch, bpy.types.Operator):  # noqa: N801
    bl_idname = "c4.submit"
    bl_label = "Submit"
    bl_description = "Sends the files the farm lacks and starts the jobs. Check first. Esc cancels"
    bl_options = {"REGISTER"}

    @classmethod
    def poll(cls, context):
        c = _c(context)
        return c is not None and c.ready()

    def execute(self, context):
        c = _c(context)
        if not c.submit():
            self.report({"WARNING"}, c.message or "Check first")
            return {"CANCELLED"}
        return {"FINISHED"}

    def invoke(self, context, event):
        if self.execute(context) != {"FINISHED"}:
            return {"CANCELLED"}
        return self._watch(context)


class C4_OT_cancel(bpy.types.Operator):  # noqa: N801
    bl_idname = "c4.cancel"
    bl_label = "Cancel"
    bl_description = "Stops the check or the submission. Nothing is started on the farm"
    bl_options = {"REGISTER"}

    @classmethod
    def poll(cls, context):
        c = _c(context)
        return c is not None and c.busy

    def execute(self, context):
        _c(context).cancel()
        return {"FINISHED"}


class C4_OT_show_file(bpy.types.Operator):  # noqa: N801
    bl_idname = "c4.show_file"
    bl_label = "Show file"
    bl_description = "Shows the submission as the file Save file writes, in place of the summary, or the summary again"
    bl_options = {"REGISTER"}

    @classmethod
    def poll(cls, context):
        c = _c(context)
        return c is not None and c.checked is not None and not c.busy

    def execute(self, context):
        _c(context).show_file()
        return {"FINISHED"}


class C4_OT_copy_file(bpy.types.Operator):  # noqa: N801
    bl_idname = "c4.copy_file"
    bl_label = "Copy file"
    bl_description = "Copies the submission, as a file, to the clipboard"
    bl_options = {"REGISTER"}

    @classmethod
    def poll(cls, context):
        c = _c(context)
        return c is not None and c.checked is not None

    def execute(self, context):
        context.window_manager.clipboard = _c(context).checked.yaml()
        self.report({"INFO"}, "The submission is on the clipboard")
        return {"FINISHED"}


class C4_OT_save_file(bpy.types.Operator):  # noqa: N801
    bl_idname = "c4.save_file"
    bl_label = "Save file"
    bl_description = "Saves the submission as a file, to submit later with c4 farm submit"
    bl_options = {"REGISTER"}

    filepath: StringProperty(name="File", subtype="FILE_PATH")
    filter_glob: StringProperty(default="*.yaml;*.yml", options={"HIDDEN"})
    check_existing: BoolProperty(default=True, options={"HIDDEN"})

    @classmethod
    def poll(cls, context):
        c = _c(context)
        return c is not None and c.checked is not None and not c.busy

    def invoke(self, context, event):
        c = _c(context)
        if c.stale():
            self.report({"WARNING"}, STALE)
            return {"CANCELLED"}
        self.filepath = c.default_path()
        context.window_manager.fileselect_add(self)
        return {"RUNNING_MODAL"}

    def execute(self, context):
        c = _c(context)
        path = c.save_file(bpy.path.abspath(self.filepath) if self.filepath.startswith("//") else self.filepath)
        if path is None:
            self.report({"ERROR"}, c.message or "Not saved")
            return {"CANCELLED"}
        self.report({"INFO"}, f"Saved {path}")
        return {"FINISHED"}


class C4_OT_refresh(bpy.types.Operator):  # noqa: N801
    bl_idname = "c4.refresh"
    bl_label = "Read the view layers again"
    bl_description = "Reads the file's view layers and frame ranges again"
    bl_options = {"REGISTER"}

    @classmethod
    def poll(cls, context):
        c = _c(context)
        return c is not None and c.draft is not None and not c.busy

    def execute(self, context):
        _c(context).refresh_scene()
        return {"FINISHED"}


class C4_OT_reload(bpy.types.Operator):  # noqa: N801
    bl_idname = "c4.reload"
    bl_label = "Read the farm's tools again"
    bl_description = "Asks the farm again for the project's tools and milestones"
    bl_options = {"REGISTER"}

    @classmethod
    def poll(cls, context):
        c = _c(context)
        return c is not None and not c.loading and not c.busy

    def execute(self, context):
        _c(context).load()
        return {"FINISHED"}


# --- drawing --------------------------------------------------------------------------------

def _link_icon(on: bool) -> str:
    return "LINKED" if on else "UNLINKED"


def _draw_specs(layout, s, specs: List[WidgetSpec]) -> None:
    for spec in specs:
        if spec.kind == "heading":
            layout.label(text=spec.label)
            continue
        item = s.fields.get(spec.param)
        if item is None:
            continue
        row = layout.row(align=True)
        row.prop(item, spec.attr, text=spec.label)
        if spec.linked is not None:
            row.prop(item, "linked", text="", icon=_link_icon(item.linked))


def _draw_rows(layout, context, rows: List[Row]) -> None:
    if not rows:
        return
    region = getattr(context, "region", None)
    scale = getattr(getattr(getattr(context, "preferences", None), "system", None), "ui_scale", 1.0) or 1.0
    width = int((getattr(region, "width", 0) or 420) / (7 * scale))
    col = layout.box().column(align=True)
    for label, text, level in rows:
        icon = ICONS.get(level, "NONE")
        if label:
            for i, chunk in enumerate(wrap(text, width - 12)):
                sp = col.split(factor=0.25)
                sp.label(text=label if i == 0 else "")
                sp.label(text=chunk, icon=icon if i == 0 else "NONE")
            continue
        for i, chunk in enumerate(wrap(text, width)):
            col.label(text=chunk, icon=icon if i == 0 else "NONE")


def draw(layout, context) -> None:
    scene = context.scene
    s = scene.c4_farm
    c = controller_for(scene)
    c.watch()
    layout.use_property_split = True
    layout.use_property_decorate = False
    col = layout.column()
    col.prop(s, "tool")
    col.prop(s, "project")
    col.prop(s, "shot")
    col.prop(s, "due")
    if s.due_value == "typed":
        col.prop(s, "due_time")
    d = c.draft
    if d is None:
        if not c.loading:
            layout.operator("c4.reload", icon="FILE_REFRESH")
        _draw_rows(layout, context, c.display_rows())
        return
    if d.tool.takes_frames:
        row = col.row(align=True)
        row.prop(s, "frames")
        row.prop(s, "frames_linked", text="", icon=_link_icon(s.frames_linked))
    if any(p.source == "scene.renderables" for p in d.tool.params):
        head = layout.row()
        head.label(text="View layers")
        head.operator("c4.refresh", text="", icon="FILE_REFRESH")
        layers = layout.column(align=True)
        layers.use_property_split = False
        if not len(s.layers):
            layers.label(text="None is used for rendering in this file")
        for it in s.layers:
            layers.prop(it, "ticked", text=it.label)
    col = layout.column()
    if d.tool.sizes:
        col.prop(s, "size")
    col.prop(s, "note")
    artist, advanced = form_specs(d.form)
    _draw_specs(col, s, artist)
    if advanced:
        if hasattr(layout, "panel"):
            header, body = layout.panel("C4_PT_farm_advanced", default_closed=True)
            header.label(text="Advanced")
            if body is not None:
                _draw_specs(body.column(), s, advanced)
        else:  # before Blender 4.1, which first drew folding panels inside a panel
            layout.label(text="Advanced")
            _draw_specs(layout.column(), s, advanced)
    row = layout.row(align=True)
    row.operator("c4.check", icon="VIEWZOOM")
    row.operator("c4.submit", icon="EXPORT")
    row = layout.row(align=True)
    row.operator("c4.show_file", text="Show summary" if c.showing_file else "Show file")
    row.operator("c4.save_file", text="Save file...")
    if c.showing_file:
        row.operator("c4.copy_file", text="", icon="COPYDOWN")
    if c.busy:
        layout.operator("c4.cancel", icon="CANCEL")
    _draw_rows(layout, context, c.display_rows())


class C4_PT_farm(bpy.types.Panel):  # noqa: N801
    bl_idname = "C4_PT_farm"
    bl_label = TITLE
    bl_space_type = "PROPERTIES"
    bl_region_type = "WINDOW"
    bl_context = "render"

    def draw(self, context):
        draw(self.layout, context)


# --- registering ----------------------------------------------------------------------------

CLASSES = (C4_PG_layer, C4_PG_field, C4_PG_settings, C4_OT_check, C4_OT_submit, C4_OT_cancel, C4_OT_show_file,
           C4_OT_copy_file, C4_OT_save_file, C4_OT_refresh, C4_OT_reload, C4_PT_farm)


@bpy.app.handlers.persistent
def _on_load(*_args):
    forget()


def register() -> None:
    for cls in CLASSES:
        bpy.utils.register_class(cls)
    bpy.types.Scene.c4_farm = PointerProperty(type=C4_PG_settings)
    if _on_load not in bpy.app.handlers.load_post:
        bpy.app.handlers.load_post.append(_on_load)


def unregister() -> None:
    if _on_load in bpy.app.handlers.load_post:
        bpy.app.handlers.load_post.remove(_on_load)
    forget()
    del bpy.types.Scene.c4_farm
    for cls in reversed(CLASSES):
        bpy.utils.unregister_class(cls)
