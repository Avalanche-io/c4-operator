"""Adds C4 to Nuke: "C4 > Submit to farm..." in the menu bar, and "C4 >
Submit this Write to C4" on the Node Graph's right-click menu, which opens
the panel with only the selected Write nodes ticked.

A studio puts this folder on NUKE_PATH. c4submit is found beside it
(``../c4submit/src``) when it is not importable already.
"""

import os
import sys


def _c4_here():
    try:
        return os.path.dirname(os.path.abspath(__file__))
    except NameError:  # Nuke may run menu.py without __file__
        import nuke
        for d in nuke.pluginPath():
            if os.path.isdir(os.path.join(d, "c4_nuke")):
                return d
        return ""


def _c4_setup():
    if sys.version_info < (3, 9):
        print("C4: submitting to the farm needs Nuke 14 or later (Python 3.9); no C4 menu added")
        return
    here = _c4_here()
    if here and here not in sys.path:
        sys.path.insert(0, here)
    try:
        import c4submit  # noqa: F401
    except ImportError:
        src = os.path.normpath(os.path.join(here, "..", "c4submit", "src"))
        if os.path.isdir(src):
            sys.path.insert(0, src)

    import nuke

    def submit():
        from c4_nuke import panel
        panel.open_panel()

    def submit_write():
        from c4_nuke import panel
        panel.submit_selected_writes()

    nuke.menu("Nuke").addMenu("C4").addCommand("Submit to farm...", submit)
    nuke.menu("Node Graph").addMenu("C4").addCommand("Submit this Write to C4", submit_write)


_c4_setup()
del _c4_here, _c4_setup
