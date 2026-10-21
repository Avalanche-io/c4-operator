"""Run by Unreal's Python plug-in when the editor starts, from any folder on
its Python path (Project Settings > Plugins > Python > Additional Paths, or
``UE_PYTHONPATH``). It makes c4submit importable (from beside this folder in
the repository, ``integrations/dcc/c4submit/src``) and loads the C4 executor,
so ``C4FarmExecutor`` can be picked as the Movie Render Queue's Default
Remote Executor.
"""

import os
import sys


def _c4_setup():
    import unreal

    here = os.path.dirname(os.path.abspath(__file__))
    src = os.path.normpath(os.path.join(here, "..", "..", "c4submit", "src"))
    for d in (here, src):
        if os.path.isdir(d) and d not in sys.path:
            sys.path.insert(0, d)
    try:
        import c4_unreal.executor  # noqa: F401 - registers C4FarmExecutor
    except Exception as e:  # noqa: BLE001 - reported in the Output Log, never stops the editor
        unreal.log_error(f"C4: the farm executor did not load: {e}")
        return
    unreal.log("C4: C4FarmExecutor is ready; pick it as the Movie Render Queue's Default Remote Executor")


_c4_setup()
del _c4_setup
