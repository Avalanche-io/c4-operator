"""From an open scene to a submitted job, the same way in every DCC.

A panel does four things, in this order:

1. ``Draft(host, tool)``: what the artist is filling in. The tool's form
   (``draft.form``), the renderables to tick, shot, frames, "Needed by" and
   a note. Built and edited on the application's main thread.
2. ``prepare(draft)``: also on the main thread, the last time the scene
   is read. Saves the scene if asked, lists its files and builds the
   submission's jobs.
3. ``check(prepared)``: on any thread. Hashes the files (cached), names the
   collected tree, asks the farm what it lacks and asks the farm to check
   the submission. The answer has the estimate and slack against the due
   time, every error with its fix, and the file itself ("Show file").
4. ``submit(checked)``: uploads only what the farm lacks, records the tree
   and submits. The farm makes one job per job in the file.

Nothing here imports a DCC module or a toolkit.
"""

import json
import os
import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional

from c4submit import collect as collectlib
from c4submit import paths, seq, submission, yamlio
from c4submit.collect import Cancelled, Collection, IDCache, Problem
from c4submit.farm import Farm, FarmError, NotOnFarm
from c4submit.form import Form
from c4submit.host import Dependency, Host, Renderable
from c4submit.tool import Tool, local_tools

Progress = Callable[[str, int, int], None]


@dataclass
class Prepared:
    draft: "Draft"
    doc: dict                                  # the submission, without its tree yet
    collection: Collection
    problems: List[Problem] = field(default_factory=list)
    signature: str = ""                        # the draft's, when prepared


@dataclass
class Checked:
    prepared: Prepared
    doc: dict                                  # the submission as checked, with its tree
    tree_id: str = ""
    missing: List[str] = field(default_factory=list)
    missing_bytes: int = 0
    answer: Optional[dict] = None              # the farm's check
    errors: List[Dict[str, str]] = field(default_factory=list)
    warnings: List[str] = field(default_factory=list)
    farm_checked: bool = False                 # False when the farm could not check it (not reachable, or no route)

    @property
    def ok(self) -> bool:
        return not self.errors and self.farm_checked

    @property
    def collection(self) -> Collection:
        return self.prepared.collection

    def stale(self) -> bool:
        """Whether the draft or the saved scene changed since this was
        prepared, so Submit would send something other than what was checked."""
        return self.prepared.draft.signature() != self.prepared.signature

    def yaml(self) -> str:
        """The submission as a file: what "Show file" shows and "Save" writes."""
        d = self.prepared.draft
        col = self.collection
        comments = [f"Saved from {d.host.name} by c4submit; submit it again with: c4 farm submit FILE",
                    f"tree: {len(col.files):,} files collected under {col.root} from {os.path.basename(d.scene_file) or 'the scene'}"]
        return yamlio.dump(self.doc, comments)


@dataclass
class Submitted:
    checked: Checked
    answer: dict
    uploaded: int = 0
    uploaded_bytes: int = 0

    @property
    def jobs(self) -> List[str]:
        return [j.get("name", "") for j in self.answer.get("jobs") or []]


class Draft:
    """The panel's state: the tool's form and the production fields above
    it. Reads the host when made and on ``refresh()``."""

    def __init__(self, host: Host, tool: Tool, project: Optional[dict] = None):
        self.host = host
        self.tool = tool
        self.project_info = project or {}
        self.project = host.project()
        self.shot = host.shot()
        self.due = "suggested"
        self.note = ""
        self.size = ""
        self.frames: Optional[str] = None      # None follows each renderable's range (linked)
        self.save_scene = True
        self.stage = host.stage
        self.root = host.root() or self._farm_root_here()
        self.refresh()

    def _farm_root_here(self) -> str:
        r = self.project_info.get("root", "")
        return r if r and os.path.isdir(r) else ""

    def refresh(self) -> None:
        """Re-read the scene: its file, range, renderables and every derived field."""
        h = self.host
        self.scene_file = h.scene_file()
        self.scene_frames = h.frames()
        old = {r.name: r for r in getattr(self, "renderables", [])}
        ticked = getattr(self, "ticked", None)
        self.renderables: List[Renderable] = h.renderables()
        if ticked is None:
            self.ticked = [r.name for r in self.renderables if r.enabled]
        else:
            self.ticked = [r.name for r in self.renderables
                           if r.name in ticked or (r.name not in old and r.enabled)]
        self._selection = h.selection()
        self._camera = h.camera()
        if hasattr(self, "form"):
            self.form.refresh(self._derive)
        else:
            self.form = Form(self.tool, derive=self._derive)

    # --- derived values -----------------------------------------------------

    def _derive(self, source: str, renderable: Optional[Renderable] = None) -> Any:
        if source == "scene.file":
            return self.scene_file or None
        if source in ("scene.frames", "shot.frames"):
            # The shot's cut comes from the tracker on the farm; until the
            # farm says, the scene's range stands in for it.
            return (renderable.frames if renderable and renderable.frames else self.scene_frames) or None
        if source == "scene.selection":
            return self._selection[0] if self._selection else None
        if source == "scene.renderables":
            if renderable:
                return renderable.name
            picked = self.picked()
            return picked[0].name if picked else None
        if source == "scene.camera":
            return self._camera
        if source == "milestone":
            return self.due if self.due and self.due != "suggested" else None
        return None

    def signature(self) -> str:
        """Everything a check depends on, as one string: the draft's fields,
        the form's values and links, the renderables as last read, and the
        saved scene's size and time. Reads no DCC, so any thread may call it;
        a panel calls ``refresh()`` first to see changes in the scene."""
        try:
            st = os.stat(self.scene_file)
            stamp = [st.st_size, st.st_mtime_ns]
        except OSError:
            stamp = []
        state = {
            "tool": [self.tool.name, self.tool.version], "project": self.project, "shot": self.shot,
            "due": self.due, "frames": self.frames, "note": self.note, "size": self.size, "root": self.root,
            "ticked": sorted(self.ticked), "form": {f.name: [f.value, f.linked] for f in self.form.fields},
            "renderables": [[r.name, r.frames, r.outputs, r.reads] for r in self.renderables],
            "scene": [self.scene_file, stamp],
        }
        return json.dumps(state, sort_keys=True, default=str)

    # --- jobs -----------------------------------------------------------

    def tick(self, name: str, on: bool = True) -> None:
        if on and name not in self.ticked:
            self.ticked.append(name)
        elif not on and name in self.ticked:
            self.ticked.remove(name)

    def picked(self) -> List[Renderable]:
        return [r for r in self.renderables if r.name in self.ticked]

    @property
    def per_renderable(self) -> bool:
        """One job per ticked renderable, when the tool takes one."""
        return any(p.source == "scene.renderables" for p in self.tool.params) and bool(self.renderables)

    def job_frames(self, r: Optional[Renderable]) -> str:
        if self.frames:
            return self.frames
        if r is not None and r.frames:
            return r.frames
        return self.scene_frames

    def keys(self) -> Dict[str, Optional[Renderable]]:
        """Job key to renderable, in the scene's order."""
        if not self.per_renderable:
            return {submission.job_key(self.stage): None}
        picked = self.picked()
        several = len(picked) > 1
        out: Dict[str, Optional[Renderable]] = {}
        for r in picked:
            k = submission.job_key(self.stage, r.key or r.name, several)
            base, n = k, 2
            while k in out:
                sfx = f"-{n}"
                k, n = base[:40 - len(sfx)].rstrip("-") + sfx, n + 1
            out[k] = r
        return out


def _to_tree_path(root: str, value: Any) -> Any:
    """A PATH value as the job sees it: relative to the tree's top."""
    if not isinstance(value, str) or not value:
        return value
    if not paths.is_absolute(value):
        return value
    return paths.relative(root, value)


def prepare(draft: Draft) -> Prepared:
    """The submission's jobs and the files they read. Main thread: the last
    call that reads the scene."""
    host = draft.host
    problems: List[Problem] = []
    if draft.save_scene and (host.writes_scene or host.is_modified()):
        try:
            host.save()
        except Exception as e:  # noqa: BLE001 - every DCC raises its own
            problems.append(Problem("error", "scene", f"could not save the {host.scene_word}: {e}",
                                    f"save it yourself; the farm reads the saved {host.scene_word}"))
        draft.refresh()
    elif host.writes_scene and not draft.save_scene:
        problems.append(Problem("warning", "scene", f"the {host.scene_word} is written for each submission; with that off, "
                                "the farm renders the last one written"))
    elif host.is_modified():
        problems.append(Problem("warning", "scene", f"the {host.scene_word} has unsaved changes; the farm renders the saved file",
                                f"save the {host.scene_word} to render what you see"))
    tool = draft.tool
    keys = draft.keys()
    if not keys:
        problems.append(Problem("error", "renderables", "nothing is ticked to render", "tick at least one"))

    picked_names = {r.name for r in keys.values() if r is not None}
    key_of = {r.name: k for k, r in keys.items() if r is not None}
    outputs = {seq.canonical(o) for r in keys.values() if r is not None for o in r.outputs}

    jobs: Dict[str, dict] = {}
    extra: List[Dependency] = []
    for key, r in keys.items():
        override = {}
        for p in tool.params:
            if p.source and draft.form.field(p.name).linked:
                v = draft._derive(p.source, r)
                if v is not None:
                    override[p.name] = v
        values = draft.form.values(override)
        for p in tool.params:
            err = p.check(values.get(p.name, p.default))
            if err:
                problems.append(Problem("error", f"jobs.{key}.with.{p.name}", f"{p.label}: {err}"))
        for p in tool.params:
            if p.type != "PATH" or p.name not in values:
                continue
            v = values[p.name]
            if not draft.root:
                continue  # the collection says the project folder is not set
            rel = _to_tree_path(draft.root, v)
            if rel is None:
                problems.append(Problem("error", f"jobs.{key}.with.{p.name}",
                                        f"{p.label}: {v} is outside the project folder {draft.root}",
                                        "a job reads only its tree; move the file under the project folder"))
                continue
            if rel != v and p.data_flow == "IN" and p.object_type == "FILE" and v != draft.scene_file:
                extra.append(Dependency(path=v, node=p.label))
            values[p.name] = rel
        job: Dict[str, Any] = {"tool": tool.name}
        if tool.takes_frames:
            fr = draft.job_frames(r)
            if fr:
                job["frames"] = fr
            else:
                problems.append(Problem("error", f"jobs.{key}.frames", "no frames to render", "set a frame range"))
        if values:
            job["with"] = values
        if r is not None:
            after = [key_of[n] for n in r.reads if n in key_of and key_of[n] != key]
            if after:
                job["after"] = after[0] if len(after) == 1 else after
        if draft.size:
            job["size"] = draft.size
        jobs[key] = job

    doc: Dict[str, Any] = {"c4": submission.FORMAT}
    if draft.project:
        doc["project"] = draft.project
    if draft.shot:
        doc["shot"] = draft.shot
    else:
        problems.append(Problem("error", "shot", "which shot is this for?",
                                "fill in Shot, or set C4_SHOT (or C4_SHOT_PATTERN) in the studio's environment"))
    if draft.due and draft.due != "suggested":
        doc["due"] = draft.due
    farm_root = draft.project_info.get("root")
    if farm_root and draft.root and not paths.same(farm_root, draft.root):
        doc["root"] = draft.root
    if draft.note:
        doc["note"] = draft.note
    doc["jobs"] = jobs

    # Files: the scene, what it reads for the jobs ticked, and files typed
    # into fields; not what a job in this submission writes for another.
    deps = []
    for d in list(host.dependencies()) + extra:
        if d.renderables is not None and picked_names and not (set(d.renderables) & picked_names):
            continue
        if seq.canonical(d.path) in outputs:
            continue
        deps.append(d)
    col = collectlib.gather(draft.root, draft.scene_file, deps)
    return Prepared(draft=draft, doc=doc, collection=col, problems=problems, signature=draft.signature())


class Submitter:
    """Checks and submits prepared submissions against one farm."""

    def __init__(self, farm: Optional[Farm] = None, cache: Optional[IDCache] = None,
                 progress: Optional[Progress] = None, cancelled: Optional[Callable[[], bool]] = None,
                 upload_workers: int = 4):
        self._farm = farm
        self.cache = cache
        self.progress = progress
        self.cancelled = cancelled
        self.upload_workers = upload_workers

    @property
    def farm(self) -> Farm:
        if self._farm is None:
            self._farm = Farm()
        return self._farm

    def _tick(self, stage: str, done: int, total: int) -> None:
        if self.progress:
            self.progress(stage, done, total)
        if self.cancelled and self.cancelled():
            raise Cancelled()

    # --- reading the farm ---------------------------------------------------

    def tools_for(self, project: str, scene_file: str) -> List[Tool]:
        """The project's tools that run scenes like the open one. On a farm
        that does not list tools yet, the tool folders on ``C4_TOOL_PATH``
        stand in, so a panel can still draw its form and save the file."""
        try:
            tools = [Tool(self.farm.tool(project, t["name"])) for t in self.farm.tools(project)]
        except NotOnFarm:
            tools = local_tools()
            if not tools:
                raise
        return [t for t in tools if t.accepts_scene(scene_file)]

    @staticmethod
    def due_choices(project_info: Optional[dict]) -> List[Dict[str, str]]:
        """"Needed by": the project's milestones, then the farm's suggestion."""
        out = []
        for m in (project_info or {}).get("milestones") or []:
            label = m.get("label") or m["name"]
            out.append({"value": m["name"], "label": label, "at": m.get("at", "")})
        out.append({"value": "suggested", "label": "When the farm suggests", "at": ""})
        return out

    # --- check and submit -----------------------------------------------------

    def check(self, prepared: Prepared, explain: bool = False) -> Checked:
        """Names the tree, asks what the farm lacks and asks the farm to
        check the submission. Creates nothing."""
        col = prepared.collection
        if not col.errors and col.files and col.tree is None:
            collectlib.identify(col, self.cache, progress=self._tick, cancelled=self.cancelled)
        doc = dict(prepared.doc)
        tree_id = col.tree_id() if col.tree is not None and len(col.tree) else ""
        if tree_id:
            doc["tree"] = tree_id
        doc = {k: doc[k] for k in sorted(doc, key=lambda k: submission.TOP.index(k) if k in submission.TOP else len(submission.TOP))}
        out = Checked(prepared=prepared, doc=doc, tree_id=tree_id)
        for p in prepared.problems + col.problems:
            (out.errors.append(p.as_error()) if p.level == "error" else out.warnings.append(p.message))
        local = submission.check(doc)
        seen = {(e["path"], e["message"]) for e in out.errors}
        out.errors += [e for e in local if (e["path"], e["message"]) not in seen]
        if out.errors:
            return out
        project = doc.get("project", "")
        try:
            if project and col.files:
                by_id = {f.id: f for f in col.files.values()}
                out.missing = [i for i in self.farm.missing(project, list(by_id)) if i in by_id]
                out.missing_bytes = sum(by_id[i].size for i in out.missing)
            elif not project:
                out.warnings.append("no project is set, so the farm uses yours; what it lacks is counted when you submit")
            answer = self.farm.check(doc, explain=explain)
            out.answer = answer
            out.farm_checked = True
            for e in answer.get("errors") or []:
                out.errors.append({k: e[k] for k in ("path", "message", "fix") if k in e})
            out.warnings += [w if isinstance(w, str) else w.get("message", str(w)) for w in answer.get("warnings") or []]
            if not answer.get("ok", not answer.get("errors")) and not out.errors:
                out.errors.append({"path": "", "message": answer.get("message") or "the farm refused the submission"})
        except NotOnFarm as e:
            out.warnings.append(f"{e.message}; the file is checked here only")
        except FarmError as e:
            out.errors.append({"path": "", "message": e.message,
                               "fix": "check C4_HUD and your sign-in" if e.status in (0, 401, 403) else ""})
        return out

    def upload(self, checked: Checked) -> int:
        """Uploads what the farm lacks, then records the tree and checks its
        ID against the one computed here. Returns the bytes sent."""
        doc = checked.doc
        project = doc.get("project", "")
        col = checked.collection
        if not col.files:
            return 0
        if not project:
            raise FarmError(0, "a project is needed to upload the scene's files", None)
        by_id = {f.id: f for f in col.files.values()}
        todo = [by_id[i] for i in checked.missing if i in by_id]
        total = sum(f.size for f in todo)
        sent, lock = [0], threading.Lock()

        def put(f):
            if self.cancelled and self.cancelled():
                raise Cancelled()
            self.farm.put_file(project, f.id, f.abs)
            with lock:
                sent[0] += f.size
                self._tick("upload", sent[0], total)

        with ThreadPoolExecutor(max_workers=max(1, self.upload_workers)) as ex:
            list(ex.map(put, todo))
        view = self.farm.post_view(project, col.tree.view_request())
        if view != checked.tree_id:
            raise FarmError(0, f"the farm named the tree {view}, not {checked.tree_id} as computed here; nothing was submitted")
        return total

    def submit(self, checked: Checked) -> Submitted:
        """Uploads what the farm lacks and submits. Refuses a submission
        whose check found errors."""
        if checked.errors:
            raise FarmError(0, "the submission has errors; fix them and check again", checked.errors)
        if checked.stale():
            raise FarmError(0, "the scene or the form changed since the check; check again before submitting")
        sent = self.upload(checked)
        answer = self.farm.submit(checked.doc)
        if answer.get("ok") is False:
            raise FarmError(0, "the farm refused the submission", answer.get("errors"))
        return Submitted(checked=checked, answer=answer, uploaded=len(checked.missing), uploaded_bytes=sent)


def save(checked: Checked, path: str) -> str:
    """Writes the submission to a file and returns its path."""
    with open(path, "w", encoding="utf-8") as f:
        f.write(checked.yaml())
    return path


def default_filename(checked: Checked) -> str:
    """``<shot>-<first job>.yaml``, as SUBMIT.md names its examples."""
    doc = checked.doc
    first = next(iter(doc.get("jobs") or {"job": None}))
    return f"{doc.get('shot') or 'submission'}-{first}.yaml"

