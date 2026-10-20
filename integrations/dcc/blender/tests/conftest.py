import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
for p in (HERE, os.path.join(HERE, "..", "addons"), os.path.join(HERE, "..", "..", "c4submit", "src")):
    p = os.path.normpath(p)
    if p not in sys.path:
        sys.path.insert(0, p)

import fakebpy  # noqa: E402

# The add-on's modules import bpy as they load, so the fake is installed
# while this folder's tests are collected, and again around each of them;
# no other folder's tests see a bpy.
fakebpy.install()

import pytest  # noqa: E402

import c4_blender  # noqa: E402
from c4_blender import panel  # noqa: E402


def pytest_collection_finish(session):
    fakebpy.uninstall()


@pytest.fixture(autouse=True)
def _blender(monkeypatch, tmp_path):
    # Tests never read the artist's ID cache or the studio's environment.
    monkeypatch.setenv("C4SUBMIT_CACHE", str(tmp_path / "ids.json"))
    for k in ("C4_HUD", "C4_HUD_TOKEN", "C4_HUD_TOKEN_FILE", "C4_PROJECT", "C4_ROOT", "C4_SHOT", "SHOT",
              "C4_SHOT_PATTERN", "C4_TOOL_PATH"):
        monkeypatch.delenv(k, raising=False)
    fakebpy.install()
    fakebpy.reset()
    c4_blender.register()
    yield
    c4_blender.unregister()
    panel._ITEMS.clear()
    fakebpy.uninstall()


@pytest.fixture
def nocache():
    from c4submit.collect import NoCache
    return NoCache()
