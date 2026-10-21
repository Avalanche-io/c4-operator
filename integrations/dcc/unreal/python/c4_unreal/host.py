"""The Unreal project and its Movie Render Queue, as c4submit reads them.

The "scene" is the project: its ``.uproject`` is the scene file, and the
files a render reads are the project's ``Config/``, ``Content/`` and
``Plugins/`` (never ``Saved/``, ``Intermediate/``, ``DerivedDataCache/`` or
``Binaries/``). Each enabled job in the queue is a renderable, so each one
is a job on the farm, with its map, level sequence and config as its own
values for the tool's fields.

A job's config (its render settings, as the queue holds them) goes to the
farm as a queue manifest of that job alone, written to
``C4/queue/<nn>-<job>.utxt`` in the project when the submission is
prepared; the ``config`` value is that file's path from the project's
folder.

Content is all of ``Content/`` (and each project plug-in's), unless
``C4_UNREAL_CONTENT=referenced``: then only the packages the jobs' maps,
their World Partition actors, their sequences and the project's config
reference, as the asset registry lists them, plus every file in
``Content/`` that is not a package (movies, data). Either way, C4 names
each file by its content, so a second submission uploads only the packages
that changed.

Only ``unreal`` is imported, and only by the methods that need it, so the
module loads (and is tested) with a fake ``unreal`` in ``sys.modules``.
Every method runs on the editor's main thread, as c4submit's Host requires.
"""

import json
import os
import re
from typing import Dict, Iterable, List, Optional, Set, Tuple

from c4submit import frames as framelib
from c4submit import paths
from c4submit.host import Dependency, Host, Renderable

#: Folders that are this machine's, never the farm's.
EXCLUDED = ("Saved", "Intermediate", "DerivedDataCache", "Binaries", ".git", ".svn", ".vs", ".idea", "__pycache__")
#: Files a package is saved as, and the files saved beside it.
PACKAGE_EXTS = (".umap", ".uasset")
SIDECAR_EXTS = (".uexp", ".ubulk", ".uptnl", ".m.ubulk")
#: Where the queue's jobs are written for the farm, in the project.
QUEUE_FOLDER = "C4/queue"

_GAME_REF = re.compile(r"/Game/[A-Za-z0-9_\-/]+")


def _unreal():
    import unreal  # noqa: PLC0415 - only inside Unreal (or a test's fake)
    return unreal


def _slug(s: str) -> str:
    return re.sub(r"-+", "-", re.sub(r"[^a-z0-9]+", "-", (s or "").lower())).strip("-") or "job"


def soft_path(p) -> str:
    """A soft object path as text: ``/Game/Maps/Sh010.Sh010``."""
    if p is None:
        return ""
    for name in ("export_text", "to_string"):
        fn = getattr(p, name, None)
        if callable(fn):
            try:
                s = fn()
            except Exception:  # noqa: BLE001 - an older Unreal names it otherwise
                continue
            if isinstance(s, str):
                return "" if s == "None" else s
    s = str(p)
    return "" if s == "None" else s


def package_of(object_path: str) -> str:
    """``/Game/Maps/Sh010.Sh010`` is in package ``/Game/Maps/Sh010``."""
    head, _, tail = object_path.rpartition("/")
    return head + "/" + tail.split(".", 1)[0] if head else object_path.split(".", 1)[0]


def walk_files(top: str, skip: Iterable[str] = EXCLUDED) -> Iterable[str]:
    """Every file under ``top``, leaving out this machine's folders and
    hidden files, in a stable order."""
    skip = set(skip)
    for d, dirs, names in os.walk(top):
        dirs[:] = sorted(x for x in dirs if x not in skip and not x.startswith("."))
        for n in sorted(names):
            if not n.startswith("."):
                yield paths.norm(os.path.join(d, n))


def is_package(path: str) -> bool:
    low = path.lower()
    return low.endswith(PACKAGE_EXTS) or low.endswith(SIDECAR_EXTS)


class UnrealHost(Host):
    """The open project and one Movie Render Queue."""

    name = "unreal"
    stage = "render"
    scene_word = "project"
    #: The jobs' queue manifests are written for every submission.
    writes_scene = True

    def __init__(self, queue, env=None):
        super().__init__(env=env)
        self.queue = queue

    # --- the project ------------------------------------------------------

    def scene_file(self) -> str:
        U = _unreal()
        p = U.Paths.get_project_file_path()
        return paths.norm(U.Paths.convert_relative_path_to_full(p)) if p else ""

    def project_dir(self) -> str:
        f = self.scene_file()
        return os.path.dirname(f) if f else ""

    def dirty(self) -> List[str]:
        """Packages with unsaved changes, by name."""
        U = _unreal()
        lib = U.EditorLoadingAndSavingUtils
        out = []
        for fn in (lib.get_dirty_map_packages, lib.get_dirty_content_packages):
            for pkg in fn() or []:
                out.append(str(pkg.get_name() if hasattr(pkg, "get_name") else pkg))
        return sorted(out)

    def is_modified(self) -> bool:
        return bool(self.dirty())

    def signature(self) -> str:
        return f"{self.scene_file()}|{','.join(self.dirty())}"

    def save(self) -> None:
        """Saves what is unsaved (asking, as Unreal does), then writes each
        job's queue manifest for the farm."""
        U = _unreal()
        if self.is_modified():
            U.EditorLoadingAndSavingUtils.save_dirty_packages_with_dialog(True, True)
            if self.is_modified():
                raise RuntimeError("some maps or assets were not saved; the farm renders what is saved, "
                                   "so save them (File > Save All) and render again")
        self.write_manifests()

    # --- the queue -------------------------------------------------------------

    def jobs(self) -> List[Tuple[str, object]]:
        """(manifest name, job) for each enabled job, in the queue's order."""
        out = []
        for i, job in enumerate(self.queue.get_jobs() if self.queue is not None else []):
            on = getattr(job, "is_enabled", None)
            if callable(on) and not on():
                continue
            out.append((f"{i + 1:02d}-{_slug(job.job_name)}", job))
        return out

    def manifest(self, stem: str) -> str:
        """A job's manifest, from the project's folder."""
        return f"{QUEUE_FOLDER}/{stem}.utxt"

    def write_manifests(self) -> List[str]:
        """Writes each enabled job alone as a queue manifest, the file a
        render on the farm is given (``-MoviePipelineConfig``)."""
        U = _unreal()
        lib = U.MoviePipelineEditorLibrary
        written = []
        folder = os.path.join(self.project_dir(), *QUEUE_FOLDER.split("/"))
        os.makedirs(folder, exist_ok=True)
        for stem, job in self.jobs():
            one = U.new_object(U.MoviePipelineQueue)
            one.duplicate_job(job)
            saved = lib.save_queue_to_manifest_file(one)
            where = next((x for x in (saved if isinstance(saved, (tuple, list)) else [saved]) if isinstance(x, str)), "")
            text = lib.convert_manifest_file_to_string(where)
            path = os.path.join(folder, stem + ".utxt")
            with open(path, "w", encoding="utf-8") as f:
                f.write(text)
            written.append(paths.norm(path))
        return written

    def _frames(self, job) -> Optional[str]:
        """The job's custom range when its output settings have one, else
        its sequence's playback range. Both end before their end frame."""
        U = _unreal()
        cfg = job.get_configuration()
        out = cfg.find_setting_by_class(U.MoviePipelineOutputSetting) if cfg is not None else None
        if out is not None and getattr(out, "use_custom_playback_range", False):
            return framelib.span(int(out.custom_start_frame), int(out.custom_end_frame) - 1)
        seq = U.load_asset(soft_path(job.sequence)) if soft_path(job.sequence) else None
        if seq is None:
            return None
        return framelib.span(int(seq.get_playback_start()), int(seq.get_playback_end()) - 1)

    def renderables(self) -> List[Renderable]:
        out, seen = [], {}
        for stem, job in self.jobs():
            name = job.job_name or stem
            n = seen[name] = seen.get(name, 0) + 1
            if n > 1:
                name = f"{name} ({n})"
            seq = soft_path(job.sequence)
            out.append(Renderable(
                name=name, label=name, frames=self._frames(job), key=name,
                values={"map": package_of(soft_path(job.map)), "sequence": seq, "config": self.manifest(stem)},
                note=os.path.basename(package_of(seq)) if seq else ""))
        return out

    def frames(self) -> str:
        rs = self.renderables()
        return next((r.frames for r in rs if r.frames), "")

    def read(self):
        rs = self.renderables()
        return next((r.frames for r in rs if r.frames), ""), rs

    def shot(self) -> str:
        """The shot as Host gives it, else ``C4_SHOT_PATTERN`` matched
        against each job's sequence and map (a project holds many shots)."""
        s = super().shot()
        pat = self.env.get("C4_SHOT_PATTERN")
        if s or not pat:
            return s
        for _, job in self.jobs():
            for p in (soft_path(job.sequence), soft_path(job.map)):
                m = re.search(pat, p)
                if m and m.groupdict().get("shot"):
                    return m.group("shot")
        return ""

    # --- what it reads -------------------------------------------------------------

    def mounts(self) -> Dict[str, str]:
        """Content mount point to its folder: ``/Game/`` and each project
        plug-in's ``/<Plugin>/``."""
        proj = self.project_dir()
        out = {"/Game/": paths.norm(os.path.join(proj, "Content"))}
        plugins = os.path.join(proj, "Plugins")
        for f in walk_files(plugins) if os.path.isdir(plugins) else []:
            if f.lower().endswith(".uplugin"):
                stem = os.path.splitext(os.path.basename(f))[0]
                out[f"/{stem}/"] = paths.norm(os.path.join(os.path.dirname(f), "Content"))
        return out

    def package_files(self, package: str, mounts: Dict[str, str]) -> Optional[List[str]]:
        """The files a package is saved in; [] when it should be in the
        project but is not there; None when it is not the project's (the
        engine's, an engine plug-in's, a C++ class)."""
        for mount, folder in mounts.items():
            if not package.startswith(mount):
                continue
            base = folder + "/" + package[len(mount):]
            found = [base + e for e in PACKAGE_EXTS if os.path.isfile(base + e)]
            found += [base + e for e in SIDECAR_EXTS if os.path.isfile(base + e)]
            return found
        return None

    def file_package(self, path: str, mounts: Dict[str, str]) -> str:
        for mount, folder in mounts.items():
            rel = paths.relative(folder, path)
            if rel not in (None, "."):
                stem = rel
                for e in SIDECAR_EXTS + PACKAGE_EXTS:
                    if stem.lower().endswith(e):
                        stem = stem[:-len(e)]
                        break
                return mount + stem
        return ""

    def dependencies(self) -> List[Dependency]:
        proj = self.project_dir()
        if not proj:
            return []
        out: List[Dependency] = []
        for top in ("Config", "Plugins"):
            d = os.path.join(proj, top)
            for f in walk_files(d) if os.path.isdir(d) else []:
                if top == "Plugins" and "/Content/" in f[len(proj):] and is_package(f) and self._referenced_only():
                    continue
                out.append(Dependency(f, node=top))
        by_config = {r.values.get("config"): r.name for r in self.renderables()}
        out += [Dependency(paths.norm(os.path.join(proj, self.manifest(stem))), node=job.job_name,
                           renderables=[by_config[self.manifest(stem)]])
                for stem, job in self.jobs()]
        self._warnings = self._code_warnings()
        mode = self.env.get("C4_UNREAL_CONTENT", "all").strip().lower()
        if mode not in ("all", "referenced", ""):
            self._warnings.append(f'C4_UNREAL_CONTENT is "{mode}", which is neither "all" nor "referenced"; '
                                  "all of Content is sent")
        content = os.path.join(proj, "Content")
        if self._referenced_only():
            try:
                return out + self._referenced(content)
            except Exception as e:  # noqa: BLE001 - the asset registry failed: send all of Content
                msg = f"the asset registry could not list what the jobs read ({e}), so all of Content is sent"
                self._warnings.append(msg)
                _unreal().log_warning("C4: " + msg)
        out += [Dependency(f, node="Content") for f in (walk_files(content) if os.path.isdir(content) else [])]
        return out

    def warnings(self) -> List[str]:
        """What the artist should know about how the project was read, as
        of the last ``dependencies()``."""
        return list(getattr(self, "_warnings", []))

    def _code_warnings(self) -> List[str]:
        """The project's and its plug-ins' C++ modules: their Binaries are
        never sent, so the farm's Unreal must build them or have them."""
        found = []
        proj = self.project_dir()
        files = [self.scene_file()]
        plugins = os.path.join(proj, "Plugins")
        files += [f for f in (walk_files(plugins) if os.path.isdir(plugins) else []) if f.lower().endswith(".uplugin")]
        for f in files:
            try:
                with open(f, encoding="utf-8") as fh:
                    modules = json.load(fh).get("Modules") or []
            except (OSError, ValueError, AttributeError):
                continue
            if modules:
                found.append(os.path.splitext(os.path.basename(f))[0])
        if not found:
            return []
        return [f"{', '.join(found)} {'has' if len(found) == 1 else 'have'} C++ code; Binaries are never sent, "
                "so the farm's Unreal must build it or have it already"]

    def _referenced_only(self) -> bool:
        return self.env.get("C4_UNREAL_CONTENT", "all").strip().lower() == "referenced"

    def _referenced(self, content: str) -> List[Dependency]:
        """The packages each job reaches through the asset registry, as
        dependencies of those jobs, and every file in Content that is not a
        package."""
        U = _unreal()
        reg = U.AssetRegistryHelpers.get_asset_registry()
        opts = U.AssetRegistryDependencyOptions(include_soft_package_references=True,
                                                include_hard_package_references=True,
                                                include_searchable_names=False,
                                                include_soft_management_references=True,
                                                include_hard_management_references=True)
        mounts = self.mounts()
        from_config = self._config_packages()
        reached: Dict[str, Set[str]] = {}
        names = {stem: r.name for (stem, _), r in zip(self.jobs(), self.renderables())}
        for stem, job in self.jobs():
            who = names[stem]
            map_pkg = package_of(soft_path(job.map))
            todo = [map_pkg, map_pkg + "_BuiltData", package_of(soft_path(job.sequence))] + from_config
            todo += self._external(map_pkg, mounts)
            seen: Set[str] = set()
            while todo:
                pkg = todo.pop()
                if not pkg or pkg in seen:
                    continue
                seen.add(pkg)
                if self.package_files(pkg, mounts) is None:
                    continue    # the engine's: the farm's Unreal has it
                reached.setdefault(pkg, set()).add(who)
                for dep in reg.get_dependencies(pkg, opts) or []:
                    todo.append(str(dep))
        out: List[Dependency] = []
        for pkg in sorted(reached):
            files = self.package_files(pkg, mounts)
            jobs = sorted(reached[pkg])
            if not files:
                if pkg.endswith("_BuiltData"):
                    continue    # a map without built lighting has none
                out.append(Dependency(mounts_path(pkg, mounts) + ".uasset", node=pkg, renderables=jobs, optional=True))
                continue
            out += [Dependency(f, node=pkg, renderables=jobs) for f in files]
        # The plug-ins' other files are sent with Plugins/ already.
        for f in walk_files(content) if os.path.isdir(content) else []:
            if not is_package(f):
                out.append(Dependency(f, node="Content"))
        return out

    def _external(self, map_pkg: str, mounts: Dict[str, str]) -> List[str]:
        """A World Partition map's actors and objects, saved as packages of
        their own in ``__ExternalActors__`` and ``__ExternalObjects__``."""
        out = []
        for mount, folder in mounts.items():
            if not map_pkg.startswith(mount):
                continue
            rel = map_pkg[len(mount):]
            for kind in ("__ExternalActors__", "__ExternalObjects__"):
                d = os.path.join(folder, kind, *rel.split("/"))
                for f in walk_files(d) if os.path.isdir(d) else []:
                    if f.lower().endswith(PACKAGE_EXTS):
                        out.append(self.file_package(f, mounts))
        return out

    def _config_packages(self) -> List[str]:
        """Packages the project's config names (a game mode, default
        maps and materials), which a render loads without a map naming them."""
        out = []
        d = os.path.join(self.project_dir(), "Config")
        for f in walk_files(d) if os.path.isdir(d) else []:
            if not f.lower().endswith(".ini"):
                continue
            try:
                with open(f, encoding="utf-8", errors="replace") as fh:
                    text = fh.read()
            except OSError:
                continue
            out += [package_of(m.group(0)) for m in _GAME_REF.finditer(text)]
        return sorted(set(out))


def mounts_path(package: str, mounts: Dict[str, str]) -> str:
    for mount, folder in mounts.items():
        if package.startswith(mount):
            return folder + "/" + package[len(mount):]
    return package
