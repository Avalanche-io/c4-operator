"""Cook a whole TOP network in one unit, with PDG's local scheduler.

    hython pdg_cook.py HIP TOP

HIP is the hip file, an absolute path in the job's tree, or a ``.py`` that
builds the scene and saves it to the path it is given (how the tool's test
makes a scene without a binary hip file in git). TOP is a TOP network
(its output node is cooked: the node with the display flag) or a TOP node
in one, by path.

Every TOP network the cook touches is set to a local scheduler made here,
with as many slots as the unit has CPUs, so a hip file saved with a farm
scheduler (HQueue, Deadline, Tractor) still cooks inside this unit. The
work items are not the farm's units: the whole cook runs here, as a
"submit graph as job" does elsewhere.

Progress is ``openjd_progress`` and ``openjd_status`` lines as work items
finish; a failed cook is a ``c4_fail: permanent|retry <reason>`` line.

Standard library, ``hou`` and ``pdg`` only, for Houdini's Python (3.11).
"""

import os
import sys
from typing import Callable, Dict, List, Optional, Set

import hou

HIP_EXTS = (".hip", ".hiplc", ".hipnc")

# PDG's work item states, by name (pdg.workItemState).
DONE = {"CookedSuccess", "CookedCache"}
FAILED = {"CookedFail"}
CANCELLED = {"CookedCancel"}

SCHEDULER = "c4_local"


class Fail(Exception):
    """A failure with its class: permanent (no retry helps) or retry."""

    def __init__(self, cls: str, reason: str):
        super().__init__(reason)
        self.cls, self.reason = cls, reason


def say(line: str) -> None:
    print(line, flush=True)


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
        say(f"houdini: the hip file loaded with warnings: {w}")
    except hou.OperationFailed as e:
        raise Fail("permanent", f"Houdini could not open {hip}: {e}")
    return hip


def is_top_network(node) -> bool:
    try:
        return node.childTypeCategory() == hou.topNodeTypeCategory()
    except (AttributeError, hou.Error):
        return False


def is_top(node) -> bool:
    try:
        return node.type().category() == hou.topNodeTypeCategory()
    except (AttributeError, hou.Error):
        return False


def output_node(net):
    """A TOP network's output node: the one with the display flag."""
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


def cook_target(path: str):
    """The TOP node to cook for a path: a TOP node itself (a TOP subnet
    included), or a TOP network's output node."""
    node = hou.node(path)
    if node is None:
        raise Fail("permanent", f"the hip file has no node {path}")
    if is_top(node):
        return node
    if is_top_network(node):
        out = output_node(node)
        if out is None:
            raise Fail("permanent", f"the TOP network {path} has no output node; set the display flag on the node to cook")
        return out
    raise Fail("permanent", f"{path} is a {node.type().name()} node, not a TOP network or a TOP node")


# --- the scheduler -----------------------------------------------------------------


def unit_cpus(cgroup: str = "/sys/fs/cgroup/cpu.max") -> int:
    """The CPUs this unit may use: its cgroup's quota when it has one (a
    container sees the whole machine's cores), else the CPUs it may run on."""
    try:
        with open(cgroup, encoding="utf-8") as f:
            quota, period = f.read().split()[:2]
        if quota != "max" and int(period) > 0:
            return max(1, int(int(quota) // int(period)))
    except (OSError, ValueError):
        pass
    try:
        return max(1, len(os.sched_getaffinity(0)))
    except (AttributeError, OSError):
        return max(1, os.cpu_count() or 1)


def _set(node, name: str, value) -> bool:
    p = node.parm(name)
    if p is None:
        return False
    try:
        p.set(value)
    except hou.Error as e:
        say(f"houdini: could not set {node.path()} {name} to {value!r}: {e}")
        return False
    return True


def local_scheduler(net, slots: int):
    """Makes a TOP network cook with a local scheduler of its own, ``slots``
    work items at a time, and returns it; None for a network without a
    default scheduler (a TOP subnet, which cooks with its network's)."""
    if net.parm("topscheduler") is None:
        return None
    sched = net.node(SCHEDULER) or net.createNode("localscheduler", SCHEDULER)
    # Total slots: a custom count, not the machine's cores.
    if _set(sched, "maxprocsmenu", "-1"):
        _set(sched, "maxprocs", int(slots))
    _set(net, "topscheduler", sched.path())
    return sched


def networks(top) -> List:
    """The TOP networks a node is in, nearest first (a TOP subnet, then
    the TOP network holding it)."""
    out = []
    n = top.parent()
    while n is not None and is_top_network(n):
        out.append(n)
        n = n.parent()
    return out


# --- the cook ------------------------------------------------------------------------


def state_name(state) -> str:
    """A pdg.workItemState's name, as ``CookedSuccess``."""
    name = getattr(state, "name", None)
    return str(name if isinstance(name, str) else state).rsplit(".", 1)[-1]


class Progress:
    """Counts work items as their states change, and prints progress as a
    share of the ones seen so far: never down, and 100 only once the cook
    has finished. PDG generates work items as it goes, so the total is not
    known up front."""

    def __init__(self, out: Callable[[str], None] = say):
        self.out = out
        self.seen: Set[int] = set()
        self.done: Set[int] = set()
        self.failed: Dict[int, str] = {}
        self.cancelled: Set[int] = set()
        self.shown = -1
        self.shown_failed = 0

    def change(self, item: int, node: str, state) -> None:
        name = state_name(state)
        self.seen.add(item)
        if name in DONE:
            self.done.add(item)
            self.failed.pop(item, None)
        elif name in FAILED:
            self.failed[item] = node
        elif name in CANCELLED:
            self.cancelled.add(item)
        else:
            return
        finished = len(self.done) + len(self.failed) + len(self.cancelled)
        pct = min(99, int(100 * finished / len(self.seen)))
        if pct <= self.shown and len(self.failed) == self.shown_failed:
            return
        if pct > self.shown:
            self.shown = pct
            self.out(f"openjd_progress: {pct}")
        self.shown_failed = len(self.failed)
        self.out(f"openjd_status: {len(self.done)} of {len(self.seen)} work items so far cooked" +
                 (f", {len(self.failed)} failed" if self.failed else ""))

    def event(self, event) -> None:
        """A pdg.Event of type WorkItemStateChange."""
        node = getattr(event, "node", None)
        name = getattr(node, "name", "") if node is not None else ""
        self.change(event.workItemId, name if isinstance(name, str) else str(name), event.currentState)

    def tally(self, top) -> None:
        """Reads the cooked node's work items, for a cook whose events were
        not heard (no graph context to listen on)."""
        try:
            items = list(top.getPDGNode().workItems)
        except (AttributeError, hou.Error):
            return
        name = top.name()
        for w in items:
            self.change(w.id, name, w.state)

    def finish(self) -> None:
        if self.failed:
            nodes = sorted(set(self.failed.values()))
            raise Fail("retry", f"{len(self.failed)} work items failed, in " + ", ".join(nodes))
        self.out("openjd_progress: 100")
        self.out(f"openjd_status: {len(self.done)} work items cooked")


def cook(top, out: Callable[[str], None] = say) -> Progress:
    """Cooks a TOP node and everything it depends on, blocking, and
    reports progress. Raises Fail when the cook or a work item fails."""
    import pdg

    progress = Progress(out)
    ctx = top.getPDGGraphContext()
    handler = None
    if ctx is not None:
        try:
            handler = ctx.addEventHandler(progress.event, pdg.EventType.WorkItemStateChange)
        except Exception as e:  # noqa: BLE001 - cook without progress rather than not at all
            say(f"houdini: no progress from the cook: {e}")
    if handler is None:
        out("openjd_status: cooking; progress shows once the cook is done")
    out(f"openjd_status: cooking {top.path()}")
    try:
        top.cookWorkItems(block=True)
    except hou.OperationFailed as e:
        raise Fail("retry", f"the cook of {top.path()} failed: {e}")
    finally:
        if ctx is not None and handler is not None:
            try:
                ctx.removeEventHandler(handler)
            except Exception:  # noqa: BLE001 - the cook is over either way
                pass
    errors = [e for e in top.errors() if e]
    if errors:
        raise Fail("retry", f"{top.path()}: " + "; ".join(errors))
    if not progress.seen:
        progress.tally(top)
    progress.finish()
    return progress


def main(argv: Optional[List[str]] = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    try:
        if len(args) != 2:
            raise Fail("permanent", "usage: pdg_cook.py HIP TOP")
        scene, path = args
        load_scene(scene)
        top = cook_target(path)
        slots = unit_cpus()
        made = [s for s in (local_scheduler(net, slots) for net in networks(top)) if s is not None]
        if made:
            say(f"openjd_status: cooking with a local scheduler, {slots} slots")
        else:
            say(f"houdini: no TOP network above {top.path()} has a default scheduler; it cooks with the one it was saved with")
        cook(top)
    except Fail as e:
        say(f"c4_fail: {e.cls} {e.reason}")
        return 2 if e.cls == "permanent" else 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
