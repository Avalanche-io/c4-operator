"""Adds C4 to Katana's farm menus. Katana runs this at start-up when this
folder's parent is on KATANA_RESOURCES. c4_katana is found in
``../python`` and c4submit in ``../../c4submit/src`` when they are not
importable already.
"""

import os
import sys


def _c4_here():
    try:
        return os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    except NameError:  # run without __file__
        for d in os.environ.get("KATANA_RESOURCES", "").split(os.pathsep):
            if os.path.isdir(os.path.join(d, "python", "c4_katana")):
                return d
        return ""


def _c4_setup():
    if sys.version_info < (3, 9):
        print("C4: submitting to the farm needs Katana 6 or later (Python 3.9); no C4 menus added")
        return
    here = _c4_here()
    for d in (os.path.join(here, "python"), os.path.normpath(os.path.join(here, "..", "c4submit", "src"))):
        if here and os.path.isdir(d) and d not in sys.path:
            sys.path.insert(0, d)
    try:
        from c4_katana import farm
        farm.install()
    except Exception as e:  # noqa: BLE001 - a broken install must not stop Katana starting
        print(f"C4: no C4 menus added: {e}")


_c4_setup()
del _c4_here, _c4_setup
