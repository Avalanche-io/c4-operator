"""The C4 nodes' parameters and what their buttons do.

The interface is drawn from the tool's parameters (TOOL.md section 6) as
Houdini parameters:

- a header the same for every tool: Tool, Project, Shot, Needed by, Frames
  with a link toggle, the render settings (LOP) or ROPs (ROP) to render,
  Size when the tool has sizes, and Note. The C4 Render LOP adds where the
  stage is written and its frame range;
- a "Tool" folder with one parameter per field the tool draws, grouped by
  ``groupLabel``, ``show: advanced`` fields in a collapsible folder, and a
  "Linked" toggle beside each field taken from the scene;
- Check, Submit, Show file and Save file, and a read-only summary.

``header``, ``tool_folder`` and ``group`` are pure: they build
``hou.ParmTemplate``s from a Draft and touch no node. The HDAs hold the
header and buttons (``build_hdas.py``); the Tool folder is added as spare
parameters and rebuilt when the tool changes. ``install(node)`` adds all of
it as spare parameters to a null LOP or ROP, so the nodes work before the
HDAs are built.

A ``Panel`` per node holds the Draft. Callbacks read the parameters into
it and write it back. Check and Submit run under a
``hou.InterruptableOperation``: the scene is read on the main thread, the
hashing and the farm's answers on a worker thread, and Cancel stops them.
"""

import os
import re
import threading
from typing import Any, Callable, Dict, List, Optional, Tuple

import hou

from c4submit import summary as summarylib
from c4submit.collect import Cancelled, IDCache
from c4submit.farm import Farm, FarmError
from c4submit.form import Field
from c4submit.submitter import Checked, Draft, Submitted, Submitter, default_filename, prepare, save
from c4submit.tool import Tool

from c4_houdini.host import LopHost, RopHost, expand, unexpanded

PREFIX = "c4p_"            # a tool parameter's Houdini parameter is c4p_<name>
LINK = "_link"             # and its link toggle c4p_<name>_link
TOOL_FOLDER = "c4_toolparms"
ACTIONS = "c4_actions"     # the separator above the buttons; the Tool folder goes before it

CALLBACK = "from c4_houdini import nodes; nodes.changed(kwargs)"

# Where the farm comes from; tests put a fake farm here.
farm_factory: Callable[[], Any] = Farm


def _menu_script(which: str) -> str:
    # A Python menu script is the body of a function returning
    # [token, label, token, label, ...].
    return f"from c4_houdini import nodes\nreturn nodes.menu(kwargs['node'], {which!r})"


def _py() -> dict:
    return {"script_callback": CALLBACK, "script_callback_language": hou.scriptLanguage.Python}


def _string(name: str, label: str, default: str = "", **kw) -> "hou.StringParmTemplate":
    opts = _py()
    opts.update(kw)
    return hou.StringParmTemplate(name, label, 1, default_value=(default,), **opts)


def _toggle(name: str, label: str, default: bool = False, **kw) -> "hou.ToggleParmTemplate":
    opts = _py()
    opts.update(kw)
    return hou.ToggleParmTemplate(name, label, default_value=default, **opts)


def _menu(name: str, label: str, which: str, default: str = "", menu_type=None, **kw) -> "hou.StringParmTemplate":
    return _string(name, label, default, item_generator_script=_menu_script(which),
                   item_generator_script_language=hou.scriptLanguage.Python,
                   menu_type=menu_type or hou.menuType.Normal, **kw)


def kind_of(node) -> str:
    """'lop' for the C4 Render LOP, 'rop' for the C4 Submit ROP."""
    cat = node.type().category().name()
    if cat == "Lop":
        return "lop"
    if cat == "Driver":
        return "rop"
    raise ValueError(f"{node.path()} is not a LOP or a ROP; C4 Render is a LOP and C4 Submit is a ROP")


# --- the parameters -----------------------------------------------------------


def header(kind: str) -> List["hou.ParmTemplate"]:
    """The parameters above the Tool folder, the same for every tool."""
    lop = kind == "lop"
    out = [
        _menu("c4_tool", "Tool", "tool", help="What renders this on the farm. The list is the project's tools "
                                              "that render " + ("USD stages." if lop else "hip files.")),
        _string("c4_project", "Project", help="The farm project (C4_PROJECT by default)."),
        _string("c4_shot", "Shot", help="The shot this is for (C4_SHOT or C4_SHOT_PATTERN by default)."),
        _menu("c4_due", "Needed by", "due", "suggested", menu_type=hou.menuType.StringReplace,
              help="When you need it: one of the shot's milestones, when the farm suggests, "
                   "or a time you type, such as \"fri 18:00\" or \"+3h\"."),
    ]
    if lop:
        out += [
            _string("c4_usdfile", "Stage file", '$HIP/usd/`chs("c4_shot")`_$OS.usd',
                    string_type=hou.stringParmType.FileReference,
                    file_type=getattr(hou.fileType, "Usd", hou.fileType.Any),
                    tags={"filechooser_mode": "write", "filechooser_pattern": "*.usd *.usdc *.usda"},
                    help="Where the stage wired into this node is written for the farm to render. "
                         "It must be under the project folder."),
            hou.FloatParmTemplate("c4_f", "Frame range", 3, default_value=(1001, 1100, 1),
                                  naming_scheme=hou.parmNamingScheme.Base1,
                                  default_expression=("$FSTART", "$FEND", "1"),
                                  default_expression_language=(hou.scriptLanguage.Hscript,) * 3,
                                  help="Start, end and step of the stage's frames.", **_py()),
        ]
    out += [
        _string("c4_frames", "Frames", join_with_next=True, disable_when="{ c4_frames_link == 1 }",
                help="The frames to render, such as 1001-1240, 1001-1240:10 or 1001,1050,1100."),
        _toggle("c4_frames_link", "Linked", True,
                help="On: each job renders its own range (" + ("the Frame range above" if lop else "each ROP's")
                     + "). Off: every job renders the frames typed here."),
        _menu("c4_renderables", "Render settings" if lop else "ROPs", "renderables",
              menu_type=hou.menuType.StringToggle,
              help=("The stage's render settings to render; each one ticked is a job." if lop else
                    "The ROPs to render; each one ticked is a job, after the ROPs it reads. "
                    "ROPs further up the chain are listed unticked.")),
        _toggle("c4_hassizes", "Has sizes", False, is_hidden=True),
    ]
    size = _menu("c4_size", "Size", "size", help="A bigger machine for a heavy shot, as the tool offers.")
    size.setConditional(hou.parmCondType.HideWhen, "{ c4_hassizes == 0 }")
    out += [size, _string("c4_note", "Note", help="Shown with the job on the farm.")]
    return out


def actions() -> List["hou.ParmTemplate"]:
    """The buttons and the summary, below the Tool folder."""
    button = hou.ButtonParmTemplate
    return [
        hou.SeparatorParmTemplate(ACTIONS),
        button("c4_check", "Check", join_with_next=True, help="Ask the farm when it would be ready.", **_py()),
        button("c4_submit", "Submit", join_with_next=True, help="Check, show the estimate, then submit.", **_py()),
        button("c4_show", "Show file", join_with_next=True, help="Show the submission as a file.", **_py()),
        button("c4_save", "Save file", help="Save the submission as a file to submit later.", **_py()),
        hou.StringParmTemplate("c4_summary", "Summary", 1, default_value=("",), is_label_hidden=True,
                               tags={"editor": "1", "editorlines": "6-24"}),
    ]


def _text(v: Any) -> str:
    if v is None:
        return ""
    if isinstance(v, bool):
        return "on" if v else "off"
    if isinstance(v, (list, tuple)):
        return ", ".join(str(x) for x in v)
    return str(v)


def field_templates(f: Field) -> List["hou.ParmTemplate"]:
    """The Houdini parameter for one field, and its link toggle when the
    field is taken from the scene."""
    p = f.param
    name = PREFIX + p.name
    control = f.control
    kw = _py()
    kw["help"] = p.description or None
    if f.derivable:
        kw["join_with_next"] = True
        kw["disable_when"] = "{ %s%s == 1 }" % (name, LINK)
    if control == "CHECK_BOX" and not p.allowed:
        t = hou.ToggleParmTemplate(name, f.label, default_value=bool(p.default), **kw)
    elif control == "DROPDOWN_LIST" or p.allowed:
        items = [_text(v) for v in p.allowed or []]
        t = hou.StringParmTemplate(name, f.label, 1, default_value=(_text(p.default),), menu_items=items,
                                   menu_labels=items,
                                   menu_type=hou.menuType.Normal if items else hou.menuType.StringReplace, **kw)
    elif control == "SPIN_BOX" and p.type in ("INT", "FLOAT"):
        integer = p.type == "INT"
        cls = hou.IntParmTemplate if integer else hou.FloatParmTemplate
        lo = p.min if p.min is not None else 0
        hi = p.max if p.max is not None else max(lo, 0) + (10 if integer else 1.0)
        cast = int if integer else float
        default = p.default if isinstance(p.default, (int, float)) and not isinstance(p.default, bool) else lo
        t = cls(name, f.label, 1, default_value=(cast(default),), min=cast(lo), max=cast(hi),
                min_is_strict=p.min is not None, max_is_strict=p.max is not None, **kw)
    elif control.startswith("CHOOSE_"):
        mode = {"OUT": "write", "INOUT": "read_and_write"}.get(p.data_flow, "read")
        tags = {"filechooser_mode": "write" if control == "CHOOSE_OUTPUT_FILE" else mode}
        if p.file_patterns:
            tags["filechooser_pattern"] = " ".join(p.file_patterns)
        ftype = hou.fileType.Directory if control == "CHOOSE_DIRECTORY" else hou.fileType.Any
        t = hou.StringParmTemplate(name, f.label, 1, default_value=(_text(p.default),),
                                   string_type=hou.stringParmType.FileReference, file_type=ftype, tags=tags, **kw)
    else:
        tags = {"editor": "1"} if control == "MULTILINE_EDIT" else {}
        t = hou.StringParmTemplate(name, f.label, 1, default_value=(_text(p.default),), tags=tags, **kw)
    out = [t]
    if f.derivable:
        out.append(_toggle(name + LINK, "Linked", True,
                           help="On: taken from the scene. Off: keeps what you type."))
    return out


def _slug(s: str) -> str:
    return re.sub(r"[^a-z0-9]+", "_", s.lower()).strip("_") or "group"


def tool_folder(draft: Draft) -> "hou.FolderParmTemplate":
    """The Tool folder: the tool's drawn fields, by group."""
    folder = hou.FolderParmTemplate(TOOL_FOLDER, "Tool", folder_type=hou.folderType.Tabs)
    for label, fields in draft.form.groups():
        templates = [t for f in fields for t in field_templates(f)]
        if not label:
            for t in templates:
                folder.addParmTemplate(t)
            continue
        kind = hou.folderType.Collapsible if all(f.advanced for f in fields) else hou.folderType.Simple
        folder.addParmTemplate(hou.FolderParmTemplate("c4g_" + _slug(label), label, templates, folder_type=kind))
    return folder


def static_group(kind: str) -> "hou.ParmTemplateGroup":
    """The parameters every C4 node of a kind has, whatever the tool: what
    the HDA's definition holds."""
    return hou.ParmTemplateGroup(header(kind) + actions())


def group(kind: str, draft: Optional[Draft] = None) -> "hou.ParmTemplateGroup":
    """The whole interface for a draft."""
    g = static_group(kind)
    if draft is not None:
        g.insertBefore(ACTIONS, tool_folder(draft))
    return g


def rebuild(node, draft: Optional[Draft]) -> None:
    """Replaces the node's Tool folder with the draft's (spare parameters),
    adding the header and buttons if the node lacks them."""
    g = node.parmTemplateGroup()
    for t in header(kind_of(node)) + actions():
        if g.find(t.name()) is None:
            g.append(t)
    if g.find(TOOL_FOLDER) is not None:
        g.remove(TOOL_FOLDER)
    if draft is not None:
        g.insertBefore(ACTIONS, tool_folder(draft))
    node.setParmTemplateGroup(g)


# --- reading and writing parameters ----------------------------------------------


def _get(node, name: str, default: Any = "") -> Any:
    p = node.parm(name)
    return p.eval() if p is not None else default


def _typed(node, name: str) -> str:
    """A text parameter as typed: a note or a frame list is not expanded."""
    p = node.parm(name)
    return unexpanded(p) if p is not None else ""


def _set(node, name: str, value: Any) -> None:
    p = node.parm(name)
    if p is None:
        return
    t = p.parmTemplate().type()
    if t == hou.parmTemplateType.Toggle:
        value = int(bool(value))
    elif t == hou.parmTemplateType.Int:
        value = int(value)
    elif t == hou.parmTemplateType.Float:
        value = float(value)
    else:
        value = _text(value) if not isinstance(value, bool) else ("true" if value else "false")
    p.set(value)


def say(node, text: str) -> None:
    """Writes the summary, which the artist cannot edit."""
    p = node.parm("c4_summary")
    if p is None:
        return
    p.lock(False)
    p.set(text)
    p.lock(True)


def _same(f: Field, a: Any, b: Any) -> bool:
    if a in (None, "") and b in (None, ""):
        return True
    try:
        return f.param.coerce(a) == f.param.coerce(b)
    except ValueError:
        return a == b


def _field_value(parm) -> Any:
    t = parm.parmTemplate().type()
    if t == hou.parmTemplateType.Toggle:
        return bool(parm.eval())
    if t == hou.parmTemplateType.String:
        return expand(unexpanded(parm))   # $HIP expanded, $F4 kept: a sequence stays one
    return parm.eval()


# --- the panel ----------------------------------------------------------------

_panels: Dict[int, "Panel"] = {}

_WORDS = {"hash": "Reading files", "upload": "Uploading"}


class Panel:
    """One node's submission: the farm's tools for its scene, the Draft,
    and the last check."""

    def __init__(self, node):
        self.node = node
        self.kind = kind_of(node)
        self.host = LopHost(node) if self.kind == "lop" else RopHost(node)
        self.cache = IDCache()
        self.farm: Any = None
        self.tools: List[Tool] = []
        self.project_info: Optional[dict] = None
        self.draft: Optional[Draft] = None
        self.checked: Optional[Checked] = None
        self.problem = ""
        self.known: set = set()
        self.synced = False
        self.load()

    # --- the farm and the draft --------------------------------------------------

    def scene_name(self) -> str:
        """The scene's name, for picking the tools that render it."""
        if isinstance(self.host, LopHost):
            return self.host.usd_path()
        return self.host.scene_file() or "untitled.hip"

    def load(self) -> None:
        """Reads the farm: the project's tools for this scene and its
        milestones. Then makes the draft for the tool the node names."""
        self.problem = ""
        project = _get(self.node, "c4_project") or self.host.project()
        try:
            if self.farm is None:
                self.farm = farm_factory()
            s = Submitter(self.farm)
            self.tools = s.tools_for(project, self.scene_name()) if project else []
            self.project_info = self.farm.project(project) if project else None
        except FarmError as e:
            self.tools, self.project_info = [], None
            self.problem = e.message
        if not project:
            self.problem = "which farm project is this for? Fill in Project, or set C4_PROJECT"
        elif not self.tools and not self.problem:
            word = "USD stages" if self.kind == "lop" else "hip files"
            self.problem = f"no tool in {project} renders {word}; ask a TD to publish one"
        names = [t.name for t in self.tools]
        current = _get(self.node, "c4_tool")
        tool = self.tools[names.index(current)] if current in names else (self.tools[0] if self.tools else None)
        self.make_draft(tool, keep=current == (tool.name if tool else None))

    def make_draft(self, tool: Optional[Tool], keep: bool) -> None:
        """A draft for a tool, carrying the header over from the last one.
        ``keep`` reads the Tool folder's values too (the same tool, as
        saved in the hip file)."""
        old = self.draft
        if tool is None:
            self.draft = None
            return
        d = Draft(self.host, tool, project=self.project_info)
        if old is not None:
            d.due, d.note, d.frames, d.shot, d.project = old.due, old.note, old.frames, old.shot, old.project
            d.ticked = [r.name for r in d.renderables if r.name in old.ticked]
        self.draft = d
        self.read_header()
        if keep:
            self.read_fields()

    def sync(self) -> None:
        """Draws the draft on the node: the Tool folder, then every value."""
        rebuild(self.node, self.draft)
        if self.draft is None:
            say(self.node, f"C4: {self.problem}")
        else:
            self.write()
            if self.problem:
                say(self.node, f"C4: {self.problem}")
        self.synced = True

    def set_tool(self, name: str) -> None:
        names = [t.name for t in self.tools]
        if name not in names:
            return
        self.make_draft(self.tools[names.index(name)], keep=False)
        self.sync()

    # --- parameters to the draft and back --------------------------------------------

    def read_header(self) -> None:
        d, n = self.draft, self.node
        if d is None:
            return
        d.project = _get(n, "c4_project") or d.project
        d.shot = _get(n, "c4_shot") or self.host.shot()
        d.due = _typed(n, "c4_due").strip() or "suggested"
        d.note = _typed(n, "c4_note")
        size = _get(n, "c4_size")
        d.size = size if size in d.tool.sizes else ""
        d.frames = None if _get(n, "c4_frames_link", 1) else (_typed(n, "c4_frames").strip() or None)
        raw = _typed(n, "c4_renderables").split()
        if raw or self.known:
            d.ticked = [r.name for r in d.renderables
                        if r.name in raw or (r.name not in self.known and r.enabled)]

    def read_fields(self) -> None:
        d, n = self.draft, self.node
        if d is None:
            return
        for f in d.form.drawn():
            p = n.parm(PREFIX + f.name)
            if p is None:
                continue
            if f.derivable:
                link = n.parm(PREFIX + f.name + LINK)
                if link is None or link.eval():
                    d.form.link(f.name, True)
                    continue
                d.form.link(f.name, False)
            v = _field_value(p)
            if not _same(f, v, f.value):
                d.form.set(f.name, v)

    def read(self) -> None:
        """The scene again, then the parameters."""
        if self.draft is None:
            return
        self.draft.refresh()
        self.read_header()
        self.read_fields()

    def write(self) -> None:
        d, n = self.draft, self.node
        if d is None:
            return
        _set(n, "c4_tool", d.tool.name)
        _set(n, "c4_project", d.project)
        _set(n, "c4_shot", d.shot)
        _set(n, "c4_due", d.due or "suggested")
        _set(n, "c4_note", d.note)
        _set(n, "c4_hassizes", bool(d.tool.sizes))
        _set(n, "c4_size", d.size)
        picked = d.picked()
        _set(n, "c4_frames_link", d.frames is None)
        _set(n, "c4_frames", d.frames or d.job_frames(picked[0] if picked else None) or "")
        _set(n, "c4_renderables", " ".join(d.ticked))
        self.known = {r.name for r in d.renderables}
        for f in d.form.drawn():
            if n.parm(PREFIX + f.name) is None:
                continue
            v = f.value
            try:
                v = f.param.coerce(v) if v not in (None, "") else v
            except ValueError:
                pass
            if n.parm(PREFIX + f.name).parmTemplate().type() in (hou.parmTemplateType.Int, hou.parmTemplateType.Float):
                if not isinstance(v, (int, float)):
                    continue  # a number field cannot show what was typed; the summary says what is wrong
            _set(n, PREFIX + f.name, v)
            if f.derivable:
                _set(n, PREFIX + f.name + LINK, f.linked)

    def edited(self) -> None:
        """After the artist changes a parameter: the draft follows, derived
        values are shown, and wrong values are named."""
        self.read()
        self.write()
        self.checked = None
        errs = [f"{self.draft.form.field(name).label}: {err}" for name, err in self.draft.form.errors()]
        say(self.node, "\n".join(errs) if errs else "Changed. Press Check for the estimate.")

    # --- check, submit, show, save ------------------------------------------------

    def _run(self, title: str, work: Callable[[Submitter, Any], Any], before: Optional[Callable[[], Any]] = None) -> Any:
        """Runs ``before`` on the main thread, then ``work`` on a worker
        thread, under an interruptable operation. Cancel raises Cancelled."""
        cancel = threading.Event()
        state = {"stage": "", "done": 0, "total": 0}

        def progress(stage: str, done: int, total: int) -> None:
            state.update(stage=stage, done=done, total=total)

        submitter = Submitter(self.farm, cache=self.cache, progress=progress, cancelled=cancel.is_set)
        result: Dict[str, Any] = {}
        with hou.InterruptableOperation(title, long_operation_name=title, open_interrupt_dialog=True) as op:

            def tick(text: str, fraction: float = -1.0) -> None:
                try:
                    op.updateLongProgress(fraction, text)
                except hou.OperationInterrupted:
                    cancel.set()

            tick(title)
            if cancel.is_set():
                raise Cancelled()
            arg = before() if before else None
            tick(title)
            if cancel.is_set():
                raise Cancelled()

            def body() -> None:
                try:
                    result["value"] = work(submitter, arg)
                except BaseException as e:  # noqa: BLE001 - handed to the main thread
                    result["error"] = e

            def show() -> None:
                stage, done, total = state["stage"], state["done"], state["total"]
                if stage == "upload":
                    text = f"Uploading {summarylib.size(done)} of {summarylib.size(total)}"
                elif total:
                    text = f"{_WORDS.get(stage, stage)}: {done:,} of {total:,}"
                else:
                    text = title
                tick(text, done / total if total else -1.0)

            worker = threading.Thread(target=body, name="c4-submit", daemon=True)
            worker.start()
            while worker.is_alive():
                worker.join(0.05)
                show()
            show()  # where it ended, when the work was quicker than one look
        if "error" in result:
            raise result["error"]
        return result.get("value")

    def ready(self) -> bool:
        if self.draft is None:
            self.load()
            self.sync()
        return self.draft is not None

    def check(self) -> Optional[Checked]:
        """Reads the node and the scene, then asks the farm. The summary
        shows the answer; None when there is nothing to check or the
        artist cancelled."""
        if not self.ready():
            return None
        self.read()
        if isinstance(self.host, LopHost):
            self.host.write_frames = self.draft.frames
        what = "Writing the stage and checking" if self.kind == "lop" else "Checking"
        try:
            checked = self._run(f"C4: {what} with the farm",
                                lambda s, prepared: s.check(prepared),
                                before=lambda: prepare(self.draft))
        except Cancelled:
            say(self.node, "Check cancelled.")
            return None
        self.write()
        self.checked = checked
        say(self.node, summarylib.text(checked))
        return checked

    def submit(self) -> Optional[Submitted]:
        """Checks, shows the summary with Submit, Show file and Cancel, and
        submits when asked."""
        checked = self.check()
        if checked is None:
            return None
        text = summarylib.text(checked)
        ui = hou.isUIAvailable()
        if not checked.ok:
            if ui:
                choice = hou.ui.displayMessage("The farm cannot take this yet.\n\n" + text, buttons=("OK", "Show file"),
                                               severity=hou.severityType.Warning, default_choice=0, close_choice=0,
                                               title="C4 Submit")
                if choice == 1:
                    self.show_file(checked)
            return None
        while ui:
            choice = hou.ui.displayMessage(text, buttons=("Submit", "Show file", "Cancel"), default_choice=0,
                                           close_choice=2, title="C4 Submit")
            if choice == 1:
                self.show_file(checked)
                continue
            if choice != 0:
                say(self.node, text + "\n\nNot submitted.")
                return None
            break
        try:
            done = self._run("C4: Submitting to the farm", lambda s, _: s.submit(checked))
        except Cancelled:
            say(self.node, text + "\n\nUpload cancelled; nothing was submitted.")
            return None
        except FarmError as e:
            say(self.node, text + f"\n\nNot submitted: {e.message}")
            if ui:
                hou.ui.displayMessage(f"Not submitted: {e.message}", severity=hou.severityType.Error, title="C4 Submit")
            return None
        sent = f", {done.uploaded:,} files uploaded ({summarylib.size(done.uploaded_bytes)})" if done.uploaded else ""
        say(self.node, "Submitted " + ", ".join(done.jobs) + sent + ".\n\n" + text)
        if ui:
            hou.ui.setStatusMessage("C4: submitted " + ", ".join(done.jobs))
        return done

    def show_file(self, checked: Optional[Checked] = None) -> Optional[str]:
        """Shows the submission as a file."""
        checked = checked or self.check()
        if checked is None:
            return None
        text = checked.yaml()
        if hou.isUIAvailable():
            hou.ui.displayMessage("The submission as a file. Save file writes it; c4 farm submit FILE submits it.",
                                  details=text, details_label="The file", details_expanded=True,
                                  title="C4 submission")
        return text

    def save_file(self, path: Optional[str] = None) -> Optional[str]:
        """Checks, then saves the submission where the artist picks
        (``$HIP/<shot>-<job>.yaml`` by default)."""
        checked = self.check()
        if checked is None:
            return None
        default = os.path.join(hou.expandString("$HIP"), default_filename(checked))
        if path is None and hou.isUIAvailable():
            path = hou.ui.selectFile(start_directory=os.path.dirname(default), title="Save the submission",
                                     default_value=default, pattern="*.yaml",
                                     chooser_mode=hou.fileChooserMode.Write)
            if not path:
                return None
        path = hou.expandString(path or default)
        save(checked, path)
        say(self.node, summarylib.text(checked) + f"\n\nSaved to {path}")
        return path


def panel_for(node, fresh: bool = False) -> Panel:
    key = node.sessionId()
    if fresh or key not in _panels:
        _panels[key] = Panel(node)
    return _panels[key]


# --- what the node calls -----------------------------------------------------------


def created(node) -> None:
    """A new node: the project and shot from the studio's environment, then
    the farm's tools. Never raises; a problem goes in the summary."""
    try:
        host = LopHost(node) if kind_of(node) == "lop" else RopHost(node)
        if not _get(node, "c4_project"):
            _set(node, "c4_project", host.project())
        if not _get(node, "c4_shot"):
            _set(node, "c4_shot", host.shot())
        panel_for(node, fresh=True).sync()
    except Exception as e:  # noqa: BLE001 - a node must be creatable without a farm
        say(node, f"C4: {e}")


def install(node) -> Panel:
    """Adds the C4 interface to a null LOP or ROP as spare parameters, so
    the node works like the HDA before the HDA is built."""
    rebuild(node, None)
    created(node)
    return panel_for(node)


def changed(kwargs: dict) -> None:
    """Every parameter's callback."""
    node = kwargs["node"]
    name = kwargs.get("parm_name", "")
    try:
        panel = panel_for(node)
        if not panel.synced:
            panel.sync()
        if name == "c4_check":
            panel.check()
        elif name == "c4_submit":
            panel.submit()
        elif name == "c4_show":
            panel.show_file()
        elif name == "c4_save":
            panel.save_file()
        elif name == "c4_tool":
            panel.set_tool(_get(node, "c4_tool"))
        elif name == "c4_project":
            panel.load()
            panel.sync()
        elif panel.draft is not None:
            panel.edited()
    except Exception as e:  # noqa: BLE001 - shown on the node rather than as a Python error
        say(node, f"C4: {e}")
        if hou.isUIAvailable():
            hou.ui.setStatusMessage(f"C4: {e}", severity=hou.severityType.Error)


def menu(node, which: str) -> List[str]:
    """A menu's items as Houdini wants them: token, label, token, label."""
    try:
        panel = panel_for(node)
    except Exception:  # noqa: BLE001 - a menu that cannot be filled is empty
        return []
    d = panel.draft
    items: List[Tuple[str, str]] = []
    if which == "tool":
        items = [(t.name, t.title) for t in panel.tools]
    elif which == "due":
        items = [(c["value"], c["label"]) for c in Submitter().due_choices(panel.project_info)]
    elif which == "renderables":
        rs = d.renderables if d is not None else panel.host.renderables()
        items = [(r.name, (r.label or r.name) + (f"  ({r.note})" if r.note else "")) for r in rs]
    elif which == "size":
        items = [("", "Normal")] + [(s, s) for s in (d.tool.sizes if d is not None else {})]
    return [x for pair in items for x in pair]


def place(kind: str) -> Any:
    """The shelf tools: a C4 Render LOP after the selected LOP, or a C4
    Submit ROP after the selected ROPs. The HDA when it is installed, else
    a null with the interface installed."""
    lop = kind == "lop"
    category = hou.lopNodeTypeCategory() if lop else hou.ropNodeTypeCategory()
    asset = "c4::c4render::1.0" if lop else "c4::c4submit::1.0"
    selected = [n for n in hou.selectedNodes() if n.type().category() == category]
    parent = selected[0].parent() if selected else hou.node("/stage" if lop else "/out")
    has_hda = hou.nodeType(category, asset) is not None
    node = parent.createNode(asset if has_hda else "null", "c4render" if lop else "c4submit")
    for i, n in enumerate(selected[:1] if lop else selected):
        node.setInput(i, n)
    node.moveToGoodPosition()
    if has_hda:
        panel_for(node, fresh=True).sync()   # read the inputs wired after OnCreated ran
    else:
        install(node)
    node.setSelected(True, clear_all_selected=True)
    return node
