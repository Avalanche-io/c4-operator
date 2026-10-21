"""C4 as the Movie Render Queue's remote executor.

A project sets ``C4FarmExecutor`` as its Default Remote Executor (Project
Settings > Plugins > Movie Render Pipeline). The artist builds the queue as
for any render and presses **Render (Remote)**. Each enabled job in the
queue is a job on the farm.

Render (Remote) then runs this flow, in Unreal's own dialogs:

1. **C4 Farm: Needed by**: a details panel with Project, Shot, Tool,
   Needed by (the project's milestones, "When the farm suggests"), a time
   to type instead, Size and Note. OK checks; Cancel sends nothing.
2. Unreal's own Save dialog for anything unsaved, then the check: the
   project's files are hashed, and the farm is asked what it lacks and
   whether the submission is good, on a worker thread behind a progress
   dialog that Cancel stops.
3. **The summary**: each job with the farm's estimate and the slack against
   Needed by, and every error with its fix. Yes submits, No shows the
   submission as a file (from which Yes saves it), Cancel sends nothing.

Everything that reads Unreal runs on the editor's main thread; the worker
thread runs c4submit alone.
"""

import os
import threading
from typing import Any, Callable, Dict, List, Optional, Tuple

import unreal

from c4submit import summary as summarylib
from c4submit.collect import Cancelled
from c4submit.farm import Farm, FarmError, NotOnFarm
from c4submit.submitter import Checked, Draft, Submitted, Submitter, default_filename, prepare, save
from c4submit.tool import Tool

from .host import UnrealHost

TITLE = "C4 Farm"
SUGGESTED = "When the farm suggests"
STANDARD = "Standard"
STALE = "Changed since the check: render again to check it again."
LOCAL_TOOLS = ("Setting C4_TOOL_PATH to the studio's tool folders lets C4 check the submission and save it as a "
               "file, to submit when the farm takes it.")
PROGRESS_WORDS = {"hash": "Reading files", "upload": "Uploading"}
#: Lines of the submission shown in a dialog; the saved file has them all.
FILE_LINES = 60
#: Where Save file writes, in the project (a folder no submission collects).
SAVE_FOLDER = ("Saved", "C4")

#: How C4 reaches the farm; tests replace it.
farm_factory: Callable[[], Farm] = lambda: Farm(client="c4submit-unreal")  # noqa: E731

# The choices the details panel's menus offer, set before it opens.
_CHOICES: Dict[str, List[str]] = {"tool": [], "needed_by": [], "size": [STANDARD]}
# Needed by and Note, kept between renders in this editor session.
_LAST: Dict[str, str] = {}


# --- the details panel -----------------------------------------------------------

@unreal.uclass()
class C4FarmOptions(unreal.Object):
    """What the artist says about the submission before the check."""

    project = unreal.uproperty(str, meta={"DisplayName": "Project", "ToolTip": "The farm project the shot belongs to"})
    shot = unreal.uproperty(str, meta={"DisplayName": "Shot", "ToolTip": "The shot the frames are for"})
    tool = unreal.uproperty(str, meta={"DisplayName": "Tool", "GetOptions": "tool_options",
                                       "ToolTip": "What renders the jobs on the farm"})
    needed_by = unreal.uproperty(str, meta={"DisplayName": "Needed by", "GetOptions": "needed_by_options",
                                            "ToolTip": "When the frames are needed: a milestone, or when the farm "
                                                       "suggests. The farm works out the rest"})
    needed_at = unreal.uproperty(str, meta={"DisplayName": "Or at a time", "ToolTip": 'A time like "fri 18:00", '
                                            '"18:00" or "+3h", or a date like 2026-10-23, in place of Needed by'})
    size = unreal.uproperty(str, meta={"DisplayName": "Size", "GetOptions": "size_options",
                                       "ToolTip": "How big a machine each frame needs, as the tool names them"})
    note = unreal.uproperty(str, meta={"DisplayName": "Note", "MultiLine": "true",
                                       "ToolTip": "Shown with the jobs on the farm, for whoever looks at them"})

    @unreal.ufunction(ret=unreal.Array(str))
    def tool_options(self):
        return list(_CHOICES["tool"])

    @unreal.ufunction(ret=unreal.Array(str))
    def needed_by_options(self):
        return list(_CHOICES["needed_by"])

    @unreal.ufunction(ret=unreal.Array(str))
    def size_options(self):
        return list(_CHOICES["size"])


# --- Unreal's dialogs --------------------------------------------------------------

class _Job:
    """Work on a worker thread: its progress and its cancel flag."""

    def __init__(self):
        self.cancel = threading.Event()
        self.lock = threading.Lock()
        self.progress: Optional[Tuple[str, int, int]] = None

    def tick(self, stage: str, done: int, total: int) -> None:
        with self.lock:
            self.progress = (stage, done, total)


class Dialogs:
    """What the artist sees, in Unreal's own dialogs."""

    def options(self, opts) -> bool:
        """The details panel; False when the artist cancels. Without the
        panel (an older Unreal), the studio's environment stands."""
        show = getattr(unreal.EditorDialog, "show_object_details_view", None)
        if show is None:
            return True
        view = None
        try:
            view = unreal.EditorDialogLibraryObjectDetailsViewOptions()
            view.show_object_name = False
            view.allow_search = False
        except Exception:  # noqa: BLE001 - the options are a nicety
            pass
        return bool(show(TITLE + ": Needed by", opts, view))

    def ask(self, title: str, text: str, buttons: str = "ok") -> str:
        """A message with ``ok``, ``yes_no`` or ``yes_no_cancel`` buttons;
        returns ``ok``, ``yes``, ``no`` or ``cancel``."""
        kinds = {"ok": unreal.AppMsgType.OK, "yes_no": unreal.AppMsgType.YES_NO,
                 "yes_no_cancel": unreal.AppMsgType.YES_NO_CANCEL}
        r = unreal.EditorDialog.show_message(title, text, kinds[buttons])
        R = unreal.AppReturnType
        return {R.YES: "yes", R.NO: "no", R.CANCEL: "cancel", R.OK: "ok"}.get(r, "cancel")

    def work(self, title: str, fn: Callable[[_Job], Any]) -> Any:
        """Runs ``fn`` on a worker thread behind Unreal's progress dialog,
        whose Cancel sets the job's cancel flag; returns what ``fn`` did."""
        job, box = _Job(), []
        t = threading.Thread(target=lambda: box.append(fn(job)), name="c4-farm", daemon=True)
        with unreal.ScopedSlowTask(100, title) as task:
            task.make_dialog(True)
            t.start()
            shown = 0
            while t.is_alive():
                with job.lock:
                    p = job.progress
                text, pct = title, shown
                if p is not None:
                    stage, done, total = p
                    word = PROGRESS_WORDS.get(stage, stage)
                    text = (f"{word} {summarylib.size(done)} of {summarylib.size(total)}" if stage == "upload"
                            else f"{word}: {done:,} of {total:,}")
                    pct = int(100 * done / total) if total else 100
                task.enter_progress_frame(max(0, pct - shown), text)
                shown = max(shown, pct)
                if task.should_cancel():
                    job.cancel.set()
                t.join(0.05)
        return box[0] if box else (None, "the work stopped without an answer")


# --- the flow --------------------------------------------------------------------------

def due_value(text: str, choices: List[Dict[str, str]]) -> str:
    """Needed by as the submission says it: a milestone by its label or
    name, "suggested", or what was typed."""
    t = (text or "").strip()
    if not t or t == SUGGESTED:
        return "suggested"
    for c in choices:
        if t in (c["label"], c["value"]):
            return c["value"]
    return t


def problems_text(checked: Checked) -> str:
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


class Flow:
    """One press of Render (Remote): options, check, summary, submit."""

    def __init__(self, queue, dialogs: Optional[Dialogs] = None, host: Optional[UnrealHost] = None):
        self.ui = dialogs or Dialogs()
        self.host = host or UnrealHost(queue)
        self._farm: Optional[Farm] = None
        self.tools: List[Tool] = []
        self.info: Optional[dict] = None
        self.choices: List[Dict[str, str]] = Submitter.due_choices(None)
        self.draft: Optional[Draft] = None
        self.checked: Optional[Checked] = None
        self.submitted: Optional[Submitted] = None
        self.saved = ""

    @property
    def farm(self) -> Farm:
        if self._farm is None:
            self._farm = farm_factory()
        return self._farm

    def say(self, text: str) -> str:
        self.ui.ask(TITLE, text, "ok")
        return text

    # --- 1. what the artist says ----------------------------------------------------

    def _load(self, project: str) -> str:
        scene = self.host.scene_file() or "project.uproject"

        def work(job):
            try:
                farm = self.farm
                info = farm.project(project)
                tools = Submitter(farm).tools_for(project, scene)
                if not tools:
                    return (None, [], f"No tool on the farm renders Unreal projects for {project}. " + LOCAL_TOOLS)
                return info, tools, ""
            except NotOnFarm as e:
                return None, [], f"{e.message}, so C4 has no tool to check this with. " + LOCAL_TOOLS
            except FarmError as e:
                hint = e.status in (0, 401, 403) and "C4_HUD" not in e.message
                return None, [], e.message + (" (check C4_HUD and your sign-in)" if hint else "")
            except Exception as e:  # noqa: BLE001 - reported, never raised into Unreal
                return None, [], f"could not read the farm: {e}"

        info, tools, err = self.ui.work(f"Reading {project}'s tools from the farm", work)
        self.info, self.tools = info, tools
        self.choices = Submitter.due_choices(info)
        return err

    def _tool_label(self, t: Tool) -> str:
        same = [x for x in self.tools if x is not t and x.title == t.title]
        return f"{t.title} ({t.name})" if same else t.title

    def ask_options(self) -> Optional[C4FarmOptions]:
        """The details panel, reading the project's tools first; None when
        the artist cancels or the farm has no tool."""
        opts = unreal.new_object(C4FarmOptions)
        opts.project = self.host.project()
        opts.shot = self.host.shot()
        opts.needed_by = _LAST.get("needed_by", SUGGESTED)
        opts.needed_at = _LAST.get("needed_at", "")
        opts.note = _LAST.get("note", "")
        opts.size = STANDARD
        loaded = None
        while True:
            project = opts.project.strip()
            if project and project != loaded:
                err = self._load(project)
                loaded = project
                if err:
                    self.say(err)
                    return None
                _CHOICES["tool"] = [self._tool_label(t) for t in self.tools]
                _CHOICES["needed_by"] = [c["label"] for c in self.choices]
                _CHOICES["size"] = [STANDARD] + sorted({s for t in self.tools for s in t.sizes})
                if opts.tool not in _CHOICES["tool"]:
                    opts.tool = _CHOICES["tool"][0]
                if opts.needed_by not in _CHOICES["needed_by"]:
                    opts.needed_by = SUGGESTED
            if not self.ui.options(opts):
                return None
            if not opts.project.strip():
                self.say("Which project is this for? Fill in Project, or set C4_PROJECT in the studio's environment.")
                return None
            if opts.project.strip() == loaded:
                break
        _LAST.update(needed_by=opts.needed_by, needed_at=opts.needed_at, note=opts.note)
        return opts

    def make_draft(self, opts: C4FarmOptions) -> Draft:
        names = {self._tool_label(t): t for t in self.tools}
        names.update({t.name: t for t in self.tools})
        tool = names.get(opts.tool.strip(), self.tools[0])
        d = Draft(self.host, tool, project=self.info)
        d.project, d.shot, d.note = opts.project.strip(), opts.shot.strip(), opts.note.strip()
        d.due = due_value(opts.needed_at, self.choices) if opts.needed_at.strip() else due_value(opts.needed_by,
                                                                                                    self.choices)
        size = opts.size.strip()
        d.size = "" if size in ("", STANDARD) else size
        return d

    # --- 2. the check ------------------------------------------------------------------

    def check(self) -> Optional[Checked]:
        try:
            prepared = prepare(self.draft)
        except Exception as e:  # noqa: BLE001
            self.say(f"Could not read the project: {e}")
            return None
        farm = self.farm

        def work(job):
            try:
                return Submitter(farm, progress=job.tick, cancelled=job.cancel.is_set).check(prepared), ""
            except Cancelled:
                return None, "Check cancelled: nothing was sent."
            except FarmError as e:
                return None, e.message
            except Exception as e:  # noqa: BLE001
                return None, f"The check failed: {e}"

        checked, err = self.ui.work("Checking with the farm", work)
        if checked is None:
            self.say(err)
        self.checked = checked
        return checked

    # --- 3. the summary ---------------------------------------------------------------------

    def summary(self) -> str:
        c = self.checked
        return (problems_text(c) + summarylib.text(c, errors=False)).rstrip("\n")

    def save_path(self) -> str:
        return os.path.join(self.host.project_dir(), *SAVE_FOLDER, default_filename(self.checked))

    def review(self) -> str:
        """The summary, then Submit, Show file or Cancel; returns what
        happened: submitted, saved, cancelled or failed."""
        c = self.checked
        while True:
            if c.ok:
                legend = ("Yes: submit to the farm.\nNo: show the submission as a file, to save it.\n"
                          "Cancel: send nothing.")
                answer = self.ui.ask(TITLE + ": ready to submit", self.summary() + "\n\n" + legend, "yes_no_cancel")
            else:
                why = ("Fix what is listed, then render again." if c.errors else
                       "The farm could not check this submission. Saved as a file, it can be submitted with "
                       "c4 farm submit when the farm takes it.")
                legend = "Yes: show the submission as a file, to save it.\nNo: send nothing."
                answer = self.ui.ask(TITLE + ": not ready", self.summary() + "\n\n" + why + "\n\n" + legend, "yes_no")
                answer = {"yes": "no", "no": "cancel"}.get(answer, "cancel")
            if answer == "cancel":
                return "cancelled"
            if answer == "yes":
                return self.submit()
            shown = self.show_file()
            if shown != "back":
                return shown

    def show_file(self) -> str:
        lines = self.checked.yaml().splitlines()
        unreal.log("C4: the submission:\n" + "\n".join(lines))
        text = "\n".join(lines[:FILE_LINES])
        if len(lines) > FILE_LINES:
            text += f"\n... and {len(lines) - FILE_LINES} lines more, in the Output Log and in the saved file"
        path = self.save_path()
        legend = f"Yes: save it as {path}.\nNo: back to the summary.\nCancel: send nothing."
        answer = self.ui.ask(TITLE + ": the submission", text + "\n\n" + legend, "yes_no_cancel")
        if answer == "no":
            return "back"
        if answer != "yes":
            return "cancelled"
        return self.save_file(path)

    def save_file(self, path: str) -> str:
        try:
            os.makedirs(os.path.dirname(path), exist_ok=True)
            save(self.checked, path)
        except OSError as e:
            self.say(f"Could not save {path}: {e}")
            return "failed"
        self.saved = path
        self.say(f"Saved {path}.\nSubmit it later with: c4 farm submit {os.path.basename(path)}")
        return "saved"

    # --- 4. submit -----------------------------------------------------------------------------

    def submit(self) -> str:
        c = self.checked
        self.draft.refresh()
        if c.stale():
            self.say(STALE)
            return "cancelled"
        farm = self.farm

        def work(job):
            try:
                return Submitter(farm, progress=job.tick, cancelled=job.cancel.is_set).submit(c), ""
            except Cancelled:
                return None, "Submit cancelled: nothing was started."
            except FarmError as e:
                return None, "Not submitted: " + e.message + _farm_errors(e)
            except Exception as e:  # noqa: BLE001
                return None, f"Not submitted: {e}"

        out, err = self.ui.work("Submitting to the farm", work)
        if out is None:
            self.say(err)
            return "cancelled" if "cancelled" in err else "failed"
        self.submitted = out
        base = getattr(farm, "base", "")
        rows = ["Submitted to the farm:"]
        for j in out.answer.get("jobs") or []:
            rows.append("  " + j.get("name", "") + (f"   {base}{j['link']}" if base and j.get("link") else ""))
        rows.append(f"{out.uploaded:,} files sent ({summarylib.size(out.uploaded_bytes)}); the farm had the rest."
                    if out.uploaded else "Nothing to send: the farm had every file.")
        self.say("\n".join(rows))
        return "submitted"

    # --- all of it -------------------------------------------------------------------------------

    def run(self) -> str:
        if not self.host.jobs():
            self.say("The queue has no enabled job to render.")
            return "cancelled"
        opts = self.ask_options()
        if opts is None:
            return "cancelled"
        self.draft = self.make_draft(opts)
        if self.check() is None:
            return "failed"
        return self.review()


def submit_queue(queue, dialogs: Optional[Dialogs] = None) -> str:
    """Render (Remote) for a queue; returns what happened."""
    return Flow(queue, dialogs).run()


# --- the executor ----------------------------------------------------------------------------------

@unreal.uclass()
class C4FarmExecutor(unreal.MoviePipelineExecutorBase):
    """The Movie Render Queue's remote executor: Render (Remote) submits
    the queue to the C4 farm, one farm job per enabled queue job."""

    @unreal.ufunction(override=True)
    def execute(self, pipeline_queue):
        try:
            submit_queue(pipeline_queue)
        except Exception as e:  # noqa: BLE001 - reported, never raised into the queue
            unreal.log_error(f"C4: {e}")
            unreal.EditorDialog.show_message(TITLE, f"Could not submit: {e}", unreal.AppMsgType.OK)
        finally:
            self.on_executor_finished_impl()

    @unreal.ufunction(override=True)
    def is_rendering(self):
        return False
