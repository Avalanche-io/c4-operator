"""The open Blender file, as c4submit reads it.

Each view layer with "Use for Rendering" on is a renderable, so each one
ticked is a job. The active scene's view layers are ticked to begin with;
the other scenes' are listed unticked. A renderable's name is
``Scene/ViewLayer`` and its job key is made from the view layer's name. A
tool's ``from: scene.renderables`` parameters named ``scene`` and
``view_layer`` get the two parts; any other gets ``Scene/ViewLayer``. A
``from: scene.camera`` parameter gets the view layer's scene's camera.

Every file the scene reads is a dependency: textures (sequences with the
frames their users read, UDIM tile sets), linked libraries, movie clips,
sounds, Alembic and USD caches, OpenVDB volumes, fonts, and the sequencer's
strips, with ``//`` paths made absolute (a linked data-block's from its
library's folder). Packed files are inside the .blend, so they are not
dependencies. Whatever else ``bpy.utils.blend_paths`` lists (a fluid
cache's folder, a point cache) is added too, as optional. Which view
layers need a file is marked only when it is cheap to know: the
compositor's and sequencer's files belong to their scene's view layers;
everything else is read by every one.

Only ``bpy`` is imported, and only by the methods that need it, so the
module loads (and is tested) with a fake ``bpy`` in ``sys.modules``. Every
method runs on Blender's main thread, as c4submit's Host requires.
"""

import os
import re
from typing import Dict, Iterable, List, Optional, Tuple

from c4submit import frames as framelib
from c4submit import paths, seq
from c4submit.host import Dependency, Host, Renderable

#: The extension Blender adds for each output format when the scene's
#: "File Extensions" is on.
EXTENSIONS = {
    "BMP": ".bmp", "IRIS": ".rgb", "PNG": ".png", "JPEG": ".jpg", "JPEG2000": ".jp2", "TARGA": ".tga",
    "TARGA_RAW": ".tga", "CINEON": ".cin", "DPX": ".dpx", "OPEN_EXR_MULTILAYER": ".exr", "OPEN_EXR": ".exr",
    "HDR": ".hdr", "TIFF": ".tif", "WEBP": ".webp",
}
#: Output formats that write one movie file rather than a file per frame.
MOVIES = ("FFMPEG", "AVI_JPEG", "AVI_RAW")
#: Image sources that are not files on disk.
NOT_FILES = ("GENERATED", "VIEWER")

_DIGITS = re.compile(r"\d+")
_TILE = re.compile(r"(?<!\d)1\d{3}(?!\d)")


def _bpy():
    import bpy  # noqa: PLC0415 - only inside Blender (or a test's fake)
    return bpy


# --- paths ----------------------------------------------------------------------

def _split(path: str) -> Tuple[str, str, str]:
    """(folder with its slash, stem, extension) of a slash-separated path."""
    i = path.rfind("/") + 1
    head, base = path[:i], path[i:]
    stem, ext = os.path.splitext(base)
    return head, stem, ext


def sequence_pattern(path: str) -> str:
    """One frame's file of a sequence as a pattern: the last run of digits
    in the file's name as ``#``, which is how Blender finds a sequence's
    frame number (``smoke_0001.png`` is ``smoke_####.png``)."""
    if seq.has_frames(path):
        return path
    head, stem, ext = _split(path)
    found = list(_DIGITS.finditer(stem))
    if not found:
        return path
    m = found[-1]
    return head + stem[:m.start()] + "#" * len(m.group(0)) + stem[m.end():] + ext


def tile_pattern(path: str) -> str:
    """A UDIM tile set's path with its tile as ``<UDIM>``. Blender writes
    the token itself; a file from before it did names a tile (1001)."""
    if seq.has_tiles(path):
        return path
    head, stem, ext = _split(path)
    found = list(_TILE.finditer(stem))
    if not found:
        return path
    m = found[-1]
    return head + stem[:m.start()] + "<UDIM>" + stem[m.end():] + ext


def user_frames(first: int, last: int, step: int, start: int, offset: int, duration: int,
                cyclic: bool = False) -> List[int]:
    """The file frames a sequence's user reads over scene frames first to
    last: the sequence starts at scene frame ``start``, holds its first and
    last files outside its ``duration`` (or repeats, ``cyclic``), and file
    numbers are shifted by ``offset``."""
    out = set()
    for f in range(first, last + 1, max(1, step)):
        n = f - start + 1
        if duration > 0:
            n = (n - 1) % duration + 1 if cyclic else min(max(n, 1), duration)
        out.add(n + offset)
    return sorted(out)


def with_frame(path: str, ext: str) -> str:
    """An output path as Blender writes it: ``#`` for the frame, four of
    them added when the name has none, then the extension."""
    if not path:
        return ""
    head, stem, e = _split(path)
    base = stem + e
    if "#" not in base:
        base += "####"
    if ext and not base.lower().endswith(ext):
        base += ext
    return head + base


# --- the host -------------------------------------------------------------------

class BlenderHost(Host):
    """The open file, read through ``bpy``. ``scene`` names the scene the
    panel is in; the active one when it is not given or is gone."""

    name = "blender"
    stage = "render"
    scene_word = "file"

    def __init__(self, scene: str = "", env=None):
        super().__init__(env=env)
        self.scene_name = scene

    def scene(self):
        bpy = _bpy()
        if self.scene_name and self.scene_name in bpy.data.scenes:
            return bpy.data.scenes[self.scene_name]
        return bpy.context.scene

    def scenes(self) -> List:
        """The panel's scene first, then the file's other scenes (not ones
        linked from a library)."""
        me = self.scene()
        return [me] + [s for s in _bpy().data.scenes
                       if s.as_pointer() != me.as_pointer() and getattr(s, "library", None) is None]

    # --- the file -------------------------------------------------------

    def scene_file(self) -> str:
        return _bpy().data.filepath or ""

    def is_modified(self) -> bool:
        return bool(_bpy().data.is_dirty)

    def save(self) -> None:
        bpy = _bpy()
        if not bpy.data.filepath:
            raise RuntimeError("the file has never been saved; save it with File > Save As first")
        if "FINISHED" not in bpy.ops.wm.save_mainfile():
            raise RuntimeError("Blender did not save the file")

    def frames(self) -> str:
        return _span(self.scene())

    def camera(self) -> Optional[str]:
        cam = getattr(self.scene(), "camera", None)
        return cam.name if cam is not None else None

    # --- paths ------------------------------------------------------------

    def absolute(self, path: str, library=None) -> str:
        """A path as Blender reads it: ``//`` is the file's folder, or its
        library's for linked data; slashes forward."""
        path = (path or "").strip()
        if not path:
            return ""
        p = _bpy().path.abspath(path, library=library) if path.startswith("//") else path
        return paths.norm(p)

    def outputs(self, scene, view_layer: str = "", layers: int = 1) -> List[str]:
        """What rendering the scene writes, as patterns: the output path,
        and the compositor's File Output nodes. With ``view_layer`` and more
        than one of the scene's ``layers`` rendering, the output path is in a
        folder named for the layer, as the blender-render tool writes it
        (``render/bg/sh010_####.exr``), so each layer's job writes its own."""
        r = scene.render
        out = []
        fmt = r.image_settings.file_format
        if fmt not in MOVIES:
            p = with_frame(self.absolute(r.filepath), EXTENSIONS.get(fmt, "") if r.use_file_extension else "")
            if p and view_layer and layers > 1 and "/" in p:
                folder, name = p.rsplit("/", 1)
                p = f"{folder}/{view_layer}/{name}"
            if p:
                out.append(p)
        tree = compositor(scene)
        for n in (tree.nodes if tree is not None else []):
            if getattr(n, "bl_idname", "") != "CompositorNodeOutputFile" or getattr(n, "mute", False):
                continue
            base = self.absolute(getattr(n, "base_path", ""))
            node_fmt = n.format.file_format
            if node_fmt == "OPEN_EXR_MULTILAYER":
                out.append(with_frame(base, ".exr"))
                continue
            for slot in getattr(n, "file_slots", []):
                f = node_fmt if getattr(slot, "use_node_format", True) else slot.format.file_format
                if f in MOVIES:
                    continue
                out.append(with_frame(paths.norm(base.rstrip("/") + "/" + slot.path), EXTENSIONS.get(f, "")))
        return [o for o in out if o]

    # --- what renders -------------------------------------------------------

    def renderables(self) -> List[Renderable]:
        me = self.scene()
        scenes = self.scenes()
        used = [(s, vl) for s in scenes for vl in s.view_layers if vl.use]
        count: Dict[str, int] = {}
        for _, vl in used:
            count[vl.name] = count.get(vl.name, 0) + 1
        rendering: Dict[int, int] = {}
        for s, _ in used:
            rendering[s.as_pointer()] = rendering.get(s.as_pointer(), 0) + 1
        outs = {_name(s, vl): self.outputs(s, vl.name, rendering[s.as_pointer()]) for s, vl in used}
        # Who writes each pattern: one layer's job, or every layer's of a
        # scene (its File Output nodes).
        made: Dict[str, List[str]] = {}
        for s, vl in used:
            for o in outs[_name(s, vl)]:
                made.setdefault(seq.canonical(o), []).append(_name(s, vl))
        # A scene whose compositor reads another scene's render comes after
        # the jobs that write what it reads.
        after: Dict[int, List[str]] = {}
        for s in scenes:
            mine = [_name(s, vl) for vl in s.view_layers if vl.use]
            for img, _ in _compositor_images(s):
                if img.source in NOT_FILES or not img.filepath:
                    continue
                p = self.absolute(img.filepath, img.library)
                for by in made.get(seq.canonical(sequence_pattern(p) if img.source == "SEQUENCE" else p), []):
                    if by not in mine and by not in after.setdefault(s.as_pointer(), []):
                        after[s.as_pointer()].append(by)
        label = {}
        for s, vl in used:
            label[_name(s, vl)] = vl.name if s.as_pointer() == me.as_pointer() else f"{s.name} / {vl.name}"
        out = []
        for s, vl in used:
            name = _name(s, vl)
            reads = list(after.get(s.as_pointer(), []))
            notes = []
            if s.as_pointer() != me.as_pointer():
                notes.append(f"scene {s.name}")
            if reads:
                notes.append("after " + ", ".join(label[r] for r in reads))
            if s.render.image_settings.file_format in MOVIES:
                notes.append("writes a movie, not a file per frame")
            values = {"scene": s.name, "view_layer": vl.name}
            if getattr(s, "camera", None) is not None:
                values["scene.camera"] = s.camera.name
            out.append(Renderable(name=name, label=label[name], frames=_span(s), outputs=outs[name],
                                  reads=reads, enabled=s.as_pointer() == me.as_pointer(), note=" · ".join(notes),
                                  key=vl.name if count[vl.name] == 1 else f"{s.name}-{vl.name}", values=values))
        return out

    # --- what it reads --------------------------------------------------------

    def dependencies(self) -> List[Dependency]:
        bpy = _bpy()
        me = self.scene()
        scenes = self.scenes()
        layers_of = {s.as_pointer(): [_name(s, vl) for vl in s.view_layers if vl.use] for s in scenes}
        users = _image_users(scenes)
        out: List[Dependency] = []
        skipped: List[str] = []

        def scope(scenes_using: Iterable) -> Optional[List[str]]:
            names: List[str] = []
            for s in scenes_using:
                if s is None:
                    return None
                names += layers_of.get(s.as_pointer(), [])
            return names

        for img in bpy.data.images:
            if img.source in NOT_FILES or getattr(img, "type", "IMAGE") in ("RENDER_RESULT", "COMPOSITING") \
                    or not img.filepath:
                continue
            p = self.absolute(img.filepath, img.library)
            if _packed(img) or img.users == 0:
                skipped.append(p)
                continue
            uses = users.get(img.as_pointer(), [])
            frames = None
            if img.source == "SEQUENCE":
                p = sequence_pattern(p)
                frames = self._sequence_frames(me, uses)
            elif img.source == "TILED":
                p = tile_pattern(p)
            # An image no render node uses (a reference image, a brush) is
            # read only if something else needs it: missing, it is a warning.
            out.append(Dependency(p, frames, node=img.name, renderables=scope(s for s, _ in uses) if uses else None,
                                  optional=not uses))

        for lib in bpy.data.libraries:
            p = self.absolute(lib.filepath, getattr(lib, "parent", None))
            if _packed(lib):
                skipped.append(p)
                continue
            out.append(Dependency(p, node=lib.name))

        f0, f1, st = me.frame_start, me.frame_end, me.frame_step
        for clip in bpy.data.movieclips:
            # Read by the compositor's Movie Clip node, a camera's background
            # or stabilisation; a camera solve is kept in the file itself.
            p = self.absolute(clip.filepath, clip.library)
            if clip.users == 0:
                skipped.append(p)
                continue
            out.append(Dependency(sequence_pattern(p) if clip.source == "SEQUENCE" else p, node=clip.name,
                                  optional=True))
        for snd in bpy.data.sounds:
            p = self.absolute(snd.filepath, snd.library)
            if _packed(snd) or snd.users == 0:
                skipped.append(p)
                continue
            # Frames rendered to files do not play sound: missing is a warning.
            out.append(Dependency(p, node=snd.name, optional=True))
        for cf in bpy.data.cache_files:
            p = self.absolute(cf.filepath, cf.library)
            if cf.users == 0:
                skipped.append(p)
                continue
            out.append(Dependency(sequence_pattern(p) if getattr(cf, "is_sequence", False) else p, node=cf.name))
        for vol in bpy.data.volumes:
            p = self.absolute(vol.filepath, vol.library)
            if _packed(vol) or vol.users == 0:
                skipped.append(p)
                continue
            frames = None
            if getattr(vol, "is_sequence", False):
                p = sequence_pattern(p)
                mode = getattr(vol, "sequence_mode", "CLIP")
                if mode != "PING_PONG":
                    frames = framelib.format(user_frames(f0, f1, st, vol.frame_start, vol.frame_offset,
                                                         vol.frame_duration, cyclic=mode == "REPEAT"))
            out.append(Dependency(p, frames, node=vol.name))
        for font in bpy.data.fonts:
            if font.filepath == "<builtin>":
                continue
            p = self.absolute(font.filepath, font.library)
            if _packed(font) or font.users == 0:
                skipped.append(p)
                continue
            out.append(Dependency(p, node=font.name))

        for s in scenes:
            for strip in _strips(s):
                if getattr(strip, "mute", False):
                    continue
                if strip.type == "IMAGE":
                    folder = self.absolute(strip.directory).rstrip("/")
                    for e in strip.elements:
                        out.append(Dependency(folder + "/" + e.filename, node=strip.name,
                                              renderables=layers_of[s.as_pointer()]))
                elif strip.type == "MOVIE":
                    out.append(Dependency(self.absolute(strip.filepath), node=strip.name,
                                          renderables=layers_of[s.as_pointer()]))

        return out + self._the_rest(out, skipped, scenes)

    def _sequence_frames(self, me, uses) -> Optional[str]:
        """The file frames a sequence's users read over their scenes' frame
        ranges; None (every file) when a user's frames are not known."""
        if not uses:
            return None
        frames = set()
        for scene, user in uses:
            if user is None:
                return None
            s = scene if scene is not None else me
            frames.update(user_frames(s.frame_start, s.frame_end, s.frame_step, user.frame_start,
                                      user.frame_offset, user.frame_duration, bool(getattr(user, "use_cyclic", False))))
        return framelib.format(frames) if frames else None

    def _the_rest(self, found: List[Dependency], skipped: List[str], scenes) -> List[Dependency]:
        """Paths ``blend_paths`` lists that nothing above holds, as optional
        dependencies; a folder (a cache) as the files in it, unless files
        above are in it already (a sequencer strip's folder)."""
        known = [d.path for d in found] + skipped + [o for s in scenes for o in self.outputs(s)]
        known += [o for s in scenes for vl in s.view_layers if vl.use for o in self.outputs(s, vl.name, 2)]
        canon = {seq.canonical(p) for p in known}
        patterns = [p for p in known if seq.has_frames(p) or seq.has_tiles(p)]
        scene_file = paths.norm(self.scene_file())

        def covered(p: str) -> bool:
            return (seq.canonical(p) in canon or paths.same(p, scene_file)
                    or any(seq.matches(pat, p) for pat in patterns))

        out: List[Dependency] = []
        for raw in _bpy().utils.blend_paths(absolute=True):
            p = paths.norm(raw)
            if not p or covered(p):
                continue
            if os.path.isdir(p):
                if any(paths.relative(p, k) not in (None, ".") for k in known):
                    continue
                for top, _, names in os.walk(p):
                    for n in sorted(names):
                        f = paths.norm(os.path.join(top, n))
                        out.append(Dependency(f, node=os.path.basename(p.rstrip("/")) or p, optional=True))
                continue
            out.append(Dependency(p, node=os.path.basename(p) or p, optional=True))
            canon.add(seq.canonical(p))
        return out


# --- the file's data -----------------------------------------------------------------

def _name(scene, layer) -> str:
    return f"{scene.name}/{layer.name}"


def _span(scene) -> str:
    return framelib.span(int(scene.frame_start), int(scene.frame_end), int(scene.frame_step))


def _packed(idb) -> bool:
    return bool(getattr(idb, "packed_file", None)) or len(getattr(idb, "packed_files", None) or ()) > 0


def compositor(scene):
    """The scene's compositor node tree, when the compositor runs.
    Blender 5 keeps it in ``compositing_node_group``; 4.x in ``node_tree``
    with "Use Nodes" on."""
    tree = getattr(scene, "compositing_node_group", None)
    if tree is not None:
        return tree
    return scene.node_tree if getattr(scene, "use_nodes", False) else None


def _strips(scene) -> List:
    se = getattr(scene, "sequence_editor", None)
    if se is None:
        return []
    strips = getattr(se, "strips_all", None)
    if strips is None:
        strips = getattr(se, "sequences_all", None)
    return list(strips or [])


def _tree_images(tree, seen=None) -> List[Tuple]:
    """(image, its frame user or None) for each node in a tree, and in the
    node groups it uses, that reads an image."""
    seen = set() if seen is None else seen
    if tree is None or tree.as_pointer() in seen:
        return []
    seen.add(tree.as_pointer())
    out = []
    for n in tree.nodes:
        if getattr(n, "mute", False):
            continue
        img = getattr(n, "image", None)
        if img is not None:
            user = getattr(n, "image_user", None)
            if user is None and hasattr(n, "frame_duration"):
                user = n     # the compositor's Image node holds its frames itself
            out.append((img, user))
        sub = getattr(n, "node_tree", None)
        if sub is not None:
            out += _tree_images(sub, seen)
    return out


def _compositor_images(scene) -> List[Tuple]:
    return _tree_images(compositor(scene))


def _image_users(scenes) -> Dict[int, List[Tuple]]:
    """Image pointer to [(scene or None, frame user or None)]: who reads
    each image at render time. A scene is given for its compositor's
    images; None means every view layer may read it (a material, the
    world, a light, a texture, a geometry node group)."""
    bpy = _bpy()
    found: Dict[int, List[Tuple]] = {}

    def add(img, scene, user):
        found.setdefault(img.as_pointer(), []).append((scene, user))

    for coll in (bpy.data.materials, bpy.data.worlds, bpy.data.lights):
        for idb in coll:
            if idb.users == 0 or not getattr(idb, "use_nodes", True):
                continue
            for img, user in _tree_images(getattr(idb, "node_tree", None)):
                add(img, None, user)
    for group in bpy.data.node_groups:
        if group.users == 0 or getattr(group, "bl_idname", "") == "CompositorNodeTree":
            continue
        for img, user in _tree_images(group):
            add(img, None, user)
    for tex in bpy.data.textures:
        img = getattr(tex, "image", None)
        if tex.users and img is not None:
            add(img, None, getattr(tex, "image_user", None))
    for s in scenes:
        for img, user in _compositor_images(s):
            add(img, s, user)
    return found
