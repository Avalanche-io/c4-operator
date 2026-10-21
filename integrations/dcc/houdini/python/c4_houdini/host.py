"""The open hip file, as c4submit reads it, for the two C4 nodes.

- ``LopHost`` is the C4 Render LOP's: the stage wired into the node is
  written to a USD file, which is what the farm renders, and each render
  settings prim on the stage is a job to tick.
- ``RopHost`` is the C4 Submit ROP's: the saved hip file is what the farm
  opens, and each ROP wired into the node is a job, after the ROPs it reads.

Every call here runs on Houdini's main thread (c4submit's ``prepare``).
"""

import os
import re
from typing import Dict, List, Mapping, Optional, Set, Tuple

import hou

from c4submit import frames as framelib
from c4submit import paths, seq
from c4submit import usd as usdlib
from c4submit.host import Dependency, Host, Renderable

# Houdini's frame variables. They are kept as written when a path is
# expanded, so a sequence stays a sequence that c4submit can read.
_FRAME_VAR = re.compile(r"\$\{F\d*\}|\$F\d*(?![A-Za-z_0-9])")

# A path with a scheme (op:, opdef:, temp:, http:) is not a file on disk.
# One letter before the colon is a Windows drive, which is.
_SCHEME = re.compile(r"^[A-Za-z][A-Za-z0-9+.-]+:")

# The output parameters of the ROPs Houdini ships, in the order they are
# looked for. Studio ROPs are found by their file chooser's write mode.
OUTPUT_PARMS = ("vm_picture", "picture", "outputimage", "lopoutput", "sopoutput", "copoutput", "dopoutput",
                "filename", "vm_dcmfilename")

# ROPs that render nothing themselves; their inputs are read through them.
PASS_THROUGH = ("merge", "null")


def expand(path: str) -> str:
    """A path with Houdini's variables expanded ($HIP, $JOB, $OS, `...`),
    except its frame variables, so ``$HIP/geo/fx.$F4.bgeo.sc`` stays a
    sequence."""
    if not path:
        return ""
    held: List[str] = []

    def hold(m: "re.Match") -> str:
        held.append(m.group(0))
        return "C4KEEP%dX" % (len(held) - 1)

    out = hou.expandString(_FRAME_VAR.sub(hold, path))
    for i, token in enumerate(held):
        out = out.replace("C4KEEP%dX" % i, token)
    return out


def is_rop(node) -> bool:
    try:
        return node.type().category().name() == "Driver"
    except AttributeError:
        return False


def writes(parm) -> bool:
    """Whether a parameter names a file its node writes, not one it reads."""
    try:
        tags = parm.parmTemplate().tags()
    except (AttributeError, hou.Error):
        tags = {}
    if tags.get("filechooser_mode") == "write":
        return True
    return parm.name() in OUTPUT_PARMS and is_rop(parm.node())


def output_parms(rop) -> list:
    """A ROP's output parameters: the known names, then any file parameter
    whose chooser writes."""
    out, names = [], set()
    for n in OUTPUT_PARMS:
        p = rop.parm(n)
        if p is not None:
            out.append(p)
            names.add(n)
    for p in rop.parms():
        if p.name() in names:
            continue
        try:
            t = p.parmTemplate()
            if t.type() != hou.parmTemplateType.String or t.stringType() != hou.stringParmType.FileReference:
                continue
        except (AttributeError, hou.Error):
            continue
        if writes(p):
            out.append(p)
            names.add(p.name())
    return out


def unexpanded(parm) -> str:
    """A parameter's value as typed, with its variables."""
    try:
        return parm.unexpandedString()
    except hou.Error:
        return parm.evalAsString()  # an expression or a non-string parameter: its value now


def lists_for(tool) -> str:
    """What the C4 Submit ROP lists for a tool: TOP networks ("tops") when
    the parameter it fills from the renderables is named ``top`` (as
    pdg-cook's), else ROPs ("rops")."""
    for p in getattr(tool, "params", []):
        if p.source == "scene.renderables":
            return "tops" if p.name == "top" else "rops"
    return "rops"


def top_networks() -> list:
    """Every TOP network in the hip file (a ``topnet`` node in any context:
    /obj, /out, /tasks, inside SOPs), by path."""
    found = {}
    for cat in hou.nodeTypeCategories().values():
        t = hou.nodeType(cat, "topnet")
        if t is None:
            continue
        for n in t.instances():
            found[n.path()] = n
    return [found[p] for p in sorted(found)]


def top_output(net):
    """A TOP network's output node, the one with the display flag (what a
    cook of the network cooks); None when no node has it."""
    try:
        shown = net.displayNode()
    except (AttributeError, hou.Error):
        shown = None
    if shown is not None:
        return shown
    for n in net.children():
        try:
            if n.isDisplayFlagSet():
                return n
        except (AttributeError, hou.Error):
            continue
    return None


def _bypassed(node) -> bool:
    try:
        return bool(node.isBypassed())
    except (AttributeError, hou.Error):
        return False


class _HoudiniHost(Host):
    """What both nodes read the same way: the hip file, the shot, and which
    paths are not the project's files."""

    name = "houdini"

    def __init__(self, node, env: Optional[Mapping[str, str]] = None):
        super().__init__(env=env)
        self.node = node

    def hip(self) -> str:
        """The hip file's path; '' when it was never saved."""
        if hou.hipFile.isNewFile():
            return ""
        return paths.norm(hou.hipFile.path())

    def hip_dir(self) -> str:
        return os.path.dirname(self.hip()) or hou.expandString("$HIP")

    def shot(self) -> str:
        """``C4_SHOT``, ``SHOT``, or ``C4_SHOT_PATTERN`` matched against the
        hip file's path (the C4 Render node's USD file is named after the
        shot, so the hip file is what a pattern reads)."""
        for k in ("C4_SHOT", "SHOT"):
            if self.env.get(k):
                return self.env[k]
        pat = self.env.get("C4_SHOT_PATTERN")
        if pat:
            m = re.search(pat, self.hip())
            if m and m.groupdict().get("shot"):
                return m.group("shot")
        return ""

    def _absolute(self, path: str) -> str:
        if not path or _SCHEME.match(path):
            return path
        if paths.is_absolute(path):
            return paths.norm(path)
        return paths.norm(os.path.join(self.hip_dir(), path))

    def _not_a_project_file(self, path: str) -> bool:
        """Houdini's own files and URLs, which the farm has or cannot use:
        anything under $HFS, the C4 nodes themselves, and op:, opdef: or
        http: paths."""
        if not path or _SCHEME.match(path):
            return True
        for var in ("HFS", "C4_DCC"):
            top = self.env.get(var) or (hou.getenv(var) if hasattr(hou, "getenv") else None)
            if top and paths.relative(top, path) is not None:
                return True
        return False


class LopHost(_HoudiniHost):
    """The C4 Render LOP: renders the stage wired into its input.

    The node's parameters ``c4_usdfile`` (where the stage is written,
    ``$HIP/usd/<shot>_<node>.usd`` by default) and ``c4_f`` (the frame
    range, ``$FSTART``-``$FEND`` by default) are read here."""

    stage = "light"
    scene_word = "stage"
    # The stage is written for every submission: the network can change
    # without the hip file changing (a time-dependent node, a referenced
    # file updated on disk).
    writes_scene = True

    def __init__(self, node, env: Optional[Mapping[str, str]] = None):
        super().__init__(node, env)
        # The frames the panel renders when they are not the node's range
        # (the artist unlinked Frames). The stage is written over them.
        self.write_frames: Optional[str] = None
        self.written = ""

    def stage_node(self):
        """The LOP whose stage is rendered: the node's first input."""
        for n in self.node.inputs():
            if n is not None:
                return n
        return None

    def usd_path(self) -> str:
        p = self.node.parm("c4_usdfile")
        v = p.eval() if p is not None else ""
        if not v:
            v = os.path.join(hou.expandString("$HIP"), "usd", f"{self.shot() or 'stage'}_{self.node.name()}.usd")
        return self._absolute(v)

    def scene_file(self) -> str:
        return self.usd_path() if self.stage_node() is not None else ""

    def is_modified(self) -> bool:
        """Whether the stage file may not match the network: it was never
        written, or the hip file has changes not saved."""
        return not os.path.isfile(self.usd_path()) or bool(hou.hipFile.hasUnsavedChanges())

    def save(self) -> None:
        """Writes the stage to ``usd_path()`` with a USD ROP over the frames
        to render, so the file holds the animation the farm renders."""
        src = self.stage_node()
        if src is None:
            raise RuntimeError("nothing is wired into the C4 Render node")
        out = self.usd_path()
        os.makedirs(os.path.dirname(out), exist_ok=True)
        start, end, step = self._write_range()
        net = hou.node("/out") or hou.node("/").createNode("ropnet", "out")
        rop = net.createNode("usd", "c4_write_stage")
        try:
            rop.parm("loppath").set(src.path())
            rop.parm("lopoutput").set(out)
            rop.parm("trange").set(1)
            rop.render(frame_range=(start, end, step))
            errors = rop.errors()
            if errors:
                raise RuntimeError("; ".join(errors))
        finally:
            rop.destroy()
        if not os.path.isfile(out):
            raise RuntimeError(f"the USD ROP wrote nothing to {out}")
        self.written = out

    def _write_range(self) -> Tuple[int, int, int]:
        fs = framelib.parse(self.write_frames or self.frames())
        steps = {b - a for a, b in zip(fs, fs[1:])}
        return fs[0], fs[-1], (steps.pop() if len(steps) == 1 else 1)

    def frames(self) -> str:
        t = self.node.parmTuple("c4_f")
        if t is not None:
            a, b, c = t.eval()
        else:
            a, b, c = float(hou.expandString("$FSTART")), float(hou.expandString("$FEND")), 1
        return framelib.span(int(round(a)), int(round(b)), max(1, int(round(c))))

    def renderables(self) -> List[Renderable]:
        """The stage's render settings prims, under /Render by convention,
        plus the stage's default one wherever it is. The default
        (``renderSettingsPrimPath``) is ticked; without one, the first."""
        src = self.stage_node()
        stage = src.stage() if src is not None else None
        if stage is None:
            return []
        from pxr import Usd, UsdRender
        default = stage.GetMetadata("renderSettingsPrimPath") or ""
        prims = []
        top = stage.GetPrimAtPath("/Render")
        if top and top.IsValid():
            prims = list(Usd.PrimRange(top))
        if default and default not in {p.GetPath().pathString for p in prims}:
            p = stage.GetPrimAtPath(default)
            if p and p.IsValid():
                prims.append(p)
        settings = [p for p in prims if p.IsA(UsdRender.Settings)]
        names = [p.GetPath().pathString for p in settings]
        tick = default if default in names else (names[0] if names else "")
        return [Renderable(name=n, label=n, key=n.rsplit("/", 1)[-1], enabled=(n == tick)) for n in names]

    def dependencies(self) -> List[Dependency]:
        """Every layer and asset the written stage reads, from USD's own
        dependency walk (sublayers, references, payloads, textures with
        their ``<UDIM>`` tiles). Paths USD cannot resolve are listed too,
        so the panel says they are missing."""
        scene = self.scene_file()
        if not scene or not os.path.isfile(scene):
            return []
        src = self.stage_node()
        who = src.path() if src is not None else self.node.path()
        found, _ = usdlib.dependencies(scene, node=who)
        return [d for d in found if not self._not_a_project_file(d.path)]


class RopHost(_HoudiniHost):
    """The C4 Submit ROP: renders the ROPs wired into it, from the saved
    hip file. Each ROP is a job; one with another listed ROP upstream runs
    after it. ROPs further upstream are listed unticked, so an artist can
    send a cache to the farm along with the render that reads it.

    For a tool that cooks TOP networks (``lists = "tops"``, set from the
    tool by ``lists_for``), the hip file's TOP networks are listed instead:
    one wired into the node is ticked (or the only one there is), and each
    carries the node a cook of it cooks as the tool's ``top``."""

    stage = "render"
    scene_word = "hip file"
    lists = "rops"

    def scene_file(self) -> str:
        return self.hip()

    def is_modified(self) -> bool:
        return bool(hou.hipFile.hasUnsavedChanges())

    def save(self) -> None:
        if hou.hipFile.isNewFile():
            raise RuntimeError("the hip file has never been saved; save it under the project folder first")
        hou.hipFile.save()

    def frames(self) -> str:
        a, b = float(hou.expandString("$FSTART")), float(hou.expandString("$FEND"))
        return framelib.span(int(round(a)), int(round(b)))

    # --- the ROP graph ---------------------------------------------------

    def _upstream(self, node, seen: Optional[Set[str]] = None) -> list:
        """The nearest ROPs feeding a node, looking through merges."""
        seen = seen if seen is not None else set()
        out = []
        for n in node.inputs():
            if n is None or n.path() in seen:
                continue
            seen.add(n.path())
            if n.type().name() in PASS_THROUGH:
                out += self._upstream(n, seen)
            else:
                out.append(n)
        return out

    def _graph(self) -> Tuple[list, Dict[str, List[str]], Set[str]]:
        """Every ROP upstream of the node, upstream first; each one's
        nearest upstream ROPs; and the ones wired in directly."""
        direct = self._upstream(self.node)
        order, ups = [], {}

        def visit(n):
            if n.path() in ups:
                return
            ups[n.path()] = []
            near = self._upstream(n)
            ups[n.path()] = [u.path() for u in near]
            for u in near:
                visit(u)
            order.append(n)

        for d in direct:
            visit(d)
        return order, ups, {d.path() for d in direct}

    def _target(self, rop):
        """What a ROP renders: a Fetch ROP's source, or itself."""
        if rop.type().name() == "fetch" and rop.parm("source") is not None:
            src = rop.node(rop.parm("source").eval()) if hasattr(rop, "node") else None
            if src is not None:
                return src
        return rop

    def outputs(self, rop) -> List[str]:
        """The files a ROP writes, as patterns ($F4 kept)."""
        out = []
        for p in output_parms(self._target(rop)):
            v = expand(unexpanded(p))
            if v and paths.is_absolute(v) and paths.norm(v) not in out:
                out.append(paths.norm(v))
        return out

    def rop_frames(self, rop) -> Optional[str]:
        rop = self._target(rop)
        t = rop.parm("trange")
        if t is None:
            return None
        if int(t.eval()) == 0:
            return str(int(round(hou.frame())))
        f = rop.parmTuple("f")
        if f is None:
            return None
        a, b, c = f.eval()
        return framelib.span(int(round(a)), int(round(b)), max(1, int(round(c))))

    def top_renderables(self) -> List[Renderable]:
        nets = top_networks()
        wired = {n.path() for n in self._upstream(self.node)}
        tick = wired & {n.path() for n in nets} or ({nets[0].path()} if len(nets) == 1 else set())
        out = []
        for n in nets:
            p = n.path()
            cook = top_output(n)
            if _bypassed(n):
                note = "bypassed"
            elif cook is None:
                note = "no output node: set the display flag on the node to cook"
            else:
                note = "cooks " + cook.name()
            out.append(Renderable(name=p, label=p, key=n.name(), enabled=(p in tick and not _bypassed(n)), note=note,
                                  values={"top": cook.path()} if cook is not None else {}))
        return out

    def renderables(self) -> List[Renderable]:
        if self.lists == "tops":
            return self.top_renderables()
        order, ups, direct = self._graph()

        def above(p: str, acc: List[str]) -> List[str]:
            for u in ups.get(p, []):
                if u not in acc:
                    acc.append(u)
                    above(u, acc)
            return acc

        below = {p: [d for d in ups if p in above(d, [])] for p in ups}
        out = []
        for n in order:
            p = n.path()
            note = ""
            if _bypassed(n):
                note = "bypassed"
            elif p not in direct:
                feeds = [os.path.basename(d) for d in below[p] if d in direct]
                note = "upstream of " + ", ".join(feeds) if feeds else "upstream"
            out.append(Renderable(name=p, label=n.name(), key=n.name(), frames=self.rop_frames(n), outputs=self.outputs(n),
                                  reads=above(p, []), enabled=(p in direct and not _bypassed(n)), note=note))
        return out

    # --- files ------------------------------------------------------------

    def dependencies(self) -> List[Dependency]:
        """Every file the hip file reads, from ``hou.fileReferences()``,
        as typed (so ``$F4`` stays a sequence and ``<UDIM>`` a tile set).
        Files ROPs write are not inputs; a reference on a bypassed node is
        only a warning when it is missing."""
        scene = self.scene_file()
        out, seen = [], set()
        for parm, ref in hou.fileReferences():
            who, optional, raw = "", False, ref
            if parm is not None:
                if writes(parm):
                    continue
                raw = unexpanded(parm)
                who = parm.node().path()
                optional = _bypassed(parm.node())
            p = self._absolute(expand(raw))
            if not p or p == scene or self._not_a_project_file(p):
                continue
            key = seq.canonical(p)
            if key in seen:
                continue
            seen.add(key)
            out.append(Dependency(path=p, node=who, optional=optional))
        return out
