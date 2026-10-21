"""A fake ``unreal``, with only what c4_unreal uses.

The project is real files on disk (``state.project``); its packages' asset
registry dependencies, its dirty packages, its level sequences and its
Movie Render Queue are plain objects a test builds. Dialogs answer from a
script: ``state.answers`` for message boxes (``"yes"``, ``"no"``,
``"cancel"``, ``"ok"``), ``state.on_options`` to edit the details panel
before it is closed, ``state.cancel_after`` to press a progress dialog's
Cancel. Every dialog shown is kept in ``state.shown``.

``install()`` puts the fake in ``sys.modules`` as ``unreal``;
``uninstall()`` takes it out again, so no other folder's tests see it.
What the fake cannot prove about the real ``unreal`` is in CHECKLIST.md.
"""

import copy
import os
import sys
import types
from typing import Any, Callable, Dict, List, Optional

# --- Python types for Unreal --------------------------------------------------------

uprops: Dict[type, Dict[str, dict]] = {}


class _UProp:
    def __init__(self, typ, meta=None):
        self.typ, self.meta = typ, dict(meta or {})


def uproperty(typ, meta=None, **kw):
    return _UProp(typ, meta)


def uclass(**kw):
    def wrap(cls):
        props = {}
        for k, v in list(vars(cls).items()):
            if isinstance(v, _UProp):
                props[k] = v.meta
                setattr(cls, k, v.typ())
        uprops[cls] = props
        return cls
    return wrap


def ufunction(**kw):
    def wrap(fn):
        fn._ufunction = kw
        return fn
    return wrap


def Array(typ):  # noqa: N802 - Unreal's name
    return list


class Object:
    pass


def new_object(cls, outer=None, name=None):
    return cls()


def get_default_object(cls):
    return cls()


# --- the movie render queue ------------------------------------------------------------

class SoftObjectPath:
    def __init__(self, path: str = ""):
        self.path = path

    def export_text(self) -> str:
        return self.path or "None"


class MoviePipelineOutputSetting(Object):
    def __init__(self, use_custom_playback_range=False, custom_start_frame=0, custom_end_frame=0):
        self.use_custom_playback_range = use_custom_playback_range
        self.custom_start_frame, self.custom_end_frame = custom_start_frame, custom_end_frame


class MoviePipelinePrimaryConfig(Object):
    def __init__(self, settings=()):
        self.settings = list(settings)

    def find_setting_by_class(self, cls):
        return next((s for s in self.settings if isinstance(s, cls)), None)


class MoviePipelineExecutorJob(Object):
    def __init__(self, job_name="", map="", sequence="", config=None, enabled=True):  # noqa: A002
        self.job_name = job_name
        self.map, self.sequence = SoftObjectPath(map), SoftObjectPath(sequence)
        self._config = config or MoviePipelinePrimaryConfig([MoviePipelineOutputSetting()])
        self._enabled = enabled

    def get_configuration(self):
        return self._config

    def is_enabled(self) -> bool:
        return self._enabled

    def set_is_enabled(self, on: bool) -> None:
        self._enabled = bool(on)


class MoviePipelineQueue(Object):
    def __init__(self, jobs=()):
        self.jobs = list(jobs)

    def get_jobs(self):
        return list(self.jobs)

    def duplicate_job(self, job):
        j = copy.copy(job)
        self.jobs.append(j)
        return j


class MoviePipelineExecutorBase(Object):
    def on_executor_finished_impl(self):
        state.finished += 1


class MoviePipelineEditorLibrary:
    @staticmethod
    def save_queue_to_manifest_file(queue):
        path = os.path.join(state.project, "Saved", "MovieRenderPipeline", "QueueManifest.utxt")
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            for j in queue.get_jobs():
                out = j.get_configuration().find_setting_by_class(MoviePipelineOutputSetting)
                f.write(f"Begin Object Class=/Script/MovieRenderPipelineCore.MoviePipelineExecutorJob Name={j.job_name}\n"
                        f"  Map={j.map.path}\n  Sequence={j.sequence.path}\n"
                        f"  Custom={getattr(out, 'use_custom_playback_range', False)}\nEnd Object\n")
        state.manifests += 1
        return path, queue

    @staticmethod
    def convert_manifest_file_to_string(path):
        with open(path, encoding="utf-8") as f:
            return f.read()


class LevelSequence(Object):
    def __init__(self, start: int, end: int):
        self.start, self.end = start, end

    def get_playback_start(self):
        return self.start

    def get_playback_end(self):
        return self.end   # as Unreal's: one past the last frame


def load_asset(path):
    return state.assets.get(path)


# --- the editor ---------------------------------------------------------------------------

class Paths:
    @staticmethod
    def get_project_file_path():
        return state.project_file

    @staticmethod
    def convert_relative_path_to_full(p):
        return os.path.abspath(p)


class Package:
    def __init__(self, name):
        self.name = name

    def get_name(self):
        return self.name


class EditorLoadingAndSavingUtils:
    @staticmethod
    def get_dirty_map_packages():
        return [Package(n) for n in state.dirty if n.startswith("/Game/Maps/")]

    @staticmethod
    def get_dirty_content_packages():
        return [Package(n) for n in state.dirty if not n.startswith("/Game/Maps/")]

    @staticmethod
    def save_dirty_packages_with_dialog(save_map_packages, save_content_packages):
        state.save_dialogs += 1
        if state.refuse_save:
            return False
        for n in state.dirty:
            p = os.path.join(state.project, "Content", n[len("/Game/"):] + ".uasset")
            if not os.path.isfile(p):
                p = p[:-len(".uasset")] + ".umap"
            with open(p, "ab") as f:
                f.write(b" saved")
        state.dirty = []
        return True


class AssetRegistryDependencyOptions:
    def __init__(self, **kw):
        self.__dict__.update(kw)


class _Registry:
    def get_dependencies(self, package_name, dependency_options):
        if state.registry_fails:
            raise RuntimeError("the asset registry is still loading")
        state.registry_asked.append(str(package_name))
        return list(state.deps.get(str(package_name), [])) or None


class AssetRegistryHelpers:
    @staticmethod
    def get_asset_registry():
        return _Registry()


class AppMsgType:
    OK, YES_NO, OK_CANCEL, YES_NO_CANCEL = "OK", "YES_NO", "OK_CANCEL", "YES_NO_CANCEL"


class AppReturnType:
    NO, YES, YES_ALL, NO_ALL, CANCEL, OK, RETRY, CONTINUE = ("NO", "YES", "YES_ALL", "NO_ALL", "CANCEL", "OK",
                                                             "RETRY", "CONTINUE")


class EditorDialogLibraryObjectDetailsViewOptions:
    def __init__(self):
        self.show_object_name = True
        self.allow_search = True


class EditorDialog:
    @staticmethod
    def show_message(title, message, message_type, default_value=None):
        state.shown.append(("message", title, message, message_type))
        if not state.answers:
            raise AssertionError(f"no answer scripted for: {title}\n{message}")
        a = state.answers.pop(0)
        if callable(a):   # the artist does something while the dialog is open
            a = a(title, message)
        allowed = {"OK": ["ok"], "YES_NO": ["yes", "no"], "YES_NO_CANCEL": ["yes", "no", "cancel"],
                   "OK_CANCEL": ["ok", "cancel"]}[message_type]
        assert a in allowed, f"{a!r} is not a button of {message_type}: {title}"
        return a.upper()

    @staticmethod
    def show_object_details_view(title, obj, options=None):
        state.shown.append(("details", title, obj, options))
        state.options_seen.append({k: getattr(obj, k) for k in uprops.get(type(obj), {})})
        if state.on_options:
            state.on_options(obj)
        return state.options_ok


class ScopedSlowTask:
    def __init__(self, work, desc=""):
        self.work, self.desc = work, desc
        self.frames: List[str] = []
        self.ticks = 0

    def __enter__(self):
        state.tasks.append(self)
        return self

    def __exit__(self, *a):
        return False

    def make_dialog(self, can_cancel=False, allow_in_pie=False):
        self.can_cancel = can_cancel

    def enter_progress_frame(self, work=1.0, desc=""):
        self.frames.append(desc)
        self.ticks += 1

    def should_cancel(self):
        return state.cancel_after is not None and self.ticks >= state.cancel_after


def log(msg):
    state.logs.append(("log", str(msg)))


def log_warning(msg):
    state.logs.append(("warning", str(msg)))


def log_error(msg):
    state.logs.append(("error", str(msg)))


# --- state and installing -------------------------------------------------------------------

class State:
    def __init__(self):
        self.project = ""
        self.project_file = ""
        self.deps: Dict[str, List[str]] = {}
        self.dirty: List[str] = []
        self.assets: Dict[str, Any] = {}
        self.answers: List[str] = []
        self.on_options: Optional[Callable[[Any], None]] = None
        self.options_ok = True
        self.options_seen: List[dict] = []
        self.cancel_after: Optional[int] = None
        self.refuse_save = False
        self.registry_fails = False
        self.registry_asked: List[str] = []
        self.shown: List[tuple] = []
        self.tasks: List[ScopedSlowTask] = []
        self.logs: List[tuple] = []
        self.finished = 0
        self.save_dialogs = 0
        self.manifests = 0

    def messages(self) -> List[str]:
        return [m[2] for m in self.shown if m[0] == "message"]


state = State()
unreal = None


def reset(project: str = "") -> State:
    state.__init__()
    state.project = project
    if project:
        state.project_file = os.path.join(project, os.path.basename(project) + ".uproject")
    return state


def _build():
    m = types.ModuleType("unreal")
    m.__file__ = __file__
    me = sys.modules[__name__]
    for name in ("uproperty", "uclass", "ufunction", "Array", "Object", "new_object", "get_default_object",
                 "SoftObjectPath", "MoviePipelineOutputSetting", "MoviePipelinePrimaryConfig",
                 "MoviePipelineExecutorJob", "MoviePipelineQueue", "MoviePipelineExecutorBase",
                 "MoviePipelineEditorLibrary", "LevelSequence", "load_asset", "Paths", "EditorLoadingAndSavingUtils",
                 "AssetRegistryDependencyOptions", "AssetRegistryHelpers", "AppMsgType", "AppReturnType",
                 "EditorDialogLibraryObjectDetailsViewOptions", "EditorDialog", "ScopedSlowTask", "log",
                 "log_warning", "log_error"):
        setattr(m, name, getattr(me, name))
    return m


def install():
    global unreal
    if unreal is None:
        unreal = _build()
    sys.modules["unreal"] = unreal
    return unreal


def uninstall():
    if sys.modules.get("unreal") is unreal:
        sys.modules.pop("unreal", None)
