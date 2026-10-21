"""Render one ROP of a hip file over this unit's frames, without its inputs.

    hython houdini_rop.py HIP ROP FRAMES

HIP is the hip file, an absolute path in the job's tree, or a ``.py`` that
builds the scene and saves it to the path it is given (how the tool's test
makes a scene without a binary hip file in git). ROP is the node's path,
as the C4 Submit ROP lists it. FRAMES is the unit's frames in OpenJD's
grammar: ``1001``, ``1001-1004``, ``1001-1010:2`` or a list of them.

The ROP is rendered with ``ignore_inputs=True``: the C4 Submit ROP makes
each ROP upstream that the artist ticked its own job, run first, and reads
the ones left unticked from disk. A ``c4_frame: <frame> <path>`` line is
printed for each frame once its file is there; a failure the script can
name is a ``c4_fail: permanent|retry <reason>`` line.

Standard library and ``hou`` only, for Houdini's Python (3.11).
"""

import os
import re
import sys
from typing import Callable, Dict, List, Optional, Tuple

import hou

# The output parameters of the ROPs Houdini ships, in the order they are
# looked for (as the C4 Submit ROP's host reads them). A studio ROP's is
# found by its file chooser's write mode.
OUTPUT_PARMS = ("vm_picture", "picture", "outputimage", "lopoutput", "sopoutput", "copoutput", "dopoutput",
                "filename", "vm_dcmfilename")

HIP_EXTS = (".hip", ".hiplc", ".hipnc")

_RANGE = re.compile(r"^(-?\d+)(?:-(-?\d+)(?::(\d+))?)?$")


class Fail(Exception):
    """A failure with its class: permanent (no retry helps) or retry."""

    def __init__(self, cls: str, reason: str):
        super().__init__(reason)
        self.cls, self.reason = cls, reason


def say(line: str) -> None:
    print(line, flush=True)


# --- frames --------------------------------------------------------------------


def parse_frames(text: str) -> List[int]:
    """The frames of an OpenJD range expression, in order, each once."""
    out: List[int] = []
    for part in (text or "").replace(" ", "").split(","):
        if not part:
            continue
        m = _RANGE.match(part)
        if not m:
            raise Fail("permanent", f"the unit's frames {text!r} are not a frame list")
        a = int(m.group(1))
        b = int(m.group(2)) if m.group(2) is not None else a
        step = int(m.group(3) or 1)
        if b < a or step < 1:
            raise Fail("permanent", f"the unit's frames {text!r} are not a frame list")
        out.extend(range(a, b + 1, step))
    seen = set()
    return [f for f in out if not (f in seen or seen.add(f))]


def ranges(frames: List[int]) -> List[Tuple[int, int, int]]:
    """The fewest (start, end, step) ranges that render exactly these
    frames, in order: one ``render()`` call each."""
    out: List[Tuple[int, int, int]] = []
    i = 0
    while i < len(frames):
        a = frames[i]
        if i + 1 == len(frames):
            out.append((a, a, 1))
            break
        step = frames[i + 1] - a
        j = i + 1
        while j + 1 < len(frames) and frames[j + 1] - frames[j] == step:
            j += 1
        if step < 1:
            out.append((a, a, 1))
            i += 1
            continue
        out.append((a, frames[j], step))
        i = j + 1
    return out


# --- the scene -------------------------------------------------------------------


def load_scene(path: str) -> str:
    """Opens the hip file, or runs a ``.py`` that builds and saves one
    beside it (``<name>.hip``) and opens that. Returns the hip file's path."""
    if not os.path.isfile(path):
        raise Fail("permanent", f"no hip file at {path}")
    hip = path
    if path.lower().endswith(".py"):
        hip = os.path.splitext(path)[0] + ".hip"
        say(f"openjd_status: building the scene with {os.path.basename(path)}")
        argv = sys.argv
        sys.argv = [path, hip]
        try:
            with open(path, encoding="utf-8") as f:
                code = compile(f.read(), path, "exec")
            exec(code, {"__name__": "__main__", "__file__": path, "hou": hou})
        except Fail:
            raise
        except Exception as e:  # noqa: BLE001 - the builder's own error, named
            raise Fail("permanent", f"{os.path.basename(path)} did not build the scene: {e}")
        finally:
            sys.argv = argv
        if not os.path.isfile(hip):
            raise Fail("permanent", f"{os.path.basename(path)} did not save the scene to {hip}")
    elif not path.lower().endswith(HIP_EXTS):
        raise Fail("permanent", f"{os.path.basename(path)} is not a hip file (.hip, .hiplc, .hipnc) or a .py that builds one")
    try:
        hou.hipFile.load(hip, suppress_save_prompt=True, ignore_load_warnings=False)
    except hou.LoadWarning as w:
        # The file is open; a missing HDA or a newer node is worth the log.
        say(f"houdini: the hip file loaded with warnings: {w}")
    except hou.OperationFailed as e:
        raise Fail("permanent", f"Houdini could not open {hip}: {e}")
    return hip


def is_rop(node) -> bool:
    """Whether a node renders: a ROP in a ROP network, or any other
    ``hou.RopNode`` (a ROP node inside SOPs or LOPs)."""
    try:
        if node.type().category().name() == "Driver":
            return True
    except AttributeError:
        pass
    return isinstance(node, getattr(hou, "RopNode", ()))


def target(rop):
    """What a ROP renders: a Fetch ROP's source, or itself."""
    if rop.type().name() == "fetch" and rop.parm("source") is not None:
        src = rop.node(rop.parm("source").eval())
        if src is not None:
            return src
    return rop


def output_parm(rop):
    """The parameter naming the file a ROP writes: a known name, else the
    first file parameter whose chooser writes. None when it has none."""
    rop = target(rop)
    for n in OUTPUT_PARMS:
        p = rop.parm(n)
        if p is not None:
            return p
    for p in rop.parms():
        try:
            t = p.parmTemplate()
            if t.type() != hou.parmTemplateType.String or t.stringType() != hou.stringParmType.FileReference:
                continue
            if t.tags().get("filechooser_mode") == "write":
                return p
        except (AttributeError, hou.Error):
            continue
    return None


def frame_file(parm, frame: int) -> str:
    """The file a ROP writes for a frame, as an absolute path."""
    if parm is None:
        return ""
    try:
        v = parm.evalAtFrame(frame)
    except hou.Error:
        return ""
    if not v or re.match(r"^[A-Za-z][A-Za-z0-9+.-]+:", v):
        return ""
    return os.path.normpath(v if os.path.isabs(v) else os.path.join(os.getcwd(), v))


# --- the render --------------------------------------------------------------------


class Frames:
    """Reports each frame once its file is on disk: as the ROP finishes it
    (a render event), else after the render returns."""

    def __init__(self, parm, frames: List[int], out: Callable[[str], None] = say):
        self.parm, self.frames, self.out = parm, frames, out
        self.reported: Dict[int, str] = {}

    def finished(self, frame: int) -> bool:
        """Reports a frame whose file is there; False when it is not yet."""
        if frame in self.reported or frame not in self.frames:
            return True
        path = frame_file(self.parm, frame)
        if self.parm is not None and (not path or not os.path.exists(path)):
            return False
        self.reported[frame] = path
        self.out(f"c4_frame: {frame} {path}".rstrip())
        return True

    def event(self, rop_node, event_type, time) -> None:
        if event_type in (hou.ropRenderEventType.PostFrame, hou.ropRenderEventType.PostWrite):
            self.finished(int(round(hou.timeToFrame(time))))

    def missing(self) -> List[Tuple[int, str]]:
        """The frames still without a file, with the file expected."""
        return [(f, frame_file(self.parm, f)) for f in self.frames if not self.finished(f)]


def render(rop, frames: List[int], out: Callable[[str], None] = say) -> Frames:
    """Renders a ROP over the frames, without its inputs, and reports each
    frame. Raises Fail when Houdini reports an error or a file is missing."""
    seen = Frames(output_parm(rop), frames, out)
    listen = callable(getattr(rop, "addRenderEventCallback", None))
    if listen:
        rop.addRenderEventCallback(seen.event)
    try:
        for a, b, step in ranges(frames):
            out(f"openjd_status: rendering {rop.path()}, frames {a}-{b}" + (f" by {step}" if step > 1 else ""))
            try:
                rop.render(frame_range=(a, b, step), ignore_inputs=True, verbose=True, output_progress=True)
            except hou.OperationFailed as e:
                raise Fail("retry", f"{rop.path()} failed to render: {e}")
            errors = [e for e in rop.errors() if e]
            if errors:
                raise Fail("retry", f"{rop.path()}: " + "; ".join(errors))
            for f in range(a, b + 1, step):
                seen.finished(f)
    finally:
        if listen:
            try:
                rop.removeRenderEventCallback(seen.event)
            except hou.Error:
                pass
    gone = seen.missing()
    if gone:
        f, path = gone[0]
        raise Fail("retry", f"{rop.path()} rendered but wrote no {path} for frame {f}"
                   + (f" (and {len(gone) - 1} more frames)" if len(gone) > 1 else ""))
    return seen


def main(argv: Optional[List[str]] = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    try:
        if len(args) != 3:
            raise Fail("permanent", "usage: houdini_rop.py HIP ROP FRAMES")
        scene, path, unit = args
        frames = parse_frames(unit)
        if not frames:
            raise Fail("permanent", "the unit has no frames")
        load_scene(scene)
        rop = hou.node(path)
        if rop is None:
            raise Fail("permanent", f"the hip file has no node {path}")
        if not is_rop(rop):
            raise Fail("permanent", f"{path} is a {rop.type().name()} node, not a ROP")
        render(rop, frames)
    except Fail as e:
        say(f"c4_fail: {e.cls} {e.reason}")
        return 2 if e.cls == "permanent" else 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
