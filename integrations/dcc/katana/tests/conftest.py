import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
for p in (HERE, os.path.join(HERE, "..", "python"), os.path.join(HERE, "..", "..", "c4submit", "src")):
    p = os.path.normpath(p)
    if p not in sys.path:
        sys.path.insert(0, p)

import fakekatana  # noqa: E402

fakekatana.install()

import pytest  # noqa: E402


@pytest.fixture(autouse=True)
def _fresh(monkeypatch, tmp_path):
    # Tests never read or write the artist's ID cache, or reach a real farm.
    monkeypatch.setenv("C4SUBMIT_CACHE", str(tmp_path / "ids.json"))
    for k in ("C4_HUD", "C4_HUD_TOKEN", "C4_HUD_TOKEN_FILE", "C4_PROJECT", "C4_SHOT", "C4_SHOT_PATTERN", "C4_ROOT",
              "SHOT", "C4_TOOL_PATH", "KATANA_RESOURCES"):
        monkeypatch.delenv(k, raising=False)
    fakekatana.reset()
    from c4_katana import dialog, farm
    dialog._open.clear()
    farm._registered.clear()
    yield
    fakekatana.reset()


@pytest.fixture(autouse=True)
def _usd_from_text(monkeypatch):
    # The fake carries no USD, and another folder's fake pxr may linger in
    # sys.modules: USD files are read as text, as without USD's Python.
    from c4submit import usd
    monkeypatch.setattr(usd, "_with_pxr", lambda path: None)
