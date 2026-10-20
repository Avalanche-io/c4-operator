"""What a DCC tells c4submit about the open scene. Each panel implements
``Host`` over its application's API (``nuke``, ``hou``, ``maya.cmds``,
``bpy``); everything else in c4submit is the same for every DCC.

Host methods are called on the application's main thread, by the panel,
in ``Submitter.prepare``. Nothing after that touches the host, so hashing,
uploads and the farm's answers can run on a worker thread.
"""

import os
import re
from dataclasses import dataclass, field
from typing import List, Mapping, Optional


@dataclass
class Renderable:
    """One thing a panel can tick to render: a Nuke Write, a Solaris
    render settings prim, a ROP. Each ticked one is a job."""
    name: str                          # the node's name or path; what scene.renderables gives
    label: str = ""                    # how the panel lists it
    frames: Optional[str] = None       # its own frame range, when it has one
    outputs: List[str] = field(default_factory=list)   # files it writes, as patterns (absolute)
    reads: List[str] = field(default_factory=list)     # names of renderables whose outputs it reads
    enabled: bool = True               # ticked by default
    note: str = ""                     # why it is disabled, or anything the panel should say


@dataclass
class Dependency:
    """A file, sequence or tile set the scene reads."""
    path: str                          # absolute; may hold frame or tile tokens
    frames: Optional[str] = None       # the frames it is read over, when it is a sequence
    node: str = ""                     # who reads it, for messages
    renderables: Optional[List[str]] = None   # the renderables that read it; None means every one
    optional: bool = False             # missing is a warning, not an error


class Host:
    """The scene, as a panel reads it. Override what the DCC knows."""

    #: the application, in messages and the request's client header
    name = "dcc"
    #: the stem of job keys from this DCC (comp, light, anim)
    stage = "render"
    #: what a scene file is called in messages
    scene_word = "scene"

    def __init__(self, env: Optional[Mapping[str, str]] = None):
        self.env = env if env is not None else os.environ

    # --- the scene -------------------------------------------------------

    def scene_file(self) -> str:
        """The open scene's absolute path; '' when it was never saved."""
        return ""

    def is_modified(self) -> bool:
        return False

    def save(self) -> None:
        """Save the open scene to its own path (the farm reads the saved file)."""
        raise NotImplementedError(f"{self.name} cannot save the {self.scene_word} from here")

    def frames(self) -> str:
        """The scene's frame range as a range expression."""
        return ""

    def renderables(self) -> List[Renderable]:
        return []

    def selection(self) -> List[str]:
        """Files the selected nodes read (a Nuke Read's file), absolute."""
        return []

    def camera(self) -> Optional[str]:
        return None

    def dependencies(self) -> List[Dependency]:
        """Every file the scene reads, besides the scene file itself."""
        return []

    # --- production context ---------------------------------------------

    def project(self) -> str:
        return self.env.get("C4_PROJECT", "")

    def shot(self) -> str:
        """The shot: ``C4_SHOT``, else ``SHOT``, else the ``shot`` group of
        ``C4_SHOT_PATTERN`` (a regular expression) matched against the
        scene's path."""
        for k in ("C4_SHOT", "SHOT"):
            if self.env.get(k):
                return self.env[k]
        pat = self.env.get("C4_SHOT_PATTERN")
        if pat:
            m = re.search(pat, self.scene_file().replace("\\", "/"))
            if m and m.groupdict().get("shot"):
                return m.group("shot")
        return ""

    def root(self) -> str:
        """The folder on this machine that is the top of the project's tree
        (the farm mounts it at the project's root path): ``C4_ROOT``."""
        return self.env.get("C4_ROOT", "")
