import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
for p in (HERE, os.path.join(HERE, "..", "python"), os.path.join(HERE, "..", "..", "c4submit", "src")):
    p = os.path.normpath(p)
    if p not in sys.path:
        sys.path.insert(0, p)

import fakeunreal  # noqa: E402

# The executor module imports unreal as it loads, so the fake is installed
# while this folder's tests are collected, and again around each of them;
# no other folder's tests see an unreal.
fakeunreal.install()

import pytest  # noqa: E402

from c4_unreal import executor  # noqa: E402


def pytest_collection_finish(session):
    fakeunreal.uninstall()


@pytest.fixture(autouse=True)
def _unreal(monkeypatch, tmp_path):
    # Tests never read the artist's ID cache or the studio's environment.
    monkeypatch.setenv("C4SUBMIT_CACHE", str(tmp_path / "ids.json"))
    for k in ("C4_HUD", "C4_HUD_TOKEN", "C4_HUD_TOKEN_FILE", "C4_PROJECT", "C4_ROOT", "C4_SHOT", "SHOT",
              "C4_SHOT_PATTERN", "C4_TOOL_PATH", "C4_UNREAL_CONTENT"):
        monkeypatch.delenv(k, raising=False)
    fakeunreal.install()
    fakeunreal.reset()
    executor._LAST.clear()
    yield
    fakeunreal.uninstall()
