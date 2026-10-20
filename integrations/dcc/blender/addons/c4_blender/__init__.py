"""C4 Farm: submit the open Blender file to a C4 render farm, from a panel
in Render Properties. Each ticked view layer is a job.

c4submit, the library under every C4 panel, comes from this add-on's own
``vendor`` folder when it has one (the zip ``build_addon.py`` makes), else
from beside it in the repository (``integrations/dcc/c4submit/src``).
"""

import os
import sys

bl_info = {
    "name": "C4 Farm",
    "author": "C4",
    "version": (0, 1, 0),
    "blender": (4, 2, 0),
    "location": "Properties > Render > C4 Farm",
    "description": "Submit the file to a C4 render farm: each ticked view layer is a job, with Needed by and the "
                   "farm's estimate before you submit",
    "category": "Render",
}


def _c4submit_folder() -> str:
    here = os.path.dirname(os.path.abspath(__file__))
    for d in (os.path.join(here, "vendor"),
              os.path.normpath(os.path.join(here, "..", "..", "..", "c4submit", "src"))):
        if os.path.isfile(os.path.join(d, "c4submit", "__init__.py")):
            return d
    return ""


_folder = _c4submit_folder()
if _folder and _folder not in sys.path:
    sys.path.insert(0, _folder)

try:
    import c4submit  # noqa: F401
except ImportError:
    raise ImportError("C4 Farm needs c4submit: install the zip build_addon.py makes, or keep "
                      "integrations/dcc/c4submit beside the add-on as it is in the repository") from None

if "panel" in locals():   # Blender's Reload Scripts
    import importlib
    host = importlib.reload(host)  # noqa: F821
    panel = importlib.reload(panel)  # noqa: F821
else:
    from . import host, panel  # noqa: F401


def register():
    panel.register()


def unregister():
    panel.unregister()
