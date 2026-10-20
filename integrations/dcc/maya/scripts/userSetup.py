"""Adds C4 to Maya: "C4 > Submit to farm..." in the main window's menus.

Maya runs this at startup when this folder (``scripts``) is on its Python
path, as it is when the C4 module file is installed. c4submit is found
beside it (``../../c4submit/src``) when the module file has not put it on
the path already. A studio with its own ``userSetup.py`` can instead put
this folder and c4submit on the path and call ``c4_maya.menu.install()``.
"""

import os
import sys


def _c4_here():
    try:
        return os.path.dirname(os.path.abspath(__file__))
    except NameError:  # run without __file__
        for d in sys.path:
            if os.path.isdir(os.path.join(d, "c4_maya")):
                return d
        return ""


def _c4_setup():
    if sys.version_info < (3, 9):
        print("C4: submitting to the farm needs Maya 2023 or later (Python 3.9); no C4 menu added")
        return
    here = _c4_here()
    if here and here not in sys.path:
        sys.path.insert(0, here)
    try:
        import c4submit  # noqa: F401
    except ImportError:
        src = os.path.normpath(os.path.join(here, "..", "..", "c4submit", "src"))
        if os.path.isdir(src):
            sys.path.insert(0, src)
    try:
        from c4_maya import menu
        menu.install()
    except Exception as e:  # noqa: BLE001 - a broken install must not stop Maya starting
        print(f"C4: no C4 menu added: {e}")


_c4_setup()
del _c4_here, _c4_setup
