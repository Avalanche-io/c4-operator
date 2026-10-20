"""Print the path of a USD stage's render settings, or nothing.

    python3 render_settings.py STAGE

The stage's own choice (its renderSettingsPrimPath) comes first, else the
first RenderSettings prim in the stage. usdrecord renders through the
settings it is given, else a camera named main_cam, else a free camera, so
passing the settings is what makes the shot camera and its frame the ones
rendered.
"""

import sys

from pxr import Usd, UsdRender

stage = Usd.Stage.Open(sys.argv[1])
settings = UsdRender.Settings.GetStageRenderSettings(stage)
if not settings:
    settings = next((UsdRender.Settings(p) for p in stage.Traverse() if p.IsA(UsdRender.Settings)), None)
print(settings.GetPath() if settings else "")
