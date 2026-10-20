import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
for p in (HERE, os.path.join(HERE, "..", "scripts"), os.path.join(HERE, "..", "..", "c4submit", "src")):
    p = os.path.normpath(p)
    if p not in sys.path:
        sys.path.insert(0, p)

import fakemaya  # noqa: E402

fakemaya.install()

import pytest  # noqa: E402


@pytest.fixture(autouse=True)
def _fresh(monkeypatch, tmp_path):
    # Tests never read or write the artist's ID cache, or reach a real farm.
    monkeypatch.setenv("C4SUBMIT_CACHE", str(tmp_path / "ids.json"))
    for k in ("C4_HUD", "C4_HUD_TOKEN", "C4_HUD_TOKEN_FILE", "C4_PROJECT", "C4_SHOT", "C4_SHOT_PATTERN", "C4_ROOT",
              "SHOT", "C4_TOOL_PATH"):
        monkeypatch.delenv(k, raising=False)
    fakemaya.reset()
    yield
    fakemaya.reset()
